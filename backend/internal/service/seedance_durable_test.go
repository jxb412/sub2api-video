//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func seedanceBillableTask() MinimaxVideoTask {
	task := minimaxBillableTask()
	task.ID, task.Provider, task.Model, task.BillingMode = "sdv_bill", VideoProviderSeedance, "seedance-test", VideoBillingModeUsageTokens
	task.InputTokens, task.OutputTokens = 0, 12000
	task.UsageReported, task.TokenPricingConfigured = true, true
	task.OutputPricePerToken = 0.00001
	task.UpstreamCostUSD = 0
	return task
}

func TestSeedanceNativeMetadataAndValidation(t *testing.T) {
	info, err := ParseSeedanceVideoRequest([]byte(`{"model":"seedance-test","content":[{"type":"text","text":"Waves --dur 5 --rs 720p"},{"type":"image_url","image_url":{"url":"https://images.example/reference.png"}}],"duration":5,"resolution":"720p"}`))
	require.NoError(t, err)
	require.Equal(t, 5, info.DurationSeconds)
	require.Equal(t, "720p", info.Resolution)
	moderation := ExtractContentModerationInput(ContentModerationProtocolOpenAIImages, info.ModerationBody())
	require.Contains(t, moderation.Images, "https://images.example/reference.png")
	for _, typ := range []string{"audio_url", "video_url"} {
		_, err := ParseSeedanceVideoRequest([]byte(`{"model":"seedance-test","content":[{"type":"` + typ + `","` + typ + `":{"url":"https://media.example/reference"}}]}`))
		require.NoError(t, err, "native reference-only requests remain supported")
	}
	for _, body := range []string{
		`{"model":"x","content":[{"type":"text","text":"waves --dur 10"}],"duration":5}`,
		`{"model":"x","content":[{"type":"image_url","image_url":{}}]}`,
		`{"model":"x","content":[{"type":"text","text":"waves"}],"return_last_frame":true}`,
		`{"model":"x","content":[{"type":"text","text":"waves"}],"callback_url":"https://callback.example"}`,
		`{"model":"x","content":[{"type":"text","text":"waves"}],"frames":121,"duration":5}`,
	} {
		_, err := ParseSeedanceVideoRequest([]byte(body))
		require.Error(t, err, body)
	}
	lifetime, err := SeedanceVideoLifetime([]byte(`{}`))
	require.NoError(t, err)
	require.Equal(t, 48*time.Hour, lifetime)
	_, err = SeedanceVideoLifetime([]byte(`{"execution_expires_after":60}`))
	require.Error(t, err)
}

func TestSeedanceFinalResponseRequiresAuthenticUsageAndVideo(t *testing.T) {
	for _, body := range []string{`[]`, `{"id":"task","status":"succeeded"}`, `{"id":"task","status":"succeeded","content":{"video_url":"https://cdn.example/a.mp4"},"usage":{"completion_tokens":-1}}`, `{"id":"../task","status":"queued"}`} {
		_, err := ParseSeedanceVideoResponse([]byte(body))
		require.Error(t, err, body)
	}
	result, err := ParseSeedanceVideoResponse([]byte(`{"id":"ark-1","status":"succeeded","resolution":"720p","duration":5,"content":{"video_url":"https://cdn.example/signed-secret.mp4"},"usage":{"completion_tokens":12000},"getoken":{"cost_usd":"0"}}`))
	require.NoError(t, err)
	require.True(t, result.UsageReported)
	require.True(t, result.UpstreamCostReported)
	require.Zero(t, result.UpstreamCostUSD)
	result.ID = "sdv_local"
	result.Model = "customer-model"
	body := SeedanceVideoStatusBody(result, "/api/v3/contents/generations/tasks/sdv_local/content")
	require.Equal(t, "succeeded", gjson.GetBytes(body, "status").String())
	require.Equal(t, "sdv_local", gjson.GetBytes(body, "id").String())
	require.Equal(t, 12000, int(gjson.GetBytes(body, "usage.completion_tokens").Int()))
	require.NotContains(t, string(body), "signed-secret")
}

