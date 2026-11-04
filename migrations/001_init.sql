-- Flow 初始表结构（PostgreSQL 15+）。由 store.Migrate 在 Server 启动时自动执行。
-- 所有业务表都带 tenant_id，便于日后演进为多租户 SaaS。

CREATE TABLE tenants (
    id         TEXT PRIMARY KEY,
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 接入用户体系前所有数据归属默认租户。
INSERT INTO tenants (id, name) VALUES ('default', 'Default');

CREATE TABLE pipelines (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT        NOT NULL REFERENCES tenants (id),
    name       TEXT        NOT NULL,
    yaml       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE runs (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT        NOT NULL REFERENCES tenants (id),
    pipeline_id TEXT        NOT NULL REFERENCES pipelines (id) ON DELETE CASCADE,
    number      INT         NOT NULL, -- 流水线内自增，取 max(number)+1 时需锁 pipeline 行
    status      TEXT        NOT NULL,
    trigger     TEXT        NOT NULL,
    env         JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    UNIQUE (pipeline_id, number)
);
CREATE INDEX runs_pipeline_created ON runs (pipeline_id, created_at DESC);
CREATE INDEX runs_tenant_status ON runs (tenant_id, status);

CREATE TABLE jobs (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT        NOT NULL REFERENCES tenants (id),
    run_id           TEXT        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    seq              INT         NOT NULL,
    name             TEXT        NOT NULL,
    stage            TEXT        NOT NULL,
    stage_index      INT         NOT NULL,
    needs            TEXT[]      NOT NULL DEFAULT '{}',
    image            TEXT        NOT NULL DEFAULT '',
    env              JSONB       NOT NULL DEFAULT '{}',
    steps            JSONB       NOT NULL,
    status           TEXT        NOT NULL,
    runner_id        TEXT        NOT NULL DEFAULT '',
    lease_until      TIMESTAMPTZ,
    cancel_requested BOOLEAN     NOT NULL DEFAULT false,
    exit_code        INT         NOT NULL DEFAULT 0,
    error            TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at       TIMESTAMPTZ,
    finished_at      TIMESTAMPTZ,
    UNIQUE (run_id, name)
);
-- 领取任务：SELECT ... WHERE status = 'queued' ORDER BY created_at, seq LIMIT 1 FOR UPDATE SKIP LOCKED
CREATE INDEX jobs_queued ON jobs (created_at, seq) WHERE status = 'queued';
-- 租约回收：扫描 lease_until < now() 的运行中任务
CREATE INDEX jobs_running_lease ON jobs (lease_until) WHERE status = 'running';

-- 日志按块追加；量大后改为写对象存储，此表只存近期或索引。
CREATE TABLE job_log_chunks (
    id         BIGSERIAL PRIMARY KEY,
    job_id     TEXT        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    byte_start BIGINT      NOT NULL, -- 该块在完整日志中的起始偏移，支撑 ?offset= 续读
    data       BYTEA       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (job_id, byte_start) -- 同时作为按偏移读取的索引
);

-- 以下为后续阶段使用，MVP 尚未接入。

CREATE TABLE runners (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT        NOT NULL REFERENCES tenants (id),
    name         TEXT        NOT NULL,
    labels       TEXT[]      NOT NULL DEFAULT '{}', -- 与 Job 的 runs-on 标签匹配
    token_hash   TEXT        NOT NULL,              -- 每个 Runner 独立 Token，替代共享 Token
    last_seen_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE secrets (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT        NOT NULL REFERENCES tenants (id),
    pipeline_id     TEXT REFERENCES pipelines (id) ON DELETE CASCADE, -- NULL 表示租户级共享
    name            TEXT        NOT NULL,
    value_encrypted BYTEA       NOT NULL, -- 信封加密，日志中按值脱敏
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (tenant_id, pipeline_id, name)
);

CREATE TABLE triggers (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT        NOT NULL REFERENCES tenants (id),
    pipeline_id TEXT        NOT NULL REFERENCES pipelines (id) ON DELETE CASCADE,
    kind        TEXT        NOT NULL, -- webhook / cron
    config      JSONB       NOT NULL, -- webhook: 仓库、分支过滤、签名密钥；cron: 表达式
    enabled     BOOLEAN     NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
