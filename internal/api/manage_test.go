package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"flow/internal/engine"
	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

// 删除运行记录、复制流水线、分组管理、日志下载、统计维度。
func TestRunDeleteCopyGroups(t *testing.T) { storetest.ForEach(t, testRunDeleteCopyGroups) }

func testRunDeleteCopyGroups(t *testing.T, s store.Store) {
	ts := startStack(t, s)
	p := createPipeline(t, ts, "name: orig\n# 注释保留\nstages: [{name: s, jobs: [{name: j, steps: [{run: 'echo hi'}]}]}]\n")
	put(t, ts, "/api/pipelines/"+p.ID+"/group", `{"group":"后端"}`, http.StatusOK)
	var ids []string
	for range 3 {
		ids = append(ids, waitRun(t, ts, post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated).Run.ID).Run.ID)
	}
	var d protocol.RunDetail
	json.Unmarshal([]byte(getText(t, ts, "/api/runs/"+ids[0])), &d)
	code, body := ts.do(t, http.MethodGet, "/api/jobs/"+d.Jobs[0].ID+"/logs?download=1", "")
	if code != http.StatusOK || !strings.Contains(body, "hi") {
		t.Errorf("log download = %d %s", code, body)
	}

	var st engine.PipelineStats
	json.Unmarshal([]byte(getText(t, ts, "/api/pipelines/"+p.ID+"/stats?days=7")), &st)
	heat := 0
	for _, row := range st.Heatmap {
		for _, n := range row {
			heat += n
		}
	}
	if len(st.ByTrigger) != 1 || st.ByTrigger[0].Key != "manual" || st.ByTrigger[0].Success != 3 || len(st.ByUser) != 1 || st.ByUser[0].Key != "admin" || heat != 3 {
		t.Errorf("stats by trigger %+v by user %+v heat %d", st.ByTrigger, st.ByUser, heat)
	}

	if code, _ := ts.do(t, http.MethodDelete, "/api/runs/"+ids[0], ""); code != http.StatusNoContent {
		t.Fatalf("delete run = %d", code)
	}
	if code, _ := ts.do(t, http.MethodGet, "/api/runs/"+ids[0], ""); code != http.StatusNotFound {
		t.Errorf("deleted run = %d", code)
	}
	if b := postRawMethod(t, ts, http.MethodDelete, "/api/pipelines/"+p.ID+"/runs"); !strings.Contains(b, `"deleted":2`) {
		t.Errorf("delete all = %s", b)
	}

	// 复制：名称改写、注释保留、分组相同
	code, body = ts.do(t, http.MethodPost, "/api/pipelines/"+p.ID+"/copy", `{"name":"copy-1"}`)
	var cp model.Pipeline
	json.Unmarshal([]byte(body), &cp)
	if code != http.StatusCreated || cp.Name != "copy-1" || cp.Group != "后端" || !strings.Contains(cp.YAML, "# 注释保留") {
		t.Fatalf("copy = %d %s", code, body)
	}
	if code, _ := ts.do(t, http.MethodPost, "/api/pipelines/"+p.ID+"/copy", `{"name":"copy-1"}`); code != http.StatusConflict {
		t.Errorf("copy duplicate name = %d", code)
	}

	// 分组：新建空分组、重命名（流水线随之改名）、删除（流水线变为未分组）
	postRaw(t, ts, "/api/pipeline-groups", `{"name":"前端"}`, http.StatusNoContent)
	if g := getText(t, ts, "/api/pipeline-groups"); g != `["前端","后端"]` && g != `["前端","后端"]`+"\n" {
		t.Errorf("groups = %s", g)
	}
	put(t, ts, "/api/pipeline-groups/后端", `{"name":"服务端"}`, http.StatusNoContent)
	var pl model.Pipeline
	json.Unmarshal([]byte(getText(t, ts, "/api/pipelines/"+p.ID)), &pl)
	if pl.Group != "服务端" {
		t.Errorf("group after rename = %q", pl.Group)
	}
	if code, b := ts.do(t, http.MethodDelete, "/api/pipeline-groups/服务端", ""); code != http.StatusNoContent {
		t.Fatalf("delete group = %d %s", code, b)
	}
	pl = model.Pipeline{}
	json.Unmarshal([]byte(getText(t, ts, "/api/pipelines/"+p.ID)), &pl)
	if pl.Group != "" {
		t.Errorf("group after delete = %q", pl.Group)
	}
}

func postRawMethod(t *testing.T, ts *stack, method, path string) string {
	t.Helper()
	code, body := ts.do(t, method, path, "")
	if code != http.StatusOK {
		t.Fatalf("%s %s = %d %s", method, path, code, body)
	}
	return body
}
