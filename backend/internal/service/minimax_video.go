package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// MiniMax H3 (and other OpenAI-shaped async video providers) integration.
//
// Upstream protocol (verified 2026-11-10 against https://www.getoken.tech):
//
//	POST {base}/videos/generations        -> blocks until the video is ready,
//	                                         returns request_id/status/video.url/getoken.cost_usd
//	GET  {base}/videos/{id}               -> fast status poll
//	GET  {base}/videos/{id}/content       -> video bytes
//
// Because the create call blocks for minutes, the gateway never forwards it
// synchronously: it records a task, returns an id immediately and lets a
// background worker finish the upstream call.
const (
	MinimaxVideoEndpointCreate  GrokMediaEndpoint = "minimax_video_create"
	MinimaxVideoEndpointStatus  GrokMediaEndpoint = "minimax_video_status"
	MinimaxVideoEndpointContent GrokMediaEndpoint = "minimax_video_content"

	// OpenAIEndpointCapabilityMinimaxVideo marks an account as able to serve
	// MiniMax-style video generation.
	OpenAIEndpointCapabilityMinimaxVideo OpenAIEndpointCapability = "minimax_video"

	// DefaultGetokenBaseURL is used when an account has no base_url configured.
	DefaultGetokenBaseURL = "https://www.getoken.tech/v1"

	minimaxVideoPathGenerations = "/videos/generations"
	minimaxVideoPathRoot        = "/videos/"
)

// MiniMax video task states (mirrored in the client-facing status field).
const (
	MinimaxVideoStatusQueued     = "queued"
	MinimaxVideoStatusProcessing = "processing"
	MinimaxVideoStatusDone       = "done"
	MinimaxVideoStatusError      = "error"
)

func (e GrokMediaEndpoint) IsMinimaxVideo() bool {
	return e == MinimaxVideoEndpointCreate || e == MinimaxVideoEndpointStatus || e == MinimaxVideoEndpointContent
}

// MinimaxVideoTask is the persisted state of one gateway-owned video job.
// It is stored through the gateway cache (same blob store as the Grok pending
// billing snapshot) so that an in-flight task survives process restarts.
type MinimaxVideoTask struct {
	ID              string  `json:"id"`
	UpstreamID      string  `json:"upstream_id,omitempty"`
	Status          string  `json:"status"`
	Model           string  `json:"model"`
	UpstreamModel   string  `json:"upstream_model,omitempty"`
	Prompt          string  `json:"prompt,omitempty"`
	DurationSeconds int     `json:"duration_seconds,omitempty"`
	Resolution      string  `json:"resolution,omitempty"`
	UpstreamCostUSD float64 `json:"upstream_cost_usd,omitempty"`
	VideoURL        string  `json:"video_url,omitempty"`
	Error           string  `json:"error,omitempty"`
	UserID          int64   `json:"user_id,omitempty"`
	APIKeyID        int64   `json:"api_key_id,omitempty"`
	GroupID         int64   `json:"group_id,omitempty"`
	AccountID       int64   `json:"account_id,omitempty"`
	CreatedAt       string  `json:"created_at,omitempty"`
	CompletedAt     string  `json:"completed_at,omitempty"`
}

// IsTerminal reports whether the task can no longer change state.
func (t MinimaxVideoTask) IsTerminal() bool {
	return t.Status == MinimaxVideoStatusDone || t.Status == MinimaxVideoStatusError
}

// MinimaxVideoRequestInfo carries the fields needed for create-time billing and
// upstream request construction.
type MinimaxVideoRequestInfo struct {
	Model           string
	Prompt          string
	DurationSeconds int
	Resolution      string
	InputImageURLs  []string
}

// ParseMinimaxVideoRequest reads the OpenAI-ish video request body that clients
// send to /v1/videos/generations. Both a plain prompt and the multimodal
// "content" array are accepted.
func ParseMinimaxVideoRequest(body []byte) (MinimaxVideoRequestInfo, error) {
	var info MinimaxVideoRequestInfo
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return info, fmt.Errorf("request body must be a JSON object")
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" {
		return info, fmt.Errorf("model is required")
	}
	info.Model = model
	info.Prompt = strings.TrimSpace(gjson.GetBytes(body, "prompt").String())
	if d := gjson.GetBytes(body, "duration"); d.Exists() {
		info.DurationSeconds = int(d.Int())
	} else if d := gjson.GetBytes(body, "seconds"); d.Exists() {
		info.DurationSeconds = int(d.Int())
	}
	if d := gjson.GetBytes(body, "duration_seconds"); d.Exists() && info.DurationSeconds <= 0 {
		info.DurationSeconds = int(d.Int())
	}
	info.Resolution = strings.TrimSpace(firstNonEmptyString(
		gjson.GetBytes(body, "resolution").String(),
		gjson.GetBytes(body, "size").String(),
		gjson.GetBytes(body, "output_resolution").String(),
	))

	// Multimodal form: {"content":[{"type":"text","text":"..."},{"type":"image_url",...}]}
	if content := gjson.GetBytes(body, "content"); content.IsArray() {
		var texts []string
		for _, item := range content.Array() {
			switch strings.TrimSpace(item.Get("type").String()) {
			case "text":
				if t := strings.TrimSpace(item.Get("text").String()); t != "" {
					texts = append(texts, t)
				}
			case "image_url":
				if url := strings.TrimSpace(item.Get("image_url.url").String()); url != "" {
					info.InputImageURLs = append(info.InputImageURLs, url)
				}
			}
		}
		if len(texts) > 0 && info.Prompt == "" {
			info.Prompt = strings.Join(texts, "\n")
		}
	}
	if url := strings.TrimSpace(gjson.GetBytes(body, "image_url").String()); url != "" {
		info.InputImageURLs = append(info.InputImageURLs, url)
	}
	if info.Prompt == "" && len(info.InputImageURLs) == 0 {
		return info, fmt.Errorf("prompt is required")
	}
	return info, nil
}

