package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"flow/internal/model"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

func seedRun(t *testing.T, s store.Store, now time.Time) {
	t.Helper()
	ctx := context.Background()
	must(t, s.CreatePipeline(ctx, &model.Pipeline{ID: "p1", TenantID: "default", Name: "demo", YAML: "x", CreatedAt: now, UpdatedAt: now}))
	r := &model.Run{ID: "r1", TenantID: "default", PipelineID: "p1", Status: model.StatusPending, Trigger: "manual", CreatedBy: "alice",
		Notify: []model.NotifyTarget{{Type: "webhook", URL: "http://x", On: []model.Status{model.StatusFailed}}}, CreatedAt: now}
	jobs := []*model.Job{
		{ID: "j1", TenantID: "default", RunID: "r1", Name: "plain", Status: model.StatusQueued, Attempt: 1, CreatedAt: now},
		{ID: "j2", TenantID: "default", RunID: "r1", Seq: 1, Name: "gpu", RunsOn: []string{"linux", "gpu"}, Status: model.StatusQueued, Attempt: 1, CreatedAt: now,
			JobOptions: model.JobOptions{Kind: "", TimeoutSec: 60, Retries: 2, Artifacts: []string{"dist"},
				Cache: &model.Cache{Key: "k", Paths: []string{".c"}}, Checkout: &model.Checkout{URL: "https://g/x.git", Ref: "main"}}},
	}
	must(t, s.Atomic(ctx, func(tx store.Tx) error { return tx.CreateRun(ctx, r, jobs) }))
}

func TestClaimFilterAndOptions(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		seedRun(t, s, now)
		next := func(f store.ClaimFilter) string {
			var id string
			must(t, s.Atomic(ctx, func(tx store.Tx) error {
				j, err := tx.NextQueuedJob(ctx, f)
				if j != nil {
					id = j.ID
				}
				return err
			}))
			return id
		}
		// 无标签的 Runner 只能领无 runs-on 要求的 Job；标签齐全才能领 gpu。
		must(t, s.Atomic(ctx, func(tx store.Tx) error {
			j, _ := tx.GetJob(ctx, "j1")
			j.Status = model.StatusRunning
			return tx.UpdateJob(ctx, j)
		}))
		if id := next(store.ClaimFilter{}); id != "" {
			t.Errorf("unlabeled runner claimed %s", id)
		}
		if id := next(store.ClaimFilter{Labels: []string{"linux"}}); id != "" {
			t.Errorf("partial labels claimed %s", id)
		}
		if id := next(store.ClaimFilter{TenantID: "other", Labels: []string{"gpu", "linux", "x"}}); id != "" {
			t.Errorf("other tenant claimed %s", id)
		}
		if id := next(store.ClaimFilter{TenantID: "default", Labels: []string{"gpu", "linux", "x"}}); id != "j2" {
			t.Errorf("labeled runner claimed %q", id)
		}
		j, _ := s.GetJob(ctx, "j2")
		if j.TimeoutSec != 60 || j.Retries != 2 || j.Cache.Key != "k" || j.Checkout.Ref != "main" || j.Artifacts[0] != "dist" || j.Attempt != 1 {
			t.Errorf("options = %+v", j.JobOptions)
		}
		r, _ := s.GetRun(ctx, "r1")
		if r.CreatedBy != "alice" || len(r.Notify) != 1 || r.Notify[0].On[0] != model.StatusFailed {
			t.Errorf("run = %+v", r)
		}

		// 通知队列：仅结束且未发送的 Run。
		pending := func() *model.Run {
			var out *model.Run
			must(t, s.Atomic(ctx, func(tx store.Tx) error {
				var err error
				out, err = tx.NextPendingNotify(ctx)
				return err
			}))
			return out
		}
		r.NotifyDone = false
		must(t, s.Atomic(ctx, func(tx store.Tx) error { return tx.UpdateRun(ctx, r) }))
		if p := pending(); p != nil {
			t.Error("running run is pending notify")
		}
		r.Status = model.StatusFailed
		must(t, s.Atomic(ctx, func(tx store.Tx) error { return tx.UpdateRun(ctx, r) }))
		if p := pending(); p == nil || p.ID != "r1" {
			t.Errorf("pending = %v", p)
		}
		r.NotifyDone = true
		must(t, s.Atomic(ctx, func(tx store.Tx) error { return tx.UpdateRun(ctx, r) }))
		if p := pending(); p != nil {
			t.Error("still pending after done")
		}

		// 产物：每个 Job 一份，覆盖时返回旧 key；删除流水线返回待清理的 key。
		a := &model.Artifact{ID: "a1", TenantID: "default", RunID: "r1", JobID: "j2", JobName: "gpu", Size: 3, BlobKey: "k1", CreatedAt: now}
		if old, err := s.PutArtifact(ctx, a); err != nil || old != "" {
			t.Fatalf("PutArtifact = %q, %v", old, err)
		}
		a2 := *a
		a2.ID, a2.BlobKey = "a2", "k2"
		if old, err := s.PutArtifact(ctx, &a2); err != nil || old != "k1" {
			t.Fatalf("overwrite = %q, %v", old, err)
		}
		if list, _ := s.ListArtifacts(ctx, "r1"); len(list) != 1 || list[0].ID != "a2" {
			t.Fatalf("artifacts = %+v", list)
		}
		if got, err := s.GetArtifact(ctx, "a2"); err != nil || got.BlobKey != "k2" {
			t.Fatalf("GetArtifact = %+v, %v", got, err)
		}
		must(t, s.PutSecret(ctx, &model.Secret{ID: "s1", TenantID: "default", PipelineID: "p1", Name: "X", Value: []byte{1}, CreatedAt: now, UpdatedAt: now}))
		if runs, _ := s.ListRuns(ctx, "p1", 1); len(runs) != 1 {
			t.Fatalf("ListRuns limit = %d", len(runs))
		}
		keys, err := s.DeletePipeline(ctx, "p1")
		if err != nil || len(keys) != 1 || keys[0] != "k2" {
			t.Fatalf("DeletePipeline = %v, %v", keys, err)
		}
		if _, err := s.GetRun(ctx, "r1"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("run survived delete: %v", err)
		}
		if _, err := s.GetJob(ctx, "j2"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("job survived delete: %v", err)
		}
		if list, _ := s.ListSecrets(ctx, "default", "p1"); len(list) != 0 {
			t.Errorf("secrets survived delete: %v", list)
		}
	})
}

