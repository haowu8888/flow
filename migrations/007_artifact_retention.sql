-- 产物保留：expires_at 为空表示永久保留；清理时跳过每条流水线最近一次成功运行的产物。
ALTER TABLE artifacts ADD COLUMN expires_at TIMESTAMPTZ;
CREATE INDEX artifacts_expires ON artifacts (expires_at) WHERE expires_at IS NOT NULL;
