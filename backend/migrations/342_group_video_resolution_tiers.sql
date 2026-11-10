-- 视频分辨率档位映射（分组级，可覆盖内置默认）。
-- 例：{"768p":"720p"} 表示把上游实际输出的 768p 按 720p 档计价。
-- 内置默认见 LookupVideoBillingResolution；这里只做覆盖，避免改代码。
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS video_resolution_tiers JSONB NOT NULL DEFAULT '{}'::jsonb;