func TestSecretsAndRunners(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		must(t, s.CreatePipeline(ctx, &model.Pipeline{ID: "p1", TenantID: "default", Name: "demo", YAML: "x", CreatedAt: now, UpdatedAt: now}))
		put := func(id, pipeline, name string, v byte) {
			must(t, s.PutSecret(ctx, &model.Secret{ID: id, TenantID: "default", PipelineID: pipeline, Name: name, Value: []byte{v}, CreatedAt: now, UpdatedAt: now}))
		}
		put("s1", "", "TOKEN", 1)
		put("s2", "", "A", 2)
		put("s3", "p1", "TOKEN", 3)
		put("s4", "", "TOKEN", 4) // 覆盖租户级 TOKEN，保留原 ID
		tenant, _ := s.ListSecrets(ctx, "default", "")
		if len(tenant) != 2 || tenant[0].Name != "A" || tenant[1].ID != "s1" || tenant[1].Value[0] != 4 {
			t.Fatalf("tenant secrets = %+v", tenant)
		}
		if pl, _ := s.ListSecrets(ctx, "default", "p1"); len(pl) != 1 || pl[0].Value[0] != 3 || pl[0].PipelineID != "p1" {
			t.Fatalf("pipeline secrets = %+v", pl)
		}
		must(t, s.DeleteSecret(ctx, "default", "", "TOKEN"))
		if err := s.DeleteSecret(ctx, "default", "", "TOKEN"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("double delete = %v", err)
		}
		if pl, _ := s.ListSecrets(ctx, "default", "p1"); len(pl) != 1 {
			t.Error("pipeline secret removed with tenant one")
		}

		r := &model.Runner{ID: "rn1", TenantID: "default", Name: "build-01", TokenHash: "h1", CreatedAt: now}
		must(t, s.CreateRunner(ctx, r))
		dup := *r
		dup.ID, dup.TokenHash = "rn2", "h2"
		if err := s.CreateRunner(ctx, &dup); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate runner name = %v", err)
		}
		must(t, s.TouchRunner(ctx, "rn1", []string{"linux"}, nil, now))
		got, err := s.GetRunnerByTokenHash(ctx, "h1")
		if err != nil || got.ID != "rn1" || got.Labels[0] != "linux" || !got.LastSeenAt.Equal(now) {
			t.Fatalf("runner = %+v, %v", got, err)
		}
		if list, _ := s.ListRunners(ctx, "default"); len(list) != 1 {
			t.Errorf("runners = %v", list)
		}
		must(t, s.DeleteRunner(ctx, "rn1"))
		if _, err := s.GetRunnerByTokenHash(ctx, "h1"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleted runner = %v", err)
		}
	})
}

