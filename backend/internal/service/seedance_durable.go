package service

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const VideoProviderSeedance = "seedance"

var seedanceVideoOption = regexp.MustCompile(`(?:^|\s)--(duration|dur|resolution|rs)\s+([^\s]+)`)

// ParseSeedanceVideoRequest extracts billing metadata without dropping native
// multimodal and future Ark fields. The original JSON is forwarded separately.
func ParseSeedanceVideoRequest(body []byte) (MinimaxVideoRequestInfo, error) {
	parsed, err := ParseSeedanceRequest(body)
	if err != nil {
		return MinimaxVideoRequestInfo{}, err
	}
	info := MinimaxVideoRequestInfo{Model: parsed.Model, Prompt: parsed.Prompt, InputImageURLs: parsed.InputImageURLs}
	if gjson.GetBytes(body, "return_last_frame").Bool() {
		return info, fmt.Errorf("return_last_frame is not supported by the protected video gateway")
	}
	if gjson.GetBytes(body, "callback_url").Exists() {
		return info, fmt.Errorf("callback_url is not supported; poll the gateway task ID")
	}
	if gjson.GetBytes(body, "frames").Exists() && gjson.GetBytes(body, "duration").Exists() {
		return info, fmt.Errorf("frames and duration are mutually exclusive")
	}
	for _, item := range gjson.GetBytes(body, "content").Array() {
		typ := item.Get("type").String()
		if typ == "text" {
			if item.Get("text").Type != gjson.String || strings.TrimSpace(item.Get("text").String()) == "" {
				return info, fmt.Errorf("text content must contain non-empty text")
			}
		} else if typ == "image_url" || typ == "video_url" || typ == "audio_url" {
			value := item.Get(typ + ".url")
			if value.Type != gjson.String || strings.TrimSpace(value.String()) == "" {
				return info, fmt.Errorf("%s content must contain a URL", typ)
			}
		} else {
			return info, fmt.Errorf("unsupported Seedance content type %q", typ)
		}
	}
	if value := gjson.GetBytes(body, "duration"); value.Exists() {
		if value.Type != gjson.Number {
			return info, fmt.Errorf("duration must be an integer")
		}
		v, err := strconv.Atoi(value.Raw)
		if err != nil || (v != -1 && (v < 1 || v > 120)) {
			return info, fmt.Errorf("duration must be -1 or an integer between 1 and 120")
		}
		info.DurationSeconds = v
	}
	if value := gjson.GetBytes(body, "resolution"); value.Exists() {
		if value.Type != gjson.String {
			return info, fmt.Errorf("resolution must be a string")
		}
		info.Resolution = strings.ToLower(strings.TrimSpace(value.String()))
	}
	for _, option := range seedanceVideoOption.FindAllStringSubmatch(info.Prompt, -1) {
		if option[1] == "dur" || option[1] == "duration" {
			v, err := strconv.Atoi(option[2])
			if err != nil || (v != -1 && (v < 1 || v > 120)) {
				return info, fmt.Errorf("invalid Seedance duration option")
			}
			if info.DurationSeconds != 0 && info.DurationSeconds != v {
				return info, fmt.Errorf("conflicting Seedance duration fields")
			}
			info.DurationSeconds = v
		} else {
			v := strings.ToLower(option[2])
			if info.Resolution != "" && info.Resolution != v {
				return info, fmt.Errorf("conflicting Seedance resolution fields")
			}
			info.Resolution = v
		}
	}
	if gjson.GetBytes(body, "frames").Exists() && info.DurationSeconds != 0 {
		return info, fmt.Errorf("frames and duration options are mutually exclusive")
	}
	if info.Resolution != "" {
		if _, ok := LookupVideoBillingResolution(info.Resolution); !ok {
			return info, fmt.Errorf("resolution must be 480p, 720p or 1080p")
		}
	}
	return info, nil
}

func SeedanceVideoTaskID() string { return "sdv_" + strings.ReplaceAll(uuid.NewString(), "-", "") }

