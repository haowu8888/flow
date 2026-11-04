package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"flow/internal/model"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

const dagYAML = `
name: dag
stages:
  - name: build
    jobs:
      - {name: a, steps: [{run: "true"}]}
      - {name: b, steps: [{run: "true"}]}
  - name: test
    jobs:
      - {name: c, needs: [a], steps: [{run: "true"}]}
      - {name: d, steps: [{run: "true"}]}
`

var ctx = context.Background()

func setup(t *testing.T, s store.Store, yaml string) (*Engine, *model.Run, *time.Time) {
	t.Helper()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := New(s, 30*time.Second)
	e.now = func() time.Time { return clock }
	p, err := e.CreatePipeline(ctx, DefaultTenant, yaml, "")
	if err != nil {
		t.Fatal(err)
	}
	run, err := e.TriggerRun(ctx, p.ID, "manual", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return e, run, &clock
}

func statuses(t *testing.T, e *Engine, runID string) map[string]model.Status {
	t.Helper()
	jobs, err := e.Jobs(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]model.Status)
	for _, j := range jobs {
		out[j.Name] = j.Status
	}
	return out
}

// claimJob 以无标签共享 Runner 的身份领取 Job。
func claimJob(e *Engine, runner string) (*model.Job, error) {
	a, err := e.Claim(ctx, RunnerInfo{ID: runner})
	if a == nil {
		return nil, err
	}
	return a.Job, err
}

func claim(t *testing.T, e *Engine, want string) *model.Job {
	t.Helper()
	j, err := claimJob(e, "r1")
	if err != nil || j == nil || j.Name != want {
		t.Fatalf("claim = %v, %v; want %s", j, err, want)
	}
	return j
}

func TestDAGNeedsStartsEarly(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, run, _ := setup(t, s, dagYAML)
		a := claim(t, e, "a")
		claim(t, e, "b")
		if j, _ := claimJob(e, "r1"); j != nil {
			t.Fatalf("unexpected job %s before deps done", j.Name)
		}
		must(t, e.Complete(ctx, a.ID, "r1", model.StatusSuccess, 0, ""))
		// c 只依赖 a，b 仍在运行时即可启动；d 依赖整个 build 阶段，仍需等待。
		claim(t, e, "c")
		if got := statuses(t, e, run.ID)["d"]; got != model.StatusPending {
			t.Fatalf("d = %s, want pending", got)
		}
		if r, _ := e.Run(ctx, run.ID); r.Status != model.StatusRunning || r.StartedAt == nil {
			t.Fatalf("run = %+v", r)
		}
	})
}

func TestFailureSkipsDownstream(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, run, _ := setup(t, s, dagYAML)
		a := claim(t, e, "a")
		b := claim(t, e, "b")
		must(t, e.Complete(ctx, a.ID, "r1", model.StatusFailed, 1, "boom"))
		must(t, e.Complete(ctx, b.ID, "r1", model.StatusSuccess, 0, ""))
		want := map[string]model.Status{"a": "failed", "b": "success", "c": "skipped", "d": "skipped"}
		if got := statuses(t, e, run.ID); !equal(got, want) {
			t.Fatalf("statuses = %v", got)
		}
		if r, _ := e.Run(ctx, run.ID); r.Status != model.StatusFailed || r.FinishedAt == nil {
			t.Fatalf("run = %+v", r)
		}
	})
}

func TestCancelRun(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, run, _ := setup(t, s, dagYAML)
		a := claim(t, e, "a")
		if _, err := e.CancelRun(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		if cancel, err := e.Heartbeat(ctx, a.ID, "r1"); err != nil || !cancel {
			t.Fatalf("heartbeat = %v, %v; want cancel", cancel, err)
		}
		must(t, e.Complete(ctx, a.ID, "r1", model.StatusCanceled, -1, ""))
		if r, _ := e.Run(ctx, run.ID); r.Status != model.StatusCanceled {
			t.Fatalf("run status = %s", r.Status)
		}
	})
}

func TestReapExpiredLease(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, run, clock := setup(t, s, dagYAML)
		a := claim(t, e, "a")
		b := claim(t, e, "b")
		*clock = clock.Add(20 * time.Second)
		if _, err := e.Heartbeat(ctx, b.ID, "r1"); err != nil { // b 续租，a 不续
			t.Fatal(err)
		}
		*clock = clock.Add(11 * time.Second)
		if n, err := e.ReapExpired(ctx); err != nil || n != 1 {
			t.Fatalf("reap = %d, %v", n, err)
		}
		got := statuses(t, e, run.ID)
		if got["a"] != model.StatusFailed || got["b"] != model.StatusRunning {
			t.Fatalf("statuses = %v", got)
		}
		if err := e.Complete(ctx, a.ID, "r1", model.StatusSuccess, 0, ""); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("late complete err = %v, want ErrLeaseLost", err)
		}
	})
}

// 多个 Runner 并发领取，每个 Job 恰好被领取一次（PostgreSQL 下验证 SKIP LOCKED 与死锁重试）。
func TestConcurrentClaimAndComplete(t *testing.T) {
	const jobs = 30
	var sb strings.Builder
	sb.WriteString("name: wide\nstages:\n  - name: s\n    jobs:\n")
	for i := range jobs {
		fmt.Fprintf(&sb, "      - {name: j%d, steps: [{run: \"true\"}]}\n", i)
	}
	sb.WriteString("  - name: last\n    jobs:\n      - {name: final, steps: [{run: \"true\"}]}\n")

	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, run, _ := setup(t, s, sb.String())
		var mu sync.Mutex
		seen := make(map[string]int)
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for w := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				runner := fmt.Sprintf("r%d", w)
				for {
					j, err := claimJob(e, runner)
					if err != nil {
						errs <- err
						return
					}
					if j == nil {
						return
					}
					mu.Lock()
					seen[j.Name]++
					mu.Unlock()
					if err := e.Complete(ctx, j.ID, runner, model.StatusSuccess, 0, ""); err != nil {
						errs <- err
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		// 并发 worker 可能在 final 入队前全部退出，补领一次。
		if j, err := claimJob(e, "r0"); err != nil {
			t.Fatal(err)
		} else if j != nil {
			seen[j.Name]++
			must(t, e.Complete(ctx, j.ID, "r0", model.StatusSuccess, 0, ""))
		}
		if len(seen) != jobs+1 {
			t.Fatalf("claimed %d distinct jobs, want %d", len(seen), jobs+1)
		}
		for name, n := range seen {
			if n != 1 {
				t.Errorf("job %s claimed %d times", name, n)
			}
		}
		if r, _ := e.Run(ctx, run.ID); r.Status != model.StatusSuccess {
			t.Fatalf("run status = %s", r.Status)
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func equal(a, b map[string]model.Status) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
