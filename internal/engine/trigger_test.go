package engine

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"flow/internal/model"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

const simpleYAML = `
name: simple
stages:
  - name: build
    jobs:
      - {name: a, steps: [{run: "true"}]}
`

func newPipeline(t *testing.T, s store.Store) (*Engine, *model.Pipeline, *time.Time) {
	t.Helper()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := New(s, 30*time.Second)
	e.now = func() time.Time { return clock }
	p, err := e.CreatePipeline(ctx, DefaultTenant, simpleYAML, "")
	if err != nil {
		t.Fatal(err)
	}
	return e, p, &clock
}

func runCount(t *testing.T, e *Engine, pipelineID string) int {
	t.Helper()
	runs, err := e.Runs(ctx, pipelineID, 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(runs)
}

func TestTriggerValidation(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, p, _ := newPipeline(t, s)
		bad := []struct {
			kind model.TriggerKind
			cfg  model.TriggerConfig
		}{
			{"manual", model.TriggerConfig{}},
			{model.TriggerCron, model.TriggerConfig{Schedule: "every day"}},
			{model.TriggerCron, model.TriggerConfig{Schedule: "* * * * *", Timezone: "Nowhere/City"}},
			{model.TriggerCron, model.TriggerConfig{Schedule: "* * * * *", Env: map[string]string{"1BAD": "x"}}},
			{model.TriggerWebhook, model.TriggerConfig{Env: map[string]string{"A-B": "x"}}},
		}
		for _, c := range bad {
			if _, err := e.CreateTrigger(ctx, p.ID, c.kind, true, c.cfg); !errors.Is(err, ErrInvalid) {
				t.Errorf("CreateTrigger(%s, %+v) err = %v", c.kind, c.cfg, err)
			}
		}
		if _, err := e.CreateTrigger(ctx, "nope", model.TriggerCron, true, model.TriggerConfig{Schedule: "@daily"}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown pipeline err = %v", err)
		}
		// 与类型无关的字段被清除，过滤规则去空白。
		tr, err := e.CreateTrigger(ctx, p.ID, model.TriggerWebhook, true,
			model.TriggerConfig{Branches: []string{" main ", ""}, Schedule: "@daily"})
		if err != nil || len(tr.Branches) != 1 || tr.Branches[0] != "main" || tr.Schedule != "" || len(tr.Secret) != 40 {
			t.Fatalf("webhook trigger = %+v, %v", tr, err)
		}
		if _, err := e.RotateTriggerSecret(ctx, tr.ID); err != nil {
			t.Fatal(err)
		}
		if rotated, _ := e.Trigger(ctx, tr.ID); rotated.Secret == tr.Secret {
			t.Error("secret not rotated")
		}
	})
}

