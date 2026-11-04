package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"flow/internal/engine"
	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/runner"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

// 主机 Agent：主机以 Agent 方式接入（无 SSH），部署任务由 Runner 经 Server 下发给 Agent 执行，日志回传到任务日志。
func TestDeployViaAgent(t *testing.T) { storetest.ForEach(t, testDeployViaAgent) }

func testDeployViaAgent(t *testing.T, s store.Store) {
	ts := startStack(t, s)
	target := filepath.Join(t.TempDir(), "app")
	grp := post[model.HostGroup](t, ts, "/api/host-groups", `{"name":"edge"}`, http.StatusCreated)
	h := post[model.Host](t, ts, "/api/hosts", fmt.Sprintf(`{"name":"edge-1","mode":"agent","group_ids":[%q]}`, grp.ID), http.StatusCreated)
	if h.Mode != model.HostModeAgent || h.Address != "" {
		t.Fatalf("host = %+v", h)
	}
	if check := post[engine.HostCheck](t, ts, "/api/hosts/"+h.ID+"/test", "", http.StatusOK); check.OK || !strings.Contains(check.Error, "尚未连接") {
		t.Errorf("check before agent = %+v", check)
	}
	tok := post[map[string]string](t, ts, "/api/hosts/"+h.ID+"/agent-token", "", http.StatusOK)["token"]
	// Agent Token 不能领取普通任务，共享 Token 不能领取 Agent 任务。
	if code, _ := ts.as(sharedRunnerToken).do(t, http.MethodPost, "/api/runner/agent/claim", `{}`); code != http.StatusUnauthorized {
		t.Errorf("shared token agent claim = %d", code)
	}
	ts.startRunner(t, runner.Config{Token: tok, Agent: true, Info: &model.NodeInfo{OS: "linux", Arch: "amd64", Hostname: "edge-box", Version: "test"}})

	put(t, ts, "/api/secrets/TOKEN", `{"value":"agent-secret-1"}`, http.StatusOK)
	p := createPipeline(t, ts, `
name: edge
stages:
  - name: build
    jobs:
      - name: build
        artifacts: [dist]
        steps: [{run: "mkdir -p dist && echo v7 > dist/app.txt"}]
  - name: deploy
    jobs:
      - name: deploy
        deploy: {hosts: edge, upload: dist, target: "`+target+`"}
        steps:
          - {name: install, run: "cat app.txt > deployed.txt && echo token=$TOKEN host=$FLOW_DEPLOY_HOST && pwd"}
          - {name: second, env: {X: "1"}, run: "echo x=$X"}
`)
	d := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	d = waitRun(t, ts, d.Run.ID)
	job := jobsByName(d)["deploy"]
	log := getText(t, ts, "/api/jobs/"+job.ID+"/logs")
	if d.Run.Status != model.StatusSuccess {
		t.Fatalf("run = %s, log:\n%s", d.Run.Status, log)
	}
	if b, err := os.ReadFile(filepath.Join(target, "deployed.txt")); err != nil || string(b) != "v7\n" {
		t.Errorf("deployed = %q, %v", b, err)
	}
	for _, want := range []string{"dispatching to agent on edge-1", "[edge-1] ==> [1/2] install", "[edge-1] token=*** host=edge-1", "[edge-1] " + target, "[edge-1] x=1"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "agent-secret-1") {
		t.Error("secret leaked")
	}
	var hosts []model.Host
	getJSON(t, ts, "/api/hosts", &hosts)
	if len(hosts) != 1 || hosts[0].AgentSeenAt == nil || hosts[0].AgentInfo == nil || hosts[0].AgentInfo.Hostname != "edge-box" {
		t.Errorf("hosts = %+v", hosts)
	}
	if check := post[engine.HostCheck](t, ts, "/api/hosts/"+h.ID+"/test", "", http.StatusOK); !check.OK || !strings.Contains(check.Output, "edge-box") {
		t.Errorf("check = %+v", check)
	}

	// 部署失败：退出码回传，后续步骤不执行。
	p2 := createPipeline(t, ts, `
name: edge-fail
stages:
  - name: deploy
    jobs:
      - name: deploy
        deploy: {hosts: edge, target: "`+target+`"}
        steps:
          - {run: "echo before; exit 4"}
          - {run: "echo never"}
`)
	d = waitRun(t, ts, post[protocol.RunDetail](t, ts, "/api/pipelines/"+p2.ID+"/runs", "", http.StatusCreated).Run.ID)
	job = jobsByName(d)["deploy"]
	log = getText(t, ts, "/api/jobs/"+job.ID+"/logs")
	if d.Run.Status != model.StatusFailed || job.ExitCode != 4 || !strings.Contains(log, "[edge-1] before") || strings.Contains(log, "never") {
		t.Errorf("failed deploy = %s exit %d, log:\n%s", d.Run.Status, job.ExitCode, log)
	}

	// 删除主机同时删除其 Agent。
	if code, _ := ts.do(t, http.MethodDelete, "/api/hosts/"+h.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete host = %d", code)
	}
	var nodes []model.Runner
	getJSON(t, ts, "/api/runners", &nodes)
	for _, n := range nodes {
		if n.Kind == engine.NodeAgent {
			t.Errorf("agent runner left: %+v", n)
		}
	}
}

