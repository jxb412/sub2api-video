package service

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"

	"log/slog"
)

// Async bridge for MiniMax-style video providers.
//
// The upstream create call blocks for minutes, so the gateway never forwards it
// on the request goroutine. Instead it:
//
//  1. stores a queued task and billing snapshot in PostgreSQL,
//  2. returns the gateway task id to the client immediately,
//  3. runs ExecuteMinimaxVideoTask in the background,
//  4. persists the terminal state before applying idempotent billing.
//
// Status and content polls read durable state. Cache expiry never loses a
// completed video or a pending settlement; an interrupted create is not replayed.

const minimaxVideoTaskKeyPrefix = "minimax-video:task:"

// MinimaxVideoTaskKey is the cache key for one owner's task. Ownership is part
// of the key so a task id alone cannot be used to read another tenant's video.
func MinimaxVideoTaskKey(taskID string, userID, apiKeyID int64) string {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" || userID <= 0 || apiKeyID <= 0 {
		return ""
	}
	return fmt.Sprintf("%s%d:%d:%s", minimaxVideoTaskKeyPrefix, userID, apiKeyID, taskID)
}

// StoreMinimaxVideoTask commits durable state before refreshing the cache.
func (s *OpenAIGatewayService) StoreMinimaxVideoTask(ctx context.Context, task MinimaxVideoTask) error {
	if s == nil {
		return fmt.Errorf("minimax video service is unavailable")
	}
	key := MinimaxVideoTaskKey(task.ID, task.UserID, task.APIKeyID)
	if key == "" {
		return fmt.Errorf("minimax video task key is invalid")
	}
	if strings.TrimSpace(task.CreatedAt) == "" {
		task.CreatedAt = GrokVideoPendingCreatedAtNow()
	}
	if task.Status == MinimaxVideoStatusQueued && task.BillingSnapshot != nil && s.cfg != nil {
		task.BillingSnapshot.SimpleMode = s.cfg.RunMode == config.RunModeSimple
		task.BillingSnapshot.SimpleModeKeyRateLimitEnabled = s.cfg.SimpleModeKeyRateLimitEnabled
	}
	if err := saveMinimaxVideoTask(ctx, task); err != nil {
		return err
	}
	if s.cache == nil {
		return nil
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return err
	}
	if err := s.cache.SetGrokVideoPendingBilling(ctx, key, payload, MinimaxVideoTaskTTL(grokVideoPendingBillingTTL(s.cfg))); err != nil {
		slog.Warn("minimax_video.cache_publish_failed", "task_id", task.ID, "error", err)
	}
	return nil
}

// LoadMinimaxVideoTask reads owner-scoped durable state, including admin changes.
func (s *OpenAIGatewayService) LoadMinimaxVideoTask(ctx context.Context, taskID string, userID, apiKeyID int64) (*MinimaxVideoTask, error) {
	if s == nil {
		return nil, fmt.Errorf("minimax video service is unavailable")
	}
	key := MinimaxVideoTaskKey(taskID, userID, apiKeyID)
	if key == "" {
		return nil, fmt.Errorf("minimax video task key is invalid")
	}
	// DB is authoritative for deletion, cancellation and restart recovery. A
	// stale cache entry must never resurrect a cancelled or deleted task.
	return loadMinimaxVideoTaskFromDB(ctx, taskID, userID, apiKeyID)
}

// UpdateMinimaxVideoTask applies a mutation to a stored task, preserving the
// ownership fields. Used by the background runner to publish progress.
func (s *OpenAIGatewayService) UpdateMinimaxVideoTask(ctx context.Context, taskID string, userID, apiKeyID int64, mutate func(*MinimaxVideoTask)) error {
	task, err := s.LoadMinimaxVideoTask(ctx, taskID, userID, apiKeyID)
	if err != nil {
		return err
	}
	if task == nil {
		return fmt.Errorf("minimax video task not found")
	}
	if mutate != nil {
		mutate(task)
	}
	return s.StoreMinimaxVideoTask(ctx, *task)
}