func TestCronTrigger(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, p, clock := newPipeline(t, s)
		tr, err := e.CreateTrigger(ctx, p.ID, model.TriggerCron, true,
			model.TriggerConfig{Schedule: "*/5 * * * *", Timezone: "UTC", Env: map[string]string{"NIGHTLY": "1"}})
		must(t, err)
		if tr.NextRunAt == nil || !tr.NextRunAt.Equal(clock.Add(5*time.Minute)) {
			t.Fatalf("next = %v", tr.NextRunAt)
		}

		*clock = clock.Add(4 * time.Minute)
		if n, err := e.FireDueCron(ctx); n != 0 || err != nil {
			t.Fatalf("fired early: %d, %v", n, err)
		}
		// 停机错过 00:05、00:10，只补触发一次，下次从当前时刻起算。
		*clock = clock.Add(8 * time.Minute) // 00:12
		if n, err := e.FireDueCron(ctx); n != 1 || err != nil {
			t.Fatalf("FireDueCron = %d, %v", n, err)
		}
		if n, _ := e.FireDueCron(ctx); n != 0 {
			t.Fatal("fired twice in the same minute")
		}
		runs, _ := e.Runs(ctx, p.ID, 100)
		if len(runs) != 1 || runs[0].Trigger != "cron" || runs[0].Env["NIGHTLY"] != "1" {
			t.Fatalf("runs = %+v", runs)
		}
		jobs, _ := e.Jobs(ctx, runs[0].ID)
		if jobs[0].Env["FLOW_TRIGGER"] != "cron" || jobs[0].Status != model.StatusQueued {
			t.Fatalf("job = %+v", jobs[0])
		}
		tr, _ = e.Trigger(ctx, tr.ID)
		if !tr.NextRunAt.Equal(time.Date(2026, 1, 1, 0, 15, 0, 0, time.UTC)) || !tr.LastFiredAt.Equal(*clock) || tr.LastError != "" {
			t.Fatalf("after fire = %+v", tr)
		}

		// 停用后不再调度；重新启用时从当前时刻计算下次时间。
		tr, err = e.UpdateTrigger(ctx, tr.ID, false, tr.TriggerConfig)
		if err != nil || tr.NextRunAt != nil || tr.LastFiredAt == nil {
			t.Fatalf("disable = %+v, %v", tr, err)
		}
		*clock = clock.Add(time.Hour)
		if n, _ := e.FireDueCron(ctx); n != 0 {
			t.Fatal("disabled trigger fired")
		}
		tr, err = e.UpdateTrigger(ctx, tr.ID, true, model.TriggerConfig{Schedule: "0 * * * *", Timezone: "UTC"})
		if err != nil || !tr.NextRunAt.Equal(time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)) {
			t.Fatalf("re-enable = %+v, %v", tr, err)
		}

		// 流水线定义失效时记录错误、继续调度，而不是卡住。
		bad, _ := s.GetPipeline(ctx, p.ID)
		bad.YAML = "name: broken"
		must(t, s.UpdatePipeline(ctx, bad))
		*clock = clock.Add(time.Hour)
		if _, err := e.FireDueCron(ctx); err != nil {
			t.Fatal(err)
		}
		tr, _ = e.Trigger(ctx, tr.ID)
		if tr.LastError == "" || !tr.NextRunAt.After(*clock) || runCount(t, e, p.ID) != 1 {
			t.Fatalf("broken pipeline = %+v", tr)
		}
	})
}

// 多个 Server 实例同时调度同一个到期触发器，只应创建一个 Run。
func TestCronConcurrentSchedulers(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, p, clock := newPipeline(t, s)
		_, err := e.CreateTrigger(ctx, p.ID, model.TriggerCron, true, model.TriggerConfig{Schedule: "* * * * *", Timezone: "UTC"})
		must(t, err)
		at := clock.Add(time.Minute)
		var wg sync.WaitGroup
		for range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				other := New(s, 30*time.Second)
				other.now = func() time.Time { return at }
				if _, err := other.FireDueCron(ctx); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if n := runCount(t, e, p.ID); n != 1 {
			t.Fatalf("runs = %d, want 1", n)
		}
	})
}

func githubPush(t *testing.T, secret, event, body string) http.Header {
	t.Helper()
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return http.Header{
		"X-Github-Event":      {event},
		"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(m.Sum(nil))},
	}
}

