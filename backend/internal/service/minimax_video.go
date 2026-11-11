package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// ResolveMinimaxVideoRateMultiplier follows the same user/group override and
// independent video-rate policy as the normal usage billing path.
func (s *OpenAIGatewayService) ResolveMinimaxVideoRateMultiplier(ctx context.Context, key *APIKey) float64 {
	rate := 1.0
	if s.cfg != nil && s.cfg.Default.RateMultiplier > 0 {
		rate = s.cfg.Default.RateMultiplier
	}
	if key != nil && key.Group != nil {
		rate = key.Group.RateMultiplier
		if key.GroupID != nil {
			rate = s.ResolveUserGroupRateMultiplier(ctx, key.UserID, *key.GroupID, rate)
		}
	}
	return resolveVideoRateMultiplier(key, rate)
}

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
// Its billing snapshot and state are stored in PostgreSQL before submission;
// completed jobs and pending settlements remain available across restarts.
type MinimaxVideoTask struct {
	ID                       string             `json:"id"`
	Provider                 string             `json:"provider,omitempty"`
	UpstreamID               string             `json:"upstream_id,omitempty"`
	UpstreamProtocol         string             `json:"upstream_protocol,omitempty"`
	UpstreamBaseURL          string             `json:"upstream_base_url,omitempty"`
	UpstreamCredentialHash   string             `json:"upstream_credential_hash,omitempty"`
	UpstreamRequestBody      json.RawMessage    `json:"upstream_request_body,omitempty"`
	RequestedDurationSeconds int                `json:"requested_duration_seconds,omitempty"`
	OutputSecondsReported    bool               `json:"output_seconds_reported,omitempty"`
	Ratio                    string             `json:"ratio,omitempty"`
	Status                   string             `json:"status"`
	Model                    string             `json:"model"`
	BillingModel             string             `json:"billing_model,omitempty"`
	UpstreamModel            string             `json:"upstream_model,omitempty"`
	Prompt                   string             `json:"prompt,omitempty"`
	DurationSeconds          int                `json:"duration_seconds,omitempty"`
	Resolution               string             `json:"resolution,omitempty"`
	UpstreamCostUSD          float64            `json:"upstream_cost_usd,omitempty"`
	UpstreamCostReported     bool               `json:"upstream_cost_reported,omitempty"`
	InputTokens              int                `json:"input_tokens,omitempty"`
	OutputTokens             int                `json:"output_tokens,omitempty"`
	UsageReported            bool               `json:"usage_reported,omitempty"`
	InputPricePerToken       float64            `json:"input_price_per_token,omitempty"`
	OutputPricePerToken      float64            `json:"output_price_per_token,omitempty"`
	TokenPricingConfigured   bool               `json:"token_pricing_configured,omitempty"`
	ProviderResponse         json.RawMessage    `json:"provider_response,omitempty"`
	ResolutionPrices         map[string]float64 `json:"resolution_prices,omitempty"`
	ResolutionTiers          map[string]string  `json:"resolution_tiers,omitempty"`
	// 计费快照：提交时固定，避免任务完成后分组改价导致对不上账。
	BillingMode        string                       `json:"billing_mode,omitempty"`
	PricePerSecond     float64                      `json:"price_per_second,omitempty"`
	PriceConfigured    bool                         `json:"price_configured,omitempty"`
	BillingSnapshot    *MinimaxVideoBillingSnapshot `json:"billing_snapshot,omitempty"`
	AdmissionCost      float64                      `json:"admission_cost,omitempty"`
	AdmissionCostKnown bool                         `json:"admission_cost_known,omitempty"`
	RateMultiplier     float64                      `json:"rate_multiplier,omitempty"`
	BilledCostUSD      float64                      `json:"billed_cost_usd,omitempty"`
	VideoURL           string                       `json:"video_url,omitempty"`
	Error              string                       `json:"error,omitempty"`
	UserID             int64                        `json:"user_id,omitempty"`
	APIKeyID           int64                        `json:"api_key_id,omitempty"`
	GroupID            int64                        `json:"group_id,omitempty"`
	AccountID          int64                        `json:"account_id,omitempty"`
	CreatedAt          string                       `json:"created_at,omitempty"`
	CompletedAt        string                       `json:"completed_at,omitempty"`
	ExpiresAt          string                       `json:"expires_at,omitempty"`
	LastPolledAt       string                       `json:"last_polled_at,omitempty"`
	CreateStarted      bool                         `json:"create_started,omitempty"`
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
	Ratio           string
	Content         json.RawMessage
}