// MinimaxVideoBaseURL returns the configured upstream base URL without a
// trailing slash, falling back to the Getoken gateway.
func MinimaxVideoBaseURL(account *Account) string {
	base := ""
	if account != nil {
		base = strings.TrimSpace(account.GetCredential("base_url"))
	}
	if base == "" {
		base = DefaultGetokenBaseURL
	}
	base = strings.TrimRight(base, "/")
	// Operators often paste the host without the /v1 suffix.
	if !strings.HasSuffix(base, "/v1") && !strings.Contains(base, "/v1/") {
		base += "/v1"
	}
	return base
}

// BuildMinimaxVideoURL builds the upstream URL for one endpoint/task.
func BuildMinimaxVideoURL(base string, endpoint GrokMediaEndpoint, taskID string) (string, error) {
	base = strings.TrimRight(base, "/")
	switch endpoint {
	case MinimaxVideoEndpointCreate:
		return base + minimaxVideoPathGenerations, nil
	case MinimaxVideoEndpointStatus, MinimaxVideoEndpointContent:
		id := strings.TrimSpace(taskID)
		if id == "" {
			return "", fmt.Errorf("minimax video task id is required")
		}
		if err := validateUpstreamPathSegment("MiniMax video task ID", id); err != nil {
			return "", err
		}
		url := base + minimaxVideoPathRoot + id
		if endpoint == MinimaxVideoEndpointContent {
			url += "/content"
		}
		return url, nil
	default:
		return "", fmt.Errorf("unsupported minimax video endpoint: %s", endpoint)
	}
}

// BuildMinimaxVideoUpstreamBody rewrites the client body into the upstream
// shape: the model is mapped through the account mapping and only documented
// fields are forwarded, so clients cannot inject arbitrary upstream options.
func BuildMinimaxVideoUpstreamBody(info MinimaxVideoRequestInfo, upstreamModel string) ([]byte, error) {
	model := strings.TrimSpace(upstreamModel)
	if model == "" {
		model = info.Model
	}
	payload := map[string]any{"model": model}
	if info.Prompt != "" {
		payload["prompt"] = info.Prompt
	}
	if info.DurationSeconds > 0 {
		payload["duration"] = info.DurationSeconds
	}
	if info.Resolution != "" {
		payload["resolution"] = info.Resolution
	}
	switch len(info.InputImageURLs) {
	case 0:
	case 1:
		payload["image_url"] = info.InputImageURLs[0]
	default:
		payload["image_url"] = info.InputImageURLs
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal minimax video request: %w", err)
	}
	return body, nil
}

// ParseMinimaxVideoUpstreamResponse extracts task state from either the
// blocking create response or a status poll response. Both share the same
// shape, so one parser covers both.
func ParseMinimaxVideoUpstreamResponse(body []byte) (MinimaxVideoTask, error) {
	var task MinimaxVideoTask
	if !gjson.ValidBytes(body) {
		return task, fmt.Errorf("upstream returned invalid JSON")
	}
	root := gjson.ParseBytes(body)
	task.UpstreamID = strings.TrimSpace(root.Get("request_id").String())
	if task.UpstreamID == "" {
		task.UpstreamID = strings.TrimSpace(root.Get("id").String())
	}
	task.Status = strings.ToLower(strings.TrimSpace(root.Get("status").String()))
	task.Resolution = strings.TrimSpace(firstNonEmptyString(
		root.Get("output_resolution").String(),
		root.Get("resolution").String(),
	))
	if d := root.Get("duration"); d.Exists() {
		task.DurationSeconds = int(d.Int())
	}
	task.VideoURL = strings.TrimSpace(root.Get("video.url").String())
	if task.VideoURL == "" {
		task.VideoURL = strings.TrimSpace(root.Get("url").String())
	}
	if cost := root.Get("getoken.cost_usd"); cost.Exists() {
		task.UpstreamCostUSD = parseFlexibleFloat(cost)
	}
	if errField := root.Get("error"); errField.Exists() && errField.Type != gjson.Null {
		if msg := strings.TrimSpace(errField.Get("message").String()); msg != "" {
			task.Error = msg
		} else if s := strings.TrimSpace(errField.String()); s != "" && s != "null" {
			task.Error = s
		}
	}
	if task.Status == "" {
		// A body that only carries a video URL is implicitly done.
		if task.VideoURL != "" {
			task.Status = MinimaxVideoStatusDone
		} else {
			task.Status = MinimaxVideoStatusProcessing
		}
	}
	return task, nil
}

// MinimaxVideoTaskID generates a gateway-owned task id. It is prefixed so that
// logs and cache keys are recognizable and cannot collide with upstream ids.
func MinimaxVideoTaskID() string {
	return "mmv_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// MinimaxVideoTaskTTL is how long a finished task stays queryable. Billing
// snapshots use the same window so late polls still settle correctly.
func MinimaxVideoTaskTTL(fallback time.Duration) time.Duration {
	if fallback <= 0 {
		return 24 * time.Hour
	}
	return fallback
}

// parseFlexibleFloat accepts both numeric and string-encoded floats, which the
// upstream mixes (cost_usd arrives as a decimal string).
func parseFlexibleFloat(value gjson.Result) float64 {
	if value.Type == gjson.Number {
		return value.Float()
	}
	s := strings.TrimSpace(value.String())
	if s == "" {
		return 0
	}
	var out float64
	if _, err := fmt.Sscanf(s, "%f", &out); err != nil {
		return 0
	}
	return out
}
