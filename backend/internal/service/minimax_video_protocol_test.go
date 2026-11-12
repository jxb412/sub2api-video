package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestMinimaxProtocolBindingSurvivesRestartAndRejectsAccountChanges(t *testing.T) {
	account := minimaxWorkerTestAccount()
	account.Credentials["minimax_video_protocol"] = "ll2"
	account.Credentials["base_url"] = "https://api.ll2.cn"
	svc := &OpenAIGatewayService{accountRepo: &minimaxWorkerAccountRepo{account: account}}
	task := minimaxBillableTask()
	task.DurationSeconds = 1
	require.NoError(t, svc.SnapshotMinimaxVideoProtocol(&task, account, []byte(`{"model":"MiniMax-H3","duration":1}`)))
	_, base, err := svc.minimaxBoundTaskAccount(context.Background(), &task)
	require.NoError(t, err)
	require.Equal(t, "https://api.ll2.cn", base)
	require.NotContains(t, task.UpstreamCredentialHash, "fresh-token")
	mock := minimaxTaskMockDB(t)
	expectMinimaxWorkerLoad(t, mock, task)
	restored, err := svc.LoadMinimaxVideoTask(context.Background(), task.ID, task.UserID, task.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, task.UpstreamRequestBody, restored.UpstreamRequestBody)
	require.Equal(t, task.UpstreamCredentialHash, restored.UpstreamCredentialHash)
	require.Equal(t, 1, restored.RequestedDurationSeconds)
	for _, field := range []string{"base_url", "api_key", "minimax_video_protocol"} {
		prior := account.Credentials[field]
		account.Credentials[field] = "changed"
		_, _, err = svc.minimaxBoundTaskAccount(context.Background(), restored)
		require.Error(t, err, field)
		account.Credentials[field] = prior
	}
	account.Credentials["header_overrides"] = map[string]any{"x-provider-project": "another-project"}
	_, _, err = svc.minimaxBoundTaskAccount(context.Background(), restored)
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLL2BillingRejectsUpstreamModeAndWaitsForFinalSeconds(t *testing.T) {
	svc := &OpenAIGatewayService{}
	task := minimaxBillableTask()
	task.UpstreamProtocol = "ll2"
	task.RequestedDurationSeconds = task.DurationSeconds
	key := &APIKey{Group: &Group{VideoBillingMode: VideoBillingModeUpstreamCost}}
	require.ErrorContains(t, svc.PrepareVideoBilling(context.Background(), &task, key), "LL2")
	task.BillingMode = VideoBillingModeLocalPrice
	_, err := svc.settleMinimaxVideoTask(context.Background(), task)
	require.ErrorContains(t, err, "final output seconds")
	require.True(t, videoTaskNeedsProviderMetadata(task))
	task.OutputSecondsReported = true
	require.False(t, videoTaskNeedsProviderMetadata(task))
	task.DurationSeconds++
	_, err = svc.settleMinimaxVideoTask(context.Background(), task)
	require.ErrorContains(t, err, "inconsistent")
}

func TestLL2AccountCostQuotaCannotSilentlyChargeZero(t *testing.T) {
	account := minimaxWorkerTestAccount()
	account.Credentials["minimax_video_protocol"] = "ll2"
	account.Extra = map[string]any{"quota_limit": 100.0}
	task := minimaxBillableTask()
	require.ErrorContains(t, (&OpenAIGatewayService{}).SnapshotMinimaxVideoProtocol(&task, account, []byte(`{}`)), "cost quotas")
	account.Credentials["minimax_video_protocol"] = "getoken"
	require.NoError(t, (&OpenAIGatewayService{}).SnapshotMinimaxVideoProtocol(&task, account, []byte(`{}`)))
}

func TestLL2TransportUsesStableIdempotencyAndExplicitParser(t *testing.T) {
	account := minimaxWorkerTestAccount()
	account.Credentials["header_overrides"] = map[string]any{"x-provider-project": "video-project", "idempotency-key": "must-not-override"}
	do := func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "Bearer supplied-key", req.Header.Get("Authorization"))
		require.Equal(t, "mmv_fixed-task", req.Header.Get("Idempotency-Key"))
		require.Equal(t, []string{"video-project"}, req.Header["x-provider-project"])
		require.NotContains(t, req.Header, "idempotency-key")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"task_id":"task_ll2"}`))}, nil
	}
	for range 2 {
		result, err := requestMinimaxVideoProtocol(context.Background(), http.MethodPost, "https://api.ll2.cn/v2/video_generation", "supplied-key", []byte(`{}`), account, "ll2", "mmv_fixed-task", do)
		require.NoError(t, err)
		require.Equal(t, "task_ll2", result.UpstreamID)
	}
}

func TestLL2PublicDownloadNeverSendsProviderAuthorization(t *testing.T) {
	origin, _ := url.Parse("https://api.ll2.cn")
	for _, target := range []string{"https://api.ll2.cn/video", "https://cdn.example/video"} {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer old-secret")
		setMinimaxVideoDownloadAuthorization(req, origin, "must-never-be-sent", true)
		require.Empty(t, req.Header.Get("Authorization"))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("LL2 must reject private/insecure downloads even when enabled for other providers")
	}))
	defer server.Close()
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Security.URLAllowlist.AllowPrivateHosts = true
	svc := &OpenAIGatewayService{cfg: cfg}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/video", nil)
	task := &MinimaxVideoTask{ID: "mmv_ll2", UpstreamProtocol: "ll2", UpstreamBaseURL: server.URL, VideoURL: server.URL + "/redirect"}
	require.Error(t, svc.StreamMinimaxVideoContent(c, task, "https://changed.invalid", "must-never-be-sent"))
	require.Empty(t, w.Body.String())
}
