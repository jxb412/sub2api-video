package handler

import (
	"context"
	"net/http"
	"strings"
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

	requestCtx := service.WithOpenAIProfitControlSuppressed(c.Request.Context())
	selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
		requestCtx,
		apiKey.GroupID,
		"",
		h.gatewayService.GenerateExplicitSessionHash(c, body),
		info.Model,
		nil,
		service.OpenAIUpstreamTransportHTTPSSE,
		service.OpenAIEndpointCapabilityMinimaxVideo,
		false, false, false,
	)
	if err != nil || selection == nil || selection.Account == nil {
		reqLog.Info("minimax_video.no_eligible_account", zap.Error(err))
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
			"type":    "api_error",
			"code":    "minimax_video_no_eligible_account",
			"message": "No eligible MiniMax video accounts in this group",
		}})
		return
	}
	account := selection.Account

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

	upstreamBody, err := service.BuildMinimaxVideoUpstreamBody(info, task.UpstreamModel)
	if err != nil {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to build upstream request")
		return
	}
	targetURL, err := service.BuildMinimaxVideoURL(service.MinimaxVideoBaseURL(account), service.MinimaxVideoEndpointCreate, "")
	if err != nil {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to build upstream URL")
		return
	}
	token := strings.TrimSpace(account.GetCredential("api_key"))
	go h.gatewayService.RunMinimaxVideoTask(context.WithoutCancel(requestCtx), task.ID, task.UserID, task.APIKeyID, targetURL, token, upstreamBody)

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
	if err := h.gatewayService.StreamMinimaxVideoContent(c, task); err != nil {
		h.errorResponse(c, http.StatusBadGateway, "upstream_error", "Failed to fetch video content")
	}
}
