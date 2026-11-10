package service

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Video task persistence.
//
// The gateway keeps hot task state in the cache so status polls stay fast, but
// history, billing and admin queries need durable rows. A package-level handle
// keeps the wiring tiny: main calls SetVideoTaskDB once after the pool is ready,
// and every layer below shares it. When it is nil every write becomes a no-op
// and reads return empty results, so a deployment without the migration keeps
// serving traffic instead of failing requests.

var (
	videoTaskDBMu sync.RWMutex
	videoTaskDB   *sql.DB
)

// SetVideoTaskDB installs the pool used by the video task store.
func SetVideoTaskDB(db *sql.DB) {
	videoTaskDBMu.Lock()
	videoTaskDB = db
	videoTaskDBMu.Unlock()
}

func videoTaskHandle() *sql.DB {
	videoTaskDBMu.RLock()
	defer videoTaskDBMu.RUnlock()
	return videoTaskDB
}

// VideoTaskRecord is one row of video_tasks.
type VideoTaskRecord struct {
	ID              int64      `json:"-"`
	TaskID          string     `json:"task_id"`
	UpstreamID      string     `json:"upstream_id,omitempty"`
	Provider        string     `json:"provider"`
	Model           string     `json:"model"`
	UpstreamModel   string     `json:"upstream_model,omitempty"`
	Prompt          string     `json:"prompt"`
	Status          string     `json:"status"`
	Resolution      string     `json:"resolution,omitempty"`
	DurationSeconds int        `json:"duration_seconds"`
	UserID          int64      `json:"user_id"`
	APIKeyID        int64      `json:"api_key_id"`
	GroupID         int64      `json:"group_id"`
	AccountID       int64      `json:"account_id"`
	VideoURL        string     `json:"-"`
	ErrorMessage    string     `json:"error_message,omitempty"`
	UpstreamCostUSD float64    `json:"upstream_cost_usd"`
	BilledCostUSD   float64    `json:"billed_cost_usd"`
	BillingMode     string     `json:"billing_mode"`
	CreatedAt       time.Time  `json:"created_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	DeletedAt       *time.Time `json:"deleted_at,omitempty"`
	// Display fields joined at query time.
	UserEmail   string `json:"user_email,omitempty"`
	GroupName   string `json:"group_name,omitempty"`
	AccountName string `json:"account_name,omitempty"`
	APIKeyName  string `json:"api_key_name,omitempty"`
}

// VideoTaskFilter narrows a list query. Zero values mean "no constraint".
type VideoTaskFilter struct {
	UserID   int64
	GroupID  int64
	AccountID int64
	Status   string
	Model    string
	Search   string
	From     *time.Time
	To       *time.Time
	Page     int
	PageSize int
	IncludeDeleted bool
}

// VideoTaskStats is the aggregate shown on both overview cards.
type VideoTaskStats struct {
	Total         int64   `json:"total"`
	Queued        int64   `json:"queued"`
	Processing    int64   `json:"processing"`
	Done          int64   `json:"done"`
	Error         int64   `json:"error"`
	Today         int64   `json:"today"`
	UpstreamCost  float64 `json:"upstream_cost_usd"`
	BilledCost    float64 `json:"billed_cost_usd"`
	SuccessRate   float64 `json:"success_rate"`
}

const videoTaskColumns = `task_id, upstream_id, provider, model, upstream_model, prompt, status,
	resolution, duration_seconds, user_id, api_key_id, group_id, account_id, video_url,
	error_message, upstream_cost_usd, billed_cost_usd, billing_mode, created_at, completed_at, deleted_at`

func scanVideoTask(rows *sql.Rows, withJoins bool) (VideoTaskRecord, error) {
	var r VideoTaskRecord
	dest := []any{
		&r.TaskID, &r.UpstreamID, &r.Provider, &r.Model, &r.UpstreamModel, &r.Prompt, &r.Status,
		&r.Resolution, &r.DurationSeconds, &r.UserID, &r.APIKeyID, &r.GroupID, &r.AccountID, &r.VideoURL,
		&r.ErrorMessage, &r.UpstreamCostUSD, &r.BilledCostUSD, &r.BillingMode, &r.CreatedAt, &r.CompletedAt, &r.DeletedAt,
	}
	if withJoins {
		dest = append(dest, &r.UserEmail, &r.GroupName, &r.AccountName, &r.APIKeyName)
	}
	err := rows.Scan(dest...)
	return r, err
}

// CreateVideoTask inserts a queued task. Safe to call with a nil pool.
func CreateVideoTask(ctx context.Context, task VideoTaskRecord) error {
	db := videoTaskHandle()
	if db == nil || strings.TrimSpace(task.TaskID) == "" {
		return nil
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now().UTC()
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO video_tasks (
			task_id, provider, model, upstream_model, prompt, status, resolution, duration_seconds,
			user_id, api_key_id, group_id, account_id, billing_mode, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (task_id) DO NOTHING`,
		task.TaskID, task.Provider, task.Model, task.UpstreamModel, task.Prompt, task.Status,
		task.Resolution, task.DurationSeconds, task.UserID, task.APIKeyID, task.GroupID, task.AccountID,
		task.BillingMode, task.CreatedAt)
	return err
}

