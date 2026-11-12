//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVideoCustomerStatusNeverExposesProcurement(t *testing.T) {
	for _, provider := range []string{"minimax", "seedance"} {
		for _, reported := range []bool{false, true} {
			t.Run(provider+map[bool]string{true: "_with_usage", false: "_without_usage"}[reported], func(t *testing.T) {
				task := MinimaxVideoTask{
					ID: "local-task", UpstreamID: "private-task", Model: "customer-model", Status: MinimaxVideoStatusDone,
					UpstreamCostUSD: 0.05, UpstreamCostReported: true, BilledCostUSD: 0.06, BillingMode: VideoBillingModeLocalPrice,
					OutputSecondsReported: reported, UsageReported: reported, DurationSeconds: 1, OutputTokens: 12000,
					ProviderResponse: json.RawMessage(`{"cost_usd":0.05,"getoken":{"cost_usd":0.05},"usage":{"cost_usd":0.05,"completion_tokens":12000},"procurement":{"actual_cost":0.05},"account_id":125,"content":{"video_url":"https://private-download"},"ratio":"16:9","fps":24,"seed":12,"generate_audio":true}`),
				}
				var body []byte
				if provider == "seedance" {
					body = SeedanceVideoStatusBody(task, "/local/content")
				} else {
					body = MinimaxVideoTaskStatusBody(task, "/local/content")
				}
				var result map[string]any
				require.NoError(t, json.Unmarshal(body, &result))
				for _, field := range []string{"cost_usd", "upstream_cost_usd", "upstream_cost_reported", "procurement", "getoken", "upstream_id", "account_id"} {
					require.NotContains(t, string(body), `"`+field+`"`)
				}
				require.NotContains(t, string(body), "private-")
				require.Equal(t, 0.06, result["billing"].(map[string]any)["billed_cost_usd"])
				if reported {
					usage := result["usage"].(map[string]any)
					if provider == "seedance" {
						require.Equal(t, float64(12000), usage["completion_tokens"])
					} else {
						require.Equal(t, float64(1), usage["output_seconds"])
					}
				} else {
					require.NotContains(t, result, "usage")
				}
				if provider == "seedance" {
					require.Equal(t, "16:9", result["ratio"])
					require.Equal(t, float64(24), result["fps"])
				}
			})
		}
	}
}
