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

func TestTemplates(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Local()
		tmpl := func(id string, kind model.TemplateKind, name string) *model.Template {
			return &model.Template{ID: id, TenantID: "default", Kind: kind, Name: name, Description: "d", YAML: "y",
				UpdatedBy: "alice", CreatedAt: now, UpdatedAt: now}
		}
		must(t, s.CreateTemplate(ctx, tmpl("t1", model.TemplateSteps, "go-build")))
		must(t, s.CreateTemplate(ctx, tmpl("t2", model.TemplatePipeline, "go-build"))) // 不同类型可重名
		must(t, s.CreateTemplate(ctx, tmpl("t3", model.TemplateSteps, "a-lint")))
		if err := s.CreateTemplate(ctx, tmpl("t4", model.TemplateSteps, "go-build")); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate name err = %v", err)
		}
		if err := s.CreateTemplate(ctx, &model.Template{ID: "t5", TenantID: "nope", Kind: model.TemplateSteps, Name: "x",
			CreatedAt: now, UpdatedAt: now}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown tenant err = %v", err)
		}

		steps, err := s.ListTemplates(ctx, "default", model.TemplateSteps)
		must(t, err)
		if len(steps) != 2 || steps[0].Name != "a-lint" || steps[1].Name != "go-build" {
			t.Errorf("steps = %+v", steps)
		}
		all, _ := s.ListTemplates(ctx, "default", "")
		if len(all) != 3 {
			t.Errorf("all = %d", len(all))
		}

		got, err := s.GetTemplateByName(ctx, "default", model.TemplateSteps, "go-build")
		if err != nil || got.ID != "t1" || got.UpdatedBy != "alice" || !got.CreatedAt.Equal(now) {
			t.Fatalf("by name = %+v, %v", got, err)
		}
		var viaTx *model.Template
		must(t, s.Atomic(ctx, func(tx store.Tx) error {
			var err error
			viaTx, err = tx.GetTemplateByName(ctx, "default", model.TemplatePipeline, "go-build")
			return err
		}))
		if viaTx.ID != "t2" {
			t.Errorf("tx by name = %+v", viaTx)
		}

		got.Name, got.YAML, got.UpdatedAt = "a-lint", "z", now.Add(time.Hour)
		if err := s.UpdateTemplate(ctx, got); !errors.Is(err, store.ErrConflict) {
			t.Errorf("rename to taken name err = %v", err)
		}
		got.Name = "go-build2"
		must(t, s.UpdateTemplate(ctx, got))
		if g, _ := s.GetTemplate(ctx, "t1"); g.Name != "go-build2" || g.YAML != "z" || !g.UpdatedAt.Equal(now.Add(time.Hour)) {
			t.Errorf("updated = %+v", g)
		}
		must(t, s.DeleteTemplate(ctx, "t1"))
		if _, err := s.GetTemplate(ctx, "t1"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleted get err = %v", err)
		}
		if err := s.DeleteTemplate(ctx, "t1"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("double delete err = %v", err)
		}
	})
}

func TestTemplateRevisions(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Local()
		tm := &model.Template{ID: "t1", TenantID: "default", Kind: model.TemplateSteps, Name: "go-build", Description: "d1", YAML: "v1",
			UpdatedBy: "alice", CreatedAt: now, UpdatedAt: now}
		must(t, s.CreateTemplate(ctx, tm))
		if tm.Version != 1 {
			t.Fatalf("created version = %d", tm.Version)
		}
		tm.Description, tm.YAML, tm.UpdatedBy, tm.UpdatedAt = "d2", "v2", "bob", now.Add(time.Hour)
		must(t, s.UpdateTemplate(ctx, tm))
		if tm.Version != 2 {
			t.Fatalf("updated version = %d", tm.Version)
		}
		// 基于旧版本保存视为并发修改。
		stale := *tm
		stale.Version = 1
		if err := s.UpdateTemplate(ctx, &stale); !errors.Is(err, store.ErrConflict) {
			t.Errorf("stale update err = %v", err)
		}
		tm.YAML, tm.RestoredFrom, tm.UpdatedAt = "v1", 1, now.Add(2*time.Hour)
		must(t, s.UpdateTemplate(ctx, tm))
		if g, _ := s.GetTemplate(ctx, "t1"); g.Version != 3 || g.YAML != "v1" {
			t.Errorf("current = %+v", g)
		}

		revs, err := s.ListTemplateRevisions(ctx, "t1", 2)
		must(t, err)
		if len(revs) != 2 || revs[0].Version != 3 || revs[0].RestoredFrom != 1 || revs[0].YAML != "" || revs[1].Version != 2 || revs[1].CreatedBy != "bob" {
			t.Errorf("revisions = %+v", revs)
		}
		r, err := s.GetTemplateRevision(ctx, "t1", 2)
		if err != nil || r.YAML != "v2" || r.Description != "d2" || !r.CreatedAt.Equal(now.Add(time.Hour)) {
			t.Errorf("revision 2 = %+v, %v", r, err)
		}
		var byName *model.TemplateRevision
		must(t, s.Atomic(ctx, func(tx store.Tx) error {
			var err error
			byName, err = tx.GetTemplateRevisionByName(ctx, "default", model.TemplateSteps, "go-build", 1)
			return err
		}))
		if byName.YAML != "v1" || byName.CreatedBy != "alice" {
			t.Errorf("by name = %+v", byName)
		}
		if _, err := s.GetTemplateRevisionByName(ctx, "default", model.TemplatePipeline, "go-build", 1); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("wrong kind err = %v", err)
		}
		if _, err := s.GetTemplateRevision(ctx, "t1", 9); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("missing revision err = %v", err)
		}
		must(t, s.DeleteTemplate(ctx, "t1"))
		if revs, _ := s.ListTemplateRevisions(ctx, "t1", 10); len(revs) != 0 {
			t.Errorf("revisions after delete = %+v", revs)
		}
	})
}
