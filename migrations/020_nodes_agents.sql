-- 构建节点：self（自建，在机器上执行安装命令接入）/ github（GitHub Actions 机器，按需触发工作流）/ agent（主机 Agent，部署任务直接在主机上执行）。
ALTER TABLE runners ADD COLUMN kind    TEXT  NOT NULL DEFAULT 'self';
ALTER TABLE runners ADD COLUMN config  JSONB NOT NULL DEFAULT '{}'; -- github：repo / workflow / ref
ALTER TABLE runners ADD COLUMN secret  TEXT  NOT NULL DEFAULT '';   -- github：访问令牌（密文）
ALTER TABLE runners ADD COLUMN info    JSONB NOT NULL DEFAULT '{}'; -- 节点上报：系统、架构、主机名、版本、并发
ALTER TABLE runners ADD COLUMN host_id TEXT  NOT NULL DEFAULT '';   -- agent：绑定的主机

-- GitHub 节点的任务在触发工作流后记录时间，超时未被领取时重新触发。
ALTER TABLE jobs ADD COLUMN dispatched_at TIMESTAMPTZ;

-- 主机接入方式：ssh（Runner 通过 SSH 连接）/ agent（主机上的 Agent 主动拉取部署任务）。
ALTER TABLE hosts ADD COLUMN mode TEXT NOT NULL DEFAULT 'ssh';

-- 部署到 Agent 主机的单台任务：执行部署任务的 Runner 创建，主机上的 Agent 领取执行。
CREATE TABLE host_tasks (
    id          TEXT        PRIMARY KEY,
    tenant_id   TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    job_id      TEXT        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    host_id     TEXT        NOT NULL,
    script      TEXT        NOT NULL,
    env         JSONB       NOT NULL DEFAULT '{}',
    target      TEXT        NOT NULL DEFAULT '',
    upload_key  TEXT        NOT NULL DEFAULT '', -- 上传内容（tar.gz）的 blob key
    status      TEXT        NOT NULL DEFAULT 'queued',
    exit_code   INT         NOT NULL DEFAULT 0,
    error       TEXT        NOT NULL DEFAULT '',
    log         BYTEA       NOT NULL DEFAULT '',
    lease_until TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL,
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);
CREATE INDEX host_tasks_queued ON host_tasks (host_id, created_at) WHERE status = 'queued';