// ParseMinimaxVideoRequest reads the OpenAI-ish video request body that clients
// send to /v1/videos/generations. Both a plain prompt and the multimodal
// "content" array are accepted.
func ParseMinimaxVideoRequest(body []byte) (MinimaxVideoRequestInfo, error) {
	var info MinimaxVideoRequestInfo
	if !utf8.Valid(body) || !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return info, fmt.Errorf("request body must be a JSON object")
	}
	if _, err := ll2MinimaxJSONObject(body); err != nil {
		return info, err
	}
	allowed := map[string]bool{"model": true, "prompt": true, "duration": true, "seconds": true, "duration_seconds": true, "resolution": true, "size": true, "output_resolution": true, "content": true, "image_url": true, "ratio": true}
	var unknown string
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		if !allowed[key.String()] {
			unknown = key.String()
			return false
		}
		return true
	})
	if unknown != "" {
		return info, fmt.Errorf("unsupported MiniMax video parameter %q", unknown)
	}
	modelValue := gjson.GetBytes(body, "model")
	model := strings.TrimSpace(modelValue.String())
	if modelValue.Type != gjson.String || model == "" {
		return info, fmt.Errorf("model is required")
	}
	info.Model = model
	if prompt := gjson.GetBytes(body, "prompt"); prompt.Exists() && prompt.Type != gjson.String {
		return info, fmt.Errorf("prompt must be a string")
	}
	info.Prompt = strings.TrimSpace(gjson.GetBytes(body, "prompt").String())
	if ratio := gjson.GetBytes(body, "ratio"); ratio.Exists() {
		if ratio.Type != gjson.String || strings.TrimSpace(ratio.String()) == "" {
			return info, fmt.Errorf("ratio must be a non-empty string")
		}
		info.Ratio = strings.TrimSpace(ratio.String())
	}
	info.DurationSeconds = 5
	durationSet := false
	for _, field := range []string{"duration", "seconds", "duration_seconds"} {
		if value := gjson.GetBytes(body, field); value.Exists() {
			duration, err := strconv.Atoi(value.String())
			if (value.Type != gjson.Number && value.Type != gjson.String) || err != nil || duration <= 0 || duration > 120 {
				return info, fmt.Errorf("%s must be an integer between 1 and 120", field)
			}
			info.DurationSeconds = duration
			if durationSet {
				return info, fmt.Errorf("use only one duration field")
			}
			durationSet = true
		}
	}
	info.Resolution = strings.TrimSpace(firstNonEmptyString(
		gjson.GetBytes(body, "resolution").String(),
		gjson.GetBytes(body, "size").String(),
		gjson.GetBytes(body, "output_resolution").String(),
	))
	resolutionCount := 0
	for _, field := range []string{"resolution", "size", "output_resolution"} {
		value := gjson.GetBytes(body, field)
		if value.Exists() {
			resolutionCount++
			if value.Type != gjson.String {
				return info, fmt.Errorf("resolution must be a string")
			}
		}
	}
	if resolutionCount > 1 {
		return info, fmt.Errorf("use only one resolution field")
	}

	// Multimodal form: {"content":[{"type":"text","text":"..."},{"type":"image_url",...}]}
	if content := gjson.GetBytes(body, "content"); content.Exists() {
		if !content.IsArray() || len(content.Array()) == 0 {
			return info, fmt.Errorf("content must be a non-empty array")
		}
		if gjson.GetBytes(body, "prompt").Exists() || gjson.GetBytes(body, "image_url").Exists() {
			return info, fmt.Errorf("use content or prompt/image_url, not both")
		}
		info.Content = json.RawMessage(content.Raw)
		var texts []string
		for _, item := range content.Array() {
			if !item.IsObject() {
				return info, fmt.Errorf("content items must be objects")
			}
			switch strings.TrimSpace(item.Get("type").String()) {
			case "text":
				if item.Get("text").Type != gjson.String {
					return info, fmt.Errorf("content text must be a string")
				}
				if t := strings.TrimSpace(item.Get("text").String()); t != "" {
					texts = append(texts, t)
				}
			case "image_url":
				if url := strings.TrimSpace(item.Get("image_url.url").String()); url != "" {
					info.InputImageURLs = append(info.InputImageURLs, url)
				}
			case "video_url", "audio_url":
				// LL2 validates media URLs and roles after account selection.
			default:
				return info, fmt.Errorf("unsupported video content type")
			}
		}
		if len(texts) > 0 && info.Prompt == "" {
			info.Prompt = strings.Join(texts, "\n")
		}
	}
	images := gjson.GetBytes(body, "image_url")
	if images.Exists() {
		if images.IsArray() {
			for _, image := range images.Array() {
				if image.Type != gjson.String || strings.TrimSpace(image.String()) == "" {
					return info, fmt.Errorf("image_url must contain URL strings")
				}
				info.InputImageURLs = append(info.InputImageURLs, strings.TrimSpace(image.String()))
			}
		} else if images.Type == gjson.String && strings.TrimSpace(images.String()) != "" {
			info.InputImageURLs = append(info.InputImageURLs, strings.TrimSpace(images.String()))
		} else {
			return info, fmt.Errorf("image_url must be a URL string or array")
		}
	}
	if strings.EqualFold(info.Model, "MiniMax-H3") {
		if info.Resolution == "" {
			info.Resolution = "768p"
		}
		if !strings.EqualFold(info.Resolution, "768p") {
			return info, fmt.Errorf("MiniMax-H3 supports only 768p resolution")
		}
		info.Resolution = "768p"
	}
	if info.Prompt == "" && len(info.InputImageURLs) == 0 {
		return info, fmt.Errorf("prompt is required")
	}
	return info, nil
}

