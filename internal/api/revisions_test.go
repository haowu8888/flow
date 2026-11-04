package api

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/store"
)

func TestPipelineRevisionsAPI(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	v1 := "name: rev\nstages: [{name: s, jobs: [{name: j, steps: [{run: echo v1}]}]}]\n"
	p := createPipeline(t, ts, v1)
	if p.Version != 1 || p.UpdatedBy != "admin" {
		t.Fatalf("created = %+v", p)
	}
	put := func(base, yaml string) (int, string) {
		path := "/api/pipelines/" + p.ID
		if base != "" {
			path += "?base_version=" + base
		}
		return ts.do(t, http.MethodPut, path, yaml)
	}

	v2 := strings.Replace(v1, "echo v1", "echo v2", 1)
	if code, body := put("1", v2); code != http.StatusOK || !strings.Contains(body, `"version":2`) {
		t.Fatalf("save v2 = %d: %s", code, body)
	}
	// 未变化的保存不产生新版本。
	if code, body := put("2", v2); code != http.StatusOK || !strings.Contains(body, `"version":2`) {
		t.Fatalf("unchanged save = %d: %s", code, body)
	}
	// 从 v1 开始编辑的人保存时提示冲突，不覆盖。
	if code, body := put("1", strings.Replace(v1, "echo v1", "echo stale", 1)); code != http.StatusConflict || !strings.Contains(body, "version 2") {
		t.Fatalf("stale save = %d: %s", code, body)
	}
	if code, _ := put("abc", v2); code != http.StatusBadRequest {
		t.Errorf("bad base_version = %d", code)
	}
	// 不带 base_version 时直接保存（API / 脚本调用）。
	v3 := strings.Replace(v1, "echo v1", "echo v3", 1)
	if code, body := put("", v3); code != http.StatusOK || !strings.Contains(body, `"version":3`) {
		t.Fatalf("save v3 = %d: %s", code, body)
	}

	var revs []model.PipelineRevision
	if err := json.Unmarshal([]byte(getText(t, ts, "/api/pipelines/"+p.ID+"/revisions")), &revs); err != nil {
		t.Fatal(err)
	}
	if len(revs) != 3 || revs[0].Version != 3 || revs[2].Version != 1 || revs[0].YAML != "" || revs[0].CreatedBy != "admin" {
		t.Fatalf("revisions = %+v", revs)
	}
	var r1 model.PipelineRevision
	if err := json.Unmarshal([]byte(getText(t, ts, "/api/pipelines/"+p.ID+"/revisions/1")), &r1); err != nil {
		t.Fatal(err)
	}
	if r1.YAML != v1 {
		t.Errorf("revision 1 yaml = %q", r1.YAML)
	}
	for _, path := range []string{"/revisions/9", "/revisions/x"} {
		if code, _ := ts.do(t, http.MethodGet, "/api/pipelines/"+p.ID+path, ""); code != http.StatusNotFound {
			t.Errorf("%s = %d", path, code)
		}
	}

	// 运行记录所用的配置版本。
	d := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	if d.Run.PipelineVersion != 3 {
		t.Errorf("run version = %d", d.Run.PipelineVersion)
	}
}

func TestRunParams(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	p := createPipeline(t, ts, `
name: params
params:
  - {name: TARGET, type: choice, options: [staging, prod], default: staging}
  - {name: VERSION, required: true}
stages:
  - name: s
    jobs:
      - name: j
        steps: [{run: 'echo "deploy $VERSION to $TARGET"'}]
`)
	runs := "/api/pipelines/" + p.ID + "/runs"
	for body, want := range map[string]int{
		``:                                       http.StatusBadRequest, // 缺少必填参数
		`{"env":{"VERSION":"1","TARGET":"dev"}}`: http.StatusBadRequest,
	} {
		if code, data := ts.do(t, http.MethodPost, runs, body); code != want {
			t.Errorf("trigger %q = %d: %s", body, code, data)
		}
	}
	d := post[protocol.RunDetail](t, ts, runs, `{"env":{"VERSION":"1.2"}}`, http.StatusCreated)
	if d.Run.Env["TARGET"] != "staging" {
		t.Errorf("run env = %v", d.Run.Env)
	}
	done := waitRun(t, ts, d.Run.ID)
	if done.Run.Status != model.StatusSuccess {
		t.Fatalf("run = %s", done.Run.Status)
	}
	if log := getText(t, ts, "/api/jobs/"+done.Jobs[0].ID+"/logs"); !strings.Contains(log, "deploy 1.2 to staging") {
		t.Errorf("log = %q", log)
	}
}

