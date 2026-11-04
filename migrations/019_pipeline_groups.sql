-- 流水线分组：可以先建空分组；流水线的 group_name 指向分组名（历史数据中只出现在流水线上的分组照常显示）。
CREATE TABLE pipeline_groups (
    tenant_id  TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, name)
);
INSERT INTO pipeline_groups (tenant_id, name, created_at)
SELECT DISTINCT tenant_id, group_name, now() FROM pipelines WHERE group_name <> '';
