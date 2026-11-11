package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// VideoExecutionPolicy is frozen at admission. Account edits affect new jobs only.
// Request retries never create another paid order: only GET or the original LL2
// idempotency key/body may be retried.
type VideoExecutionPolicy struct {
	RequestTimeoutSeconds int  `json:"request_timeout_seconds"`
	TaskTimeoutSeconds    int  `json:"task_timeout_seconds"`
	RetryEnabled          bool `json:"retry_enabled"`
	MaxRetries            int  `json:"max_retries"`
	RetryIntervalSeconds  int  `json:"retry_interval_seconds"`
}

type VideoExecutionState struct {
	FailureCount      int    `json:"failure_count"`
	LastError         string `json:"last_error,omitempty"`
	NextRetryAt       string `json:"next_retry_at,omitempty"`
	PausedReason      string `json:"paused_reason,omitempty"`
	DeadlineAt        string `json:"deadline_at,omitempty"`
	RetryGeneration   int    `json:"retry_generation"`
	AttemptInProgress bool   `json:"attempt_in_progress,omitempty"`
	LastAttemptAt     string `json:"last_attempt_at,omitempty"`
}

var ErrVideoExecutionPaused = errors.New("video automatic execution is paused or waiting for its retry interval")
var errVideoMetadataMissing = errors.New("provider usage required for settlement is missing")

func defaultVideoExecutionPolicy(provider, protocol string) VideoExecutionPolicy {
	p := VideoExecutionPolicy{RequestTimeoutSeconds: 30, TaskTimeoutSeconds: 1200, RetryEnabled: true, MaxRetries: 3, RetryIntervalSeconds: 30}
	if provider == VideoProviderSeedance {
		p.RequestTimeoutSeconds, p.TaskTimeoutSeconds = 45, 172800
	} else if protocol != MinimaxVideoProtocolLL2 {
		// Getoken's create request itself blocks until generation completes.
		p.RequestTimeoutSeconds = 1200
	}
	return p
}

func videoExecutionPolicyOverrides(extra map[string]any) (map[string]json.RawMessage, error) {
	raw, ok := extra["video_execution_policy"]
	if !ok || raw == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("video execution policy must be an object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("video execution policy must be an object")
	}
	return fields, nil
}

// ValidateVideoExecutionPolicyExtra rejects invalid persisted operator settings.
// Omitted fields inherit the provider default; null removes the override.
func ValidateVideoExecutionPolicyExtra(extra map[string]any) (validationErr error) {
	defer func() {
		if validationErr != nil {
			validationErr = infraerrors.BadRequest("INVALID_VIDEO_EXECUTION_POLICY", validationErr.Error())
		}
	}()
	fields, err := videoExecutionPolicyOverrides(extra)
	if err != nil {
		return err
	}
	limits := map[string][2]int{"request_timeout_seconds": {5, 1200}, "task_timeout_seconds": {60, 259200}, "max_retries": {0, 10}, "retry_interval_seconds": {5, 3600}}
	for name, value := range fields {
		if name == "retry_enabled" {
			var enabled bool
			if string(value) == "null" || json.Unmarshal(value, &enabled) != nil {
				return fmt.Errorf("video retry_enabled must be a boolean")
			}
			continue
		}
		limit, ok := limits[name]
		if !ok {
			return fmt.Errorf("unknown video execution policy field %s", name)
		}
		var number float64
		if string(value) == "null" || json.Unmarshal(value, &number) != nil || math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || number < float64(limit[0]) || number > float64(limit[1]) {
			return fmt.Errorf("video %s must be an integer between %d and %d", name, limit[0], limit[1])
		}
	}
	return nil
}

func SnapshotVideoExecutionPolicy(task *MinimaxVideoTask, account *Account) error {
	if task == nil || account == nil {
		return fmt.Errorf("video execution account unavailable")
	}
	if err := ValidateVideoExecutionPolicyExtra(account.Extra); err != nil {
		return err
	}
	policy := defaultVideoExecutionPolicy(task.Provider, task.UpstreamProtocol)
	fields, _ := videoExecutionPolicyOverrides(account.Extra)
	if fields != nil {
		payload, _ := json.Marshal(fields)
		if err := json.Unmarshal(payload, &policy); err != nil {
			return err
		}
	}
	task.ExecutionPolicy = &policy
	return nil
}

