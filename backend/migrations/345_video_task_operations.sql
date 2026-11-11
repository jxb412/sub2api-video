-- Operational recovery state is separate from provider snapshots so a late
-- worker cannot erase retry limits. Procurement records never modify billing.
ALTER TABLE video_tasks ADD COLUMN IF NOT EXISTS execution_state JSONB NOT NULL DEFAULT '{}';

CREATE TABLE IF NOT EXISTS video_task_procurement (
    task_id VARCHAR(64) PRIMARY KEY REFERENCES video_tasks(task_id),
    estimated_cost NUMERIC(20,8),
    actual_cost NUMERIC(20,8),
    revision BIGINT NOT NULL DEFAULT 0,
    reference TEXT NOT NULL DEFAULT '',
    note TEXT NOT NULL DEFAULT '',
    updated_by BIGINT,
    updated_at TIMESTAMPTZ,
    CHECK (estimated_cost IS NULL OR (estimated_cost >= 0 AND estimated_cost <= 1000000000)),
    CHECK (actual_cost IS NULL OR (actual_cost >= 0 AND actual_cost <= 1000000000))
);

CREATE TABLE IF NOT EXISTS video_task_operations_audit (
    id BIGSERIAL PRIMARY KEY,
    task_id VARCHAR(64) NOT NULL REFERENCES video_tasks(task_id),
    action VARCHAR(32) NOT NULL,
    actor_id BIGINT NOT NULL,
    before_value JSONB NOT NULL,
    after_value JSONB NOT NULL,
    note TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_video_task_operations_audit_task
    ON video_task_operations_audit(task_id, id DESC);
