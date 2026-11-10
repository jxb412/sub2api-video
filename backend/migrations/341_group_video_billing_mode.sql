-- 视频计费模式（分组级）。
-- local_price   : 按 groups.video_model_prices 的秒单价 × 时长 × VideoRateMultiplier
-- upstream_cost : 跟随上游回传的 cost_usd × VideoRateMultiplier
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS video_billing_mode VARCHAR(24) NOT NULL DEFAULT 'local_price';
