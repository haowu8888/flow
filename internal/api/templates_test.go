package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"flow/internal/model"
	"flow/internal/pipeline"
	"flow/internal/protocol"
	"flow/internal/store"
)

const greetTemplate = `
description: 打招呼
inputs:
  WHO: {required: true}
  PUNCT: {default: "!"}
steps:
  - name: hello
    run: echo "hello $WHO$PUNCT tag=$TAG"
  - name: bye
    run: echo "bye $WHO"
`

func TestStepTemplates(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	tmpl := post[model.Template](t, ts, "/api/templates",
		jsonBody(map[string]any{"kind": "steps", "name": "greet", "yaml": greetTemplate}), http.StatusCreated)
	if tmpl.Description != "打招呼" || tmpl.UpdatedBy != "admin" {
		t.Errorf("template = %+v", tmpl)
	}

	// 引用不存在的模板或缺少必填输入时，保存流水线即报错。
	for _, bad := range []string{`{uses: nope}`, `{uses: greet}`} {
		code, body := ts.do(t, http.MethodPost, "/api/pipelines", "name: bad\nstages: [{name: s, jobs: [{name: j, steps: ["+bad+"]}]}]\n")
		if code != http.StatusBadRequest {
			t.Errorf("pipeline with %s = %d: %s", bad, code, body)
		}
	}

	// 校验接口与保存规则一致（含模板引用），且不创建流水线。
	for body, want := range map[string]int{
		"name: ok\nstages: [{name: s, jobs: [{name: j, steps: [{uses: greet, with: {WHO: x}}]}]}]\n": http.StatusNoContent,
		"name: bad\nstages: [{name: s, jobs: [{name: j, steps: [{uses: greet}]}]}]\n":                http.StatusBadRequest,
		"name: bad\nstages: [{name: s, jobs: [{name: j, needs: [x], steps: [{run: a}]}]}]\n":         http.StatusBadRequest,
		"name: [\n": http.StatusBadRequest,
	} {
		if code, data := ts.do(t, http.MethodPost, "/api/pipelines/validate", body); code != want {
			t.Errorf("validate %q = %d (want %d): %s", body, code, want, data)
		}
	}
	if ps := getText(t, ts, "/api/pipelines"); strings.Contains(ps, `"ok"`) {
		t.Errorf("validate created a pipeline: %s", ps)
	}

	p := createPipeline(t, ts, `
name: uses-demo
env: {VERSION: "1.2"}
stages:
  - name: s
    jobs:
      - name: j
        steps:
          - name: greet
            uses: greet
            with: {WHO: flow}
            env: {TAG: "v${VERSION}"}
`)
	d := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	if steps := d.Jobs[0].Steps; len(steps) != 2 || steps[0].Name != "greet / hello" || steps[0].Env["WHO"] != "flow" {
		t.Fatalf("expanded steps = %+v", steps)
	}
	d = waitRun(t, ts, d.Run.ID)
	log := getText(t, ts, "/api/jobs/"+d.Jobs[0].ID+"/logs")
	if d.Run.Status != model.StatusSuccess || !strings.Contains(log, "hello flow! tag=v1.2") || !strings.Contains(log, "bye flow") {
		t.Errorf("run = %s, log:\n%s", d.Run.Status, log)
	}

	users := getText(t, ts, "/api/templates/"+tmpl.ID+"/users")
	if !strings.Contains(users, p.ID) {
		t.Errorf("users = %s", users)
	}
	// 被引用时：不能改名、不能删除、不能改成会破坏引用方的样子（新增必填输入）。
	update := func(name, yaml string) (int, string) {
		return ts.do(t, http.MethodPut, "/api/templates/"+tmpl.ID, jsonBody(map[string]any{"name": name, "yaml": yaml}))
	}
	if code, body := update("greet2", greetTemplate); code != http.StatusConflict {
		t.Errorf("rename used = %d: %s", code, body)
	}
	breaking := strings.Replace(greetTemplate, "inputs:", "inputs:\n  LANG: {required: true}", 1)
	if code, body := update("greet", breaking); code != http.StatusBadRequest || !strings.Contains(body, "uses-demo") {
		t.Errorf("breaking change = %d: %s", code, body)
	}
	if code, body := ts.do(t, http.MethodDelete, "/api/templates/"+tmpl.ID, ""); code != http.StatusConflict {
		t.Errorf("delete used = %d: %s", code, body)
	}

	// 兼容的修改对之后的运行生效，已创建的运行保持快照。
	if code, body := update("greet", strings.Replace(greetTemplate, "hello", "hi", 2)); code != http.StatusOK {
		t.Fatalf("update = %d: %s", code, body)
	}
	d2 := waitRun(t, ts, post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated).Run.ID)
	if log := getText(t, ts, "/api/jobs/"+d2.Jobs[0].ID+"/logs"); !strings.Contains(log, "hi flow!") {
		t.Errorf("new run log:\n%s", log)
	}
	if old := jobsOf(t, ts, d.Run.ID)[0].Steps[0]; old.Name != "greet / hello" {
		t.Errorf("old run steps changed: %+v", old)
	}

	// 去掉引用后可以删除。
	if code, body := ts.do(t, http.MethodPut, "/api/pipelines/"+p.ID, "name: uses-demo\nstages: [{name: s, jobs: [{name: j, steps: [{run: 'true'}]}]}]\n"); code != http.StatusOK {
		t.Fatalf("update pipeline = %d: %s", code, body)
	}
	if code, body := ts.do(t, http.MethodDelete, "/api/templates/"+tmpl.ID, ""); code != http.StatusNoContent {
		t.Errorf("delete unused = %d: %s", code, body)
	}

	if code, _ := ts.do(t, http.MethodPost, "/api/templates", jsonBody(map[string]any{"kind": "steps", "name": "Bad Name", "yaml": greetTemplate})); code != http.StatusBadRequest {
		t.Errorf("bad name = %d", code)
	}
	if code, _ := ts.do(t, http.MethodPost, "/api/templates", jsonBody(map[string]any{"kind": "steps", "name": "nested", "yaml": "steps: [{uses: greet}]"})); code != http.StatusBadRequest {
		t.Errorf("nested uses = %d", code)
	}
}

