package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"flow/internal/model"
	"flow/migrations"
)

const migrationLockKey int64 = 7_352_001

// Migrate 按文件名顺序执行 migrations/*.sql 中尚未应用的脚本；advisory lock 防止多实例并发迁移。
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.FS.ReadFile(name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

// Postgres 是生产存储。所有方法由 pgq 提供，事务内与事务外共用同一套 SQL。
type Postgres struct {
	pgq
	pool *pgxpool.Pool
}

func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pgq: pgq{q: pool}, pool: pool}
}

func (p *Postgres) Atomic(ctx context.Context, fn func(Tx) error) error {
	const maxAttempts = 5
	for attempt := 1; ; attempt++ {
		err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error { return fn(pgq{q: tx}) })
		if err == nil || attempt == maxAttempts || !retryable(err) {
			return err
		}
	}
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

type pgq struct{ q querier }

const (
	pipelineCols = `id, tenant_id, name, yaml, created_at, updated_at, version, updated_by, group_name,
		created_by, description, archived, deleted_at, is_public`
	runCols = `id, tenant_id, pipeline_id, number, status, trigger, env, created_at, started_at, finished_at,
		created_by, notify, notify_done, pipeline_version, git_commit, rollback_of, concurrency_group, artifacts_from, remark, concurrency_max`
	jobCols = `id, tenant_id, run_id, seq, name, stage, stage_index, needs, image, env, steps, status,
		runner_id, lease_until, cancel_requested, exit_code, error, created_at, started_at, finished_at,
		runs_on, options, attempt, decided_by, step_states, host_states, outputs, notify_waiting, notify_done, dispatched_at`
	triggerCols = `id, tenant_id, pipeline_id, kind, enabled, config, secret, next_run_at, last_fired_at, last_error,
		created_at, updated_at`
)

// 流水线与修订记录用单条带 CTE 的语句写入，无需外层事务也是原子的。
func (s pgq) CreatePipeline(ctx context.Context, p *model.Pipeline) error {
	_, err := s.q.Exec(ctx, `WITH p AS (
			INSERT INTO pipelines (`+pipelineCols+`) VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $8, $9, $10, FALSE, NULL, $11) RETURNING id
		)
		INSERT INTO pipeline_revisions (pipeline_id, version, name, yaml, created_by, created_at)
		SELECT id, 1, $3, $4, $7, $5 FROM p`,
		p.ID, p.TenantID, p.Name, p.YAML, p.CreatedAt, p.UpdatedAt, p.UpdatedBy, p.Group, p.CreatedBy, p.Description, p.IsPublic)
	if err != nil {
		return mapErr(err)
	}
	p.Version = 1
	return nil
}

func (s pgq) UpdatePipeline(ctx context.Context, p *model.Pipeline) error {
	err := s.q.QueryRow(ctx, `WITH p AS (
			UPDATE pipelines SET name = $2, yaml = $3, updated_at = $4, updated_by = $5, version = version + 1
			WHERE id = $1 AND version = $6 AND deleted_at IS NULL
			RETURNING version, group_name, created_by, description, archived, is_public
		)
		INSERT INTO pipeline_revisions (pipeline_id, version, name, yaml, created_by, created_at, restored_from)
		SELECT $1, version, $2, $3, $5, $4, $7 FROM p
		RETURNING (SELECT group_name FROM p), (SELECT created_by FROM p), (SELECT description FROM p), (SELECT archived FROM p), (SELECT is_public FROM p)`,
		p.ID, p.Name, p.YAML, p.UpdatedAt, p.UpdatedBy, p.Version, p.RestoredFrom).Scan(&p.Group, &p.CreatedBy, &p.Description, &p.Archived, &p.IsPublic)
	if errors.Is(err, pgx.ErrNoRows) {
		cur, err := s.GetPipeline(ctx, p.ID)
		if err != nil {
			return err
		}
		if cur.DeletedAt != nil {
			return ErrNotFound
		}
		return ErrConflict
	}
	if err != nil {
		return mapErr(err)
	}
	p.Version++
	return nil
}

