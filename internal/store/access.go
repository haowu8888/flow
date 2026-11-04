package store

import (
	"context"
	"slices"
	"strings"

	"flow/internal/model"

	"github.com/jackc/pgx/v5"
)

// 流水线公开、成员与自定义角色。

func (s pgq) SetPipelinePublic(ctx context.Context, id string, public bool) error {
	return expectRow(s.q.Exec(ctx, `UPDATE pipelines SET is_public = $2 WHERE id = $1`, id, public))
}

func (s pgq) ListPipelineRoles(ctx context.Context, tenantID string) ([]*model.PipelineRole, error) {
	rows, err := s.q.Query(ctx, `SELECT tenant_id, id, name, description, permissions, created_at FROM pipeline_roles
		WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
	return collect(rows, err, func(row pgx.Row) (*model.PipelineRole, error) {
		var r model.PipelineRole
		if err := row.Scan(&r.TenantID, &r.ID, &r.Name, &r.Description, &r.Permissions, &r.CreatedAt); err != nil {
			return nil, mapErr(err)
		}
		return &r, nil
	})
}

func (s pgq) PutPipelineRole(ctx context.Context, r *model.PipelineRole) error {
	_, err := s.q.Exec(ctx, `INSERT INTO pipeline_roles (tenant_id, id, name, description, permissions, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, id) DO UPDATE SET name = $3, description = $4, permissions = $5`,
		r.TenantID, r.ID, r.Name, r.Description, nonNil(r.Permissions), r.CreatedAt)
	return mapErr(err)
}

func (s pgq) DeletePipelineRole(ctx context.Context, tenantID, id string) error {
	return expectRow(s.q.Exec(ctx, `DELETE FROM pipeline_roles WHERE tenant_id = $1 AND id = $2`, tenantID, id))
}

func (s pgq) ListPipelineMembers(ctx context.Context, pipelineID string) ([]*model.PipelineMember, error) {
	rows, err := s.q.Query(ctx, `SELECT pipeline_id, user_id, role_id, created_at FROM pipeline_members
		WHERE pipeline_id = $1 ORDER BY created_at, user_id`, pipelineID)
	return collect(rows, err, scanMember)
}

func scanMember(row pgx.Row) (*model.PipelineMember, error) {
	var m model.PipelineMember
	if err := row.Scan(&m.PipelineID, &m.UserID, &m.RoleID, &m.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &m, nil
}

func (s pgq) PutPipelineMember(ctx context.Context, m *model.PipelineMember) error {
	_, err := s.q.Exec(ctx, `INSERT INTO pipeline_members (pipeline_id, user_id, role_id, created_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (pipeline_id, user_id) DO UPDATE SET role_id = $3`, m.PipelineID, m.UserID, m.RoleID, m.CreatedAt)
	return mapErr(err)
}

func (s pgq) DeletePipelineMember(ctx context.Context, pipelineID, userID string) error {
	return expectRow(s.q.Exec(ctx, `DELETE FROM pipeline_members WHERE pipeline_id = $1 AND user_id = $2`, pipelineID, userID))
}

func (s pgq) UserPipelineRoles(ctx context.Context, userID string) (map[string]string, error) {
	rows, err := s.q.Query(ctx, `SELECT pipeline_id, user_id, role_id, created_at FROM pipeline_members WHERE user_id = $1`, userID)
	ms, err := collect(rows, err, scanMember)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(ms))
	for _, m := range ms {
		out[m.PipelineID] = m.RoleID
	}
	return out, nil
}

func (s pgq) RoleUsage(ctx context.Context, tenantID string) (map[string]int, error) {
	rows, err := s.q.Query(ctx, `SELECT m.role_id, count(DISTINCT m.pipeline_id) FROM pipeline_members m
		JOIN pipelines p ON p.id = m.pipeline_id WHERE p.tenant_id = $1 GROUP BY m.role_id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// ---- 内存实现 ----

type accessData struct {
	roles   map[string]*model.PipelineRole      // key: tenant/id
	members map[[2]string]*model.PipelineMember // key: pipeline / user
}

func (m *Memory) access() *accessData {
	if m.accessData == nil {
		m.accessData = &accessData{roles: map[string]*model.PipelineRole{}, members: map[[2]string]*model.PipelineMember{}}
	}
	return m.accessData
}

func (m *Memory) SetPipelinePublic(_ context.Context, id string, public bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pipelines[id]
	if !ok {
		return ErrNotFound
	}
	p.IsPublic = public
	return nil
}

func (m *Memory) ListPipelineRoles(_ context.Context, tenantID string) ([]*model.PipelineRole, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.PipelineRole, 0)
	for _, r := range m.access().roles {
		if r.TenantID == tenantID {
			out = append(out, clone(r))
		}
	}
	slices.SortFunc(out, func(a, b *model.PipelineRole) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

func (m *Memory) PutPipelineRole(_ context.Context, r *model.PipelineRole) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tenants[r.TenantID] == nil {
		return ErrNotFound
	}
	a := m.access()
	for _, o := range a.roles {
		if o.TenantID == r.TenantID && o.Name == r.Name && o.ID != r.ID {
			return ErrConflict
		}
	}
	key := r.TenantID + "/" + r.ID
	c := clone(r)
	c.Permissions = slices.Clone(r.Permissions)
	if old := a.roles[key]; old != nil {
		c.CreatedAt = old.CreatedAt
	}
	a.roles[key] = c
	return nil
}

func (m *Memory) DeletePipelineRole(_ context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := tenantID + "/" + id
	if m.access().roles[key] == nil {
		return ErrNotFound
	}
	delete(m.access().roles, key)
	return nil
}

func (m *Memory) ListPipelineMembers(_ context.Context, pipelineID string) ([]*model.PipelineMember, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.PipelineMember, 0)
	for k, mb := range m.access().members {
		if k[0] == pipelineID {
			out = append(out, clone(mb))
		}
	}
	slices.SortFunc(out, func(a, b *model.PipelineMember) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.UserID, b.UserID)
	})
	return out, nil
}

