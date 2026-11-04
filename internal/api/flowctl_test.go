package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"flow/internal/engine"
	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

// 步骤进度：Runner 逐步上报开始 / 结束，失败步骤之后的步骤保持未执行。
func TestStepProgress(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	p := createPipeline(t, ts, `
name: steps
stages:
  - name: s
    jobs:
      - name: j
        steps:
          - {name: ok, run: "sleep 0.2"}
          - {name: boom, run: "exit 3"}
          - {name: never, run: "true"}
`)
	d := waitRun(t, ts, post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated).Run.ID)
	st := d.Jobs[0].StepStates
	if len(st) != 3 || st[0].Status != model.StatusSuccess || st[1].Status != model.StatusFailed || st[1].ExitCode != 3 ||
		st[2].Status != model.StatusPending || st[2].StartedAt != nil {
		t.Fatalf("step states = %+v", st)
	}
	if st[0].StartedAt == nil || st[0].FinishedAt == nil || st[0].FinishedAt.Sub(*st[0].StartedAt) < 150*time.Millisecond {
		t.Errorf("step 0 timing = %v .. %v", st[0].StartedAt, st[0].FinishedAt)
	}
	// 重试后步骤进度重新开始。
	d = post[protocol.RunDetail](t, ts, "/api/runs/"+d.Run.ID+"/retry", "", http.StatusOK)
	if s := d.Jobs[0].StepStates; s[0].Status != model.StatusPending && s[0].Status != model.StatusRunning {
		t.Errorf("after retry = %+v", s)
	}
}

// 并发组：同组运行依次执行，后来的排队；不同组互不影响；排队中的运行可直接取消。
func TestConcurrencyQueue(t *testing.T) { storetest.ForEach(t, testConcurrencyQueue) }

func testConcurrencyQueue(t *testing.T, s store.Store) {
	ts := startStack(t, s)
	p := createPipeline(t, ts, `
name: deploy
params: [{name: TARGET, default: staging}]
concurrency: {group: "deploy-${TARGET}"}
stages:
  - name: s
    jobs: [{name: j, steps: [{run: "sleep 0.5"}]}]
`)
	trigger := func(target string) protocol.RunDetail {
		return post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", `{"env":{"TARGET":"`+target+`"}}`, http.StatusCreated)
	}
	a, b, c, other := trigger("prod"), trigger("prod"), trigger("prod"), trigger("staging")
	if a.Run.Status == model.StatusQueued || a.Run.ConcurrencyGroup != "deploy-prod" {
		t.Fatalf("first run = %s group %q", a.Run.Status, a.Run.ConcurrencyGroup)
	}
	if b.Run.Status != model.StatusQueued || c.Run.Status != model.StatusQueued || other.Run.Status == model.StatusQueued {
		t.Fatalf("statuses = %s %s %s", b.Run.Status, c.Run.Status, other.Run.Status)
	}
	if got := post[protocol.RunDetail](t, ts, "/api/runs/"+b.Run.ID+"/cancel", "", http.StatusOK); got.Run.Status != model.StatusCanceled {
		t.Errorf("cancel queued = %s", got.Run.Status)
	}
	ra, rc := waitRun(t, ts, a.Run.ID), waitRun(t, ts, c.Run.ID)
	if ra.Run.Status != model.StatusSuccess || rc.Run.Status != model.StatusSuccess {
		t.Fatalf("runs = %s %s", ra.Run.Status, rc.Run.Status)
	}
	if rc.Run.StartedAt.Before(*ra.Run.FinishedAt) {
		t.Errorf("queued run started %v before the first finished %v", rc.Run.StartedAt, ra.Run.FinishedAt)
	}
	if ro := waitRun(t, ts, other.Run.ID); ro.Run.StartedAt.After(*ra.Run.FinishedAt) {
		t.Errorf("other group waited: started %v", ro.Run.StartedAt)
	}
}

