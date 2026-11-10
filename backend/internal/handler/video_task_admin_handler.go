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
		Status:  strings.TrimSpace(c.Query("status")),
		Model:   strings.TrimSpace(c.Query("model")),
		Search:  strings.TrimSpace(c.Query("search")),
		Page:    queryInt(c.Query("page"), 1),
		PageSize: queryInt(c.Query("page_size"), 20),
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
	return f
}

func queryInt(raw string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
		return v
	}
	return def
}

// ListVideoTasks 全量视频任务列表。
func (h *OpenAIGatewayHandler) ListVideoTasks(c *gin.Context) {
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
func (h *OpenAIGatewayHandler) VideoTaskStats(c *gin.Context) {
	f := adminVideoFilterFromQuery(c)
	f.Status, f.Model, f.Search, f.Page, f.PageSize = "", "", "", 0, 0
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
		"stats":         stats,
		"margin_percent": margin,
	}})
}

// GetVideoTask 任务详情（管理端可看已删除）。
func (h *OpenAIGatewayHandler) GetVideoTask(c *gin.Context) {
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
func (h *OpenAIGatewayHandler) DeleteVideoTask(c *gin.Context) {
	deleted, err := service.DeleteVideoTask(c.Request.Context(), strings.TrimSpace(c.Param("id")), 0)
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

// FailVideoTask 把卡住的任务标记为失败并清零计费（触发退款语义）。
func (h *OpenAIGatewayHandler) FailVideoTask(c *gin.Context) {
	taskID := strings.TrimSpace(c.Param("id"))
	reason := strings.TrimSpace(c.DefaultPostForm("reason", "标记失败（管理端操作）"))
	if err := service.CompleteVideoTask(c.Request.Context(), taskID, "", service.MinimaxVideoStatusError,
		"", "", reason, 0, 0, 0, ""); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "failed to update task"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success"})
}
