-- 账户资料、登录会话的设备信息；流水线成员与角色、公开。
ALTER TABLE users ADD COLUMN email  TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN gender TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN bio    TEXT NOT NULL DEFAULT '';

ALTER TABLE tokens ADD COLUMN user_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN ip         TEXT NOT NULL DEFAULT '';

-- 自定义角色（内置的 owner / admin / member 不入库）；permissions 为 view、run、edit、delete、deleteRun、manageMembers 的子集。
CREATE TABLE pipeline_roles (
    tenant_id   TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    id          TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    permissions TEXT[]      NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name)
);

CREATE TABLE pipeline_members (
    pipeline_id TEXT        NOT NULL REFERENCES pipelines (id) ON DELETE CASCADE,
    user_id     TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role_id     TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (pipeline_id, user_id)
);
CREATE INDEX pipeline_members_user ON pipeline_members (user_id);

-- 不公开时只有拥有者、成员与租户管理员能看到；公开时租户内所有人可查看。
ALTER TABLE pipelines ADD COLUMN is_public BOOLEAN NOT NULL DEFAULT FALSE;

-- 已有流水线保持原来「租户内都能看、开发者能改」的行为：设为公开，开发者加为管理者。
UPDATE pipelines SET is_public = TRUE;
INSERT INTO pipeline_members (pipeline_id, user_id, role_id, created_at)
SELECT p.id, m.user_id, 'admin', now()
FROM pipelines p JOIN memberships m ON m.tenant_id = p.tenant_id
JOIN users u ON u.id = m.user_id
WHERE m.role = 'developer' AND u.username <> p.created_by
ON CONFLICT DO NOTHING;