func TestWebhookTrigger(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, p, _ := newPipeline(t, s)
		tr, err := e.CreateTrigger(ctx, p.ID, model.TriggerWebhook, true,
			model.TriggerConfig{Branches: []string{"main", "release/*"}, Env: map[string]string{"DEPLOY": "yes"}})
		must(t, err)
		push := func(ref string) string {
			return `{"ref":"` + ref + `","after":"abc123","repository":{"full_name":"acme/app","clone_url":"https://git.example.com/acme/app.git"}}`
		}

		body := push("refs/heads/release/1.0")
		res, err := e.HandleWebhook(ctx, tr.ID, githubPush(t, tr.Secret, "push", body), []byte(body))
		if err != nil || res.Run == nil || res.Run.Trigger != "webhook" {
			t.Fatalf("push = %+v, %v", res, err)
		}
		if env := res.Run.Env; env["FLOW_GIT_BRANCH"] != "release/1.0" || env["FLOW_GIT_COMMIT"] != "abc123" ||
			env["FLOW_GIT_URL"] != "https://git.example.com/acme/app.git" || env["DEPLOY"] != "yes" {
			t.Fatalf("env = %v", env)
		}
		if tr, _ := e.Trigger(ctx, tr.ID); tr.LastFiredAt == nil {
			t.Error("LastFiredAt not recorded")
		}

		skipped := []struct{ event, body string }{
			{"push", push("refs/heads/dev")},
			{"push", push("refs/tags/v1")},
			{"push", `{"ref":"refs/heads/main","after":"0000000000000000000000000000000000000000"}`},
			{"ping", `{"zen":"hi"}`},
			{"pull_request", `{}`},
			{"push", `{"ref":"refs/heads/main","after":"abc124","head_commit":{"id":"abc124","message":"docs: typo [skip ci]"}}`},
			{"push", `{"ref":"refs/heads/main","after":"abc125","head_commit":{"id":"abc125","message":"WIP\n\n[CI SKIP]"}}`},
		}
		for _, c := range skipped {
			res, err := e.HandleWebhook(ctx, tr.ID, githubPush(t, tr.Secret, c.event, c.body), []byte(c.body))
			if err != nil || res.Run != nil || res.Skipped == "" {
				t.Errorf("%s %s = %+v, %v", c.event, c.body, res, err)
			}
		}

		body = push("refs/heads/main")
		if _, err := e.HandleWebhook(ctx, tr.ID, githubPush(t, "wrong", "push", body), []byte(body)); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("bad signature err = %v", err)
		}
		if _, err := e.HandleWebhook(ctx, tr.ID, githubPush(t, tr.Secret, "push", "garbage"), []byte("garbage")); !errors.Is(err, ErrInvalid) {
			t.Errorf("bad payload err = %v", err)
		}
		_, err = e.UpdateTrigger(ctx, tr.ID, false, tr.TriggerConfig)
		must(t, err)
		if res, err := e.HandleWebhook(ctx, tr.ID, githubPush(t, tr.Secret, "push", body), []byte(body)); err != nil || res.Run != nil {
			t.Errorf("disabled = %+v, %v", res, err)
		}
		if n := runCount(t, e, p.ID); n != 1 {
			t.Fatalf("runs = %d, want 1", n)
		}

		cron, err := e.CreateTrigger(ctx, p.ID, model.TriggerCron, true, model.TriggerConfig{Schedule: "@daily"})
		must(t, err)
		if _, err := e.HandleWebhook(ctx, cron.ID, http.Header{}, nil); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("webhook on cron trigger err = %v", err)
		}
	})
}

func prBody(action, headRepo, target string) string {
	return `{"action":"` + action + `","number":7,"pull_request":{"number":7,"title":"Add feature",
		"head":{"ref":"feat","sha":"h1","repo":{"full_name":"` + headRepo + `"}},
		"base":{"ref":"` + target + `","repo":{"full_name":"acme/app"}},"user":{"login":"alice"}}}`
}

