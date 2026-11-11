package service

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func ll2SubmissionFixture(t *testing.T) (MinimaxVideoTask, *Account) {
	t.Helper()
	task := minimaxBillableTask()
	task.Status, task.VideoURL, task.CompletedAt = MinimaxVideoStatusQueued, "", ""
	task.UpstreamID = ""
	account := minimaxWorkerTestAccount()
	account.Credentials["minimax_video_protocol"] = MinimaxVideoProtocolLL2
	account.Credentials["base_url"] = "https://provider.example"
	body := []byte(`{"model":"MiniMax-H3", "content":[{"type":"text","text":"hello"}],"duration":5,"resolution":"768P","ratio":"16:9"}`)
	require.NoError(t, (&OpenAIGatewayService{}).SnapshotMinimaxVideoProtocol(&task, account, body))
	return task, account
}

func expectLL2SubmitLock(mock sqlmock.Sqlmock, task MinimaxVideoTask, acquired bool) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock(hashtextextended($1, 0))")).
		WithArgs("minimax-video-submit:" + task.ID).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(acquired))
}

func expectLL2SubmitUnlock(mock sqlmock.Sqlmock, task MinimaxVideoTask) {
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_unlock(hashtextextended($1, 0))")).
		WithArgs("minimax-video-submit:" + task.ID).WillReturnResult(sqlmock.NewResult(0, 1))
}

func expectLL2SavedTask(mock sqlmock.Sqlmock, saved *MinimaxVideoTask) {
	args := make([]driver.Value, 20)
	for i := range args {
		args[i] = sqlmock.AnyArg()
	}
	args[18] = minimaxWorkerSnapshotArg{task: saved}
	mock.ExpectExec("INSERT INTO video_tasks").WithArgs(args...).WillReturnResult(sqlmock.NewResult(0, 1))
}

type ll2AccountPoolProbe struct {
	AccountRepository
	account *Account
}

func (r *ll2AccountPoolProbe) GetByID(ctx context.Context, _ int64) (*Account, error) {
	var available int
	if err := videoTaskHandle().QueryRowContext(ctx, "SELECT 1").Scan(&available); err != nil {
		return nil, err
	}
	return r.account, nil
}

