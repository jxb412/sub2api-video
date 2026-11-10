package handler

import (
	"context"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// MiniMax H3 (and other blocking video providers) entry points.
//
// The upstream create call blocks for minutes, so the handler never waits for
// it: it validates the request, runs the same security audit as the Grok media
// path, picks an account with the minimax_video capability, stores the task and
// returns a gateway task id. The blocking upstream call runs in the background.
func (h *OpenAIGatewayHandler) MinimaxVideoGeneration(c *gin.Context) {
	requestStart := time.Now()
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(c, "handler.openai_gateway.minimax_video",
		zap.Int64("user_id", subject.UserID), zap.Int64("api_key_id", apiKey.ID))

	if mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type")); err != nil || mediaType != "application/json" {
		h.errorResponse(c, http.StatusUnsupportedMediaType, "invalid_request_error", "MiniMax video requires application/json")
		return
	}
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil || len(body) == 0 {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}
	info, err := service.ParseMinimaxVideoRequest(body)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if !service.GroupAllowsImageGeneration(apiKey.Group) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", service.ImageGenerationPermissionMessage())
		return
	}

	// Same moderation gate as the Grok media path: video prompts are audited
	// before any upstream submit happens.
	if moderationBody := info.ModerationBody(); len(moderationBody) > 0 {
		decision := h.checkSecurityAudit(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIImages, info.Model, moderationBody)
		if decision != nil && !decision.AllowNextStage {
			h.openAISecurityAuditError(c, decision)
			return
		}
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	if h.billingCacheService == nil || h.concurrencyHelper == nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Video admission is unavailable")
		return
	}
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.errorResponse(c, status, code, message)
		return
	}
	// Admission slots belong to the background task, not the short create HTTP request.
	var releases []func()
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			for _, fn := range releases {
				if fn != nil {
					fn()
				}
			}
		})
	}
	started := false
	defer func() {
		if !started {
			release()
		}
	}()
	userRelease, acquired, err := h.concurrencyHelper.TryAcquireUserSlot(c.Request.Context(), subject.UserID, subject.Concurrency)
	if err != nil || !acquired {
		h.errorResponse(c, http.StatusTooManyRequests, "rate_limit_error", "Video concurrency limit reached; retry later")
		return
	}
	releases = append(releases, userRelease)
	requestCtx := service.WithOpenAIProfitControlSuppressed(c.Request.Context())
	platform := service.PlatformMiniMax
	if apiKey.Group != nil {
		platform = apiKey.Group.Platform
		if target, ok := service.ResolvedTargetPlatformFromContext(requestCtx); ok {
			platform = target
		}
	}
	// Existing MiniMax groups may contain OpenAI API-key relay accounts.
	// Both pools require the explicit MiniMax capability and the same group binding.
	platforms := []string{platform}
	if platform == service.PlatformMiniMax {
		platforms = []string{service.PlatformOpenAI, service.PlatformMiniMax}
	}
	var selection *service.AccountSelectionResult
	for _, candidatePlatform := range platforms {
		selection, _, err = h.gatewayService.SelectAccountWithSchedulerForCapability(
			requestCtx, apiKey.GroupID, "", h.gatewayService.GenerateExplicitSessionHash(c, body), info.Model,
			nil, service.OpenAIUpstreamTransportHTTPSSE, service.OpenAIEndpointCapabilityMinimaxVideo,
			false, false, false, candidatePlatform,
		)
		if selection != nil && selection.Account != nil {
			break
		}
	}
	if selection != nil && selection.ReleaseFunc != nil {
		releases = append(releases, selection.ReleaseFunc)
	}
	if err != nil || selection == nil || selection.Account == nil {
		reqLog.Info("minimax_video.no_eligible_account", zap.Error(err))
		h.errorResponse(c, http.StatusServiceUnavailable, "minimax_video_no_eligible_account", "No eligible MiniMax video accounts in this group")
		return
	}
	account := selection.Account
	if !selection.Acquired {
		if selection.WaitPlan == nil {
			h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "No available video account slots")
			return
		}
		accountRelease, acquired, err := h.concurrencyHelper.TryAcquireAccountSlot(requestCtx, account.ID, selection.WaitPlan.MaxConcurrency)
		if err != nil || !acquired {
			h.errorResponse(c, http.StatusTooManyRequests, "rate_limit_error", "Video account concurrency limit reached; retry later")
			return
		}
		releases = append(releases, accountRelease)
	}
	upstreamBody, err := service.BuildMinimaxVideoUpstreamBody(info, account.GetMappedModel(info.Model))
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Invalid video request")
		return
	}
	targetURL, err := h.gatewayService.MinimaxVideoRequestURL(account)
	if err != nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Video upstream URL is not configured correctly")
		return
	}
	task := service.MinimaxVideoTask{
		ID:              service.MinimaxVideoTaskID(),
		Status:          service.MinimaxVideoStatusQueued,
		Model:           info.Model,
		UpstreamModel:   account.GetMappedModel(info.Model),
		Prompt:          info.Prompt,
		DurationSeconds: info.DurationSeconds,
		Resolution:      info.Resolution,
		UserID:          subject.UserID,
		APIKeyID:        apiKey.ID,
		AccountID:       account.ID,
		CreatedAt:       service.GrokVideoPendingCreatedAtNow(),
	}
	if apiKey.GroupID != nil {
		task.GroupID = *apiKey.GroupID
	}

	// 计费快照：把分组的价格与倍率固定在任务上。
	task.BillingMode = service.VideoBillingModeDefault
	task.RateMultiplier = h.gatewayService.ResolveMinimaxVideoRateMultiplier(requestCtx, apiKey)
	if apiKey.Group != nil {
		settings := service.VideoBillingSettingsFor(apiKey.Group)
		task.BillingMode = settings.Mode
		if price := apiKey.Group.GetVideoPriceForModel(info.Model, service.ResolveVideoBillingTier(info.Resolution, settings.Tiers)); price != nil {
			task.PricePerSecond = *price
			task.PriceConfigured = true
		}
	}
	service.SnapshotMinimaxVideoBilling(&task, apiKey, account, subscription)
	task.BillingSnapshot.Platform = service.QuotaPlatform(requestCtx, apiKey)
	// Keep inflight reservation alive until settlement, even after the create response closes.
	workerCtx, cancelWorker := context.WithTimeout(context.WithoutCancel(requestCtx), 22*time.Minute)
	releases = append(releases, cancelWorker)
	workerCtx, inflightDone, err := reserveMinimaxVideoCharge(workerCtx, h.billingCacheService, h.gatewayService, apiKey, subscription, task, service.InflightEstimateRequest{
		Model: info.Model, Kind: service.InflightEstimateVideo, Units: 1,
		VideoResolution: info.Resolution, VideoDurationSeconds: info.DurationSeconds,
	})
	if err != nil {
		status, code, message, _ := billingErrorDetails(err)
		h.errorResponse(c, status, code, message)
		return
	}
	releases = append(releases, inflightDone)
	if err := h.gatewayService.StoreMinimaxVideoTask(requestCtx, task); err != nil {
		reqLog.Warn("minimax_video.task_store_failed", zap.Error(err))
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to create video task")
		return
	}

	// Bind the task to the chosen account so status/content polls resolve even
	// after a failover or restart.
	if err := h.gatewayService.BindMinimaxVideoTaskAccount(requestCtx, apiKey.GroupID, task.ID, subject.UserID, apiKey.ID, account.ID); err != nil {
		reqLog.Warn("minimax_video.task_bind_failed", zap.Error(err))
	}

	token := strings.TrimSpace(account.GetCredential("api_key"))
	started = true
	go func() {
		defer release()
		h.gatewayService.RunMinimaxVideoTask(workerCtx, task.ID, task.UserID, task.APIKeyID, targetURL, token, upstreamBody)
		if h.apiKeyService != nil {
			h.apiKeyService.InvalidateAuthCacheByKey(context.Background(), apiKey.Key)
		}
	}()

	c.JSON(http.StatusOK, gin.H{
		"id":         task.ID,
		"request_id": task.ID,
		"status":     service.MinimaxVideoStatusQueued,
		"model":      task.Model,
		"created_at": task.CreatedAt,
	})
	reqLog.Info("minimax_video.task_queued",
		zap.String("task_id", task.ID),
		zap.Int64("account_id", account.ID),
		zap.Duration("accept_latency", time.Since(requestStart)))
}