// CompleteVideoTask writes the terminal state plus the cost figures.
func CompleteVideoTask(ctx context.Context, taskID, upstreamID, status, resolution, videoURL, errMsg string,
	durationSeconds int, upstreamCost, billedCost float64, billingMode string) error {
	db := videoTaskHandle()
	if db == nil || strings.TrimSpace(taskID) == "" {
		return nil
	}
	var completed any
	if status == MinimaxVideoStatusDone || status == MinimaxVideoStatusError {
		completed = time.Now().UTC()
	}
	_, err := db.ExecContext(ctx, `
		UPDATE video_tasks SET
			status = $2,
			upstream_id = COALESCE(NULLIF($3,''), upstream_id),
			resolution = COALESCE(NULLIF($4,''), resolution),
			video_url = COALESCE(NULLIF($5,''), video_url),
			error_message = $6,
			duration_seconds = GREATEST(duration_seconds, $7),
			upstream_cost_usd = $8,
			billed_cost_usd = $9,
			billing_mode = COALESCE(NULLIF($10,''), billing_mode),
			completed_at = COALESCE($11, completed_at)
		WHERE task_id = $1`,
		taskID, status, upstreamID, resolution, videoURL, errMsg, durationSeconds,
		upstreamCost, billedCost, billingMode, completed)
	return err
}