func TestLL2SubmitPreservesIdentityAcrossRestart(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	videoTaskHandle().SetMaxOpenConns(1)
	task, account := ll2SubmissionFixture(t)
	// A previous process persisted intent and sent the request, then died before
	// storing the provider ID. Replaying these bytes with this key is safe.
	task.CreateStarted, task.Status = true, MinimaxVideoStatusProcessing
	expectMinimaxWorkerLoad(t, mock, task)
	mock.ExpectQuery("SELECT 1").WillReturnRows(sqlmock.NewRows([]string{"available"}).AddRow(1))
	expectLL2SubmitLock(mock, task, true)
	expectMinimaxWorkerLoad(t, mock, task)
	var intent, accepted MinimaxVideoTask
	expectLL2SavedTask(mock, &intent)
	expectLL2SavedTask(mock, &accepted)
	expectLL2SubmitUnlock(mock, task)
	upstream := &minimaxWorkerHTTP{status: http.StatusOK, body: `{"task_id":"ll2-task-123"}`}
	svc := &OpenAIGatewayService{accountRepo: &ll2AccountPoolProbe{account: account}, httpUpstream: upstream}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, svc.RunLL2MinimaxVideoTask(ctx, task.ID, task.UserID, task.APIKeyID))
	require.Len(t, upstream.requests, 1)
	req := upstream.requests[0]
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	// JSONB may canonicalize the original JSON. Both the first attempt and all
	// retries use the persisted bytes, rather than the pre-admission builder.
	require.Equal(t, []byte(intent.UpstreamRequestBody), body)
	require.JSONEq(t, string(task.UpstreamRequestBody), string(body))
	require.Equal(t, task.ID, req.Header.Get("Idempotency-Key"))
	require.Equal(t, "/v2/video_generation", req.URL.Path)
	require.True(t, intent.CreateStarted)
	require.Equal(t, "ll2-task-123", accepted.UpstreamID)
	require.False(t, accepted.IsTerminal())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLL2KnownTaskNeverResubmits(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	task, _ := ll2SubmissionFixture(t)
	task.UpstreamID = "already-accepted"
	expectMinimaxWorkerLoad(t, mock, task)
	// No account lookup, lock, or HTTP request is required after acceptance.
	require.NoError(t, (&OpenAIGatewayService{}).RunLL2MinimaxVideoTask(context.Background(), task.ID, task.UserID, task.APIKeyID))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLL2SubmitNeverPublishesUnconfirmedCompletion(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	task, account := ll2SubmissionFixture(t)
	expectMinimaxWorkerLoad(t, mock, task)
	expectLL2SubmitLock(mock, task, true)
	expectMinimaxWorkerLoad(t, mock, task)
	var intent, accepted MinimaxVideoTask
	expectLL2SavedTask(mock, &intent)
	expectLL2SavedTask(mock, &accepted)
	expectLL2SubmitUnlock(mock, task)
	upstream := &minimaxWorkerHTTP{status: 200, body: `{"task":{"id":"ll2-task-123","status":"succeeded","duration":15,"usage":{"output_seconds":15},"content":{"url":"https://cdn.example/result.mp4"}}}`}
	svc := &OpenAIGatewayService{accountRepo: &minimaxWorkerAccountRepo{account: account}, httpUpstream: upstream}
	require.NoError(t, svc.RunLL2MinimaxVideoTask(context.Background(), task.ID, task.UserID, task.APIKeyID))
	require.Equal(t, "ll2-task-123", accepted.UpstreamID)
	require.Equal(t, MinimaxVideoStatusProcessing, accepted.Status)
	require.Equal(t, task.RequestedDurationSeconds, accepted.DurationSeconds)
	require.False(t, accepted.OutputSecondsReported)
	require.Empty(t, accepted.VideoURL)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLL2SubmissionErrorsOnlyFailDefinitelyRejectedFirstAttempt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		prior    bool
		terminal bool
	}{
		{name: "first invalid request", status: 400, body: `{}`, terminal: true},
		{name: "temporary unavailable", status: 503, body: `{}`},
		{name: "rate limited", status: 429, body: `{}`},
		{name: "ambiguous gateway timeout", status: 504, body: `{}`},
		{name: "invalid accepted response", status: 200, body: `{"request_id":"http-not-task"}`},
		{name: "rejected recovery cannot disprove original acceptance", status: 400, body: `{}`, prior: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := minimaxTaskMockDB(t)
			task, account := ll2SubmissionFixture(t)
			task.CreateStarted = tc.prior
			expectMinimaxWorkerLoad(t, mock, task)
			expectLL2SubmitLock(mock, task, true)
			expectMinimaxWorkerLoad(t, mock, task)
			var intent, outcome MinimaxVideoTask
			expectLL2SavedTask(mock, &intent)
			if tc.terminal {
				expectLL2SavedTask(mock, &outcome)
			}
			expectLL2SubmitUnlock(mock, task)
			upstream := &minimaxWorkerHTTP{status: tc.status, body: tc.body}
			svc := &OpenAIGatewayService{accountRepo: &minimaxWorkerAccountRepo{account: account}, httpUpstream: upstream}
			require.Error(t, svc.RunLL2MinimaxVideoTask(context.Background(), task.ID, task.UserID, task.APIKeyID))
			require.True(t, intent.CreateStarted)
			require.False(t, intent.IsTerminal())
			if tc.terminal {
				require.Equal(t, MinimaxVideoStatusError, outcome.Status)
				require.NotEmpty(t, outcome.CompletedAt)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestLL2SubmissionLockPreventsConcurrentCreate(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	task, account := ll2SubmissionFixture(t)
	expectMinimaxWorkerLoad(t, mock, task)
	expectLL2SubmitLock(mock, task, false)
	upstream := &minimaxWorkerHTTP{}
	svc := &OpenAIGatewayService{accountRepo: &minimaxWorkerAccountRepo{account: account}, httpUpstream: upstream}
	require.NoError(t, svc.RunLL2MinimaxVideoTask(context.Background(), task.ID, task.UserID, task.APIKeyID))
	require.Empty(t, upstream.requests)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLL2SubmissionChangedBodyDoesNotReachProvider(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	task, account := ll2SubmissionFixture(t)
	expectMinimaxWorkerLoad(t, mock, task)
	expectLL2SubmitLock(mock, task, true)
	fresh := task
	fresh.UpstreamRequestBody = []byte(`{"changed":true}`)
	expectMinimaxWorkerLoad(t, mock, fresh)
	expectLL2SubmitUnlock(mock, task)
	upstream := &minimaxWorkerHTTP{}
	svc := &OpenAIGatewayService{accountRepo: &minimaxWorkerAccountRepo{account: account}, httpUpstream: upstream}
	require.ErrorContains(t, svc.RunLL2MinimaxVideoTask(context.Background(), task.ID, task.UserID, task.APIKeyID), "identity changed")
	require.Empty(t, upstream.requests)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLL2SubmissionExpiryAndCredentialChangeRemainPending(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{true: "expiry", false: "credentials"}[expired], func(t *testing.T) {
			mock := minimaxTaskMockDB(t)
			task, account := ll2SubmissionFixture(t)
			if expired {
				task.CreatedAt = time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339Nano)
			} else {
				account.Credentials["api_key"] = "rotated-token"
			}
			expectMinimaxWorkerLoad(t, mock, task)
			upstream := &minimaxWorkerHTTP{}
			svc := &OpenAIGatewayService{accountRepo: &minimaxWorkerAccountRepo{account: account}, httpUpstream: upstream}
			err := svc.RunLL2MinimaxVideoTask(context.Background(), task.ID, task.UserID, task.APIKeyID)
			require.Error(t, err)
			if expired {
				require.ErrorIs(t, err, ErrLL2SubmissionExpired)
			}
			require.Empty(t, upstream.requests)
			require.NoError(t, mock.ExpectationsWereMet(), "neither case proves generation failed")
		})
	}
}

func TestLL2SubmissionRejectionClassification(t *testing.T) {
	require.False(t, ll2SubmissionDefinitelyRejected(context.DeadlineExceeded, false))
	require.False(t, ll2SubmissionDefinitelyRejected(errors.New("read body failed"), false))
	for _, status := range []int{408, 409, 425, 429, 500, 502, 503, 504} {
		require.False(t, ll2SubmissionDefinitelyRejected(&MinimaxVideoUpstreamError{StatusCode: status}, false))
	}
	require.True(t, ll2SubmissionDefinitelyRejected(&MinimaxVideoUpstreamError{StatusCode: 400}, false))
	require.False(t, ll2SubmissionDefinitelyRejected(&MinimaxVideoUpstreamError{StatusCode: 400}, true))
}

func TestLL2SubmissionSnapshotRejectsMissingRequest(t *testing.T) {
	task, _ := ll2SubmissionFixture(t)
	task.UpstreamRequestBody = nil
	require.Error(t, validateLL2SubmissionSnapshot(task, time.Now()))
	task.UpstreamRequestBody = []byte(`[]`)
	require.Error(t, validateLL2SubmissionSnapshot(task, time.Now()))
	task.UpstreamRequestBody = []byte(strings.Repeat(" ", 2) + `{}`)
	require.NoError(t, validateLL2SubmissionSnapshot(task, time.Now()))
}
