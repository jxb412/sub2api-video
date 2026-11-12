package service

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// 环境变量为未配置分组的调用方提供默认值；分组显式配置优先：
//
//	VIDEO_BILLING_MODE=local_price | upstream_cost
//	VIDEO_RESOLUTION_TIERS=768p:720p,1024p:1080p
//
// 分组级价格、模式和分辨率映射持久化在 groups 表。
const (
	envVideoBillingMode = "VIDEO_BILLING_MODE"
	envVideoTiers       = "VIDEO_RESOLUTION_TIERS"
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
			to, ok := LookupVideoBillingResolution(parts[1])
			if !videoResolutionSourcePattern.MatchString(from) || !ok {
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
			if tier, ok := LookupVideoBillingResolution(v); ok {
				merged[strings.ToLower(strings.TrimSpace(k))] = tier
			}
		}
		settings.Tiers = merged
	}
	return settings
}

var videoResolutionSourcePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// NormalizeVideoResolutionTiers validates administrator input instead of silently
// turning a misspelled expensive tier into the cheapest price.
func NormalizeVideoResolutionTiers(input map[string]string) (map[string]string, error) {
	if len(input) > 100 {
		return nil, fmt.Errorf("video_resolution_tiers must contain at most 100 entries")
	}
	result := make(map[string]string, len(input))
	for source, target := range input {
		key := strings.ToLower(strings.TrimSpace(source))
		if !videoResolutionSourcePattern.MatchString(key) {
			return nil, fmt.Errorf("invalid video resolution %q", source)
		}
		tier, ok := LookupVideoBillingResolution(target)
		if !ok {
			return nil, fmt.Errorf("invalid video billing tier %q: expected 480p, 720p or 1080p", target)
		}
		if previous, exists := result[key]; exists && previous != tier {
			return nil, fmt.Errorf("conflicting video billing tiers for %q", key)
		}
		result[key] = tier
	}
	return result, nil
}
