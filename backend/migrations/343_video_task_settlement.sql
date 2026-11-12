-- Preserve the immutable billing inputs independently of Redis. Existing rows
-- intentionally have no snapshot and are never charged retroactively.
ALTER TABLE video_tasks ADD COLUMN IF NOT EXISTS task_snapshot JSONB;
ALTER TABLE video_tasks ADD COLUMN IF NOT EXISTS settled_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_video_tasks_unsettled
    ON video_tasks (created_at) WHERE status = 'done' AND settled_at IS NULL AND task_snapshot IS NOT NULL;
