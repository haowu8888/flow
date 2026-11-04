package store

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"flow/internal/model"
)

// DeploymentFilter 限定部署记录查询；空字段不限制。
type DeploymentFilter struct {
	EnvironmentID string
	PipelineID    string
	Limit         int
}

// ---- PostgreSQL ----

const (
	envCols   = `id, tenant_id, name, description, rank, require_approval, approvers, created_at, updated_at, variables, secrets`
	groupCols = `id, tenant_id, name, description, COALESCE(environment_id, ''), created_at, updated_at`
	hostCols  = `id, tenant_id, name, address, port, username, auth_type, credential, description, host_key, group_ids,
		checked_at, check_error, created_at, updated_at, jump_host_id, mode`
	deployCols = `id, tenant_id, pipeline_id, pipeline_name, run_id, run_number, job_id, job_name, environment_id,
		environment_name, group_name, hosts, git_commit, status, created_by, started_at, finished_at`
)

func scanEnv(row pgx.Row) (*model.Environment, error) {
	var e model.Environment
	if err := row.Scan(&e.ID, &e.TenantID, &e.Name, &e.Description, &e.Rank, &e.RequireApproval, &e.Approvers,
		&e.CreatedAt, &e.UpdatedAt, &e.Variables, &e.Secrets); err != nil {
		return nil, mapErr(err)
	}
	return &e, nil
}

