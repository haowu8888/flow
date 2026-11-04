package engine

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"flow/internal/blob"
	"flow/internal/model"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

func byName(t *testing.T, e *Engine, runID string) map[string]*model.Job {
	t.Helper()
	jobs, err := e.Jobs(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*model.Job{}
	for _, j := range jobs {
		out[j.Name] = j
	}
	return out
}

func runStatus(t *testing.T, e *Engine, runID string) model.Status {
	t.Helper()
	r, err := e.Run(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	return r.Status
}

const approvalYAML = `
name: gated
stages:
  - name: build
    jobs: [{name: build, steps: [{run: "true"}]}]
  - name: gate
    jobs: [{name: approve, type: approval, approvers: [alice]}]
  - name: deploy
    jobs: [{name: deploy, steps: [{run: "true"}]}]
`

func TestApprovalGate(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, run, _ := setup(t, s, approvalYAML)
		must(t, e.Complete(ctx, claim(t, e, "build").ID, "r1", model.StatusSuccess, 0, ""))
		jobs := byName(t, e, run.ID)
		if jobs["approve"].Status != model.StatusWaiting || jobs["approve"].StartedAt == nil {
			t.Fatalf("approve = %+v", jobs["approve"])
		}
		if j, _ := claimJob(e, "r1"); j != nil {
			t.Fatalf("runner claimed %s while gate waiting", j.Name)
		}
		if _, err := e.Approve(ctx, jobs["approve"].ID, "bob", false, true, ""); !errors.Is(err, ErrForbidden) {
			t.Fatalf("bob approve = %v", err)
		}
		j, err := e.Approve(ctx, jobs["approve"].ID, "alice", false, true, "LGTM")
		if err != nil || j.Status != model.StatusSuccess || j.DecidedBy != "alice" {
			t.Fatalf("alice approve = %+v, %v", j, err)
		}
		if _, err := e.Approve(ctx, jobs["approve"].ID, "alice", false, true, ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("double approve = %v", err)
		}
		must(t, e.Complete(ctx, claim(t, e, "deploy").ID, "r1", model.StatusSuccess, 0, ""))
		if st := runStatus(t, e, run.ID); st != model.StatusSuccess {
			t.Fatalf("run = %s", st)
		}
		if log, _ := e.ReadLog(ctx, jobs["approve"].ID, 0); !strings.Contains(string(log), "approved by alice: LGTM") {
			t.Errorf("approval log = %q", log)
		}

		// 驳回：下游跳过，运行失败；管理员不受审批人名单限制。
		run2, err := e.TriggerRun(ctx, run.PipelineID, "manual", "bob", nil)
		must(t, err)
		must(t, e.Complete(ctx, claim(t, e, "build").ID, "r1", model.StatusSuccess, 0, ""))
		gate := byName(t, e, run2.ID)["approve"]
		if _, err := e.Approve(ctx, gate.ID, "root", true, false, "not today"); err != nil {
			t.Fatal(err)
		}
		jobs = byName(t, e, run2.ID)
		if jobs["approve"].Status != model.StatusFailed || jobs["approve"].Error != "rejected by root: not today" || jobs["deploy"].Status != model.StatusSkipped {
			t.Fatalf("after reject: %+v / %+v", jobs["approve"], jobs["deploy"])
		}
		if r, _ := e.Run(ctx, run2.ID); r.Status != model.StatusFailed || r.CreatedBy != "bob" {
			t.Fatalf("run2 = %+v", r)
		}

		// 取消会结束等待中的卡点。
		run3, _ := e.TriggerRun(ctx, run.PipelineID, "manual", "", nil)
		must(t, e.Complete(ctx, claim(t, e, "build").ID, "r1", model.StatusSuccess, 0, ""))
		if _, err := e.CancelRun(ctx, run3.ID); err != nil {
			t.Fatal(err)
		}
		if st := byName(t, e, run3.ID)["approve"].Status; st != model.StatusCanceled {
			t.Fatalf("gate after cancel = %s", st)
		}
	})
}

const whenYAML = `
name: conditions
stages:
  - name: build
    jobs: [{name: build, steps: [{run: "true"}]}]
  - name: after
    jobs:
      - {name: deploy, steps: [{run: "true"}]}
      - {name: rollback, when: on_failure, steps: [{run: "true"}]}
      - {name: cleanup, when: always, steps: [{run: "true"}]}
`

