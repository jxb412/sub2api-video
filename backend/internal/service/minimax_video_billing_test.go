package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func minimaxBillableTask() MinimaxVideoTask {
	return MinimaxVideoTask{ID: "mmv_billing", UserID: 1, APIKeyID: 2, AccountID: 3, GroupID: 4,
		Status: MinimaxVideoStatusDone, Model: "MiniMax-H3", VideoURL: "https://video.example/test.mp4",
		DurationSeconds: 5, Resolution: "768p", UpstreamCostUSD: 0.375, BillingMode: VideoBillingModeLocalPrice,
		PriceConfigured: true, PricePerSecond: 0.1, RateMultiplier: 1,
		CreatedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), CompletedAt: time.Now().UTC().Format(time.RFC3339Nano),
		BillingSnapshot: &MinimaxVideoBillingSnapshot{AccountType: "apikey", AccountRateMultiplier: 1, APIKeyQuota: true, APIKeyRateLimits: true}}
}

func TestMinimaxSettlementHonorsSnapshotAndFreePrices(t *testing.T) {
	task := minimaxBillableTask()
	got := ComputeMinimaxVideoSettlement(task, task.BillingMode, nil, task.RateMultiplier)
	require.Equal(t, 0.5, got.BilledCost)
	task.PricePerSecond = 0
	got = ComputeMinimaxVideoSettlement(task, task.BillingMode, nil, 1)
	require.Zero(t, got.BilledCost)
	require.Equal(t, "group_price", got.PriceSource)
	task.PricePerSecond = 0.1
	got = ComputeMinimaxVideoSettlement(task, task.BillingMode, nil, 0)
	require.Zero(t, got.BilledCost)
	task.DurationSeconds = 20
	got = ComputeMinimaxVideoSettlement(task, task.BillingMode, nil, 1)
	require.Equal(t, 2.0, got.BilledCost, "must not clamp a provider's actual duration to Grok's 15 second limit")
}

func TestMinimaxSettlementHDPriceFallback(t *testing.T) {
	task := minimaxBillableTask()
	task.PriceConfigured = false
	task.PricePerSecond = 0
	prices := NormalizeVideoModelPrices(map[string]map[string]float64{"MiniMax-H3": {"480p": 0.03, "720p": 0.1}})
	got := ComputeMinimaxVideoSettlement(task, VideoBillingModeLocalPrice, prices, 1)
	require.Equal(t, 0.5, got.BilledCost)
}

func TestMinimaxBillingUsesExactlyOnePayer(t *testing.T) {
	task := minimaxBillableTask()
	settlement := ComputeMinimaxVideoSettlement(task, task.BillingMode, nil, 1)
	cmd, err := minimaxVideoBillingCommand(task, settlement)
	require.NoError(t, err)
	require.Equal(t, 0.5, cmd.BalanceCost)
	require.Zero(t, cmd.SubscriptionCost)
	task.BillingSnapshot.SubscriptionID = 9
	cmd, err = minimaxVideoBillingCommand(task, settlement)
	require.NoError(t, err)
	require.Zero(t, cmd.BalanceCost)
	require.Equal(t, 0.5, cmd.SubscriptionCost)
	require.Equal(t, 0.5, cmd.APIKeyQuotaCost)
	require.Equal(t, 0.5, cmd.APIKeyRateLimitCost)
	require.Equal(t, int64(9), *cmd.SubscriptionID)
}

func TestMinimaxBillingRejectsInvalidAmountsAndLegacySnapshots(t *testing.T) {
	for _, amount := range []float64{-1, math.NaN(), math.Inf(1)} {
		_, err := minimaxVideoBillingCommand(minimaxBillableTask(), MinimaxVideoSettlement{BilledCost: amount})
		require.Error(t, err)
	}
	task := minimaxBillableTask()
	task.BillingSnapshot = nil
	_, err := minimaxVideoBillingCommand(task, MinimaxVideoSettlement{BilledCost: 0.5})
	require.Error(t, err, "do not retrospectively charge old cached tasks")
}

type minimaxLedgerStub struct {
	UsageBillingRepository
	claims  map[string]string
	charges int
	amount  float64
}

func (r *minimaxLedgerStub) Apply(_ context.Context, cmd *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	if prior, exists := r.claims[cmd.RequestID]; exists {
		if prior != cmd.RequestFingerprint {
			return nil, ErrUsageBillingRequestConflict
		}
		return &UsageBillingApplyResult{}, nil
	}
	r.claims[cmd.RequestID] = cmd.RequestFingerprint
	r.charges++
	r.amount += cmd.BalanceCost + cmd.SubscriptionCost
	return &UsageBillingApplyResult{Applied: true}, nil
}

type minimaxUsageLogStub struct {
	UsageLogRepository
	fail bool
	log  *UsageLog
}

