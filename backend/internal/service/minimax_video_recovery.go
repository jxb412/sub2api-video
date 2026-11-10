package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// StartMinimaxVideoRecovery retries durable settlements after transient failures
// or restarts. It never repeats an upstream create request. The caller owns ctx.
func (s *OpenAIGatewayService) StartMinimaxVideoRecovery(ctx context.Context, invalidators ...APIKeyAuthCacheInvalidator) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		runCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		if err := s.recoverMinimaxVideoTasks(runCtx, invalidators...); err != nil && ctx.Err() == nil {
			slog.Error("minimax_video.recovery_failed", "error", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *OpenAIGatewayService) recoverMinimaxVideoTasks(ctx context.Context, invalidators ...APIKeyAuthCacheInvalidator) error {
	db := videoTaskHandle()
	if db == nil {
		return fmt.Errorf("video task database unavailable")
	}
	// The worker has a 20 minute deadline; a 5 minute grace period avoids
	// racing an active request in another replica. Old rows are repaired too.
	if _, err := db.ExecContext(ctx, `UPDATE video_tasks SET status='error',
		error_message='Video generation interrupted or timed out', completed_at=now()
		WHERE status IN ('queued','processing') AND created_at < now()-interval '25 minutes'`); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT task_snapshot FROM video_tasks
		WHERE status='done' AND settled_at IS NULL AND task_snapshot IS NOT NULL
		ORDER BY created_at LIMIT 100`)
	if err != nil {
		return err
	}
	var tasks []MinimaxVideoTask
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return err
		}
		var task MinimaxVideoTask
		if err := json.Unmarshal(payload, &task); err != nil {
			rows.Close()
			return err
		}
		if task.BillingSnapshot != nil {
			tasks = append(tasks, task)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := s.settleMinimaxVideoTask(ctx, task); err != nil {
			slog.Error("minimax_video.settlement_retry_failed", "task_id", task.ID, "error", err)
		}
		// A committed charge may precede a failed usage-log write. Refresh auth
		// quota state after every attempt, including retries after that failure.
		for _, invalidator := range invalidators {
			if invalidator != nil {
				invalidator.InvalidateAuthCacheByUserID(ctx, task.UserID)
			}
		}
	}
	return nil
}