func TestPipelineTemplates(t *testing.T) {
	ts := newStack(t, store.NewMemory())
	var list []model.Template
	if err := json.Unmarshal([]byte(getText(t, ts, "/api/templates?kind=pipeline")), &list); err != nil {
		t.Fatal(err)
	}
	builtin := 0
	for _, tm := range list {
		if tm.Builtin {
			builtin++
			if _, err := pipeline.Parse([]byte(tm.YAML)); err != nil {
				t.Errorf("builtin %s invalid: %v", tm.ID, err)
			}
		}
	}
	if builtin < 5 {
		t.Errorf("builtin templates = %d", builtin)
	}
	if code, _ := ts.do(t, http.MethodPut, "/api/templates/"+list[0].ID, jsonBody(map[string]any{"name": "x", "yaml": list[0].YAML})); code != http.StatusForbidden {
		t.Errorf("edit builtin = %d", code)
	}

	own := post[model.Template](t, ts, "/api/templates", jsonBody(map[string]any{
		"kind": "pipeline", "name": "我们的发布流程", "description": "团队标准", "yaml": "name: x\nstages: [{name: s, jobs: [{name: j, steps: [{run: 'true'}]}]}]\n",
	}), http.StatusCreated)
	if code, _ := ts.do(t, http.MethodPost, "/api/templates", jsonBody(map[string]any{"kind": "pipeline", "name": "我们的发布流程", "yaml": own.YAML})); code != http.StatusConflict {
		t.Errorf("duplicate = %d", code)
	}
	if code, _ := ts.do(t, http.MethodPost, "/api/templates", jsonBody(map[string]any{"kind": "pipeline", "name": "bad", "yaml": "name: x"})); code != http.StatusBadRequest {
		t.Errorf("invalid pipeline yaml = %d", code)
	}

	// 只读成员可以看，不能写；其他租户看不到。
	postRaw(t, ts, "/api/members", `{"username":"vic","password":"viewer-pass","role":"viewer"}`, http.StatusCreated)
	viewer := ts.as(ts.login(t, "vic", "viewer-pass"))
	if code, _ := viewer.do(t, http.MethodGet, "/api/templates/"+own.ID, ""); code != http.StatusOK {
		t.Errorf("viewer read = %d", code)
	}
	if code, _ := viewer.do(t, http.MethodDelete, "/api/templates/"+own.ID, ""); code != http.StatusForbidden {
		t.Errorf("viewer delete = %d", code)
	}
	postRaw(t, ts, "/api/tenants", `{"id":"team-b","name":"B"}`, http.StatusCreated)
	other := ts.as(ts.login(t, "admin", adminPassword))
	postRaw(t, other, "/api/me/tenant", `{"id":"team-b"}`, http.StatusOK)
	if code, _ := other.do(t, http.MethodGet, "/api/templates/"+own.ID, ""); code != http.StatusNotFound {
		t.Errorf("cross-tenant read = %d", code)
	}
	if code, _ := other.do(t, http.MethodGet, "/api/templates/"+list[0].ID, ""); code != http.StatusOK {
		t.Errorf("builtin from other tenant = %d", code)
	}
	if body := getText(t, other, "/api/templates"); strings.Contains(body, own.ID) {
		t.Errorf("cross-tenant list leaks: %s", body)
	}
}