func TestSeedanceSnapshotsExplicitTokenPriceAndRejectsMissingTariff(t *testing.T) {
	price := 0.00001
	group := &Group{ID: 4, RateMultiplier: 1, VideoBillingMode: VideoBillingModeUsageTokens, ModelPricing: []ChannelModelPricing{{Models: []string{"seedance-test"}, BillingMode: BillingModeToken, OutputPrice: &price}}}
	key := &APIKey{ID: 2, Group: group, GroupID: &group.ID}
	svc := &OpenAIGatewayService{billingService: NewBillingService(&config.Config{}, nil)}
	task := seedanceBillableTask()
	require.NoError(t, svc.PrepareVideoBilling(context.Background(), &task, key))
	require.Equal(t, price, task.OutputPricePerToken)
	price = 0.99
	require.Equal(t, 0.12, ComputeMinimaxVideoSettlement(task, task.BillingMode, nil, 1).BilledCost, "accepted tariff is immutable")
	group.ModelPricing = nil
	task = seedanceBillableTask()
	require.ErrorContains(t, svc.PrepareVideoBilling(context.Background(), &task, key), "prices are not configured")
	zero := 0.0
	group.ModelPricing = []ChannelModelPricing{{Models: []string{"seedance-test"}, BillingMode: BillingModeToken, OutputPrice: &zero}}
	require.NoError(t, svc.PrepareVideoBilling(context.Background(), &task, key))
	require.Zero(t, task.OutputPricePerToken)
	group.VideoBillingMode = VideoBillingModeLocalPrice
	require.ErrorContains(t, svc.PrepareVideoBilling(context.Background(), &task, key), "not configured")
}

