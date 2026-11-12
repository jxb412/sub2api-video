package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestVideoProcurementInputSeparatesUnknownAndZero(t *testing.T) {
	for _, tc := range []struct {
		body    string
		wantErr bool
		present bool
		value   *float64
	}{
		{`{"estimated_cost":1.234567891,"expected_revision":0,"note":"price list estimate"}`, false, false, nil},
		{`{"actual_cost":null,"expected_revision":1,"note":"undo unverified debit"}`, false, true, nil},
		{`{"actual_cost":0,"reference":"bill-free","expected_revision":0,"note":"supplier confirmed free"}`, false, true, func() *float64 { v := 0.0; return &v }()},
		{`{"actual_cost":0,"expected_revision":0,"note":"no evidence"}`, true, false, nil},
		{`{"actual_cost":1,"reference":"bill","note":"missing revision"}`, true, false, nil},
		{`{"actual_cost":-1,"reference":"bill","expected_revision":0,"note":"negative"}`, true, false, nil},
		{`{"actual_cost":1000000001,"reference":"bill","expected_revision":0,"note":"over limit"}`, true, false, nil},
		{`{"estimated_cost":"1","expected_revision":0,"note":"string"}`, true, false, nil},
		{`{"expected_revision":0,"note":"no cost operation"}`, true, false, nil},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var in ReconcileVideoTaskInput
			err := json.Unmarshal([]byte(tc.body), &in)
			if err == nil {
				err = in.validate()
			}
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.present, in.ActualCost.Present)
			require.Equal(t, tc.value, in.ActualCost.Value)
			if in.EstimatedCost.Value != nil {
				require.Equal(t, 1.23456789, *in.EstimatedCost.Value)
			}
		})
	}
}

func TestVideoProcurementValidationRejectsNonFiniteServiceInput(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		rev := int64(0)
		in := ReconcileVideoTaskInput{EstimatedCost: OptionalVideoCost{Present: true, Value: &v}, Note: "estimate", ExpectedRevision: &rev}
		require.ErrorIs(t, in.validate(), ErrVideoOperationInvalid)
	}
}

func videoOperationMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	old := videoTaskHandle()
	SetVideoTaskDB(db)
	t.Cleanup(func() { SetVideoTaskDB(old); require.NoError(t, mock.ExpectationsWereMet()); db.Close() })
	return db, mock
}

type videoProcurementAuditArg struct {
	actual   *float64
	revision int64
}

func (a videoProcurementAuditArg) Match(value driver.Value) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	var rec VideoTaskProcurement
	if json.Unmarshal([]byte(s), &rec) != nil || rec.Revision != a.revision {
		return false
	}
	if a.actual == nil {
		return rec.ActualCost == nil
	}
	return rec.ActualCost != nil && *rec.ActualCost == *a.actual
}

