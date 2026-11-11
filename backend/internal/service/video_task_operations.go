package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

var (
	ErrVideoOperationConflict = errors.New("video task changed or cannot be resumed; refresh and retry")
	ErrVideoOperationNotFound = errors.New("video task not found")
	ErrVideoOperationInvalid  = errors.New("invalid video operation")
)

// Procurement is administrative bookkeeping in the site's balance units. It
// must never be used as an input to user billing or overwrite provider usage.
type VideoTaskProcurement struct {
	EstimatedCost        *float64   `json:"estimated_cost"`
	ActualCost           *float64   `json:"actual_cost"`
	Revision             int64      `json:"revision"`
	Reference            string     `json:"reference"`
	Note                 string     `json:"note"`
	UpdatedBy            *int64     `json:"updated_by"`
	UpdatedAt            *time.Time `json:"updated_at"`
	ReconciliationStatus string     `json:"reconciliation_status"`
}

type VideoTaskOperationAudit struct {
	ID        int64           `json:"id"`
	Action    string          `json:"action"`
	ActorID   int64           `json:"actor_id"`
	Before    json.RawMessage `json:"before"`
	After     json.RawMessage `json:"after"`
	Note      string          `json:"note"`
	CreatedAt time.Time       `json:"created_at"`
}

// Only the admin handlers return this type; normal task APIs retain their
// existing DTO and do not expose purchase records, operators or retry errors.
type AdminVideoTaskRecord struct {
	VideoTaskRecord
	Procurement            VideoTaskProcurement      `json:"procurement"`
	Execution              VideoExecutionState       `json:"execution"`
	ExceptionCodes         []string                  `json:"exception_codes"`
	CanRetry               bool                      `json:"can_retry"`
	RetryUnavailableReason string                    `json:"retry_unavailable_reason,omitempty"`
	OperationsHistory      []VideoTaskOperationAudit `json:"operations_history,omitempty"`
}

type AdminVideoTaskFilter struct {
	VideoTaskFilter
	Exception            string
	ReconciliationStatus string
}

type VideoProcurementStats struct {
	EstimatedCostTotal     float64  `json:"estimated_cost_total"`
	ActualCostTotal        float64  `json:"actual_cost_total"`
	ActualCostCount        int64    `json:"actual_cost_count"`
	PendingCount           int64    `json:"pending_count"`
	ReconciledRevenue      float64  `json:"reconciled_revenue"`
	ConfirmedMarginPercent *float64 `json:"confirmed_margin_percent"`
}

type AdminVideoStats struct {
	Stats           VideoTaskStats        `json:"stats"`
	MarginPercent   *float64              `json:"margin_percent"`
	Procurement     VideoProcurementStats `json:"procurement"`
	ExceptionCounts map[string]int64      `json:"exception_counts"`
}

const videoAdminFrom = ` FROM video_tasks t LEFT JOIN video_task_procurement p ON p.task_id=t.task_id `

