package store_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"flow/internal/model"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

func TestRetention(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Local()
		must(t, s.CreatePipeline(ctx, &model.Pipeline{ID: "p", TenantID: "default", Name: "p", YAML: "x", CreatedAt: now, UpdatedAt: now}))
		// 5 个运行：#1 成功、#2 失败、#3 成功、#4 失败、#5 运行中；每个运行一个带产物的 Job，均已过期。
		statuses := []model.Status{model.StatusSuccess, model.StatusFailed, model.StatusSuccess, model.StatusFailed, model.StatusRunning}
		expired := now.Add(-time.Hour)
		for i, st := range statuses {
			rid, jid := fmt.Sprintf("r%d", i+1), fmt.Sprintf("j%d", i+1)
			must(t, s.Atomic(ctx, func(tx store.Tx) error {
				return tx.CreateRun(ctx, &model.Run{ID: rid, TenantID: "default", PipelineID: "p", Status: st, Trigger: "manual", CreatedAt: now},
					[]*model.Job{{ID: jid, TenantID: "default", RunID: rid, Name: "j", Status: model.StatusSuccess, Attempt: 1, CreatedAt: now}})
			}))
			exp := &expired
			if i == 1 {
				exp = nil // 永久保留
			}
			_, err := s.PutArtifact(ctx, &model.Artifact{ID: "a" + rid, TenantID: "default", RunID: rid, JobID: jid, JobName: "j",
				BlobKey: "k" + rid, CreatedAt: now, ExpiresAt: exp})
			must(t, err)
		}
		if a, _ := s.GetArtifact(ctx, "ar1"); a.ExpiresAt == nil || !a.ExpiresAt.Equal(expired) {
			t.Errorf("expires_at not stored: %+v", a)
		}

		arts, err := s.ExpiredArtifacts(ctx, now, 100)
		must(t, err)
		var ids []string
		for _, a := range arts {
			ids = append(ids, a.ID)
		}
		slices.Sort(ids)
		// r2 永久保留；r3 是最近一次成功运行，保留。
		if fmt.Sprint(ids) != "[ar1 ar4 ar5]" {
			t.Errorf("expired = %v", ids)
		}
		if arts, _ := s.ExpiredArtifacts(ctx, expired.Add(-time.Minute), 100); len(arts) != 0 {
			t.Errorf("not yet expired returned %d", len(arts))
		}
		if arts, _ := s.ExpiredArtifacts(ctx, now, 2); len(arts) != 2 {
			t.Errorf("limit ignored: %d", len(arts))
		}
		must(t, s.DeleteArtifacts(ctx, ids))
		if left, _ := s.ListArtifacts(ctx, "r1"); len(left) != 0 {
			t.Errorf("r1 artifacts left: %d", len(left))
		}

		// 保留最新 2 个运行：#5（运行中）、#4 保留；#3、#2、#1 删除（运行中的不会被删）。
		n, keys, err := s.PruneRuns(ctx, "p", 2, 100)
		must(t, err)
		slices.Sort(keys)
		if n != 3 || fmt.Sprint(keys) != "[kr2 kr3]" {
			t.Errorf("pruned %d, keys %v", n, keys)
		}
		runs, _ := s.ListRuns(ctx, "p", 10)
		if len(runs) != 2 || runs[0].ID != "r5" || runs[1].ID != "r4" {
			t.Errorf("runs left = %v", runs)
		}
		if _, err := s.GetJob(ctx, "j1"); err == nil {
			t.Error("job of pruned run still exists")
		}
		if n, _, _ := s.PruneRuns(ctx, "p", 0, 100); n != 1 {
			t.Errorf("keep 0 pruned %d, want 1 (running run kept)", n)
		}
	})
}
