package service

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestVideoExecutionPolicyValidationAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"fraction", map[string]any{"max_retries": 1.5}},
		{"negative", map[string]any{"max_retries": -1}},
		{"unbounded", map[string]any{"task_timeout_seconds": 259201}},
		{"zero timeout", map[string]any{"request_timeout_seconds": 0}},
		{"bool string", map[string]any{"retry_enabled": "false"}},
		{"null bool", map[string]any{"retry_enabled": nil}},
		{"unknown field", map[string]any{"retry_count": 3}},
		{"array", []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, ValidateVideoExecutionPolicyExtra(map[string]any{"video_execution_policy": tc.value}))
		})
	}
	for _, provider := range []string{"minimax", VideoProviderSeedance} {
		for _, protocol := range []string{MinimaxVideoProtocolLL2, MinimaxVideoProtocolGetoken} {
			task := MinimaxVideoTask{Provider: provider, UpstreamProtocol: protocol}
			account := &Account{Extra: map[string]any{"video_execution_policy": map[string]any{"max_retries": 0, "retry_enabled": false}}}
			require.NoError(t, SnapshotVideoExecutionPolicy(&task, account))
			want := defaultVideoExecutionPolicy(provider, protocol)
			require.Equal(t, want.RequestTimeoutSeconds, task.ExecutionPolicy.RequestTimeoutSeconds)
			require.Equal(t, want.TaskTimeoutSeconds, task.ExecutionPolicy.TaskTimeoutSeconds)
			require.Zero(t, task.ExecutionPolicy.MaxRetries)
			require.False(t, task.ExecutionPolicy.RetryEnabled)
			account.Extra["video_execution_policy"] = map[string]any{"max_retries": 9}
			require.Zero(t, task.ExecutionPolicy.MaxRetries, "account edits must not mutate in-flight policy")
		}
	}
	require.NoError(t, ValidateVideoExecutionPolicyExtra(nil))
	require.NoError(t, ValidateVideoExecutionPolicyExtra(map[string]any{"video_execution_policy": nil}))
}

func TestVideoExecutionFailureBudgetAndSafeErrors(t *testing.T) {
	policy := defaultVideoExecutionPolicy("minimax", MinimaxVideoProtocolLL2)
	policy.MaxRetries = 2
	now := time.Now().UTC()
	var state VideoExecutionState
	for failure := 1; failure <= 3; failure++ {
		videoExecutionFailed(&state, policy, now, errors.New("Bearer TOP-SECRET https://signed.invalid?token=SECRET"))
		require.Equal(t, failure, state.FailureCount)
		require.NotContains(t, state.LastError, "SECRET")
		if failure <= 2 {
			require.Empty(t, state.PausedReason)
			require.Equal(t, now.Add(30*time.Second).Format(time.RFC3339Nano), state.NextRetryAt)
		} else {
			require.Equal(t, "retries_exhausted", state.PausedReason)
			require.Empty(t, state.NextRetryAt)
		}
	}
	policy.RetryEnabled = false
	state = VideoExecutionState{}
	videoExecutionFailed(&state, policy, now, context.DeadlineExceeded)
	require.Equal(t, "retry_disabled", state.PausedReason)
	require.Equal(t, "Provider request timed out", state.LastError)
}

func expectVideoExecutionRead(mock sqlmock.Sqlmock, id string, state VideoExecutionState) {
	payload, _ := json.Marshal(state)
	mock.ExpectQuery("SELECT execution_state FROM video_tasks").WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"execution_state"}).AddRow(string(payload)))
}