func scanGroup(row pgx.Row) (*model.HostGroup, error) {
	var g model.HostGroup
	if err := row.Scan(&g.ID, &g.TenantID, &g.Name, &g.Description, &g.EnvironmentID, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &g, nil
}

func scanHost(row pgx.Row) (*model.Host, error) {
	var h model.Host
	if err := row.Scan(&h.ID, &h.TenantID, &h.Name, &h.Address, &h.Port, &h.Username, &h.AuthType, &h.Credential,
		&h.Description, &h.HostKey, &h.GroupIDs, &h.CheckedAt, &h.CheckError, &h.CreatedAt, &h.UpdatedAt, &h.JumpHostID, &h.Mode); err != nil {
		return nil, mapErr(err)
	}
	return &h, nil
}

func scanDeployment(row pgx.Row) (*model.Deployment, error) {
	var d model.Deployment
	if err := row.Scan(&d.ID, &d.TenantID, &d.PipelineID, &d.PipelineName, &d.RunID, &d.RunNumber, &d.JobID, &d.JobName,
		&d.EnvironmentID, &d.EnvironmentName, &d.GroupName, &d.Hosts, &d.Commit, &d.Status, &d.CreatedBy,
		&d.StartedAt, &d.FinishedAt); err != nil {
		return nil, mapErr(err)
	}
	return &d, nil
}

func (s pgq) CreateEnvironment(ctx context.Context, e *model.Environment) error {
	_, err := s.q.Exec(ctx, `INSERT INTO environments (`+envCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		e.ID, e.TenantID, e.Name, e.Description, e.Rank, e.RequireApproval, nonNil(e.Approvers), e.CreatedAt, e.UpdatedAt,
		nonNilMap(e.Variables), nonNilMap(e.Secrets))
	return mapErr(err)
}

func (s pgq) UpdateEnvironment(ctx context.Context, e *model.Environment) error {
	return expectRow(s.q.Exec(ctx, `UPDATE environments SET name = $2, description = $3, rank = $4, require_approval = $5,
		approvers = $6, updated_at = $7, variables = $8, secrets = $9 WHERE id = $1`,
		e.ID, e.Name, e.Description, e.Rank, e.RequireApproval, nonNil(e.Approvers), e.UpdatedAt, nonNilMap(e.Variables), nonNilMap(e.Secrets)))
}

func (s pgq) DeleteEnvironment(ctx context.Context, id string) error {
	return expectRow(s.q.Exec(ctx, `DELETE FROM environments WHERE id = $1`, id))
}

func (s pgq) GetEnvironment(ctx context.Context, id string) (*model.Environment, error) {
	return scanEnv(s.q.QueryRow(ctx, `SELECT `+envCols+` FROM environments WHERE id = $1`, id))
}

func (s pgq) ListEnvironments(ctx context.Context, tenantID string) ([]*model.Environment, error) {
	rows, err := s.q.Query(ctx, `SELECT `+envCols+` FROM environments WHERE tenant_id = $1 ORDER BY rank, name`, tenantID)
	return collect(rows, err, scanEnv)
}

func (s pgq) CreateHostGroup(ctx context.Context, g *model.HostGroup) error {
	_, err := s.q.Exec(ctx, `INSERT INTO host_groups (id, tenant_id, name, description, environment_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7)`,
		g.ID, g.TenantID, g.Name, g.Description, g.EnvironmentID, g.CreatedAt, g.UpdatedAt)
	return mapErr(err)
}

func (s pgq) UpdateHostGroup(ctx context.Context, g *model.HostGroup) error {
	return expectRow(s.q.Exec(ctx, `UPDATE host_groups SET name = $2, description = $3, environment_id = NULLIF($4, ''),
		updated_at = $5 WHERE id = $1`, g.ID, g.Name, g.Description, g.EnvironmentID, g.UpdatedAt))
}

func (s pgq) DeleteHostGroup(ctx context.Context, id string) error {
	if _, err := s.q.Exec(ctx, `UPDATE hosts SET group_ids = array_remove(group_ids, $1) WHERE $1 = ANY (group_ids)`, id); err != nil {
		return err
	}
	return expectRow(s.q.Exec(ctx, `DELETE FROM host_groups WHERE id = $1`, id))
}

func (s pgq) GetHostGroup(ctx context.Context, id string) (*model.HostGroup, error) {
	return scanGroup(s.q.QueryRow(ctx, `SELECT `+groupCols+` FROM host_groups WHERE id = $1`, id))
}

func (s pgq) GetHostGroupByName(ctx context.Context, tenantID, name string) (*model.HostGroup, error) {
	return scanGroup(s.q.QueryRow(ctx, `SELECT `+groupCols+` FROM host_groups WHERE tenant_id = $1 AND name = $2`, tenantID, name))
}

func (s pgq) ListHostGroups(ctx context.Context, tenantID string) ([]*model.HostGroup, error) {
	rows, err := s.q.Query(ctx, `SELECT `+groupCols+` FROM host_groups WHERE tenant_id = $1 ORDER BY name`, tenantID)
	return collect(rows, err, scanGroup)
}

func (s pgq) CreateHost(ctx context.Context, h *model.Host) error {
	if h.Mode == "" {
		h.Mode = "ssh"
	}
	_, err := s.q.Exec(ctx, `INSERT INTO hosts (`+hostCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		h.ID, h.TenantID, h.Name, h.Address, h.Port, h.Username, h.AuthType, h.Credential, h.Description, h.HostKey,
		nonNil(h.GroupIDs), h.CheckedAt, h.CheckError, h.CreatedAt, h.UpdatedAt, h.JumpHostID, h.Mode)
	return mapErr(err)
}

func (s pgq) UpdateHost(ctx context.Context, h *model.Host) error {
	return expectRow(s.q.Exec(ctx, `UPDATE hosts SET name = $2, address = $3, port = $4, username = $5, auth_type = $6,
		credential = $7, description = $8, host_key = $9, group_ids = $10, checked_at = $11, check_error = $12, updated_at = $13,
		jump_host_id = $14 WHERE id = $1`, h.ID, h.Name, h.Address, h.Port, h.Username, h.AuthType, h.Credential, h.Description, h.HostKey,
		nonNil(h.GroupIDs), h.CheckedAt, h.CheckError, h.UpdatedAt, h.JumpHostID))
}

func (s pgq) DeleteHost(ctx context.Context, id string) error {
	return expectRow(s.q.Exec(ctx, `DELETE FROM hosts WHERE id = $1`, id))
}

func (s pgq) GetHost(ctx context.Context, id string) (*model.Host, error) {
	return scanHost(s.q.QueryRow(ctx, `SELECT `+hostCols+` FROM hosts WHERE id = $1`, id))
}

func (s pgq) ListHosts(ctx context.Context, tenantID string) ([]*model.Host, error) {
	rows, err := s.q.Query(ctx, `SELECT `+hostCols+` FROM hosts WHERE tenant_id = $1 ORDER BY name`, tenantID)
	return collect(rows, err, scanHost)
}

func (s pgq) CreateDeployment(ctx context.Context, d *model.Deployment) error {
	_, err := s.q.Exec(ctx, `INSERT INTO deployments (`+deployCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (job_id) DO UPDATE SET hosts = EXCLUDED.hosts, git_commit = EXCLUDED.git_commit,
			status = EXCLUDED.status, started_at = EXCLUDED.started_at, finished_at = NULL`,
		d.ID, d.TenantID, d.PipelineID, d.PipelineName, d.RunID, d.RunNumber, d.JobID, d.JobName, d.EnvironmentID,
		d.EnvironmentName, d.GroupName, nonNil(d.Hosts), d.Commit, d.Status, d.CreatedBy, d.StartedAt, d.FinishedAt)
	return mapErr(err)
}

func (s pgq) FinishDeployment(ctx context.Context, jobID string, status model.Status, at time.Time) error {
	_, err := s.q.Exec(ctx, `UPDATE deployments SET status = $2, finished_at = $3 WHERE job_id = $1`, jobID, status, at)
	return err
}

func (s pgq) ListDeployments(ctx context.Context, tenantID string, f DeploymentFilter) ([]*model.Deployment, error) {
	rows, err := s.q.Query(ctx, `SELECT `+deployCols+` FROM deployments
		WHERE tenant_id = $1 AND ($2 = '' OR environment_id = $2) AND ($3 = '' OR pipeline_id = $3)
		ORDER BY started_at DESC, id LIMIT $4`, tenantID, f.EnvironmentID, f.PipelineID, f.Limit)
	return collect(rows, err, scanDeployment)
}

// ---- 内存 ----

type hostData struct {
	envs        map[string]*model.Environment
	groups      map[string]*model.HostGroup
	hosts       map[string]*model.Host
	deployments []*model.Deployment
}

func (m *Memory) hd() *hostData {
	if m.hostData == nil {
		m.hostData = &hostData{envs: map[string]*model.Environment{}, groups: map[string]*model.HostGroup{}, hosts: map[string]*model.Host{}}
	}
	return m.hostData
}

func cloneEnv(e *model.Environment) *model.Environment {
	c := clone(e)
	c.Approvers, c.Variables, c.Secrets, c.SecretNames = slices.Clone(e.Approvers), maps.Clone(e.Variables), maps.Clone(e.Secrets), slices.Clone(e.SecretNames)
	return c
}

func sortedBy[T any](items []*T, less func(a, b *T) int) []*T {
	slices.SortFunc(items, less)
	return items
}

func (m *Memory) CreateEnvironment(_ context.Context, e *model.Environment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.hd()
	if m.tenants[e.TenantID] == nil {
		return ErrNotFound
	}
	for _, o := range d.envs {
		if o.ID == e.ID || o.TenantID == e.TenantID && o.Name == e.Name {
			return ErrConflict
		}
	}
	d.envs[e.ID] = cloneEnv(e)
	return nil
}

func (m *Memory) UpdateEnvironment(_ context.Context, e *model.Environment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.hd()
	old, ok := d.envs[e.ID]
	if !ok {
		return ErrNotFound
	}
	for _, o := range d.envs {
		if o.ID != e.ID && o.TenantID == old.TenantID && o.Name == e.Name {
			return ErrConflict
		}
	}
	c := clone(old)
	c.Name, c.Description, c.Rank, c.RequireApproval, c.Approvers, c.UpdatedAt = e.Name, e.Description, e.Rank, e.RequireApproval, slices.Clone(e.Approvers), e.UpdatedAt
	c.Variables, c.Secrets = maps.Clone(e.Variables), maps.Clone(e.Secrets)
	d.envs[e.ID] = c
	return nil
}

func (m *Memory) DeleteEnvironment(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.hd()
	if _, ok := d.envs[id]; !ok {
		return ErrNotFound
	}
	delete(d.envs, id)
	for _, g := range d.groups {
		if g.EnvironmentID == id {
			g.EnvironmentID = ""
		}
	}
	return nil
}

func (m *Memory) GetEnvironment(_ context.Context, id string) (*model.Environment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.hd().envs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneEnv(e), nil
}

func (m *Memory) ListEnvironments(_ context.Context, tenantID string) ([]*model.Environment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Environment, 0)
	for _, e := range m.hd().envs {
		if e.TenantID == tenantID {
			out = append(out, cloneEnv(e))
		}
	}
	return sortedBy(out, func(a, b *model.Environment) int {
		if a.Rank != b.Rank {
			return a.Rank - b.Rank
		}
		return strings.Compare(a.Name, b.Name)
	}), nil
}

func (m *Memory) CreateHostGroup(_ context.Context, g *model.HostGroup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.hd()
	if m.tenants[g.TenantID] == nil || g.EnvironmentID != "" && d.envs[g.EnvironmentID] == nil {
		return ErrNotFound
	}
	for _, o := range d.groups {
		if o.ID == g.ID || o.TenantID == g.TenantID && o.Name == g.Name {
			return ErrConflict
		}
	}
	d.groups[g.ID] = clone(g)
	return nil
}

func (m *Memory) UpdateHostGroup(_ context.Context, g *model.HostGroup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.hd()
	old, ok := d.groups[g.ID]
	if !ok || g.EnvironmentID != "" && d.envs[g.EnvironmentID] == nil {
		return ErrNotFound
	}
	for _, o := range d.groups {
		if o.ID != g.ID && o.TenantID == old.TenantID && o.Name == g.Name {
			return ErrConflict
		}
	}
	c := clone(old)
	c.Name, c.Description, c.EnvironmentID, c.UpdatedAt = g.Name, g.Description, g.EnvironmentID, g.UpdatedAt
	d.groups[g.ID] = c
	return nil
}

func (m *Memory) DeleteHostGroup(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.hd()
	if _, ok := d.groups[id]; !ok {
		return ErrNotFound
	}
	delete(d.groups, id)
	for _, h := range d.hosts {
		h.GroupIDs = slices.DeleteFunc(h.GroupIDs, func(x string) bool { return x == id })
	}
	return nil
}

func (m *Memory) GetHostGroup(_ context.Context, id string) (*model.HostGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.hd().groups[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(g), nil
}

func (m *Memory) GetHostGroupByName(_ context.Context, tenantID, name string) (*model.HostGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, g := range m.hd().groups {
		if g.TenantID == tenantID && g.Name == name {
			return clone(g), nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) ListHostGroups(_ context.Context, tenantID string) ([]*model.HostGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.HostGroup, 0)
	for _, g := range m.hd().groups {
		if g.TenantID == tenantID {
			out = append(out, clone(g))
		}
	}
	return sortedBy(out, func(a, b *model.HostGroup) int { return strings.Compare(a.Name, b.Name) }), nil
}

func (m *Memory) hostNameTaken(h *model.Host) bool {
	for _, o := range m.hd().hosts {
		if o.ID != h.ID && o.TenantID == h.TenantID && o.Name == h.Name {
			return true
		}
	}
	return false
}

func (m *Memory) CreateHost(_ context.Context, h *model.Host) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h.Mode == "" {
		h.Mode = "ssh"
	}
	d := m.hd()
	if m.tenants[h.TenantID] == nil {
		return ErrNotFound
	}
	if _, dup := d.hosts[h.ID]; dup || m.hostNameTaken(h) {
		return ErrConflict
	}
	d.hosts[h.ID] = h.Clone()
	return nil
}

func (m *Memory) UpdateHost(_ context.Context, h *model.Host) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.hd()
	old, ok := d.hosts[h.ID]
	if !ok {
		return ErrNotFound
	}
	if m.hostNameTaken(h) {
		return ErrConflict
	}
	c := h.Clone()
	c.TenantID, c.CreatedAt = old.TenantID, old.CreatedAt
	d.hosts[h.ID] = c
	return nil
}

func (m *Memory) DeleteHost(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.hd().hosts[id]; !ok {
		return ErrNotFound
	}
	delete(m.hd().hosts, id)
	return nil
}

func (m *Memory) GetHost(_ context.Context, id string) (*model.Host, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h, ok := m.hd().hosts[id]
	if !ok {
		return nil, ErrNotFound
	}
	return h.Clone(), nil
}

func (m *Memory) ListHosts(_ context.Context, tenantID string) ([]*model.Host, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Host, 0)
	for _, h := range m.hd().hosts {
		if h.TenantID == tenantID {
			out = append(out, h.Clone())
		}
	}
	return sortedBy(out, func(a, b *model.Host) int { return strings.Compare(a.Name, b.Name) }), nil
}

func cloneDeployment(d *model.Deployment) *model.Deployment {
	c := clone(d)
	c.Hosts = slices.Clone(d.Hosts)
	return c
}

func (m *Memory) CreateDeployment(_ context.Context, dep *model.Deployment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.hd()
	if i := slices.IndexFunc(d.deployments, func(x *model.Deployment) bool { return x.JobID == dep.JobID }); i >= 0 {
		old := d.deployments[i]
		old.Hosts, old.Commit, old.Status, old.StartedAt, old.FinishedAt = slices.Clone(dep.Hosts), dep.Commit, dep.Status, dep.StartedAt, nil
		return nil
	}
	d.deployments = append(d.deployments, cloneDeployment(dep))
	return nil
}

func (m *Memory) FinishDeployment(_ context.Context, jobID string, status model.Status, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.hd().deployments {
		if x.JobID == jobID {
			x.Status, x.FinishedAt = status, &at
		}
	}
	return nil
}

func (m *Memory) ListDeployments(_ context.Context, tenantID string, f DeploymentFilter) ([]*model.Deployment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Deployment, 0)
	for _, x := range m.hd().deployments {
		if x.TenantID == tenantID && (f.EnvironmentID == "" || x.EnvironmentID == f.EnvironmentID) &&
			(f.PipelineID == "" || x.PipelineID == f.PipelineID) {
			out = append(out, cloneDeployment(x))
		}
	}
	slices.SortStableFunc(out, func(a, b *model.Deployment) int { return b.StartedAt.Compare(a.StartedAt) })
	if len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}
