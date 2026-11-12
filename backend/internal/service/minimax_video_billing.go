package service

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"
)

var (
	videoBillingCacheMu sync.RWMutex
	videoBillingCache   *BillingCacheService
)

// SetVideoBillingCache is retained for the existing application wiring.
func SetVideoBillingCache(svc *BillingCacheService) {
	videoBillingCacheMu.Lock()
	videoBillingCache = svc
	videoBillingCacheMu.Unlock()
}

func videoBillingCacheHandle() *BillingCacheService {
	videoBillingCacheMu.RLock()
	defer videoBillingCacheMu.RUnlock()
	return videoBillingCache
}

// Only billing metadata is persisted here: never an API credential or user
// object. It fixes the payer and quota policy at task submission time.
type MinimaxVideoBillingSnapshot struct {
	SubscriptionID                int64   `json:"subscription_id,omitempty"`
	APIKeyQuota                   bool    `json:"api_key_quota"`
	APIKeyRateLimits              bool    `json:"api_key_rate_limits"`
	AccountType                   string  `json:"account_type"`
	AccountQuota                  bool    `json:"account_quota"`
	AccountDailyQuota             bool    `json:"account_daily_quota"`
	AccountWeeklyQuota            bool    `json:"account_weekly_quota"`
	AccountRateMultiplier         float64 `json:"account_rate_multiplier"`
	Platform                      string  `json:"platform,omitempty"`
	SimpleMode                    bool    `json:"simple_mode,omitempty"`
	SimpleModeKeyRateLimitEnabled bool    `json:"simple_mode_key_rate_limit_enabled,omitempty"`
}

func SnapshotMinimaxVideoBilling(task *MinimaxVideoTask, key *APIKey, account *Account, subscription *UserSubscription) {
	if task == nil || key == nil || account == nil {
		return
	}
	snapshot := &MinimaxVideoBillingSnapshot{
		APIKeyQuota: key.Quota > 0, APIKeyRateLimits: key.HasRateLimits(),
		AccountType:           account.Type,
		AccountQuota:          account.SupportsAccountQuota() && account.HasAnyQuotaLimit(),
		AccountDailyQuota:     account.GetQuotaDailyLimit() > 0,
		AccountWeeklyQuota:    account.GetQuotaWeeklyLimit() > 0,
		AccountRateMultiplier: account.BillingRateMultiplier(),
		Platform:              PlatformFromAPIKey(key),
	}
	if subscription != nil && key.Group != nil && key.Group.IsSubscriptionType() {
		snapshot.SubscriptionID = subscription.ID
	}
	task.BillingSnapshot = snapshot
}

type MinimaxVideoSettlement struct {
	BillingMode  string  `json:"billing_mode"`
	BilledCost   float64 `json:"billed_cost_usd"`
	UpstreamCost float64 `json:"upstream_cost_usd"`
	PriceSource  string  `json:"price_source"`
	BaseCost     float64 `json:"base_cost_usd"`
}

func ComputeMinimaxVideoSettlement(task MinimaxVideoTask, mode string, prices map[string]map[string]float64, rateMultiplier float64) MinimaxVideoSettlement {
	normalized, _ := NormalizeVideoBillingMode(mode)
	out := MinimaxVideoSettlement{BillingMode: normalized, UpstreamCost: task.UpstreamCostUSD}
	if rateMultiplier < 0 {
		rateMultiplier = 1
	}
	if normalized == VideoBillingModeUsageTokens {
		out.BaseCost = float64(task.InputTokens)*task.InputPricePerToken + float64(task.OutputTokens)*task.OutputPricePerToken
		out.BilledCost, out.PriceSource = QuantizeUsageBillingAmount(out.BaseCost*rateMultiplier), "token_usage"
		return out
	}
	// Old snapshots did not distinguish a missing multiplier from explicit 0.
	if task.BillingSnapshot == nil && rateMultiplier == 0 {
		rateMultiplier = 1
	}
	price := LookupVideoModelPriceWithTiers(prices, nil, task.Model, task.Resolution)
	if task.PriceConfigured || task.PricePerSecond > 0 {
		price = &task.PricePerSecond
	}
	if !VideoBillingModeUsesUpstreamCost(normalized) && price != nil {
		duration := task.DurationSeconds
		if duration <= 0 {
			duration = VideoBillingDefaultDurationSeconds
		}
		out.BilledCost = *price * float64(duration) * rateMultiplier
		out.PriceSource = "group_price"
	} else {
		out.BilledCost = task.UpstreamCostUSD * rateMultiplier
		out.PriceSource = "upstream_cost"
		if !VideoBillingModeUsesUpstreamCost(normalized) {
			out.PriceSource = "fallback_upstream"
		}
	}
	out.BilledCost = QuantizeUsageBillingAmount(out.BilledCost)
	return out
}

