package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// StartMinimaxVideoRecovery retries durable settlements after transient failures
// or restarts. Only LL2 can repeat the original idempotent create. The caller owns ctx.
func (s *OpenAIGatewayService) StartMinimaxVideoRecovery(ctx context.Context, invalidators ...APIKeyAuthCacheInvalidator) {
	// A supplier request may legitimately use a 20-minute timeout. Run the two
	// providers and local ledger recovery independently, so one slow supplier
	// cannot consume another provider's task lifetime or delay committed charges.
	runVideoRecoveryLoops(ctx, time.Minute, []videoRecoveryLoop{
		{name: "settlement", timeout: 45 * time.Second, recover: func(c context.Context) error { return s.recoverMinimaxVideoTasks(c, invalidators...) }},
		{name: "seedance", timeout: 22 * time.Minute, recover: func(c context.Context) error { return s.recoverSeedanceVideoTasks(c, invalidators...) }},
		{name: "minimax", timeout: 22 * time.Minute, recover: func(c context.Context) error { return s.recoverKnownMinimaxVideoTasks(c, invalidators...) }},
	})
}

type videoRecoveryLoop struct {
	name    string
	timeout time.Duration
	recover func(context.Context) error
}

func runVideoRecoveryLoops(ctx context.Context, interval time.Duration, loops []videoRecoveryLoop) {
	var workers sync.WaitGroup
	for _, loop := range loops {
		workers.Add(1)
		go func(loop videoRecoveryLoop) {
			defer workers.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				if ctx.Err() != nil {
					return
				}
				runCtx, cancel := context.WithTimeout(ctx, loop.timeout)
				err := loop.recover(runCtx)
				cancel()
				if err != nil && ctx.Err() == nil {
					slog.Warn("video.recovery_failed", "worker", loop.name, "error", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}(loop)
	}
	workers.Wait()
}

func (s *OpenAIGatewayService) recoverMinimaxVideoTasks(ctx context.Context, invalidators ...APIKeyAuthCacheInvalidator) error {
	db := videoTaskHandle()
	if db == nil {
		return fmt.Errorf("video task database unavailable")
	}
	// A local timeout is not proof of provider cancellation. Keep reservations
	// and billing state intact, and route expired work to operator review.
	if _, err := db.ExecContext(ctx, `UPDATE video_tasks SET execution_state=execution_state ||
		jsonb_build_object('paused_reason','task_timeout','last_error','Task exceeded the configured automatic processing deadline','next_retry_at','')
		WHERE deleted_at IS NULL AND settled_at IS NULL AND status IN ('queued','processing','done')
		AND task_snapshot->'execution_policy' IS NOT NULL AND COALESCE(execution_state->>'paused_reason','')=''
		AND (COALESCE((execution_state->>'attempt_in_progress')::boolean,false)=false OR
		COALESCE(NULLIF(execution_state->>'last_attempt_at','')::timestamptz,created_at) +
		make_interval(secs => LEAST((task_snapshot->'execution_policy'->>'request_timeout_seconds')::integer,
		(task_snapshot->'execution_policy'->>'task_timeout_seconds')::integer) + 60) < now())
		AND COALESCE(NULLIF(execution_state->>'deadline_at','')::timestamptz,
		created_at + make_interval(secs => (task_snapshot->'execution_policy'->>'task_timeout_seconds')::integer)) <= now()`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE video_tasks SET execution_state=execution_state ||
		jsonb_build_object('paused_reason','submission_expired','last_error','Safe idempotent submission recovery window expired; reconcile with the supplier','next_retry_at','')
		WHERE status IN ('queued','processing') AND upstream_id='' AND deleted_at IS NULL
		AND task_snapshot->>'upstream_protocol'='ll2' AND created_at <= now()-interval '24 hours'
		AND COALESCE(execution_state->>'paused_reason','')=''`); err != nil {
		return err
	}
	// Legacy interrupted non-idempotent submits also need review, never an
	// automatic failure/refund or a newly paid upstream order.
	if _, err := db.ExecContext(ctx, `UPDATE video_tasks SET execution_state=execution_state ||
		jsonb_build_object('paused_reason','submission_uncertain','last_error','Provider acceptance is unknown; reconcile with the supplier','next_retry_at','')
		WHERE status IN ('queued','processing') AND upstream_id='' AND deleted_at IS NULL
		AND task_snapshot->'execution_policy' IS NULL AND COALESCE(task_snapshot->>'upstream_protocol','')<>'ll2'
		AND COALESCE(execution_state->>'paused_reason','')='' AND created_at < now()-interval '25 minutes'`); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT task_snapshot FROM video_tasks
		WHERE status='done' AND settled_at IS NULL AND task_snapshot IS NOT NULL
		AND jsonb_typeof(task_snapshot->'billing_snapshot')='object'
		ORDER BY COALESCE((task_snapshot->>'last_settlement_attempt')::timestamptz,created_at) LIMIT 100`)
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
		_, _ = db.ExecContext(ctx, `UPDATE video_tasks SET task_snapshot=jsonb_set(task_snapshot,'{last_settlement_attempt}',to_jsonb(now()::text)) WHERE task_id=$1 AND settled_at IS NULL`, task.ID)
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

func (s *OpenAIGatewayService) recoverKnownMinimaxVideoTasks(ctx context.Context, invalidators ...APIKeyAuthCacheInvalidator) error {
	rows, err := videoTaskHandle().QueryContext(ctx, `SELECT task_id,user_id,api_key_id FROM video_tasks
		WHERE provider='minimax' AND deleted_at IS NULL
		AND COALESCE(execution_state->>'paused_reason','')=''
		AND (NULLIF(execution_state->>'next_retry_at','') IS NULL OR (execution_state->>'next_retry_at')::timestamptz <= now()) AND
		(upstream_id<>'' OR (task_snapshot->>'upstream_protocol'='ll2' AND created_at>now()-interval '24 hours')) AND
		(status IN ('queued','processing') OR (status='done' AND settled_at IS NULL AND
		 ((billing_mode='upstream_cost' AND COALESCE((task_snapshot->>'upstream_cost_reported')::boolean,false)=false) OR
		  (task_snapshot->>'upstream_protocol'='ll2' AND COALESCE((task_snapshot->>'output_seconds_reported')::boolean,false)=false))))
		ORDER BY COALESCE((task_snapshot->>'last_polled_at')::timestamptz,created_at) LIMIT 100`)
	if err != nil {
		return err
	}
	type pending struct {
		id        string
		user, key int64
	}
	var tasks []pending
	for rows.Next() {
		var task pending
		if err := rows.Scan(&task.id, &task.user, &task.key); err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, task)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range tasks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		task, err := s.LoadMinimaxVideoTask(ctx, p.id, p.user, p.key)
		if err != nil {
			return err
		}
		if task == nil {
			continue
		}
		if task.UpstreamProtocol == MinimaxVideoProtocolLL2 && task.UpstreamID == "" {
			_, _ = videoTaskHandle().ExecContext(ctx, `UPDATE video_tasks SET task_snapshot=jsonb_set(task_snapshot,'{last_polled_at}',to_jsonb(now()::text)) WHERE task_id=$1 AND deleted_at IS NULL`, task.ID)
			if err := s.RunLL2MinimaxVideoTask(ctx, p.id, p.user, p.key); err != nil {
				slog.Warn("minimax_video.ll2_submission_retry_pending", "task_id", p.id, "error", err)
			}
			continue
		}
		if err := s.PollMinimaxVideoTask(ctx, task); err != nil {
			slog.Warn("minimax_video.poll_retry_failed", "task_id", p.id, "error", err)
		}
		for _, i := range invalidators {
			if i != nil {
				i.InvalidateAuthCacheByUserID(ctx, p.user)
			}
		}
	}
	return nil
}

func (s *OpenAIGatewayService) recoverSeedanceVideoTasks(ctx context.Context, invalidators ...APIKeyAuthCacheInvalidator) error {
	rows, err := videoTaskHandle().QueryContext(ctx, `SELECT task_id,user_id,api_key_id FROM video_tasks
		WHERE provider='seedance' AND COALESCE(execution_state->>'paused_reason','')=''
		AND (NULLIF(execution_state->>'next_retry_at','') IS NULL OR (execution_state->>'next_retry_at')::timestamptz <= now()) AND (status IN ('queued','processing') OR
		(status='done' AND settled_at IS NULL AND
		 ((billing_mode='usage_tokens' AND COALESCE((task_snapshot->>'usage_reported')::boolean,false)=false) OR
		  (billing_mode='upstream_cost' AND COALESCE((task_snapshot->>'upstream_cost_reported')::boolean,false)=false))))
		AND upstream_id<>'' AND deleted_at IS NULL
		ORDER BY COALESCE((task_snapshot->>'last_polled_at')::timestamptz,created_at) LIMIT 100`)
	if err != nil {
		return err
	}
	type pending struct {
		id            string
		userID, keyID int64
	}
	var pendingTasks []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.userID, &p.keyID); err != nil {
			rows.Close()
			return err
		}
		pendingTasks = append(pendingTasks, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, task := range pendingTasks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := s.pollSeedanceTask(ctx, task.id, task.userID, task.keyID); err != nil {
			slog.Warn("seedance_video.poll_retry_failed", "task_id", task.id, "error", err)
		}
		for _, i := range invalidators {
			if i != nil {
				i.InvalidateAuthCacheByUserID(ctx, task.userID)
			}
		}
	}
	return nil
}