// cancel_previous：新运行取消同组未结束的运行，等其中止后再开始。
func TestConcurrencyCancelPrevious(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	p := createPipeline(t, ts, `
name: preview
params: [{name: SLEEP, default: "0"}]
concurrency: {cancel_previous: true}
stages:
  - name: s
    jobs: [{name: j, steps: [{run: "sleep $SLEEP"}]}]
`)
	runs := "/api/pipelines/" + p.ID + "/runs"
	old := post[protocol.RunDetail](t, ts, runs, `{"env":{"SLEEP":"30"}}`, http.StatusCreated)
	deadline := time.Now().Add(10 * time.Second)
	for jobsOf(t, ts, old.Run.ID)[0].Status != model.StatusRunning {
		if time.Now().After(deadline) {
			t.Fatal("first run did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	start := time.Now()
	next := post[protocol.RunDetail](t, ts, runs, "", http.StatusCreated)
	if next.Run.Status != model.StatusQueued {
		t.Errorf("new run = %s, want queued until the old one stops", next.Run.Status)
	}
	if d := waitRun(t, ts, old.Run.ID); d.Run.Status != model.StatusCanceled {
		t.Errorf("old run = %s", d.Run.Status)
	}
	if d := waitRun(t, ts, next.Run.ID); d.Run.Status != model.StatusSuccess || time.Since(start) > 10*time.Second {
		t.Errorf("new run = %s after %s", d.Run.Status, time.Since(start))
	}
}

// 提交状态：运行开始时置为进行中，结束时写入结果。
func TestCommitStatus(t *testing.T) {
	var mu sync.Mutex
	var calls []map[string]string
	git := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		body["path"], body["auth"] = r.URL.Path, r.Header.Get("Authorization")
		mu.Lock()
		calls = append(calls, body)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer git.Close()

	ts := startStack(t, store.NewMemory())
	p := createPipeline(t, ts, `
name: status
source: {url: "`+git.URL+`/acme/app.git", provider: gitea, token_secret: GIT_TOKEN, report_status: true}
stages:
  - name: s
    jobs: [{name: j, checkout: false, steps: [{run: "exit 1"}]}]
`)
	put(t, ts, "/api/pipelines/"+p.ID+"/secrets/GIT_TOKEN", `{"value":"s3cret"}`, http.StatusOK)
	sha := strings.Repeat("ab", 20)
	d := waitRun(t, ts, post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", `{"env":{"FLOW_GIT_COMMIT":"`+sha+`"}}`, http.StatusCreated).Run.ID)
	if d.Run.Status != model.StatusFailed {
		t.Fatalf("run = %s", d.Run.Status)
	}
	if _, err := ts.eng.DeliverNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
	want := "/api/v1/repos/acme/app/statuses/" + sha
	if c := calls[0]; c["path"] != want || c["auth"] != "token s3cret" || c["state"] != "pending" || c["context"] != "flow/status" {
		t.Errorf("running call = %v", c)
	}
	if c := calls[1]; c["state"] != "failure" || !strings.Contains(c["description"], "失败任务：j") {
		t.Errorf("final call = %v", c)
	}
}

// 测试报告：步骤失败时仍解析并上传 JUnit 报告。
func TestJUnitReport(t *testing.T) { storetest.ForEach(t, testJUnitReport) }

func testJUnitReport(t *testing.T, s store.Store) {
	ts := startStack(t, s)
	p := createPipeline(t, ts, `
name: tests
stages:
  - name: test
    jobs:
      - name: unit
        reports: {junit: ["reports/*.xml"]}
        steps:
          - name: test
            run: |
              mkdir -p reports
              cat > reports/TEST-a.xml <<'XML'
              <testsuite name="calc"><testcase classname="Calc" name="adds" time="0.2"/>
              <testcase classname="Calc" name="divides" time="0.1"><failure message="division by zero">trace</failure></testcase>
              <testcase classname="Calc" name="later"><skipped/></testcase></testsuite>
              XML
              exit 1
`)
	d := waitRun(t, ts, post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated).Run.ID)
	if d.Run.Status != model.StatusFailed {
		t.Fatalf("run = %s", d.Run.Status)
	}
	var reports []model.TestReport
	if err := json.Unmarshal([]byte(getText(t, ts, "/api/runs/"+d.Run.ID+"/tests")), &reports); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].JobName != "unit" || reports[0].Total != 3 || reports[0].Failed != 1 || reports[0].Skipped != 1 {
		t.Fatalf("reports = %+v", reports)
	}
	if c := reports[0].Cases[0]; c.Name != "divides" || c.Status != model.CaseFailed || !strings.Contains(c.Message, "division by zero") {
		t.Errorf("first case = %+v", c)
	}
	if log := getText(t, ts, "/api/jobs/"+d.Jobs[0].ID+"/logs"); !strings.Contains(log, "test report: 3 tests, 1 passed, 1 failed, 1 skipped") {
		t.Errorf("log:\n%s", log)
	}
}

// 统计：成功率、耗时、恢复时间、任务与测试汇总。
func TestPipelineStats(t *testing.T) { storetest.ForEach(t, testPipelineStats) }

func testPipelineStats(t *testing.T, s store.Store) {
	ts := startStack(t, s)
	p := createPipeline(t, ts, `
name: stats
params: [{name: FAIL, default: "0"}]
stages:
  - name: s
    jobs:
      - name: build
        steps: [{run: "sleep 0.2; test $FAIL = 0"}]
      - name: lint
        steps: [{run: "true"}]
`)
	runs := "/api/pipelines/" + p.ID + "/runs"
	for _, fail := range []string{"0", "1", "0"} {
		waitRun(t, ts, post[protocol.RunDetail](t, ts, runs, `{"env":{"FAIL":"`+fail+`"}}`, http.StatusCreated).Run.ID)
	}
	var st engine.PipelineStats
	if err := json.Unmarshal([]byte(getText(t, ts, "/api/pipelines/"+p.ID+"/stats?days=7")), &st); err != nil {
		t.Fatal(err)
	}
	if st.Runs != 3 || st.Success != 2 || st.Failed != 1 || st.SuccessRate < 0.66 || st.SuccessRate > 0.67 {
		t.Fatalf("stats = %+v", st)
	}
	if len(st.Daily) != 7 || st.Daily[6].Total != 3 || len(st.Recent) != 3 || st.Recent[0].Number != 1 || st.MeanRecovery <= 0 {
		t.Errorf("daily = %+v recent = %+v recovery %v", st.Daily, st.Recent, st.MeanRecovery)
	}
	if len(st.Jobs) != 2 || st.Jobs[0].Name != "build" || st.Jobs[0].Runs != 3 || st.Jobs[0].Failed != 1 || st.Jobs[0].AvgDuration < 0.15 {
		t.Errorf("jobs = %+v", st.Jobs)
	}
}

// 审计日志：成功的写操作被记录（含对象名称），失败的不记录；只读查询不记录。
func TestAuditLog(t *testing.T) { storetest.ForEach(t, testAuditLog) }

func testAuditLog(t *testing.T, s store.Store) {
	ts := startStack(t, s)
	yaml := "name: audited\nstages: [{name: s, jobs: [{name: j, steps: [{run: 'true'}]}]}]\n"
	p := createPipeline(t, ts, yaml)
	put(t, ts, "/api/pipelines/"+p.ID, strings.Replace(yaml, "'true'", "'echo hi'", 1), http.StatusOK)
	ts.do(t, http.MethodPut, "/api/pipelines/"+p.ID, "not: [valid") // 失败，不记录
	run := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	waitRun(t, ts, run.Run.ID)
	put(t, ts, "/api/pipelines/"+p.ID+"/secrets/TOKEN", `{"value":"x"}`, http.StatusOK)
	getText(t, ts, "/api/pipelines")
	if code, _ := ts.do(t, http.MethodDelete, "/api/pipelines/"+p.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	// 回收站中不能运行；恢复后再删除并彻底删除
	if code, _ := ts.do(t, http.MethodPost, "/api/pipelines/"+p.ID+"/runs", ""); code != http.StatusBadRequest {
		t.Fatalf("run in trash = %d", code)
	}
	if trash := getText(t, ts, "/api/pipelines/trash"); !strings.Contains(trash, `"deleted_at"`) {
		t.Fatalf("trash = %s", trash)
	}
	post[model.Pipeline](t, ts, "/api/pipelines/"+p.ID+"/recover", "", http.StatusOK)
	if code, _ := ts.do(t, http.MethodDelete, "/api/pipelines/"+p.ID+"?purge=true", ""); code != http.StatusBadRequest {
		t.Fatalf("purge outside trash = %d", code)
	}
	ts.do(t, http.MethodDelete, "/api/pipelines/"+p.ID, "")
	if code, _ := ts.do(t, http.MethodDelete, "/api/pipelines/"+p.ID+"?purge=true", ""); code != http.StatusNoContent {
		t.Fatalf("purge = %d", code)
	}

	var events []model.AuditEvent
	if err := json.Unmarshal([]byte(getText(t, ts, "/api/audit")), &events); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.Action+"|"+e.TargetName+"|"+e.Detail)
		if e.Actor != "admin" || e.IP == "" {
			t.Errorf("event = %+v", e)
		}
	}
	want := []string{
		"pipeline.delete|audited|彻底删除",
		"pipeline.delete|audited|移入回收站",
		"pipeline.recover|audited|从回收站恢复",
		"pipeline.delete|audited|移入回收站",
		"secret.put|TOKEN|流水线 audited",
		"run.trigger|#1|流水线 audited",
		"pipeline.update|audited|版本 v2",
		"pipeline.create|audited|版本 v1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var page []model.AuditEvent
	json.Unmarshal([]byte(getText(t, ts, "/api/audit?category=pipeline&limit=2")), &page)
	if len(page) != 2 || page[0].Action != "pipeline.delete" {
		t.Errorf("filtered = %+v", page)
	}
}

// 待我审批与运行筛选。
func TestApprovalsAndRunFilter(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	p := createPipeline(t, ts, `
name: gated
params: [{name: FAIL, default: "0"}]
stages:
  - name: build
    jobs: [{name: build, steps: [{run: "test $FAIL = 0"}]}]
  - name: gate
    jobs: [{name: approve-release, type: approval, approvers: [alice]}]
`)
	runs := "/api/pipelines/" + p.ID + "/runs"
	waitRun(t, ts, post[protocol.RunDetail](t, ts, runs, `{"env":{"FAIL":"1"}}`, http.StatusCreated).Run.ID)
	d := post[protocol.RunDetail](t, ts, runs, "", http.StatusCreated)
	var list []engine.Approval
	for range 100 {
		json.Unmarshal([]byte(getText(t, ts, "/api/approvals")), &list)
		if len(list) > 0 {
			break
		}
		waitShort()
	}
	// admin 可以审批指定了审批人的卡点。
	if len(list) != 1 || list[0].PipelineName != "gated" || list[0].Run.ID != d.Run.ID || list[0].Job.Name != "approve-release" {
		t.Fatalf("approvals = %+v", list)
	}
	var failed []model.Run
	json.Unmarshal([]byte(getText(t, ts, runs+"?status=failed")), &failed)
	if len(failed) != 1 || failed[0].Number != 1 {
		t.Errorf("failed runs = %+v", failed)
	}
	var page []model.Run
	json.Unmarshal([]byte(getText(t, ts, runs+"?before=2&limit=5")), &page)
	if len(page) != 1 || page[0].Number != 1 {
		t.Errorf("page = %+v", page)
	}
}
