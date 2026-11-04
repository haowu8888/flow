-- 用户、租户成员与角色、登录会话 / 个人访问令牌。

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT        NOT NULL UNIQUE,
    display_name  TEXT        NOT NULL DEFAULT '',
    password_hash TEXT        NOT NULL,
    is_admin      BOOLEAN     NOT NULL DEFAULT false, -- 系统管理员
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    tenant_id TEXT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id   TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role      TEXT NOT NULL, -- viewer / developer / admin
    PRIMARY KEY (tenant_id, user_id)
);
CREATE INDEX memberships_user ON memberships (user_id);

CREATE TABLE tokens (
    id           TEXT PRIMARY KEY,
    hash         TEXT        NOT NULL UNIQUE, -- 明文令牌的 SHA-256
    user_id      TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    tenant_id    TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    kind         TEXT        NOT NULL, -- session / api
    name         TEXT        NOT NULL DEFAULT '',
    prefix       TEXT        NOT NULL DEFAULT '',
    expires_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);
CREATE INDEX tokens_user ON tokens (user_id);
