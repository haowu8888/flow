package api

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/runner"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

func jobsByName(d protocol.RunDetail) map[string]*model.Job {
	m := map[string]*model.Job{}
	for _, j := range d.Jobs {
		m[j.Name] = j
	}
	return m
}

func put(t *testing.T, ts *stack, path, body string, wantCode int) string {
	t.Helper()
	code, data := ts.do(t, http.MethodPut, path, body)
	if code != wantCode {
		t.Fatalf("PUT %s = %d: %s", path, code, data)
	}
	return data
}

// 密钥：注入为环境变量，流水线级覆盖租户级，日志中按值脱敏，接口不返回值。
func TestSecretsInjectedAndMasked(t *testing.T) { storetest.ForEach(t, testSecretsInjectedAndMasked) }

func testSecretsInjectedAndMasked(t *testing.T, s store.Store) {
	ts := startStack(t, s)
	p := createPipeline(t, ts, `
name: secrets
env: {API_TOKEN: from-yaml}
stages:
  - name: s
    jobs:
      - name: show
        steps:
          - run: |
              echo "token=$API_TOKEN db=$DB_PASS"
              test "$API_TOKEN" = "pipeline-token-123"
              test "$DB_PASS" = "tenant-db-pass"
`)
	put(t, ts, "/api/secrets/API_TOKEN", `{"value":"tenant-token-000"}`, http.StatusOK)
	put(t, ts, "/api/secrets/DB_PASS", `{"value":"tenant-db-pass"}`, http.StatusOK)
	put(t, ts, "/api/pipelines/"+p.ID+"/secrets/API_TOKEN", `{"value":"pipeline-token-123"}`, http.StatusOK)
	put(t, ts, "/api/secrets/FLOW_X", `{"value":"x"}`, http.StatusBadRequest)

	if list := getText(t, ts, "/api/secrets"); strings.Contains(list, "tenant-db-pass") || !strings.Contains(list, "DB_PASS") {
		t.Errorf("secret list leaks value or misses name: %s", list)
	}
	detail := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	detail = waitRun(t, ts, detail.Run.ID)
	log := getText(t, ts, "/api/jobs/"+detail.Jobs[0].ID+"/logs")
	if detail.Run.Status != model.StatusSuccess {
		t.Fatalf("run = %s, log:\n%s", detail.Run.Status, log)
	}
	if !strings.Contains(log, "token=*** db=***") || strings.Contains(log, "pipeline-token-123") || strings.Contains(log, "tenant-db-pass") {
		t.Errorf("log not masked:\n%s", log)
	}
}

