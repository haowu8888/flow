package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"flow/internal/model"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

// 列表分组 / 收藏 / 最近运行，以及回退、回滚相关字段的读写。
func TestPipelineListMeta(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for _, id := range []string{"p1", "p2"} {
			must(t, s.CreatePipeline(ctx, &model.Pipeline{ID: id, TenantID: "default", Name: id, YAML: "v1", CreatedAt: now, UpdatedAt: now}))
		}
		must(t, s.CreateUser(ctx, &model.User{ID: "u1", Username: "alice", PasswordHash: "x", CreatedAt: now}))

		// 分组不随配置保存被覆盖；回退来源写入修订记录。
		must(t, s.SetPipelineGroup(ctx, "p1", "后端"))
		p, err := s.GetPipeline(ctx, "p1")
		must(t, err)
		p.YAML, p.RestoredFrom = "v1 again", 1
		must(t, s.UpdatePipeline(ctx, p))
		if p.Group != "后端" {
			t.Errorf("group after update = %q", p.Group)
		}
		got, err := s.GetPipeline(ctx, "p1")
		must(t, err)
		if got.Group != "后端" || got.Version != 2 {
			t.Errorf("pipeline = %+v", got)
		}
		revs, err := s.ListRevisions(ctx, "p1", 10)
		must(t, err)
		if revs[0].RestoredFrom != 1 || revs[1].RestoredFrom != 0 {
			t.Errorf("restored_from = %d, %d", revs[0].RestoredFrom, revs[1].RestoredFrom)
		}
		if err := s.SetPipelineGroup(ctx, "nope", "x"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("group missing pipeline err = %v", err)
		}

		// 收藏可重复设置。
		must(t, s.SetFavorite(ctx, "u1", "p2", true, now))
		must(t, s.SetFavorite(ctx, "u1", "p2", true, now))
		must(t, s.SetFavorite(ctx, "u1", "p1", true, now))
		must(t, s.SetFavorite(ctx, "u1", "p1", false, now))
		favs, err := s.ListFavorites(ctx, "u1")
		must(t, err)
		if !slices.Equal(favs, []string{"p2"}) {
			t.Errorf("favorites = %v", favs)
		}
		if favs, _ := s.ListFavorites(ctx, "nobody"); favs == nil || len(favs) != 0 {
			t.Errorf("empty favorites = %#v", favs)
		}

		// 每条流水线最近 N 次运行，新的在前；提交与回滚来源可读写。
		for i := range 4 {
			r := &model.Run{ID: "r" + string(rune('a'+i)), TenantID: "default", PipelineID: "p1", Status: model.StatusSuccess,
				Trigger: "manual", CreatedAt: now.Add(time.Duration(i) * time.Minute), NotifyDone: true}
			if i == 3 {
				r.RollbackOf = "ra"
			}
			must(t, s.Atomic(ctx, func(tx store.Tx) error { return tx.CreateRun(ctx, r, nil) }))
		}
		recent, err := s.RecentRuns(ctx, "default", 3)
		must(t, err)
		if len(recent["p1"]) != 3 || recent["p1"][0].Number != 4 || recent["p1"][2].Number != 2 || len(recent["p2"]) != 0 {
			t.Fatalf("recent = %v", recent)
		}
		if recent["p1"][0].RollbackOf != "ra" {
			t.Errorf("rollback_of = %q", recent["p1"][0].RollbackOf)
		}
		r, err := s.GetRun(ctx, "ra")
		must(t, err)
		r.Commit = "0123456789012345678901234567890123456789"
		must(t, s.Atomic(ctx, func(tx store.Tx) error { return tx.UpdateRun(ctx, r) }))
		if r, _ := s.GetRun(ctx, "ra"); r.Commit != "0123456789012345678901234567890123456789" {
			t.Errorf("commit = %q", r.Commit)
		}

		// 删除流水线 / 用户同时清理收藏。
		_, err = s.DeletePipeline(ctx, "p2")
		must(t, err)
		if favs, _ := s.ListFavorites(ctx, "u1"); len(favs) != 0 {
			t.Errorf("favorites after delete = %v", favs)
		}
	})
}