func (s pgq) SetPipelineGroup(ctx context.Context, id, group string) error {
	tag, err := s.q.Exec(ctx, `UPDATE pipelines SET group_name = $2 WHERE id = $1`, id, group)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s pgq) SetPipelineInfo(ctx context.Context, id, description string, archived bool) error {
	tag, err := s.q.Exec(ctx, `UPDATE pipelines SET description = $2, archived = $3 WHERE id = $1`, id, description, archived)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s pgq) SetPipelineDeleted(ctx context.Context, id string, at *time.Time) error {
	tag, err := s.q.Exec(ctx, `UPDATE pipelines SET deleted_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s pgq) ListDeletedPipelines(ctx context.Context, tenantID string) ([]*model.Pipeline, error) {
	rows, err := s.q.Query(ctx, `SELECT `+pipelineCols+` FROM pipelines WHERE tenant_id = $1 AND deleted_at IS NOT NULL
		ORDER BY deleted_at DESC, id`, tenantID)
	return collect(rows, err, scanPipeline)
}

func (s pgq) SetFavorite(ctx context.Context, userID, pipelineID string, on bool, at time.Time) error {
	if !on {
		_, err := s.q.Exec(ctx, `DELETE FROM pipeline_favorites WHERE user_id = $1 AND pipeline_id = $2`, userID, pipelineID)
		return err
	}
	_, err := s.q.Exec(ctx, `INSERT INTO pipeline_favorites (user_id, pipeline_id, created_at) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, userID, pipelineID, at)
	return mapErr(err)
}

func (s pgq) ListFavorites(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.q.Query(ctx, `SELECT pipeline_id FROM pipeline_favorites WHERE user_id = $1 ORDER BY pipeline_id`, userID)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if ids == nil {
		ids = []string{}
	}
	return ids, err
}

func (s pgq) RecentRuns(ctx context.Context, tenantID string, perPipeline int) (map[string][]*model.Run, error) {
	rows, err := s.q.Query(ctx, `SELECT `+runCols+` FROM (
			SELECT *, row_number() OVER (PARTITION BY pipeline_id ORDER BY created_at DESC, number DESC) AS rn
			FROM runs WHERE tenant_id = $1
		) r WHERE rn <= $2 ORDER BY pipeline_id, created_at DESC, number DESC`, tenantID, perPipeline)
	runs, err := collect(rows, err, scanRun)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]*model.Run)
	for _, r := range runs {
		out[r.PipelineID] = append(out[r.PipelineID], r)
	}
	return out, nil
}

func (s pgq) ListRevisions(ctx context.Context, pipelineID string, limit int) ([]*model.PipelineRevision, error) {
	rows, err := s.q.Query(ctx, `SELECT pipeline_id, version, name, '', created_by, created_at, restored_from FROM pipeline_revisions
		WHERE pipeline_id = $1 ORDER BY version DESC LIMIT $2`, pipelineID, limit)
	return collect(rows, err, scanRevision)
}

func (s pgq) GetRevision(ctx context.Context, pipelineID string, version int) (*model.PipelineRevision, error) {
	return scanRevision(s.q.QueryRow(ctx, `SELECT pipeline_id, version, name, yaml, created_by, created_at, restored_from
		FROM pipeline_revisions WHERE pipeline_id = $1 AND version = $2`, pipelineID, version))
}

func (s pgq) GetPipeline(ctx context.Context, id string) (*model.Pipeline, error) {
	return scanPipeline(s.q.QueryRow(ctx, `SELECT `+pipelineCols+` FROM pipelines WHERE id = $1`, id))
}

func (s pgq) ListPipelines(ctx context.Context, tenantID string) ([]*model.Pipeline, error) {
	rows, err := s.q.Query(ctx, `SELECT `+pipelineCols+` FROM pipelines WHERE tenant_id = $1 AND deleted_at IS NULL ORDER BY created_at, id`, tenantID)
	return collect(rows, err, scanPipeline)
}

