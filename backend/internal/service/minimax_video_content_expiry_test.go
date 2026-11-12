//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestVideoExpiryStatusQueryNeverFollowsRedirect(t *testing.T) {
	redirected := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		t.Error("Provider status redirect must not be followed")
	}))
	defer destination.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer provider.Close()
	task := &MinimaxVideoTask{UpstreamID: "upstream-1"}
	require.False(t, minimaxVideoContentExpired(context.Background(), provider.Client(), task, provider.URL+"/v1", "secret"))
	require.False(t, redirected)
}

func TestCompletedVideoDownloadDistinguishesExpiredContent(t *testing.T) {
	for _, tc := range []struct {
		name, queryBody string
		contentStatus   int
		expired         bool
		queryCalls      int
	}{
		{"expired_getoken", `{"request_id":"upstream-1","status":"expired","video":null}`, 409, true, 1},
		{"temporary_not_ready", `{"request_id":"upstream-1","status":"processing"}`, 409, false, 1},
		{"different_task", `{"request_id":"other","status":"expired"}`, 404, false, 1},
		{"invalid_response", `<html>unavailable</html>`, 403, false, 1},
		{"explicit_gone", ``, 410, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queries := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
				if r.URL.Path == "/v1/videos/upstream-1" {
					queries++
					_, _ = w.Write([]byte(tc.queryBody))
					return
				}
				require.Equal(t, "/v1/videos/upstream-1/content", r.URL.Path)
				w.WriteHeader(tc.contentStatus)
			}))
			defer server.Close()
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Security.URLAllowlist.AllowPrivateHosts = true
			svc := &OpenAIGatewayService{cfg: cfg}
			task := &MinimaxVideoTask{ID: "mmv_local", UpstreamID: "upstream-1", Status: MinimaxVideoStatusDone, VideoURL: "/v1/videos/upstream-1/content", BilledCostUSD: .06}
			before := *task
			writer := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(writer)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/content", nil)
			err := svc.StreamMinimaxVideoContent(ctx, task, server.URL+"/v1", "secret")
			if tc.expired {
				require.ErrorIs(t, err, ErrVideoContentExpired)
			} else {
				require.ErrorIs(t, err, ErrVideoContentUnavailable)
			}
			require.Equal(t, tc.queryCalls, queries)
			require.Equal(t, before, *task, "A missing download must not alter successful generation or billing")
			require.False(t, ctx.Writer.Written())
		})
	}
}