// 构建节点：自建节点上报环境信息、按 node:<名称> 指定节点、重置令牌；GitHub 节点在任务排队时触发工作流，-once 领取后退出。
func TestBuildNodes(t *testing.T) { storetest.ForEach(t, testBuildNodes) }

func testBuildNodes(t *testing.T, s store.Store) {
	ts := newStack(t, s)
	created := post[runnerCreated](t, ts, "/api/runners", `{"name":"mac-mini"}`, http.StatusCreated)
	if created.Kind != engine.NodeSelf || !strings.HasPrefix(created.Token, "fr_") {
		t.Fatalf("created = %+v", created)
	}
	ts.startRunner(t, runner.Config{Token: created.Token, Concurrency: 3})
	p := createPipeline(t, ts, `
name: pinned
stages:
  - name: build
    jobs:
      - {name: build, runs-on: ["node:mac-mini"], steps: [{run: "echo on-node"}]}
`)
	if d := waitRun(t, ts, post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated).Run.ID); d.Run.Status != model.StatusSuccess {
		t.Fatalf("pinned run = %s", d.Run.Status)
	}
	var nodes []model.Runner
	getJSON(t, ts, "/api/runners", &nodes)
	if len(nodes) != 1 || nodes[0].Info.Concurrency != 3 || nodes[0].Info.OS == "" || nodes[0].Info.Hostname == "" || nodes[0].LastSeenAt == nil {
		t.Errorf("nodes = %+v", nodes)
	}
	reset := post[runnerCreated](t, ts, "/api/runners/"+created.ID+"/token", "", http.StatusOK)
	if reset.Token == created.Token {
		t.Error("token not rotated")
	}
	if code, _ := ts.as(created.Token).do(t, http.MethodPost, "/api/runner/claim", `{}`); code != http.StatusUnauthorized {
		t.Errorf("old token claim = %d", code)
	}
	put(t, ts, "/api/runners/"+created.ID, `{"name":"mac-studio"}`, http.StatusOK)

	// GitHub 节点：伪造的 GitHub API 收到 workflow_dispatch 后启动 -once Runner。
	var mu sync.Mutex
	var dispatches []string
	var ghToken string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		dispatches = append(dispatches, r.URL.Path+" "+string(b))
		mu.Unlock()
		go func() {
			cfg := runner.Config{ServerURL: ts.URL, Token: ghToken, Once: true, OnceWait: 5 * time.Second, WorkDir: t.TempDir(),
				PollInterval: 50 * time.Millisecond, HeartbeatInterval: 100 * time.Millisecond, LogFlushInterval: 50 * time.Millisecond}
			runner.New(cfg).Run(context.Background())
		}()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer gh.Close()
	old := engine.GitHubAPI
	engine.GitHubAPI = gh.URL
	defer func() { engine.GitHubAPI = old }()

	if code, body := ts.do(t, http.MethodPost, "/api/runners", `{"name":"gh","kind":"github","repo":"bad"}`); code != http.StatusBadRequest {
		t.Errorf("bad repo = %d %s", code, body)
	}
	ghNode := post[runnerCreated](t, ts, "/api/runners", `{"name":"gh","kind":"github","repo":"acme/ci","token":"ghp_secret"}`, http.StatusCreated)
	ghToken = ghNode.Token
	if ghNode.Config["workflow"] != "flow-runner.yml" || ghNode.Config["ref"] != "main" || strings.Contains(getText(t, ts, "/api/runners"), "ghp_secret") {
		t.Errorf("github node = %+v", ghNode)
	}
	wf := getText(t, ts, "/api/runners/"+ghNode.ID+"/workflow")
	if !strings.Contains(wf, "workflow_dispatch") || !strings.Contains(wf, "FLOW_SERVER: "+ts.URL) || !strings.Contains(wf, "/api/runner/download/linux/amd64") {
		t.Errorf("workflow:\n%s", wf)
	}
	p2 := createPipeline(t, ts, `
name: on-github
stages:
  - name: build
    jobs:
      - {name: build, runs-on: ["node:gh"], steps: [{run: "echo on-github"}]}
`)
	d := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p2.ID+"/runs", "", http.StatusCreated)
	if n, err := ts.eng.DispatchGitHub(context.Background()); err != nil || n != 1 {
		t.Fatalf("dispatch = %d, %v", n, err)
	}
	if n, _ := ts.eng.DispatchGitHub(context.Background()); n != 0 {
		t.Errorf("redispatched immediately: %d", n)
	}
	d = waitRun(t, ts, d.Run.ID)
	log := getText(t, ts, "/api/jobs/"+d.Jobs[0].ID+"/logs")
	if d.Run.Status != model.StatusSuccess || !strings.Contains(log, "dispatched GitHub workflow flow-runner.yml on acme/ci@main") || !strings.Contains(log, "on-github") {
		t.Errorf("github run = %s, log:\n%s", d.Run.Status, log)
	}
	mu.Lock()
	defer mu.Unlock()
	var body struct {
		Ref    string            `json:"ref"`
		Inputs map[string]string `json:"inputs"`
	}
	path, raw, _ := strings.Cut(dispatches[0], " ")
	json.Unmarshal([]byte(raw), &body)
	if path != "/repos/acme/ci/actions/workflows/flow-runner.yml/dispatches" || body.Ref != "main" || body.Inputs["job"] != d.Jobs[0].ID {
		t.Errorf("dispatch = %s", dispatches[0])
	}
}

// 安装脚本嵌入 Server 地址；没有对应平台的二进制时下载返回 404。
func TestInstallScript(t *testing.T) {
	ts := newStack(t, store.NewMemory())
	sh := getText(t, ts.as(""), "/api/runner/install.sh")
	if !strings.Contains(sh, `SERVER="${FLOW_SERVER:-`+ts.URL+`}"`) || !strings.Contains(sh, "systemctl") {
		t.Errorf("install.sh:\n%s", sh)
	}
	if code, _ := ts.as("").do(t, http.MethodGet, "/api/runner/download/plan9/mips", ""); code != http.StatusNotFound {
		t.Errorf("download = %d", code)
	}
	if code, _ := ts.as("").do(t, http.MethodGet, "/api/runner/download/..%2f/x", ""); code != http.StatusBadRequest && code != http.StatusNotFound {
		t.Errorf("traversal = %d", code)
	}
}