// A terminal video missing provider metadata is still recoverable. Timeouts
// indicate local automatic processing has paused, not provider cancellation.
const videoExceptionSQL = `array_remove(ARRAY[
 CASE WHEN (t.status IN ('queued','processing') OR (t.status='done' AND t.settled_at IS NULL)) AND (t.execution_state->>'paused_reason'='task_timeout' OR
   (t.status IN ('queued','processing') AND
    COALESCE(NULLIF(t.execution_state->>'deadline_at','')::timestamptz,
     t.created_at + COALESCE((t.task_snapshot->'execution_policy'->>'task_timeout_seconds')::int,
      CASE WHEN t.provider='seedance' THEN 172800 ELSE 1200 END)*interval '1 second') < now())) THEN 'timeout' END,
 CASE WHEN t.status IN ('queued','processing') AND (t.execution_state->>'paused_reason' IN ('submission_uncertain','submission_expired') OR
   (t.status IN ('queued','processing') AND t.upstream_id='' AND
    COALESCE((t.task_snapshot->>'create_started')::boolean,false) AND
    t.created_at < now()-COALESCE((t.task_snapshot->'execution_policy'->>'request_timeout_seconds')::int,
      CASE WHEN t.provider='seedance' THEN 45 WHEN t.task_snapshot->>'upstream_protocol'='ll2' THEN 30 ELSE 1200 END)*interval '1 second')) THEN 'submission_unknown' END,
 CASE WHEN (t.status IN ('queued','processing') OR (t.status='done' AND t.settled_at IS NULL)) AND t.execution_state->>'paused_reason' IN ('retries_exhausted','retry_disabled') THEN 'retry_exhausted' END,
 CASE WHEN t.status='done' AND t.settled_at IS NULL AND
   ((t.billing_mode='usage_tokens' AND NOT COALESCE((t.task_snapshot->>'usage_reported')::boolean,false)) OR
    (t.billing_mode='upstream_cost' AND NOT (COALESCE((t.task_snapshot->>'upstream_cost_reported')::boolean,false) OR t.upstream_cost_usd>0)) OR
    (t.task_snapshot->>'upstream_protocol'='ll2' AND NOT COALESCE((t.task_snapshot->>'output_seconds_reported')::boolean,false))) THEN 'missing_usage' END,
 CASE WHEN t.status='done' AND t.settled_at IS NULL AND t.task_snapshot IS NOT NULL THEN 'settlement_pending' END,
 CASE WHEN p.actual_cost IS NULL AND (t.status IN ('done','error') OR COALESCE(t.execution_state->>'paused_reason','')<>'') THEN 'reconciliation_pending' END,
 CASE WHEN (t.status IN ('queued','processing') OR (t.status='done' AND t.settled_at IS NULL)) AND t.execution_state->>'paused_reason'='configuration_changed' THEN 'configuration_error' END
]::text[],NULL)`

const videoProcurementJSON = `jsonb_build_object('estimated_cost',p.estimated_cost,'actual_cost',p.actual_cost,
 'revision',COALESCE(p.revision,0),'reference',COALESCE(p.reference,''),'note',COALESCE(p.note,''),
 'updated_by',p.updated_by,'updated_at',p.updated_at,
 'reconciliation_status',CASE WHEN p.actual_cost IS NULL THEN 'pending' ELSE 'reconciled' END)`

// Match ResumeVideoTaskExecution: a normal active task cannot reset its retry
// budget. The first explicit resume of done/unsettled work is allowed so an
// operator can request a metadata recovery cycle after inspecting the task.
const videoAdminRetryNeededSQL = `(COALESCE(t.execution_state->>'paused_reason','')<>'' OR
 COALESCE((t.execution_state->>'failure_count')::int,0)>0 OR
 COALESCE((t.execution_state->>'attempt_in_progress')::boolean,false) OR
 (t.status='done' AND COALESCE((t.execution_state->>'retry_generation')::int,0)=0) OR
 COALESCE(NULLIF(t.execution_state->>'deadline_at','')::timestamptz,
  t.created_at + COALESCE((t.task_snapshot->'execution_policy'->>'task_timeout_seconds')::int,
   CASE WHEN t.provider='seedance' THEN 172800 ELSE 1200 END)*interval '1 second')<=now())`

