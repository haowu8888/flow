-- 版本回退、运行回滚所需的提交记录，以及流水线列表的分组与收藏。
ALTER TABLE pipeline_revisions ADD COLUMN restored_from INT NOT NULL DEFAULT 0;

ALTER TABLE pipelines ADD COLUMN group_name TEXT NOT NULL DEFAULT '';

ALTER TABLE runs
    ADD COLUMN git_commit  TEXT NOT NULL DEFAULT '',
    ADD COLUMN rollback_of TEXT NOT NULL DEFAULT '';
UPDATE runs SET git_commit = env ->> 'FLOW_GIT_COMMIT' WHERE env ? 'FLOW_GIT_COMMIT';

CREATE TABLE pipeline_favorites (
    user_id     TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    pipeline_id TEXT        NOT NULL REFERENCES pipelines (id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, pipeline_id)
);