// 版本回退：以历史版本生成新版本并立即生效。
func TestRestoreRevisionAPI(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	v1 := "name: restore\nstages: [{name: s, jobs: [{name: j, steps: [{run: echo v1}]}]}]\n"
	p := createPipeline(t, ts, v1)
	v2 := strings.Replace(v1, "echo v1", "echo v2", 1)
	put(t, ts, "/api/pipelines/"+p.ID+"?base_version=1", v2, http.StatusOK)

	restore := "/api/pipelines/" + p.ID + "/revisions/1/restore"
	if code, body := ts.do(t, http.MethodPost, restore+"?base_version=1", ""); code != http.StatusConflict {
		t.Fatalf("stale restore = %d: %s", code, body)
	}
	got := post[model.Pipeline](t, ts, restore+"?base_version=2", "", http.StatusOK)
	if got.Version != 3 || got.YAML != v1 {
		t.Fatalf("restored = %+v", got)
	}
	var revs []model.PipelineRevision
	if err := json.Unmarshal([]byte(getText(t, ts, "/api/pipelines/"+p.ID+"/revisions")), &revs); err != nil {
		t.Fatal(err)
	}
	if revs[0].Version != 3 || revs[0].RestoredFrom != 1 || revs[1].RestoredFrom != 0 {
		t.Errorf("revisions = %+v", revs)
	}
	// 回退到与当前相同的内容不产生新版本。
	if again := post[model.Pipeline](t, ts, restore, "", http.StatusOK); again.Version != 3 {
		t.Errorf("repeat restore version = %d", again.Version)
	}
	if code, _ := ts.do(t, http.MethodPost, "/api/pipelines/"+p.ID+"/revisions/9/restore", ""); code != http.StatusNotFound {
		t.Errorf("missing revision = %d", code)
	}
}

