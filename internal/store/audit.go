package store

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"flow/internal/model"
)

// AuditFilter 限定审计日志查询；空字段不限制。Before 用于翻页（返回早于它的记录）。
type AuditFilter struct {
	Actor    string
	Category string // action 前缀，如 pipeline、host
	TargetID string
	Before   time.Time
	Limit    int
}

const auditCols = `id, tenant_id, actor, action, target_type, target_id, target_name, detail, ip, created_at`

func scanAudit(row pgx.Row) (*model.AuditEvent, error) {
	var a model.AuditEvent
	if err := row.Scan(&a.ID, &a.TenantID, &a.Actor, &a.Action, &a.TargetType, &a.TargetID, &a.TargetName, &a.Detail,
		&a.IP, &a.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &a, nil
}

func (s pgq) CreateAuditEvent(ctx context.Context, a *model.AuditEvent) error {
	_, err := s.q.Exec(ctx, `INSERT INTO audit_events (`+auditCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		a.ID, a.TenantID, a.Actor, a.Action, a.TargetType, a.TargetID, a.TargetName, a.Detail, a.IP, a.CreatedAt)
	return err
}

func (s pgq) ListAuditEvents(ctx context.Context, tenantID string, f AuditFilter) ([]*model.AuditEvent, error) {
	before := f.Before
	if before.IsZero() {
		before = time.Now().Add(time.Hour)
	}
	rows, err := s.q.Query(ctx, `SELECT `+auditCols+` FROM audit_events
		WHERE tenant_id = $1 AND created_at < $2 AND ($3 = '' OR actor = $3) AND ($4 = '' OR action LIKE $4 || '.%')
			AND ($5 = '' OR target_id = $5)
		ORDER BY created_at DESC, id DESC LIMIT $6`, tenantID, before, f.Actor, f.Category, f.TargetID, f.Limit)
	return collect(rows, err, scanAudit)
}

func (s pgq) DeleteAuditBefore(ctx context.Context, before time.Time) error {
	_, err := s.q.Exec(ctx, `DELETE FROM audit_events WHERE created_at < $1`, before)
	return err
}

func (m *Memory) CreateAuditEvent(_ context.Context, a *model.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, clone(a))
	return nil
}

func (m *Memory) ListAuditEvents(_ context.Context, tenantID string, f AuditFilter) ([]*model.AuditEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.AuditEvent, 0)
	for i := len(m.audit) - 1; i >= 0 && len(out) < f.Limit; i-- {
		a := m.audit[i]
		if a.TenantID == tenantID && (f.Before.IsZero() || a.CreatedAt.Before(f.Before)) && (f.Actor == "" || a.Actor == f.Actor) &&
			(f.Category == "" || strings.HasPrefix(a.Action, f.Category+".")) && (f.TargetID == "" || a.TargetID == f.TargetID) {
			out = append(out, clone(a))
		}
	}
	return out, nil
}

func (m *Memory) DeleteAuditBefore(_ context.Context, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = slices.DeleteFunc(m.audit, func(a *model.AuditEvent) bool { return a.CreatedAt.Before(before) })
	return nil
}
