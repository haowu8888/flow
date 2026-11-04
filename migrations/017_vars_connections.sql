-- 通用变量组：每次运行注入为环境变量；私密变量的值加密存储（只写不读）。
CREATE TABLE var_groups (
    id          TEXT        PRIMARY KEY,
    tenant_id   TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    vars        JSONB       NOT NULL DEFAULT '[]', -- [{name, value, secret}]，secret 的 value 为密文（base64）
    created_by  TEXT        NOT NULL DEFAULT '',
    updated_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, name)
);

-- 服务连接：第三方凭据（Git、镜像仓库、NPM、对象存储、Bark、企业微信机器人、Telegram、邮件、Cloudreve），
-- 流水线步骤按名称引用；私密字段加密存储。
CREATE TABLE service_connections (
    id            TEXT        PRIMARY KEY,
    tenant_id     TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    type          TEXT        NOT NULL,
    name          TEXT        NOT NULL,
    description   TEXT        NOT NULL DEFAULT '',
    config        JSONB       NOT NULL DEFAULT '{}', -- 非私密字段
    secrets       JSONB       NOT NULL DEFAULT '{}', -- 私密字段，值为密文（base64）
    visibility    TEXT        NOT NULL DEFAULT 'private', -- private / public / custom
    allowed_users TEXT[]      NOT NULL DEFAULT '{}',      -- custom 时可引用的用户名
    created_by    TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, name)
);

-- 运行备注（手动运行时填写，之后可修改）。
ALTER TABLE runs ADD COLUMN remark TEXT NOT NULL DEFAULT '';

-- 并发组允许同时执行的运行数（创建时从流水线定义快照，0 / 1 都表示一次一个）。
ALTER TABLE runs ADD COLUMN concurrency_max INT NOT NULL DEFAULT 0;
