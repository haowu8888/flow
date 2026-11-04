package store

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"flow/internal/model"
)

const reportCols = `job_id, run_id, tenant_id, job_name, total, passed, failed, skipped, duration, cases, created_at`

func scanReport(row pgx.Row) (*model.TestReport, error) {
	var r model.TestReport
	if err := row.Scan(&r.JobID, &r.RunID, &r.TenantID, &r.JobName, &r.Total, &r.Passed, &r.Failed, &r.Skipped,
		&r.Duration, &r.Cases, &r.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &r, nil
}

func (s pgq) PutTestReport(ctx context.Context, r *model.TestReport) error {
	_, err := s.q.Exec(ctx, `INSERT INTO test_reports (`+reportCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (job_id) DO UPDATE SET total = EXCLUDED.total, passed = EXCLUDED.passed, failed = EXCLUDED.failed,
			skipped = EXCLUDED.skipped, duration = EXCLUDED.duration, cases = EXCLUDED.cases, created_at = EXCLUDED.created_at`,
		r.JobID, r.RunID, r.TenantID, r.JobName, r.Total, r.Passed, r.Failed, r.Skipped, r.Duration, nonNil(r.Cases), r.CreatedAt)
	return mapErr(err)
}

func (s pgq) ListTestReports(ctx context.Context, runID string) ([]*model.TestReport, error) {
	rows, err := s.q.Query(ctx, `SELECT `+reportCols+` FROM test_reports WHERE run_id = $1 ORDER BY job_name`, runID)
	return collect(rows, err, scanReport)
}

func (m *Memory) PutTestReport(_ context.Context, r *model.TestReport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[r.JobID]; !ok {
		return ErrNotFound
	}
	if m.reports == nil {
		m.reports = map[string]*model.TestReport{}
	}
	c := clone(r)
	c.Cases = slices.Clone(r.Cases)
	m.reports[r.JobID] = c
	return nil
}

func (m *Memory) ListTestReports(_ context.Context, runID string) ([]*model.TestReport, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.TestReport, 0)
	for _, r := range m.reports {
		if r.RunID == runID {
			c := clone(r)
			c.Cases = slices.Clone(r.Cases)
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b *model.TestReport) int {
		if a.JobName < b.JobName {
			return -1
		}
		if a.JobName > b.JobName {
			return 1
		}
		return 0
	})
	return out, nil
}