func TestWhenConditions(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, run, clock := setup(t, s, whenYAML)
		must(t, e.Complete(ctx, claim(t, e, "build").ID, "r1", model.StatusFailed, 1, "boom"))
		jobs := byName(t, e, run.ID)
		if jobs["deploy"].Status != model.StatusSkipped || jobs["rollback"].Status != model.StatusQueued || jobs["cleanup"].Status != model.StatusQueued {
			t.Fatalf("after failure: deploy=%s rollback=%s cleanup=%s", jobs["deploy"].Status, jobs["rollback"].Status, jobs["cleanup"].Status)
		}

		*clock = clock.Add(time.Second) // 让 run2 的 Job 排在 run1 之后入队
		run2, _ := e.TriggerRun(ctx, run.PipelineID, "manual", "", nil)
		for _, j := range []string{"rollback", "cleanup"} {
			must(t, e.Complete(ctx, claim(t, e, j).ID, "r1", model.StatusSuccess, 0, ""))
		}
		if st := runStatus(t, e, run.ID); st != model.StatusFailed {
			t.Fatalf("run1 = %s", st)
		}
		must(t, e.Complete(ctx, claim(t, e, "build").ID, "r1", model.StatusSuccess, 0, ""))
		jobs = byName(t, e, run2.ID)
		if jobs["deploy"].Status != model.StatusQueued || jobs["rollback"].Status != model.StatusSkipped || jobs["cleanup"].Status != model.StatusQueued {
			t.Fatalf("after success: deploy=%s rollback=%s cleanup=%s", jobs["deploy"].Status, jobs["rollback"].Status, jobs["cleanup"].Status)
		}
	})
}

const retryYAML = `
name: flaky
stages:
  - name: s
    jobs: [{name: flaky, retry: 1, steps: [{run: "true"}]}, {name: solid, steps: [{run: "true"}]}]
  - name: t
    jobs: [{name: after, steps: [{run: "true"}]}]
`

func TestAutoRetryAndRetryRun(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, run, clock := setup(t, s, retryYAML)
		flaky := claim(t, e, "flaky")
		solid := claim(t, e, "solid")
		must(t, e.Complete(ctx, solid.ID, "r1", model.StatusSuccess, 0, ""))
		must(t, e.Complete(ctx, flaky.ID, "r1", model.StatusFailed, 2, "exit code 2"))
		j := byName(t, e, run.ID)["flaky"]
		if j.Status != model.StatusQueued || j.Attempt != 2 || j.Error != "" || j.RunnerID != "" {
			t.Fatalf("after first failure = %+v", j)
		}
		if log, _ := e.ReadLog(ctx, j.ID, 0); !strings.Contains(string(log), "retrying: attempt 2 of 2") {
			t.Errorf("retry log = %q", log)
		}
		// 第二次：Runner 失联、租约过期，不再重试。
		claim(t, e, "flaky")
		*clock = clock.Add(time.Minute)
		if n, err := e.ReapExpired(ctx); n != 1 || err != nil {
			t.Fatalf("reap = %d, %v", n, err)
		}
		jobs := byName(t, e, run.ID)
		if jobs["flaky"].Status != model.StatusFailed || jobs["after"].Status != model.StatusSkipped || runStatus(t, e, run.ID) != model.StatusFailed {
			t.Fatalf("after exhausting retries: %s / %s", jobs["flaky"].Status, jobs["after"].Status)
		}

		// 手动重试：成功的 solid 保留，失败与跳过的重新调度。
		if _, err := e.RetryRun(ctx, run.ID, "alice"); err != nil {
			t.Fatal(err)
		}
		jobs = byName(t, e, run.ID)
		if jobs["solid"].Status != model.StatusSuccess || jobs["flaky"].Status != model.StatusQueued || jobs["flaky"].Attempt != 3 || jobs["after"].Status != model.StatusPending {
			t.Fatalf("after RetryRun: %+v", jobs)
		}
		if st := runStatus(t, e, run.ID); st != model.StatusRunning {
			t.Fatalf("run after retry = %s", st)
		}
		must(t, e.Complete(ctx, claim(t, e, "flaky").ID, "r1", model.StatusSuccess, 0, ""))
		must(t, e.Complete(ctx, claim(t, e, "after").ID, "r1", model.StatusSuccess, 0, ""))
		if st := runStatus(t, e, run.ID); st != model.StatusSuccess {
			t.Fatalf("run after retry = %s", st)
		}
		if _, err := e.RetryRun(ctx, run.ID, "alice"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("retry successful run = %v", err)
		}
	})
}