func minimaxVideoBillingCommand(task MinimaxVideoTask, settlement MinimaxVideoSettlement) (*UsageBillingCommand, error) {
	if task.BillingSnapshot == nil {
		return nil, fmt.Errorf("video task has no durable billing snapshot")
	}
	if settlement.BilledCost < 0 || settlement.UpstreamCost < 0 || math.IsNaN(settlement.BilledCost) || math.IsInf(settlement.BilledCost, 0) || math.IsNaN(settlement.UpstreamCost) || math.IsInf(settlement.UpstreamCost, 0) {
		return nil, fmt.Errorf("invalid video settlement amount")
	}
	p := task.BillingSnapshot
	cmd := &UsageBillingCommand{RequestID: task.ID, UserID: task.UserID, APIKeyID: task.APIKeyID,
		AccountID: task.AccountID, AccountType: p.AccountType, Model: firstNonEmptyString(task.BillingModel, task.Model), MediaType: "video"}
	if p.SimpleMode {
		if p.SimpleModeKeyRateLimitEnabled && p.APIKeyRateLimits {
			cmd.APIKeyRateLimitCost = settlement.BilledCost
		}
		cmd.Normalize()
		return cmd, nil
	}
	if p.SubscriptionID > 0 {
		id := p.SubscriptionID
		cmd.SubscriptionID = &id
		cmd.SubscriptionCost = settlement.BilledCost
		cmd.BillingType = BillingTypeSubscription
	} else {
		cmd.BalanceCost = settlement.BilledCost
	}
	if p.APIKeyQuota {
		cmd.APIKeyQuotaCost = settlement.BilledCost
	}
	if p.APIKeyRateLimits {
		cmd.APIKeyRateLimitCost = settlement.BilledCost
	}
	if p.AccountQuota {
		base := settlement.UpstreamCost
		if task.BillingMode == VideoBillingModeUsageTokens && !task.UpstreamCostReported {
			base = settlement.BaseCost
		}
		cmd.AccountQuotaCost = base * p.AccountRateMultiplier
		cmd.AccountQuotaHasDailyLimit = p.AccountDailyQuota
		cmd.AccountQuotaHasWeeklyLimit = p.AccountWeeklyQuota
	}
	cmd.Normalize()
	return cmd, nil
}

