//go:build integration

package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/migrations"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// This opt-in suite uses an isolated schema on an explicitly supplied test DB.
// It never reads existing application rows or sends a provider request.
func videoOperationsIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("VIDEO_OPERATIONS_TEST_DSN")
	if dsn == "" {
		t.Skip("VIDEO_OPERATIONS_TEST_DSN is not configured")
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, u.Scheme)
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("video_operations_test_%d", time.Now().UnixNano())
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	db.SetMaxOpenConns(5)
	previous := videoTaskHandle()
	SetVideoTaskDB(db)
	t.Cleanup(func() {
		SetVideoTaskDB(previous)
		_ = db.Close()
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})
	for _, name := range []string{"340_video_tasks.sql", "343_video_task_settlement.sql", "344_video_task_cost_precision.sql", "345_video_task_operations.sql"} {
		body, err := migrations.FS.ReadFile(name)
		require.NoError(t, err)
		_, err = db.Exec(string(body))
		require.NoError(t, err, name)
	}
	_, err = db.Exec(`CREATE TABLE users(id BIGINT PRIMARY KEY,email TEXT,balance NUMERIC(20,8));
CREATE TABLE groups(id BIGINT PRIMARY KEY,name TEXT);
CREATE TABLE accounts(id BIGINT PRIMARY KEY,name TEXT);
CREATE TABLE api_keys(id BIGINT PRIMARY KEY,name TEXT);
INSERT INTO users VALUES(1,'video-test@example.invalid',100);
INSERT INTO groups VALUES(4,'Video test'); INSERT INTO accounts VALUES(3,'Test supplier');
INSERT INTO api_keys VALUES(2,'Test key');`)
	require.NoError(t, err)
	return db
}

