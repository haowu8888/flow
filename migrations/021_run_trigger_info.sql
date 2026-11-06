-- 触发详情：Webhook 推送的提交列表、对比链接、推送人；流水线 Webhook 覆盖的变量。
ALTER TABLE runs ADD COLUMN trigger_info JSONB;
