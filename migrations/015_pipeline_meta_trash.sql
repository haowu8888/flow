-- 流水线列表元信息（不属于配置、不产生版本）：创建人、说明、归档标记；删除先进回收站（deleted_at 非空）。
ALTER TABLE pipelines ADD COLUMN created_by  TEXT NOT NULL DEFAULT '';
ALTER TABLE pipelines ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE pipelines ADD COLUMN archived    BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE pipelines ADD COLUMN deleted_at  TIMESTAMPTZ;
UPDATE pipelines p SET created_by = r.created_by FROM pipeline_revisions r WHERE r.pipeline_id = p.id AND r.version = 1;