// The durable billing ledger deduplicates the charge. A subsequent retry can
// finish the usage log and settlement marker without charging a second time.
func (s *OpenAIGatewayService) settleMinimaxVideoTask(ctx context.Context, task MinimaxVideoTask) (MinimaxVideoSettlement, error) {
	settlement := ComputeMinimaxVideoSettlement(task, task.BillingMode, nil, task.RateMultiplier)
	if s == nil || task.Status != MinimaxVideoStatusDone || task.VideoURL == "" {
		return settlement, fmt.Errorf("video is not complete")
	}
	if task.UpstreamProtocol == MinimaxVideoProtocolLL2 && (!task.OutputSecondsReported || task.DurationSeconds != task.RequestedDurationSeconds || task.BillingMode != VideoBillingModeLocalPrice) {
		return settlement, fmt.Errorf("LL2 final output seconds or local pricing unavailable/inconsistent; settlement pending")
	}
	if task.BillingMode == VideoBillingModeUsageTokens && (!task.TokenPricingConfigured || !task.UsageReported) {
		return settlement, fmt.Errorf("Seedance token price or final usage is unavailable; settlement pending")
	}
	if task.BillingMode == VideoBillingModeLocalPrice && !task.PriceConfigured {
		return settlement, fmt.Errorf("video local price is unavailable; settlement pending")
	}
	if task.BillingMode == VideoBillingModeUpstreamCost && !task.UpstreamCostReported && task.UpstreamCostUSD == 0 {
		return settlement, fmt.Errorf("upstream cost was not reported; settlement pending")
	}
	if s.usageBillingRepo == nil || s.usageLogRepo == nil {
		return settlement, fmt.Errorf("video billing repository unavailable")
	}
	cmd, err := minimaxVideoBillingCommand(task, settlement)
	if err != nil {
		return settlement, err
	}
	billingCtx, cancel := detachedBillingContext(ctx)
	defer cancel()
	if err := s.maintainVideoSubscriptionWindows(billingCtx, task, cmd); err != nil {
		return settlement, err
	}
	result, err := s.usageBillingRepo.Apply(billingCtx, cmd)
	if err != nil {
		return settlement, err
	}
	// Invalidation is idempotent even if the previous process died immediately
	// after committing. Never deduct an incremental cache amount on retries.
	billing := s.billingCacheService
	if billing == nil {
		billing = videoBillingCacheHandle()
	}
	// Match the shared gateway's user/platform quota accounting, only after
	// the ledger reports that this request was applied for the first time.
	if result != nil && result.Applied && !task.BillingSnapshot.SimpleMode && cmd.SubscriptionID == nil &&
		settlement.BilledCost > 0 && task.BillingSnapshot.Platform != "" && s.userPlatformQuotaRepo != nil && billing != nil {
		platform := task.BillingSnapshot.Platform
		if billing.HasUserPlatformQuotaLimit(billingCtx, task.UserID, platform) {
			billing.IncrementUserPlatformQuotaUsage(task.UserID, platform, settlement.BilledCost)
			if s.cfg == nil || !s.cfg.Database.UserPlatformQuotaFlusherEnabled {
				persistPostUsagePlatformQuota(billingCtx, s.userPlatformQuotaRepo, task.UserID, platform, settlement.BilledCost)
			}
		}
	}
	if billing != nil {
		if cmd.SubscriptionID != nil {
			err = billing.InvalidateSubscription(billingCtx, task.UserID, task.GroupID)
		} else {
			err = billing.InvalidateUserBalance(billingCtx, task.UserID)
		}
		if err != nil {
			return settlement, err
		}
		if err = billing.InvalidateAPIKeyRateLimit(billingCtx, task.APIKeyID); err != nil {
			return settlement, err
		}
	}
	if err = s.writeMinimaxVideoUsageLog(billingCtx, task, settlement, cmd); err != nil {
		return settlement, err
	}
	if err = UpdateVideoTaskBilling(billingCtx, task.ID, settlement.UpstreamCost, settlement.BilledCost, settlement.BillingMode); err != nil {
		return settlement, err
	}
	return settlement, nil
}

// Generation may finish days after admission. Advance the subscription windows
// before charging so the next authenticated request cannot erase this payment
// while cleaning up an old window. Reuse the normal conditional reset policy,
// including daily-card and final partial-window exemptions.
func (s *OpenAIGatewayService) maintainVideoSubscriptionWindows(ctx context.Context, task MinimaxVideoTask, cmd *UsageBillingCommand) error {
	if cmd.SubscriptionID == nil || cmd.SubscriptionCost <= 0 || s.cfg == nil || !s.cfg.SubscriptionMaintenance.AutoResetEnabled {
		return nil
	}
	if s.userSubRepo == nil {
		return fmt.Errorf("video subscription repository unavailable")
	}
	sub, err := s.userSubRepo.GetByID(ctx, *cmd.SubscriptionID)
	if err != nil {
		return err
	}
	if sub == nil || sub.UserID != task.UserID || sub.GroupID != task.GroupID {
		return fmt.Errorf("video subscription ownership changed")
	}
	billing := s.billingCacheService
	if billing == nil {
		billing = videoBillingCacheHandle()
	}
	// No extra cache or worker is allocated: this delegates only synchronous
	// maintenance through the same repository and billing-cache invalidations.
	maintenance := &SubscriptionService{userSubRepo: s.userSubRepo, billingCacheService: billing, autoResetEnabled: true, now: time.Now}
	_, err = maintenance.EnsureWindowMaintenance(ctx, sub)
	return err
}

