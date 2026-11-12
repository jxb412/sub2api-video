package service

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func videoAdmissionFixture() (MinimaxVideoTask, *APIKey, *Account) {
	task := minimaxBillableTask()
	task.Status, task.VideoURL, task.CompletedAt = MinimaxVideoStatusQueued, "", ""
	return task, &APIKey{ID: task.APIKeyID, UserID: task.UserID, GroupID: &task.GroupID}, &Account{ID: task.AccountID}
}

func expectVideoAdmissionLocks(mock sqlmock.Sqlmock, balance float64) {
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs("video-admit-user:1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs("video-admit-account:3").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT balance,status FROM users.*FOR UPDATE").WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"balance", "status"}).AddRow(balance, StatusActive))
}

func expectVideoAdmissionKey(mock sqlmock.Sqlmock, quota, used float64) {
	columns := strings.Split("user_id,group_id,status,expires_at,quota,quota_used,rate_limit_5h,rate_limit_1d,rate_limit_7d,usage_5h,usage_1d,usage_7d,window_5h_start,window_1d_start,window_7d_start", ",")
	mock.ExpectQuery("SELECT user_id,group_id,status,expires_at,quota,quota_used.*FROM api_keys.*FOR UPDATE").WithArgs(int64(2)).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(1, 4, StatusActive, nil, quota, used, 10, 0, 0, 0, 0, 0, time.Now(), nil, nil))
}

func expectVideoAdmissionConcurrency(mock sqlmock.Sqlmock, limit, userActive, accountActive int) {
	mock.ExpectQuery("SELECT concurrency,status FROM accounts.*FOR UPDATE").WithArgs(int64(3)).
		WillReturnRows(sqlmock.NewRows([]string{"concurrency", "status"}).AddRow(limit, StatusActive))
	mock.ExpectQuery("SELECT.*count.*FROM video_tasks WHERE status IN").WithArgs(int64(1), int64(3)).
		WillReturnRows(sqlmock.NewRows([]string{"user_count", "account_count"}).AddRow(userActive, accountActive))
}

func expectVideoAdmissionHolds(t *testing.T, mock sqlmock.Sqlmock, tasks ...MinimaxVideoTask) {
	t.Helper()
	rows := sqlmock.NewRows([]string{"task_snapshot", "status", "api_key_id"})
	for _, task := range tasks {
		body, err := json.Marshal(task)
		require.NoError(t, err)
		rows.AddRow(string(body), task.Status, task.APIKeyID)
	}
	// Both ledger tables must exclude already applied debits even when the
	// worker has not yet written settled_at on its task row.
	mock.ExpectQuery("SELECT t.task_snapshot,t.status,t.api_key_id.*t.status IN \\('queued','processing','done'\\).*t.settled_at IS NULL.*NOT EXISTS.*usage_billing_dedup d.*NOT EXISTS.*usage_billing_dedup_archive d").
		WithArgs(int64(1)).WillReturnRows(rows)
}

type admissionSnapshotArgument struct{ validate func(MinimaxVideoTask) bool }

func (a admissionSnapshotArgument) Match(value driver.Value) bool {
	body, ok := value.(string)
	if !ok {
		return false
	}
	var task MinimaxVideoTask
	return json.Unmarshal([]byte(body), &task) == nil && a.validate(task)
}

