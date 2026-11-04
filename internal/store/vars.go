package store

import (
	"context"
	"maps"
	"slices"
	"strings"

	"flow/internal/model"

	"github.com/jackc/pgx/v5"
)

// 通用变量组、服务连接与运行备注。

const varGroupCols = `id, tenant_id, name, description, vars, created_by, updated_by, created_at, updated_at`

func scanVarGroup(row pgx.Row) (*model.VarGroup, error) {
	var g model.VarGroup
	if err := row.Scan(&g.ID, &g.TenantID, &g.Name, &g.Description, &g.Vars, &g.CreatedBy, &g.UpdatedBy, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	if g.Vars == nil {
		g.Vars = []model.GroupVar{}
	}
	return &g, nil
}

func (s pgq) ListVarGroups(ctx context.Context, tenantID string) ([]*model.VarGroup, error) {
	rows, err := s.q.Query(ctx, `SELECT `+varGroupCols+` FROM var_groups WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
	return collect(rows, err, scanVarGroup)
}

func (s pgq) GetVarGroup(ctx context.Context, id string) (*model.VarGroup, error) {
	return scanVarGroup(s.q.QueryRow(ctx, `SELECT `+varGroupCols+` FROM var_groups WHERE id = $1`, id))
}

func (s pgq) PutVarGroup(ctx context.Context, g *model.VarGroup) error {
	_, err := s.q.Exec(ctx, `INSERT INTO var_groups (`+varGroupCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (id) DO UPDATE SET name = $3, description = $4, vars = $5, updated_by = $7, updated_at = $9`,
		g.ID, g.TenantID, g.Name, g.Description, nonNil(g.Vars), g.CreatedBy, g.UpdatedBy, g.CreatedAt, g.UpdatedAt)
	return mapErr(err)
}

func (s pgq) DeleteVarGroup(ctx context.Context, id string) error {
	return expectRow(s.q.Exec(ctx, `DELETE FROM var_groups WHERE id = $1`, id))
}

const connCols = `id, tenant_id, type, name, description, config, secrets, visibility, allowed_users, created_by, created_at, updated_at`

func scanConn(row pgx.Row) (*model.ServiceConnection, error) {
	var c model.ServiceConnection
	if err := row.Scan(&c.ID, &c.TenantID, &c.Type, &c.Name, &c.Description, &c.Config, &c.Secrets, &c.Visibility,
		&c.AllowedUsers, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	fillConn(&c)
	return &c, nil
}

// fillConn 规整空字段并列出已设置的私密字段。
func fillConn(c *model.ServiceConnection) {
	if c.Config == nil {
		c.Config = map[string]string{}
	}
	if c.Secrets == nil {
		c.Secrets = map[string]string{}
	}
	if c.AllowedUsers == nil {
		c.AllowedUsers = []string{}
	}
	c.SecretKeys = slices.Sorted(maps.Keys(c.Secrets))
}

func (s pgq) ListConnections(ctx context.Context, tenantID string) ([]*model.ServiceConnection, error) {
	rows, err := s.q.Query(ctx, `SELECT `+connCols+` FROM service_connections WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
	return collect(rows, err, scanConn)
}

func (s pgq) GetConnection(ctx context.Context, id string) (*model.ServiceConnection, error) {
	return scanConn(s.q.QueryRow(ctx, `SELECT `+connCols+` FROM service_connections WHERE id = $1`, id))
}

func (s pgq) GetConnectionByName(ctx context.Context, tenantID, name string) (*model.ServiceConnection, error) {
	return scanConn(s.q.QueryRow(ctx, `SELECT `+connCols+` FROM service_connections WHERE tenant_id = $1 AND name = $2`, tenantID, name))
}

func (s pgq) PutConnection(ctx context.Context, c *model.ServiceConnection) error {
	_, err := s.q.Exec(ctx, `INSERT INTO service_connections (`+connCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (id) DO UPDATE SET name = $4, description = $5, config = $6, secrets = $7, visibility = $8,
			allowed_users = $9, updated_at = $12`,
		c.ID, c.TenantID, c.Type, c.Name, c.Description, nonNilMap(c.Config), nonNilMap(c.Secrets), c.Visibility,
		nonNil(c.AllowedUsers), c.CreatedBy, c.CreatedAt, c.UpdatedAt)
	return mapErr(err)
}

func (s pgq) DeleteConnection(ctx context.Context, id string) error {
	return expectRow(s.q.Exec(ctx, `DELETE FROM service_connections WHERE id = $1`, id))
}

func (s pgq) SetRunRemark(ctx context.Context, id, remark string) error {
	return expectRow(s.q.Exec(ctx, `UPDATE runs SET remark = $2 WHERE id = $1`, id, remark))
}

// ---- 内存实现 ----

type varData struct {
	groups map[string]*model.VarGroup
	conns  map[string]*model.ServiceConnection
}

func (m *Memory) vars() *varData {
	if m.varData == nil {
		m.varData = &varData{groups: map[string]*model.VarGroup{}, conns: map[string]*model.ServiceConnection{}}
	}
	return m.varData
}

func cloneGroup(g *model.VarGroup) *model.VarGroup {
	c := *g
	c.Vars = slices.Clone(g.Vars)
	if c.Vars == nil {
		c.Vars = []model.GroupVar{}
	}
	return &c
}

func cloneConn(c *model.ServiceConnection) *model.ServiceConnection {
	o := *c
	o.Config, o.Secrets, o.AllowedUsers = maps.Clone(c.Config), maps.Clone(c.Secrets), slices.Clone(c.AllowedUsers)
	fillConn(&o)
	return &o
}

func sortByCreated[T any](list []T, at func(T) (int64, string)) {
	slices.SortFunc(list, func(a, b T) int {
		ta, ia := at(a)
		tb, ib := at(b)
		if ta != tb {
			if ta < tb {
				return -1
			}
			return 1
		}
		return strings.Compare(ia, ib)
	})
}

func (m *Memory) ListVarGroups(_ context.Context, tenantID string) ([]*model.VarGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.VarGroup, 0)
	for _, g := range m.vars().groups {
		if g.TenantID == tenantID {
			out = append(out, cloneGroup(g))
		}
	}
	sortByCreated(out, func(g *model.VarGroup) (int64, string) { return g.CreatedAt.UnixNano(), g.ID })
	return out, nil
}

func (m *Memory) GetVarGroup(_ context.Context, id string) (*model.VarGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g := m.vars().groups[id]
	if g == nil {
		return nil, ErrNotFound
	}
	return cloneGroup(g), nil
}

func (m *Memory) PutVarGroup(_ context.Context, g *model.VarGroup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tenants[g.TenantID] == nil {
		return ErrNotFound
	}
	for _, o := range m.vars().groups {
		if o.TenantID == g.TenantID && o.Name == g.Name && o.ID != g.ID {
			return ErrConflict
		}
	}
	c := cloneGroup(g)
	if old := m.vars().groups[g.ID]; old != nil {
		c.CreatedAt, c.CreatedBy = old.CreatedAt, old.CreatedBy
	}
	m.vars().groups[g.ID] = c
	return nil
}

func (m *Memory) DeleteVarGroup(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vars().groups[id] == nil {
		return ErrNotFound
	}
	delete(m.vars().groups, id)
	return nil
}

func (m *Memory) ListConnections(_ context.Context, tenantID string) ([]*model.ServiceConnection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.ServiceConnection, 0)
	for _, c := range m.vars().conns {
		if c.TenantID == tenantID {
			out = append(out, cloneConn(c))
		}
	}
	sortByCreated(out, func(c *model.ServiceConnection) (int64, string) { return c.CreatedAt.UnixNano(), c.ID })
	return out, nil
}

func (m *Memory) GetConnection(_ context.Context, id string) (*model.ServiceConnection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c := m.vars().conns[id]
	if c == nil {
		return nil, ErrNotFound
	}
	return cloneConn(c), nil
}

func (m *Memory) GetConnectionByName(_ context.Context, tenantID, name string) (*model.ServiceConnection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, c := range m.vars().conns {
		if c.TenantID == tenantID && c.Name == name {
			return cloneConn(c), nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) PutConnection(_ context.Context, c *model.ServiceConnection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tenants[c.TenantID] == nil {
		return ErrNotFound
	}
	for _, o := range m.vars().conns {
		if o.TenantID == c.TenantID && o.Name == c.Name && o.ID != c.ID {
			return ErrConflict
		}
	}
	n := cloneConn(c)
	if old := m.vars().conns[c.ID]; old != nil {
		n.CreatedAt, n.CreatedBy, n.Type = old.CreatedAt, old.CreatedBy, old.Type
	}
	m.vars().conns[c.ID] = n
	return nil
}

func (m *Memory) DeleteConnection(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vars().conns[id] == nil {
		return ErrNotFound
	}
	delete(m.vars().conns, id)
	return nil
}

func (m *Memory) SetRunRemark(_ context.Context, id, remark string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.runs[id]
	if r == nil {
		return ErrNotFound
	}
	r.Remark = remark
	return nil
}