func SeedanceVideoLifetime(body []byte) (time.Duration, error) {
	seconds := int64(172800)
	if value := gjson.GetBytes(body, "execution_expires_after"); value.Exists() {
		if value.Type != gjson.Number {
			return 0, fmt.Errorf("execution_expires_after must be an integer")
		}
		var err error
		seconds, err = strconv.ParseInt(value.Raw, 10, 32)
		if err != nil || seconds < 3600 || seconds > 259200 {
			return 0, fmt.Errorf("execution_expires_after must be between 3600 and 259200 seconds")
		}
	}
	return time.Duration(seconds) * time.Second, nil
}
func (s *OpenAIGatewayService) VideoChannelMapping(ctx context.Context, key *APIKey, model string) ChannelMappingResult {
	if s.channelService != nil && key.GroupID != nil {
		return s.channelService.ResolveChannelMapping(ctx, *key.GroupID, model)
	}
	return ChannelMappingResult{MappedModel: model}
}

func (s *OpenAIGatewayService) SeedanceVideoRequest(account *Account, body []byte, model string) (string, []byte, error) {
	if account == nil || !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance) {
		return "", nil, fmt.Errorf("Seedance account capability is not configured")
	}
	base, err := s.validateUpstreamBaseURL(account.GetCredential("base_url"))
	if err != nil {
		return "", nil, err
	}
	target, err := buildSeedanceURL(base, SeedanceEndpointCreate, "")
	if err != nil {
		return "", nil, err
	}
	mapped, err := sjson.SetBytes(body, "model", model)
	return target, mapped, err
}

// PrepareVideoBilling fixes the entire tariff before any provider submission.
// Missing prices are configuration errors; explicitly configured zero is valid.
func (s *OpenAIGatewayService) PrepareVideoBilling(ctx context.Context, task *MinimaxVideoTask, key *APIKey) error {
	if task.BillingModel == "" {
		task.BillingModel = task.Model
	}
	settings := VideoBillingSettingsFor(key.Group)
	task.BillingMode, task.RateMultiplier = settings.Mode, s.ResolveMinimaxVideoRateMultiplier(ctx, key)
	if task.UpstreamProtocol == MinimaxVideoProtocolLL2 && task.BillingMode != VideoBillingModeLocalPrice {
		return fmt.Errorf("LL2 reports output seconds without monetary cost; configure local_price per-second video pricing for this group")
	}
	task.ResolutionTiers = settings.Tiers
	switch task.BillingMode {
	case VideoBillingModeLocalPrice:
		if task.DurationSeconds <= 0 || strings.TrimSpace(task.Resolution) == "" {
			return fmt.Errorf("local_price requires an explicit positive duration and resolution")
		}
		task.ResolutionPrices = make(map[string]float64)
		if key.Group != nil {
			for _, tier := range []string{"480p", "720p", "1080p"} {
				if price := key.Group.GetVideoPriceForModel(task.BillingModel, tier); price != nil {
					task.ResolutionPrices[tier] = *price
				}
			}
		}
		price, ok := task.ResolutionPrices[ResolveVideoBillingTier(task.Resolution, settings.Tiers)]
		if !ok || !validVideoAmount(price) {
			return fmt.Errorf("video price is not configured for this model and resolution")
		}
		task.PricePerSecond, task.PriceConfigured = price, true
	case VideoBillingModeUsageTokens:
		if task.Provider != VideoProviderSeedance {
			return fmt.Errorf("usage_tokens billing is supported only for Seedance")
		}
		if s.billingService == nil {
			return fmt.Errorf("video token pricing is unavailable")
		}
		resolver := s.resolver
		if resolver == nil {
			resolver = NewModelPricingResolver(s.channelService, s.billingService)
		}
		resolved := resolver.Resolve(ctx, PricingInput{Model: task.BillingModel, GroupID: key.GroupID, Group: key.Group})
		if resolved == nil || resolved.Mode != BillingModeToken {
			return fmt.Errorf("Seedance usage_tokens requires a token price card")
		}
		if len(resolved.Intervals) > 0 {
			return fmt.Errorf("Seedance token pricing requires flat rates; tiered channel prices are not supported")
		}
		pricing := resolver.GetIntervalPricing(resolved, 1)
		if pricing == nil {
			return fmt.Errorf("Seedance token prices are not configured")
		}
		// A provider reports completion tokens, not prompt-language token estimates.
		// A zero output rate is valid only when explicitly supplied in a price card.
		explicitOutput := resolved.channelPricing != nil && resolved.channelPricing.OutputPrice != nil
		if pricing.OutputPricePerToken == 0 && !explicitOutput {
			return fmt.Errorf("Seedance output token price is not configured")
		}
		multiplier := resolvedChannelTimeMultiplier(resolved, time.Now())
		task.InputPricePerToken, task.OutputPricePerToken = pricing.InputPricePerToken*multiplier, pricing.OutputPricePerToken*multiplier
		if !validVideoAmount(task.InputPricePerToken) || !validVideoAmount(task.OutputPricePerToken) {
			return fmt.Errorf("invalid Seedance token price")
		}
		task.TokenPricingConfigured = true
	case VideoBillingModeUpstreamCost:
		// Completion must contain a valid actual cost; missing cost remains pending
		// settlement and is never silently interpreted as free.
	default:
		return fmt.Errorf("unsupported video billing mode")
	}
	return nil
}