func jsonBody(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func jobsOf(t *testing.T, ts *stack, runID string) []*model.Job {
	t.Helper()
	var d protocol.RunDetail
	if err := json.Unmarshal([]byte(getText(t, ts, "/api/runs/"+runID)), &d); err != nil {
		t.Fatal(err)
	}
	return d.Jobs
}

func TestTemplateVersions(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	tmpl := post[model.Template](t, ts, "/api/templates",
		jsonBody(map[string]any{"kind": "steps", "name": "greet", "yaml": greetTemplate}), http.StatusCreated)
	if tmpl.Version != 1 {
		t.Fatalf("created version = %d", tmpl.Version)
	}
	put := func(yaml string, base int) (int, string) {
		return ts.do(t, http.MethodPut, "/api/templates/"+tmpl.ID+"?base_version="+strconv.Itoa(base),
			jsonBody(map[string]any{"name": "greet", "yaml": yaml}))
	}
	v2 := strings.Replace(greetTemplate, "hello", "hi", 1)
	if code, body := put(v2, 1); code != http.StatusOK || !strings.Contains(body, `"version":2`) {
		t.Fatalf("update = %d: %s", code, body)
	}
	if code, body := put(greetTemplate, 1); code != http.StatusConflict || !strings.Contains(body, "version 2") {
		t.Errorf("stale update = %d: %s", code, body)
	}
	if code, body := put(v2, 2); code != http.StatusOK || !strings.Contains(body, `"version":2`) {
		t.Errorf("unchanged save should keep version = %d: %s", code, body)
	}

	// 锁定 v1 的引用不受之后修改影响；未锁定的使用最新版本。
	p := createPipeline(t, ts, `
name: pinned
stages:
  - name: s
    jobs:
      - name: j
        steps:
          - {uses: greet@1, with: {WHO: old}}
          - {uses: greet, with: {WHO: new}}
`)
	d := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	steps := d.Jobs[0].Steps
	if len(steps) != 4 || steps[0].Name != "greet / hello" || steps[2].Name != "greet / hi" {
		t.Fatalf("expanded steps = %+v", steps)
	}
	users := getText(t, ts, "/api/templates/"+tmpl.ID+"/users")
	if !strings.Contains(users, `"versions":[0,1]`) {
		t.Errorf("users = %s", users)
	}
	// 新增必填输入会破坏未锁定的引用；只有锁定引用时则允许。
	breaking := strings.Replace(v2, "inputs:", "inputs:\n  LANG: {required: true}", 1)
	if code, body := put(breaking, 2); code != http.StatusBadRequest {
		t.Errorf("breaking change = %d: %s", code, body)
	}
	for bad, want := range map[string]string{"greet@9": "no version 9", "greet@0": "invalid template reference", "nope@1": "not found"} {
		code, body := ts.do(t, http.MethodPost, "/api/pipelines/validate", "name: x\nstages: [{name: s, jobs: [{name: j, steps: [{uses: '"+bad+"', with: {WHO: a}}]}]}]\n")
		if code != http.StatusBadRequest || !strings.Contains(body, want) {
			t.Errorf("validate %s = %d: %s", bad, code, body)
		}
	}

	// 历史与回退：回退生成新版本，历史保留。
	var revs []model.TemplateRevision
	getJSON(t, ts, "/api/templates/"+tmpl.ID+"/revisions", &revs)
	if len(revs) != 2 || revs[0].Version != 2 || revs[0].YAML != "" {
		t.Fatalf("revisions = %+v", revs)
	}
	var r1 model.TemplateRevision
	getJSON(t, ts, "/api/templates/"+tmpl.ID+"/revisions/1", &r1)
	if r1.YAML != greetTemplate || r1.CreatedBy != "admin" {
		t.Errorf("revision 1 = %+v", r1)
	}
	if code, body := ts.do(t, http.MethodPost, "/api/templates/"+tmpl.ID+"/revisions/1/restore?base_version=1", ""); code != http.StatusConflict {
		t.Errorf("stale restore = %d: %s", code, body)
	}
	restored := post[model.Template](t, ts, "/api/templates/"+tmpl.ID+"/revisions/1/restore?base_version=2", "", http.StatusOK)
	if restored.Version != 3 || restored.YAML != greetTemplate {
		t.Errorf("restored = %+v", restored)
	}
	getJSON(t, ts, "/api/templates/"+tmpl.ID+"/revisions", &revs)
	if len(revs) != 3 || revs[0].RestoredFrom != 1 {
		t.Errorf("revisions after restore = %+v", revs)
	}
	if code, _ := ts.do(t, http.MethodPost, "/api/templates/"+tmpl.ID+"/revisions/9/restore", ""); code != http.StatusNotFound {
		t.Errorf("restore missing = %d", code)
	}
	if code, body := ts.do(t, http.MethodGet, "/api/templates/builtin:go/revisions", ""); code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Errorf("builtin revisions = %d: %s", code, body)
	}
}

func getJSON(t *testing.T, ts *stack, path string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(getText(t, ts, path)), v); err != nil {
		t.Fatal(err)
	}
}
