-- 流水线配置版本：每次保存写一条修订记录，保存时按 version 检查并发修改；运行记录所用的版本。
ALTER TABLE pipelines
    ADD COLUMN version    INT  NOT NULL DEFAULT 1,
    ADD COLUMN updated_by TEXT NOT NULL DEFAULT '';

CREATE TABLE pipeline_revisions (
    pipeline_id TEXT        NOT NULL REFERENCES pipelines (id) ON DELETE CASCADE,
    version     INT         NOT NULL,
    name        TEXT        NOT NULL,
    yaml        TEXT        NOT NULL,
    created_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (pipeline_id, version)
);
INSERT INTO pipeline_revisions (pipeline_id, version, name, yaml, created_at)
SELECT id, 1, name, yaml, updated_at FROM pipelines;

ALTER TABLE runs ADD COLUMN pipeline_version INT NOT NULL DEFAULT 0;
