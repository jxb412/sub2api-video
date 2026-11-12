//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"encoding/json"
	"github.com/DATA-DOG/go-sqlmock"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"time"
)

func TestSeedanceLegacyHandlerLifecycleAndOwnership(t *testing.T) {
	h, slots, bindings, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	var owner int64
	upstream.call = func(req *http.Request, id int64) (*http.Response, error) {
		body := `{"id":"task-ark","status":"queued"}`
		if req.Method == http.MethodPost {
			owner = id
		} else {
			require.Equal(t, owner, id)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	newContext := func(method string) (*gin.Context, *httptest.ResponseRecorder) {
		c, w := grokMediaSlotContext(context.Background(), method == http.MethodPost)
		key, _ := middleware.GetAPIKeyFromContext(c)
		key.Group.Platform = service.PlatformOpenAI
		body := ""
		if method == http.MethodPost {
			body = `{"model":"doubao-seedance","content":[{"type":"text","text":"waves"}]}`
		}
		c.Request = httptest.NewRequest(method, "/api/v3/contents/generations/tasks", strings.NewReader(body))
		c.Params = gin.Params{{Key: "task_id", Value: "task-ark"}}
		return c, w
	}
	c, w := newContext(http.MethodPost)
	h.handleGrokMedia(c, service.SeedanceEndpointCreate, "")
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Positive(t, owner)
	require.Len(t, bindings.pending, 1)
	slots.assertReleased(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		c, w = newContext(method)
		h.SeedanceTasks(c)
		require.Equal(t, 200, w.Code, w.Body.String())
		slots.assertReleased(t)
	}
	for _, other := range []string{"user", "key", "group", "task", "provider"} {
		c, w = newContext(http.MethodGet)
		key, _ := middleware.GetAPIKeyFromContext(c)
		switch other {
		case "user":
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 11, Concurrency: 5})
		case "key":
			key.ID = 21
		case "group":
			group := int64(25)
			key.GroupID = &group
		case "task":
			c.Params = gin.Params{{Key: "task_id", Value: "other"}}
		case "provider":
			c.Params = gin.Params{{Key: "request_id", Value: "task-ark"}}
		}
		before := upstream.calls
		if other == "provider" {
			h.GrokVideoStatus(c)
		} else {
			h.SeedanceTasks(c)
		}
		require.Equal(t, 404, w.Code, other+": "+w.Body.String())
		require.Equal(t, before, upstream.calls)
		slots.assertReleased(t)
	}
	c, _ = newContext(http.MethodGet)
	key, _ := middleware.GetAPIKeyFromContext(c)
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	result := &service.OpenAIForwardResult{Usage: service.OpenAIUsage{OutputTokens: 12345}, ResponseID: "seedance:task-ark"}
	for i := range 20 {
		billed := prepareSeedanceCompletionBilling(context.Background(), h, key, subject, result.ResponseID, result)
		if i == 0 {
			require.NotNil(t, billed)
			require.Equal(t, "doubao-seedance", billed.BillingModel)
			require.Equal(t, 12345, billed.Usage.OutputTokens)
			require.Zero(t, billed.VideoCount)
		} else {
			require.Nil(t, billed)
		}
	}
	require.Len(t, bindings.billed, 1)
}

func TestSeedanceDurableHandlerOwnershipAndProtectedStatus(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	service.SetVideoTaskDB(db)
	t.Cleanup(func() { service.SetVideoTaskDB(nil); db.Close() })
	h, _, _, _ := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		c, w := grokMediaSlotContext(context.Background(), false)
		key, _ := middleware.GetAPIKeyFromContext(c)
		key.Group.Platform = service.PlatformOpenAI
		c.Request = httptest.NewRequest(http.MethodGet, "/api/v3/contents/generations/tasks/sdv_owned", nil)
		c.Params = gin.Params{{Key: "task_id", Value: "sdv_owned"}}
		return c, w
	}
	c, w := newContext()
	key, _ := middleware.GetAPIKeyFromContext(c)
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	task := service.MinimaxVideoTask{ID: "sdv_owned", Provider: "seedance", Status: "done", Model: "seedance-test", UpstreamID: "up", UsageReported: true, OutputTokens: 12000, ProviderResponse: json.RawMessage(`{"id":"up","status":"succeeded","content":{"video_url":"https://signed.example/secret"}}`)}
	payload, _ := json.Marshal(task)
	columns := strings.Split("task_snapshot,task_id,upstream_id,status,model,upstream_model,prompt,resolution,duration_seconds,user_id,api_key_id,group_id,account_id,video_url,error_message,upstream_cost_usd,billed_cost_usd,billing_mode,created_at,completed_at", ",")
	mock.ExpectQuery("SELECT task_snapshot").WithArgs("sdv_owned", subject.UserID, key.ID).WillReturnRows(sqlmock.NewRows(columns).AddRow(string(payload), task.ID, "up", "done", task.Model, task.Model, "waves", "720p", 5, subject.UserID, key.ID, *key.GroupID, 1, "https://signed.example/secret", "", 0, 0.12, "usage_tokens", time.Now(), time.Now()))
	h.SeedanceTasks(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "/api/v3/contents/generations/tasks/sdv_owned/content")
	require.NotContains(t, w.Body.String(), "signed.example")
	c, w = newContext()
	otherKey, _ := middleware.GetAPIKeyFromContext(c)
	otherKey.ID++
	mock.ExpectQuery("SELECT task_snapshot").WithArgs("sdv_owned", subject.UserID, otherKey.ID).WillReturnRows(sqlmock.NewRows(columns))
	h.SeedanceTasks(c)
	require.Equal(t, 404, w.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVideoProviderAndBillingFilters(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/videos?provider=seedance&billing_only=true&status=done", nil)
	for _, filter := range []service.VideoTaskFilter{minimaxVideoFilterFromQuery(c, 1), adminVideoFilterFromQuery(c)} {
		require.Equal(t, "seedance", filter.Provider)
		require.True(t, filter.BillingOnly)
		require.Equal(t, "done", filter.Status)
	}
}