// MinimaxVideoUpstreamClient is the HTTP client used for the blocking create
// call. Video generation routinely takes several minutes, so the timeout is far
// above the default gateway timeout.
func MinimaxVideoUpstreamClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// ExecuteMinimaxVideoTask performs the blocking upstream create call and parses
// the result into a terminal task state. It does not touch the cache, so callers
// can run it inside their own worker.
func ExecuteMinimaxVideoTask(ctx context.Context, client *http.Client, targetURL, bearerToken string, body []byte) (MinimaxVideoTask, error) {
	if client == nil {
		client = MinimaxVideoUpstreamClient(0)
	}
	return requestMinimaxVideo(ctx, http.MethodPost, targetURL, bearerToken, body, nil, client.Do)
}

func requestMinimaxVideo(ctx context.Context, method, targetURL, bearerToken string, body []byte, account *Account, do func(*http.Request) (*http.Response, error)) (MinimaxVideoTask, error) {
	var task MinimaxVideoTask
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bytes.NewReader(body))
	if err != nil {
		return task, fmt.Errorf("build minimax video request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	if token := strings.TrimSpace(bearerToken); token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	if account != nil {
		account.ApplyHeaderOverrides(req.Header)
	}
	resp, err := do(req)
	if err != nil {
		return task, fmt.Errorf("call minimax video upstream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return task, fmt.Errorf("read minimax video upstream: %w", err)
	}
	if len(payload) > 8<<20 {
		return task, fmt.Errorf("minimax video response exceeds size limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return task, &MinimaxVideoUpstreamError{StatusCode: resp.StatusCode, Body: payload}
	}
	parsed, err := ParseMinimaxVideoUpstreamResponse(payload)
	if err != nil {
		return task, err
	}
	if parsed.Status == MinimaxVideoStatusError && method == http.MethodPost {
		msg := parsed.Error
		if msg == "" {
			msg = "upstream reported an error"
		}
		return parsed, fmt.Errorf("%s", msg)
	}
	if parsed.Status != MinimaxVideoStatusDone {
		// The blocking endpoint should already be done; treat anything else as
		// a task we can still poll later.
		return parsed, nil
	}
	return parsed, nil
}

// Use the same account transport as other gateway calls, including proxy and
// operator header overrides. Credentials are loaded anew before every poll.
func (s *OpenAIGatewayService) callMinimaxVideo(ctx context.Context, account *Account, method, target string, body []byte) (MinimaxVideoTask, error) {
	if account == nil || strings.TrimSpace(account.GetCredential("api_key")) == "" {
		return MinimaxVideoTask{}, fmt.Errorf("minimax video account credential unavailable")
	}
	ctx = WithHTTPUpstreamRedirectsDisabled(ctx)
	if method == http.MethodPost {
		ctx = WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileVideo)
	}
	do := MinimaxVideoUpstreamClient(20 * time.Minute).Do
	if s.httpUpstream != nil {
		proxy := ""
		if account.Proxy != nil {
			proxy = account.Proxy.URL()
		}
		do = func(req *http.Request) (*http.Response, error) {
			return s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
		}
	}
	return requestMinimaxVideo(ctx, method, target, account.GetCredential("api_key"), body, account, do)
}

// MinimaxVideoUpstreamError carries the upstream status and body so handlers can
// map it to a client-visible error without leaking provider internals.
type MinimaxVideoUpstreamError struct {
	StatusCode int
	Body       []byte
}

func (e *MinimaxVideoUpstreamError) Error() string {
	if e == nil {
		return "minimax video upstream error"
	}
	msg := strings.TrimSpace(string(e.Body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return fmt.Sprintf("minimax video upstream returned %d: %s", e.StatusCode, msg)
}

// MinimaxVideoTaskStatusBody renders the client-facing status document, using
// the same field names the upstream uses so SDKs keep working.
func MinimaxVideoTaskStatusBody(task MinimaxVideoTask, contentPath string) []byte {
	if task.Status == "" {
		task.Status = MinimaxVideoStatusProcessing
	}
	out := map[string]any{
		"id":         task.ID,
		"request_id": task.ID,
		"status":     task.Status,
		"model":      task.Model,
		"created_at": task.CreatedAt,
	}
	if task.UpstreamID != "" {
		out["upstream_id"] = task.UpstreamID
	}
	if task.DurationSeconds > 0 {
		out["duration"] = task.DurationSeconds
	}
	if task.Resolution != "" {
		out["resolution"] = task.Resolution
		out["output_resolution"] = task.Resolution
	}
	if task.Status == MinimaxVideoStatusDone && contentPath != "" {
		out["video"] = map[string]any{"url": contentPath}
	}
	if task.Error != "" {
		out["error"] = map[string]any{"message": task.Error, "type": "upstream_error"}
	}
	if task.UpstreamCostReported || task.UpstreamCostUSD > 0 {
		out["usage"] = map[string]any{"upstream_cost_usd": task.UpstreamCostUSD}
	}
	if task.CompletedAt != "" {
		out["completed_at"] = task.CompletedAt
	}
	body, err := json.Marshal(out)
	if err != nil {
		return []byte(`{"status":"error","error":{"message":"failed to render status"}}`)
	}
	return body
}

// BindMinimaxVideoTaskAccount ties a task id to the account that owns it so
// status/content polls resolve without re-running the scheduler.
func (s *OpenAIGatewayService) BindMinimaxVideoTaskAccount(
	ctx context.Context, groupID *int64, taskID string, userID, apiKeyID, accountID int64,
) error {
	return s.BindGrokMediaVideoRequestAccount(ctx, groupID, taskID, userID, apiKeyID, accountID)
}

// RunMinimaxVideoTask performs the blocking upstream call and publishes the
// terminal state. It is meant to be started as a goroutine by the handler.
func (s *OpenAIGatewayService) RunMinimaxVideoTask(
	ctx context.Context, taskID string, userID, apiKeyID int64,
	targetURL, bearerToken string, body []byte,
) {
	if s == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("minimax_video.worker_panic", "task_id", taskID, "panic", recovered)
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			task, err := s.LoadMinimaxVideoTask(cleanup, taskID, userID, apiKeyID)
			if err == nil && task != nil && task.UpstreamID == "" {
				_, _ = FailPendingVideoTask(cleanup, taskID, "Video worker interrupted")
			}
		}
	}()
	claimed, err := claimMinimaxVideoTask(ctx, taskID, userID, apiKeyID)
	if err != nil || !claimed {
		slog.Warn("minimax_video.worker_not_claimed", "task_id", taskID, "error", err)
		return
	}
	task, err := s.LoadMinimaxVideoTask(ctx, taskID, userID, apiKeyID)
	if err != nil || task == nil {
		slog.Error("minimax_video.task_load_failed", "task_id", taskID, "error", err)
		return
	}
	// Keep the immutable billing snapshot for submission. Polling always reloads
	// durable state so an operator's terminal decision cannot be overwritten.
	workCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	account, base, runErr := s.minimaxTaskAccount(workCtx, task.AccountID)
	var result MinimaxVideoTask
	if runErr == nil {
		targetURL, runErr = BuildMinimaxVideoURL(base, MinimaxVideoEndpointCreate, "")
	}
	if runErr == nil {
		result, runErr = s.callMinimaxVideo(workCtx, account, http.MethodPost, targetURL, body)
	}
	if runErr == nil && result.Status == MinimaxVideoStatusDone {
		runErr = s.validateMinimaxCompletedResult(result, base)
	}
	if runErr == nil && !result.IsTerminal() && result.UpstreamID == "" {
		runErr = fmt.Errorf("upstream pending response has no task id")
	}
	if runErr != nil {
		task.Status = MinimaxVideoStatusError
		task.Error = "Video submission failed; do not automatically retry an ambiguous submission"
		task.CompletedAt = GrokVideoPendingCreatedAtNow()
		slog.Warn("minimax_video.task_failed", "task_id", taskID, "error", runErr)
	} else {
		mergeVideoProviderResult(task, result)
	}
	if err := s.persistVideoOutcome(*task); err != nil || task.IsTerminal() {
		if err != nil {
			slog.Warn("minimax_video.outcome_persist_failed", "task_id", taskID, "error", err)
		}
		return
	}
	// A known provider ID is never resubmitted. Lookup failures and local worker
	// deadlines leave the durable pending task available for restart recovery.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-workCtx.Done():
			return
		case <-ticker.C:
		}
		if err := s.PollMinimaxVideoTask(workCtx, task); err != nil {
			slog.Warn("minimax_video.poll_retry_pending", "task_id", taskID, "error", err)
		}
		if task.IsTerminal() {
			return
		}
	}
}