func (m *Memory) PutPipelineMember(_ context.Context, mb *model.PipelineMember) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pipelines[mb.PipelineID] == nil || m.users[mb.UserID] == nil {
		return ErrNotFound
	}
	k := [2]string{mb.PipelineID, mb.UserID}
	c := clone(mb)
	if old := m.access().members[k]; old != nil {
		c.CreatedAt = old.CreatedAt
	}
	m.access().members[k] = c
	return nil
}

func (m *Memory) DeletePipelineMember(_ context.Context, pipelineID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]string{pipelineID, userID}
	if m.access().members[k] == nil {
		return ErrNotFound
	}
	delete(m.access().members, k)
	return nil
}

func (m *Memory) UserPipelineRoles(_ context.Context, userID string) (map[string]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]string{}
	for k, mb := range m.access().members {
		if k[1] == userID {
			out[k[0]] = mb.RoleID
		}
	}
	return out, nil
}

func (m *Memory) RoleUsage(_ context.Context, tenantID string) (map[string]int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := map[[2]string]bool{}
	out := map[string]int{}
	for k, mb := range m.access().members {
		if p := m.pipelines[k[0]]; p != nil && p.TenantID == tenantID && !seen[[2]string{mb.RoleID, k[0]}] {
			seen[[2]string{mb.RoleID, k[0]}] = true
			out[mb.RoleID]++
		}
	}
	return out, nil
}

// dropAccess 删除流水线或用户时清理成员关系（调用方持有写锁）。
func (m *Memory) dropAccess(pipelineID, userID string) {
	if m.accessData == nil {
		return
	}
	for k := range m.accessData.members {
		if (pipelineID != "" && k[0] == pipelineID) || (userID != "" && k[1] == userID) {
			delete(m.accessData.members, k)
		}
	}
}
