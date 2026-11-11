package service

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Expiry stops automatic submission retries, but is not evidence that an
// ambiguous upstream request failed. The durable hold must remain for review.
var ErrLL2SubmissionExpired = errors.New("LL2 submission recovery window expired; task requires reconciliation")

// RunLL2MinimaxVideoTask performs at most one idempotent submission attempt.
// Once a provider ID is durable, all subsequent calls become no-ops and the
// normal poll worker takes over. An uncertain HTTP outcome remains pending.
func (s *OpenAIGatewayService) RunLL2MinimaxVideoTask(ctx context.Context, taskID string, userID, apiKeyID int64) (retErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			retErr = fmt.Errorf("LL2 submission worker interrupted: %v", recovered)
		}
	}()
	task, err := s.LoadMinimaxVideoTask(ctx, taskID, userID, apiKeyID)
	if err != nil || task == nil {
		return err
	}
	if task.IsTerminal() || task.UpstreamID != "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, videoRequestTimeout(*task, 30*time.Second)+5*time.Second)
	defer cancel()
	if err := validateLL2SubmissionSnapshot(*task, time.Now()); err != nil {
		if errors.Is(err, ErrLL2SubmissionExpired) {
			_ = pauseVideoExecution(ctx, videoTaskHandle(), *task, "submission_expired", "Safe idempotent submission recovery window expired; reconcile with the supplier")
		}
		return err
	}
	// Repository access precedes the dedicated lock connection. This must also
	// work when the database pool only has a single connection.
	account, base, err := s.minimaxBoundTaskAccount(ctx, task)
	if err != nil {
		_ = pauseVideoExecution(ctx, videoTaskHandle(), *task, "configuration_changed", "Original provider account configuration is unavailable or changed")
		return err
	}
	db := videoTaskHandle()
	if db == nil {
		return fmt.Errorf("video database unavailable")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	lockKey := "minimax-video-submit:" + task.ID
	var claimed bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", lockKey).Scan(&claimed); err != nil || !claimed {
		_ = conn.Close()
		return err
	}
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", lockKey); err != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
			_ = conn.Close()
		})
	}
	defer release()
	fresh, err := loadMinimaxVideoTaskWithQueryer(ctx, conn, taskID, userID, apiKeyID)
	if err != nil || fresh == nil {
		return err
	}
	if fresh.IsTerminal() || fresh.UpstreamID != "" {
		return nil
	}
	if !sameLL2SubmissionSnapshot(*task, *fresh) {
		return fmt.Errorf("LL2 task submission identity changed while acquiring the lock")
	}
	task = fresh
	if err := validateLL2SubmissionSnapshot(*task, time.Now()); err != nil {
		return err
	}
	target, err := BuildLL2MinimaxVideoURL(base, MinimaxVideoEndpointCreate, "")
	if err != nil {
		return err
	}
	previouslyAttempted := task.CreateStarted
	task.CreateStarted = true
	task.Status = MinimaxVideoStatusProcessing
	task.LastPolledAt = time.Now().UTC().Format(time.RFC3339Nano)
	// Persist intent before the network call. Restart recovery sends precisely
	// these bytes and this task ID, never a rebuilt request or a fresh key.
	if err := saveMinimaxVideoTaskWithExecer(ctx, conn, *task); err != nil {
		return err
	}
	result, submitErr := runVideoProviderAttempt(ctx, conn, *task, 30*time.Second, func(callCtx context.Context) (MinimaxVideoTask, error) {
		result, err := s.callMinimaxVideoProtocol(callCtx, account, MinimaxVideoProtocolLL2, http.MethodPost,
			target, task.UpstreamRequestBody, task.ID)
		if err == nil {
			if strings.TrimSpace(result.UpstreamID) == "" {
				err = fmt.Errorf("LL2 submission returned no task ID; outcome requires idempotent recovery")
			} else {
				err = validateUpstreamPathSegment("LL2 video task ID", result.UpstreamID)
			}
		}
		// POST acknowledges acceptance only; GET supplies authoritative metadata.
		if err == nil {
			result.Status = MinimaxVideoStatusProcessing
		}
		return result, err
	})
	if submitErr != nil {
		if !ll2SubmissionDefinitelyRejected(submitErr, previouslyAttempted) {
			return submitErr
		}
		task.Status = MinimaxVideoStatusError
		task.Error = "Video request rejected by the provider"
		task.CompletedAt = GrokVideoPendingCreatedAtNow()
	} else {
		if strings.TrimSpace(result.UpstreamID) == "" {
			return fmt.Errorf("LL2 submission returned no task ID; outcome requires idempotent recovery")
		}
		if err := validateUpstreamPathSegment("LL2 video task ID", result.UpstreamID); err != nil {
			return err
		}
		// The create contract only acknowledges acceptance. A subsequent GET
		// must confirm the ID, final output and seconds before publication/billing.
		task.UpstreamID = result.UpstreamID
		task.Status = MinimaxVideoStatusProcessing
	}
	// Save through the lock connection even if the request timed out while the
	// provider response was being read. Release it before entering billing.
	persistCtx, cancelPersist := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelPersist()
	for attempt := 0; attempt < 3; attempt++ {
		err = saveMinimaxVideoTaskWithExecer(persistCtx, conn, *task)
		if err == nil || errors.Is(err, ErrVideoTaskTerminal) {
			break
		}
		select {
		case <-persistCtx.Done():
			return persistCtx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	if err != nil {
		return err
	}
	release()
	return submitErr
}

func validateLL2SubmissionSnapshot(task MinimaxVideoTask, now time.Time) error {
	if task.UpstreamProtocol != MinimaxVideoProtocolLL2 || strings.TrimSpace(task.UpstreamBaseURL) == "" ||
		strings.TrimSpace(task.UpstreamCredentialHash) == "" || !json.Valid(task.UpstreamRequestBody) ||
		!bytes.HasPrefix(bytes.TrimSpace(task.UpstreamRequestBody), []byte("{")) {
		return fmt.Errorf("LL2 submission snapshot is incomplete")
	}
	created, err := time.Parse(time.RFC3339Nano, task.CreatedAt)
	if err != nil {
		return fmt.Errorf("LL2 task creation time is invalid")
	}
	deadline := created.Add(24 * time.Hour)
	if task.ExpiresAt != "" {
		expires, err := time.Parse(time.RFC3339Nano, task.ExpiresAt)
		if err != nil {
			return fmt.Errorf("LL2 submission expiry is invalid")
		}
		if expires.Before(deadline) {
			deadline = expires
		}
	}
	if !now.Before(deadline) {
		return ErrLL2SubmissionExpired
	}
	return nil
}

func sameLL2SubmissionSnapshot(a, b MinimaxVideoTask) bool {
	return a.ID == b.ID && a.UserID == b.UserID && a.APIKeyID == b.APIKeyID && a.AccountID == b.AccountID &&
		a.UpstreamProtocol == b.UpstreamProtocol && a.UpstreamBaseURL == b.UpstreamBaseURL &&
		a.UpstreamCredentialHash == b.UpstreamCredentialHash && bytes.Equal(a.UpstreamRequestBody, b.UpstreamRequestBody)
}

func ll2SubmissionDefinitelyRejected(err error, previouslyAttempted bool) bool {
	// A retry rejection cannot prove a previous ambiguous attempt was rejected.
	// Keep its durable hold and original identity for operator reconciliation.
	if previouslyAttempted {
		return false
	}
	var upstreamErr *MinimaxVideoUpstreamError
	if !errors.As(err, &upstreamErr) {
		return false
	}
	switch upstreamErr.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}
