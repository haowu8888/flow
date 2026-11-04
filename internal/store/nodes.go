package store

import (
	"context"
	"slices"

	"flow/internal/model"
)

// 构建节点：按类型列出 Runner、查找指定节点上排队的任务（GitHub 节点按需触发工作流）。

func (s pgq) ListRunnersByKind(ctx context.Context, kind string) ([]*model.Runner, error) {
	rows, err := s.q.Query(ctx, `SELECT `+runnerCols+` FROM runners WHERE kind = $1 ORDER BY created_at, id`, kind)
	return collect(rows, err, scanRunner)
}

func (s pgq) QueuedJobsWithLabel(ctx context.Context, tenantID, label string, limit int) ([]*model.Job, error) {
	rows, err := s.q.Query(ctx, `SELECT `+jobCols+` FROM jobs WHERE status = 'queued' AND tenant_id = $1 AND $2 = ANY(runs_on)
		ORDER BY created_at, seq LIMIT $3`, tenantID, label, limit)
	return collect(rows, err, scanJob)
}

func (m *Memory) ListRunnersByKind(_ context.Context, kind string) ([]*model.Runner, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Runner, 0)
	for _, id := range m.runnerOrder {
		if r := m.runners[id]; r.Kind == kind {
			c := clone(r)
			c.Labels = slices.Clone(r.Labels)
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *Memory) QueuedJobsWithLabel(_ context.Context, tenantID, label string, limit int) ([]*model.Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*model.Job
	for _, id := range m.jobOrder {
		j := m.jobs[id]
		if j.Status == model.StatusQueued && j.TenantID == tenantID && slices.Contains(j.RunsOn, label) {
			out = append(out, j.Clone())
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
