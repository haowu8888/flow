-- 步骤级进度与运行并发组。
ALTER TABLE jobs ADD COLUMN step_states JSONB NOT NULL DEFAULT '[]';

ALTER TABLE runs ADD COLUMN concurrency_group TEXT NOT NULL DEFAULT '';
CREATE INDEX runs_concurrency_active ON runs (tenant_id, concurrency_group, created_at)
    WHERE concurrency_group <> '' AND finished_at IS NULL;
