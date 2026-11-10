package service

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// 视频结算需要的扣费通道。与 videoTaskDB 一样用包级句柄注入，
// 避免把 BillingCacheService 穿过整条 handler → goroutine 链路。
var (
	videoBillingCacheMu sync.RWMutex
	videoBillingCache   *BillingCacheService
)

// SetVideoBillingCache 在装配处注入扣费缓存服务。
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

// MiniMax 视频任务的结算与记账。
//
// 视频链路是独立入口，不经过 gateway_usage_billing 的通用流程，所以结算必须
// 显式完成三件事：
//  1. 按分组配置的计费模式算出应扣金额；
//  2. 把金额写回 video_tasks（对账用）；
//  3. 写一条 usage_logs 记录，让「使用记录」页面、统计与账单都能看到。
//
// 计费模式：
//   - local_price   → 分组配置的秒单价 × 时长 × 分组倍率
//   - upstream_cost → 上游 cost_usd × 分组倍率
//
// 没有配置秒单价时退化为 upstream_cost，避免出现「配了 local_price 但算不出钱」
// 的静默免费情况。

// MinimaxVideoSettlement 是一次结算的结果。
type MinimaxVideoSettlement struct {
	BillingMode  string  `json:"billing_mode"`
	BilledCost   float64 `json:"billed_cost_usd"`
	UpstreamCost float64 `json:"upstream_cost_usd"`
	// PriceSource 记录金额来源，便于排查："group_price" / "upstream_cost" / "fallback_upstream"
	PriceSource string `json:"price_source"`
}

// ComputeMinimaxVideoSettlement 纯函数，便于单测：给定任务与分组配置算出应扣金额。
func ComputeMinimaxVideoSettlement(task MinimaxVideoTask, mode string, prices map[string]map[string]float64, rateMultiplier float64) MinimaxVideoSettlement {
	normalized, _ := NormalizeVideoBillingMode(mode)
	out := MinimaxVideoSettlement{
		BillingMode:  normalized,
		UpstreamCost: task.UpstreamCostUSD,
	}
	if rateMultiplier <= 0 {
		rateMultiplier = 1
	}
	if normalized == VideoBillingModeUpstreamCost {
		out.BilledCost = task.UpstreamCostUSD * rateMultiplier
		out.PriceSource = "upstream_cost"
		return out
	}
	duration := NormalizeVideoBillingDurationSecondsOrDefault(task.DurationSeconds)
	resolution := NormalizeVideoBillingResolutionOrDefault(task.Resolution)
	if price := LookupVideoModelPrice(prices, task.Model, resolution); price != nil {
		out.BilledCost = *price * float64(duration) * rateMultiplier
		out.PriceSource = "group_price"
		return out
	}
	// 没配秒单价：跟随上游成本，宁可少赚也不能白送。
	out.BilledCost = task.UpstreamCostUSD * rateMultiplier
	out.PriceSource = "fallback_upstream"
	return out
}

// SettleMinimaxVideoTask 在任务完成时结算、记账并落库。
// 只读任务上的计费快照，不查分组：任务完成后分组改价不影响已发生的费用。
func (s *OpenAIGatewayService) SettleMinimaxVideoTask(ctx context.Context, task MinimaxVideoTask) MinimaxVideoSettlement {
	if s == nil || task.Status != MinimaxVideoStatusDone {
		return MinimaxVideoSettlement{}
	}
	settlement := ComputeMinimaxVideoSettlement(task, task.BillingMode, nil, task.RateMultiplier)
	if task.PricePerSecond > 0 && !VideoBillingModeUsesUpstreamCost(task.BillingMode) {
		duration := NormalizeVideoBillingDurationSecondsOrDefault(task.DurationSeconds)
		multiplier := task.RateMultiplier
		if multiplier <= 0 {
			multiplier = 1
		}
		settlement.BilledCost = task.PricePerSecond * float64(duration) * multiplier
		settlement.PriceSource = "group_price"
	}

	if err := UpdateVideoTaskBilling(ctx, task.ID, task.UpstreamCostUSD, settlement.BilledCost, settlement.BillingMode); err != nil {
		slog.Warn("minimax_video.billing_persist_failed", "task_id", task.ID, "error", err)
	}
	s.writeMinimaxVideoUsageLog(ctx, task, settlement)
	if settlement.BilledCost > 0 {
		s.chargeMinimaxVideoUsage(ctx, task, settlement)
	}
	return settlement
}