func validVideoAmount(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
func validVideoCost(value gjson.Result) bool {
	if value.Type != gjson.Number && value.Type != gjson.String {
		return false
	}
	v, err := strconv.ParseFloat(value.String(), 64)
	return err == nil && validVideoAmount(v)
}

func ParseSeedanceVideoResponse(body []byte) (MinimaxVideoTask, error) {
	var task MinimaxVideoTask
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return task, fmt.Errorf("Seedance returned invalid JSON")
	}
	root := gjson.ParseBytes(body)
	task.Provider, task.UpstreamID = VideoProviderSeedance, strings.TrimSpace(root.Get("id").String())
	if task.UpstreamID != "" {
		if err := validateUpstreamPathSegment("Seedance task ID", task.UpstreamID); err != nil {
			return task, err
		}
	}
	switch root.Get("status").String() {
	case "", "queued", "pending":
		task.Status = MinimaxVideoStatusQueued
	case "running", "processing":
		task.Status = MinimaxVideoStatusProcessing
	case "succeeded":
		task.Status = MinimaxVideoStatusDone
	case "failed", "cancelled", "canceled", "expired":
		task.Status = MinimaxVideoStatusError
	default:
		return task, fmt.Errorf("unknown Seedance task status")
	}
	task.VideoURL = strings.TrimSpace(root.Get("content.video_url").String())
	task.Resolution = strings.ToLower(strings.TrimSpace(root.Get("resolution").String()))
	if duration := root.Get("duration"); duration.Exists() {
		v, err := strconv.Atoi(duration.String())
		if err != nil || v <= 0 || v > 120 {
			return task, fmt.Errorf("invalid Seedance output duration")
		}
		task.DurationSeconds = v
	}
	if value := root.Get("usage.completion_tokens"); value.Exists() {
		v, err := strconv.ParseInt(value.String(), 10, 32)
		if err != nil || v < 0 {
			return task, fmt.Errorf("invalid Seedance completion token usage")
		}
		task.OutputTokens, task.UsageReported = int(v), true
	}
	if value := root.Get("usage.prompt_tokens"); value.Exists() {
		v, err := strconv.ParseInt(value.String(), 10, 32)
		if err != nil || v < 0 {
			return task, fmt.Errorf("invalid Seedance prompt token usage")
		}
		task.InputTokens = int(v)
	}
	for _, path := range []string{"usage.cost_usd", "cost_usd", "getoken.cost_usd"} {
		if value := root.Get(path); value.Exists() {
			if !validVideoCost(value) {
				return task, fmt.Errorf("invalid Seedance upstream cost")
			}
			task.UpstreamCostUSD, task.UpstreamCostReported = parseFlexibleFloat(value), true
			break
		}
	}
	if task.Status == MinimaxVideoStatusDone && task.VideoURL == "" {
		return task, fmt.Errorf("Seedance success response is missing content.video_url")
	}
	if task.Status == MinimaxVideoStatusError {
		task.Error = "Video generation failed, was cancelled, or expired"
	}
	if task.UpstreamID == "" {
		return task, fmt.Errorf("Seedance response is missing task ID")
	}
	task.ProviderResponse = append(json.RawMessage(nil), body...)
	return task, nil
}