// SnapshotSeedanceVideoIdentity uses exactly the normalization used by both
// submission and polling, including the configured outbound URL allowlist.
func (s *OpenAIGatewayService) SnapshotSeedanceVideoIdentity(task *MinimaxVideoTask, account *Account) error {
	if task == nil || account == nil || task.Provider != VideoProviderSeedance {
		return fmt.Errorf("Seedance task account unavailable")
	}
	base, err := s.validateUpstreamBaseURL(account.GetCredential("base_url"))
	if err != nil {
		return err
	}
	task.UpstreamBaseURL = base
	task.UpstreamCredentialHash = minimaxCredentialHash(account)
	return nil
}

func videoRequestTimeout(task MinimaxVideoTask, fallback time.Duration) time.Duration {
	if task.ExecutionPolicy != nil && task.ExecutionPolicy.RequestTimeoutSeconds >= 5 {
		return time.Duration(task.ExecutionPolicy.RequestTimeoutSeconds) * time.Second
	}
	return fallback
}

func videoTaskTimeout(task MinimaxVideoTask, fallback time.Duration) time.Duration {
	if task.ExecutionPolicy != nil && task.ExecutionPolicy.TaskTimeoutSeconds >= 60 {
		return time.Duration(task.ExecutionPolicy.TaskTimeoutSeconds) * time.Second
	}
	return fallback
}

type videoExecutionDB interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func readVideoExecutionState(ctx context.Context, db videoExecutionDB, id string) (VideoExecutionState, error) {
	var payload []byte
	var state VideoExecutionState
	if err := db.QueryRowContext(ctx, `SELECT execution_state FROM video_tasks WHERE task_id=$1 AND deleted_at IS NULL`, id).Scan(&payload); err != nil {
		return state, err
	}
	if len(payload) != 0 {
		if err := json.Unmarshal(payload, &state); err != nil {
			return state, err
		}
	}
	return state, nil
}

func saveVideoExecutionState(ctx context.Context, db videoExecutionDB, id string, state VideoExecutionState) error {
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `UPDATE video_tasks SET execution_state=$2::jsonb WHERE task_id=$1 AND deleted_at IS NULL`, id, string(payload))
	return err
}

func videoExecutionFailed(state *VideoExecutionState, policy VideoExecutionPolicy, now time.Time, err error) {
	state.AttemptInProgress = false
	state.FailureCount++
	// Provider bodies and transport errors can contain credentials or signed URLs.
	// Persist a useful category, never the raw upstream message.
	state.LastError = "Provider request failed or returned an invalid response"
	if errors.Is(err, context.DeadlineExceeded) {
		state.LastError = "Provider request timed out"
	} else if errors.Is(err, errVideoMetadataMissing) {
		state.LastError = "Provider usage required for settlement is missing"
	}
	state.NextRetryAt = now.Add(time.Duration(policy.RetryIntervalSeconds) * time.Second).UTC().Format(time.RFC3339Nano)
	if !policy.RetryEnabled {
		state.PausedReason = "retry_disabled"
	} else if state.FailureCount > policy.MaxRetries {
		state.PausedReason = "retries_exhausted"
	}
	if state.PausedReason != "" {
		state.NextRetryAt = ""
	}
}