const labelsYAML = `
name: labels
source: {url: "https://git.example.com/acme/app.git", branch: main, token_secret: GIT_TOKEN}
stages:
  - name: s
    jobs:
      - {name: gpu, runs-on: [linux, gpu], matrix: {CUDA: ["11", "12"]}, steps: [{run: "true"}]}
      - {name: docs, checkout: false, steps: [{run: "true"}]}
`

func TestLabelsMatrixSecretsCheckout(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		e, p, _ := newPipeline(t, s)
		p, err := e.UpdatePipeline(ctx, p.ID, labelsYAML, "", 0)
		must(t, err)
		_, err = e.PutSecret(ctx, DefaultTenant, "", "GIT_TOKEN", "tenant-token")
		must(t, err)
		_, err = e.PutSecret(ctx, DefaultTenant, p.ID, "GIT_TOKEN", "pipeline-token")
		must(t, err)
		_, err = e.PutSecret(ctx, DefaultTenant, "", "SHARED", "shared-value")
		must(t, err)
		for _, bad := range []string{"FLOW_X", "1BAD", ""} {
			if _, err := e.PutSecret(ctx, DefaultTenant, "", bad, "v"); !errors.Is(err, ErrInvalid) {
				t.Errorf("secret name %q = %v", bad, err)
			}
		}
		if list, _ := e.Secrets(ctx, DefaultTenant, ""); len(list) != 2 || list[0].Value != nil {
			t.Fatalf("secrets list = %+v", list)
		}

		run, err := e.TriggerRun(ctx, p.ID, "webhook", "", map[string]string{"FLOW_GIT_BRANCH": "dev", "FLOW_GIT_COMMIT": "abc"})
		must(t, err)
		// 无标签 Runner 只能领到 docs。
		a, err := e.Claim(ctx, RunnerInfo{ID: "plain"})
		if err != nil || a == nil || a.Job.Name != "docs" || a.Job.Checkout != nil {
			t.Fatalf("plain claim = %+v, %v", a, err)
		}
		if a.Secrets["GIT_TOKEN"] != "pipeline-token" || a.Secrets["SHARED"] != "shared-value" {
			t.Fatalf("secrets = %v", a.Secrets)
		}
		if a, _ := e.Claim(ctx, RunnerInfo{ID: "plain"}); a != nil {
			t.Fatalf("plain runner claimed %s", a.Job.Name)
		}
		a, err = e.Claim(ctx, RunnerInfo{ID: "big", Labels: []string{"gpu", "linux", "x86"}})
		if err != nil || a == nil || a.Job.Name != "gpu (11)" || a.Job.Env["CUDA"] != "11" {
			t.Fatalf("gpu claim = %+v, %v", a, err)
		}
		co := a.Job.Checkout
		if co == nil || co.Ref != "dev" || co.Commit != "abc" || co.Depth != 50 || co.TokenSecret != "GIT_TOKEN" {
			t.Fatalf("checkout = %+v", co)
		}
		for _, j := range byName(t, e, run.ID) {
			for k, v := range j.Env {
				if strings.Contains(v, "token") {
					t.Errorf("secret leaked into job env %s=%s", k, v)
				}
			}
		}
	})
}