// 回滚运行：用原运行的配置版本、参数与代码提交重新运行，即使之后配置和代码都已变化。
func TestRollbackRun(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(content string) string {
		os.WriteFile(filepath.Join(repo, "VERSION"), []byte(content+"\n"), 0o644)
		git("add", ".")
		git("commit", "-q", "-m", content)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q", "-b", "main")
	c1 := commit("code-1")

	ts := startStack(t, store.NewMemory())
	v1 := `
name: deploy
source: {url: "file://` + repo + `", branch: main}
params: [{name: TARGET, default: staging}]
stages:
  - name: build
    jobs: [{name: build, steps: [{run: "echo config-1 $(cat VERSION) $TARGET"}]}]
  - name: deploy
    jobs: [{name: deploy, steps: [{run: "echo deployed $(cat VERSION)"}]}]
`
	p := createPipeline(t, ts, v1)
	runs := "/api/pipelines/" + p.ID + "/runs"
	first := waitRun(t, ts, post[protocol.RunDetail](t, ts, runs, `{"env":{"TARGET":"prod"}}`, http.StatusCreated).Run.ID)
	if first.Run.Status != model.StatusSuccess || first.Run.Commit != c1 {
		t.Fatalf("first run = %s, commit %q (want %s)", first.Run.Status, first.Run.Commit, c1)
	}

	// 之后代码与配置都变了，且新运行失败。
	commit("code-2")
	put(t, ts, "/api/pipelines/"+p.ID, strings.Replace(strings.Replace(v1, "config-1", "config-2", 1), `"echo deployed`, `"exit 1; echo`, 1), http.StatusOK)
	bad := waitRun(t, ts, post[protocol.RunDetail](t, ts, runs, "", http.StatusCreated).Run.ID)
	if bad.Run.Status != model.StatusFailed || bad.Run.PipelineVersion != 2 {
		t.Fatalf("second run = %s v%d", bad.Run.Status, bad.Run.PipelineVersion)
	}
	if code, _ := ts.do(t, http.MethodPost, "/api/runs/"+bad.Run.ID+"/rollback", ""); code != http.StatusBadRequest {
		t.Errorf("rollback to failed run = %d", code)
	}

	rb := post[protocol.RunDetail](t, ts, "/api/runs/"+first.Run.ID+"/rollback", "", http.StatusCreated)
	rb = waitRun(t, ts, rb.Run.ID)
	if rb.Run.Status != model.StatusSuccess || rb.Run.Trigger != "rollback" || rb.Run.RollbackOf != first.Run.ID ||
		rb.Run.PipelineVersion != 1 || rb.Run.Commit != c1 || rb.Run.Env["TARGET"] != "prod" || rb.Run.CreatedBy != "admin" {
		t.Fatalf("rollback run = %+v", rb.Run)
	}
	jobs := jobsByName(rb)
	if log := getText(t, ts, "/api/jobs/"+jobs["build"].ID+"/logs"); !strings.Contains(log, "config-1 code-1 prod") {
		t.Errorf("build log:\n%s", log)
	}
	if log := getText(t, ts, "/api/jobs/"+jobs["deploy"].ID+"/logs"); !strings.Contains(log, "deployed code-1") {
		t.Errorf("deploy log:\n%s", log)
	}
	// 当前配置不受回滚运行影响。
	if cur := post[protocol.RunDetail](t, ts, runs, "", http.StatusCreated); cur.Run.PipelineVersion != 2 {
		t.Errorf("next run version = %d", cur.Run.PipelineVersion)
	}
}

// 列表汇总：一次返回分组、收藏与最近运行。
func TestPipelineSummaryAPI(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	a := createPipeline(t, ts, "name: a\nstages: [{name: s, jobs: [{name: j, steps: [{run: 'true'}]}]}]\n")
	b := createPipeline(t, ts, "name: b\nstages: [{name: s, jobs: [{name: j, steps: [{run: 'true'}]}]}]\n")
	for range 3 {
		waitRun(t, ts, post[protocol.RunDetail](t, ts, "/api/pipelines/"+a.ID+"/runs", "", http.StatusCreated).Run.ID)
	}
	put(t, ts, "/api/pipelines/"+a.ID+"/group", `{"group":" 后端 "}`, http.StatusOK)
	put(t, ts, "/api/pipelines/"+b.ID+"/group", `{"group":"`+strings.Repeat("x", 41)+`"}`, http.StatusBadRequest)
	put(t, ts, "/api/pipelines/"+b.ID+"/favorite", "", http.StatusNoContent)

	var rows []PipelineSummary
	if err := json.Unmarshal([]byte(getText(t, ts, "/api/pipelines/summary?runs=2")), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Name != "a" || rows[0].Group != "后端" || rows[0].Favorite ||
		len(rows[0].RecentRuns) != 2 || rows[0].RecentRuns[0].Number != 3 {
		t.Fatalf("row a = %+v", rows[0])
	}
	if !rows[1].Favorite || rows[1].RecentRuns == nil || len(rows[1].RecentRuns) != 0 {
		t.Fatalf("row b = %+v", rows[1])
	}
	if code, _ := ts.do(t, http.MethodDelete, "/api/pipelines/"+b.ID+"/favorite", ""); code != http.StatusNoContent {
		t.Errorf("unfavorite = %d", code)
	}
	if !strings.Contains(getText(t, ts, "/api/pipelines/summary"), `"favorite":false,"recent_runs":[]`) {
		t.Errorf("favorite not cleared")
	}
}