// Known video charges must fit all payer limits before starting billable work.
// The reservation holds headroom only; the completion ledger performs the debit.
func reserveMinimaxVideoCharge(ctx context.Context, billing *service.BillingCacheService, estimator inflightReservationEstimator, key *service.APIKey, subscription *service.UserSubscription, task service.MinimaxVideoTask, req service.InflightEstimateRequest) (context.Context, func(), error) {
	if task.PriceConfigured && !service.VideoBillingModeUsesUpstreamCost(task.BillingMode) {
		settlement := service.ComputeMinimaxVideoSettlement(task, task.BillingMode, nil, task.RateMultiplier)
		reservation, err := billing.ReserveFixedCharge(ctx, key.User, key, key.Group, subscription, settlement.BilledCost)
		if err != nil {
			return ctx, inflightNoop, err
		}
		return service.WithInflightReservation(ctx, reservation), reservation.HandlerDone, nil
	}
	return reserveInflightBalanceCtx(ctx, billing, estimator, key, subscription, req)
}

// MinimaxVideoStatus returns the gateway-owned task state.
func (h *OpenAIGatewayHandler) MinimaxVideoStatus(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	taskID := strings.TrimSpace(c.Param("request_id"))
	if taskID == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "request_id is required")
		return
	}
	task, err := h.gatewayService.LoadMinimaxVideoTask(c.Request.Context(), taskID, subject.UserID, apiKey.ID)
	if err != nil || task == nil {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Video request not found")
		return
	}
	contentPath := ""
	if task.Status == service.MinimaxVideoStatusDone {
		contentPath = "/v1/videos/" + task.ID + "/content"
	}
	c.Data(http.StatusOK, "application/json", service.MinimaxVideoTaskStatusBody(*task, contentPath))
}

// MinimaxVideoContent streams the finished video bytes.
func (h *OpenAIGatewayHandler) MinimaxVideoContent(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	taskID := strings.TrimSpace(c.Param("request_id"))
	task, err := h.gatewayService.LoadMinimaxVideoTask(c.Request.Context(), taskID, subject.UserID, apiKey.ID)
	if err != nil || task == nil {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Video request not found")
		return
	}
	if task.Status != service.MinimaxVideoStatusDone || strings.TrimSpace(task.VideoURL) == "" {
		h.errorResponse(c, http.StatusConflict, "invalid_request_error", "Video is not ready")
		return
	}
	baseURL, token := h.gatewayService.MinimaxVideoAccountCredentials(c.Request.Context(), task.AccountID)
	if err := h.gatewayService.StreamMinimaxVideoContent(c, task, baseURL, token); err != nil {
		if !c.Writer.Written() {
			h.errorResponse(c, http.StatusBadGateway, "upstream_error", "Failed to fetch video content")
		}
	}
}
