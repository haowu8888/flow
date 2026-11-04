-- 任务输出（步骤写入 $FLOW_OUTPUT 的 KEY=VALUE，下游任务作为环境变量读取，运行条件可引用）。
ALTER TABLE jobs ADD COLUMN outputs JSONB NOT NULL DEFAULT '{}';
-- 待发送的通知：进入待审批时（审批消息）、结束时（任务插件）；发送后清除，多实例下由 SKIP LOCKED 保证只发一次。
ALTER TABLE jobs ADD COLUMN notify_waiting BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE jobs ADD COLUMN notify_done    BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX jobs_pending_notify ON jobs (id) WHERE notify_waiting OR notify_done;

-- 命名构建物：「构建物上传」步骤按名称上传，同一流水线同名构建物的版本号递增；name 为空的是任务的默认归档。
ALTER TABLE artifacts ADD COLUMN name    TEXT NOT NULL DEFAULT '';
ALTER TABLE artifacts ADD COLUMN version TEXT NOT NULL DEFAULT '';
ALTER TABLE artifacts ADD COLUMN format  TEXT NOT NULL DEFAULT 'tar.gz';
ALTER TABLE artifacts DROP CONSTRAINT artifacts_job_id_key;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_job_name UNIQUE (job_id, name);
