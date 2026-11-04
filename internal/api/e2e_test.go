package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"flow/internal/auth"
	"flow/internal/engine"
	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/runner"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

const (
	sharedRunnerToken = "secret"
	adminPassword     = "admin-password"
)

// stack 是测试用的 Server，token 为管理员 admin 的会话令牌，辅助函数默认以它鉴权。
type stack struct {
	*httptest.Server
	eng   *engine.Engine
	token string
}

// newStack 启动 Server（不带 Runner）并以管理员登录。
func newStack(t *testing.T, s store.Store) *stack {
	t.Helper()
	eng := engine.New(s, 5*time.Second)
	authSvc := auth.NewService(s)
	if _, _, err := authSvc.Bootstrap(context.Background(), adminPassword); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewHandler(eng, authSvc, Config{RunnerToken: sharedRunnerToken}))
	t.Cleanup(ts.Close)
	st := &stack{Server: ts, eng: eng}
	st.token = st.login(t, "admin", adminPassword)
	return st
}

func (st *stack) login(t *testing.T, user, password string) string {
	t.Helper()
	var resp struct {
		Token string `json:"token"`
	}
	code, body := st.as("").do(t, http.MethodPost, "/api/auth/login", fmt.Sprintf(`{"username":%q,"password":%q}`, user, password))
	if code != http.StatusOK || json.Unmarshal([]byte(body), &resp) != nil || resp.Token == "" {
		t.Fatalf("login %s = %d: %s", user, code, body)
	}
	return resp.Token
}

// as 返回以另一令牌（空为匿名）发请求的视图。
func (st *stack) as(token string) *stack {
	c := *st
	c.token = token
	return &c
}

// startRunner 启动一个 Runner，直到测试结束。
func (st *stack) startRunner(t *testing.T, cfg runner.Config) {
	t.Helper()
	cfg.ServerURL = st.URL
	if cfg.WorkDir == "" {
		cfg.WorkDir = t.TempDir()
	}
	cfg.PollInterval, cfg.HeartbeatInterval, cfg.LogFlushInterval = 50*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.New(cfg).Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

// 端到端：真实 HTTP Server + 共享 Token 的 Runner 执行 shell Step。
func startStack(t *testing.T, s store.Store) *stack {
	t.Helper()
	st := newStack(t, s)
	st.startRunner(t, runner.Config{Token: sharedRunnerToken, RunnerID: "test-runner", Concurrency: 2})
	return st
}

func TestEndToEnd(t *testing.T) { storetest.ForEach(t, testEndToEnd) }

func testEndToEnd(t *testing.T, s store.Store) {
	ts := startStack(t, s)
	p := createPipeline(t, ts, `
name: e2e
env: {GREETING: hi}
stages:
  - name: build
    jobs:
      - name: compile
        steps:
          - {name: hello, run: 'echo "$GREETING $WHO from $FLOW_JOB"'}
          - run: echo artifact > out.txt && cat out.txt
  - name: test
    jobs:
      - {name: unit, steps: [{run: "echo testing; exit 3"}]}
  - name: deploy
    jobs:
      - {name: prod, steps: [{run: echo deploy}]}
`)
	detail := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", `{"env":{"WHO":"flow"}}`, http.StatusCreated)
	detail = waitRun(t, ts, detail.Run.ID)

	if detail.Run.Status != model.StatusFailed {
		t.Errorf("run status = %s", detail.Run.Status)
	}
	jobs := map[string]*model.Job{}
	for _, j := range detail.Jobs {
		jobs[j.Name] = j
	}
	if j := jobs["compile"]; j.Status != model.StatusSuccess {
		t.Errorf("compile = %s (%s)", j.Status, j.Error)
	}
	if j := jobs["unit"]; j.Status != model.StatusFailed || j.ExitCode != 3 {
		t.Errorf("unit = %s exit %d", j.Status, j.ExitCode)
	}
	if j := jobs["prod"]; j.Status != model.StatusSkipped {
		t.Errorf("prod = %s", j.Status)
	}
	logText := getText(t, ts, "/api/jobs/"+jobs["compile"].ID+"/logs")
	for _, want := range []string{"hi flow from compile", "artifact", "==> success"} {
		if !strings.Contains(logText, want) {
			t.Errorf("compile log missing %q:\n%s", want, logText)
		}
	}
}

func TestCancelKillsRunningStep(t *testing.T) { storetest.ForEach(t, testCancelKillsRunningStep) }

func testCancelKillsRunningStep(t *testing.T, s store.Store) {
	ts := startStack(t, s)
	p := createPipeline(t, ts, `
name: slow
stages: [{name: s, jobs: [{name: sleepy, steps: [{run: "echo start; sleep 30"}]}]}]
`)
	detail := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	jobID := detail.Jobs[0].ID
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(getText(t, ts, "/api/jobs/"+jobID+"/logs"), "start") {
		if time.Now().After(deadline) {
			t.Fatal("job did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	start := time.Now()
	post[protocol.RunDetail](t, ts, "/api/runs/"+detail.Run.ID+"/cancel", "", http.StatusOK)
	detail = waitRun(t, ts, detail.Run.ID)
	if detail.Run.Status != model.StatusCanceled || detail.Jobs[0].Status != model.StatusCanceled {
		t.Fatalf("run = %s, job = %s", detail.Run.Status, detail.Jobs[0].Status)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancel took %s", elapsed)
	}
}

func TestRunnerAuth(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	resp, err := http.Post(ts.URL+"/api/runner/claim", "application/json", strings.NewReader(`{"runner_id":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func createPipeline(t *testing.T, ts *stack, yaml string) model.Pipeline {
	t.Helper()
	return post[model.Pipeline](t, ts, "/api/pipelines", yaml, http.StatusCreated)
}

func waitRun(t *testing.T, ts *stack, runID string) protocol.RunDetail {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var d protocol.RunDetail
		if err := json.Unmarshal([]byte(getText(t, ts, "/api/runs/"+runID)), &d); err != nil {
			t.Fatal(err)
		}
		if d.Run.Status.Terminal() {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("run still %s", d.Run.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func post[T any](t *testing.T, ts *stack, path, body string, wantCode int) T {
	t.Helper()
	code, data := ts.do(t, http.MethodPost, path, body)
	if code != wantCode {
		t.Fatalf("POST %s = %d: %s", path, code, data)
	}
	var v T
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func getText(t *testing.T, ts *stack, path string) string {
	t.Helper()
	code, data := ts.do(t, http.MethodGet, path, "")
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, code, data)
	}
	return data
}
