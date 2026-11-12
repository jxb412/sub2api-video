package handler

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Customer responses use an allowlist. Store and admin records also contain
// procurement costs and routing metadata that must not cross this boundary.
type customerVideoTask struct {
	TaskID          string     `json:"task_id"`
	Provider        string     `json:"provider"`
	Model           string     `json:"model"`
	Prompt          string     `json:"prompt"`
	Status          string     `json:"status"`
	Resolution      string     `json:"resolution,omitempty"`
	DurationSeconds int        `json:"duration_seconds"`
	UserID          int64      `json:"user_id"`
	APIKeyID        int64      `json:"api_key_id"`
	GroupID         int64      `json:"group_id"`
	ErrorMessage    string     `json:"error_message,omitempty"`
	BilledCostUSD   float64    `json:"billed_cost_usd"`
	BillingMode     string     `json:"billing_mode"`
	CreatedAt       time.Time  `json:"created_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	DeletedAt       *time.Time `json:"deleted_at,omitempty"`
	SettledAt       *time.Time `json:"settled_at,omitempty"`
	InputTokens     int        `json:"input_tokens"`
	OutputTokens    int        `json:"output_tokens"`
	BillingStatus   string     `json:"billing_status"`
	GroupName       string     `json:"group_name,omitempty"`
	APIKeyName      string     `json:"api_key_name,omitempty"`
}

func customerVideoTaskFromRecord(rec service.VideoTaskRecord) customerVideoTask {
	return customerVideoTask{
		TaskID: rec.TaskID, Provider: rec.Provider, Model: rec.Model, Prompt: rec.Prompt,
		Status: rec.Status, Resolution: rec.Resolution, DurationSeconds: rec.DurationSeconds,
		UserID: rec.UserID, APIKeyID: rec.APIKeyID, GroupID: rec.GroupID,
		ErrorMessage: rec.ErrorMessage, BilledCostUSD: rec.BilledCostUSD, BillingMode: rec.BillingMode,
		CreatedAt: rec.CreatedAt, CompletedAt: rec.CompletedAt, DeletedAt: rec.DeletedAt, SettledAt: rec.SettledAt,
		InputTokens: rec.InputTokens, OutputTokens: rec.OutputTokens, BillingStatus: rec.BillingStatus,
		GroupName: rec.GroupName, APIKeyName: rec.APIKeyName,
	}
}

type customerVideoTaskStats struct {
	Total       int64   `json:"total"`
	Queued      int64   `json:"queued"`
	Processing  int64   `json:"processing"`
	Done        int64   `json:"done"`
	Error       int64   `json:"error"`
	Today       int64   `json:"today"`
	BilledCost  float64 `json:"billed_cost_usd"`
	SuccessRate float64 `json:"success_rate"`
}

func customerVideoStatsFromRecord(stats service.VideoTaskStats) customerVideoTaskStats {
	return customerVideoTaskStats{
		Total: stats.Total, Queued: stats.Queued, Processing: stats.Processing,
		Done: stats.Done, Error: stats.Error, Today: stats.Today,
		BilledCost: stats.BilledCost, SuccessRate: stats.SuccessRate,
	}
}
