-- 模板版本：每次保存写一条修订记录（可对比、回退）；步骤模板可用 uses: name@版本 锁定版本。
ALTER TABLE templates ADD COLUMN version INT NOT NULL DEFAULT 1;

CREATE TABLE template_revisions (
    template_id   TEXT        NOT NULL REFERENCES templates (id) ON DELETE CASCADE,
    version       INT         NOT NULL,
    name          TEXT        NOT NULL,
    description   TEXT        NOT NULL DEFAULT '',
    yaml          TEXT        NOT NULL,
    created_by    TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL,
    restored_from INT         NOT NULL DEFAULT 0,
    PRIMARY KEY (template_id, version)
);
INSERT INTO template_revisions (template_id, version, name, description, yaml, created_by, created_at)
SELECT id, 1, name, description, yaml, updated_by, updated_at FROM templates;