func (s *OpenAIGatewayService) SettleMinimaxVideoTask(ctx context.Context, task MinimaxVideoTask) MinimaxVideoSettlement {
	settlement, err := s.settleMinimaxVideoTask(ctx, task)
	if err != nil {
		slog.Error("minimax_video.settlement_pending", "task_id", task.ID, "error", err)
	}
	return settlement
}

func (s *OpenAIGatewayService) writeMinimaxVideoUsageLog(ctx context.Context, task MinimaxVideoTask, settlement MinimaxVideoSettlement, cmd *UsageBillingCommand) error {
	mode, endpoint, mediaType := string(BillingModeVideo), "/v1/videos/generations", "video"
	if task.Provider == VideoProviderSeedance {
		endpoint = "/api/v3/contents/generations/tasks"
	}
	if task.BillingMode == VideoBillingModeUsageTokens {
		mode = string(BillingModeToken)
	}
	durationMs := 0
	if start, err := time.Parse(time.RFC3339Nano, task.CreatedAt); err == nil {
		if end, err := time.Parse(time.RFC3339Nano, task.CompletedAt); err == nil {
			durationMs = int(end.Sub(start).Milliseconds())
		}
	}
	var groupID *int64
	if task.GroupID > 0 {
		id := task.GroupID
		groupID = &id
	}
	upstreamModel, resolution, duration := task.UpstreamModel, task.Resolution, task.DurationSeconds
	accountRate := task.BillingSnapshot.AccountRateMultiplier
	accountCost := QuantizeUsageBillingAmount(settlement.UpstreamCost * accountRate)
	totalCost := settlement.UpstreamCost
	if settlement.PriceSource == "group_price" {
		totalCost = task.PricePerSecond * float64(duration)
	}
	if settlement.PriceSource == "token_usage" {
		totalCost = settlement.BaseCost
		if !task.UpstreamCostReported {
			accountCost = QuantizeUsageBillingAmount(totalCost * accountRate)
		}
	}
	created := time.Now().UTC()
	if completed, err := time.Parse(time.RFC3339Nano, task.CompletedAt); err == nil {
		created = completed
	}
	_, err := s.usageLogRepo.Create(ctx, &UsageLog{
		UserID: task.UserID, APIKeyID: task.APIKeyID, AccountID: task.AccountID, RequestID: task.ID,
		Model: firstNonEmptyString(task.BillingModel, task.Model), RequestedModel: task.Model, UpstreamModel: &upstreamModel, GroupID: groupID,
		SubscriptionID: cmd.SubscriptionID, BillingType: cmd.BillingType, BillingMode: &mode,
		RequestType: RequestTypeSync, InboundEndpoint: &endpoint, DurationMs: &durationMs,
		MediaType: &mediaType, VideoCount: 1, VideoResolution: &resolution, VideoDurationSeconds: &duration,
		InputTokens: task.InputTokens, OutputTokens: task.OutputTokens,
		InputCost: float64(task.InputTokens) * task.InputPricePerToken, OutputCost: float64(task.OutputTokens) * task.OutputPricePerToken,
		TotalCost: totalCost, ActualCost: settlement.BilledCost, RateMultiplier: task.RateMultiplier,
		AccountRateMultiplier: &accountRate, AccountStatsCost: &accountCost, CreatedAt: created,
	})
	return err
}
