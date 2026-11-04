package store

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"flow/internal/model"

	"github.com/jackc/pgx/v5"
)

// 部署到 Agent 主机的单台任务。

const hostTaskCols = `id, tenant_id, job_id, host_id, script, env, target, upload_key, status, exit_code, error, lease_until,
	created_at, started_at, finished_at`

func scanHostTask(row pgx.Row) (*model.HostTask, error) {
	var t model.HostTask
	if err := row.Scan(&t.ID, &t.TenantID, &t.JobID, &t.HostID, &t.Script, &t.Env, &t.Target, &t.UploadKey, &t.Status,
		&t.ExitCode, &t.Error, &t.LeaseUntil, &t.CreatedAt, &t.StartedAt, &t.FinishedAt); err != nil {
		return nil, mapErr(err)
	}
	t.HasUpload = t.UploadKey != ""
	return &t, nil
}

func (s pgq) CreateHostTask(ctx context.Context, t *model.HostTask) error {
	_, err := s.q.Exec(ctx, `INSERT INTO host_tasks (`+hostTaskCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		t.ID, t.TenantID, t.JobID, t.HostID, t.Script, nonNilMap(t.Env), t.Target, t.UploadKey, t.Status, t.ExitCode, t.Error,
		t.LeaseUntil, t.CreatedAt, t.StartedAt, t.FinishedAt)
	return mapErr(err)
}

func (s pgq) GetHostTask(ctx context.Context, id string) (*model.HostTask, error) {
	return scanHostTask(s.q.QueryRow(ctx, `SELECT `+hostTaskCols+` FROM host_tasks WHERE id = $1`, id))
}

// ClaimHostTask 领取主机最早排队的任务并设置租约。
func (s pgq) ClaimHostTask(ctx context.Context, hostID string, now, lease time.Time) (*model.HostTask, error) {
	t, err := scanHostTask(s.q.QueryRow(ctx, `UPDATE host_tasks SET status = 'running', started_at = $2, lease_until = $3
		WHERE id = (SELECT id FROM host_tasks WHERE host_id = $1 AND status = 'queued' ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+hostTaskCols, hostID, now, lease))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return t, err
}

func (s pgq) TouchHostTask(ctx context.Context, id string, lease time.Time) error {
	return expectRow(s.q.Exec(ctx, `UPDATE host_tasks SET lease_until = $2 WHERE id = $1 AND status = 'running'`, id, lease))
}

func (s pgq) AppendHostTaskLog(ctx context.Context, id string, data []byte) error {
	return expectRow(s.q.Exec(ctx, `UPDATE host_tasks SET log = log || $2 WHERE id = $1`, id, data))
}

func (s pgq) ReadHostTaskLog(ctx context.Context, id string, offset int) ([]byte, error) {
	var data []byte
	err := s.q.QueryRow(ctx, `SELECT substring(log FROM $2::int + 1) FROM host_tasks WHERE id = $1`, id, max(offset, 0)).Scan(&data)
	return data, mapErr(err)
}

// FinishHostTask 结束任务（只对未结束的生效，重复上报无副作用）。
func (s pgq) FinishHostTask(ctx context.Context, id string, status model.Status, exitCode int, errMsg string, at time.Time) error {
	_, err := s.q.Exec(ctx, `UPDATE host_tasks SET status = $2, exit_code = $3, error = $4, finished_at = $5, lease_until = NULL
		WHERE id = $1 AND finished_at IS NULL`, id, status, exitCode, errMsg, at)
	return err
}

// ExpireHostTasks 把租约过期（Agent 失联）的任务置为失败，返回数量。
func (s pgq) ExpireHostTasks(ctx context.Context, now time.Time) (int, error) {
	tag, err := s.q.Exec(ctx, `UPDATE host_tasks SET status = 'failed', error = 'agent lost', finished_at = $1, lease_until = NULL
		WHERE status = 'running' AND lease_until < $1`, now)
	return int(tag.RowsAffected()), err
}

// ---- 内存实现 ----

func (m *Memory) tasks() map[string]*model.HostTask {
	if m.hostTasks == nil {
		m.hostTasks = map[string]*model.HostTask{}
	}
	return m.hostTasks
}

func cloneTask(t *model.HostTask) *model.HostTask {
	c := *t
	c.Env, c.Log = maps.Clone(t.Env), slices.Clone(t.Log)
	c.HasUpload = c.UploadKey != ""
	return &c
}

func (m *Memory) CreateHostTask(_ context.Context, t *model.HostTask) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs[t.JobID] == nil {
		return ErrNotFound
	}
	m.tasks()[t.ID] = cloneTask(t)
	return nil
}

func (m *Memory) GetHostTask(_ context.Context, id string) (*model.HostTask, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t := m.tasks()[id]
	if t == nil {
		return nil, ErrNotFound
	}
	return cloneTask(t), nil
}

func (m *Memory) ClaimHostTask(_ context.Context, hostID string, now, lease time.Time) (*model.HostTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *model.HostTask
	for _, t := range m.tasks() {
		if t.HostID == hostID && t.Status == model.StatusQueued && (best == nil || t.CreatedAt.Before(best.CreatedAt)) {
			best = t
		}
	}
	if best == nil {
		return nil, nil
	}
	best.Status, best.StartedAt, best.LeaseUntil = model.StatusRunning, &now, &lease
	return cloneTask(best), nil
}

func (m *Memory) TouchHostTask(_ context.Context, id string, lease time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks()[id]
	if t == nil || t.Status != model.StatusRunning {
		return ErrNotFound
	}
	t.LeaseUntil = &lease
	return nil
}

func (m *Memory) AppendHostTaskLog(_ context.Context, id string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks()[id]
	if t == nil {
		return ErrNotFound
	}
	t.Log = append(t.Log, data...)
	return nil
}

func (m *Memory) ReadHostTaskLog(_ context.Context, id string, offset int) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t := m.tasks()[id]
	if t == nil {
		return nil, ErrNotFound
	}
	if offset >= len(t.Log) {
		return []byte{}, nil
	}
	return slices.Clone(t.Log[max(offset, 0):]), nil
}

func (m *Memory) FinishHostTask(_ context.Context, id string, status model.Status, exitCode int, errMsg string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks()[id]
	if t == nil || t.FinishedAt != nil {
		return nil
	}
	t.Status, t.ExitCode, t.Error, t.FinishedAt, t.LeaseUntil = status, exitCode, errMsg, &at, nil
	return nil
}

func (m *Memory) ExpireHostTasks(_ context.Context, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, t := range m.tasks() {
		if t.Status == model.StatusRunning && t.LeaseUntil != nil && t.LeaseUntil.Before(now) {
			t.Status, t.Error, t.FinishedAt, t.LeaseUntil = model.StatusFailed, "agent lost", &now, nil
			n++
		}
	}
	return n, nil
}