// 注册 Runner：独立 Token、按标签领取、只领取所属租户的 Job，标识以注册 ID 为准。
func TestRegisteredRunnerLabels(t *testing.T) {
	ts := newStack(t, store.NewMemory())
	var reg struct {
		model.Runner
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(postRaw(t, ts, "/api/runners", `{"name":"builder"}`, http.StatusCreated)), &reg); err != nil || reg.Token == "" {
		t.Fatalf("register = %+v, %v", reg, err)
	}
	ts.startRunner(t, runner.Config{Token: reg.Token, RunnerID: "spoofed", Labels: []string{"linux", "docker"}})

	p := createPipeline(t, ts, `
name: labels
stages:
  - name: s
    jobs:
      - {name: any, steps: [{run: "true"}]}
      - {name: linux, runs-on: linux, steps: [{run: "true"}]}
      - {name: gpu, runs-on: [linux, gpu], steps: [{run: "true"}]}
`)
	detail := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := json.Unmarshal([]byte(getText(t, ts, "/api/runs/"+detail.Run.ID)), &detail); err != nil {
			t.Fatal(err)
		}
		jobs := jobsByName(detail)
		if jobs["any"].Status == model.StatusSuccess && jobs["linux"].Status == model.StatusSuccess {
			if jobs["gpu"].Status != model.StatusQueued {
				t.Errorf("gpu job = %s, want queued", jobs["gpu"].Status)
			}
			if jobs["linux"].RunnerID != reg.ID {
				t.Errorf("runner id = %q, want registered %q", jobs["linux"].RunnerID, reg.ID)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("jobs not run: any=%s linux=%s", jobs["any"].Status, jobs["linux"].Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if list := getText(t, ts, "/api/runners"); !strings.Contains(list, `"labels":["linux","docker"]`) || strings.Contains(list, reg.Token) {
		t.Errorf("runner list = %s", list)
	}
	post[protocol.RunDetail](t, ts, "/api/runs/"+detail.Run.ID+"/cancel", "", http.StatusOK)

	if code, _ := ts.do(t, http.MethodDelete, "/api/runners/"+reg.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete runner = %d", code)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/runner/claim", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+reg.Token)
	if code, _ := send(t, req); code != http.StatusUnauthorized {
		t.Errorf("deleted runner claim = %d", code)
	}
}

func postRaw(t *testing.T, ts *stack, path, body string, wantCode int) string {
	t.Helper()
	code, data := ts.do(t, http.MethodPost, path, body)
	if code != wantCode {
		t.Fatalf("POST %s = %d: %s", path, code, data)
	}
	return data
}

// 产物：上游上传，下游（含隔着其他 Stage 的间接下游）自动解压，界面可下载。
func TestArtifactsFlowDownstream(t *testing.T) { storetest.ForEach(t, testArtifactsFlowDownstream) }

func testArtifactsFlowDownstream(t *testing.T, s store.Store) {
	ts := startStack(t, s)
	p := createPipeline(t, ts, `
name: artifacts
stages:
  - name: build
    jobs:
      - name: compile
        artifacts: [dist, "*.log"]
        steps: [{run: "mkdir -p dist/sub && echo v1 > dist/sub/app.txt && echo done > build.log && echo skip > other.txt"}]
  - name: lint
    jobs: [{name: lint, steps: [{run: "echo lint"}]}]
  - name: test
    jobs:
      - name: check
        steps: [{run: "cat dist/sub/app.txt build.log && test ! -e other.txt"}]
`)
	detail := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	detail = waitRun(t, ts, detail.Run.ID)
	jobs := jobsByName(detail)
	if detail.Run.Status != model.StatusSuccess {
		t.Fatalf("run = %s\ncompile:\n%s\ncheck:\n%s", detail.Run.Status,
			getText(t, ts, "/api/jobs/"+jobs["compile"].ID+"/logs"), getText(t, ts, "/api/jobs/"+jobs["check"].ID+"/logs"))
	}
	if log := getText(t, ts, "/api/jobs/"+jobs["check"].ID+"/logs"); !strings.Contains(log, "v1\ndone") {
		t.Errorf("check log:\n%s", log)
	}

	var arts []model.Artifact
	json.Unmarshal([]byte(getText(t, ts, "/api/runs/"+detail.Run.ID+"/artifacts")), &arts)
	if len(arts) != 1 || arts[0].JobName != "compile" || arts[0].Size == 0 {
		t.Fatalf("artifacts = %+v", arts)
	}
	names := tarNames(t, getText(t, ts, "/api/artifacts/"+arts[0].ID+"/download"))
	if strings.Join(names, ",") != "build.log,dist/,dist/sub/,dist/sub/app.txt" {
		t.Errorf("archive entries = %v", names)
	}
}

func tarNames(t *testing.T, data string) []string {
	t.Helper()
	gz, err := gzip.NewReader(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

// 人工卡点：等待审批，只读成员不能审批，开发者通过后继续执行下游。
func TestApprovalGate(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	postRaw(t, ts, "/api/members", `{"username":"vic","password":"viewer-pass","role":"viewer"}`, http.StatusCreated)
	viewer := ts.as(ts.login(t, "vic", "viewer-pass"))

	p := createPipeline(t, ts, `
name: gated
stages:
  - name: build
    jobs: [{name: build, steps: [{run: echo build}]}]
  - name: gate
    jobs: [{name: release-approval, type: approval}]
  - name: deploy
    jobs: [{name: deploy, steps: [{run: echo deploy}]}]
`)
	detail := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	var gate *model.Job
	deadline := time.Now().Add(5 * time.Second)
	for gate == nil || gate.Status != model.StatusWaiting {
		if time.Now().After(deadline) {
			t.Fatalf("gate = %+v", gate)
		}
		time.Sleep(50 * time.Millisecond)
		json.Unmarshal([]byte(getText(t, ts, "/api/runs/"+detail.Run.ID)), &detail)
		gate = jobsByName(detail)["release-approval"]
	}
	if code, _ := viewer.do(t, http.MethodPost, "/api/jobs/"+gate.ID+"/approve", `{"approve":true}`); code != http.StatusForbidden {
		t.Errorf("viewer approve = %d", code)
	}
	j := post[model.Job](t, ts, "/api/jobs/"+gate.ID+"/approve", `{"approve":true,"comment":"ship it"}`, http.StatusOK)
	if j.Status != model.StatusSuccess || j.DecidedBy != "admin" {
		t.Errorf("approved job = %+v", j)
	}
	detail = waitRun(t, ts, detail.Run.ID)
	if detail.Run.Status != model.StatusSuccess || jobsByName(detail)["deploy"].Status != model.StatusSuccess {
		t.Errorf("run = %s", detail.Run.Status)
	}
}

// 失败自动重试、整次运行重试、超时。
func TestRetryAndTimeout(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	mark := filepath.Join(t.TempDir(), "mark")
	p := createPipeline(t, ts, `
name: flaky
env: {MARK: `+mark+`}
stages:
  - name: s
    jobs:
      - name: flaky
        retry: 1
        steps: [{run: 'if [ -f "$MARK" ]; then echo second; else touch "$MARK"; echo first; exit 1; fi'}]
      - name: slow
        timeout: 1s
        steps: [{run: "sleep 30"}]
`)
	detail := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	detail = waitRun(t, ts, detail.Run.ID)
	jobs := jobsByName(detail)
	if j := jobs["flaky"]; j.Status != model.StatusSuccess || j.Attempt != 2 {
		t.Errorf("flaky = %s attempt %d", j.Status, j.Attempt)
	}
	if j := jobs["slow"]; j.Status != model.StatusFailed || !strings.Contains(j.Error, "timed out after 1s") {
		t.Errorf("slow = %s %q", j.Status, j.Error)
	}

	// 重试整次运行：成功的 Job 保留结果，失败的按创建时的定义快照重新执行（slow 仍会超时）。
	retried := post[protocol.RunDetail](t, ts, "/api/runs/"+detail.Run.ID+"/retry", "", http.StatusOK)
	retried = waitRun(t, ts, retried.Run.ID)
	jobs = jobsByName(retried)
	if jobs["flaky"].Attempt != 2 || jobs["slow"].Attempt != 2 || jobs["slow"].Status != model.StatusFailed {
		t.Errorf("after retry: flaky attempt %d, slow %s attempt %d", jobs["flaky"].Attempt, jobs["slow"].Status, jobs["slow"].Attempt)
	}
	if code, _ := ts.do(t, http.MethodPost, "/api/runs/"+detail.Run.ID+"/cancel", ""); code != http.StatusOK {
		t.Errorf("cancel finished run = %d", code)
	}
}

// 缓存：首次未命中，成功后保存；第二次运行恢复。
func TestRunnerCache(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	p := createPipeline(t, ts, `
name: cached
stages:
  - name: s
    jobs:
      - name: deps
        cache: {key: "deps-$FLOW_JOB", paths: [vendor]}
        steps: [{run: 'if [ -f vendor/lib ]; then echo HIT; else mkdir -p vendor && echo x > vendor/lib && echo MISS; fi'}]
`)
	var logs []string
	for range 2 {
		d := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
		d = waitRun(t, ts, d.Run.ID)
		logs = append(logs, getText(t, ts, "/api/jobs/"+d.Jobs[0].ID+"/logs"))
	}
	if !strings.Contains(logs[0], "cache miss: deps-deps") || !strings.Contains(logs[0], "MISS") || !strings.Contains(logs[0], "cache saved") {
		t.Errorf("first run:\n%s", logs[0])
	}
	if !strings.Contains(logs[1], "cache restored: deps-deps") || !strings.Contains(logs[1], "HIT") {
		t.Errorf("second run:\n%s", logs[1])
	}
}

// 代码源：Runner 浅克隆指定分支并检出。
func TestCheckoutSource(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "README"), []byte("hello from repo\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "init")

	ts := startStack(t, store.NewMemory())
	p := createPipeline(t, ts, `
name: src
source: {url: "file://`+repo+`", branch: main}
stages:
  - name: s
    jobs:
      - {name: read, steps: [{run: "cat README && git log --oneline | wc -l"}]}
      - {name: no-src, checkout: false, steps: [{run: "test ! -e README"}]}
`)
	d := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	d = waitRun(t, ts, d.Run.ID)
	log := getText(t, ts, "/api/jobs/"+jobsByName(d)["read"].ID+"/logs")
	if d.Run.Status != model.StatusSuccess || !strings.Contains(log, "hello from repo") || !strings.Contains(log, "HEAD is") {
		t.Errorf("run = %s, log:\n%s", d.Run.Status, log)
	}
}

// 鉴权与隔离：未登录 401、Cookie 写请求须带 CSRF 头、只读角色不能写、其他租户的资源不可见。
func TestAuthAndTenantIsolation(t *testing.T) {
	ts := newStack(t, store.NewMemory())
	p := createPipeline(t, ts, "name: mine\nstages: [{name: s, jobs: [{name: j, steps: [{run: 'true'}]}]}]\n")

	if code, _ := ts.as("").do(t, http.MethodGet, "/api/pipelines", ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d", code)
	}
	if code, _ := ts.as("").do(t, http.MethodPost, "/api/auth/login", `{"username":"admin","password":"wrong-password"}`); code != http.StatusUnauthorized {
		t.Errorf("bad password = %d", code)
	}

	cookieReq := func(method, path string, csrf bool) int {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(""))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: ts.token})
		if csrf {
			req.Header.Set(csrfHeader, "1")
		}
		code, _ := send(t, req)
		return code
	}
	if code := cookieReq(http.MethodGet, "/api/pipelines", false); code != http.StatusOK {
		t.Errorf("cookie GET = %d", code)
	}
	if code := cookieReq(http.MethodPost, "/api/pipelines/"+p.ID+"/runs", false); code != http.StatusForbidden {
		t.Errorf("cookie POST without csrf header = %d", code)
	}
	if code := cookieReq(http.MethodPost, "/api/pipelines/"+p.ID+"/runs", true); code != http.StatusCreated {
		t.Errorf("cookie POST with csrf header = %d", code)
	}

	postRaw(t, ts, "/api/members", `{"username":"vic","password":"viewer-pass","role":"viewer"}`, http.StatusCreated)
	viewer := ts.as(ts.login(t, "vic", "viewer-pass"))
	if code, _ := viewer.do(t, http.MethodGet, "/api/pipelines/"+p.ID, ""); code != http.StatusNotFound {
		t.Errorf("viewer read private = %d", code)
	}
	put(t, ts, "/api/pipelines/"+p.ID+"/public", `{"public":true}`, http.StatusOK)
	if code, _ := viewer.do(t, http.MethodGet, "/api/pipelines/"+p.ID, ""); code != http.StatusOK {
		t.Errorf("viewer read = %d", code)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/pipelines/" + p.ID + "/runs"},
		{http.MethodPut, "/api/secrets/X"},
		{http.MethodPost, "/api/runners"},
		{http.MethodPost, "/api/members"},
	} {
		if code, _ := viewer.do(t, c.method, c.path, `{}`); code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d", c.method, c.path, code)
		}
	}

	// 系统管理员新建租户并加入另一个用户；该用户看不到 default 租户的流水线。
	postRaw(t, ts, "/api/tenants", `{"id":"team-b","name":"B 组"}`, http.StatusCreated)
	if code, _ := viewer.do(t, http.MethodPost, "/api/tenants", `{"id":"team-c"}`); code != http.StatusForbidden {
		t.Errorf("non-admin create tenant = %d", code)
	}
	adminB := ts.as(ts.token)
	postRaw(t, adminB, "/api/me/tenant", `{"id":"team-b"}`, http.StatusOK)
	postRaw(t, adminB, "/api/members", `{"username":"bob","password":"bob-password","role":"admin"}`, http.StatusCreated)
	bob := ts.as(ts.login(t, "bob", "bob-password"))
	if code, _ := bob.do(t, http.MethodGet, "/api/pipelines/"+p.ID, ""); code != http.StatusNotFound {
		t.Errorf("cross-tenant read = %d", code)
	}
	if list := getText(t, bob, "/api/pipelines"); strings.Contains(list, p.ID) {
		t.Errorf("cross-tenant list = %s", list)
	}
	if code, _ := bob.do(t, http.MethodPost, "/api/pipelines/"+p.ID+"/runs", ""); code != http.StatusNotFound {
		t.Errorf("cross-tenant trigger = %d", code)
	}
	postRaw(t, adminB, "/api/me/tenant", `{"id":"default"}`, http.StatusOK)

	// API 令牌：可用于脚本触发，记录为 api 触发。
	var tok struct {
		Token string `json:"token"`
	}
	json.Unmarshal([]byte(postRaw(t, ts, "/api/me/tokens", `{"name":"ci"}`, http.StatusCreated)), &tok)
	d := post[protocol.RunDetail](t, ts.as(tok.Token), "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	if d.Run.Trigger != "api" || d.Run.CreatedBy != "admin" {
		t.Errorf("api-triggered run = %+v", d.Run)
	}

	postRaw(t, ts, "/api/auth/logout", "", http.StatusNoContent)
	if code, _ := ts.do(t, http.MethodGet, "/api/me", ""); code != http.StatusUnauthorized {
		t.Errorf("after logout = %d", code)
	}
}