// Explicit additions reconstruct calculated public fields. Decoding into the
// existing DTO drops internal columns; raw snapshots never reach an HTTP body.
const videoAdminRecordJSON = `(to_jsonb(t)-'task_snapshot'-'execution_state'-'video_url') || jsonb_build_object(
 'input_tokens',COALESCE((t.task_snapshot->>'input_tokens')::int,0),
 'output_tokens',COALESCE((t.task_snapshot->>'output_tokens')::int,0),
 'billing_status',CASE WHEN t.settled_at IS NOT NULL THEN 'settled' WHEN t.status='error' THEN 'not_charged' WHEN t.task_snapshot IS NULL THEN 'legacy' ELSE 'pending' END,
 'upstream_cost_reported',COALESCE((t.task_snapshot->>'upstream_cost_reported')::boolean,false) OR t.upstream_cost_usd>0,
 'user_email',COALESCE(u.email,''),'group_name',COALESCE(g.name,''),'account_name',COALESCE(a.name,''),'api_key_name',COALESCE(k.name,''),
 'can_retry',t.deleted_at IS NULL AND t.settled_at IS NULL AND t.status<>'error' AND t.task_snapshot IS NOT NULL AND ` + videoAdminRetryNeededSQL + ` AND
  (t.upstream_id<>'' OR (t.task_snapshot->>'upstream_protocol'='ll2' AND t.created_at>now()-interval '24 hours' AND t.task_snapshot->'upstream_request_body' IS NOT NULL)),
 'retry_unavailable_reason',CASE WHEN t.deleted_at IS NOT NULL THEN 'deleted' WHEN t.settled_at IS NOT NULL OR t.status='error' THEN 'completed'
  WHEN t.task_snapshot IS NULL THEN 'no_snapshot'
  WHEN NOT ` + videoAdminRetryNeededSQL + ` THEN 'already_active' WHEN t.upstream_id<>'' THEN ''
  WHEN t.task_snapshot->>'upstream_protocol'='ll2' AND t.created_at<=now()-interval '24 hours' THEN 'submission_expired'
  WHEN t.task_snapshot->>'upstream_protocol'='ll2' AND t.task_snapshot->'upstream_request_body' IS NOT NULL THEN '' ELSE 'submission_unknown' END)`

func videoAdminWhere(f AdminVideoTaskFilter) (string, []any, error) {
	where, args := []string{"TRUE"}, []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if !f.IncludeDeleted {
		where = append(where, "t.deleted_at IS NULL")
	}
	for _, v := range []struct {
		column string
		value  int64
	}{{"user_id", f.UserID}, {"group_id", f.GroupID}, {"account_id", f.AccountID}} {
		if v.value > 0 {
			add("t."+v.column+"=$%d", v.value)
		}
	}
	for _, v := range []struct{ column, value string }{{"status", f.Status}, {"model", f.Model}, {"provider", f.Provider}} {
		if strings.TrimSpace(v.value) != "" {
			add("t."+v.column+"=$%d", strings.TrimSpace(v.value))
		}
	}
	if f.BillingOnly {
		where = append(where, "t.status IN ('done','error')")
	}
	if v := strings.TrimSpace(f.Search); v != "" {
		add("(lower(t.task_id) LIKE $%[1]d OR lower(t.prompt) LIKE $%[1]d)", "%"+strings.ToLower(v)+"%")
	}
	if f.From != nil {
		add("t.created_at >= $%d", *f.From)
	}
	if f.To != nil {
		add("t.created_at <= $%d", *f.To)
	}
	switch f.ReconciliationStatus {
	case "":
	case "pending":
		where = append(where, "p.actual_cost IS NULL")
	case "reconciled":
		where = append(where, "p.actual_cost IS NOT NULL")
	default:
		return "", nil, fmt.Errorf("%w: unknown reconciliation status", ErrVideoOperationInvalid)
	}
	switch f.Exception {
	case "":
	case "any":
		where = append(where, "cardinality("+videoExceptionSQL+")>0")
	case "timeout", "submission_unknown", "retry_exhausted", "missing_usage", "settlement_pending", "reconciliation_pending", "configuration_error":
		add("$%d = ANY("+videoExceptionSQL+")", f.Exception)
	default:
		return "", nil, fmt.Errorf("%w: unknown exception", ErrVideoOperationInvalid)
	}
	return " WHERE " + strings.Join(where, " AND "), args, nil
}

func scanAdminVideoTask(rows *sql.Rows) (AdminVideoTaskRecord, error) {
	var rec AdminVideoTaskRecord
	var task, procurement, execution, exceptions []byte
	if err := rows.Scan(&task, &procurement, &execution, &exceptions); err != nil {
		return rec, err
	}
	for _, pair := range []struct {
		data   []byte
		target any
	}{{task, &rec}, {procurement, &rec.Procurement}, {execution, &rec.Execution}, {exceptions, &rec.ExceptionCodes}} {
		if err := json.Unmarshal(pair.data, pair.target); err != nil {
			return rec, err
		}
	}
	return rec, nil
}