func TestVideoAdmissionPersistsReservationAndFreshPolicyAtomically(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	task, key, account := videoAdmissionFixture()
	task.BillingSnapshot.APIKeyQuota, task.BillingSnapshot.APIKeyRateLimits = false, false
	expectVideoAdmissionLocks(mock, 1)
	expectVideoAdmissionKey(mock, 10, 0)
	expectVideoAdmissionConcurrency(mock, 2, 0, 0)
	expectVideoAdmissionHolds(t, mock)
	mock.ExpectExec("INSERT INTO video_tasks").WithArgs(task.ID, "minimax", task.Model, task.UpstreamModel, task.Prompt,
		task.Resolution, task.DurationSeconds, task.UserID, task.APIKeyID, task.GroupID, task.AccountID,
		task.BillingMode, sqlmock.AnyArg(), admissionSnapshotArgument{validate: func(saved MinimaxVideoTask) bool {
			return saved.AdmissionCost == 0.5 && saved.AdmissionCostKnown && saved.BillingSnapshot.APIKeyQuota && saved.BillingSnapshot.APIKeyRateLimits
		}}).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	require.NoError(t, (&OpenAIGatewayService{}).AdmitVideoTask(context.Background(), &task, key, account, nil, 2))
	require.Equal(t, 0.5, task.AdmissionCost, "background worker must retain the durable reservation")
	require.True(t, task.BillingSnapshot.APIKeyQuota)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVideoAdmissionCountsOtherKeysAndDoneUnsettledBalance(t *testing.T) {
	for _, status := range []string{MinimaxVideoStatusProcessing, MinimaxVideoStatusDone} {
		t.Run(status, func(t *testing.T) {
			mock := minimaxTaskMockDB(t)
			task, key, account := videoAdmissionFixture()
			held := task
			held.ID, held.APIKeyID, held.Status = "mmv_other", 99, status
			expectVideoAdmissionLocks(mock, 0.9)
			expectVideoAdmissionKey(mock, 10, 0)
			expectVideoAdmissionConcurrency(mock, 2, 1, 1)
			expectVideoAdmissionHolds(t, mock, held)
			mock.ExpectRollback()
			err := (&OpenAIGatewayService{}).AdmitVideoTask(context.Background(), &task, key, account, nil, 2)
			require.ErrorIs(t, err, ErrInsufficientBalance)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestVideoAdmissionRejectsDurableConcurrencyWithoutRedis(t *testing.T) {
	for _, counts := range [][2]int{{2, 0}, {0, 2}} {
		mock := minimaxTaskMockDB(t)
		task, key, account := videoAdmissionFixture()
		expectVideoAdmissionLocks(mock, 100)
		expectVideoAdmissionKey(mock, 10, 0)
		expectVideoAdmissionConcurrency(mock, 2, counts[0], counts[1])
		mock.ExpectRollback()
		require.ErrorIs(t, (&OpenAIGatewayService{}).AdmitVideoTask(context.Background(), &task, key, account, nil, 2), ErrVideoTaskConcurrency)
		require.NoError(t, mock.ExpectationsWereMet())
	}
}

func TestVideoAdmissionRollsBackFailedInsert(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	task, key, account := videoAdmissionFixture()
	expectVideoAdmissionLocks(mock, 1)
	expectVideoAdmissionKey(mock, 10, 0)
	expectVideoAdmissionConcurrency(mock, 2, 0, 0)
	expectVideoAdmissionHolds(t, mock)
	mock.ExpectExec("INSERT INTO video_tasks").WillReturnError(errors.New("insert failed"))
	mock.ExpectRollback()
	require.ErrorContains(t, (&OpenAIGatewayService{}).AdmitVideoTask(context.Background(), &task, key, account, nil, 2), "insert failed")
	require.Zero(t, task.AdmissionCost, "caller must not treat a rolled-back task as admitted")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVideoAdmissionHoldsSeparatePayersAndKeyPolicies(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	mock.ExpectBegin()
	tx, err := videoTaskHandle().Begin()
	require.NoError(t, err)
	task, _, _ := videoAdmissionFixture()
	otherKey := task
	otherKey.APIKeyID = 99
	subTask := task
	subTask.BillingSnapshot = &MinimaxVideoBillingSnapshot{SubscriptionID: 7, APIKeyRateLimits: true}
	simple := task
	simple.BillingSnapshot = &MinimaxVideoBillingSnapshot{SimpleMode: true, SimpleModeKeyRateLimitEnabled: true, APIKeyRateLimits: true}
	legacy := task
	legacy.BillingSnapshot = nil
	expectVideoAdmissionHolds(t, mock, task, otherKey, subTask, simple, legacy)
	holds, err := readVideoAdmissionHolds(context.Background(), tx, task.UserID, task.APIKeyID, 7)
	require.NoError(t, err)
	require.Equal(t, videoAdmissionHolds{balance: 1, subscription: 0.5, keyQuota: 0.5, keyRate: 1.5}, holds)
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDurableVideoAdmissionUsesFinalCostOnlyWhenKnown(t *testing.T) {
	task, _, _ := videoAdmissionFixture()
	task.BillingMode, task.AdmissionCost = VideoBillingModeUpstreamCost, 0.8
	task.UpstreamCostUSD, task.RateMultiplier = 0.9, 2
	amount, err := durableVideoAdmissionAmount(task)
	require.NoError(t, err)
	require.Equal(t, 0.8, amount)
	task.Status, task.UpstreamCostReported = MinimaxVideoStatusDone, true
	amount, err = durableVideoAdmissionAmount(task)
	require.NoError(t, err)
	require.Equal(t, 1.8, amount)
	task.BillingMode, task.TokenPricingConfigured, task.UsageReported = VideoBillingModeUsageTokens, true, true
	task.OutputTokens, task.OutputPricePerToken = 150000, 0.00001
	amount, err = durableVideoAdmissionAmount(task)
	require.NoError(t, err)
	require.Equal(t, 3.0, amount)
	task.BillingMode, task.PricePerSecond = VideoBillingModeLocalPrice, 0
	amount, err = durableVideoAdmissionAmount(task)
	require.NoError(t, err)
	require.Zero(t, amount, "explicit zero price must not consume an old estimate")
	task.BillingMode, task.AdmissionCost = VideoBillingModeUpstreamCost, math.Inf(1)
	task.UpstreamCostReported, task.UpstreamCostUSD = false, 0
	_, err = durableVideoAdmissionAmount(task)
	require.Error(t, err)
}

func TestVideoAdmissionFundsEnforceAllKeyWindows(t *testing.T) {
	now := time.Now()
	task, _, _ := videoAdmissionFixture()
	task.AdmissionCost = 0.3
	tests := []struct {
		name string
		key  APIKey
		want error
	}{
		{"quota", APIKey{Quota: 1, QuotaUsed: 0.3}, ErrAPIKeyQuotaExhausted},
		{"5h", APIKey{RateLimit5h: 1, Usage5h: 0.3, Window5hStart: &now}, ErrAPIKeyRateLimit5hExceeded},
		{"day", APIKey{RateLimit1d: 1, Usage1d: 0.3, Window1dStart: &now}, ErrAPIKeyRateLimit1dExceeded},
		{"week", APIKey{RateLimit7d: 1, Usage7d: 0.3, Window7dStart: &now}, ErrAPIKeyRateLimit7dExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkVideoAdmissionFunds(task, &tc.key, nil, nil, 10, videoAdmissionHolds{keyQuota: 0.5, keyRate: 0.5}, now, false)
			require.ErrorIs(t, err, tc.want)
		})
	}
	expired := now.Add(-8 * 24 * time.Hour)
	key := &APIKey{RateLimit5h: 1, RateLimit1d: 1, RateLimit7d: 1, Usage5h: 100, Usage1d: 100, Usage7d: 100, Window5hStart: &expired, Window1dStart: &expired, Window7dStart: &expired}
	require.NoError(t, checkVideoAdmissionFunds(task, key, nil, nil, 10, videoAdmissionHolds{keyRate: 0.7}, now, false))
	task.AdmissionCost = 0
	require.NoError(t, checkVideoAdmissionFunds(task, &APIKey{}, nil, nil, 0, videoAdmissionHolds{}, now, false))
}

func TestVideoAdmissionSubscriptionAndSimpleMode(t *testing.T) {
	now := time.Now()
	task, key, _ := videoAdmissionFixture()
	task.AdmissionCost, task.BillingSnapshot.SubscriptionID = 0.5, 7
	limit := 1.0
	sub := &UserSubscription{StartsAt: now.Add(-time.Hour), ExpiresAt: now.AddDate(0, 2, 0), DailyUsageUSD: 0.2, WeeklyUsageUSD: 0.2, MonthlyUsageUSD: 0.2}
	for _, group := range []*Group{{DailyLimitUSD: &limit}, {WeeklyLimitUSD: &limit}, {MonthlyLimitUSD: &limit}} {
		err := checkVideoAdmissionFunds(task, key, sub, group, 0, videoAdmissionHolds{balance: 100, subscription: 0.4}, now, false)
		require.Error(t, err)
		require.NoError(t, checkVideoAdmissionFunds(task, key, sub, group, 0, videoAdmissionHolds{balance: 100, subscription: 0.3}, now, false))
	}
	task.BillingSnapshot.SimpleMode = true
	key.Quota, key.QuotaUsed, key.RateLimit5h, key.Usage5h, key.Window5hStart = 1, 100, 1, 100, &now
	require.NoError(t, checkVideoAdmissionFunds(task, key, nil, nil, -1, videoAdmissionHolds{balance: 100, keyRate: 100}, now, false))
	task.BillingSnapshot.SimpleModeKeyRateLimitEnabled = true
	require.ErrorIs(t, checkVideoAdmissionFunds(task, key, nil, nil, -1, videoAdmissionHolds{}, now, false), ErrAPIKeyRateLimit5hExceeded)
}

func TestVideoAdmissionSubscriptionRespectsDisabledAutomaticReset(t *testing.T) {
	now := time.Now()
	expiredWindow := now.Add(-31 * 24 * time.Hour)
	task, key, _ := videoAdmissionFixture()
	task.AdmissionCost, task.BillingSnapshot.SubscriptionID = 0.5, 7
	limit := 1.0
	sub := &UserSubscription{
		StartsAt: now.Add(-60 * 24 * time.Hour), ExpiresAt: now.Add(60 * 24 * time.Hour),
		DailyWindowStart: &expiredWindow, WeeklyWindowStart: &expiredWindow, MonthlyWindowStart: &expiredWindow,
		DailyUsageUSD: 1, WeeklyUsageUSD: 1, MonthlyUsageUSD: 1,
	}
	cases := []struct {
		name  string
		group Group
		want  error
	}{
		{"daily", Group{DailyLimitUSD: &limit}, ErrDailyLimitExceeded},
		{"weekly", Group{WeeklyLimitUSD: &limit}, ErrWeeklyLimitExceeded},
		{"monthly", Group{MonthlyLimitUSD: &limit}, ErrMonthlyLimitExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, checkVideoAdmissionFunds(task, key, sub, &tc.group, 0, videoAdmissionHolds{}, now, false), tc.want,
				"disabling maintenance preserves accumulated subscription usage even after the window expires")
			require.NoError(t, checkVideoAdmissionFunds(task, key, sub, &tc.group, 0, videoAdmissionHolds{}, now, true))
		})
	}
}

func TestVideoAdmissionDerivesTokenEstimateWithoutRedis(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	task, key, account := videoAdmissionFixture()
	task.BillingMode, task.TokenPricingConfigured, task.OutputPricePerToken = VideoBillingModeUsageTokens, true, 0.0001
	expectVideoAdmissionLocks(mock, 1)
	expectVideoAdmissionKey(mock, 10, 0)
	expectVideoAdmissionConcurrency(mock, 2, 0, 0)
	expectVideoAdmissionHolds(t, mock)
	mock.ExpectExec("INSERT INTO video_tasks").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	cfg := &config.Config{}
	cfg.Billing.InflightReservation.DefaultMaxTokens = 4000
	require.NoError(t, (&OpenAIGatewayService{cfg: cfg}).AdmitVideoTask(context.Background(), &task, key, account, nil, 2))
	require.Equal(t, 0.4, task.AdmissionCost)
	require.False(t, task.AdmissionCostKnown)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVideoAdmissionReadsFreshSubscriptionWithoutChargingBalance(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	task, key, account := videoAdmissionFixture()
	task.BillingSnapshot.SubscriptionID = 7
	expectVideoAdmissionLocks(mock, 0)
	now, expires := time.Now(), time.Now().AddDate(0, 1, 0)
	columns := strings.Split("user_id,group_id,status,starts_at,expires_at,daily_window_start,weekly_window_start,monthly_window_start,daily_usage_usd,weekly_usage_usd,monthly_usage_usd,daily_limit_usd,weekly_limit_usd,monthly_limit_usd", ",")
	mock.ExpectQuery("SELECT s.user_id,s.group_id,s.status,s.starts_at,s.expires_at.*FROM user_subscriptions s JOIN groups g.*FOR UPDATE OF s").WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(1, 4, SubscriptionStatusActive, now.Add(-time.Hour), expires, now, now, now, 0.5, 0.5, 0.5, 1, 1, 1))
	expectVideoAdmissionKey(mock, 10, 0)
	expectVideoAdmissionConcurrency(mock, 2, 0, 0)
	expectVideoAdmissionHolds(t, mock)
	mock.ExpectExec("INSERT INTO video_tasks").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	require.NoError(t, (&OpenAIGatewayService{}).AdmitVideoTask(context.Background(), &task, key, account, &UserSubscription{ID: 7}, 2))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVideoAdmissionRejectsReassignedKeyBeforeQueueing(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	task, key, account := videoAdmissionFixture()
	expectVideoAdmissionLocks(mock, 10)
	columns := strings.Split("user_id,group_id,status,expires_at,quota,quota_used,rate_limit_5h,rate_limit_1d,rate_limit_7d,usage_5h,usage_1d,usage_7d,window_5h_start,window_1d_start,window_7d_start", ",")
	mock.ExpectQuery("SELECT user_id,group_id,status,expires_at,quota,quota_used.*FROM api_keys.*FOR UPDATE").WithArgs(int64(2)).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(1, 99, StatusActive, nil, 0, 0, 0, 0, 0, 0, 0, 0, nil, nil, nil))
	mock.ExpectRollback()
	require.ErrorContains(t, (&OpenAIGatewayService{}).AdmitVideoTask(context.Background(), &task, key, account, nil, 2), "group changed")
	require.NoError(t, mock.ExpectationsWereMet())
}
