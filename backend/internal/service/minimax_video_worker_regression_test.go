package service

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

type minimaxWorkerAccountRepo struct {
	AccountRepository
	account *Account
	ids     []int64
}

func (r *minimaxWorkerAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.ids = append(r.ids, id)
	return r.account, nil
}

type minimaxWorkerHTTP struct {
	HTTPUpstream
	status      int
	body        string
	requests    []*http.Request
	proxy       string
	accountID   int64
	concurrency int
}

func (h *minimaxWorkerHTTP) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	h.requests = append(h.requests, req)
	h.proxy, h.accountID, h.concurrency = proxy, id, concurrency
	return &http.Response{StatusCode: h.status, Body: io.NopCloser(strings.NewReader(h.body)), Header: make(http.Header)}, nil
}

type minimaxWorkerSnapshotArg struct{ task *MinimaxVideoTask }

func (a minimaxWorkerSnapshotArg) Match(value driver.Value) bool {
	s, ok := value.(string)
	return ok && json.Unmarshal([]byte(s), a.task) == nil
}

type minimaxPollLedgerProbe struct{ UsageBillingRepository }

func (*minimaxPollLedgerProbe) Apply(ctx context.Context, _ *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	var available int
	if err := videoTaskHandle().QueryRowContext(ctx, "SELECT 1").Scan(&available); err != nil {
		return nil, err
	}
	return &UsageBillingApplyResult{Applied: true}, nil
}

func minimaxWorkerTestAccount() *Account {
	return &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 7,
		Proxy: &Proxy{Protocol: "http", Host: "proxy.example", Port: 3128},
		Credentials: map[string]any{"base_url": "https://provider.example", "api_key": "fresh-token",
			"header_override_enabled": true, "header_overrides": map[string]any{"x-provider-project": "video-project"}}}
}

func expectMinimaxWorkerLoad(t *testing.T, mock sqlmock.Sqlmock, task MinimaxVideoTask) {
	t.Helper()
	payload, err := json.Marshal(task)
	require.NoError(t, err)
	created, err := time.Parse(time.RFC3339Nano, task.CreatedAt)
	require.NoError(t, err)
	var completed any
	if task.IsTerminal() {
		completed, err = time.Parse(time.RFC3339Nano, task.CompletedAt)
		require.NoError(t, err)
	}
	columns := strings.Split("task_snapshot,task_id,upstream_id,status,model,upstream_model,prompt,resolution,duration_seconds,user_id,api_key_id,group_id,account_id,video_url,error_message,upstream_cost_usd,billed_cost_usd,billing_mode,created_at,completed_at", ",")
	mock.ExpectQuery("SELECT task_snapshot.*FROM video_tasks WHERE task_id=\\$1 AND user_id=\\$2 AND api_key_id=\\$3 AND deleted_at IS NULL").
		WithArgs(task.ID, task.UserID, task.APIKeyID).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(string(payload), task.ID, task.UpstreamID, task.Status, task.Model, task.UpstreamModel, task.Prompt, task.Resolution, task.DurationSeconds, task.UserID, task.APIKeyID, task.GroupID, task.AccountID, task.VideoURL, task.Error, task.UpstreamCostUSD, task.BilledCostUSD, task.BillingMode, created, completed))
}