func (s *OpenAIGatewayService) callSeedanceVideo(ctx context.Context, account *Account, method, target string, body []byte) (MinimaxVideoTask, error) {
	var result MinimaxVideoTask
	if account == nil || strings.TrimSpace(account.GetCredential("api_key")) == "" {
		return result, fmt.Errorf("Seedance account credential unavailable")
	}
	if method == http.MethodPost {
		ctx = WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileVideo)
	}
	req, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(ctx), method, target, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Authorization", "Bearer "+account.GetCredential("api_key"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	account.ApplyHeaderOverrides(req.Header)
	proxy := ""
	if account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	var resp *http.Response
	if s.httpUpstream != nil {
		resp, err = s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	} else {
		// The immutable task policy bounds ctx; do not silently cap a configured
		// request timeout at the legacy 30-second fallback client timeout.
		resp, err = MinimaxVideoUpstreamClient(20 * time.Minute).Do(req)
	}
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return result, err
	}
	if len(payload) > 8<<20 {
		return result, fmt.Errorf("Seedance response exceeds size limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result, &MinimaxVideoUpstreamError{StatusCode: resp.StatusCode}
	}
	return ParseSeedanceVideoResponse(payload)
}

// RunSeedanceVideoTask submits once. A successful provider ID is persisted before
// polling; after a process restart the recovery worker resumes polling that ID.
func (s *OpenAIGatewayService) RunSeedanceVideoTask(ctx context.Context, task MinimaxVideoTask, account *Account, target string, body []byte) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("seedance_video.worker_panic", "task_id", task.ID, "panic", recovered)
		}
	}()
	claimed, err := claimMinimaxVideoTask(ctx, task.ID, task.UserID, task.APIKeyID)
	if err != nil || !claimed {
		return
	}
	task.Status = MinimaxVideoStatusProcessing
	task.CreateStarted = true
	result, err := runVideoProviderAttempt(ctx, videoTaskHandle(), task, 45*time.Second, func(submitCtx context.Context) (MinimaxVideoTask, error) {
		result, err := s.callSeedanceVideo(submitCtx, account, http.MethodPost, target, body)
		if err == nil && result.Status == MinimaxVideoStatusDone {
			err = s.ValidateVideoDownloadURL(result.VideoURL, account.GetCredential("base_url"))
		}
		if err == nil && strings.TrimSpace(result.UpstreamID) == "" {
			err = fmt.Errorf("Seedance submission returned no provider task ID")
		}
		return result, err
	})
	if err != nil {
		if errors.Is(err, ErrVideoExecutionPaused) {
			return
		}
		if ll2SubmissionDefinitelyRejected(err, false) {
			task.Status, task.Error, task.CompletedAt = MinimaxVideoStatusError, "Video request rejected by the provider", GrokVideoPendingCreatedAtNow()
		} else {
			task.Status = MinimaxVideoStatusProcessing
			if result.UpstreamID != "" && validateUpstreamPathSegment("Seedance video task ID", result.UpstreamID) == nil {
				task.UpstreamID = result.UpstreamID
			}
			cleanup, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
			if task.UpstreamID == "" {
				_ = pauseVideoExecution(cleanup, videoTaskHandle(), task, "submission_uncertain", "Provider acceptance is unknown; this submission cannot be replayed safely")
			}
			cancelCleanup()
		}
		s.persistVideoOutcome(task)
		return
	}
	mergeVideoProviderResult(&task, result)
	if err := s.persistVideoOutcome(task); err != nil || task.IsTerminal() {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		done, err := s.pollSeedanceTask(ctx, task.ID, task.UserID, task.APIKeyID)
		if done {
			return
		}
		if err != nil && (ctx.Err() != nil || errors.Is(err, ErrVideoExecutionPaused)) {
			return
		}
	}
}