func TestVideoExecutionGateStopsRetryAndDeadlineWithoutNetwork(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state VideoExecutionState
	}{
		{"paused", VideoExecutionState{PausedReason: "retries_exhausted"}},
		{"backoff", VideoExecutionState{NextRetryAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}},
		{"deadline", VideoExecutionState{DeadlineAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}},
		{"interrupted exhausted", VideoExecutionState{AttemptInProgress: true, FailureCount: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := minimaxTaskMockDB(t)
			policy := defaultVideoExecutionPolicy("minimax", MinimaxVideoProtocolLL2)
			task := MinimaxVideoTask{ID: "gate-task", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExecutionPolicy: &policy}
			expectVideoExecutionRead(mock, task.ID, tc.state)
			mock.ExpectExec("UPDATE video_tasks SET execution_state").WithArgs(task.ID, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
			calls := 0
			_, err := runVideoProviderAttempt(context.Background(), videoTaskHandle(), task, 30*time.Second, func(context.Context) (MinimaxVideoTask, error) {
				calls++
				return MinimaxVideoTask{}, nil
			})
			require.ErrorIs(t, err, ErrVideoExecutionPaused)
			require.Zero(t, calls)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestVideoExecutionSuccessResetsOnlyConsecutiveFailures(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	policy := defaultVideoExecutionPolicy("minimax", MinimaxVideoProtocolLL2)
	task := MinimaxVideoTask{ID: "healthy-task", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExecutionPolicy: &policy}
	state := VideoExecutionState{FailureCount: 2, LastError: "Provider request timed out", AttemptInProgress: true, RetryGeneration: 4, DeadlineAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}
	mock.ExpectExec("UPDATE video_tasks SET execution_state").WithArgs(task.ID, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, finishVideoExecution(videoTaskHandle(), task, &state, nil))
	require.Zero(t, state.FailureCount)
	require.Empty(t, state.LastError)
	require.False(t, state.AttemptInProgress)
	require.Equal(t, 4, state.RetryGeneration)
	require.NotEmpty(t, state.DeadlineAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVideoExecutionMissingSettlementMetadataConsumesRetry(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	policy := defaultVideoExecutionPolicy(VideoProviderSeedance, "")
	policy.MaxRetries = 0
	task := MinimaxVideoTask{ID: "missing-usage", Provider: VideoProviderSeedance, BillingMode: VideoBillingModeUsageTokens, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExecutionPolicy: &policy}
	expectVideoExecutionRead(mock, task.ID, VideoExecutionState{})
	mock.ExpectExec("UPDATE video_tasks SET execution_state").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE video_tasks SET execution_state").WithArgs(task.ID, executionJSONContains("retries_exhausted")).WillReturnResult(sqlmock.NewResult(0, 1))
	result, err := runVideoProviderAttempt(context.Background(), videoTaskHandle(), task, 30*time.Second, func(context.Context) (MinimaxVideoTask, error) {
		return MinimaxVideoTask{Status: MinimaxVideoStatusDone, UpstreamID: "provider-task"}, nil
	})
	require.NoError(t, err, "completion must still be saved while billing waits for usage")
	require.Equal(t, MinimaxVideoStatusDone, result.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVideoExecutionRequestUsesEarlierTaskDeadline(t *testing.T) {
	mock := minimaxTaskMockDB(t)
	policy := defaultVideoExecutionPolicy("minimax", MinimaxVideoProtocolLL2)
	policy.RequestTimeoutSeconds = 120
	task := MinimaxVideoTask{ID: "deadline-task", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExecutionPolicy: &policy}
	deadline := time.Now().Add(10 * time.Second).UTC()
	expectVideoExecutionRead(mock, task.ID, VideoExecutionState{DeadlineAt: deadline.Format(time.RFC3339Nano)})
	mock.ExpectExec("UPDATE video_tasks SET execution_state").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE video_tasks SET execution_state").WillReturnResult(sqlmock.NewResult(0, 1))
	_, err := runVideoProviderAttempt(context.Background(), videoTaskHandle(), task, time.Second, func(ctx context.Context) (MinimaxVideoTask, error) {
		actual, ok := ctx.Deadline()
		require.True(t, ok)
		require.Equal(t, deadline, actual, "the task deadline must cap a longer configured request timeout")
		return MinimaxVideoTask{Status: MinimaxVideoStatusProcessing}, nil
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVideoRecoverySlowSupplierDoesNotBlockOtherProviderOrSettlement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := make(chan struct{})
	progress := make(chan string, 2)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		runVideoRecoveryLoops(ctx, time.Hour, []videoRecoveryLoop{
			{name: "slow supplier", timeout: time.Hour, recover: func(c context.Context) error {
				close(started)
				<-c.Done()
				return c.Err()
			}},
			{name: "other supplier", timeout: time.Second, recover: func(c context.Context) error {
				select {
				case <-started:
					progress <- "supplier"
					return nil
				case <-c.Done():
					return c.Err()
				}
			}},
			{name: "settlement", timeout: time.Second, recover: func(c context.Context) error {
				select {
				case <-started:
					progress <- "settlement"
					return nil
				case <-c.Done():
					return c.Err()
				}
			}},
		})
	}()
	observed := map[string]bool{}
	for len(observed) < 2 {
		select {
		case name := <-progress:
			observed[name] = true
		case <-ctx.Done():
			t.Fatal("a slow supplier blocked another recovery loop")
		}
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("recovery workers did not stop with the service context")
	}
}

type executionJSONContains string

func (s executionJSONContains) Match(value driver.Value) bool {
	text, ok := value.(string)
	return ok && strings.Contains(text, string(s))
}
