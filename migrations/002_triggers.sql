-- 触发器：webhook 签名密钥、cron 调度状态与最近触发结果。
ALTER TABLE triggers
    ADD COLUMN secret        TEXT        NOT NULL DEFAULT '',
    ADD COLUMN next_run_at   TIMESTAMPTZ,
    ADD COLUMN last_fired_at TIMESTAMPTZ,
    ADD COLUMN last_error    TEXT        NOT NULL DEFAULT '',
    ADD COLUMN updated_at    TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE INDEX triggers_pipeline ON triggers (pipeline_id, created_at);
-- 调度器：SELECT ... WHERE kind = 'cron' AND enabled AND next_run_at <= now() FOR UPDATE SKIP LOCKED
CREATE INDEX triggers_cron_due ON triggers (next_run_at) WHERE kind = 'cron' AND enabled;
