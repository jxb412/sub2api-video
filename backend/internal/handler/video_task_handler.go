package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 用户端视频任务接口。
//
// 设计要点：
//  1. 全部按 user_id 收敛，任务 id 猜不到别人的数据（store 层也会再校验一次）。
//  2. 列表与统计共享同一套筛选参数，前端两个组件传同样的 query 即可。
//  3. 进行中的任务没有费用，返回 0；失败任务 billed_cost_usd 为 0（全额退还）。

func minimaxVideoFilterFromQuery(c *gin.Context, userID int64) service.VideoTaskFilter {
	f := service.VideoTaskFilter{
		UserID:   userID,
		Status:   strings.TrimSpace(c.Query("status")),
		Model:    strings.TrimSpace(c.Query("model")),
		Search:   strings.TrimSpace(c.Query("search")),
		Page:     atoiDefault(c.Query("page"), 1),
		PageSize: atoiDefault(c.Query("page_size"), 20),
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
	if gid := atoiDefault(c.Query("group_id"), 0); gid > 0 {
		f.GroupID = int64(gid)
	}
	return f
}

func atoiDefault(raw string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
		return v
	}
	return def
}

// ListVideoTasks 列出当前用户的视频任务。
func (h *OpenAIGatewayHandler) ListVideoTasks(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	f := minimaxVideoFilterFromQuery(c, subject.UserID)
	items, total, err := service.ListVideoTasks(c.Request.Context(), f)
	if err != nil {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to load video tasks")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"items":     items,
		"total":     total,
		"page":      f.Page,
		"page_size": f.PageSize,
	}})
}

// GetVideoTask 返回单个任务详情。
func (h *OpenAIGatewayHandler) GetVideoTask(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	rec, err := service.GetVideoTask(c.Request.Context(), strings.TrimSpace(c.Param("id")), subject.UserID)
	if err != nil {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to load video task")
		return
	}
	if rec == nil {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Video task not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rec})
}

// DeleteVideoTask 软删除任务；进行中的任务不允许删除。
func (h *OpenAIGatewayHandler) DeleteVideoTask(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	taskID := strings.TrimSpace(c.Param("id"))
	rec, err := service.GetVideoTask(c.Request.Context(), taskID, subject.UserID)
	if err == nil && rec != nil && (rec.Status == service.MinimaxVideoStatusQueued || rec.Status == service.MinimaxVideoStatusProcessing) {
		h.errorResponse(c, http.StatusConflict, "invalid_request_error", "Generating tasks cannot be deleted")
		return
	}
	deleted, err := service.DeleteVideoTask(c.Request.Context(), taskID, subject.UserID)
	if err != nil {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to delete video task")
		return
	}
	if !deleted {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Video task not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

// VideoTaskStats 概览卡数据。
func (h *OpenAIGatewayHandler) VideoTaskStats(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	f := minimaxVideoFilterFromQuery(c, subject.UserID)
	f.Status, f.Model, f.Search, f.Page, f.PageSize = "", "", "", 0, 0
	stats, err := service.VideoTaskStatsFor(c.Request.Context(), f)
	if err != nil {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to load video stats")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": stats})
}

// DownloadVideoTask 供面板下载视频：JWT 鉴权 + 归属校验后回源取流。
// 网关侧的 /v1/videos/{id}/content 走 API Key 鉴权，这里补一条面板通道。
func (h *OpenAIGatewayHandler) DownloadVideoTask(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	taskID := strings.TrimSpace(c.Param("id"))
	rec, err := service.GetVideoTask(c.Request.Context(), taskID, subject.UserID)
	if err != nil || rec == nil {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Video task not found")
		return
	}
	if rec.Status != service.MinimaxVideoStatusDone || strings.TrimSpace(rec.VideoURL) == "" {
		h.errorResponse(c, http.StatusConflict, "invalid_request_error", "Video is not ready")
		return
	}
	baseURL, token := h.gatewayService.MinimaxVideoAccountCredentials(c.Request.Context(), rec.AccountID)
	task := &service.MinimaxVideoTask{
		ID:         rec.TaskID,
		Status:     rec.Status,
		VideoURL:   rec.VideoURL,
		AccountID:  rec.AccountID,
		Resolution: rec.Resolution,
	}
	if err := h.gatewayService.StreamMinimaxVideoContent(c, task, baseURL, token); err != nil {
		h.errorResponse(c, http.StatusBadGateway, "upstream_error", "Failed to fetch video content")
	}
}
