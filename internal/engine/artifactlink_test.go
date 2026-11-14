package engine

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"flow/internal/model"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

// 免登录下载链接带过期时间：到期前可用，过期或改动 exp 后失效。
func TestArtifactLinkExpires(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, run, clock := setup(t, s, dagYAML)
		jobs, err := e.Jobs(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		a := &model.Artifact{ID: "00000000-0000-4000-8000-0000000000a1", TenantID: DefaultTenant, RunID: run.ID, JobID: jobs[0].ID, JobName: jobs[0].Name, BlobKey: "k", CreatedAt: *clock}
		if _, err := s.PutArtifact(ctx, a); err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(e.ArtifactLinkPath(a))
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if got, err := e.ArtifactByLink(ctx, a.ID, q.Get("exp"), q.Get("sig")); err != nil || got.ID != a.ID {
			t.Fatalf("fresh link = %v, %v", got, err)
		}
		if _, err := e.ArtifactByLink(ctx, a.ID, q.Get("exp")+"0", q.Get("sig")); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("tampered exp = %v", err)
		}
		if _, err := e.ArtifactByLink(ctx, a.ID, "", q.Get("sig")); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("missing exp = %v", err)
		}
		*clock = clock.Add(artifactLinkTTL + 1e9)
		if _, err := e.ArtifactByLink(ctx, a.ID, q.Get("exp"), q.Get("sig")); !errors.Is(err, ErrLinkExpired) || !strings.Contains(err.Error(), "expired") {
			t.Errorf("expired link = %v", err)
		}
	})
}

// 领取后没能下发的 Job 放回队列，不用等租约过期；已被别的 Runner 持有时不动。
func TestReleaseClaim(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, run, _ := setup(t, s, dagYAML)
		a := claim(t, e, "a")
		e.releaseClaim(ctx, a.ID, "r2")
		if got := statuses(t, e, run.ID); got["a"] != model.StatusRunning {
			t.Fatalf("released by other runner: %v", got)
		}
		e.releaseClaim(ctx, a.ID, "r1")
		j, err := e.Job(ctx, a.ID)
		if err != nil || j.Status != model.StatusQueued || j.RunnerID != "" || j.StartedAt != nil || j.Attempt != a.Attempt {
			t.Fatalf("released job = %+v, %v", j, err)
		}
		claim(t, e, "a")
	})
}