func mergeVideoProviderResult(task *MinimaxVideoTask, result MinimaxVideoTask) {
	task.UpstreamID, task.Status = result.UpstreamID, result.Status
	task.ProviderResponse = result.ProviderResponse
	if result.Resolution != "" {
		task.Resolution = result.Resolution
	}
	if result.DurationSeconds > 0 {
		task.DurationSeconds = result.DurationSeconds
	}
	if task.UpstreamProtocol == MinimaxVideoProtocolLL2 {
		task.OutputSecondsReported = result.OutputSecondsReported
		if !result.OutputSecondsReported {
			task.DurationSeconds = task.RequestedDurationSeconds
		}
	}
	task.VideoURL, task.Error = result.VideoURL, result.Error
	task.UpstreamCostUSD, task.UpstreamCostReported = result.UpstreamCostUSD, result.UpstreamCostReported
	task.InputTokens, task.OutputTokens, task.UsageReported = result.InputTokens, result.OutputTokens, result.UsageReported
	if task.BillingMode == VideoBillingModeLocalPrice && len(task.ResolutionPrices) > 0 {
		price, ok := task.ResolutionPrices[ResolveVideoBillingTier(task.Resolution, task.ResolutionTiers)]
		task.PricePerSecond, task.PriceConfigured = price, ok
	}
	if task.IsTerminal() {
		task.CompletedAt = GrokVideoPendingCreatedAtNow()
	}
}

func (s *OpenAIGatewayService) persistVideoOutcome(task MinimaxVideoTask) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = s.StoreMinimaxVideoTask(ctx, task)
		if err == nil || errors.Is(err, ErrVideoTaskTerminal) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	if err == nil && task.Status == MinimaxVideoStatusDone {
		_, err = s.settleMinimaxVideoTask(ctx, task)
	}
	return err
}

