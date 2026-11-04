package engine

import (
	"errors"
	"testing"
	"time"

	"flow/internal/model"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

const oneJobYAML = `
name: one
stages:
  - name: build
    jobs: [{name: build, steps: [{run: "true"}]}]
`

// 回收站：移入后不在列表中、不能运行或保存；恢复后照常；只有回收站中的才能彻底删除。
func TestTrashPipeline(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, run, _ := setup(t, s, oneJobYAML)
		id := run.PipelineID
		must(t, e.SetPipelineInfo(ctx, id, "  静态站点  ", true))
		if err := e.PurgePipeline(ctx, id); !errors.Is(err, ErrInvalid) {
			t.Fatalf("purge outside trash err = %v", err)
		}

		must(t, e.TrashPipeline(ctx, id))
		if ps, _ := e.Pipelines(ctx, DefaultTenant); len(ps) != 0 {
			t.Fatalf("pipelines after trash = %d", len(ps))
		}
		trash, err := e.TrashedPipelines(ctx, DefaultTenant)
		must(t, err)
		if len(trash) != 1 || trash[0].DeletedAt == nil || trash[0].Description != "静态站点" || !trash[0].Archived {
			t.Fatalf("trash = %+v", trash)
		}
		if _, err := e.TriggerRun(ctx, id, "manual", "", nil); !errors.Is(err, ErrInTrash) {
			t.Fatalf("trigger in trash err = %v", err)
		}
		if _, err := e.UpdatePipeline(ctx, id, oneJobYAML+"# x\n", "bob", 0); !errors.Is(err, ErrInTrash) {
			t.Fatalf("update in trash err = %v", err)
		}
		// 运行记录保留
		if _, err := e.Run(ctx, run.ID); err != nil {
			t.Fatalf("run after trash: %v", err)
		}

		must(t, e.RestorePipeline(ctx, id))
		p, err := e.UpdatePipeline(ctx, id, oneJobYAML+"# x\n", "bob", 0)
		must(t, err)
		if p.Description != "静态站点" || !p.Archived || p.DeletedAt != nil {
			t.Fatalf("meta lost on save: %+v", p)
		}
		if _, err := e.TriggerRun(ctx, id, "manual", "", nil); err != nil {
			t.Fatalf("trigger after restore: %v", err)
		}

		must(t, e.TrashPipeline(ctx, id))
		must(t, e.PurgePipeline(ctx, id))
		if _, err := e.Pipeline(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("pipeline after purge err = %v", err)
		}
	})
}

func TestDashboard(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, run, clock := setup(t, s, oneJobYAML)
		mine, err := e.CreatePipeline(ctx, DefaultTenant, "name: mine\n"+oneJobYAML[len("\nname: one\n"):], "alice")
		must(t, err)

		// one #1 成功（耗时 30s），mine #1 失败，one #2 仍在排队。
		j := claim(t, e, "build")
		*clock = clock.Add(30 * time.Second)
		must(t, e.Complete(ctx, j.ID, "r1", model.StatusSuccess, 0, ""))
		_, err = e.TriggerRun(ctx, mine.ID, "manual", "alice", nil)
		must(t, err)
		must(t, e.Complete(ctx, claim(t, e, "build").ID, "r1", model.StatusFailed, 1, ""))
		queued, err := e.TriggerRun(ctx, run.PipelineID, "manual", "", nil)
		must(t, err)

		d, err := e.Dashboard(ctx, DefaultTenant, "alice", 7, nil)
		must(t, err)
		if d.Pipelines != 2 || d.Mine != 1 {
			t.Errorf("pipelines = %d, mine = %d", d.Pipelines, d.Mine)
		}
		c := d.Current
		if c.Runs != 3 || c.Success != 1 || c.Failed != 1 || c.Queued != 1 || c.SuccessRate != 0.5 || c.AvgDuration != 30 {
			t.Errorf("current = %+v", c)
		}
		if d.Previous.Runs != 0 || d.Previous.SuccessRate != -1 {
			t.Errorf("previous = %+v", d.Previous)
		}
		if len(d.Daily) != 7 || d.Daily[6].Success != 1 || d.Daily[6].Failed != 1 {
			t.Errorf("daily = %+v", d.Daily)
		}
		if len(d.Active) != 1 || d.Active[0].ID != queued.ID || d.Active[0].PipelineName != "one" {
			t.Errorf("active = %+v", d.Active)
		}
		if len(d.Recent) != 3 || len(d.Ranking) != 2 || d.Ranking[0].Name != "one" || d.Ranking[0].Runs != 2 {
			t.Errorf("recent = %d, ranking = %+v", len(d.Recent), d.Ranking)
		}

		// 8 天后：这些运行落入上一周期。
		*clock = clock.AddDate(0, 0, 8)
		d, err = e.Dashboard(ctx, DefaultTenant, "alice", 7, nil)
		must(t, err)
		if d.Current.Runs != 0 || d.Previous.Runs != 3 {
			t.Errorf("after 8 days current = %+v previous = %+v", d.Current, d.Previous)
		}
	})
}