func TestPullRequestTrigger(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, _, _ := newPipeline(t, s)
		p, err := e.CreatePipeline(ctx, DefaultTenant, `
name: with-source
source: {url: "https://git.example.com/acme/app.git", branch: main}
stages:
  - name: build
    jobs: [{name: a, steps: [{run: "true"}]}]
`, "")
		must(t, err)
		send := func(tr *model.Trigger, event, body string) (*WebhookResult, error) {
			return e.HandleWebhook(ctx, tr.ID, githubPush(t, tr.Secret, event, body), []byte(body))
		}

		pushOnly, err := e.CreateTrigger(ctx, p.ID, model.TriggerWebhook, true, model.TriggerConfig{})
		must(t, err)
		if got := pushOnly.Events; len(got) != 1 || got[0] != "push" {
			t.Errorf("default events = %v", got)
		}
		if res, err := send(pushOnly, "pull_request", prBody("opened", "acme/app", "main")); err != nil || res.Run != nil {
			t.Errorf("pr on push-only trigger = %+v, %v", res, err)
		}

		tr, err := e.CreateTrigger(ctx, p.ID, model.TriggerWebhook, true,
			model.TriggerConfig{Events: []string{"pull_request", "push", "pull_request"}, Branches: []string{"main"}, AllowForks: false})
		must(t, err)
		if got := tr.Events; len(got) != 2 || got[0] != "push" || got[1] != "pull_request" {
			t.Errorf("events = %v", got)
		}
		res, err := send(tr, "pull_request", prBody("opened", "acme/app", "main"))
		if err != nil || res.Run == nil {
			t.Fatalf("pr opened = %+v, %v", res, err)
		}
		env := res.Run.Env
		if env["FLOW_GIT_EVENT"] != "pull_request" || env["FLOW_GIT_PR_NUMBER"] != "7" || env["FLOW_GIT_BRANCH"] != "" {
			t.Errorf("env = %v", env)
		}
		jobs, _ := e.Jobs(ctx, res.Run.ID)
		if co := jobs[0].Checkout; co == nil || co.Ref != "refs/pull/7/head" || co.Commit != "h1" {
			t.Errorf("checkout = %+v", co)
		}

		skipped := map[string]string{
			"closed":      prBody("closed", "acme/app", "main"),
			"fork":        prBody("opened", "mallory/app", "main"),
			"wrong base":  prBody("opened", "acme/app", "dev"),
			"title edit":  prBody("edited", "acme/app", "main"),
			"bad payload": `{"action":"opened"}`,
		}
		for name, body := range skipped {
			res, err := send(tr, "pull_request", body)
			if name == "bad payload" {
				if !errors.Is(err, ErrInvalid) {
					t.Errorf("%s: err = %v", name, err)
				}
				continue
			}
			if err != nil || res.Run != nil || res.Skipped == "" {
				t.Errorf("%s = %+v, %v", name, res, err)
			}
		}

		// 允许 fork 后可以运行
		cfg := tr.TriggerConfig
		cfg.AllowForks = true
		tr, err = e.UpdateTrigger(ctx, tr.ID, true, cfg)
		must(t, err)
		if res, err := send(tr, "pull_request", prBody("synchronize", "mallory/app", "main")); err != nil || res.Run == nil {
			t.Errorf("fork allowed = %+v, %v", res, err)
		}

		if _, err := e.CreateTrigger(ctx, p.ID, model.TriggerWebhook, true, model.TriggerConfig{Events: []string{"issues"}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("unknown event err = %v", err)
		}
		cron, err := e.CreateTrigger(ctx, p.ID, model.TriggerCron, true,
			model.TriggerConfig{Schedule: "@daily", Events: []string{"push"}, AllowForks: true})
		must(t, err)
		if len(cron.Events) != 0 || cron.AllowForks {
			t.Errorf("cron kept webhook fields: %+v", cron.TriggerConfig)
		}
	})
}

func TestWebhookDeliveryDedup(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, p, clock := newPipeline(t, s)
		tr, err := e.CreateTrigger(ctx, p.ID, model.TriggerWebhook, true, model.TriggerConfig{})
		must(t, err)
		body := `{"ref":"refs/heads/main","after":"abc"}`
		deliver := func(id string) (*WebhookResult, error) {
			h := githubPush(t, tr.Secret, "push", body)
			if id != "" {
				h.Set("X-GitHub-Delivery", id)
			}
			return e.HandleWebhook(ctx, tr.ID, h, []byte(body))
		}

		first, err := deliver("d-1")
		if err != nil || first.Run == nil || first.Duplicate {
			t.Fatalf("first = %+v, %v", first, err)
		}
		again, err := deliver("d-1")
		if err != nil || !again.Duplicate || again.Run == nil || again.Run.ID != first.Run.ID {
			t.Fatalf("redelivery = %+v, %v", again, err)
		}
		// 并发重复投递（PostgreSQL 中由唯一键串行化）只创建一个 Run。
		var wg sync.WaitGroup
		dups := make(chan bool, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := deliver("d-2")
				if err != nil {
					t.Error(err)
					return
				}
				dups <- res.Duplicate
			}()
		}
		wg.Wait()
		close(dups)
		fresh := 0
		for d := range dups {
			if !d {
				fresh++
			}
		}
		if fresh != 1 {
			t.Errorf("concurrent deliveries created %d runs, want 1", fresh)
		}
		// 无投递 ID 的投递不去重。
		for range 2 {
			if res, err := deliver(""); err != nil || res.Run == nil {
				t.Fatalf("no delivery id = %+v, %v", res, err)
			}
		}
		if n := runCount(t, e, p.ID); n != 4 {
			t.Errorf("runs = %d, want 4", n)
		}

		// 过期记录清理后，同一投递 ID 会再次触发。
		*clock = clock.Add(deliveryRetention + time.Hour)
		must(t, e.CleanupDeliveries(ctx))
		if res, err := deliver("d-1"); err != nil || res.Duplicate || res.Run == nil {
			t.Errorf("after cleanup = %+v, %v", res, err)
		}
	})
}