func TestArtifactsFlowDownstream(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		blobs := blob.NewMemory()
		e, run, _ := setup(t, s, dagYAML)
		e.blobs = blobs
		a := claim(t, e, "a")
		if _, err := e.UploadArtifact(ctx, a.ID, "intruder", strings.NewReader("x")); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("foreign upload = %v", err)
		}
		art, err := e.UploadArtifact(ctx, a.ID, "r1", strings.NewReader("first"))
		must(t, err)
		art2, err := e.UploadArtifact(ctx, a.ID, "r1", strings.NewReader("second!"))
		if err != nil || art2.Size != 7 {
			t.Fatalf("overwrite = %+v, %v", art2, err)
		}
		if _, err := blobs.Open(ctx, art.BlobKey); err == nil {
			t.Error("old blob not deleted")
		}
		must(t, e.Complete(ctx, a.ID, "r1", model.StatusSuccess, 0, ""))
		asg, err := e.Claim(ctx, RunnerInfo{ID: "r1"})
		if err != nil || asg.Job.Name != "b" || len(asg.Artifacts) != 0 {
			t.Fatalf("b assignment = %+v, %v", asg, err)
		}
		asg, err = e.Claim(ctx, RunnerInfo{ID: "r1"})
		if err != nil || asg.Job.Name != "c" || len(asg.Artifacts) != 1 || asg.Artifacts[0].JobName != "a" {
			t.Fatalf("c assignment = %+v, %v", asg, err)
		}
		rc, err := e.OpenArtifact(ctx, asg.Artifacts[0])
		must(t, err)
		data, _ := io.ReadAll(rc)
		rc.Close()
		if string(data) != "second!" {
			t.Errorf("artifact = %q", data)
		}
		must(t, e.DeletePipeline(ctx, run.PipelineID))
		if _, err := blobs.Open(ctx, art2.BlobKey); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("blob survived pipeline delete: %v", err)
		}
	})
}

func TestNotifications(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		var mu sync.Mutex
		var got []map[string]any
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			got = append(got, body)
			paths = append(paths, r.URL.RequestURI())
			mu.Unlock()
			io.WriteString(w, `{"errcode":0}`)
		}))
		defer srv.Close()

		e, p, _ := newPipeline(t, s)
		e.publicURL = "https://flow.example.com/"
		_, err := e.UpdatePipeline(ctx, p.ID, `
name: notified
notify:
  - {type: webhook, url: "`+srv.URL+`/hook?token=${HOOK_TOKEN}", on: [failed, success]}
  - {type: dingtalk, url: "`+srv.URL+`/ding", on: [failed]}
  - {type: wecom, url: "`+srv.URL+`/missing/${NOPE}", on: [failed]}
stages: [{name: s, jobs: [{name: unit, steps: [{run: "true"}]}]}]
`, "", 0)
		must(t, err)
		_, err = e.PutSecret(ctx, DefaultTenant, p.ID, "HOOK_TOKEN", "t0ken")
		must(t, err)

		run, err := e.TriggerRun(ctx, p.ID, "manual", "alice", nil)
		must(t, err)
		if n, _ := e.DeliverNotifications(ctx); n != 0 {
			t.Fatal("notified before run finished")
		}
		must(t, e.Complete(ctx, claim(t, e, "unit").ID, "r1", model.StatusFailed, 1, "boom"))
		if n, err := e.DeliverNotifications(ctx); n != 1 || err != nil {
			t.Fatalf("deliver = %d, %v", n, err)
		}
		if n, _ := e.DeliverNotifications(ctx); n != 0 {
			t.Fatal("delivered twice")
		}
		mu.Lock()
		if len(got) != 2 || paths[0] != "/hook?token=t0ken" || got[0]["status"] != "failed" || got[0]["url"] != "https://flow.example.com/#/runs/"+run.ID {
			t.Fatalf("deliveries = %v %v", paths, got)
		}
		if failed := got[0]["failed_jobs"].([]any); failed[0] != "unit" || got[0]["trigger"] != "手动（alice）" {
			t.Errorf("webhook body = %v", got[0])
		}
		got, paths = nil, nil
		mu.Unlock()

		// 成功只匹配 webhook 一个渠道。
		_, err = e.TriggerRun(ctx, p.ID, "manual", "", nil)
		must(t, err)
		must(t, e.Complete(ctx, claim(t, e, "unit").ID, "r1", model.StatusSuccess, 0, ""))
		e.DeliverNotifications(ctx)
		mu.Lock()
		defer mu.Unlock()
		if len(got) != 1 || got[0]["status"] != "success" {
			t.Fatalf("success deliveries = %v", got)
		}
	})
}