const videoAdminJoins = ` LEFT JOIN users u ON u.id=t.user_id LEFT JOIN groups g ON g.id=t.group_id
 LEFT JOIN accounts a ON a.id=t.account_id LEFT JOIN api_keys k ON k.id=t.api_key_id `

func ListAdminVideoTasks(ctx context.Context, f AdminVideoTaskFilter) ([]AdminVideoTaskRecord, int64, error) {
	db := videoTaskHandle()
	if db == nil {
		return nil, 0, fmt.Errorf("video task database unavailable")
	}
	where, args, err := videoAdminWhere(f)
	if err != nil {
		return nil, 0, err
	}
	var total int64
	if err = db.QueryRowContext(ctx, "SELECT count(*)"+videoAdminFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page, size := f.Page, f.PageSize
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	args = append(args, size, (page-1)*size)
	query := "SELECT " + videoAdminRecordJSON + "," + videoProcurementJSON + ",t.execution_state,to_jsonb(" + videoExceptionSQL + ")" + videoAdminFrom + videoAdminJoins + where + fmt.Sprintf(" ORDER BY t.created_at DESC,t.id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]AdminVideoTaskRecord, 0, size)
	for rows.Next() {
		r, err := scanAdminVideoTask(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func GetAdminVideoTask(ctx context.Context, id string) (*AdminVideoTaskRecord, error) {
	db := videoTaskHandle()
	if db == nil {
		return nil, fmt.Errorf("video task database unavailable")
	}
	rows, err := db.QueryContext(ctx, "SELECT "+videoAdminRecordJSON+","+videoProcurementJSON+",t.execution_state,to_jsonb("+videoExceptionSQL+")"+videoAdminFrom+videoAdminJoins+" WHERE t.task_id=$1", id)
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		err = rows.Err()
		rows.Close()
		return nil, err
	}
	r, err := scanAdminVideoTask(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	history, err := db.QueryContext(ctx, `SELECT id,action,actor_id,before_value,after_value,note,created_at FROM video_task_operations_audit WHERE task_id=$1 ORDER BY id DESC LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	defer history.Close()
	r.OperationsHistory = make([]VideoTaskOperationAudit, 0)
	for history.Next() {
		var a VideoTaskOperationAudit
		if err = history.Scan(&a.ID, &a.Action, &a.ActorID, &a.Before, &a.After, &a.Note, &a.CreatedAt); err != nil {
			return nil, err
		}
		r.OperationsHistory = append(r.OperationsHistory, a)
	}
	return &r, history.Err()
}

func AdminVideoTaskStatsFor(ctx context.Context, f AdminVideoTaskFilter) (AdminVideoStats, error) {
	result := AdminVideoStats{ExceptionCounts: map[string]int64{}}
	db := videoTaskHandle()
	if db == nil {
		return result, fmt.Errorf("video task database unavailable")
	}
	where, args, err := videoAdminWhere(f)
	if err != nil {
		return result, err
	}
	q := `SELECT count(*),count(*) FILTER(WHERE t.status='queued'),count(*) FILTER(WHERE t.status='processing'),
 count(*) FILTER(WHERE t.status='done'),count(*) FILTER(WHERE t.status='error'),count(*) FILTER(WHERE t.created_at>=date_trunc('day',now())),
 COALESCE(sum(t.upstream_cost_usd),0),COALESCE(sum(t.billed_cost_usd),0),
 count(*) FILTER(WHERE t.status='done' AND NOT(COALESCE((t.task_snapshot->>'upstream_cost_reported')::boolean,false) OR t.upstream_cost_usd>0)),
 COALESCE(sum(p.estimated_cost),0),COALESCE(sum(p.actual_cost),0),count(p.actual_cost),count(*) FILTER(WHERE p.actual_cost IS NULL),
 COALESCE(sum(t.billed_cost_usd) FILTER(WHERE p.actual_cost IS NOT NULL),0)` + videoAdminFrom + where
	s, p := &result.Stats, &result.Procurement
	err = db.QueryRowContext(ctx, q, args...).Scan(&s.Total, &s.Queued, &s.Processing, &s.Done, &s.Error, &s.Today, &s.UpstreamCost, &s.BilledCost, &s.UnknownUpstreamCostCount, &p.EstimatedCostTotal, &p.ActualCostTotal, &p.ActualCostCount, &p.PendingCount, &p.ReconciledRevenue)
	if err != nil {
		return result, err
	}
	if finished := s.Done + s.Error; finished > 0 {
		s.SuccessRate = float64(s.Done) / float64(finished) * 100
	}
	if s.BilledCost > 0 && s.UnknownUpstreamCostCount == 0 {
		v := (s.BilledCost - s.UpstreamCost) / s.BilledCost * 100
		result.MarginPercent = &v
	}
	if p.ReconciledRevenue > 0 {
		v := (p.ReconciledRevenue - p.ActualCostTotal) / p.ReconciledRevenue * 100
		p.ConfirmedMarginPercent = &v
	}
	rows, err := db.QueryContext(ctx, "SELECT exception_code,count(*) FROM (SELECT unnest("+videoExceptionSQL+") AS exception_code"+videoAdminFrom+where+") e GROUP BY exception_code", args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var code string
		var count int64
		if err := rows.Scan(&code, &count); err != nil {
			return result, err
		}
		result.ExceptionCounts[code] = count
	}
	return result, rows.Err()
}

// OptionalVideoCost distinguishes omission (preserve), null (clear), and zero
// (confirmed free). It rejects NaN/infinities and excessive amounts explicitly.
type OptionalVideoCost struct {
	Present bool
	Value   *float64
}

func (v *OptionalVideoCost) UnmarshalJSON(data []byte) error {
	v.Present = true
	if string(data) == "null" {
		v.Value = nil
		return nil
	}
	var amount float64
	if err := json.Unmarshal(data, &amount); err != nil {
		return err
	}
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 || amount > 1e9 {
		return fmt.Errorf("cost must be between 0 and 1000000000")
	}
	// Match PostgreSQL NUMERIC(20,8), ensuring the audit equals the saved amount.
	amount = math.Round(amount*1e8) / 1e8
	v.Value = &amount
	return nil
}

type ReconcileVideoTaskInput struct {
	EstimatedCost    OptionalVideoCost `json:"estimated_cost"`
	ActualCost       OptionalVideoCost `json:"actual_cost"`
	Reference        string            `json:"reference"`
	Note             string            `json:"note"`
	ExpectedRevision *int64            `json:"expected_revision"`
}

func (in *ReconcileVideoTaskInput) validate() error {
	in.Reference = strings.TrimSpace(in.Reference)
	in.Note = strings.TrimSpace(in.Note)
	if in.ExpectedRevision == nil || *in.ExpectedRevision < 0 || (!in.EstimatedCost.Present && !in.ActualCost.Present) || in.Note == "" || len(in.Note) > 2000 || len(in.Reference) > 500 {
		return fmt.Errorf("%w: costs, revision and a note (up to 2000 characters) are required", ErrVideoOperationInvalid)
	}
	for _, v := range []OptionalVideoCost{in.EstimatedCost, in.ActualCost} {
		if v.Present && v.Value != nil && (math.IsNaN(*v.Value) || math.IsInf(*v.Value, 0) || *v.Value < 0 || *v.Value > 1e9) {
			return fmt.Errorf("%w: cost outside allowed range", ErrVideoOperationInvalid)
		}
	}
	// Service callers can construct inputs without JSON decoding. Normalize here
	// as well so audit values always equal the NUMERIC(20,8) database values.
	for _, v := range []*OptionalVideoCost{&in.EstimatedCost, &in.ActualCost} {
		if v.Present && v.Value != nil {
			amount := math.Round(*v.Value*1e8) / 1e8
			v.Value = &amount
		}
	}
	if in.ActualCost.Present && in.ActualCost.Value != nil && in.Reference == "" {
		return fmt.Errorf("%w: supplier bill reference is required for actual cost", ErrVideoOperationInvalid)
	}
	return nil
}

func ReconcileVideoTask(ctx context.Context, taskID string, actorID int64, in ReconcileVideoTaskInput) error {
	if actorID <= 0 {
		return ErrVideoOperationInvalid
	}
	if err := in.validate(); err != nil {
		return err
	}
	db := videoTaskHandle()
	if db == nil {
		return fmt.Errorf("video task database unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err = tx.QueryRowContext(ctx, "SELECT task_id FROM video_tasks WHERE task_id=$1 FOR UPDATE", taskID).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return ErrVideoOperationNotFound
	} else if err != nil {
		return err
	}
	var before VideoTaskProcurement
	before.ReconciliationStatus = "pending"
	err = tx.QueryRowContext(ctx, `SELECT estimated_cost,actual_cost,revision,reference,note,updated_by,updated_at FROM video_task_procurement WHERE task_id=$1`, taskID).Scan(&before.EstimatedCost, &before.ActualCost, &before.Revision, &before.Reference, &before.Note, &before.UpdatedBy, &before.UpdatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if before.ActualCost != nil {
		before.ReconciliationStatus = "reconciled"
	}
	if before.Revision != *in.ExpectedRevision {
		return ErrVideoOperationConflict
	}
	after := before
	after.Revision++
	after.Note = in.Note
	at := time.Now().UTC()
	after.UpdatedAt = &at
	after.UpdatedBy = &actorID
	if in.EstimatedCost.Present {
		after.EstimatedCost = in.EstimatedCost.Value
	}
	if in.ActualCost.Present {
		after.ActualCost = in.ActualCost.Value
		after.Reference = in.Reference
	}
	if after.ActualCost == nil {
		after.ReconciliationStatus = "pending"
	} else {
		after.ReconciliationStatus = "reconciled"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO video_task_procurement(task_id,estimated_cost,actual_cost,revision,reference,note,updated_by,updated_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(task_id) DO UPDATE SET estimated_cost=EXCLUDED.estimated_cost,actual_cost=EXCLUDED.actual_cost,
 revision=EXCLUDED.revision,reference=EXCLUDED.reference,note=EXCLUDED.note,updated_by=EXCLUDED.updated_by,updated_at=EXCLUDED.updated_at`, taskID, after.EstimatedCost, after.ActualCost, after.Revision, after.Reference, after.Note, actorID, at)
	if err != nil {
		return err
	}
	beforeJSON, err := json.Marshal(before)
	if err != nil {
		return err
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO video_task_operations_audit(task_id,action,actor_id,before_value,after_value,note) VALUES($1,'reconcile',$2,$3::jsonb,$4::jsonb,$5)`, taskID, actorID, string(beforeJSON), string(afterJSON), in.Note)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Administrative termination must be attributable. It stops local processing
// only; the provider may still complete or charge, so procurement remains open.
func FailVideoTaskWithAudit(ctx context.Context, taskID string, actorID int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if actorID <= 0 || reason == "" || len(reason) > 1000 {
		return ErrVideoOperationInvalid
	}
	db := videoTaskHandle()
	if db == nil {
		return fmt.Errorf("video task database unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, previousError string
	var settled, deleted *time.Time
	err = tx.QueryRowContext(ctx, `SELECT status,error_message,settled_at,deleted_at FROM video_tasks WHERE task_id=$1 FOR UPDATE`, taskID).Scan(&status, &previousError, &settled, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrVideoOperationNotFound
	}
	if err != nil {
		return err
	}
	if (status != MinimaxVideoStatusQueued && status != MinimaxVideoStatusProcessing) || settled != nil || deleted != nil {
		return ErrVideoOperationConflict
	}
	before, _ := json.Marshal(map[string]string{"status": status, "error_message": previousError})
	after, _ := json.Marshal(map[string]string{"status": MinimaxVideoStatusError, "error_message": reason})
	if _, err = tx.ExecContext(ctx, `UPDATE video_tasks SET status='error',error_message=$2,completed_at=now() WHERE task_id=$1`, taskID, reason); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO video_task_operations_audit(task_id,action,actor_id,before_value,after_value,note) VALUES($1,'fail',$2,$3::jsonb,$4::jsonb,$5)`, taskID, actorID, string(before), string(after), reason); err != nil {
		return err
	}
	return tx.Commit()
}