// writeMinimaxVideoUsageLog 按现有 UsageLog 字段写入一条视频用量记录。
func (s *OpenAIGatewayService) writeMinimaxVideoUsageLog(ctx context.Context, task MinimaxVideoTask, settlement MinimaxVideoSettlement) {
	if s == nil || s.usageLogRepo == nil {
		return
	}
	mode := settlement.BillingMode
	requestType := RequestTypeSync
	endpoint := "/v1/videos/generations"
	durationMs := 0
	if task.CompletedAt != "" && task.CreatedAt != "" {
		if start, err := time.Parse(time.RFC3339Nano, task.CreatedAt); err == nil {
			if end, err := time.Parse(time.RFC3339Nano, task.CompletedAt); err == nil {
				durationMs = int(end.Sub(start).Milliseconds())
			}
		}
	}
	durationSeconds := task.DurationSeconds
	var groupID *int64
	if task.GroupID > 0 {
		gid := task.GroupID
		groupID = &gid
	}
	upstreamModel := task.UpstreamModel
	log := &UsageLog{
		UserID:               task.UserID,
		APIKeyID:             task.APIKeyID,
		AccountID:            task.AccountID,
		RequestID:            task.ID,
		Model:                task.Model,
		RequestedModel:       task.Model,
		UpstreamModel:        &upstreamModel,
		GroupID:              groupID,
		BillingMode:          &mode,
		RequestType:          requestType,
		InboundEndpoint:      &endpoint,
		DurationMs:           &durationMs,
		VideoDurationSeconds: &durationSeconds,
		TotalCost:            settlement.UpstreamCost,
		ActualCost:           settlement.BilledCost,
		CreatedAt:            time.Now().UTC(),
	}
	if _, err := s.usageLogRepo.Create(ctx, log); err != nil {
		slog.Warn("minimax_video.usage_log_failed", "task_id", task.ID, "error", err)
		return
	}
	slog.Info("minimax_video.usage_logged", "task_id", task.ID,
		"billed", settlement.BilledCost, "upstream", settlement.UpstreamCost,
		"mode", settlement.BillingMode, "source", settlement.PriceSource)
}

// chargeMinimaxVideoUsage 真正扣减余额与订阅用量。
// 两步都失败时只记日志：视频已经交付，扣费失败不应让任务状态回滚，
// 由对账任务按 usage_logs 补扣。
func (s *OpenAIGatewayService) chargeMinimaxVideoUsage(ctx context.Context, task MinimaxVideoTask, settlement MinimaxVideoSettlement) {
	billing := videoBillingCacheHandle()
	if billing == nil {
		slog.Warn("minimax_video.charge_skipped_no_billing_cache", "task_id", task.ID)
		return
	}
	if err := billing.DeductBalanceCache(ctx, task.UserID, settlement.BilledCost); err != nil {
		slog.Warn("minimax_video.deduct_balance_failed", "task_id", task.ID,
			"user_id", task.UserID, "amount", settlement.BilledCost, "error", err)
	} else {
		slog.Info("minimax_video.balance_deducted", "task_id", task.ID,
			"user_id", task.UserID, "amount", settlement.BilledCost)
	}
	if task.GroupID > 0 {
		if err := billing.UpdateSubscriptionUsage(ctx, task.UserID, task.GroupID, settlement.BilledCost); err != nil {
			slog.Warn("minimax_video.subscription_usage_failed", "task_id", task.ID, "error", err)
		}
	}
}