func (r *minimaxUsageLogStub) Create(_ context.Context, log *UsageLog) (bool, error) {
	if r.fail {
		r.fail = false
		return false, errors.New("temporary usage store failure")
	}
	r.log = log
	return true, nil
}

func TestMinimaxSettlementRetriesWithoutDoubleCharging(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	oldDB := videoTaskHandle()
	SetVideoTaskDB(db)
	t.Cleanup(func() { SetVideoTaskDB(oldDB); db.Close() })
	oldCache := videoBillingCacheHandle()
	SetVideoBillingCache(nil)
	t.Cleanup(func() { SetVideoBillingCache(oldCache) })
	ledger := &minimaxLedgerStub{claims: map[string]string{}}
	logs := &minimaxUsageLogStub{fail: true}
	svc := &OpenAIGatewayService{usageBillingRepo: ledger, usageLogRepo: logs}
	task := minimaxBillableTask()
	_, err = svc.settleMinimaxVideoTask(context.Background(), task)
	require.Error(t, err)
	require.Equal(t, 1, ledger.charges)
	mock.ExpectExec("UPDATE video_tasks").WithArgs(task.ID, 0.375, 0.5, VideoBillingModeLocalPrice).WillReturnResult(sqlmock.NewResult(0, 1))
	_, err = svc.settleMinimaxVideoTask(context.Background(), task)
	require.NoError(t, err)
	require.Equal(t, 1, ledger.charges)
	require.Equal(t, 0.5, ledger.amount)
	require.Equal(t, 1, logs.log.VideoCount)
	require.Equal(t, "video", *logs.log.MediaType)
	require.Equal(t, 0.5, logs.log.ActualCost)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMinimaxFailedTaskNeverCharges(t *testing.T) {
	ledger := &minimaxLedgerStub{claims: map[string]string{}}
	svc := &OpenAIGatewayService{usageBillingRepo: ledger, usageLogRepo: &minimaxUsageLogStub{}}
	task := minimaxBillableTask()
	task.Status = MinimaxVideoStatusError
	_, err := svc.settleMinimaxVideoTask(context.Background(), task)
	require.Error(t, err)
	require.Zero(t, ledger.charges)
}

func TestMinimaxSimpleModeNeverChargesBalances(t *testing.T) {
	task := minimaxBillableTask()
	task.BillingSnapshot.SimpleMode = true
	task.BillingSnapshot.SubscriptionID = 9
	task.BillingSnapshot.AccountQuota = true
	settlement := ComputeMinimaxVideoSettlement(task, task.BillingMode, nil, 1)
	cmd, err := minimaxVideoBillingCommand(task, settlement)
	require.NoError(t, err)
	require.Zero(t, cmd.BalanceCost)
	require.Zero(t, cmd.SubscriptionCost)
	require.Zero(t, cmd.APIKeyQuotaCost)
	require.Zero(t, cmd.APIKeyRateLimitCost)
	require.Zero(t, cmd.AccountQuotaCost)
	task.BillingSnapshot.SimpleModeKeyRateLimitEnabled = true
	cmd, err = minimaxVideoBillingCommand(task, settlement)
	require.NoError(t, err)
	require.Equal(t, 0.5, cmd.APIKeyRateLimitCost)
	require.Zero(t, cmd.BalanceCost)
	require.Zero(t, cmd.SubscriptionCost)
}

type videoWindowSubscriptionRepo struct {
	UserSubscriptionRepository
	sub        UserSubscription
	resetErr   error
	resetCalls int
}

func (r *videoWindowSubscriptionRepo) GetByID(context.Context, int64) (*UserSubscription, error) {
	copy := r.sub
	return &copy, nil
}

func (r *videoWindowSubscriptionRepo) ResetDailyUsage(_ context.Context, _ int64, expected *time.Time, next time.Time) error {
	if r.resetErr != nil {
		return r.resetErr
	}
	if expected != nil && r.sub.DailyWindowStart.Equal(*expected) {
		r.sub.DailyWindowStart, r.sub.DailyUsageUSD = &next, 0
		r.resetCalls++
	}
	return nil
}
func (r *videoWindowSubscriptionRepo) ResetWeeklyUsage(_ context.Context, _ int64, expected *time.Time, next time.Time) error {
	if r.resetErr != nil {
		return r.resetErr
	}
	if expected != nil && r.sub.WeeklyWindowStart.Equal(*expected) {
		r.sub.WeeklyWindowStart, r.sub.WeeklyUsageUSD = &next, 0
		r.resetCalls++
	}
	return nil
}
func (r *videoWindowSubscriptionRepo) ResetMonthlyUsage(_ context.Context, _ int64, expected *time.Time, next time.Time) error {
	if r.resetErr != nil {
		return r.resetErr
	}
	if expected != nil && r.sub.MonthlyWindowStart.Equal(*expected) {
		r.sub.MonthlyWindowStart, r.sub.MonthlyUsageUSD = &next, 0
		r.resetCalls++
	}
	return nil
}

type videoWindowLedger struct {
	minimaxLedgerStub
	sub *videoWindowSubscriptionRepo
}

func (l *videoWindowLedger) Apply(ctx context.Context, cmd *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	result, err := l.minimaxLedgerStub.Apply(ctx, cmd)
	if err == nil && result.Applied {
		l.sub.sub.DailyUsageUSD += cmd.SubscriptionCost
		l.sub.sub.WeeklyUsageUSD += cmd.SubscriptionCost
		l.sub.sub.MonthlyUsageUSD += cmd.SubscriptionCost
	}
	return result, err
}

func TestMinimaxSubscriptionSettlementMaintainsElapsedWindows(t *testing.T) {
	for _, autoReset := range []bool{true, false} {
		t.Run(fmt.Sprintf("auto_reset_%v", autoReset), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			oldDB := videoTaskHandle()
			SetVideoTaskDB(db)
			t.Cleanup(func() { SetVideoTaskDB(oldDB); db.Close() })
			oldCache := videoBillingCacheHandle()
			SetVideoBillingCache(nil)
			t.Cleanup(func() { SetVideoBillingCache(oldCache) })
			now := time.Now()
			stale := now.Add(-45 * 24 * time.Hour)
			task := minimaxBillableTask()
			task.BillingSnapshot.SubscriptionID = 9
			repo := &videoWindowSubscriptionRepo{sub: UserSubscription{ID: 9, UserID: task.UserID, GroupID: task.GroupID, StartsAt: stale, ExpiresAt: now.Add(90 * 24 * time.Hour), DailyWindowStart: &stale, WeeklyWindowStart: &stale, MonthlyWindowStart: &stale, DailyUsageUSD: 11, WeeklyUsageUSD: 22, MonthlyUsageUSD: 33}}
			ledger := &videoWindowLedger{minimaxLedgerStub: minimaxLedgerStub{claims: map[string]string{}}, sub: repo}
			cfg := &config.Config{}
			cfg.SubscriptionMaintenance.AutoResetEnabled = autoReset
			svc := &OpenAIGatewayService{cfg: cfg, userSubRepo: repo, usageBillingRepo: ledger, usageLogRepo: &minimaxUsageLogStub{}}
			for range 2 {
				mock.ExpectExec("UPDATE video_tasks").WithArgs(task.ID, 0.375, 0.5, VideoBillingModeLocalPrice).WillReturnResult(sqlmock.NewResult(0, 1))
				_, err := svc.settleMinimaxVideoTask(context.Background(), task)
				require.NoError(t, err)
			}
			require.Equal(t, 1, ledger.charges)
			if autoReset {
				require.Equal(t, 3, repo.resetCalls)
				// Normal auth maintenance after completion must preserve the newly
				// charged amount, including after an idempotent settlement retry.
				maintenance := &SubscriptionService{userSubRepo: repo, autoResetEnabled: true, now: time.Now}
				sub, _ := repo.GetByID(context.Background(), 9)
				_, err := maintenance.EnsureWindowMaintenance(context.Background(), sub)
				require.NoError(t, err)
				require.Equal(t, 0.5, repo.sub.DailyUsageUSD)
				require.Equal(t, 0.5, repo.sub.WeeklyUsageUSD)
				require.Equal(t, 0.5, repo.sub.MonthlyUsageUSD)
			} else {
				require.Zero(t, repo.resetCalls)
				require.Equal(t, 11.5, repo.sub.DailyUsageUSD)
				require.Equal(t, 22.5, repo.sub.WeeklyUsageUSD)
				require.Equal(t, 33.5, repo.sub.MonthlyUsageUSD)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestMinimaxSubscriptionMaintenanceFailurePreventsCharge(t *testing.T) {
	now := time.Now()
	stale := now.Add(-48 * time.Hour)
	task := minimaxBillableTask()
	task.BillingSnapshot.SubscriptionID = 9
	repo := &videoWindowSubscriptionRepo{sub: UserSubscription{ID: 9, UserID: task.UserID, GroupID: task.GroupID, StartsAt: stale, ExpiresAt: now.Add(30 * 24 * time.Hour), DailyWindowStart: &stale, WeeklyWindowStart: &stale, MonthlyWindowStart: &stale}, resetErr: errors.New("temporary subscription storage failure")}
	ledger := &minimaxLedgerStub{claims: map[string]string{}}
	cfg := &config.Config{}
	cfg.SubscriptionMaintenance.AutoResetEnabled = true
	svc := &OpenAIGatewayService{cfg: cfg, userSubRepo: repo, usageBillingRepo: ledger, usageLogRepo: &minimaxUsageLogStub{}}
	_, err := svc.settleMinimaxVideoTask(context.Background(), task)
	require.ErrorContains(t, err, "temporary subscription storage failure")
	require.Zero(t, ledger.charges)
}
