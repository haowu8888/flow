package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/engine"
	"flow/internal/model"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

func TestWebhookEndToEnd(t *testing.T) { storetest.ForEach(t, testWebhookEndToEnd) }

func testWebhookEndToEnd(t *testing.T, s store.Store) {
	ts := startStack(t, s)
	p := createPipeline(t, ts, `
name: hooked
stages:
  - name: build
    jobs:
      - {name: show, steps: [{run: 'echo "ref=$FLOW_GIT_BRANCH sha=$FLOW_GIT_COMMIT via=$FLOW_TRIGGER target=$TARGET"'}]}
`)
	tr := post[model.Trigger](t, ts, "/api/pipelines/"+p.ID+"/triggers",
		`{"kind":"webhook","branches":["main"],"env":{"TARGET":"staging"}}`, http.StatusCreated)
	if !tr.Enabled || tr.Secret == "" {
		t.Fatalf("trigger = %+v", tr)
	}

	push := `{"ref":"refs/heads/main","before":"cafe0001","after":"deadbeef","repository":{"full_name":"acme/app"},
		"project":{"web_url":"https://gitlab.example.com/acme/app"},"user_username":"lin","total_commits_count":3,
		"commits":[{"id":"beef0001","message":"fix: a\n\nbody","author":{"name":"Lin"},"url":"https://gitlab.example.com/acme/app/-/commit/beef0001"},
			{"id":"deadbeef","message":"feat: b","author":{"name":"Zed"}}]}`
	code, body := hook(t, ts, tr.ID, http.Header{"X-Gitlab-Event": {"Push Hook"}, "X-Gitlab-Token": {tr.Secret}}, push)
	if code != http.StatusAccepted {
		t.Fatalf("webhook = %d: %s", code, body)
	}
	var res engine.WebhookResult
	if err := json.Unmarshal([]byte(body), &res); err != nil || res.Run == nil {
		t.Fatalf("webhook result = %s, %v", body, err)
	}
	detail := waitRun(t, ts, res.Run.ID)
	if detail.Run.Status != model.StatusSuccess || detail.Run.Trigger != "webhook" {
		t.Fatalf("run = %+v", detail.Run)
	}
	// 触发详情：提交列表、推送人、仓库地址
	if ti := detail.Run.TriggerInfo; ti == nil || ti.Provider != "gitlab" || ti.Pusher != "lin" || ti.Branch != "main" || ti.Before != "cafe0001" ||
		ti.RepoURL != "https://gitlab.example.com/acme/app" || ti.TotalCommits != 3 || len(ti.Commits) != 2 ||
		ti.Commits[0].Message != "fix: a" || ti.Commits[0].Author != "Lin" || ti.Commits[1].ID != "deadbeef" {
		t.Errorf("trigger info = %+v", ti)
	}
	if log := getText(t, ts, "/api/jobs/"+detail.Jobs[0].ID+"/logs"); !strings.Contains(log, "ref=main sha=deadbeef via=webhook target=staging") {
		t.Errorf("log:\n%s", log)
	}

	dev := strings.Replace(push, "main", "dev", 1)
	if code, body := hook(t, ts, tr.ID, http.Header{"X-Gitlab-Event": {"Push Hook"}, "X-Gitlab-Token": {tr.Secret}}, dev); code != http.StatusOK || !strings.Contains(body, "skipped") {
		t.Errorf("filtered = %d: %s", code, body)
	}
	if code, _ := hook(t, ts, tr.ID, http.Header{"X-Gitlab-Token": {"wrong"}}, push); code != http.StatusUnauthorized {
		t.Errorf("bad token = %d", code)
	}
	if code, _ := hook(t, ts, "missing", http.Header{"X-Gitlab-Token": {tr.Secret}}, push); code != http.StatusNotFound {
		t.Errorf("unknown trigger = %d", code)
	}

	// 管理接口：列表、修改、换密钥、删除。
	if list := getText(t, ts, "/api/pipelines/"+p.ID+"/triggers"); !strings.Contains(list, tr.ID) {
		t.Errorf("list = %s", list)
	}
	if code, body := do(t, ts, http.MethodPut, "/api/triggers/"+tr.ID, `{"kind":"webhook","enabled":false,"tags":["v*"]}`); code != http.StatusOK ||
		!strings.Contains(body, `"enabled":false`) || !strings.Contains(body, `"tags":["v*"]`) {
		t.Errorf("update = %d: %s", code, body)
	}
	rotated := post[model.Trigger](t, ts, "/api/triggers/"+tr.ID+"/rotate-secret", "", http.StatusOK)
	if rotated.Secret == tr.Secret {
		t.Error("secret not rotated")
	}
	if code, body := do(t, ts, http.MethodPost, "/api/pipelines/"+p.ID+"/triggers", `{"kind":"cron","schedule":"bad"}`); code != http.StatusBadRequest {
		t.Errorf("invalid cron = %d: %s", code, body)
	}
	if code, _ := do(t, ts, http.MethodDelete, "/api/triggers/"+tr.ID, ""); code != http.StatusNoContent {
		t.Errorf("delete = %d", code)
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/triggers/"+tr.ID, ""); code != http.StatusNotFound {
		t.Errorf("get deleted = %d", code)
	}
}

