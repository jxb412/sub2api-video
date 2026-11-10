package service

import (
	"os"
	"strings"
)

// 视频计费配置通过环境变量提供，避免任何 ORM/schema 改动：
//
//	VIDEO_BILLING_MODE=local_price | upstream_cost
//	VIDEO_PRICE_MINIMAX_H3_720P=0.10        （也可用统一前缀覆盖单价）
//	VIDEO_RESOLUTION_TIERS=768p:720p,1024p:1080p
//
// 分组级的价格表（groups.video_model_prices）仍然优先生效；环境变量只在其
// 未配置时兜底，并承载「计费模式」与「档位映射」两个全局开关。
const (
	envVideoBillingMode  = "VIDEO_BILLING_MODE"
	envVideoTiers        = "VIDEO_RESOLUTION_TIERS"
)

// VideoBillingSettings 是运行时生效的视频计费配置。
type VideoBillingSettings struct {
	Mode  string
	Tiers map[string]string
}

// VideoBillingSettingsFromEnv 解析环境变量配置。
func VideoBillingSettingsFromEnv() VideoBillingSettings {
	settings := VideoBillingSettings{Mode: VideoBillingModeDefault, Tiers: map[string]string{}}
	if raw := strings.TrimSpace(os.Getenv(envVideoBillingMode)); raw != "" {
		if normalized, ok := NormalizeVideoBillingMode(raw); ok {
			settings.Mode = normalized
		}
	}
	if raw := strings.TrimSpace(os.Getenv(envVideoTiers)); raw != "" {
		for _, pair := range strings.Split(raw, ",") {
			parts := strings.SplitN(strings.TrimSpace(pair), ":", 2)
			if len(parts) != 2 {
				continue
			}
			from := strings.ToLower(strings.TrimSpace(parts[0]))
			to := strings.ToLower(strings.TrimSpace(parts[1]))
			if from == "" || to == "" {
				continue
			}
			settings.Tiers[from] = to
		}
	}
	return settings
}

// VideoBillingSettingsFor 组合分组配置与环境变量：分组显式配置优先。
func VideoBillingSettingsFor(group *Group) VideoBillingSettings {
	settings := VideoBillingSettingsFromEnv()
	if group == nil {
		return settings
	}
	if mode := strings.TrimSpace(group.VideoBillingMode); mode != "" {
		if normalized, ok := NormalizeVideoBillingMode(mode); ok {
			settings.Mode = normalized
		}
	}
	if len(group.VideoResolutionTiers) > 0 {
		merged := make(map[string]string, len(settings.Tiers)+len(group.VideoResolutionTiers))
		for k, v := range settings.Tiers {
			merged[k] = v
		}
		for k, v := range group.VideoResolutionTiers {
			merged[strings.ToLower(strings.TrimSpace(k))] = stdv(v)
		}
		settings.Tiers = merged
	}
	return settings
}

func stdv(v string) string { return strings.ToLower(strings.TrimSpace(v)) }
