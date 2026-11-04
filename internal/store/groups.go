package store

import (
	"context"
	"slices"
	"time"

	"flow/internal/model"

	"github.com/jackc/pgx/v5"
)

// 流水线分组与单条运行记录的删除。

// ListGroups 返回租户的分组名（含只出现在流水线上的分组），按名称排序。
func (s pgq) ListGroups(ctx context.Context, tenantID string) ([]string, error) {
	rows, err := s.q.Query(ctx, `SELECT name FROM pipeline_groups WHERE tenant_id = $1
		UNION SELECT group_name FROM pipelines WHERE tenant_id = $1 AND group_name <> '' ORDER BY 1`, tenantID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s pgq) CreateGroup(ctx context.Context, tenantID, name string, at time.Time) error {
	_, err := s.q.Exec(ctx, `INSERT INTO pipeline_groups (tenant_id, name, created_at) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, tenantID, name, at)
	return mapErr(err)
}

// RenameGroup 重命名分组并更新其中的流水线；新名称已存在时合并。
func (s pgq) RenameGroup(ctx context.Context, tenantID, from, to string, at time.Time) error {
	// 单条语句完成，无需外层事务
	_, err := s.q.Exec(ctx, `WITH d AS (DELETE FROM pipeline_groups WHERE tenant_id = $1 AND name = $2),
		i AS (INSERT INTO pipeline_groups (tenant_id, name, created_at) VALUES ($1, $3, $4) ON CONFLICT DO NOTHING)
		UPDATE pipelines SET group_name = $3 WHERE tenant_id = $1 AND group_name = $2`, tenantID, from, to, at)
	return mapErr(err)
}

// DeleteGroup 删除分组，其中的流水线变为未分组。
func (s pgq) DeleteGroup(ctx context.Context, tenantID, name string) error {
	_, err := s.q.Exec(ctx, `WITH d AS (DELETE FROM pipeline_groups WHERE tenant_id = $1 AND name = $2)
		UPDATE pipelines SET group_name = '' WHERE tenant_id = $1 AND group_name = $2`, tenantID, name)
	return err
}

// DeleteRun 删除已结束的运行（连同任务、日志、产物记录），返回要清理的产物 blob key。
func (s pgq) DeleteRun(ctx context.Context, id string) ([]string, error) {
	var status model.Status
	if err := s.q.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, id).Scan(&status); err != nil {
		return nil, mapErr(err)
	}
	if !status.Terminal() {
		return nil, ErrConflict
	}
	rows, err := s.q.Query(ctx, `SELECT blob_key FROM artifacts WHERE run_id = $1`, id)
	if err != nil {
		return nil, err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	_, err = s.q.Exec(ctx, `DELETE FROM runs WHERE id = $1`, id)
	return keys, mapErr(err)
}

// ---- 内存实现 ----

func (m *Memory) ListGroups(_ context.Context, tenantID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	set := map[string]bool{}
	for k := range m.groups {
		if k[0] == tenantID {
			set[k[1]] = true
		}
	}
	for _, p := range m.pipelines {
		if p.TenantID == tenantID && p.Group != "" {
			set[p.Group] = true
		}
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	slices.Sort(out)
	return out, nil
}

func (m *Memory) CreateGroup(_ context.Context, tenantID, name string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.groups == nil {
		m.groups = map[[2]string]bool{}
	}
	m.groups[[2]string{tenantID, name}] = true
	return nil
}

func (m *Memory) RenameGroup(_ context.Context, tenantID, from, to string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.groups == nil {
		m.groups = map[[2]string]bool{}
	}
	delete(m.groups, [2]string{tenantID, from})
	m.groups[[2]string{tenantID, to}] = true
	for _, p := range m.pipelines {
		if p.TenantID == tenantID && p.Group == from {
			p.Group = to
		}
	}
	return nil
}

func (m *Memory) DeleteGroup(_ context.Context, tenantID, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.groups, [2]string{tenantID, name})
	for _, p := range m.pipelines {
		if p.TenantID == tenantID && p.Group == name {
			p.Group = ""
		}
	}
	return nil
}

func (m *Memory) DeleteRun(_ context.Context, id string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.runs[id]
	if r == nil {
		return nil, ErrNotFound
	}
	if !r.Status.Terminal() {
		return nil, ErrConflict
	}
	blobs := m.deleteRunData(id)
	m.runOrder = slices.DeleteFunc(m.runOrder, func(x string) bool { return x == id })
	return blobs, nil
}