// PostgreSQL session locks serialize polling across worker goroutines and replicas.
// The connection is dedicated until unlock, and a crashed process releases it.
func (s *OpenAIGatewayService) pollSeedanceTask(ctx context.Context, id string, userID, keyID int64) (bool, error) {
	db := videoTaskHandle()
	if db == nil {
		return false, fmt.Errorf("video database unavailable")
	}
	// Account repository lookups happen before taking a dedicated connection. A
	// one-connection pool must never wait on itself while holding the task lock.
	preliminary, err := loadMinimaxVideoTaskFromDB(ctx, id, userID, keyID)
	if err != nil || preliminary == nil {
		return preliminary == nil, err
	}
	if preliminary.IsTerminal() && (preliminary.Status != MinimaxVideoStatusDone || !videoTaskNeedsProviderMetadata(*preliminary)) {
		return true, nil
	}
	ctx, cancel := context.WithTimeout(ctx, videoRequestTimeout(*preliminary, 30*time.Second)+5*time.Second)
	defer cancel()
	if s.accountRepo == nil {
		return false, fmt.Errorf("video account repository unavailable")
	}
	account, err := s.accountRepo.GetByID(ctx, preliminary.AccountID)
	if err != nil || account == nil {
		_, _ = db.ExecContext(ctx, `UPDATE video_tasks SET task_snapshot=jsonb_set(task_snapshot,'{last_polled_at}',to_jsonb(now()::text)) WHERE task_id=$1 AND settled_at IS NULL`, id)
		if err == nil {
			err = fmt.Errorf("video account unavailable")
		}
		_ = pauseVideoExecution(ctx, db, *preliminary, "configuration_changed", "Original provider account configuration is unavailable or changed")
		return false, err
	}
	base, err := s.validateUpstreamBaseURL(account.GetCredential("base_url"))
	if err != nil {
		_, _ = db.ExecContext(ctx, `UPDATE video_tasks SET task_snapshot=jsonb_set(task_snapshot,'{last_polled_at}',to_jsonb(now()::text)) WHERE task_id=$1 AND settled_at IS NULL`, id)
		_ = pauseVideoExecution(ctx, db, *preliminary, "configuration_changed", "Original provider endpoint configuration is unavailable or changed")
		return false, err
	}
	if (preliminary.UpstreamBaseURL != "" && preliminary.UpstreamBaseURL != base) || (preliminary.UpstreamCredentialHash != "" && preliminary.UpstreamCredentialHash != minimaxCredentialHash(account)) {
		_ = pauseVideoExecution(ctx, db, *preliminary, "configuration_changed", "Original provider endpoint or credential changed")
		return false, fmt.Errorf("Seedance account endpoint or credential changed")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	lockKey := "seedance-video-poll:" + id
	var claimed bool
	if err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", lockKey).Scan(&claimed); err != nil || !claimed {
		conn.Close()
		return false, err
	}
	var releaseOnce sync.Once
	releaseLock := func() {
		releaseOnce.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", lockKey); err != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
			_ = conn.Close()
		})
	}
	defer releaseLock()
	task, err := loadMinimaxVideoTaskWithQueryer(ctx, conn, id, userID, keyID)
	if err != nil || task == nil {
		return task == nil, err
	}
	wasDone := task.Status == MinimaxVideoStatusDone
	if task.IsTerminal() && (!wasDone || !videoTaskNeedsProviderMetadata(*task)) {
		return true, nil
	}
	if task.Provider != VideoProviderSeedance || task.UpstreamID == "" {
		return false, nil
	}
	if task.AccountID != account.ID {
		return false, fmt.Errorf("video account binding changed")
	}
	task.LastPolledAt = time.Now().UTC().Format(time.RFC3339Nano)
	if !wasDone {
		if err := saveMinimaxVideoTaskWithExecer(ctx, conn, *task); err != nil {
			return errors.Is(err, ErrVideoTaskTerminal), err
		}
	} else {
		if _, err := conn.ExecContext(ctx, `UPDATE video_tasks SET task_snapshot=jsonb_set(task_snapshot,'{last_polled_at}',to_jsonb(now()::text)) WHERE task_id=$1 AND settled_at IS NULL`, id); err != nil {
			return false, err
		}
	}
	persistLocked := func() (bool, error) {
		err := saveMinimaxVideoTaskWithExecer(ctx, conn, *task)
		releaseLock()
		if err == nil && task.Status == MinimaxVideoStatusDone {
			_, err = s.settleMinimaxVideoTask(ctx, *task)
		}
		return task.IsTerminal(), err
	}
	target, err := buildSeedanceURL(base, SeedanceEndpointStatus, task.UpstreamID)
	if err != nil {
		return false, err
	}
	result, err := runVideoProviderAttempt(ctx, conn, *task, 30*time.Second, func(pollCtx context.Context) (MinimaxVideoTask, error) {
		result, err := s.callSeedanceVideo(pollCtx, account, http.MethodGet, target, nil)
		if err != nil {
			return result, err
		}
		if result.UpstreamID != task.UpstreamID {
			return result, fmt.Errorf("Seedance poll returned a different task ID")
		}
		if result.Status == MinimaxVideoStatusDone {
			if err := s.ValidateVideoDownloadURL(result.VideoURL, base); err != nil {
				return result, err
			}
		} else if wasDone {
			return result, fmt.Errorf("provider tried to regress a completed task")
		}
		return result, nil
	})
	if err != nil {
		return false, err
	}
	if wasDone {
		if result.Status != MinimaxVideoStatusDone {
			return false, fmt.Errorf("provider tried to regress a completed task")
		}
		if !task.UsageReported && result.UsageReported {
			task.InputTokens, task.OutputTokens, task.UsageReported = result.InputTokens, result.OutputTokens, true
		}
		if !task.UpstreamCostReported && result.UpstreamCostReported {
			task.UpstreamCostUSD, task.UpstreamCostReported = result.UpstreamCostUSD, true
		}
		payload, err := json.Marshal(task)
		if err != nil {
			return false, err
		}
		res, err := conn.ExecContext(ctx, `UPDATE video_tasks SET task_snapshot=$2,upstream_cost_usd=$3 WHERE task_id=$1 AND status='done' AND settled_at IS NULL AND deleted_at IS NULL`, task.ID, string(payload), task.UpstreamCostUSD)
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		releaseLock()
		if err != nil || n == 0 {
			return true, err
		}
		_, err = s.settleMinimaxVideoTask(ctx, *task)
		return err == nil, err
	}
	mergeVideoProviderResult(task, result)
	return persistLocked()
}

