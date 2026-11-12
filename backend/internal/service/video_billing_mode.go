package service

import "strings"

// Video billing modes let an operator choose, per group, how a video job is
// priced. Both modes always record the upstream-reported cost for reconciliation.
const (
	// VideoBillingModeLocalPrice charges the operator-configured per-second
	// price from groups.video_model_prices (model x resolution tier).
	VideoBillingModeLocalPrice = "local_price"

	// VideoBillingModeUpstreamCost charges exactly what the upstream provider
	// reported as cost_usd, so the gateway never invents a price.
	VideoBillingModeUpstreamCost = "upstream_cost"
	VideoBillingModeUsageTokens  = "usage_tokens"

	// VideoBillingModeDefault is used when a group leaves the field empty.
	VideoBillingModeDefault = VideoBillingModeLocalPrice
)

// NormalizeVideoBillingMode canonicalises an operator-provided mode string.
// Unknown values fall back to the default and report false so callers can warn.
func NormalizeVideoBillingMode(mode string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", VideoBillingModeLocalPrice, "local", "price", "localprice":
		return VideoBillingModeLocalPrice, true
	case VideoBillingModeUpstreamCost, "upstream", "cost", "cost_usd", "upstreamcost":
		return VideoBillingModeUpstreamCost, true
	case VideoBillingModeUsageTokens:
		return VideoBillingModeUsageTokens, true
	default:
		return VideoBillingModeDefault, false
	}
}

// VideoBillingModeUsesUpstreamCost reports whether the mode prices from the
// upstream-reported cost instead of the local per-second table.
func VideoBillingModeUsesUpstreamCost(mode string) bool {
	normalized, _ := NormalizeVideoBillingMode(mode)
	return normalized == VideoBillingModeUpstreamCost
}
