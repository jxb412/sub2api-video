-- 视频生成任务（MiniMax H3 / Seedance 等异步视频模型）
--
-- 设计要点：
--  1. 网关先落库再返回 task id，Redis 只做热态缓存，保证重启/过期后仍可查历史。
--  2. 归属字段（user_id / api_key_id / group_id / account_id）齐全，用户端只能看到自己的，
--     管理端可按用户/分组/账号/状态/模型检索。
--  3. 计费双模式共用同一批列：local_price 写 billed_cost_usd（我方定价），
--     upstream_cost 写 billed_cost_usd（跟随上游），upstream_cost_usd 永远记录上游真实成本，
--     便于两种口径对账。
--  4. 软删除：deleted_at 置位后用户端不可见，管理端仍可审计。

CREATE TABLE IF NOT EXISTS video_tasks (
    id                  BIGSERIAL PRIMARY KEY,
    task_id             VARCHAR(64)  NOT NULL,
    upstream_id         VARCHAR(128) NOT NULL DEFAULT '',
    provider            VARCHAR(32)  NOT NULL DEFAULT 'minimax',
    model               VARCHAR(128) NOT NULL DEFAULT '',
    upstream_model      VARCHAR(128) NOT NULL DEFAULT '',
    prompt              TEXT         NOT NULL DEFAULT '',
    status              VARCHAR(24)  NOT NULL DEFAULT 'queued',
    resolution          VARCHAR(24)  NOT NULL DEFAULT '',
    duration_seconds    INTEGER      NOT NULL DEFAULT 0,
    user_id             BIGINT       NOT NULL,
    api_key_id          BIGINT       NOT NULL DEFAULT 0,
    group_id            BIGINT       NOT NULL DEFAULT 0,
    account_id          BIGINT       NOT NULL DEFAULT 0,
    video_url           TEXT         NOT NULL DEFAULT '',
    error_message       TEXT         NOT NULL DEFAULT '',
    upstream_cost_usd   NUMERIC(18, 6) NOT NULL DEFAULT 0,
    billed_cost_usd     NUMERIC(18, 6) NOT NULL DEFAULT 0,
    billing_mode        VARCHAR(24)  NOT NULL DEFAULT 'local_price',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    completed_at        TIMESTAMPTZ,
    expires_at          TIMESTAMPTZ,
    deleted_at          TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_video_tasks_task_id ON video_tasks (task_id);
CREATE INDEX IF NOT EXISTS idx_video_tasks_user_created ON video_tasks (user_id, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_video_tasks_status_created ON video_tasks (status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_video_tasks_group_created ON video_tasks (group_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_video_tasks_account_created ON video_tasks (account_id, created_at DESC);