// ModerationBody renders the auditable part of the request (model + prompt +
// input images) so the risk-control centre can inspect it exactly like the Grok
// media path does.
func (i MinimaxVideoRequestInfo) ModerationBody() []byte {
	if strings.TrimSpace(i.Prompt) == "" && len(i.InputImageURLs) == 0 {
		return nil
	}
	payload := map[string]any{"model": i.Model}
	if len(i.Content) > 0 {
		payload["content"] = i.Content
	}
	if i.Prompt != "" {
		payload["prompt"] = i.Prompt
	}
	if len(i.InputImageURLs) == 1 {
		payload["image_url"] = i.InputImageURLs[0]
	} else if len(i.InputImageURLs) > 1 {
		payload["image_url"] = i.InputImageURLs
	}
	if len(i.InputImageURLs) > 0 {
		images := make([]map[string]any, 0, len(i.InputImageURLs))
		for _, url := range i.InputImageURLs {
			images = append(images, map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}})
		}
		payload["images"] = images
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return body
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
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid MiniMax video base URL")
	}
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
	if info.Ratio != "" {
		return nil, fmt.Errorf("ratio requires the LL2 video protocol")
	}
	for _, item := range gjson.ParseBytes(info.Content).Array() {
		if kind := item.Get("type").String(); (kind != "text" && kind != "image_url") || item.Get("role").Exists() {
			return nil, fmt.Errorf("media roles, video and audio references require the LL2 video protocol")
		}
	}
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
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
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
		task.UpstreamCostReported = validVideoCost(cost)
	}
	if errField := root.Get("error"); errField.Exists() && errField.Type != gjson.Null {
		if msg := strings.TrimSpace(errField.Get("message").String()); msg != "" {
			task.Error = msg
		} else if s := strings.TrimSpace(errField.String()); s != "" && s != "null" {
			task.Error = s
		}
	}
	switch task.Status {
	case "succeeded", "completed", "success":
		task.Status = MinimaxVideoStatusDone
	case "failed", "cancelled", "canceled", "expired":
		task.Status = MinimaxVideoStatusError
	case "running", "in_progress":
		task.Status = MinimaxVideoStatusProcessing
	case "pending":
		task.Status = MinimaxVideoStatusQueued
	}
	if task.Error != "" && task.Status != MinimaxVideoStatusDone {
		task.Status = MinimaxVideoStatusError
	}
	if task.Status == "" {
		// A body that only carries a video URL is implicitly done.
		if task.VideoURL != "" {
			task.Status = MinimaxVideoStatusDone
		} else {
			task.Status = MinimaxVideoStatusProcessing
		}
	}
	if task.Status != MinimaxVideoStatusDone && task.Status != MinimaxVideoStatusError && task.Status != MinimaxVideoStatusQueued && task.Status != MinimaxVideoStatusProcessing {
		return task, fmt.Errorf("unknown MiniMax video status")
	}
	if task.Status == MinimaxVideoStatusDone && task.VideoURL == "" {
		return task, fmt.Errorf("completed MiniMax response is missing video URL")
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
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value.String()), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
		return 0
	}
	return parsed
}