func TestUsersAndTokens(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		must(t, s.CreateTenant(ctx, &model.Tenant{ID: "t2", Name: "Team 2", CreatedAt: now}))
		if err := s.CreateTenant(ctx, &model.Tenant{ID: "t2", Name: "dup", CreatedAt: now}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("dup tenant = %v", err)
		}
		if ts, _ := s.ListTenants(ctx); len(ts) != 2 {
			t.Errorf("tenants = %v", ts)
		}
		u := &model.User{ID: "u1", Username: "alice", PasswordHash: "h", CreatedAt: now}
		must(t, s.CreateUser(ctx, u))
		if err := s.CreateUser(ctx, &model.User{ID: "u2", Username: "alice", PasswordHash: "h", CreatedAt: now}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("dup user = %v", err)
		}
		u.DisplayName, u.IsAdmin = "Alice", true
		must(t, s.UpdateUser(ctx, u))
		if got, err := s.GetUserByName(ctx, "alice"); err != nil || got.DisplayName != "Alice" || !got.IsAdmin {
			t.Fatalf("user = %+v, %v", got, err)
		}
		if n, _ := s.CountUsers(ctx); n != 1 {
			t.Errorf("count = %d", n)
		}
		must(t, s.PutMembership(ctx, &model.Membership{TenantID: "default", UserID: "u1", Role: model.RoleViewer}))
		must(t, s.PutMembership(ctx, &model.Membership{TenantID: "default", UserID: "u1", Role: model.RoleAdmin}))
		must(t, s.PutMembership(ctx, &model.Membership{TenantID: "t2", UserID: "u1", Role: model.RoleDeveloper}))
		if ms, _ := s.UserMemberships(ctx, "u1"); len(ms) != 2 || ms[0].Role != model.RoleAdmin {
			t.Errorf("memberships = %+v", ms)
		}
		if ms, _ := s.ListMembers(ctx, "t2"); len(ms) != 1 || ms[0].Role != model.RoleDeveloper {
			t.Errorf("members = %+v", ms)
		}
		must(t, s.DeleteMembership(ctx, "t2", "u1"))

		exp := now.Add(time.Hour)
		must(t, s.CreateToken(ctx, &model.Token{ID: "tk1", Hash: "hash1", UserID: "u1", TenantID: "default", Kind: "session", ExpiresAt: &exp, CreatedAt: now}))
		must(t, s.CreateToken(ctx, &model.Token{ID: "tk2", Hash: "hash2", UserID: "u1", TenantID: "default", Kind: "api", Name: "ci", CreatedAt: now}))
		must(t, s.SetTokenTenant(ctx, "tk1", "t2"))
		must(t, s.TouchToken(ctx, "tk1", now))
		tk, err := s.GetTokenByHash(ctx, "hash1")
		if err != nil || tk.TenantID != "t2" || tk.LastUsedAt == nil {
			t.Fatalf("token = %+v, %v", tk, err)
		}
		if list, _ := s.ListTokens(ctx, "u1", "api"); len(list) != 1 || list[0].Name != "ci" {
			t.Errorf("api tokens = %+v", list)
		}
		must(t, s.DeleteExpiredTokens(ctx, now.Add(2*time.Hour)))
		if _, err := s.GetTokenByHash(ctx, "hash1"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expired token = %v", err)
		}
		must(t, s.DeleteUser(ctx, "u1"))
		if _, err := s.GetTokenByHash(ctx, "hash2"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("token survived user delete: %v", err)
		}
		if ms, _ := s.ListMembers(ctx, "default"); len(ms) != 0 {
			t.Errorf("membership survived user delete")
		}
	})
}
