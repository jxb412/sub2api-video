package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"log/slog"
)

// Async bridge for MiniMax-style video providers.
//
// The upstream create call blocks for minutes, so the gateway never forwards it
// on the request goroutine. Instead it:
//
//  1. stores a queued task (cache-backed, survives restarts),
//  2. returns the gateway task id to the client immediately,
//  3. runs ExecuteMinimaxVideoTask in the background,
//  4. writes the terminal state back into the same cache entry.
//
// Status and content polls read that entry, so a slow upstream never holds a
// client connection open.

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

// StoreMinimaxVideoTask persists the task state with the pending-billing TTL so
// late status/content polls still resolve after a restart.
func (s *OpenAIGatewayService) StoreMinimaxVideoTask(ctx context.Context, task MinimaxVideoTask) error {
	if s == nil || s.cache == nil {
		return fmt.Errorf("minimax video task cache is unavailable")
	}
	key := MinimaxVideoTaskKey(task.ID, task.UserID, task.APIKeyID)
	if key == "" {
		return fmt.Errorf("minimax video task key is invalid")
	}
	if strings.TrimSpace(task.CreatedAt) == "" {
		task.CreatedAt = GrokVideoPendingCreatedAtNow()
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return err
	}
	return s.cache.SetGrokVideoPendingBilling(ctx, key, payload, MinimaxVideoTaskTTL(grokVideoPendingBillingTTL(s.cfg)))
}

// LoadMinimaxVideoTask reads a task; returns nil when the entry is absent.
func (s *OpenAIGatewayService) LoadMinimaxVideoTask(ctx context.Context, taskID string, userID, apiKeyID int64) (*MinimaxVideoTask, error) {
	if s == nil || s.cache == nil {
		return nil, fmt.Errorf("minimax video task cache is unavailable")
	}
	key := MinimaxVideoTaskKey(taskID, userID, apiKeyID)
	if key == "" {
		return nil, fmt.Errorf("minimax video task key is invalid")
	}
	payload, err := s.cache.GetGrokVideoPendingBilling(ctx, key)
	if err != nil || len(payload) == 0 {
		return nil, err
	}
	var task MinimaxVideoTask
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, err
	}
	return &task, nil
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
	return &http.Client{Timeout: timeout}
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
	started := time.Now()
	_ = s.UpdateMinimaxVideoTask(ctx, taskID, userID, apiKeyID, func(task *MinimaxVideoTask) {
		task.Status = MinimaxVideoStatusProcessing
	})
	result, err := ExecuteMinimaxVideoTask(ctx, MinimaxVideoUpstreamClient(0), targetURL, bearerToken, body)
	if err != nil {
		slog.Warn("minimax_video.task_failed", "task_id", taskID, "error", err, "elapsed", time.Since(started))
		_ = s.UpdateMinimaxVideoTask(ctx, taskID, userID, apiKeyID, func(task *MinimaxVideoTask) {
			task.Status = MinimaxVideoStatusError
			task.Error = err.Error()
			task.CompletedAt = GrokVideoPendingCreatedAtNow()
		})
		return
	}
	defer func() {
		if rec := loadVideoTaskForPersist(ctx, taskID, userID, apiKeyID, s); rec != nil {
			billed := 0.0
			if rec.Status == MinimaxVideoStatusDone {
				billed = rec.UpstreamCostUSD // local_price 结算在下载阶段覆盖
			}
			_ = CompleteVideoTask(ctx, rec.ID, rec.UpstreamID, rec.Status, rec.Resolution,
				rec.VideoURL, rec.Error, rec.DurationSeconds,
				rec.UpstreamCostUSD, billed, VideoBillingModeDefault)
			// 结算：写回金额 + 记 usage_logs + 扣费
			if rec.Status == MinimaxVideoStatusDone {
				s.SettleMinimaxVideoTask(ctx, *rec)
			}
		}
	}()
	_ = s.UpdateMinimaxVideoTask(ctx, taskID, userID, apiKeyID, func(task *MinimaxVideoTask) {
		task.Status = MinimaxVideoStatusDone
		task.UpstreamID = result.UpstreamID
		task.VideoURL = result.VideoURL
		task.UpstreamCostUSD = result.UpstreamCostUSD
		if result.Resolution != "" {
			task.Resolution = result.Resolution
		}
		if result.DurationSeconds > 0 {
			task.DurationSeconds = result.DurationSeconds
		}
		task.CompletedAt = GrokVideoPendingCreatedAtNow()
	})
	slog.Info("minimax_video.task_done", "task_id", taskID, "elapsed", time.Since(started),
		"upstream_cost_usd", result.UpstreamCostUSD)
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

// StreamMinimaxVideoContent proxies the finished video bytes to the client.
func (s *OpenAIGatewayService) StreamMinimaxVideoContent(c *gin.Context, task *MinimaxVideoTask, baseURL, token string) error {
	if s == nil || c == nil || task == nil || strings.TrimSpace(task.VideoURL) == "" {
		return fmt.Errorf("video content is unavailable")
	}
	url := task.VideoURL
	if strings.HasPrefix(url, "/") {
		// Upstream reports a path that already contains the /v1 prefix, so the
		// configured base URL must be reduced to its origin before joining.
		if strings.TrimSpace(baseURL) == "" {
			baseURL = DefaultGetokenBaseURL
		}
		base := strings.TrimRight(baseURL, "/")
		base = strings.TrimSuffix(base, "/v1")
		url = base + url
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token = strings.TrimSpace(token); token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	resp, err := MinimaxVideoUpstreamClient(10 * time.Minute).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	contentType := resp.Header.Get("content-type")
	if contentType == "" {
		contentType = "video/mp4"
	}
	c.Header("content-type", contentType)
	c.Status(http.StatusOK)
	_, err = io.Copy(c.Writer, resp.Body)
	return err
}

// loadVideoTaskForPersist reads the in-cache task so the DB row can be updated
// from the same snapshot (keeps cache and table consistent).
func loadVideoTaskForPersist(ctx context.Context, taskID string, userID, apiKeyID int64, s *OpenAIGatewayService) *MinimaxVideoTask {
	if s == nil {
		return nil
	}
	task, err := s.LoadMinimaxVideoTask(ctx, taskID, userID, apiKeyID)
	if err != nil || task == nil {
		return nil
	}
	return task
}

