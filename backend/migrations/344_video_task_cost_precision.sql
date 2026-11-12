-- Video token settlement uses the same eight-decimal USD precision as the
-- transactional billing ledger. Widening preserves all existing amounts.
ALTER TABLE video_tasks
    ALTER COLUMN upstream_cost_usd TYPE NUMERIC(20, 8),
    ALTER COLUMN billed_cost_usd TYPE NUMERIC(20, 8);
