package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func minimaxTaskMockDB(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	prior := videoTaskHandle()
	SetVideoTaskDB(db)
	t.Cleanup(func() { SetVideoTaskDB(prior); db.Close() })
	return mock
}

func TestMinimaxTaskRequiresDurableStorage(t *testing.T) {
	prior := videoTaskHandle()
	SetVideoTaskDB(nil)
	t.Cleanup(func() { SetVideoTaskDB(prior) })
	task := minimaxBillableTask()
	task.Status = MinimaxVideoStatusQueued
	require.Error(t, (&OpenAIGatewayService{}).StoreMinimaxVideoTask(context.Background(), task))
}

func TestMinimaxTaskStoreReportsDatabaseFailure(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	mock.ExpectExec("INSERT INTO video_tasks").WillReturnError(errors.New("database unavailable"))
	task := minimaxBillableTask()
	task.Status = MinimaxVideoStatusQueued
	err := (&OpenAIGatewayService{}).StoreMinimaxVideoTask(context.Background(), task)
	require.ErrorContains(t, err, "database unavailable")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMinimaxTaskLoadWithoutRedisHonorsCancelledState(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	task := minimaxBillableTask()
	payload, err := json.Marshal(task)
	require.NoError(t, err)
	columns := strings.Split("task_snapshot,task_id,upstream_id,status,model,upstream_model,prompt,resolution,duration_seconds,user_id,api_key_id,group_id,account_id,video_url,error_message,upstream_cost_usd,billed_cost_usd,billing_mode,created_at,completed_at", ",")
	created := time.Now().Add(-time.Minute)
	completed := time.Now()
	mock.ExpectQuery("SELECT task_snapshot.*FROM video_tasks WHERE task_id=\\$1 AND user_id=\\$2 AND api_key_id=\\$3 AND deleted_at IS NULL").
		WithArgs(task.ID, task.UserID, task.APIKeyID).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(string(payload), task.ID, "up-id", "error", task.Model, task.Model, "prompt", "768p", 5, task.UserID, task.APIKeyID, task.GroupID, task.AccountID, "", "cancelled", 0, 0, task.BillingMode, created, completed))
	loaded, err := (&OpenAIGatewayService{}).LoadMinimaxVideoTask(context.Background(), task.ID, task.UserID, task.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, "error", loaded.Status)
	require.Empty(t, loaded.VideoURL)
	require.Equal(t, task.BillingSnapshot, loaded.BillingSnapshot)
	require.Equal(t, 0.1, loaded.PricePerSecond)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMinimaxTaskCannotOverwriteTerminalRow(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	mock.ExpectExec("INSERT INTO video_tasks").WillReturnResult(sqlmock.NewResult(0, 0))
	err := (&OpenAIGatewayService{}).StoreMinimaxVideoTask(context.Background(), minimaxBillableTask())
	require.ErrorIs(t, err, ErrVideoTaskTerminal)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMinimaxPendingResponseRemainsPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/videos/generations":
			w.Write([]byte(`{"id":"up-id","status":"processing"}`))
		case "/v1/videos/up-id":
			require.Equal(t, http.MethodGet, r.Method)
			w.Write([]byte(`{"status":"done","video":{"url":"/v1/videos/up-id/content"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	result, err := ExecuteMinimaxVideoTask(context.Background(), srv.Client(), srv.URL+"/v1/videos/generations", "secret", []byte(`{}`))
	require.NoError(t, err)
	require.False(t, result.IsTerminal())
	result, err = pollMinimaxVideoUpstream(context.Background(), srv.Client(), srv.URL+"/v1/videos/generations", "secret", result.UpstreamID)
	require.NoError(t, err)
	require.Equal(t, "done", result.Status)
	require.Equal(t, "up-id", result.UpstreamID)
}

func TestMinimaxCreateDoesNotFollowRedirects(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := ExecuteMinimaxVideoTask(context.Background(), MinimaxVideoUpstreamClient(time.Second), srv.URL, "secret", []byte(`{}`))
	require.Error(t, err)
	require.False(t, called)
}