func pollMinimaxVideoUpstream(ctx context.Context, client *http.Client, createURL, token, upstreamID string) (MinimaxVideoTask, error) {
	base := strings.TrimSuffix(createURL, minimaxVideoPathGenerations)
	target, err := BuildMinimaxVideoURL(base, MinimaxVideoEndpointStatus, upstreamID)
	if err != nil {
		return MinimaxVideoTask{}, err
	}
	if client == nil {
		client = MinimaxVideoUpstreamClient(30 * time.Second)
	}
	result, err := requestMinimaxVideo(ctx, http.MethodGet, target, token, nil, nil, client.Do)
	if result.UpstreamID == "" {
		result.UpstreamID = upstreamID
	}
	if err == nil && result.UpstreamID != upstreamID {
		return MinimaxVideoTask{}, fmt.Errorf("minimax poll returned a different task ID")
	}
	return result, err
}

func (s *OpenAIGatewayService) minimaxTaskAccount(ctx context.Context, id int64) (*Account, string, error) {
	if s == nil || s.accountRepo == nil {
		return nil, "", fmt.Errorf("video account repository unavailable")
	}
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if account == nil || strings.TrimSpace(account.GetCredential("api_key")) == "" {
		return nil, "", fmt.Errorf("video account credential unavailable")
	}
	base, err := s.validateUpstreamBaseURL(MinimaxVideoBaseURL(account))
	return account, base, err
}