func TestMinimaxWorkerActualTierReportedZeroAndAccountTransport(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	task := minimaxBillableTask()
	task.Status, task.CompletedAt, task.VideoURL = MinimaxVideoStatusProcessing, "", ""
	task.CreateStarted = true
	task.ResolutionPrices = map[string]float64{"720p": 0.1, "1080p": 0.4}
	task.ResolutionTiers = map[string]string{"768p": "720p"}
	mock.ExpectExec("UPDATE video_tasks SET status='processing'").WithArgs(task.ID, task.UserID, task.APIKeyID).WillReturnResult(sqlmock.NewResult(0, 1))
	expectMinimaxWorkerLoad(t, mock, task)
	var saved MinimaxVideoTask
	args := make([]driver.Value, 20)
	for i := range args {
		args[i] = sqlmock.AnyArg()
	}
	args[18] = minimaxWorkerSnapshotArg{task: &saved}
	mock.ExpectExec("INSERT INTO video_tasks").WithArgs(args...).WillReturnResult(sqlmock.NewResult(0, 1))
	upstream := &minimaxWorkerHTTP{status: 200, body: `{"id":"provider-id","status":"done","output_resolution":"1080p","duration":8,"video":{"url":"https://cdn.example/result.mp4"},"getoken":{"cost_usd":"0"}}`}
	repo := &minimaxWorkerAccountRepo{account: minimaxWorkerTestAccount()}
	svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream}
	svc.RunMinimaxVideoTask(context.Background(), task.ID, task.UserID, task.APIKeyID, "https://stale.invalid", "stale-token", []byte(`{"model":"MiniMax-H3"}`))
	require.Equal(t, []int64{task.AccountID}, repo.ids)
	require.Len(t, upstream.requests, 1)
	req := upstream.requests[0]
	require.Equal(t, "https://provider.example/v1/videos/generations", req.URL.String())
	require.Equal(t, "Bearer fresh-token", req.Header.Get("Authorization"))
	require.Equal(t, []string{"video-project"}, req.Header["x-provider-project"])
	require.Equal(t, "http://proxy.example:3128", upstream.proxy)
	require.Equal(t, int64(3), upstream.accountID)
	require.Equal(t, 7, upstream.concurrency)
	require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
	require.Equal(t, HTTPUpstreamProfileVideo, HTTPUpstreamProfileFromContext(req.Context()))
	require.True(t, saved.UpstreamCostReported, "explicit zero must settle instead of remaining pending")
	require.True(t, saved.CreateStarted, "a queued provider result must never permit create replay")
	require.Equal(t, "done", saved.Status)
	require.Equal(t, "1080p", saved.Resolution)
	require.Equal(t, 8, saved.DurationSeconds)
	require.Equal(t, 0.4, saved.PricePerSecond)
	require.Equal(t, 3.2, ComputeMinimaxVideoSettlement(saved, saved.BillingMode, nil, 1).BilledCost)
	var status map[string]any
	require.NoError(t, json.Unmarshal(MinimaxVideoTaskStatusBody(saved, "/content"), &status))
	require.Equal(t, float64(0), status["usage"].(map[string]any)["upstream_cost_usd"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMinimaxWorkerPollRetainsKnownTaskOnTransientOrInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body  string
		status      int
		alreadyDone bool
	}{
		{name: "transient 503", status: 503, body: `{"error":"temporarily unavailable"}`},
		{name: "different provider task", status: 200, body: `{"id":"wrong-id","status":"done","video":{"url":"https://cdn.example/video.mp4"}}`},
		{name: "unsafe success URL", status: 200, body: `{"id":"provider-id","status":"done","video":{"url":"https://127.0.0.1/private"}}`},
		{name: "missing success URL", status: 200, body: `{"id":"provider-id","status":"done"}`},
		{name: "metadata retry rotates before failing", status: 503, body: `{}`, alreadyDone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := minimaxTaskMockDB(t)
			videoTaskHandle().SetMaxOpenConns(1)
			task := minimaxBillableTask()
			task.UpstreamID, task.Provider = "provider-id", "minimax"
			if tc.alreadyDone {
				task.BillingMode, task.UpstreamCostUSD, task.UpstreamCostReported = VideoBillingModeUpstreamCost, 0, false
			} else {
				task.Status, task.CompletedAt, task.VideoURL = MinimaxVideoStatusProcessing, "", ""
			}
			key := "minimax-video-poll:" + task.ID
			mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock(hashtextextended($1, 0))")).WithArgs(key).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
			expectMinimaxWorkerLoad(t, mock, task)
			if tc.alreadyDone {
				mock.ExpectExec("UPDATE video_tasks SET task_snapshot=jsonb_set").WithArgs(task.ID).WillReturnResult(sqlmock.NewResult(0, 1))
			} else {
				mock.ExpectExec("INSERT INTO video_tasks").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_unlock(hashtextextended($1, 0))")).WithArgs(key).WillReturnResult(sqlmock.NewResult(0, 1))
			upstream := &minimaxWorkerHTTP{status: tc.status, body: tc.body}
			svc := &OpenAIGatewayService{accountRepo: &minimaxWorkerAccountRepo{account: minimaxWorkerTestAccount()}, httpUpstream: upstream}
			err := svc.PollMinimaxVideoTask(context.Background(), &task)
			require.Error(t, err)
			require.Equal(t, "provider-id", task.UpstreamID)
			if tc.alreadyDone {
				require.Equal(t, MinimaxVideoStatusDone, task.Status)
			} else {
				require.Equal(t, MinimaxVideoStatusProcessing, task.Status)
			}
			require.Len(t, upstream.requests, 1)
			require.Equal(t, http.MethodGet, upstream.requests[0].Method)
			require.Equal(t, "/v1/videos/provider-id", upstream.requests[0].URL.Path)
			require.NotEmpty(t, task.LastPolledAt)
			require.NoError(t, mock.ExpectationsWereMet(), "invalid polls must not publish or charge a terminal result")
		})
	}
}

func TestMinimaxWorkerPollReleasesSingleConnectionBeforeSettlement(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	videoTaskHandle().SetMaxOpenConns(1)
	task := minimaxBillableTask()
	task.Provider, task.UpstreamID = "minimax", "provider-id"
	task.Status, task.CompletedAt, task.VideoURL = MinimaxVideoStatusProcessing, "", ""
	key := "minimax-video-poll:" + task.ID
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock(hashtextextended($1, 0))")).WithArgs(key).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	expectMinimaxWorkerLoad(t, mock, task)
	mock.ExpectExec("INSERT INTO video_tasks").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO video_tasks").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_unlock(hashtextextended($1, 0))")).WithArgs(key).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT 1").WillReturnRows(sqlmock.NewRows([]string{"available"}).AddRow(1))
	mock.ExpectExec("UPDATE video_tasks").WillReturnResult(sqlmock.NewResult(0, 1))
	upstream := &minimaxWorkerHTTP{status: 200, body: `{"id":"provider-id","status":"done","video":{"url":"https://cdn.example/video.mp4"}}`}
	svc := &OpenAIGatewayService{accountRepo: &minimaxWorkerAccountRepo{account: minimaxWorkerTestAccount()}, httpUpstream: upstream,
		usageBillingRepo: &minimaxPollLedgerProbe{}, usageLogRepo: &minimaxUsageLogStub{}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, svc.PollMinimaxVideoTask(ctx, &task))
	require.Equal(t, MinimaxVideoStatusDone, task.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}