func (s pgq) CreateRun(ctx context.Context, r *model.Run, jobs []*model.Job) error {
	// 锁住流水线行以串行化同一流水线的 Run 编号分配。
	if err := s.q.QueryRow(ctx, `SELECT 1 FROM pipelines WHERE id = $1 FOR UPDATE`, r.PipelineID).Scan(new(int)); err != nil {
		return mapErr(err)
	}
	if err := s.q.QueryRow(ctx, `SELECT COALESCE(MAX(number), 0) + 1 FROM runs WHERE pipeline_id = $1`, r.PipelineID).Scan(&r.Number); err != nil {
		return err
	}
	if _, err := s.q.Exec(ctx, `INSERT INTO runs (`+runCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		r.ID, r.TenantID, r.PipelineID, r.Number, r.Status, r.Trigger, nonNilMap(r.Env), r.CreatedAt, r.StartedAt, r.FinishedAt,
		r.CreatedBy, nonNil(r.Notify), r.NotifyDone, r.PipelineVersion, r.Commit, r.RollbackOf, r.ConcurrencyGroup, r.ArtifactsFrom, r.Remark, r.ConcurrencyMax); err != nil {
		return mapErr(err)
	}
	b := &pgx.Batch{}
	for _, j := range jobs {
		b.Queue(`INSERT INTO jobs (`+jobCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
			$11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30)`,
			j.ID, j.TenantID, j.RunID, j.Seq, j.Name, j.Stage, j.StageIndex, nonNil(j.Needs), j.Image,
			nonNilMap(j.Env), nonNil(j.Steps), j.Status, j.RunnerID, nullTime(j.LeaseUntil),
			j.CancelRequested, j.ExitCode, j.Error, j.CreatedAt, j.StartedAt, j.FinishedAt,
			nonNil(j.RunsOn), j.JobOptions, j.Attempt, j.DecidedBy, nonNil(j.StepStates), nonNil(j.HostStates),
			nonNilMap(j.Outputs), j.NotifyWaiting, j.NotifyDone, j.DispatchedAt)
	}
	return mapErr(s.q.SendBatch(ctx, b).Close())
}

func (s pgq) GetRun(ctx context.Context, id string) (*model.Run, error) {
	return scanRun(s.q.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1`, id))
}

func (s pgq) LockRun(ctx context.Context, id string) (*model.Run, error) {
	return scanRun(s.q.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1 FOR UPDATE`, id))
}

func (s pgq) ListRuns(ctx context.Context, pipelineID string, limit int) ([]*model.Run, error) {
	rows, err := s.q.Query(ctx, `SELECT `+runCols+` FROM runs WHERE pipeline_id = $1
		ORDER BY created_at DESC, number DESC LIMIT $2`, pipelineID, limit)
	return collect(rows, err, scanRun)
}

func (s pgq) LockConcurrency(ctx context.Context, tenantID, group string) error {
	_, err := s.q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('concurrency/' || $1 || '/' || $2, 0))`, tenantID, group)
	return err
}

func (s pgq) ActiveRuns(ctx context.Context, tenantID, group string) ([]*model.Run, error) {
	rows, err := s.q.Query(ctx, `SELECT `+runCols+` FROM runs
		WHERE tenant_id = $1 AND concurrency_group = $2 AND finished_at IS NULL ORDER BY created_at, number`, tenantID, group)
	return collect(rows, err, scanRun)
}

func (s pgq) UpdateRun(ctx context.Context, r *model.Run) error {
	tag, err := s.q.Exec(ctx, `UPDATE runs SET status = $2, started_at = $3, finished_at = $4, notify_done = $5, git_commit = $6
		WHERE id = $1`, r.ID, r.Status, r.StartedAt, r.FinishedAt, r.NotifyDone, r.Commit)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s pgq) GetJob(ctx context.Context, id string) (*model.Job, error) {
	return scanJob(s.q.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = $1`, id))
}

func (s pgq) ListJobs(ctx context.Context, runID string) ([]*model.Job, error) {
	rows, err := s.q.Query(ctx, `SELECT `+jobCols+` FROM jobs WHERE run_id = $1 ORDER BY seq`, runID)
	return collect(rows, err, scanJob)
}

func (s pgq) NextQueuedJob(ctx context.Context, f ClaimFilter) (*model.Job, error) {
	j, err := scanJob(s.q.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs
		WHERE status = 'queued' AND ($1 = '' OR tenant_id = $1) AND runs_on <@ $2::text[]
		ORDER BY created_at, seq LIMIT 1 FOR UPDATE SKIP LOCKED`, f.TenantID, nonNil(f.Labels)))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return j, err
}

