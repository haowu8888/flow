package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"flow/internal/model"
)

const templateCols = `id, tenant_id, kind, name, description, yaml, updated_by, created_at, updated_at, version`

func scanTemplate(row pgx.Row) (*model.Template, error) {
	var t model.Template
	if err := row.Scan(&t.ID, &t.TenantID, &t.Kind, &t.Name, &t.Description, &t.YAML, &t.UpdatedBy,
		&t.CreatedAt, &t.UpdatedAt, &t.Version); err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

// 模板与修订记录用单条带 CTE 的语句写入，无需外层事务也是原子的。
func (s pgq) CreateTemplate(ctx context.Context, t *model.Template) error {
	_, err := s.q.Exec(ctx, `WITH t AS (
			INSERT INTO templates (`+templateCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1) RETURNING id
		)
		INSERT INTO template_revisions (template_id, version, name, description, yaml, created_by, created_at)
		SELECT id, 1, $4, $5, $6, $7, $8 FROM t`,
		t.ID, t.TenantID, t.Kind, t.Name, t.Description, t.YAML, t.UpdatedBy, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return mapErr(err)
	}
	t.Version = 1
	return nil
}

func (s pgq) UpdateTemplate(ctx context.Context, t *model.Template) error {
	err := s.q.QueryRow(ctx, `WITH t AS (
			UPDATE templates SET name = $2, description = $3, yaml = $4, updated_by = $5, updated_at = $6, version = version + 1
			WHERE id = $1 AND version = $7 RETURNING version
		)
		INSERT INTO template_revisions (template_id, version, name, description, yaml, created_by, created_at, restored_from)
		SELECT $1, version, $2, $3, $4, $5, $6, $8 FROM t
		RETURNING version`,
		t.ID, t.Name, t.Description, t.YAML, t.UpdatedBy, t.UpdatedAt, t.Version, t.RestoredFrom).Scan(&t.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := s.GetTemplate(ctx, t.ID); err != nil {
			return err
		}
		return ErrConflict
	}
	return mapErr(err)
}

const templateRevCols = `template_id, version, name, description, yaml, created_by, created_at, restored_from`

func scanTemplateRevision(row pgx.Row) (*model.TemplateRevision, error) {
	var r model.TemplateRevision
	if err := row.Scan(&r.TemplateID, &r.Version, &r.Name, &r.Description, &r.YAML, &r.CreatedBy, &r.CreatedAt, &r.RestoredFrom); err != nil {
		return nil, mapErr(err)
	}
	return &r, nil
}

func (s pgq) ListTemplateRevisions(ctx context.Context, templateID string, limit int) ([]*model.TemplateRevision, error) {
	rows, err := s.q.Query(ctx, `SELECT template_id, version, name, description, '', created_by, created_at, restored_from
		FROM template_revisions WHERE template_id = $1 ORDER BY version DESC LIMIT $2`, templateID, limit)
	return collect(rows, err, scanTemplateRevision)
}

func (s pgq) GetTemplateRevision(ctx context.Context, templateID string, version int) (*model.TemplateRevision, error) {
	return scanTemplateRevision(s.q.QueryRow(ctx, `SELECT `+templateRevCols+` FROM template_revisions
		WHERE template_id = $1 AND version = $2`, templateID, version))
}

func (s pgq) GetTemplateRevisionByName(ctx context.Context, tenantID string, kind model.TemplateKind, name string, version int) (*model.TemplateRevision, error) {
	return scanTemplateRevision(s.q.QueryRow(ctx, `SELECT r.template_id, r.version, r.name, r.description, r.yaml, r.created_by, r.created_at, r.restored_from
		FROM template_revisions r JOIN templates t ON t.id = r.template_id
		WHERE t.tenant_id = $1 AND t.kind = $2 AND t.name = $3 AND r.version = $4`, tenantID, kind, name, version))
}

func (s pgq) DeleteTemplate(ctx context.Context, id string) error {
	return expectRow(s.q.Exec(ctx, `DELETE FROM templates WHERE id = $1`, id))
}

func (s pgq) GetTemplate(ctx context.Context, id string) (*model.Template, error) {
	return scanTemplate(s.q.QueryRow(ctx, `SELECT `+templateCols+` FROM templates WHERE id = $1`, id))
}

func (s pgq) ListTemplates(ctx context.Context, tenantID string, kind model.TemplateKind) ([]*model.Template, error) {
	rows, err := s.q.Query(ctx, `SELECT `+templateCols+` FROM templates WHERE tenant_id = $1 AND ($2 = '' OR kind = $2)
		ORDER BY kind, name`, tenantID, kind)
	return collect(rows, err, scanTemplate)
}

func (s pgq) GetTemplateByName(ctx context.Context, tenantID string, kind model.TemplateKind, name string) (*model.Template, error) {
	return scanTemplate(s.q.QueryRow(ctx, `SELECT `+templateCols+` FROM templates WHERE tenant_id = $1 AND kind = $2 AND name = $3`,
		tenantID, kind, name))
}

func (m *Memory) templateNameTaken(t *model.Template) bool {
	for _, o := range m.templates {
		if o.ID != t.ID && o.TenantID == t.TenantID && o.Kind == t.Kind && o.Name == t.Name {
			return true
		}
	}
	return false
}

func (m *Memory) CreateTemplate(_ context.Context, t *model.Template) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tenants[t.TenantID] == nil {
		return ErrNotFound
	}
	if _, dup := m.templates[t.ID]; dup || m.templateNameTaken(t) {
		return ErrConflict
	}
	t.Version = 1
	m.templates[t.ID] = clone(t)
	m.addTemplateRevision(t, t.CreatedAt)
	return nil
}

func (m *Memory) addTemplateRevision(t *model.Template, at time.Time) {
	m.templateRevs[t.ID] = append(m.templateRevs[t.ID], &model.TemplateRevision{
		TemplateID: t.ID, Version: t.Version, Name: t.Name, Description: t.Description, YAML: t.YAML,
		CreatedBy: t.UpdatedBy, RestoredFrom: t.RestoredFrom, CreatedAt: at,
	})
}

func (m *Memory) UpdateTemplate(_ context.Context, t *model.Template) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.templates[t.ID]
	if !ok {
		return ErrNotFound
	}
	if m.templateNameTaken(t) || old.Version != t.Version {
		return ErrConflict
	}
	t.Version++
	c := clone(old)
	c.Name, c.Description, c.YAML, c.UpdatedBy, c.UpdatedAt, c.Version = t.Name, t.Description, t.YAML, t.UpdatedBy, t.UpdatedAt, t.Version
	m.templates[t.ID] = c
	m.addTemplateRevision(t, t.UpdatedAt)
	return nil
}

func (m *Memory) ListTemplateRevisions(_ context.Context, templateID string, limit int) ([]*model.TemplateRevision, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	revs := m.templateRevs[templateID]
	out := make([]*model.TemplateRevision, 0, min(limit, len(revs)))
	for i := len(revs) - 1; i >= 0 && len(out) < limit; i-- {
		r := clone(revs[i])
		r.YAML = ""
		out = append(out, r)
	}
	return out, nil
}

func (m *Memory) GetTemplateRevision(_ context.Context, templateID string, version int) (*model.TemplateRevision, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.templateRevs[templateID] {
		if r.Version == version {
			return clone(r), nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) GetTemplateRevisionByName(ctx context.Context, tenantID string, kind model.TemplateKind, name string, version int) (*model.TemplateRevision, error) {
	t, err := m.GetTemplateByName(ctx, tenantID, kind, name)
	if err != nil {
		return nil, err
	}
	return m.GetTemplateRevision(ctx, t.ID, version)
}

func (m *Memory) DeleteTemplate(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.templates[id]; !ok {
		return ErrNotFound
	}
	delete(m.templates, id)
	delete(m.templateRevs, id)
	return nil
}

func (m *Memory) GetTemplate(_ context.Context, id string) (*model.Template, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.templates[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(t), nil
}

func (m *Memory) ListTemplates(_ context.Context, tenantID string, kind model.TemplateKind) ([]*model.Template, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Template, 0)
	for _, t := range m.templates {
		if t.TenantID == tenantID && (kind == "" || t.Kind == kind) {
			out = append(out, clone(t))
		}
	}
	slices.SortFunc(out, func(a, b *model.Template) int {
		if c := strings.Compare(string(a.Kind), string(b.Kind)); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

func (m *Memory) GetTemplateByName(_ context.Context, tenantID string, kind model.TemplateKind, name string) (*model.Template, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.templates {
		if t.TenantID == tenantID && t.Kind == kind && t.Name == name {
			return clone(t), nil
		}
	}
	return nil, ErrNotFound
}
