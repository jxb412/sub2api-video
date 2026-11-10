package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 管理端视频任务查询与管理。
//
// 比用户端多三个维度：用户、分组、账号，并支持「包含已删除」用于审计。
// 统计口径额外返回毛利，便于双计费模式的经营分析。

func adminVideoFilterFromQuery(c *gin.Context) service.VideoTaskFilter {
	f := service.VideoTaskFilter{
		Status:         strings.TrimSpace(c.Query("status")),
		Model:          strings.TrimSpace(c.Query("model")),
		Provider:       strings.TrimSpace(c.Query("provider")),
		BillingOnly:    c.Query("billing_only") == "true",
		Search:         strings.TrimSpace(c.Query("search")),
		Page:           queryInt(c.Query("page"), 1),
		PageSize:       queryInt(c.Query("page_size"), 20),
		IncludeDeleted: c.Query("include_deleted") == "1" || c.Query("include_deleted") == "true",
	}
	if v := queryInt(c.Query("user_id"), 0); v > 0 {
		f.UserID = int64(v)
	}
	if v := queryInt(c.Query("group_id"), 0); v > 0 {
		f.GroupID = int64(v)
	}
	if v := queryInt(c.Query("account_id"), 0); v > 0 {
		f.AccountID = int64(v)
	}
	if raw := strings.TrimSpace(c.Query("from")); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			f.From = &t
		}
	}
	if raw := strings.TrimSpace(c.Query("to")); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			f.To = &t
		}
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 {
		f.PageSize = 20
	}
	if f.PageSize > 100 {
		f.PageSize = 100
	}
	return f
}

func queryInt(raw string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
		return v
	}
	return def
}

// ListVideoTasks 全量视频任务列表。
func (h *OpenAIGatewayHandler) AdminListVideoTasks(c *gin.Context) {
	f := adminVideoFilterFromQuery(c)
	items, total, err := service.ListVideoTasks(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load video tasks"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": gin.H{
		"items":     items,
		"total":     total,
		"page":      f.Page,
		"page_size": f.PageSize,
	}})
}

// VideoTaskStats 统计 + 毛利。
func (h *OpenAIGatewayHandler) AdminVideoTaskStats(c *gin.Context) {
	f := adminVideoFilterFromQuery(c)
	f.Page, f.PageSize = 0, 0
	stats, err := service.VideoTaskStatsFor(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load stats"})
		return
	}
	margin := 0.0
	if stats.BilledCost > 0 {
		margin = (stats.BilledCost - stats.UpstreamCost) / stats.BilledCost * 100
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": gin.H{
		"stats":          stats,
		"margin_percent": margin,
	}})
}

// GetVideoTask 任务详情（管理端可看已删除）。
func (h *OpenAIGatewayHandler) AdminGetVideoTask(c *gin.Context) {
	rec, err := service.GetVideoTask(c.Request.Context(), strings.TrimSpace(c.Param("id")), 0)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load task"})
		return
	}
	if rec == nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "video task not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": rec})
}

// DeleteVideoTask 软删除任意任务。
func (h *OpenAIGatewayHandler) AdminDeleteVideoTask(c *gin.Context) {
	taskID := strings.TrimSpace(c.Param("id"))
	rec, err := service.GetVideoTask(c.Request.Context(), taskID, 0)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to load task"})
		return
	}
	if rec != nil && (rec.Status == service.MinimaxVideoStatusQueued || rec.Status == service.MinimaxVideoStatusProcessing ||
		(rec.Status == service.MinimaxVideoStatusDone && rec.BillingStatus == "pending")) {
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "task generation or settlement is pending"})
		return
	}
	deleted, err := service.DeleteVideoTask(c.Request.Context(), taskID, 0)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to delete task"})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "video task not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": gin.H{"deleted": true}})
}

// FailVideoTask marks only unfinished tasks as failed; it never rewrites settled charges.
func (h *OpenAIGatewayHandler) FailVideoTask(c *gin.Context) {
	taskID := strings.TrimSpace(c.Param("id"))
	var input struct {
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid request body"})
		return
	}
	reason := strings.TrimSpace(input.Reason)
	if reason == "" {
		reason = "Marked failed by administrator"
	}
	if len(reason) > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "reason is too long"})
		return
	}
	updated, err := service.FailPendingVideoTask(c.Request.Context(), taskID, reason)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to update task"})
		return
	}
	if !updated {
		c.JSON(http.StatusConflict, gin.H{"code": 409, "message": "task is missing or already completed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success", "data": gin.H{"updated": true}})
}
