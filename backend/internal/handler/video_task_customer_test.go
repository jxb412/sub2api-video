//go:build unit

package handler

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func assertCustomerVideoPrivacy(t *testing.T, body string) {
	t.Helper()
	for _, field := range []string{"upstream_cost_usd", "upstream_cost_reported", "unknown_upstream_cost_count", "procurement", "account_id", "account_name", "upstream_id", "upstream_model", "user_email"} {
		require.NotContains(t, body, `"`+field+`"`, field)
	}
	require.NotContains(t, body, "supplier-private")
}

func TestCustomerVideoHandlersExcludeProcurementAndRoutingFields(t *testing.T) {
	for _, endpoint := range []string{"list", "detail", "stats"} {
		t.Run(endpoint, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			service.SetVideoTaskDB(db)
			t.Cleanup(func() { service.SetVideoTaskDB(nil); _ = db.Close() })
			c, w := grokMediaSlotContext(context.Background(), false)
			subject, _ := middleware.GetAuthSubjectFromContext(c)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/videos", nil)
			c.Params = gin.Params{{Key: "id", Value: "mmv_owned"}}
			h := &OpenAIGatewayHandler{}
			columns := strings.Split("task_id,upstream_id,provider,model,upstream_model,prompt,status,resolution,duration_seconds,user_id,api_key_id,group_id,account_id,video_url,error_message,upstream_cost_usd,billed_cost_usd,billing_mode,created_at,completed_at,deleted_at,settled_at,input_tokens,output_tokens,billing_status,upstream_cost_reported", ",")
			now := time.Now()
			values := []driver.Value{"mmv_owned", "supplier-private-task", "minimax", "MiniMax-H3", "supplier-private-model", "waves", "done", "768P", 1, subject.UserID, 15, 9, 125, "https://supplier-private/video.mp4", "", 0.05, 0.06, "local_price", now, now, nil, now, 0, 0, "settled", true}
			switch endpoint {
			case "list":
				mock.ExpectQuery("SELECT count\\(\\*\\) FROM video_tasks t").WithArgs(subject.UserID).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
				columns = append(columns, "user_email", "group_name", "account_name", "api_key_name")
				values = append(values, "supplier-private-email", "Videos", "supplier-private-account", "My API key")
				mock.ExpectQuery("SELECT t.task_id").WithArgs(subject.UserID, 20, 0).WillReturnRows(sqlmock.NewRows(columns).AddRow(values...))
				h.ListVideoTasks(c)
			case "detail":
				mock.ExpectQuery("SELECT task_id").WithArgs("mmv_owned", subject.UserID).WillReturnRows(sqlmock.NewRows(columns).AddRow(values...))
				h.GetVideoTask(c)
			case "stats":
				mock.ExpectQuery("SELECT").WithArgs(subject.UserID).WillReturnRows(sqlmock.NewRows([]string{"total", "queued", "processing", "done", "error", "today", "cost", "billed", "unknown"}).AddRow(1, 0, 0, 1, 0, 1, 0.05, 0.06, 2))
				h.VideoTaskStats(c)
			}
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assertCustomerVideoPrivacy(t, w.Body.String())
			require.Contains(t, w.Body.String(), `"billed_cost_usd":0.06`)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestAdminVideoResponsesRetainProcurement(t *testing.T) {
	actual := 0.05
	record := service.AdminVideoTaskRecord{VideoTaskRecord: service.VideoTaskRecord{
		TaskID: "mmv_owned", UpstreamID: "supplier-task", AccountID: 125, UpstreamCostUSD: actual, UpstreamCostReported: true,
	}, Procurement: service.VideoTaskProcurement{ActualCost: &actual}}
	body, err := json.Marshal(record)
	require.NoError(t, err)
	require.Contains(t, string(body), `"upstream_cost_usd":0.05`)
	require.Contains(t, string(body), `"procurement"`)
	require.Contains(t, string(body), `"actual_cost":0.05`)
}

func TestVideoDownloadErrorsAreSpecificAndDoNotExposeProviderDetails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"expired", fmt.Errorf("private URL: %w", service.ErrVideoContentExpired), http.StatusGone, "video_content_expired"},
		{"unavailable", fmt.Errorf("private URL: %w", service.ErrVideoContentUnavailable), http.StatusBadGateway, "video_content_unavailable"},
		{"unknown", fmt.Errorf("private URL and credential"), http.StatusBadGateway, "upstream_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			(&OpenAIGatewayHandler{}).writeVideoContentError(c, tc.err)
			require.Equal(t, tc.status, w.Code)
			require.Contains(t, w.Body.String(), `"type":"`+tc.code+`"`)
			require.NotContains(t, w.Body.String(), "private")
		})
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Data(http.StatusOK, "video/mp4", []byte("partial-video"))
	(&OpenAIGatewayHandler{}).writeVideoContentError(c, service.ErrVideoContentExpired)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "partial-video", w.Body.String())
}