func TestVideoReconciliationAtomicAuditAndNoBillingWrites(t *testing.T) {
	for _, failAudit := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "audit_rollback"}[failAudit], func(t *testing.T) {
			_, mock := videoOperationMockDB(t)
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT task_id FROM video_tasks.*FOR UPDATE").WithArgs("task-1").WillReturnRows(sqlmock.NewRows([]string{"task_id"}).AddRow("task-1"))
			mock.ExpectQuery("SELECT estimated_cost,actual_cost,revision").WithArgs("task-1").WillReturnRows(sqlmock.NewRows([]string{"estimated", "actual", "revision", "reference", "note", "actor", "at"}))
			zero := 0.0
			rev := int64(0)
			mock.ExpectExec("INSERT INTO video_task_procurement").WithArgs("task-1", nil, zero, int64(1), "bill-123", "confirmed free", int64(99), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
			audit := mock.ExpectExec("INSERT INTO video_task_operations_audit").WithArgs("task-1", int64(99), videoProcurementAuditArg{nil, 0}, videoProcurementAuditArg{&zero, 1}, "confirmed free")
			if failAudit {
				audit.WillReturnError(errors.New("audit unavailable"))
				mock.ExpectRollback()
			} else {
				audit.WillReturnResult(sqlmock.NewResult(1, 1))
				mock.ExpectCommit()
			}
			err := ReconcileVideoTask(context.Background(), "task-1", 99, ReconcileVideoTaskInput{ActualCost: OptionalVideoCost{Present: true, Value: &zero}, Reference: "bill-123", Note: "confirmed free", ExpectedRevision: &rev})
			if failAudit {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestVideoReconciliationRejectsStaleRevision(t *testing.T) {
	_, mock := videoOperationMockDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT task_id FROM video_tasks.*FOR UPDATE").WithArgs("task-1").WillReturnRows(sqlmock.NewRows([]string{"task_id"}).AddRow("task-1"))
	mock.ExpectQuery("SELECT estimated_cost,actual_cost,revision").WithArgs("task-1").WillReturnRows(sqlmock.NewRows([]string{"estimated", "actual", "revision", "reference", "note", "actor", "at"}).AddRow(2.0, 1.0, int64(2), "bill", "previous", int64(9), time.Now()))
	mock.ExpectRollback()
	revision := int64(1)
	err := ReconcileVideoTask(context.Background(), "task-1", 9, ReconcileVideoTaskInput{ActualCost: OptionalVideoCost{Present: true}, Note: "clear outdated", ExpectedRevision: &revision})
	require.ErrorIs(t, err, ErrVideoOperationConflict)
}

func TestVideoAdminFiltersRejectUnknownValuesAndBindInputs(t *testing.T) {
	_, _, err := videoAdminWhere(AdminVideoTaskFilter{Exception: "'; DELETE FROM users;--"})
	require.ErrorIs(t, err, ErrVideoOperationInvalid)
	_, _, err = videoAdminWhere(AdminVideoTaskFilter{ReconciliationStatus: "bogus"})
	require.ErrorIs(t, err, ErrVideoOperationInvalid)
	where, args, err := videoAdminWhere(AdminVideoTaskFilter{VideoTaskFilter: VideoTaskFilter{AccountID: 125, GroupID: 4, Search: "A'B"}, Exception: "missing_usage", ReconciliationStatus: "pending"})
	require.NoError(t, err)
	require.NotContains(t, where, "A'B")
	require.Contains(t, where, "p.actual_cost IS NULL")
	require.Contains(t, where, "$4 = ANY(")
	require.Equal(t, []any{int64(4), int64(125), "%a'b%", "missing_usage"}, args)
}

func TestVideoFailIsAuditedAtomicallyWithoutBillingChanges(t *testing.T) {
	_, mock := videoOperationMockDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT status,error_message,settled_at,deleted_at FROM video_tasks.*FOR UPDATE").WithArgs("task-1").WillReturnRows(sqlmock.NewRows([]string{"status", "error", "settled", "deleted"}).AddRow("processing", "", nil, nil))
	mock.ExpectExec("UPDATE video_tasks SET status='error',error_message=\\$2,completed_at=now").WithArgs("task-1", "confirmed by supplier").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO video_task_operations_audit").WithArgs("task-1", int64(7), `{"error_message":"","status":"processing"}`, `{"error_message":"confirmed by supplier","status":"error"}`, "confirmed by supplier").WillReturnError(errors.New("audit offline"))
	mock.ExpectRollback()
	require.Error(t, FailVideoTaskWithAudit(context.Background(), "task-1", 7, "confirmed by supplier"))
}

func TestAdminVideoDTOCannotExposeProviderSnapshot(t *testing.T) {
	_, mock := videoOperationMockDB(t)
	mock.ExpectQuery("SELECT").WillReturnRows(sqlmock.NewRows([]string{"task", "procurement", "execution", "exceptions"}).AddRow(
		`{"task_id":"task-1","provider":"minimax","can_retry":false,"retry_unavailable_reason":"completed","task_snapshot":{"upstream_credential_hash":"secret","upstream_request_body":{"image_url":"private"}},"video_url":"https://private.example"}`,
		`{"actual_cost":null,"estimated_cost":1,"revision":0,"reconciliation_status":"pending"}`, `{}`, `["reconciliation_pending"]`))
	rows, err := videoTaskHandle().QueryContext(context.Background(), "SELECT")
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	rec, err := scanAdminVideoTask(rows)
	require.NoError(t, err)
	payload, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NotContains(t, string(payload), "secret")
	require.NotContains(t, string(payload), "private.example")
	require.NotContains(t, string(payload), "image_url")
	require.Equal(t, "completed", rec.RetryUnavailableReason)
	require.Nil(t, rec.Procurement.ActualCost)
}