// GetVideoTask loads one task scoped to its owner (userID <= 0 means admin).
func GetVideoTask(ctx context.Context, taskID string, userID int64) (*VideoTaskRecord, error) {
	db := videoTaskHandle()
	if db == nil || strings.TrimSpace(taskID) == "" {
		return nil, nil
	}
	query := `SELECT ` + videoTaskColumns + ` FROM video_tasks WHERE task_id = $1 AND deleted_at IS NULL`
	args := []any{taskID}
	if userID > 0 {
		query += ` AND user_id = $2`
		args = append(args, userID)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, nil
	}
	rec, err := scanVideoTask(rows, false)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// DeleteVideoTask soft-deletes a task.
func DeleteVideoTask(ctx context.Context, taskID string, userID int64) (bool, error) {
	db := videoTaskHandle()
	if db == nil || strings.TrimSpace(taskID) == "" {
		return false, nil
	}
	query := `UPDATE video_tasks SET deleted_at = now() WHERE task_id = $1 AND deleted_at IS NULL`
	args := []any{taskID}
	if userID > 0 {
		query += ` AND user_id = $2`
		args = append(args, userID)
	}
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListVideoTasks returns a page of tasks plus the total count.
func ListVideoTasks(ctx context.Context, f VideoTaskFilter) ([]VideoTaskRecord, int64, error) {
	db := videoTaskHandle()
	if db == nil {
		return nil, 0, nil
	}
	where := []string{}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if !f.IncludeDeleted {
		where = append(where, "t.deleted_at IS NULL")
	}
	if f.UserID > 0 {
		add("t.user_id = $%d", f.UserID)
	}
	if f.GroupID > 0 {
		add("t.group_id = $%d", f.GroupID)
	}
	if f.AccountID > 0 {
		add("t.account_id = $%d", f.AccountID)
	}
	if strings.TrimSpace(f.Status) != "" {
		add("t.status = $%d", strings.TrimSpace(f.Status))
	}
	if strings.TrimSpace(f.Model) != "" {
		add("t.model = $%d", strings.TrimSpace(f.Model))
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		args = append(args, "%"+strings.ToLower(s)+"%")
		idx := len(args)
		where = append(where, fmt.Sprintf("(lower(t.task_id) LIKE $%d OR lower(t.prompt) LIKE $%d)", idx, idx))
	}
	if f.From != nil {
		add("t.created_at >= $%d", *f.From)
	}
	if f.To != nil {
		add("t.created_at <= $%d", *f.To)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM video_tasks t`+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	page, size := f.Page, f.PageSize
	if page <= 0 {
		page = 1
	}
	if size <= 0 || size > 200 {
		size = 20
	}
	args = append(args, size, (page-1)*size)
	query := fmt.Sprintf(`
		SELECT %s,
		       COALESCE(u.email,''), COALESCE(g.name,''), COALESCE(a.name,''), COALESCE(k.name,'')
		FROM video_tasks t
		LEFT JOIN users u ON u.id = t.user_id
		LEFT JOIN groups g ON g.id = t.group_id
		LEFT JOIN accounts a ON a.id = t.account_id
		LEFT JOIN api_keys k ON k.id = t.api_key_id
		%s
		ORDER BY t.created_at DESC
		LIMIT $%d OFFSET $%d`, videoTaskColumns, clause, len(args)-1, len(args))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]VideoTaskRecord, 0, size)
	for rows.Next() {
		rec, err := scanVideoTask(rows, true)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, rec)
	}
	return out, total, rows.Err()
}

// VideoTaskStatsFor aggregates counts and costs for a filter.
func VideoTaskStatsFor(ctx context.Context, f VideoTaskFilter) (VideoTaskStats, error) {
	var stats VideoTaskStats
	db := videoTaskHandle()
	if db == nil {
		return stats, nil
	}
	where := []string{"deleted_at IS NULL"}
	args := []any{}
	if f.UserID > 0 {
		args = append(args, f.UserID)
		where = append(where, fmt.Sprintf("user_id = $%d", len(args)))
	}
	if f.GroupID > 0 {
		args = append(args, f.GroupID)
		where = append(where, fmt.Sprintf("group_id = $%d", len(args)))
	}
	if f.From != nil {
		args = append(args, *f.From)
		where = append(where, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	clause := " WHERE " + strings.Join(where, " AND ")
	query := `SELECT
		count(*),
		count(*) FILTER (WHERE status = 'queued'),
		count(*) FILTER (WHERE status = 'processing'),
		count(*) FILTER (WHERE status = 'done'),
		count(*) FILTER (WHERE status = 'error'),
		count(*) FILTER (WHERE created_at >= date_trunc('day', now())),
		COALESCE(sum(upstream_cost_usd),0),
		COALESCE(sum(billed_cost_usd),0)
		FROM video_tasks` + clause
	if err := db.QueryRowContext(ctx, query, args...).Scan(
		&stats.Total, &stats.Queued, &stats.Processing, &stats.Done, &stats.Error,
		&stats.Today, &stats.UpstreamCost, &stats.BilledCost); err != nil {
		return stats, err
	}
	finished := stats.Done + stats.Error
	if finished > 0 {
		stats.SuccessRate = float64(stats.Done) / float64(finished) * 100
	}
	return stats, nil
}

// UpdateVideoTaskBilling records the settled cost once the video is delivered.
func UpdateVideoTaskBilling(ctx context.Context, taskID string, upstreamCost, billedCost float64, mode string) error {
	db := videoTaskHandle()
	if db == nil || strings.TrimSpace(taskID) == "" {
		return nil
	}
	_, err := db.ExecContext(ctx, `
		UPDATE video_tasks
		   SET upstream_cost_usd = GREATEST(upstream_cost_usd, $2),
		       billed_cost_usd = $3,
		       billing_mode = COALESCE(NULLIF($4,''), billing_mode)
		 WHERE task_id = $1`, taskID, upstreamCost, billedCost, mode)
	return err
}
