-- 测试报告（JUnit 解析结果）与审计日志。

-- 测试报告：每个 Job 一份。
CREATE TABLE test_reports (
    job_id     TEXT PRIMARY KEY REFERENCES jobs (id) ON DELETE CASCADE,
    run_id     TEXT        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    tenant_id  TEXT        NOT NULL,
    job_name   TEXT        NOT NULL,
    total      INT         NOT NULL,
    passed     INT         NOT NULL,
    failed     INT         NOT NULL,
    skipped    INT         NOT NULL,
    duration   DOUBLE PRECISION NOT NULL,
    cases      JSONB       NOT NULL DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX test_reports_run ON test_reports (run_id);

-- 审计日志：谁在什么时候对什么对象做了什么。
CREATE TABLE audit_events (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT        NOT NULL,
    actor       TEXT        NOT NULL,
    action      TEXT        NOT NULL,
    target_type TEXT        NOT NULL DEFAULT '',
    target_id   TEXT        NOT NULL DEFAULT '',
    target_name TEXT        NOT NULL DEFAULT '',
    detail      TEXT        NOT NULL DEFAULT '',
    ip          TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX audit_events_tenant ON audit_events (tenant_id, created_at DESC);
