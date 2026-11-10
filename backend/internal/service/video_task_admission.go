package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var ErrVideoTaskConcurrency = infraerrors.TooManyRequests("VIDEO_CONCURRENCY_LIMIT", "Active video tasks have reached the user or account concurrency limit")

// AdmitVideoTask makes the queued row the durable reservation. Redis continues
// to coordinate videos with ordinary requests, but losing its TTL or restarting
// a worker cannot remove admission protection between video tasks. No money is
// deducted here. Unknown provider usage is an estimate, never a final-cost cap.
func (s *OpenAIGatewayService) AdmitVideoTask(ctx context.Context, taskPtr *MinimaxVideoTask, key *APIKey, account *Account, subscription *UserSubscription, userConcurrency int) error {
	if taskPtr == nil {
		return ErrBillingServiceUnavailable.WithCause(fmt.Errorf("missing video task"))
	}
	task := *taskPtr
	if s == nil || key == nil || account == nil || task.BillingSnapshot == nil ||
		task.ID == "" || task.Status != MinimaxVideoStatusQueued || task.UserID <= 0 ||
		task.UserID != key.UserID || task.APIKeyID != key.ID || task.AccountID != account.ID ||
		key.GroupID == nil || task.GroupID != *key.GroupID {
		return ErrBillingServiceUnavailable.WithCause(fmt.Errorf("invalid durable video admission"))
	}
	snapshot := *task.BillingSnapshot
	task.BillingSnapshot = &snapshot
	if s.cfg != nil {
		task.BillingSnapshot.SimpleMode = s.cfg.RunMode == config.RunModeSimple
		task.BillingSnapshot.SimpleModeKeyRateLimitEnabled = s.cfg.SimpleModeKeyRateLimitEnabled
	}
	if task.Provider == "" {
		task.Provider = "minimax"
	}
	if task.CreatedAt == "" {
		task.CreatedAt = GrokVideoPendingCreatedAtNow()
	}
	if task.BillingMode == VideoBillingModeLocalPrice && task.PriceConfigured {
		task.AdmissionCost = ComputeMinimaxVideoSettlement(task, task.BillingMode, nil, task.RateMultiplier).BilledCost
		task.AdmissionCostKnown = true
	} else if task.AdmissionCost == 0 && task.BillingMode == VideoBillingModeUsageTokens && task.TokenPricingConfigured {
		// Derive from the immutable tariff even when Redis reservation is disabled.
		outputTokens := defaultInflightDefaultMaxTokens
		if s.cfg != nil && s.cfg.Billing.InflightReservation.DefaultMaxTokens > 0 {
			outputTokens = s.cfg.Billing.InflightReservation.DefaultMaxTokens
		}
		task.AdmissionCost = QuantizeUsageBillingAmount(float64(outputTokens) * task.OutputPricePerToken * task.RateMultiplier)
	} else if task.AdmissionCost == 0 && task.BillingMode == VideoBillingModeUpstreamCost {
		if amount, ok := s.EstimateInflightReservation(ctx, key, InflightEstimateRequest{
			Model: task.BillingModel, Kind: InflightEstimateVideo, Units: 1,
			VideoResolution: task.Resolution, VideoDurationSeconds: task.DurationSeconds,
		}); ok {
			task.AdmissionCost = amount
		}
	}
	if !validVideoAmount(task.AdmissionCost) {
		return ErrBillingServiceUnavailable.WithCause(fmt.Errorf("invalid video admission amount"))
	}
	task.AdmissionCost = QuantizeUsageBillingAmount(task.AdmissionCost)
	db := videoTaskHandle()
	if db == nil {
		return ErrBillingServiceUnavailable.WithCause(fmt.Errorf("video task database unavailable"))
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ErrBillingServiceUnavailable.WithCause(err)
	}
	defer tx.Rollback()
	// Every admission takes these namespaces in the same order. No network calls
	// happen while holding locks. Transaction locks disappear on process failure.
	for _, lock := range []string{fmt.Sprintf("video-admit-user:%d", task.UserID), fmt.Sprintf("video-admit-account:%d", task.AccountID)} {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", lock); err != nil {
			return ErrBillingServiceUnavailable.WithCause(err)
		}
	}
	// Match ledger lock order: payer, key, account. Locking the payer before
	// reading dedup makes a committed debit and its reservation mutually exclusive.
	var balance float64
	var userStatus string
	if err := tx.QueryRowContext(ctx, `SELECT balance,status FROM users
		WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, task.UserID).Scan(&balance, &userStatus); err != nil {
		return ErrBillingServiceUnavailable.WithCause(err)
	}
	if userStatus != StatusActive {
		return infraerrors.Forbidden("USER_DISABLED", "User is disabled")
	}
	var sub *UserSubscription
	var group *Group
	if task.BillingSnapshot.SubscriptionID > 0 && !task.BillingSnapshot.SimpleMode {
		if subscription == nil || subscription.ID != task.BillingSnapshot.SubscriptionID {
			return ErrSubscriptionInvalid
		}
		sub, group, err = readVideoAdmissionSubscription(ctx, tx, task)
		if err != nil {
			return err
		}
	}
	freshKey, err := readVideoAdmissionKey(ctx, tx, task)
	if err != nil {
		return err
	}
	// Authentication/scheduling may have used cached key data. The accepted
	// snapshot must use the same fresh policy that was checked under this lock.
	task.BillingSnapshot.APIKeyQuota = freshKey.Quota > 0
	task.BillingSnapshot.APIKeyRateLimits = freshKey.HasRateLimits()
	var accountConcurrency int
	var accountStatus string
	if err := tx.QueryRowContext(ctx, `SELECT concurrency,status FROM accounts
		WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, task.AccountID).Scan(&accountConcurrency, &accountStatus); err != nil {
		return ErrBillingServiceUnavailable.WithCause(err)
	}
	if accountStatus != StatusActive {
		return infraerrors.ServiceUnavailable("VIDEO_ACCOUNT_UNAVAILABLE", "Video account is unavailable")
	}
	var userActive, accountActive int
	if err := tx.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE user_id=$1), count(*) FILTER (WHERE account_id=$2)
		FROM video_tasks WHERE status IN ('queued','processing') AND deleted_at IS NULL
		AND (user_id=$1 OR account_id=$2)`, task.UserID, task.AccountID).Scan(&userActive, &accountActive); err != nil {
		return ErrBillingServiceUnavailable.WithCause(err)
	}
	if (userConcurrency > 0 && userActive >= userConcurrency) || (accountConcurrency > 0 && accountActive >= accountConcurrency) {
		return ErrVideoTaskConcurrency
	}
	holds, err := readVideoAdmissionHolds(ctx, tx, task.UserID, task.APIKeyID, task.BillingSnapshot.SubscriptionID)
	if err != nil {
		return ErrBillingServiceUnavailable.WithCause(err)
	}
	autoReset := s.cfg != nil && s.cfg.SubscriptionMaintenance.AutoResetEnabled
	if err := checkVideoAdmissionFunds(task, freshKey, sub, group, balance, holds, time.Now(), autoReset); err != nil {
		return err
	}
	if err := insertVideoAdmissionTask(ctx, tx, task); err != nil {
		return ErrBillingServiceUnavailable.WithCause(err)
	}
	if err := tx.Commit(); err != nil {
		return ErrBillingServiceUnavailable.WithCause(err)
	}
	*taskPtr = task
	return nil
}

type videoAdmissionHolds struct{ balance, subscription, keyQuota, keyRate float64 }

func readVideoAdmissionHolds(ctx context.Context, tx *sql.Tx, userID, keyID, subscriptionID int64) (videoAdmissionHolds, error) {
	var holds videoAdmissionHolds
	rows, err := tx.QueryContext(ctx, `SELECT t.task_snapshot,t.status,t.api_key_id
		FROM video_tasks t WHERE t.user_id=$1 AND t.deleted_at IS NULL
		AND t.status IN ('queued','processing','done') AND t.settled_at IS NULL AND t.task_snapshot IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM usage_billing_dedup d WHERE d.request_id=t.task_id AND d.api_key_id=t.api_key_id)
		AND NOT EXISTS (SELECT 1 FROM usage_billing_dedup_archive d WHERE d.request_id=t.task_id AND d.api_key_id=t.api_key_id)`, userID)
	if err != nil {
		return holds, err
	}
	defer rows.Close()
	for rows.Next() {
		var body []byte
		var status string
		var ownerKey int64
		if err := rows.Scan(&body, &status, &ownerKey); err != nil {
			return holds, err
		}
		var task MinimaxVideoTask
		if err := json.Unmarshal(body, &task); err != nil {
			return holds, fmt.Errorf("invalid outstanding video snapshot: %w", err)
		}
		task.Status = status
		if task.BillingSnapshot == nil {
			continue // Legacy rows have no billing policy and are not charged retroactively.
		}
		amount, err := durableVideoAdmissionAmount(task)
		if err != nil {
			return holds, err
		}
		p := task.BillingSnapshot
		if !p.SimpleMode {
			if p.SubscriptionID == 0 {
				holds.balance += amount
			} else if p.SubscriptionID == subscriptionID {
				holds.subscription += amount
			}
		}
		if ownerKey == keyID {
			if !p.SimpleMode && p.APIKeyQuota {
				holds.keyQuota += amount
			}
			if p.APIKeyRateLimits && (!p.SimpleMode || p.SimpleModeKeyRateLimitEnabled) {
				holds.keyRate += amount
			}
		}
	}
	if err := rows.Err(); err != nil {
		return holds, err
	}
	if !validVideoAmount(holds.balance) || !validVideoAmount(holds.subscription) || !validVideoAmount(holds.keyQuota) || !validVideoAmount(holds.keyRate) {
		return holds, fmt.Errorf("invalid outstanding video admission total")
	}
	return holds, nil
}

func durableVideoAdmissionAmount(task MinimaxVideoTask) (float64, error) {
	amount := task.AdmissionCost
	knownFinal := task.Status == MinimaxVideoStatusDone &&
		((task.BillingMode == VideoBillingModeUsageTokens && task.TokenPricingConfigured && task.UsageReported) ||
			(task.BillingMode == VideoBillingModeUpstreamCost && (task.UpstreamCostReported || task.UpstreamCostUSD > 0)))
	if knownFinal || (task.BillingMode == VideoBillingModeLocalPrice && task.PriceConfigured) {
		amount = ComputeMinimaxVideoSettlement(task, task.BillingMode, nil, task.RateMultiplier).BilledCost
	} else if amount == 0 && task.BillingMode == VideoBillingModeUsageTokens && task.TokenPricingConfigured {
		// Old accepted snapshots may predate admission metadata. A conservative
		// default estimate restores headroom without inventing a final token count.
		amount = float64(defaultInflightDefaultMaxTokens) * task.OutputPricePerToken * task.RateMultiplier
	}
	if !validVideoAmount(amount) {
		return 0, fmt.Errorf("invalid outstanding video admission amount")
	}
	return QuantizeUsageBillingAmount(amount), nil
}

func readVideoAdmissionKey(ctx context.Context, tx *sql.Tx, task MinimaxVideoTask) (*APIKey, error) {
	k := &APIKey{}
	var groupID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT user_id,group_id,status,expires_at,quota,quota_used,
		rate_limit_5h,rate_limit_1d,rate_limit_7d,usage_5h,usage_1d,usage_7d,
		window_5h_start,window_1d_start,window_7d_start FROM api_keys
		WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, task.APIKeyID).Scan(
		&k.UserID, &groupID, &k.Status, &k.ExpiresAt, &k.Quota, &k.QuotaUsed,
		&k.RateLimit5h, &k.RateLimit1d, &k.RateLimit7d, &k.Usage5h, &k.Usage1d, &k.Usage7d,
		&k.Window5hStart, &k.Window1dStart, &k.Window7dStart)
	if err != nil {
		return nil, ErrBillingServiceUnavailable.WithCause(err)
	}
	if k.UserID != task.UserID || !groupID.Valid || groupID.Int64 != task.GroupID || !k.IsActive() || k.IsExpired() {
		return nil, infraerrors.Forbidden("VIDEO_KEY_CHANGED", "Video API key is unavailable or its group changed")
	}
	return k, nil
}

func readVideoAdmissionSubscription(ctx context.Context, tx *sql.Tx, task MinimaxVideoTask) (*UserSubscription, *Group, error) {
	sub, group := &UserSubscription{}, &Group{}
	err := tx.QueryRowContext(ctx, `SELECT s.user_id,s.group_id,s.status,s.starts_at,s.expires_at,
		s.daily_window_start,s.weekly_window_start,s.monthly_window_start,
		s.daily_usage_usd,s.weekly_usage_usd,s.monthly_usage_usd,
		g.daily_limit_usd,g.weekly_limit_usd,g.monthly_limit_usd
		FROM user_subscriptions s JOIN groups g ON g.id=s.group_id AND g.deleted_at IS NULL
		WHERE s.id=$1 AND s.deleted_at IS NULL FOR UPDATE OF s`, task.BillingSnapshot.SubscriptionID).Scan(
		&sub.UserID, &sub.GroupID, &sub.Status, &sub.StartsAt, &sub.ExpiresAt,
		&sub.DailyWindowStart, &sub.WeeklyWindowStart, &sub.MonthlyWindowStart,
		&sub.DailyUsageUSD, &sub.WeeklyUsageUSD, &sub.MonthlyUsageUSD,
		&group.DailyLimitUSD, &group.WeeklyLimitUSD, &group.MonthlyLimitUSD)
	if err != nil {
		return nil, nil, ErrBillingServiceUnavailable.WithCause(err)
	}
	if sub.UserID != task.UserID || sub.GroupID != task.GroupID || !sub.IsActive() || time.Now().Before(sub.StartsAt) {
		return nil, nil, ErrSubscriptionInvalid
	}
	return sub, group, nil
}

func checkVideoAdmissionFunds(task MinimaxVideoTask, key *APIKey, sub *UserSubscription, group *Group, balance float64, holds videoAdmissionHolds, now time.Time, autoReset bool) error {
	// Monetary comparisons use the same 8-decimal unit as the ledger.
	exceeds := func(used, held, amount, limit float64) bool {
		return QuantizeUsageBillingAmount(used+held+amount) > QuantizeUsageBillingAmount(limit)
	}
	amount := task.AdmissionCost
	if !task.BillingSnapshot.SimpleMode {
		if sub == nil {
			if exceeds(0, holds.balance, amount, balance) {
				return ErrInsufficientBalance
			}
		} else {
			daily, weekly, monthly := sub.DailyUsageUSD, sub.WeeklyUsageUSD, sub.MonthlyUsageUSD
			if autoReset && sub.canAutomaticallyResetDailyAt(now) {
				daily = 0
			}
			if autoReset && sub.canAutomaticallyResetWeeklyAt(now) {
				weekly = 0
			}
			if autoReset && sub.canAutomaticallyResetMonthlyAt(now) {
				monthly = 0
			}
			if group.HasDailyLimit() && exceeds(daily, holds.subscription, amount, *group.DailyLimitUSD) {
				return ErrDailyLimitExceeded
			}
			if group.HasWeeklyLimit() && exceeds(weekly, holds.subscription, amount, *group.WeeklyLimitUSD) {
				return ErrWeeklyLimitExceeded
			}
			if group.HasMonthlyLimit() && exceeds(monthly, holds.subscription, amount, *group.MonthlyLimitUSD) {
				return ErrMonthlyLimitExceeded
			}
		}
		if key.Quota > 0 && exceeds(key.QuotaUsed, holds.keyQuota, amount, key.Quota) {
			return ErrAPIKeyQuotaExhausted
		}
	}
	if !task.BillingSnapshot.SimpleMode || task.BillingSnapshot.SimpleModeKeyRateLimitEnabled {
		if key.RateLimit5h > 0 && exceeds(key.EffectiveUsage5h(), holds.keyRate, amount, key.RateLimit5h) {
			return ErrAPIKeyRateLimit5hExceeded
		}
		if key.RateLimit1d > 0 && exceeds(key.EffectiveUsage1d(), holds.keyRate, amount, key.RateLimit1d) {
			return ErrAPIKeyRateLimit1dExceeded
		}
		if key.RateLimit7d > 0 && exceeds(key.EffectiveUsage7d(), holds.keyRate, amount, key.RateLimit7d) {
			return ErrAPIKeyRateLimit7dExceeded
		}
	}
	return nil
}

func insertVideoAdmissionTask(ctx context.Context, tx *sql.Tx, task MinimaxVideoTask) error {
	created, err := time.Parse(time.RFC3339Nano, task.CreatedAt)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return err
	}
	// No upsert: every admitted request owns a new gateway-generated ID. The
	// actual insert and every check share one transaction, so concurrent creates
	// cannot both spend the same durable headroom.
	_, err = tx.ExecContext(ctx, `INSERT INTO video_tasks
		(task_id,provider,model,upstream_model,prompt,status,resolution,duration_seconds,
		user_id,api_key_id,group_id,account_id,billing_mode,created_at,task_snapshot)
		VALUES ($1,$2,$3,$4,$5,'queued',$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		task.ID, task.Provider, task.Model, task.UpstreamModel, task.Prompt, task.Resolution,
		task.DurationSeconds, task.UserID, task.APIKeyID, task.GroupID, task.AccountID,
		task.BillingMode, created, string(payload))
	return err
}
