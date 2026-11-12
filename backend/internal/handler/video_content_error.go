package handler

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *OpenAIGatewayHandler) writeVideoContentError(c *gin.Context, err error) {
	if c.Writer.Written() {
		return
	}
	switch {
	case errors.Is(err, service.ErrVideoContentExpired):
		h.errorResponse(c, http.StatusGone, "video_content_expired", "Video file has expired and is no longer available. Completed generation and billing records are retained.")
	case errors.Is(err, service.ErrVideoContentUnavailable):
		h.errorResponse(c, http.StatusBadGateway, "video_content_unavailable", "Video file is currently unavailable. Please retry later or contact support.")
	default:
		h.errorResponse(c, http.StatusBadGateway, "upstream_error", "Failed to fetch video content")
	}
}