func (s pgq) ExpiredJobRunIDs(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.q.Query(ctx, `SELECT DISTINCT run_id FROM jobs WHERE status = 'running' AND lease_until < $1`, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s pgq) UpdateJob(ctx context.Context, j *model.Job) error {
	tag, err := s.q.Exec(ctx, `UPDATE jobs SET status = $2, runner_id = $3, lease_until = $4, cancel_requested = $5,
		exit_code = $6, error = $7, started_at = $8, finished_at = $9, attempt = $10, decided_by = $11, step_states = $12,
		host_states = $13, outputs = $14, notify_waiting = $15, notify_done = $16, dispatched_at = $17 WHERE id = $1`,
		j.ID, j.Status, j.RunnerID, nullTime(j.LeaseUntil), j.CancelRequested, j.ExitCode, j.Error, j.StartedAt, j.FinishedAt,
		j.Attempt, j.DecidedBy, nonNil(j.StepStates), nonNil(j.HostStates), nonNilMap(j.Outputs), j.NotifyWaiting, j.NotifyDone, j.DispatchedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AppendLog 以当前日志末尾为起始偏移追加一块；(job_id, byte_start) 唯一约束兜底并发写入。
func (s pgq) AppendLog(ctx context.Context, jobID string, data []byte) error {
	_, err := s.q.Exec(ctx, `INSERT INTO job_log_chunks (job_id, byte_start, data)
		SELECT $1, COALESCE((SELECT byte_start + length(data) FROM job_log_chunks
		                     WHERE job_id = $1 ORDER BY byte_start DESC LIMIT 1), 0), $2`, jobID, data)
	return mapErr(err)
}

func (s pgq) ReadLog(ctx context.Context, jobID string, offset int) ([]byte, error) {
	var exists bool
	if err := s.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1)`, jobID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	offset = max(offset, 0)
	rows, err := s.q.Query(ctx, `SELECT byte_start, data FROM job_log_chunks
		WHERE job_id = $1 AND byte_start + length(data) > $2 ORDER BY byte_start`, jobID, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []byte{}
	for rows.Next() && len(out) < MaxLogRead {
		var start int64
		var data []byte
		if err := rows.Scan(&start, &data); err != nil {
			return nil, err
		}
		if skip := offset - int(start); skip > 0 {
			data = data[skip:]
		}
		out = append(out, data...)
	}
	return out, rows.Err()
}

func (s pgq) CreateTrigger(ctx context.Context, t *model.Trigger) error {
	_, err := s.q.Exec(ctx, `INSERT INTO triggers (`+triggerCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		t.ID, t.TenantID, t.PipelineID, t.Kind, t.Enabled, t.TriggerConfig, t.Secret, t.NextRunAt, t.LastFiredAt,
		t.LastError, t.CreatedAt, t.UpdatedAt)
	return mapErr(err)
}

func (s pgq) UpdateTrigger(ctx context.Context, t *model.Trigger) error {
	return expectRow(s.q.Exec(ctx, `UPDATE triggers SET enabled = $2, config = $3, secret = $4, next_run_at = $5, updated_at = $6
		WHERE id = $1`, t.ID, t.Enabled, t.TriggerConfig, t.Secret, t.NextRunAt, t.UpdatedAt))
}

func (s pgq) DeleteTrigger(ctx context.Context, id string) error {
	return expectRow(s.q.Exec(ctx, `DELETE FROM triggers WHERE id = $1`, id))
}

func (s pgq) GetTrigger(ctx context.Context, id string) (*model.Trigger, error) {
	return scanTrigger(s.q.QueryRow(ctx, `SELECT `+triggerCols+` FROM triggers WHERE id = $1`, id))
}

func (s pgq) ListTriggers(ctx context.Context, pipelineID string) ([]*model.Trigger, error) {
	rows, err := s.q.Query(ctx, `SELECT `+triggerCols+` FROM triggers WHERE pipeline_id = $1 ORDER BY created_at, id`, pipelineID)
	return collect(rows, err, scanTrigger)
}

func (s pgq) NextDueTrigger(ctx context.Context, now time.Time) (*model.Trigger, error) {
	t, err := scanTrigger(s.q.QueryRow(ctx, `SELECT `+triggerCols+` FROM triggers
		WHERE kind = 'cron' AND enabled AND next_run_at <= $1
		ORDER BY next_run_at LIMIT 1 FOR UPDATE SKIP LOCKED`, now))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return t, err
}

func (s pgq) MarkTriggerFired(ctx context.Context, id string, next *time.Time, at time.Time, errMsg string) error {
	return expectRow(s.q.Exec(ctx, `UPDATE triggers SET next_run_at = $2, last_fired_at = $3, last_error = $4 WHERE id = $1`,
		id, next, at, errMsg))
}

func (s pgq) ClaimDelivery(ctx context.Context, triggerID, deliveryID string, at time.Time) (string, bool, error) {
	tag, err := s.q.Exec(ctx, `INSERT INTO webhook_deliveries (trigger_id, delivery_id, created_at) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, triggerID, deliveryID, at)
	if err != nil {
		return "", false, mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		return "", false, nil
	}
	var runID string
	err = s.q.QueryRow(ctx, `SELECT run_id FROM webhook_deliveries WHERE trigger_id = $1 AND delivery_id = $2`,
		triggerID, deliveryID).Scan(&runID)
	return runID, true, mapErr(err)
}

func (s pgq) SetDeliveryRun(ctx context.Context, triggerID, deliveryID, runID string) error {
	return expectRow(s.q.Exec(ctx, `UPDATE webhook_deliveries SET run_id = $3 WHERE trigger_id = $1 AND delivery_id = $2`,
		triggerID, deliveryID, runID))
}

func (s pgq) DeleteDeliveriesBefore(ctx context.Context, before time.Time) error {
	_, err := s.q.Exec(ctx, `DELETE FROM webhook_deliveries WHERE created_at < $1`, before)
	return mapErr(err)
}

func expectRow(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanTrigger(row pgx.Row) (*model.Trigger, error) {
	var t model.Trigger
	if err := row.Scan(&t.ID, &t.TenantID, &t.PipelineID, &t.Kind, &t.Enabled, &t.TriggerConfig, &t.Secret,
		&t.NextRunAt, &t.LastFiredAt, &t.LastError, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

func scanPipeline(row pgx.Row) (*model.Pipeline, error) {
	var p model.Pipeline
	if err := row.Scan(&p.ID, &p.TenantID, &p.Name, &p.YAML, &p.CreatedAt, &p.UpdatedAt, &p.Version, &p.UpdatedBy, &p.Group,
		&p.CreatedBy, &p.Description, &p.Archived, &p.DeletedAt, &p.IsPublic); err != nil {
		return nil, mapErr(err)
	}
	return &p, nil
}

func scanRevision(row pgx.Row) (*model.PipelineRevision, error) {
	var r model.PipelineRevision
	if err := row.Scan(&r.PipelineID, &r.Version, &r.Name, &r.YAML, &r.CreatedBy, &r.CreatedAt, &r.RestoredFrom); err != nil {
		return nil, mapErr(err)
	}
	return &r, nil
}

func scanRun(row pgx.Row) (*model.Run, error) {
	var r model.Run
	if err := row.Scan(&r.ID, &r.TenantID, &r.PipelineID, &r.Number, &r.Status, &r.Trigger, &r.Env,
		&r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.CreatedBy, &r.Notify, &r.NotifyDone, &r.PipelineVersion,
		&r.Commit, &r.RollbackOf, &r.ConcurrencyGroup, &r.ArtifactsFrom, &r.Remark, &r.ConcurrencyMax); err != nil {
		return nil, mapErr(err)
	}
	return &r, nil
}

func scanJob(row pgx.Row) (*model.Job, error) {
	var j model.Job
	var lease *time.Time
	if err := row.Scan(&j.ID, &j.TenantID, &j.RunID, &j.Seq, &j.Name, &j.Stage, &j.StageIndex, &j.Needs,
		&j.Image, &j.Env, &j.Steps, &j.Status, &j.RunnerID, &lease, &j.CancelRequested, &j.ExitCode,
		&j.Error, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.RunsOn, &j.JobOptions, &j.Attempt, &j.DecidedBy, &j.StepStates, &j.HostStates,
		&j.Outputs, &j.NotifyWaiting, &j.NotifyDone, &j.DispatchedAt); err != nil {
		return nil, mapErr(err)
	}
	if lease != nil {
		j.LeaseUntil = *lease
	}
	return &j, nil
}

func collect[T any](rows pgx.Rows, err error, scan func(pgx.Row) (*T, error)) ([]*T, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*T, 0)
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %s", ErrConflict, pgErr.ConstraintName)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%w: %s", ErrNotFound, pgErr.ConstraintName)
		}
	}
	return err
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func nonNilMap[V any](m map[string]V) map[string]V {
	if m == nil {
		return map[string]V{}
	}
	return m
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
