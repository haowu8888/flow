-- 跳板机、环境级变量与密钥、按环境重新部署（沿用原运行的产物）。
ALTER TABLE hosts ADD COLUMN jump_host_id TEXT NOT NULL DEFAULT '';

ALTER TABLE environments
    ADD COLUMN variables JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN secrets   JSONB NOT NULL DEFAULT '{}'; -- 名称 → 密文（base64）

ALTER TABLE runs ADD COLUMN artifacts_from TEXT NOT NULL DEFAULT '';
