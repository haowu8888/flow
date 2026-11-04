package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"flow/internal/model"
)

// 本文件为流水线删除、通知、密钥、Runner、产物、租户 / 用户 / 令牌的 PostgreSQL 实现。

func (s pgq) DeletePipeline(ctx context.Context, id string) ([]string, error) {
	rows, err := s.q.Query(ctx, `SELECT a.blob_key FROM artifacts a JOIN runs r ON r.id = a.run_id WHERE r.pipeline_id = $1`, id)
	if err != nil {
		return nil, err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	// secrets.pipeline_id 与 runs 等均为 ON DELETE CASCADE。
	if err := expectRow(s.q.Exec(ctx, `DELETE FROM pipelines WHERE id = $1`, id)); err != nil {
		return nil, err
	}
	return keys, nil
}

func (s pgq) NextPendingNotify(ctx context.Context) (*model.Run, error) {
	r, err := scanRun(s.q.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE NOT notify_done
		AND status IN ('success', 'failed', 'canceled') ORDER BY finished_at LIMIT 1 FOR UPDATE SKIP LOCKED`))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return r, err
}

const secretCols = `id, tenant_id, COALESCE(pipeline_id, ''), name, value_encrypted, created_at, updated_at`

func (s pgq) PutSecret(ctx context.Context, sec *model.Secret) error {
	return mapErr(s.q.QueryRow(ctx, `INSERT INTO secrets (id, tenant_id, pipeline_id, name, value_encrypted, created_at, updated_at)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7)
		ON CONFLICT (tenant_id, pipeline_id, name) DO UPDATE SET value_encrypted = EXCLUDED.value_encrypted, updated_at = EXCLUDED.updated_at
		RETURNING id, created_at`,
		sec.ID, sec.TenantID, sec.PipelineID, sec.Name, sec.Value, sec.CreatedAt, sec.UpdatedAt).Scan(&sec.ID, &sec.CreatedAt))
}

func (s pgq) DeleteSecret(ctx context.Context, tenantID, pipelineID, name string) error {
	return expectRow(s.q.Exec(ctx, `DELETE FROM secrets WHERE tenant_id = $1 AND pipeline_id IS NOT DISTINCT FROM NULLIF($2, '') AND name = $3`,
		tenantID, pipelineID, name))
}

func (s pgq) ListSecrets(ctx context.Context, tenantID, pipelineID string) ([]*model.Secret, error) {
	rows, err := s.q.Query(ctx, `SELECT `+secretCols+` FROM secrets
		WHERE tenant_id = $1 AND pipeline_id IS NOT DISTINCT FROM NULLIF($2, '') ORDER BY name`, tenantID, pipelineID)
	return collect(rows, err, func(row pgx.Row) (*model.Secret, error) {
		var x model.Secret
		if err := row.Scan(&x.ID, &x.TenantID, &x.PipelineID, &x.Name, &x.Value, &x.CreatedAt, &x.UpdatedAt); err != nil {
			return nil, mapErr(err)
		}
		return &x, nil
	})
}

const runnerCols = `id, tenant_id, name, labels, token_hash, last_seen_at, created_at, kind, config, secret, info, host_id`

func scanRunner(row pgx.Row) (*model.Runner, error) {
	var r model.Runner
	if err := row.Scan(&r.ID, &r.TenantID, &r.Name, &r.Labels, &r.TokenHash, &r.LastSeenAt, &r.CreatedAt, &r.Kind, &r.Config,
		&r.Secret, &r.Info, &r.HostID); err != nil {
		return nil, mapErr(err)
	}
	return &r, nil
}

func (s pgq) CreateRunner(ctx context.Context, r *model.Runner) error {
	if r.Kind == "" {
		r.Kind = "self"
	}
	_, err := s.q.Exec(ctx, `INSERT INTO runners (`+runnerCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		r.ID, r.TenantID, r.Name, nonNil(r.Labels), r.TokenHash, r.LastSeenAt, r.CreatedAt, r.Kind, nonNilMap(r.Config), r.Secret, r.Info, r.HostID)
	return mapErr(err)
}

func (s pgq) GetRunner(ctx context.Context, id string) (*model.Runner, error) {
	return scanRunner(s.q.QueryRow(ctx, `SELECT `+runnerCols+` FROM runners WHERE id = $1`, id))
}

func (s pgq) GetRunnerByTokenHash(ctx context.Context, hash string) (*model.Runner, error) {
	return scanRunner(s.q.QueryRow(ctx, `SELECT `+runnerCols+` FROM runners WHERE token_hash = $1`, hash))
}

func (s pgq) ListRunners(ctx context.Context, tenantID string) ([]*model.Runner, error) {
	rows, err := s.q.Query(ctx, `SELECT `+runnerCols+` FROM runners WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
	return collect(rows, err, scanRunner)
}

func (s pgq) DeleteRunner(ctx context.Context, id string) error {
	return expectRow(s.q.Exec(ctx, `DELETE FROM runners WHERE id = $1`, id))
}

func (s pgq) TouchRunner(ctx context.Context, id string, labels []string, info *model.NodeInfo, at time.Time) error {
	if info == nil {
		return expectRow(s.q.Exec(ctx, `UPDATE runners SET labels = $2, last_seen_at = $3 WHERE id = $1`, id, nonNil(labels), at))
	}
	return expectRow(s.q.Exec(ctx, `UPDATE runners SET labels = $2, last_seen_at = $3, info = $4 WHERE id = $1`, id, nonNil(labels), at, *info))
}

// UpdateRunner 修改节点名称与配置（GitHub 节点的仓库、令牌等）。
func (s pgq) UpdateRunner(ctx context.Context, r *model.Runner) error {
	return expectRow(s.q.Exec(ctx, `UPDATE runners SET name = $2, config = $3, secret = $4, token_hash = $5 WHERE id = $1`,
		r.ID, r.Name, nonNilMap(r.Config), r.Secret, r.TokenHash))
}

const artifactCols = `id, tenant_id, run_id, job_id, job_name, size, blob_key, created_at, expires_at, name, version, format`

func scanArtifact(row pgx.Row) (*model.Artifact, error) {
	var a model.Artifact
	if err := row.Scan(&a.ID, &a.TenantID, &a.RunID, &a.JobID, &a.JobName, &a.Size, &a.BlobKey, &a.CreatedAt, &a.ExpiresAt,
		&a.Name, &a.Version, &a.Format); err != nil {
		return nil, mapErr(err)
	}
	return &a, nil
}

func (s pgq) PutArtifact(ctx context.Context, a *model.Artifact) (string, error) {
	var old string
	err := s.q.QueryRow(ctx, `SELECT blob_key FROM artifacts WHERE job_id = $1 AND name = $2 FOR UPDATE`, a.JobID, a.Name).Scan(&old)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if a.Format == "" {
		a.Format = "tar.gz"
	}
	_, err = s.q.Exec(ctx, `INSERT INTO artifacts (`+artifactCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (job_id, name) DO UPDATE SET id = EXCLUDED.id, size = EXCLUDED.size, blob_key = EXCLUDED.blob_key,
		created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at, version = EXCLUDED.version, format = EXCLUDED.format`,
		a.ID, a.TenantID, a.RunID, a.JobID, a.JobName, a.Size, a.BlobKey, a.CreatedAt, a.ExpiresAt, a.Name, a.Version, a.Format)
	if err != nil {
		return "", mapErr(err)
	}
	return old, nil
}

func (s pgq) GetArtifact(ctx context.Context, id string) (*model.Artifact, error) {
	return scanArtifact(s.q.QueryRow(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE id = $1`, id))
}

func (s pgq) ListArtifacts(ctx context.Context, runID string) ([]*model.Artifact, error) {
	rows, err := s.q.Query(ctx, `SELECT a.id, a.tenant_id, a.run_id, a.job_id, a.job_name, a.size, a.blob_key, a.created_at, a.expires_at, a.name, a.version, a.format
		FROM artifacts a JOIN jobs j ON j.id = a.job_id WHERE a.run_id = $1 ORDER BY j.seq, a.name`, runID)
	return collect(rows, err, scanArtifact)
}

func (s pgq) ExpiredArtifacts(ctx context.Context, now time.Time, limit int) ([]*model.Artifact, error) {
	rows, err := s.q.Query(ctx, `SELECT a.id, a.tenant_id, a.run_id, a.job_id, a.job_name, a.size, a.blob_key, a.created_at, a.expires_at, a.name, a.version, a.format
		FROM artifacts a JOIN runs r ON r.id = a.run_id
		WHERE a.expires_at <= $1 AND r.id IS DISTINCT FROM (
			SELECT l.id FROM runs l WHERE l.pipeline_id = r.pipeline_id AND l.status = 'success' ORDER BY l.number DESC LIMIT 1)
		ORDER BY a.expires_at LIMIT $2`, now, limit)
	return collect(rows, err, scanArtifact)
}

func (s pgq) DeleteArtifacts(ctx context.Context, ids []string) error {
	_, err := s.q.Exec(ctx, `DELETE FROM artifacts WHERE id = ANY($1)`, ids)
	return mapErr(err)
}

func (s pgq) PruneRuns(ctx context.Context, pipelineID string, keep, limit int) (int, []string, error) {
	rows, err := s.q.Query(ctx, `SELECT id FROM (
			SELECT id, status FROM runs WHERE pipeline_id = $1 ORDER BY number DESC OFFSET $2
		) old WHERE status IN ('success', 'failed', 'canceled') LIMIT $3`, pipelineID, keep, limit)
	if err != nil {
		return 0, nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(ids) == 0 {
		return 0, nil, err
	}
	rows, err = s.q.Query(ctx, `SELECT blob_key FROM artifacts WHERE run_id = ANY($1)`, ids)
	if err != nil {
		return 0, nil, err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, nil, err
	}
	// jobs、日志分块、产物均为 ON DELETE CASCADE。
	tag, err := s.q.Exec(ctx, `DELETE FROM runs WHERE id = ANY($1)`, ids)
	if err != nil {
		return 0, nil, mapErr(err)
	}
	return int(tag.RowsAffected()), keys, nil
}

func (s pgq) CreateTenant(ctx context.Context, t *model.Tenant) error {
	_, err := s.q.Exec(ctx, `INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)`, t.ID, t.Name, t.CreatedAt)
	return mapErr(err)
}

func scanTenant(row pgx.Row) (*model.Tenant, error) {
	var t model.Tenant
	if err := row.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

func (s pgq) GetTenant(ctx context.Context, id string) (*model.Tenant, error) {
	return scanTenant(s.q.QueryRow(ctx, `SELECT id, name, created_at FROM tenants WHERE id = $1`, id))
}

func (s pgq) ListTenants(ctx context.Context) ([]*model.Tenant, error) {
	rows, err := s.q.Query(ctx, `SELECT id, name, created_at FROM tenants ORDER BY created_at, id`)
	return collect(rows, err, scanTenant)
}

const userCols = `id, username, display_name, password_hash, is_admin, created_at, email, gender, bio`

func scanUser(row pgx.Row) (*model.User, error) {
	var u model.User
	if err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.IsAdmin, &u.CreatedAt, &u.Email, &u.Gender, &u.Bio); err != nil {
		return nil, mapErr(err)
	}
	return &u, nil
}

func (s pgq) CreateUser(ctx context.Context, u *model.User) error {
	_, err := s.q.Exec(ctx, `INSERT INTO users (`+userCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		u.ID, u.Username, u.DisplayName, u.PasswordHash, u.IsAdmin, u.CreatedAt, u.Email, u.Gender, u.Bio)
	return mapErr(err)
}

func (s pgq) UpdateUser(ctx context.Context, u *model.User) error {
	return expectRow(s.q.Exec(ctx, `UPDATE users SET display_name = $2, password_hash = $3, is_admin = $4, email = $5, gender = $6, bio = $7 WHERE id = $1`,
		u.ID, u.DisplayName, u.PasswordHash, u.IsAdmin, u.Email, u.Gender, u.Bio))
}

func (s pgq) DeleteUser(ctx context.Context, id string) error {
	return expectRow(s.q.Exec(ctx, `DELETE FROM users WHERE id = $1`, id))
}

func (s pgq) GetUser(ctx context.Context, id string) (*model.User, error) {
	return scanUser(s.q.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

func (s pgq) GetUserByName(ctx context.Context, username string) (*model.User, error) {
	return scanUser(s.q.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE username = $1`, username))
}

func (s pgq) ListUsers(ctx context.Context) ([]*model.User, error) {
	rows, err := s.q.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at, id`)
	return collect(rows, err, scanUser)
}

func (s pgq) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func scanMembership(row pgx.Row) (*model.Membership, error) {
	var m model.Membership
	if err := row.Scan(&m.TenantID, &m.UserID, &m.Role); err != nil {
		return nil, mapErr(err)
	}
	return &m, nil
}

func (s pgq) PutMembership(ctx context.Context, m *model.Membership) error {
	_, err := s.q.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET role = EXCLUDED.role`, m.TenantID, m.UserID, m.Role)
	return mapErr(err)
}

func (s pgq) DeleteMembership(ctx context.Context, tenantID, userID string) error {
	return expectRow(s.q.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID))
}

func (s pgq) ListMembers(ctx context.Context, tenantID string) ([]*model.Membership, error) {
	rows, err := s.q.Query(ctx, `SELECT tenant_id, user_id, role FROM memberships WHERE tenant_id = $1 ORDER BY user_id`, tenantID)
	return collect(rows, err, scanMembership)
}

func (s pgq) UserMemberships(ctx context.Context, userID string) ([]*model.Membership, error) {
	rows, err := s.q.Query(ctx, `SELECT tenant_id, user_id, role FROM memberships WHERE user_id = $1 ORDER BY tenant_id`, userID)
	return collect(rows, err, scanMembership)
}

const tokenCols = `id, hash, user_id, tenant_id, kind, name, prefix, expires_at, created_at, last_used_at, user_agent, ip`

func scanToken(row pgx.Row) (*model.Token, error) {
	var t model.Token
	if err := row.Scan(&t.ID, &t.Hash, &t.UserID, &t.TenantID, &t.Kind, &t.Name, &t.Prefix, &t.ExpiresAt,
		&t.CreatedAt, &t.LastUsedAt, &t.UserAgent, &t.IP); err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

func (s pgq) CreateToken(ctx context.Context, t *model.Token) error {
	_, err := s.q.Exec(ctx, `INSERT INTO tokens (`+tokenCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		t.ID, t.Hash, t.UserID, t.TenantID, t.Kind, t.Name, t.Prefix, t.ExpiresAt, t.CreatedAt, t.LastUsedAt, t.UserAgent, t.IP)
	return mapErr(err)
}

func (s pgq) GetTokenByHash(ctx context.Context, hash string) (*model.Token, error) {
	return scanToken(s.q.QueryRow(ctx, `SELECT `+tokenCols+` FROM tokens WHERE hash = $1`, hash))
}

func (s pgq) ListTokens(ctx context.Context, userID, kind string) ([]*model.Token, error) {
	rows, err := s.q.Query(ctx, `SELECT `+tokenCols+` FROM tokens WHERE user_id = $1 AND kind = $2 ORDER BY created_at, id`, userID, kind)
	return collect(rows, err, scanToken)
}

func (s pgq) DeleteToken(ctx context.Context, id string) error {
	return expectRow(s.q.Exec(ctx, `DELETE FROM tokens WHERE id = $1`, id))
}

func (s pgq) SetTokenTenant(ctx context.Context, id, tenantID string) error {
	return expectRow(s.q.Exec(ctx, `UPDATE tokens SET tenant_id = $2 WHERE id = $1`, id, tenantID))
}

func (s pgq) TouchToken(ctx context.Context, id string, at time.Time) error {
	return expectRow(s.q.Exec(ctx, `UPDATE tokens SET last_used_at = $2 WHERE id = $1`, id, at))
}

func (s pgq) DeleteExpiredTokens(ctx context.Context, now time.Time) error {
	_, err := s.q.Exec(ctx, `DELETE FROM tokens WHERE expires_at <= $1`, now)
	return err
}
