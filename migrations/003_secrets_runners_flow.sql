-- 密钥、Runner 独立 Token 与标签、构建产物、人工卡点 / 重试 / 通知等流程能力。

ALTER TABLE secrets ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
CREATE INDEX secrets_pipeline ON secrets (pipeline_id);

ALTER TABLE runners ADD COLUMN token_prefix TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX runners_token_hash ON runners (token_hash);

ALTER TABLE runs
    ADD COLUMN created_by  TEXT    NOT NULL DEFAULT '',
    ADD COLUMN notify      JSONB   NOT NULL DEFAULT '[]', -- 创建时从流水线定义快照
    ADD COLUMN notify_done BOOLEAN NOT NULL DEFAULT true;  -- 有通知配置的 Run 结束时置 false，由后台发送后置 true
CREATE INDEX runs_notify_pending ON runs (finished_at) WHERE NOT notify_done;

ALTER TABLE jobs
    ADD COLUMN runs_on    TEXT[] NOT NULL DEFAULT '{}', -- Runner 标签须包含全部
    ADD COLUMN options    JSONB  NOT NULL DEFAULT '{}', -- 卡点、条件、超时、重试、产物、缓存、代码源
    ADD COLUMN attempt    INT    NOT NULL DEFAULT 1,
    ADD COLUMN decided_by TEXT   NOT NULL DEFAULT '';

CREATE TABLE artifacts (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT        NOT NULL REFERENCES tenants (id),
    run_id     TEXT        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    job_id     TEXT        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    job_name   TEXT        NOT NULL,
    size       BIGINT      NOT NULL,
    blob_key   TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (job_id) -- 每个 Job 一个归档；重试时覆盖
);
CREATE INDEX artifacts_run ON artifacts (run_id);
