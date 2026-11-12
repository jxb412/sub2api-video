package handler

import (
	"context"
	"mime"
	"net/http"
	"strings"
	"time"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// SeedanceTasks exposes Ark's native asynchronous video task protocol.
func (h *OpenAIGatewayHandler) SeedanceTasks(c *gin.Context) {
	if c.Request.Method == http.MethodPost && c.GetHeader("Content-Type") != "" {
		mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || mediaType != "application/json" {
			h.errorResponse(c, http.StatusUnsupportedMediaType, "invalid_request_error", "Seedance requires application/json")
			return
		}
	}
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key.Group == nil || (key.Group.Platform != service.PlatformOpenAI && key.Group.Platform != service.PlatformComposite) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "Seedance requires an OpenAI or composite group")
		return
	}
	if c.Request.Method == http.MethodPost {
		h.asyncVideoGeneration(c, service.VideoProviderSeedance)
		return
	}
	if strings.HasPrefix(c.Param("task_id"), "sdv_") {
		h.seedanceDurableLookup(c)
		return
	}
	endpoint := service.SeedanceEndpointCreate
	setActualUpstreamEndpoint(c, EndpointSeedanceTasks)
	taskID := ""
	if c.Request.Method != http.MethodPost {
		taskID = service.SeedanceTaskKey(c.Param("task_id"))
		endpoint = service.SeedanceEndpointStatus
		if c.Request.Method == http.MethodDelete {
			endpoint = service.SeedanceEndpointDelete
		}
	}
	h.handleGrokMedia(c, endpoint, taskID)
}

func (h *OpenAIGatewayHandler) seedanceDurableLookup(c *gin.Context) {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid user")
		return
	}
	task, err := h.gatewayService.LoadMinimaxVideoTask(c.Request.Context(), c.Param("task_id"), subject.UserID, key.ID)
	if err != nil || task == nil || task.Provider != service.VideoProviderSeedance || key.GroupID == nil || task.GroupID != *key.GroupID {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Video task not found")
		return
	}
	if c.Request.Method == http.MethodDelete {
		if !task.IsTerminal() {
			h.errorResponse(c, http.StatusConflict, "task_not_terminal", "Generating tasks cannot be deleted; deletion does not cancel upstream generation")
			return
		}
		deleted, err := service.DeleteVideoTask(c.Request.Context(), task.ID, subject.UserID)
		if err != nil {
			h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to delete task")
			return
		}
		if !deleted {
			h.errorResponse(c, http.StatusConflict, "settlement_pending", "Task settlement is pending; retry deletion later")
			return
		}
		c.Status(http.StatusNoContent)
		return
	}
	contentPath := "/api/v3/contents/generations/tasks/" + task.ID + "/content"
	if strings.HasSuffix(c.Request.URL.Path, "/content") {
		if task.Status != service.MinimaxVideoStatusDone || task.VideoURL == "" {
			h.errorResponse(c, http.StatusConflict, "video_not_ready", "Video is not ready")
			return
		}
		base, token := h.gatewayService.MinimaxVideoAccountCredentials(c.Request.Context(), task.AccountID)
		if err := h.gatewayService.StreamMinimaxVideoContent(c, task, base, token); err != nil {
			h.writeVideoContentError(c, err)
		}
		return
	}
	c.Data(http.StatusOK, "application/json", service.SeedanceVideoStatusBody(*task, contentPath))
}

func (h *OpenAIGatewayHandler) SeedanceVideoContent(c *gin.Context) { h.seedanceDurableLookup(c) }

// Ark reports actual completion tokens. Never infer tokens from duration or use
// Grok's per-second video tariff. Repeated polls share the durable task dedup key.
func prepareSeedanceCompletionBilling(ctx context.Context, h *OpenAIGatewayHandler, key *service.APIKey, subject middleware.AuthSubject, taskID string, result *service.OpenAIForwardResult) *service.OpenAIForwardResult {
	if result == nil || result.Usage.OutputTokens <= 0 {
		return nil
	}
	pending, err := h.gatewayService.LoadGrokVideoPendingBilling(ctx, taskID, subject.UserID, key.ID)
	if err != nil || pending == nil {
		return nil
	}
	claimed, err := h.gatewayService.ClaimGrokVideoBilling(ctx, taskID, subject.UserID, key.ID)
	if err != nil || !claimed {
		return nil
	}
	merged := *result
	merged.Model = pending.Model
	merged.BillingModel = firstNonEmptyString(pending.BillingModel, pending.Model)
	merged.UpstreamModel = firstNonEmptyString(pending.UpstreamModel, result.UpstreamModel)
	merged.RequestID = service.StableGrokVideoBillingRequestID(taskID)
	merged.ResponseID = taskID
	merged.Duration = service.GrokVideoE2EDuration(pending.CreatedAt, time.Now())
	return &merged
}
