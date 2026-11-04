package model

import "time"

// AuditEvent 是一次写操作的审计记录。
type AuditEvent struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"-"`
	Actor      string    `json:"actor"`  // 用户名
	Action     string    `json:"action"` // 如 pipeline.update、run.trigger、host.delete
	TargetType string    `json:"target_type,omitempty"`
	TargetID   string    `json:"target_id,omitempty"`
	TargetName string    `json:"target_name,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	IP         string    `json:"ip,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}
