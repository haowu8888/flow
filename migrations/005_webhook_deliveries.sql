-- Webhook 投递去重：Git 平台超时重试或手动重发同一投递（相同投递 ID）时复用当时创建的 Run。
CREATE TABLE webhook_deliveries (
    trigger_id  TEXT        NOT NULL REFERENCES triggers (id) ON DELETE CASCADE,
    delivery_id TEXT        NOT NULL,
    run_id      TEXT        NOT NULL DEFAULT '', -- 空：投递被记录但未创建 Run（如流水线 YAML 无效）
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (trigger_id, delivery_id)
);
-- 定期清理过期记录
CREATE INDEX webhook_deliveries_created ON webhook_deliveries (created_at);
