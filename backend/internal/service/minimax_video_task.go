package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	var task MinimaxVideoTask
	if client == nil {
		client = MinimaxVideoUpstreamClient(0)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return task, fmt.Errorf("build minimax video request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	if token := strings.TrimSpace(bearerToken); token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return task, fmt.Errorf("call minimax video upstream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return task, fmt.Errorf("read minimax video upstream: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return task, &MinimaxVideoUpstreamError{StatusCode: resp.StatusCode, Body: payload}
	}
	parsed, err := ParseMinimaxVideoUpstreamResponse(payload)
	if err != nil {
		return task, err
	}
	if parsed.Status == MinimaxVideoStatusError {
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
	if task.UpstreamCostUSD > 0 {
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
			_, _ = FailPendingVideoTask(cleanup, taskID, "Video worker interrupted")
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
	// Keep the billing snapshot in memory for the entire call, so Redis loss
	// after the upstream has charged us cannot lose the payer or price.
	workCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	client := MinimaxVideoUpstreamClient(20 * time.Minute)
	result, runErr := ExecuteMinimaxVideoTask(workCtx, client, targetURL, bearerToken, body)
	for runErr == nil && !result.IsTerminal() {
		if result.UpstreamID == "" {
			runErr = fmt.Errorf("upstream pending response has no task id")
			break
		}
		if result.UpstreamID != "" {
			task.UpstreamID = result.UpstreamID
		}
		task.Status = MinimaxVideoStatusProcessing
		if err := s.StoreMinimaxVideoTask(workCtx, *task); err != nil {
			slog.Warn("minimax_video.progress_persist_failed", "task_id", taskID, "error", err)
			return
		}
		select {
		case <-workCtx.Done():
			runErr = workCtx.Err()
		case <-time.After(5 * time.Second):
			result, runErr = pollMinimaxVideoUpstream(workCtx, client, targetURL, bearerToken, task.UpstreamID)
		}
	}
	if runErr == nil && (result.Status != MinimaxVideoStatusDone || strings.TrimSpace(result.VideoURL) == "") {
		runErr = fmt.Errorf("upstream did not return a completed video")
	}
	task.CompletedAt = GrokVideoPendingCreatedAtNow()
	if runErr != nil {
		task.Status = MinimaxVideoStatusError
		// Detailed upstream bodies belong in server logs, not public task state.
		task.Error = "Video generation failed or timed out"
		slog.Warn("minimax_video.task_failed", "task_id", taskID, "error", runErr)
	} else {
		task.Status = MinimaxVideoStatusDone
		if result.UpstreamID != "" {
			task.UpstreamID = result.UpstreamID
		}
		task.VideoURL = result.VideoURL
		task.UpstreamCostUSD = result.UpstreamCostUSD
		if result.Resolution != "" {
			task.Resolution = result.Resolution
		}
		if result.DurationSeconds > 0 {
			task.DurationSeconds = result.DurationSeconds
		}
	}
	// The generation context may have expired. Persist the outcome independently.
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer persistCancel()
	var persistErr error
	for attempt := 0; attempt < 3; attempt++ {
		persistErr = s.StoreMinimaxVideoTask(persistCtx, *task)
		if persistErr == nil || errors.Is(persistErr, ErrVideoTaskTerminal) {
			break
		}
		select {
		case <-persistCtx.Done():
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	if persistErr != nil {
		slog.Error("minimax_video.terminal_persist_failed", "task_id", taskID, "error", persistErr)
		return
	}
	if task.Status == MinimaxVideoStatusDone {
		s.SettleMinimaxVideoTask(persistCtx, *task)
	}
}

func pollMinimaxVideoUpstream(ctx context.Context, client *http.Client, createURL, token, upstreamID string) (MinimaxVideoTask, error) {
	base := strings.TrimSuffix(createURL, minimaxVideoPathGenerations)
	target, err := BuildMinimaxVideoURL(base, MinimaxVideoEndpointStatus, upstreamID)
	if err != nil {
		return MinimaxVideoTask{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return MinimaxVideoTask{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return MinimaxVideoTask{}, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return MinimaxVideoTask{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return MinimaxVideoTask{}, &MinimaxVideoUpstreamError{StatusCode: resp.StatusCode, Body: payload}
	}
	result, err := ParseMinimaxVideoUpstreamResponse(payload)
	if result.UpstreamID == "" {
		result.UpstreamID = upstreamID
	}
	return result, err
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
