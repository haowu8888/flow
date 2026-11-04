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

func TestStoreContract(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		p := &model.Pipeline{ID: "p1", TenantID: "default", Name: "demo", YAML: "x", CreatedAt: now, UpdatedAt: now}
		must(t, s.CreatePipeline(ctx, p))
		dup := *p
		dup.ID = "p2"
		if err := s.CreatePipeline(ctx, &dup); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate name err = %v", err)
		}

		var numbers []int
		for i, id := range []string{"r1", "r2"} {
			r := &model.Run{ID: id, TenantID: "default", PipelineID: "p1", Status: model.StatusPending, Trigger: "api", CreatedAt: now.Add(time.Duration(i) * time.Second)}
			jobs := []*model.Job{
				{ID: id + "-a", TenantID: "default", RunID: id, Seq: 0, Name: "a", Stage: "s", Steps: []model.Step{{Name: "x", Run: "true"}}, Status: model.StatusQueued, CreatedAt: r.CreatedAt},
				{ID: id + "-b", TenantID: "default", RunID: id, Seq: 1, Name: "b", Stage: "s", Needs: []string{"a"}, Env: map[string]string{"K": "V"}, Status: model.StatusPending, CreatedAt: r.CreatedAt},
			}
			must(t, s.Atomic(ctx, func(tx store.Tx) error { return tx.CreateRun(ctx, r, jobs) }))
			numbers = append(numbers, r.Number)
		}
		if numbers[0] != 1 || numbers[1] != 2 {
			t.Fatalf("run numbers = %v", numbers)
		}
		runs, err := s.ListRuns(ctx, "p1", 10)
		if err != nil || len(runs) != 2 || runs[0].ID != "r2" {
			t.Fatalf("ListRuns = %v, %v", runs, err)
		}
		jobs, err := s.ListJobs(ctx, "r1")
		if err != nil || len(jobs) != 2 || jobs[1].Needs[0] != "a" || jobs[1].Env["K"] != "V" || jobs[0].Steps[0].Run != "true" {
			t.Fatalf("ListJobs = %+v, %v", jobs, err)
		}

		// 先入队的 Run 先被领取；租约过期检测。
		must(t, s.Atomic(ctx, func(tx store.Tx) error {
			j, err := tx.NextQueuedJob(ctx, store.ClaimFilter{})
			if err != nil || j == nil || j.ID != "r1-a" {
				t.Fatalf("NextQueuedJob = %v, %v", j, err)
			}
			j.Status, j.RunnerID, j.LeaseUntil = model.StatusRunning, "rn", now.Add(time.Minute)
			return tx.UpdateJob(ctx, j)
		}))
		if ids, _ := s.ExpiredJobRunIDs(ctx, now); len(ids) != 0 {
			t.Fatalf("expired too early: %v", ids)
		}
		if ids, _ := s.ExpiredJobRunIDs(ctx, now.Add(2*time.Minute)); len(ids) != 1 || ids[0] != "r1" {
			t.Fatalf("expired = %v", ids)
		}

		must(t, s.AppendLog(ctx, "r1-a", []byte("abc")))
		must(t, s.AppendLog(ctx, "r1-a", []byte("def")))
		for offset, want := range map[int]string{0: "abcdef", 2: "cdef", 3: "def", 6: ""} {
			if got, err := s.ReadLog(ctx, "r1-a", offset); err != nil || string(got) != want {
				t.Errorf("ReadLog(%d) = %q, %v; want %q", offset, got, err, want)
			}
		}
		if err := s.AppendLog(ctx, "nope", []byte("x")); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("AppendLog unknown job err = %v", err)
		}
		if _, err := s.ReadLog(ctx, "nope", 0); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ReadLog unknown job err = %v", err)
		}
	})
}

func TestTriggerContract(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		must(t, s.CreatePipeline(ctx, &model.Pipeline{ID: "p1", TenantID: "default", Name: "demo", YAML: "x", CreatedAt: now, UpdatedAt: now}))
		if err := s.CreateTrigger(ctx, &model.Trigger{ID: "orphan", TenantID: "default", PipelineID: "nope", Kind: model.TriggerCron}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("trigger for unknown pipeline err = %v", err)
		}
		at := func(m int) *time.Time { v := now.Add(time.Duration(m) * time.Minute); return &v }
		for i, tr := range []*model.Trigger{
			{ID: "c1", Kind: model.TriggerCron, Enabled: true, NextRunAt: at(5), TriggerConfig: model.TriggerConfig{Schedule: "*/5 * * * *", Timezone: "UTC"}},
			{ID: "c2", Kind: model.TriggerCron, Enabled: true, NextRunAt: at(1), TriggerConfig: model.TriggerConfig{Schedule: "* * * * *", Env: map[string]string{"A": "1"}}},
			{ID: "c3", Kind: model.TriggerCron, Enabled: false, NextRunAt: at(0)},
			{ID: "w1", Kind: model.TriggerWebhook, Enabled: true, Secret: "s", TriggerConfig: model.TriggerConfig{Branches: []string{"main"}, Tags: []string{"v*"}}},
		} {
			tr.TenantID, tr.PipelineID, tr.CreatedAt, tr.UpdatedAt = "default", "p1", now.Add(time.Duration(i)*time.Second), now
			must(t, s.CreateTrigger(ctx, tr))
		}
		list, err := s.ListTriggers(ctx, "p1")
		if err != nil || len(list) != 4 || list[0].ID != "c1" || list[3].ID != "w1" {
			t.Fatalf("ListTriggers = %v, %v", list, err)
		}
		w, err := s.GetTrigger(ctx, "w1")
		if err != nil || w.Secret != "s" || w.Branches[0] != "main" || w.Tags[0] != "v*" || w.NextRunAt != nil {
			t.Fatalf("GetTrigger = %+v, %v", w, err)
		}

		// 到期顺序按 next_run_at；未到期、停用、webhook 不返回。
		var fired []string
		for {
			var id string
			must(t, s.Atomic(ctx, func(tx store.Tx) error {
				tr, err := tx.NextDueTrigger(ctx, now.Add(5*time.Minute))
				if err != nil || tr == nil {
					return err
				}
				id = tr.ID
				return tx.MarkTriggerFired(ctx, tr.ID, at(60), now, "boom")
			}))
			if id == "" {
				break
			}
			fired = append(fired, id)
		}
		if len(fired) != 2 || fired[0] != "c2" || fired[1] != "c1" {
			t.Fatalf("due order = %v", fired)
		}
		c2, _ := s.GetTrigger(ctx, "c2")
		if !c2.NextRunAt.Equal(*at(60)) || !c2.LastFiredAt.Equal(now) || c2.LastError != "boom" || c2.Env["A"] != "1" {
			t.Fatalf("after fire = %+v", c2)
		}

		// UpdateTrigger 不覆盖触发结果。
		c2.Enabled, c2.Schedule, c2.LastError, c2.LastFiredAt = false, "0 * * * *", "", nil
		must(t, s.UpdateTrigger(ctx, c2))
		c2, _ = s.GetTrigger(ctx, "c2")
		if c2.Enabled || c2.Schedule != "0 * * * *" || c2.LastError != "boom" || c2.LastFiredAt == nil {
			t.Fatalf("after update = %+v", c2)
		}

		must(t, s.DeleteTrigger(ctx, "c2"))
		if _, err := s.GetTrigger(ctx, "c2"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("deleted trigger err = %v", err)
		}
		if err := s.DeleteTrigger(ctx, "c2"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("double delete err = %v", err)
		}
		if err := s.UpdateTrigger(ctx, c2); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("update deleted err = %v", err)
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
