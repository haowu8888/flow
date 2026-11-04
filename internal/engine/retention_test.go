package engine

import (
	"strings"
	"testing"
	"time"

	"flow/internal/model"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

// finishRun 领取并完成运行中的全部 Job，带产物的 Job 上传一个产物。
func finishRun(t *testing.T, e *Engine, status model.Status) {
	t.Helper()
	for {
		a, err := e.Claim(ctx, RunnerInfo{ID: "r"})
		must(t, err)
		if a == nil {
			return
		}
		if len(a.Job.Artifacts) > 0 {
			_, err := e.UploadArtifact(ctx, a.Job.ID, "r", strings.NewReader("data"))
			must(t, err)
		}
		must(t, e.Complete(ctx, a.Job.ID, "r", status, 0, ""))
	}
}

func TestArtifactRetention(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		e := New(s, 30*time.Second, WithArtifactRetention(48*time.Hour))
		e.now = func() time.Time { return clock }
		yaml := func(name, retention string) string {
			return "name: " + name + "\n" + retention + "stages: [{name: s, jobs: [{name: j, artifacts: [out], steps: [{run: x}]}]}]\n"
		}
		def, err := e.CreatePipeline(ctx, DefaultTenant, yaml("default-ttl", ""), "")
		must(t, err)
		short, err := e.CreatePipeline(ctx, DefaultTenant, yaml("short", "retention: {artifacts: 1h, runs: 2}\n"), "")
		must(t, err)
		forever, err := e.CreatePipeline(ctx, DefaultTenant, yaml("forever", "retention: {artifacts: \"0\"}\n"), "")
		must(t, err)

		run := func(p *model.Pipeline, status model.Status) *model.Run {
			r, err := e.TriggerRun(ctx, p.ID, "manual", "", nil)
			must(t, err)
			finishRun(t, e, status)
			return r
		}
		d1 := run(def, model.StatusSuccess)
		d2 := run(def, model.StatusFailed)
		d3 := run(def, model.StatusFailed)
		s1 := run(short, model.StatusSuccess)
		s2 := run(short, model.StatusSuccess)
		f1 := run(forever, model.StatusFailed)

		arts, _ := e.Artifacts(ctx, d1.ID)
		if len(arts) != 1 || arts[0].ExpiresAt == nil || !arts[0].ExpiresAt.Equal(clock.Add(48*time.Hour)) {
			t.Fatalf("default ttl artifacts = %+v", arts)
		}
		s1Arts, _ := e.Artifacts(ctx, s1.ID)
		if !s1Arts[0].ExpiresAt.Equal(clock.Add(time.Hour)) {
			t.Errorf("short ttl = %v", s1Arts[0].ExpiresAt)
		}
		if arts, _ := e.Artifacts(ctx, f1.ID); arts[0].ExpiresAt != nil {
			t.Errorf("forever ttl = %v", arts[0].ExpiresAt)
		}

		count := func(runID string) int {
			arts, err := e.Artifacts(ctx, runID)
			must(t, err)
			return len(arts)
		}
		// 2 小时后：short 的产物过期，但 s2 是最近一次成功运行，保留。
		clock = clock.Add(2 * time.Hour)
		n, _, err := e.ApplyRetention(ctx)
		must(t, err)
		if n != 1 || count(s1.ID) != 0 || count(s2.ID) != 1 || count(d1.ID) != 1 {
			t.Errorf("after 2h: removed %d, s1=%d s2=%d d1=%d", n, count(s1.ID), count(s2.ID), count(d1.ID))
		}
		if rc, err := e.blobs.Open(ctx, s1Arts[0].BlobKey); err == nil {
			rc.Close()
			t.Error("expired artifact blob not deleted")
		}
		// 3 天后：default 的失败运行产物过期；d1 是最近一次成功运行，保留；forever 不过期。
		clock = clock.Add(72 * time.Hour)
		n, _, err = e.ApplyRetention(ctx)
		must(t, err)
		if n != 2 || count(d1.ID) != 1 || count(d2.ID) != 0 || count(d3.ID) != 0 || count(f1.ID) != 1 {
			t.Errorf("after 3d: removed %d, d1=%d d2=%d d3=%d f1=%d", n, count(d1.ID), count(d2.ID), count(d3.ID), count(f1.ID))
		}

		// retention.runs: 2 —— 第 3 个运行结束后删除最旧的运行。
		s3 := run(short, model.StatusFailed)
		_, pruned, err := e.ApplyRetention(ctx)
		must(t, err)
		runs, _ := e.Runs(ctx, short.ID, 10)
		if pruned != 1 || len(runs) != 2 || runs[0].ID != s3.ID || runs[1].ID != s2.ID {
			t.Errorf("pruned %d, runs = %v", pruned, runs)
		}
		if _, err := e.Run(ctx, s1.ID); err == nil {
			t.Error("oldest run not deleted")
		}
		if runs, _ := e.Runs(ctx, def.ID, 10); len(runs) != 3 {
			t.Errorf("pipeline without runs limit pruned: %d", len(runs))
		}
	})
}