func videoTaskNeedsProviderMetadata(task MinimaxVideoTask) bool {
	return (task.UpstreamProtocol == MinimaxVideoProtocolLL2 && !task.OutputSecondsReported) || (task.BillingMode == VideoBillingModeUsageTokens && !task.UsageReported) || (task.BillingMode == VideoBillingModeUpstreamCost && !task.UpstreamCostReported)
}

func SeedanceVideoStatusBody(task MinimaxVideoTask, contentPath string) []byte {
	out := make(map[string]any)
	if len(task.ProviderResponse) > 0 {
		_ = json.Unmarshal(task.ProviderResponse, &out)
	}
	out["id"], out["model"] = task.ID, task.Model
	if task.UpstreamID != "" {
		out["upstream_id"] = task.UpstreamID
	}
	status := map[string]string{MinimaxVideoStatusQueued: "queued", MinimaxVideoStatusProcessing: "running", MinimaxVideoStatusDone: "succeeded", MinimaxVideoStatusError: "failed"}[task.Status]
	out["status"] = status
	if created, err := time.Parse(time.RFC3339Nano, task.CreatedAt); err == nil {
		out["created_at"] = created.Unix()
	}
	if completed, err := time.Parse(time.RFC3339Nano, task.CompletedAt); err == nil {
		out["updated_at"] = completed.Unix()
	}
	if task.DurationSeconds > 0 {
		out["duration"] = task.DurationSeconds
	}
	if task.Resolution != "" {
		out["resolution"] = task.Resolution
	}
	// Never expose signed upstream download URLs through the gateway status route.
	delete(out, "content")
	if task.Status == MinimaxVideoStatusDone {
		out["content"] = map[string]any{"video_url": contentPath}
	}
	delete(out, "error")
	if task.Error != "" {
		out["error"] = map[string]string{"code": "video_generation_failed", "message": task.Error}
	}
	if task.UsageReported {
		out["usage"] = map[string]int{"prompt_tokens": task.InputTokens, "completion_tokens": task.OutputTokens, "total_tokens": task.InputTokens + task.OutputTokens}
	}
	out["billing"] = map[string]any{"mode": task.BillingMode, "billed_cost_usd": task.BilledCostUSD}
	result, _ := json.Marshal(out)
	return result
}

func (s *OpenAIGatewayService) ValidateVideoDownloadURL(raw, base string) error {
	_, _, err := s.minimaxVideoContentURL(raw, base)
	return err
}

// Keep URL parsing strict before an operator-provided API base is concatenated.
func validateSeedanceBaseURL(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid Seedance base URL")
	}
	return nil
}
