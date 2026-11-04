package store

import (
	"context"
	"time"

	"flow/internal/model"
)

// RunsSince 返回流水线在 since 之后创建的运行（最多 limit 个，新的在前）。
func (s pgq) RunsSince(ctx context.Context, pipelineID string, since time.Time, limit int) ([]*model.Run, error) {
	rows, err := s.q.Query(ctx, `SELECT `+runCols+` FROM runs WHERE pipeline_id = $1 AND created_at >= $2
		ORDER BY created_at DESC, number DESC LIMIT $3`, pipelineID, since, limit)
	return collect(rows, err, scanRun)
}

// JobsOfRuns 返回这些运行的全部 Job。
func (s pgq) JobsOfRuns(ctx context.Context, runIDs []string) ([]*model.Job, error) {
	rows, err := s.q.Query(ctx, `SELECT `+jobCols+` FROM jobs WHERE run_id = ANY($1) ORDER BY run_id, seq`, nonNil(runIDs))
	return collect(rows, err, scanJob)
}

// TestSummaries 汇总这些运行的测试数量（不含用例），键为运行 ID。
func (s pgq) TestSummaries(ctx context.Context, runIDs []string) (map[string]model.TestSummary, error) {
	rows, err := s.q.Query(ctx, `SELECT run_id, SUM(total), SUM(passed), SUM(failed), SUM(skipped), SUM(duration)
		FROM test_reports WHERE run_id = ANY($1) GROUP BY run_id`, nonNil(runIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]model.TestSummary{}
	for rows.Next() {
		var id string
		var t model.TestSummary
		if err := rows.Scan(&id, &t.Total, &t.Passed, &t.Failed, &t.Skipped, &t.Duration); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

func (m *Memory) RunsSince(_ context.Context, pipelineID string, since time.Time, limit int) ([]*model.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Run, 0)
	for i := len(m.runOrder) - 1; i >= 0 && len(out) < limit; i-- {
		if r := m.runs[m.runOrder[i]]; r.PipelineID == pipelineID && !r.CreatedAt.Before(since) {
			out = append(out, r.Clone())
		}
	}
	return out, nil
}

func (m *Memory) JobsOfRuns(_ context.Context, runIDs []string) ([]*model.Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Job, 0)
	for _, rid := range runIDs {
		for _, jid := range m.runJobs[rid] {
			out = append(out, m.jobs[jid].Clone())
		}
	}
	return out, nil
}

func (m *Memory) TestSummaries(_ context.Context, runIDs []string) (map[string]model.TestSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	want := map[string]bool{}
	for _, id := range runIDs {
		want[id] = true
	}
	out := map[string]model.TestSummary{}
	for _, r := range m.reports {
		if want[r.RunID] {
			t := out[r.RunID]
			t.Total, t.Passed, t.Failed, t.Skipped, t.Duration = t.Total+r.Total, t.Passed+r.Passed, t.Failed+r.Failed, t.Skipped+r.Skipped, t.Duration+r.Duration
			out[r.RunID] = t
		}
	}
	return out, nil
}

// RunFilter 限定运行查询；空字段不限制。BeforeNumber 用于翻页（返回编号小于它的运行）。
type RunFilter struct {
	Status       model.Status
	Trigger      string
	BeforeNumber int
	Limit        int
}

func (s pgq) QueryRuns(ctx context.Context, pipelineID string, f RunFilter) ([]*model.Run, error) {
	rows, err := s.q.Query(ctx, `SELECT `+runCols+` FROM runs WHERE pipeline_id = $1
		AND ($2 = '' OR status = $2) AND ($3 = '' OR trigger = $3) AND ($4 = 0 OR number < $4)
		ORDER BY number DESC LIMIT $5`, pipelineID, f.Status, f.Trigger, f.BeforeNumber, f.Limit)
	return collect(rows, err, scanRun)
}

// WaitingJobs 返回租户内等待审批的 Job（人工卡点与部署审批），按创建时间正序。
func (s pgq) WaitingJobs(ctx context.Context, tenantID string) ([]*model.Job, error) {
	rows, err := s.q.Query(ctx, `SELECT `+jobCols+` FROM jobs WHERE tenant_id = $1 AND status = 'waiting' ORDER BY created_at, seq`, tenantID)
	return collect(rows, err, scanJob)
}

func (m *Memory) QueryRuns(_ context.Context, pipelineID string, f RunFilter) ([]*model.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Run, 0)
	for i := len(m.runOrder) - 1; i >= 0 && len(out) < f.Limit; i-- {
		r := m.runs[m.runOrder[i]]
		if r.PipelineID == pipelineID && (f.Status == "" || r.Status == f.Status) && (f.Trigger == "" || r.Trigger == f.Trigger) &&
			(f.BeforeNumber == 0 || r.Number < f.BeforeNumber) {
			out = append(out, r.Clone())
		}
	}
	return out, nil
}

func (m *Memory) WaitingJobs(_ context.Context, tenantID string) ([]*model.Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Job, 0)
	for _, id := range m.jobOrder {
		if j := m.jobs[id]; j.TenantID == tenantID && j.Status == model.StatusWaiting {
			out = append(out, j.Clone())
		}
	}
	return out, nil
}