func hook(t *testing.T, ts *stack, id string, h http.Header, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/hooks/"+id, strings.NewReader(body))
	req.Header = h
	return send(t, req)
}

func (st *stack) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, st.URL+path, strings.NewReader(body))
	if st.token != "" {
		req.Header.Set("Authorization", "Bearer "+st.token)
	}
	return send(t, req)
}

func do(t *testing.T, ts *stack, method, path, body string) (int, string) {
	t.Helper()
	return ts.do(t, method, path, body)
}

func send(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// GitLab MR 事件：按 refs/merge-requests/N/head 拉取 MR 代码（不在任何分支上），重复投递复用同一 Run。
func TestMergeRequestWebhook(t *testing.T) {
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
	git("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "README"), []byte("main\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "init")
	git("checkout", "-q", "-b", "feat")
	os.WriteFile(filepath.Join(repo, "FEATURE"), []byte("from merge request\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "feature")
	sha := git("rev-parse", "HEAD")
	git("update-ref", "refs/merge-requests/7/head", sha)
	git("checkout", "-q", "main")
	git("branch", "-q", "-D", "feat") // 只能经由 MR 引用拉到

	ts := startStack(t, store.NewMemory())
	p := createPipeline(t, ts, `
name: mr
source: {url: "file://`+repo+`", branch: main}
stages:
  - name: check
    jobs:
      - {name: test, steps: [{run: 'cat FEATURE && echo "mr=$FLOW_GIT_PR_NUMBER target=$FLOW_GIT_PR_TARGET_BRANCH branch=[$FLOW_GIT_BRANCH]"'}]}
`)
	tr := post[model.Trigger](t, ts, "/api/pipelines/"+p.ID+"/triggers",
		`{"kind":"webhook","events":["pull_request"],"branches":["main"]}`, http.StatusCreated)

	mr := `{"object_kind":"merge_request","user":{"username":"erin"},"project":{"path_with_namespace":"grp/app"},
		"object_attributes":{"iid":7,"action":"open","title":"Feature","source_branch":"feat","target_branch":"main",
		"source_project_id":1,"target_project_id":1,"last_commit":{"id":"` + sha + `"}}}`
	h := http.Header{"X-Gitlab-Event": {"Merge Request Hook"}, "X-Gitlab-Token": {tr.Secret}, "Idempotency-Key": {"retry-me"}}
	code, body := hook(t, ts, tr.ID, h, mr)
	var res engine.WebhookResult
	if code != http.StatusAccepted || json.Unmarshal([]byte(body), &res) != nil || res.Run == nil {
		t.Fatalf("mr webhook = %d: %s", code, body)
	}
	d := waitRun(t, ts, res.Run.ID)
	log := getText(t, ts, "/api/jobs/"+d.Jobs[0].ID+"/logs")
	if d.Run.Status != model.StatusSuccess || !strings.Contains(log, "from merge request") || !strings.Contains(log, "mr=7 target=main branch=[]") {
		t.Errorf("run = %s, log:\n%s", d.Run.Status, log)
	}

	// 平台重试同一投递：200，返回原来的 Run。
	code, body = hook(t, ts, tr.ID, h, mr)
	var dup engine.WebhookResult
	if code != http.StatusOK || json.Unmarshal([]byte(body), &dup) != nil || !dup.Duplicate || dup.Run == nil || dup.Run.ID != res.Run.ID {
		t.Errorf("redelivery = %d: %s", code, body)
	}
	// 推送事件未订阅。
	push := `{"ref":"refs/heads/main","after":"` + sha + `"}`
	if code, body := hook(t, ts, tr.ID, http.Header{"X-Gitlab-Event": {"Push Hook"}, "X-Gitlab-Token": {tr.Secret}}, push); code != http.StatusOK ||
		!strings.Contains(body, "push events are not enabled") {
		t.Errorf("push on pr-only trigger = %d: %s", code, body)
	}
}
