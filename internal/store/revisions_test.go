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

func TestPipelineRevisions(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		p := &model.Pipeline{ID: "p1", TenantID: "default", Name: "demo", YAML: "v1", UpdatedBy: "alice", CreatedAt: now, UpdatedAt: now}
		must(t, s.CreatePipeline(ctx, p))
		if p.Version != 1 {
			t.Fatalf("created version = %d", p.Version)
		}

		// 基于已读到的版本保存：版本加 1 并记录修订。
		stale, err := s.GetPipeline(ctx, "p1")
		must(t, err)
		next := *stale
		next.YAML, next.UpdatedBy, next.UpdatedAt = "v2", "bob", now.Add(time.Minute)
		must(t, s.UpdatePipeline(ctx, &next))
		if next.Version != 2 {
			t.Fatalf("updated version = %d", next.Version)
		}
		// 基于旧版本再次保存视为并发修改。
		stale.YAML = "lost"
		if err := s.UpdatePipeline(ctx, stale); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("stale update err = %v", err)
		}
		got, err := s.GetPipeline(ctx, "p1")
		must(t, err)
		if got.Version != 2 || got.YAML != "v2" || got.UpdatedBy != "bob" {
			t.Fatalf("pipeline = %+v", got)
		}
		missing := next
		missing.ID = "nope"
		if err := s.UpdatePipeline(ctx, &missing); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing update err = %v", err)
		}

		revs, err := s.ListRevisions(ctx, "p1", 10)
		must(t, err)
		if len(revs) != 2 || revs[0].Version != 2 || revs[0].CreatedBy != "bob" || revs[0].YAML != "" || revs[1].CreatedBy != "alice" {
			t.Fatalf("revisions = %+v", revs)
		}
		if revs, _ := s.ListRevisions(ctx, "p1", 1); len(revs) != 1 {
			t.Fatalf("limited revisions = %d", len(revs))
		}
		r1, err := s.GetRevision(ctx, "p1", 1)
		must(t, err)
		if r1.YAML != "v1" || !r1.CreatedAt.Equal(now) {
			t.Fatalf("revision 1 = %+v", r1)
		}
		if _, err := s.GetRevision(ctx, "p1", 3); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing revision err = %v", err)
		}

		// 运行记录所用的配置版本。
		run := &model.Run{ID: "r1", TenantID: "default", PipelineID: "p1", Status: model.StatusPending, Trigger: "api", PipelineVersion: 2, CreatedAt: now}
		must(t, s.Atomic(ctx, func(tx store.Tx) error { return tx.CreateRun(ctx, run, nil) }))
		if r, _ := s.GetRun(ctx, "r1"); r.PipelineVersion != 2 {
			t.Fatalf("run version = %d", r.PipelineVersion)
		}

		_, err = s.DeletePipeline(ctx, "p1")
		must(t, err)
		if revs, _ := s.ListRevisions(ctx, "p1", 10); len(revs) != 0 {
			t.Fatalf("revisions after delete = %d", len(revs))
		}
	})
}