func TestSeedanceLedgerRetryIsExactlyOnceAndKeepsUsage(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	ledger := &minimaxLedgerStub{claims: map[string]string{}}
	logs := &minimaxUsageLogStub{fail: true}
	svc := &OpenAIGatewayService{usageBillingRepo: ledger, usageLogRepo: logs}
	task := seedanceBillableTask()
	_, err := svc.settleMinimaxVideoTask(context.Background(), task)
	require.Error(t, err)
	require.Equal(t, 1, ledger.charges)
	mock.ExpectExec("UPDATE video_tasks").WithArgs(task.ID, 0.0, 0.12, VideoBillingModeUsageTokens).WillReturnResult(sqlmock.NewResult(0, 1))
	_, err = svc.settleMinimaxVideoTask(context.Background(), task)
	require.NoError(t, err)
	require.Equal(t, 1, ledger.charges)
	require.Equal(t, 0.12, ledger.amount)
	require.Equal(t, 12000, logs.log.OutputTokens)
	require.Equal(t, "/api/v3/contents/generations/tasks", *logs.log.InboundEndpoint)
	require.Equal(t, 0.12, *logs.log.AccountStatsCost)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSeedanceMissingFinalMetadataNeverBecomesFree(t *testing.T) {
	ledger := &minimaxLedgerStub{claims: map[string]string{}}
	svc := &OpenAIGatewayService{usageBillingRepo: ledger, usageLogRepo: &minimaxUsageLogStub{}}
	task := seedanceBillableTask()
	task.UsageReported = false
	_, err := svc.settleMinimaxVideoTask(context.Background(), task)
	require.ErrorContains(t, err, "settlement pending")
	require.Zero(t, ledger.charges)
	task.BillingMode = VideoBillingModeUpstreamCost
	_, err = svc.settleMinimaxVideoTask(context.Background(), task)
	require.ErrorContains(t, err, "not reported")
	require.Zero(t, ledger.charges)
}

func TestSeedanceProviderFailureNeverCharges(t *testing.T) {
	ledger := &minimaxLedgerStub{claims: map[string]string{}}
	svc := &OpenAIGatewayService{usageBillingRepo: ledger, usageLogRepo: &minimaxUsageLogStub{}}
	for _, state := range []string{"failed", "cancelled", "expired"} {
		result, err := ParseSeedanceVideoResponse([]byte(`{"id":"up","status":"` + state + `","usage":{"completion_tokens":12000}}`))
		require.NoError(t, err)
		task := seedanceBillableTask()
		mergeVideoProviderResult(&task, result)
		_, err = svc.settleMinimaxVideoTask(context.Background(), task)
		require.Error(t, err)
	}
	require.Zero(t, ledger.charges)
}

func TestSeedanceCreatePersistsBeforePollingAndSurvivesMissingClientPoll(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	task := seedanceBillableTask()
	task.Status = MinimaxVideoStatusQueued
	mock.ExpectExec("UPDATE video_tasks SET status='processing'").WithArgs(task.ID, task.UserID, task.APIKeyID).WillReturnResult(sqlmock.NewResult(0, 1))
	// A provider may already complete in its create response. Billing must happen
	// without any downstream GET and only after the final snapshot is durable.
	mock.ExpectExec("INSERT INTO video_tasks").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE video_tasks").WithArgs(task.ID, 0.0, 0.12, VideoBillingModeUsageTokens).WillReturnResult(sqlmock.NewResult(0, 1))
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"up","status":"succeeded","duration":5,"resolution":"720p","content":{"video_url":"https://cdn.example/a.mp4"},"usage":{"completion_tokens":12000}}`)}
	ledger := &minimaxLedgerStub{claims: map[string]string{}}
	svc := &OpenAIGatewayService{httpUpstream: upstream, usageBillingRepo: ledger, usageLogRepo: &minimaxUsageLogStub{}}
	svc.RunSeedanceVideoTask(context.Background(), task, seedanceTestAccount(), "https://ark.example/api/v3/contents/generations/tasks", []byte(`{}`))
	require.Equal(t, 1, ledger.charges)
	require.Equal(t, http.MethodPost, upstream.request.Method)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSeedanceTaskPersistsOwnerAndTariffWithoutRedis(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	task := seedanceBillableTask()
	task.UpstreamID = "up-1"
	payload, err := json.Marshal(task)
	require.NoError(t, err)
	columns := strings.Split("task_snapshot,task_id,upstream_id,status,model,upstream_model,prompt,resolution,duration_seconds,user_id,api_key_id,group_id,account_id,video_url,error_message,upstream_cost_usd,billed_cost_usd,billing_mode,created_at,completed_at", ",")
	created := time.Now().Add(-time.Minute)
	completed := time.Now()
	mock.ExpectQuery("SELECT task_snapshot.*FROM video_tasks WHERE task_id=\\$1 AND user_id=\\$2 AND api_key_id=\\$3 AND deleted_at IS NULL").WithArgs(task.ID, task.UserID, task.APIKeyID).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(string(payload), task.ID, task.UpstreamID, task.Status, task.Model, task.Model, task.Prompt, task.Resolution, 5, task.UserID, task.APIKeyID, task.GroupID, task.AccountID, task.VideoURL, "", 0, 0.12, task.BillingMode, created, completed))
	loaded, err := (&OpenAIGatewayService{}).LoadMinimaxVideoTask(context.Background(), task.ID, task.UserID, task.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, VideoProviderSeedance, loaded.Provider)
	require.Equal(t, 12000, loaded.OutputTokens)
	require.Equal(t, 0.00001, loaded.OutputPricePerToken)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSeedanceSubmitAmbiguityIsNotRetried(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	task := seedanceBillableTask()
	task.Status = MinimaxVideoStatusQueued
	mock.ExpectExec("UPDATE video_tasks SET status='processing'").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO video_tasks").WillReturnResult(sqlmock.NewResult(0, 1))
	upstream := &seedanceAmbiguousUpstream{}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	svc.RunSeedanceVideoTask(context.Background(), task, seedanceTestAccount(), "https://ark.example/api/v3/contents/generations/tasks", nil)
	require.Equal(t, 1, upstream.calls)
	require.NoError(t, mock.ExpectationsWereMet())
}

type seedanceAmbiguousUpstream struct {
	HTTPUpstream
	calls int
}

func TestSeedanceRecoveryEnrichesMissingUsageAndSettlesWithoutClient(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	videoTaskHandle().SetMaxOpenConns(1)
	task := seedanceBillableTask()
	task.AccountID = seedanceTestAccount().ID
	task.UpstreamID = "ark-1"
	task.UsageReported = false
	task.OutputTokens = 0
	lockKey := "seedance-video-poll:" + task.ID
	expectMinimaxWorkerLoad(t, mock, task)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock(hashtextextended($1, 0))")).WithArgs(lockKey).WillReturnRows(sqlmock.NewRows([]string{"claimed"}).AddRow(true))
	expectMinimaxWorkerLoad(t, mock, task)
	mock.ExpectExec("UPDATE video_tasks SET task_snapshot=jsonb_set").WithArgs(task.ID).WillReturnResult(sqlmock.NewResult(0, 1))
	var enriched MinimaxVideoTask
	mock.ExpectExec("UPDATE video_tasks SET task_snapshot=\\$2,upstream_cost_usd=\\$3").WithArgs(task.ID, minimaxWorkerSnapshotArg{task: &enriched}, 0.0).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_unlock(hashtextextended($1, 0))")).WithArgs(lockKey).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE video_tasks").WithArgs(task.ID, 0.0, 0.12, VideoBillingModeUsageTokens).WillReturnResult(sqlmock.NewResult(0, 1))
	ledger := &minimaxLedgerStub{claims: map[string]string{}}
	upstream := &minimaxWorkerHTTP{status: 200, body: `{"id":"ark-1","status":"succeeded","content":{"video_url":"https://cdn.example/video.mp4"},"usage":{"completion_tokens":12000}}`}
	svc := &OpenAIGatewayService{accountRepo: &minimaxWorkerAccountRepo{account: seedanceTestAccount()}, httpUpstream: upstream, usageBillingRepo: ledger, usageLogRepo: &minimaxUsageLogStub{}}
	done, err := svc.pollSeedanceTask(context.Background(), task.ID, task.UserID, task.APIKeyID)
	require.NoError(t, err)
	require.True(t, done)
	require.Equal(t, 1, ledger.charges)
	require.Equal(t, 12000, enriched.OutputTokens)
	require.True(t, enriched.UsageReported)
	require.Equal(t, task.OutputPricePerToken, enriched.OutputPricePerToken)
	require.Equal(t, task.CompletedAt, enriched.CompletedAt)
	require.True(t, HTTPUpstreamRedirectsDisabled(upstream.requests[0].Context()))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSeedanceRecoveryKeepsPendingOnTransientLookupFailure(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	videoTaskHandle().SetMaxOpenConns(1)
	task := seedanceBillableTask()
	task.AccountID = seedanceTestAccount().ID
	task.UpstreamID = "ark-pending"
	task.Status = MinimaxVideoStatusProcessing
	task.CompletedAt = ""
	task.ExpiresAt = time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339Nano)
	lockKey := "seedance-video-poll:" + task.ID
	expectMinimaxWorkerLoad(t, mock, task)
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WithArgs(lockKey).WillReturnRows(sqlmock.NewRows([]string{"claimed"}).AddRow(true))
	expectMinimaxWorkerLoad(t, mock, task)
	mock.ExpectExec("INSERT INTO video_tasks").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT pg_advisory_unlock").WithArgs(lockKey).WillReturnResult(sqlmock.NewResult(0, 1))
	ledger := &minimaxLedgerStub{claims: map[string]string{}}
	upstream := &minimaxWorkerHTTP{status: 503, body: `{"error":"temporarily unavailable"}`}
	svc := &OpenAIGatewayService{accountRepo: &minimaxWorkerAccountRepo{account: seedanceTestAccount()}, httpUpstream: upstream, usageBillingRepo: ledger}
	done, err := svc.pollSeedanceTask(context.Background(), task.ID, task.UserID, task.APIKeyID)
	require.Error(t, err)
	require.False(t, done)
	require.Zero(t, ledger.charges)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, http.MethodGet, upstream.requests[0].Method)
	require.NoError(t, mock.ExpectationsWereMet())
}

func (s *seedanceAmbiguousUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	s.calls++
	return nil, errors.New("connection reset after write")
}