// beginVideoExecution must run inside the provider's task advisory lock (or its
// single-submit claim). Intent is durable before I/O, so a process crash cannot
// reset or silently bypass the retry budget.
func beginVideoExecution(ctx context.Context, db videoExecutionDB, task MinimaxVideoTask) (*VideoExecutionState, error) {
	if task.ExecutionPolicy == nil {
		return nil, nil // Existing tasks retain their legacy policy until admin resume.
	}
	state, err := readVideoExecutionState(ctx, db, task.ID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if state.DeadlineAt == "" {
		created, err := time.Parse(time.RFC3339Nano, task.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("video creation time is invalid")
		}
		state.DeadlineAt = created.Add(time.Duration(task.ExecutionPolicy.TaskTimeoutSeconds) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	if state.AttemptInProgress {
		videoExecutionFailed(&state, *task.ExecutionPolicy, now, errors.New("interrupted provider request"))
	}
	deadline, err := time.Parse(time.RFC3339Nano, state.DeadlineAt)
	if err != nil {
		return nil, fmt.Errorf("video execution deadline is invalid")
	}
	if !now.Before(deadline) {
		state.PausedReason, state.LastError, state.NextRetryAt = "task_timeout", "Task exceeded the configured automatic processing deadline", ""
	}
	wait := false
	if state.NextRetryAt != "" {
		next, err := time.Parse(time.RFC3339Nano, state.NextRetryAt)
		if err != nil {
			return nil, fmt.Errorf("video retry time is invalid")
		}
		wait = now.Before(next)
	}
	if state.PausedReason != "" || wait {
		if err := saveVideoExecutionState(ctx, db, task.ID, state); err != nil {
			return nil, err
		}
		return nil, ErrVideoExecutionPaused
	}
	state.AttemptInProgress, state.LastAttemptAt = true, now.Format(time.RFC3339Nano)
	if err := saveVideoExecutionState(ctx, db, task.ID, state); err != nil {
		return nil, err
	}
	return &state, nil
}

func finishVideoExecution(db videoExecutionDB, task MinimaxVideoTask, state *VideoExecutionState, executionErr error) error {
	if state == nil || task.ExecutionPolicy == nil {
		return nil
	}
	if executionErr != nil {
		videoExecutionFailed(state, *task.ExecutionPolicy, time.Now().UTC(), executionErr)
	} else {
		state.AttemptInProgress, state.FailureCount = false, 0
		state.LastError, state.NextRetryAt, state.PausedReason = "", "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return saveVideoExecutionState(ctx, db, task.ID, *state)
}

func runVideoProviderAttempt(ctx context.Context, db videoExecutionDB, task MinimaxVideoTask, fallback time.Duration, run func(context.Context) (MinimaxVideoTask, error)) (MinimaxVideoTask, error) {
	state, err := beginVideoExecution(ctx, db, task)
	if err != nil {
		return MinimaxVideoTask{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, videoRequestTimeout(task, fallback))
	defer cancel()
	if state != nil {
		deadline, err := time.Parse(time.RFC3339Nano, state.DeadlineAt)
		if err != nil {
			return MinimaxVideoTask{}, err
		}
		var cancelDeadline context.CancelFunc
		requestCtx, cancelDeadline = context.WithDeadline(requestCtx, deadline)
		defer cancelDeadline()
	}
	result, requestErr := run(requestCtx)
	policyErr := requestErr
	if requestErr == nil && result.Status == MinimaxVideoStatusDone {
		metadata := task
		mergeVideoProviderResult(&metadata, result)
		if videoTaskNeedsProviderMetadata(metadata) {
			policyErr = errVideoMetadataMissing
		}
	}
	if err := finishVideoExecution(db, task, state, policyErr); err != nil {
		// Preserve an accepted provider ID/result even when recording the retry
		// bookkeeping fails. The durable intent counts as interrupted on restart.
		slog.Warn("video.execution_state_persist_failed", "task_id", task.ID, "error", err)
	}
	return result, requestErr
}

func pauseVideoExecution(ctx context.Context, db videoExecutionDB, task MinimaxVideoTask, reason, message string) error {
	if task.ExecutionPolicy == nil {
		return nil
	}
	_, err := db.ExecContext(ctx, `UPDATE video_tasks SET execution_state=execution_state || jsonb_build_object('paused_reason',$2::text,'last_error',$3::text,'next_retry_at','','attempt_in_progress',false) WHERE task_id=$1 AND deleted_at IS NULL`, task.ID, reason, message)
	return err
}

// ResumeVideoTaskExecution extends only the local processing window. It never
// changes the upstream task identity, the user ledger, or the provider expiry.
// All provider locks are taken before the row lock to avoid polling deadlocks.
func ResumeVideoTaskExecution(ctx context.Context, taskID string, actorID int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if actorID <= 0 || reason == "" || len(reason) > 1000 {
		return fmt.Errorf("%w: actor and reason are required", ErrVideoOperationInvalid)
	}
	db := videoTaskHandle()
	if db == nil {
		return fmt.Errorf("video database unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, prefix := range []string{"minimax-video-submit:", "minimax-video-poll:", "seedance-video-poll:"} {
		var locked bool
		if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`, prefix+taskID).Scan(&locked); err != nil {
			return err
		}
		if !locked {
			return fmt.Errorf("%w: provider request is still in progress", ErrVideoOperationConflict)
		}
	}
	var snapshot, previous []byte
	var upstreamID, status string
	var deleted, settled *time.Time
	err = tx.QueryRowContext(ctx, `SELECT task_snapshot,execution_state,upstream_id,status,deleted_at,settled_at FROM video_tasks WHERE task_id=$1 FOR UPDATE`, taskID).Scan(&snapshot, &previous, &upstreamID, &status, &deleted, &settled)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrVideoOperationNotFound
	}
	if err != nil {
		return err
	}
	if deleted != nil || settled != nil || status == MinimaxVideoStatusError {
		return fmt.Errorf("%w: completed, deleted or failed tasks cannot be resumed", ErrVideoOperationConflict)
	}
	var task MinimaxVideoTask
	if err := json.Unmarshal(snapshot, &task); err != nil || task.ID != taskID {
		return fmt.Errorf("%w: task snapshot is unavailable", ErrVideoOperationConflict)
	}
	task.UpstreamID, task.Status = upstreamID, status
	if upstreamID == "" {
		if task.UpstreamProtocol != MinimaxVideoProtocolLL2 {
			return fmt.Errorf("%w: this provider cannot safely replay an uncertain submission", ErrVideoOperationConflict)
		}
		if err := validateLL2SubmissionSnapshot(task, time.Now()); err != nil {
			return fmt.Errorf("%w: %s", ErrVideoOperationConflict, err)
		}
	}
	if task.ExecutionPolicy == nil {
		policy := defaultVideoExecutionPolicy(task.Provider, task.UpstreamProtocol)
		task.ExecutionPolicy = &policy
	}
	var state VideoExecutionState
	if err := json.Unmarshal(previous, &state); err != nil {
		return err
	}
	if state.PausedReason == "" && state.FailureCount == 0 && !state.AttemptInProgress {
		deadline, _ := time.Parse(time.RFC3339Nano, state.DeadlineAt)
		if deadline.IsZero() {
			created, parseErr := time.Parse(time.RFC3339Nano, task.CreatedAt)
			if parseErr == nil {
				deadline = created.Add(time.Duration(task.ExecutionPolicy.TaskTimeoutSeconds) * time.Second)
			}
		}
		if (state.RetryGeneration > 0 || status != MinimaxVideoStatusDone) && (deadline.IsZero() || time.Now().Before(deadline)) {
			return fmt.Errorf("%w: task is already active", ErrVideoOperationConflict)
		}
	}
	state = VideoExecutionState{RetryGeneration: state.RetryGeneration + 1, DeadlineAt: time.Now().UTC().Add(time.Duration(task.ExecutionPolicy.TaskTimeoutSeconds) * time.Second).Format(time.RFC3339Nano)}
	updated, _ := json.Marshal(state)
	policyJSON, _ := json.Marshal(task.ExecutionPolicy)
	if _, err := tx.ExecContext(ctx, `UPDATE video_tasks SET execution_state=$2::jsonb,task_snapshot=jsonb_set(task_snapshot,'{execution_policy}',$3::jsonb) WHERE task_id=$1`, taskID, string(updated), string(policyJSON)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO video_task_operations_audit(task_id,action,actor_id,before_value,after_value,note) VALUES ($1,'resume',$2,$3::jsonb,$4::jsonb,$5)`, taskID, actorID, string(previous), string(updated), reason); err != nil {
		return err
	}
	return tx.Commit()
}
