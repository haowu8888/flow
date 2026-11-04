-- 模板：steps 为可在流水线中以 uses 引用的步骤组，pipeline 为新建流水线时的起点。
CREATE TABLE templates (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT        NOT NULL REFERENCES tenants (id),
    kind        TEXT        NOT NULL, -- steps / pipeline
    name        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    yaml        TEXT        NOT NULL,
    updated_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, kind, name)
);