func integrationVideoTask(t *testing.T, id, status, provider string) MinimaxVideoTask {
	t.Helper()
	policy := defaultVideoExecutionPolicy(provider, MinimaxVideoProtocolLL2)
	task := MinimaxVideoTask{ID: id, Provider: provider, UpstreamID: "upstream-" + id,
		Status: status, Model: "test-video", BillingMode: VideoBillingModeLocalPrice,
		CreatedAt: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
		UserID:    1, APIKeyID: 2, AccountID: 3, GroupID: 4, ExecutionPolicy: &policy,
		OutputSecondsReported: true, DurationSeconds: 1, RequestedDurationSeconds: 1,
	}
	if task.IsTerminal() {
		task.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	require.NoError(t, saveMinimaxVideoTask(context.Background(), task))
	return task
}

func TestVideoOperationsPostgresReconciliation(t *testing.T) {
	db := videoOperationsIntegrationDB(t)
	ctx := context.Background()
	integrationVideoTask(t, "ops_done", MinimaxVideoStatusDone, "minimax")
	integrationVideoTask(t, "ops_failed", MinimaxVideoStatusError, "seedance")
	_, err := db.Exec(`UPDATE video_tasks SET billed_cost_usd=10,settled_at=now() WHERE task_id='ops_done'`)
	require.NoError(t, err)
	var input ReconcileVideoTaskInput
	require.NoError(t, json.Unmarshal([]byte(`{"estimated_cost":2,"note":"Estimate only","expected_revision":0}`), &input))
	require.NoError(t, ReconcileVideoTask(ctx, "ops_done", 1, input))
	rec, err := GetAdminVideoTask(ctx, "ops_done")
	require.NoError(t, err)
	require.Equal(t, 2.0, *rec.Procurement.EstimatedCost)
	require.Nil(t, rec.Procurement.ActualCost)
	require.Contains(t, rec.ExceptionCodes, "reconciliation_pending")
	require.Len(t, rec.OperationsHistory, 1)

	// Competing editors at the same revision cannot overwrite one another.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, value := range []int{3, 4} {
		wg.Add(1)
		go func(value int) {
			defer wg.Done()
			var candidate ReconcileVideoTaskInput
			err := json.Unmarshal([]byte(fmt.Sprintf(`{"actual_cost":%d,"reference":"bill-123","note":"Checked supplier bill","expected_revision":1}`, value)), &candidate)
			if err == nil {
				err = ReconcileVideoTask(ctx, "ops_done", 1, candidate)
			}
			results <- err
		}(value)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrVideoOperationConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, conflict)
	rec, err = GetAdminVideoTask(ctx, "ops_done")
	require.NoError(t, err)
	require.Len(t, rec.OperationsHistory, 2)
	require.EqualValues(t, 2, rec.Procurement.Revision)
	confirmed := *rec.Procurement.ActualCost
	// A failed generation may still have a supplier debit.
	input = ReconcileVideoTaskInput{}
	require.NoError(t, json.Unmarshal([]byte(`{"actual_cost":1,"reference":"bill-failed","note":"Provider charged failed task","expected_revision":0}`), &input))
	require.NoError(t, ReconcileVideoTask(ctx, "ops_failed", 1, input))
	stats, err := AdminVideoTaskStatsFor(ctx, AdminVideoTaskFilter{})
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.Procurement.ActualCostCount)
	require.Equal(t, confirmed+1, stats.Procurement.ActualCostTotal)
	require.InDelta(t, (10-confirmed-1)/10*100, *stats.Procurement.ConfirmedMarginPercent, 0.000001)
	rows, total, err := ListAdminVideoTasks(ctx, AdminVideoTaskFilter{VideoTaskFilter: VideoTaskFilter{Page: 1, PageSize: 1, AccountID: 3, GroupID: 4}, ReconciliationStatus: "reconciled"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.EqualValues(t, 2, total)
	userRecord, err := GetVideoTask(ctx, "ops_done", 1)
	require.NoError(t, err)
	encoded, err := json.Marshal(userRecord)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "procurement")
	require.NotContains(t, string(encoded), "execution")
	require.NotContains(t, string(encoded), "bill-123")
	var balance, billed, reported float64
	require.NoError(t, db.QueryRow(`SELECT balance FROM users WHERE id=1`).Scan(&balance))
	require.NoError(t, db.QueryRow(`SELECT billed_cost_usd,upstream_cost_usd FROM video_tasks WHERE task_id='ops_done'`).Scan(&billed, &reported))
	require.Equal(t, 100.0, balance)
	require.Equal(t, 10.0, billed)
	require.Zero(t, reported)
	// Explicit null restores unknown; confirmed zero remains a real value.
	for revision, payload := range []string{
		`{"actual_cost":null,"note":"Reopen disputed bill","expected_revision":2}`,
		`{"actual_cost":0,"reference":"waiver","note":"Confirmed no charge","expected_revision":3}`,
	} {
		input = ReconcileVideoTaskInput{}
		require.NoError(t, json.Unmarshal([]byte(payload), &input))
		require.NoError(t, ReconcileVideoTask(ctx, "ops_done", 1, input))
		rec, err = GetAdminVideoTask(ctx, "ops_done")
		require.NoError(t, err)
		if revision == 0 {
			require.Nil(t, rec.Procurement.ActualCost)
		} else {
			require.NotNil(t, rec.Procurement.ActualCost)
			require.Zero(t, *rec.Procurement.ActualCost)
		}
	}
}

