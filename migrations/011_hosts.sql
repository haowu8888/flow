-- 部署环境、主机组、主机与部署记录。
CREATE TABLE environments (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT        NOT NULL REFERENCES tenants (id),
    name             TEXT        NOT NULL,
    description      TEXT        NOT NULL DEFAULT '',
    rank             INT         NOT NULL DEFAULT 0,
    require_approval BOOLEAN     NOT NULL DEFAULT false,
    approvers        TEXT[]      NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, name)
);

CREATE TABLE host_groups (
    id             TEXT PRIMARY KEY,
    tenant_id      TEXT        NOT NULL REFERENCES tenants (id),
    name           TEXT        NOT NULL,
    description    TEXT        NOT NULL DEFAULT '',
    environment_id TEXT REFERENCES environments (id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, name)
);

CREATE TABLE hosts (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT        NOT NULL REFERENCES tenants (id),
    name        TEXT        NOT NULL,
    address     TEXT        NOT NULL,
    port        INT         NOT NULL DEFAULT 22,
    username    TEXT        NOT NULL,
    auth_type   TEXT        NOT NULL,
    credential  BYTEA       NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    host_key    TEXT        NOT NULL DEFAULT '',
    group_ids   TEXT[]      NOT NULL DEFAULT '{}',
    checked_at  TIMESTAMPTZ,
    check_error TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, name)
);
CREATE INDEX hosts_groups ON hosts USING GIN (group_ids);

CREATE TABLE deployments (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT        NOT NULL REFERENCES tenants (id),
    pipeline_id      TEXT        NOT NULL REFERENCES pipelines (id) ON DELETE CASCADE,
    pipeline_name    TEXT        NOT NULL,
    run_id           TEXT        NOT NULL,
    run_number       INT         NOT NULL,
    job_id           TEXT        NOT NULL UNIQUE,
    job_name         TEXT        NOT NULL,
    environment_id   TEXT        NOT NULL DEFAULT '',
    environment_name TEXT        NOT NULL DEFAULT '',
    group_name       TEXT        NOT NULL,
    hosts            TEXT[]      NOT NULL DEFAULT '{}',
    git_commit       TEXT        NOT NULL DEFAULT '',
    status           TEXT        NOT NULL,
    created_by       TEXT        NOT NULL DEFAULT '',
    started_at       TIMESTAMPTZ NOT NULL,
    finished_at      TIMESTAMPTZ
);
CREATE INDEX deployments_env ON deployments (tenant_id, environment_id, started_at DESC);
CREATE INDEX deployments_pipeline ON deployments (pipeline_id, started_at DESC);

ALTER TABLE jobs ADD COLUMN host_states JSONB NOT NULL DEFAULT '[]';