// PollMinimaxVideoTask resumes an existing provider task without submitting a
// second generation. A dedicated DB connection serializes polls across workers.
// Transient errors preserve pending state; only a valid terminal response is
// published and charged. The caller receives authoritative state in task.
func (s *OpenAIGatewayService) PollMinimaxVideoTask(ctx context.Context, task *MinimaxVideoTask) error {
	ctx, cancelPoll := context.WithTimeout(ctx, 30*time.Second)
	defer cancelPoll()
	if task == nil {
		return fmt.Errorf("video task unavailable")
	}
	db := videoTaskHandle()
	if db == nil {
		return fmt.Errorf("video database unavailable")
	}
	// Account loading can use the shared pool. Complete it before holding the
	// session-lock connection, so polling works even with a one-connection pool.
	account, base, err := s.minimaxTaskAccount(ctx, task.AccountID)
	if err != nil {
		// Missing/deleted accounts must not permanently monopolize the oldest
		// recovery batch. No lock connection is held while rotating this attempt.
		_, _ = db.ExecContext(ctx, `UPDATE video_tasks SET task_snapshot=jsonb_set(task_snapshot,'{last_polled_at}',to_jsonb(now()::text)) WHERE task_id=$1 AND deleted_at IS NULL`, task.ID)
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	lockKey := "minimax-video-poll:" + task.ID
	var claimed bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", lockKey).Scan(&claimed); err != nil || !claimed {
		_ = conn.Close()
		return err
	}
	var releaseOnce sync.Once
	releaseLock := func() {
		releaseOnce.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", lockKey); err != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
			_ = conn.Close()
		})
	}
	defer releaseLock()
	fresh, err := loadMinimaxVideoTaskWithQueryer(ctx, conn, task.ID, task.UserID, task.APIKeyID)
	if err != nil {
		return err
	}
	if fresh == nil {
		task.Status = MinimaxVideoStatusError
		return nil
	}
	*task = *fresh
	if task.AccountID != account.ID {
		return fmt.Errorf("video task account changed during poll")
	}
	wasDone := task.Status == MinimaxVideoStatusDone
	if task.IsTerminal() && (!wasDone || !videoTaskNeedsProviderMetadata(*task)) {
		return nil
	}
	if task.Provider == VideoProviderSeedance || task.UpstreamID == "" {
		return nil
	}
	task.LastPolledAt = time.Now().UTC().Format(time.RFC3339Nano)
	if wasDone {
		if _, err := conn.ExecContext(ctx, `UPDATE video_tasks SET task_snapshot=jsonb_set(task_snapshot,'{last_polled_at}',to_jsonb(now()::text)) WHERE task_id=$1 AND status='done' AND settled_at IS NULL AND deleted_at IS NULL`, task.ID); err != nil {
			return err
		}
	} else {
		if err := saveMinimaxVideoTaskWithExecer(ctx, conn, *task); err != nil {
			return err
		}
	}
	target, err := BuildMinimaxVideoURL(base, MinimaxVideoEndpointStatus, task.UpstreamID)
	if err != nil {
		return err
	}
	pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := s.callMinimaxVideo(pollCtx, account, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	if result.UpstreamID == "" {
		result.UpstreamID = task.UpstreamID
	}
	if result.UpstreamID != task.UpstreamID {
		return fmt.Errorf("minimax poll returned a different task ID")
	}
	if result.Status == MinimaxVideoStatusDone {
		if err := s.validateMinimaxCompletedResult(result, base); err != nil {
			return err
		}
	}
	if result.Status == MinimaxVideoStatusError {
		result.Error = "Video generation failed or was cancelled by the provider"
	}
	if wasDone {
		if result.Status != MinimaxVideoStatusDone {
			return fmt.Errorf("provider tried to regress a completed task")
		}
		if !task.UpstreamCostReported && result.UpstreamCostReported {
			task.UpstreamCostUSD, task.UpstreamCostReported = result.UpstreamCostUSD, true
		}
		if !task.UsageReported && result.UsageReported {
			task.InputTokens, task.OutputTokens, task.UsageReported = result.InputTokens, result.OutputTokens, true
		}
		payload, err := json.Marshal(task)
		if err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, `UPDATE video_tasks SET task_snapshot=$2,upstream_cost_usd=$3 WHERE task_id=$1 AND status='done' AND settled_at IS NULL AND deleted_at IS NULL`, task.ID, string(payload), task.UpstreamCostUSD)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		releaseLock()
		_, err = s.settleMinimaxVideoTask(ctx, *task)
		return err
	}
	mergeVideoProviderResult(task, result)
	// The provider result survives a request deadline. Persist through the lock
	// connection, then release it before the billing ledger acquires connections.
	persistCtx, cancelPersist := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelPersist()
	if err := saveMinimaxVideoTaskWithExecer(persistCtx, conn, *task); err != nil {
		return err
	}
	releaseLock()
	if task.Status == MinimaxVideoStatusDone {
		_, err = s.settleMinimaxVideoTask(persistCtx, *task)
	}
	return err
}

func (s *OpenAIGatewayService) validateMinimaxCompletedResult(result MinimaxVideoTask, base string) error {
	if strings.TrimSpace(result.VideoURL) == "" {
		return fmt.Errorf("minimax success response has no video URL")
	}
	return s.ValidateVideoDownloadURL(result.VideoURL, base)
}

// MinimaxVideoAccountCredentials returns the upstream base URL and API key of
// the account that owns a task, so content downloads can authenticate.
func (s *OpenAIGatewayService) MinimaxVideoAccountCredentials(ctx context.Context, accountID int64) (string, string) {
	if s == nil || s.accountRepo == nil || accountID <= 0 {
		return DefaultGetokenBaseURL, ""
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		return DefaultGetokenBaseURL, ""
	}
	base := strings.TrimSpace(account.GetCredential("base_url"))
	if base == "" {
		base = DefaultGetokenBaseURL
	}
	return base, strings.TrimSpace(account.GetCredential("api_key"))
}
