-- 部署记录中的构建物（deploy.artifact）：名称、版本、大小，构建物被清理后仍可展示。
ALTER TABLE deployments ADD COLUMN artifact JSONB;