func TestVideoOperationsPostgresExecutionAndExceptions(t *testing.T) {
	db := videoOperationsIntegrationDB(t)
	ctx := context.Background()
	task := integrationVideoTask(t, "ops_retry", MinimaxVideoStatusProcessing, "minimax")
	task.ExecutionPolicy.MaxRetries = 1
	require.NoError(t, saveMinimaxVideoTask(ctx, task))
	loaded, err := loadMinimaxVideoTaskFromDB(ctx, task.ID, task.UserID, task.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, 1, loaded.ExecutionPolicy.MaxRetries, "policy survives process/cache loss")
	for attempt := 0; attempt < 2; attempt++ {
		state, err := beginVideoExecution(ctx, db, *loaded)
		require.NoError(t, err)
		require.NoError(t, finishVideoExecution(db, *loaded, state, context.DeadlineExceeded))
		_, err = db.Exec(`UPDATE video_tasks SET execution_state=execution_state-'next_retry_at' WHERE task_id=$1`, task.ID)
		require.NoError(t, err)
	}
	_, err = beginVideoExecution(ctx, db, *loaded)
	require.ErrorIs(t, err, ErrVideoExecutionPaused)
	state, err := readVideoExecutionState(ctx, db, task.ID)
	require.NoError(t, err)
	require.Equal(t, 2, state.FailureCount)
	require.Equal(t, "retries_exhausted", state.PausedReason)
	rows, total, err := ListAdminVideoTasks(ctx, AdminVideoTaskFilter{Exception: "retry_exhausted"})
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, task.ID, rows[0].TaskID)
	// A live polling connection must block an administrator's reset.
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, "minimax-video-poll:"+task.ID)
	require.NoError(t, err)
	require.ErrorIs(t, ResumeVideoTaskExecution(ctx, task.ID, 1, "Retry checked upstream"), ErrVideoOperationConflict)
	_, err = conn.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, "minimax-video-poll:"+task.ID)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	db.SetMaxOpenConns(1)
	resumeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	require.NoError(t, ResumeVideoTaskExecution(resumeCtx, task.ID, 1, "Retry checked upstream"))
	state, err = readVideoExecutionState(ctx, db, task.ID)
	require.NoError(t, err)
	require.Zero(t, state.FailureCount)
	require.Equal(t, 1, state.RetryGeneration)
	require.Empty(t, state.PausedReason)
	for attempt := 0; attempt < 5; attempt++ {
		state, err := beginVideoExecution(ctx, db, *loaded)
		require.NoError(t, err)
		require.NoError(t, finishVideoExecution(db, *loaded, state, nil))
	}
	state, err = readVideoExecutionState(ctx, db, task.ID)
	require.NoError(t, err)
	require.Zero(t, state.FailureCount, "normal healthy polling never consumes retries")
	unknown := integrationVideoTask(t, "ops_unknown", MinimaxVideoStatusProcessing, "seedance")
	unknown.UpstreamID, unknown.CreateStarted = "", true
	require.NoError(t, saveMinimaxVideoTask(ctx, unknown))
	require.ErrorIs(t, ResumeVideoTaskExecution(ctx, unknown.ID, 1, "unsafe replay"), ErrVideoOperationConflict)
	timeout := integrationVideoTask(t, "ops_timeout", MinimaxVideoStatusProcessing, "minimax")
	_, err = db.Exec(`UPDATE video_tasks SET execution_state=jsonb_build_object('deadline_at',(now()-interval '1 second')::text) WHERE task_id=$1`, timeout.ID)
	require.NoError(t, err)
	// Use RFC3339 since the execution state is an API-compatible snapshot.
	require.NoError(t, saveVideoExecutionState(ctx, db, timeout.ID, VideoExecutionState{DeadlineAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)}))
	_, err = beginVideoExecution(ctx, db, timeout)
	require.ErrorIs(t, err, ErrVideoExecutionPaused)
	missing := integrationVideoTask(t, "ops_missing", MinimaxVideoStatusDone, "seedance")
	_, err = db.Exec(`UPDATE video_tasks SET billing_mode='usage_tokens',task_snapshot=task_snapshot || '{"billing_mode":"usage_tokens","usage_reported":false}'::jsonb WHERE task_id=$1`, missing.ID)
	require.NoError(t, err)
	for code, id := range map[string]string{"timeout": timeout.ID, "submission_unknown": unknown.ID, "missing_usage": missing.ID} {
		rows, total, err := ListAdminVideoTasks(ctx, AdminVideoTaskFilter{Exception: code})
		require.NoError(t, err, code)
		require.EqualValues(t, 1, total, code)
		require.Equal(t, id, rows[0].TaskID, code)
		stats, err := AdminVideoTaskStatsFor(ctx, AdminVideoTaskFilter{Exception: code})
		require.NoError(t, err)
		require.EqualValues(t, 1, stats.Stats.Total)
	}
}
