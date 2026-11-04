package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

// 通用变量组、服务连接注入；流水线 Webhook；运行备注。
func TestVarsConnectionsWebhook(t *testing.T) { storetest.ForEach(t, testVarsConnectionsWebhook) }

func testVarsConnectionsWebhook(t *testing.T, s store.Store) {
	ts := startStack(t, s)

	// 变量组：非私密变量注入，私密变量注入且脱敏；保存时不回显，提交掩码保留原值。
	code, body := ts.do(t, http.MethodPost, "/api/var-groups", `{"name":"common","vars":[{"name":"REGION","value":"cn"},{"name":"NPM_TOKEN","value":"tok-123456","secret":true}]}`)
	if code != http.StatusCreated || strings.Contains(body, "tok-123456") {
		t.Fatalf("create group = %d %s", code, body)
	}
	var g model.VarGroup
	json.Unmarshal([]byte(body), &g)
	if code, b := ts.do(t, http.MethodPut, "/api/var-groups/"+g.ID, `{"name":"common","vars":[{"name":"REGION","value":"cn-east"},{"name":"NPM_TOKEN","value":"••••••","secret":true}]}`); code != http.StatusOK {
		t.Fatalf("update group = %d %s", code, b)
	}
	if code, _ := ts.do(t, http.MethodPost, "/api/var-groups", `{"name":"bad","vars":[{"name":"X","value":"","secret":true}]}`); code != http.StatusBadRequest {
		t.Errorf("secret without value = %d", code)
	}

	// 服务连接：私密字段不回显；步骤引用时注入 CONN_*，私密值脱敏。
	code, body = ts.do(t, http.MethodPost, "/api/connections", `{"type":"registry","name":"acr","values":{"platform":"acr","username":"deployer","password":"reg-pass-999"}}`)
	if code != http.StatusCreated || strings.Contains(body, "reg-pass-999") || !strings.Contains(body, `"secrets":["password"]`) {
		t.Fatalf("create connection = %d %s", code, body)
	}
	if code, _ := ts.do(t, http.MethodPost, "/api/connections", `{"type":"registry","name":"x","values":{"username":"u"}}`); code != http.StatusBadRequest {
		t.Errorf("missing required secret = %d", code)
	}
	bad := "name: badconn\nstages: [{name: s, jobs: [{name: j, steps: [{run: 'true', connection: nope}]}]}]\n"
	if code, _ := ts.do(t, http.MethodPost, "/api/pipelines", bad); code != http.StatusBadRequest {
		t.Errorf("unknown connection = %d", code)
	}

	yaml := `name: vars
vars:
  - {name: TARGET, value: test, runtime: true}
  - {name: FIXED, value: keep}
var_groups: [common]
concurrency: {max: 2}
stages:
  - name: s
    jobs:
      - name: j
        steps:
          - run: 'echo "region=$REGION target=$TARGET fixed=$FIXED token=$NPM_TOKEN"; sleep 0.3'
          - name: login
            connection: acr
            run: 'echo "user=$CONN_USERNAME pass=$CONN_PASSWORD type=$CONN_TYPE"'
`
	p := createPipeline(t, ts, yaml)
	detail := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", `{"remark":"首次发布"}`, http.StatusCreated)
	if detail.Run.Remark != "首次发布" {
		t.Errorf("remark = %q", detail.Run.Remark)
	}
	done := waitRun(t, ts, detail.Run.ID)
	log := getText(t, ts, "/api/jobs/"+done.Jobs[0].ID+"/logs")
	if done.Run.Status != model.StatusSuccess || !strings.Contains(log, "region=cn-east target=test fixed=keep token=***") ||
		!strings.Contains(log, "user=deployer pass=*** type=registry") || strings.Contains(log, "reg-pass-999") {
		t.Fatalf("status %s, log:\n%s", done.Run.Status, log)
	}
	put(t, ts, "/api/runs/"+done.Run.ID+"/remark", `{"remark":"已验证"}`, http.StatusOK)

	// 流水线 Webhook：未开启时 401；运行时变量生效，其余忽略。
	hook := "/api/open/flow/webhook/" + p.ID
	if code, _ := ts.do(t, http.MethodPost, hook+"?token=x", `{}`); code != http.StatusUnauthorized {
		t.Errorf("webhook without trigger = %d", code)
	}
	var tr model.Trigger
	json.Unmarshal([]byte(postRaw(t, ts, "/api/pipelines/"+p.ID+"/triggers", `{"kind":"generic"}`, http.StatusCreated)), &tr)
	if tr.Secret == "" {
		t.Fatal("generic trigger has no token")
	}
	code, body = ts.do(t, http.MethodPost, hook+"?token="+tr.Secret, `{"TARGET":"prod","FIXED":"changed","N":3}`)
	var res struct {
		OK      bool     `json:"ok"`
		RunID   string   `json:"runId"`
		Number  int      `json:"buildNumber"`
		Vars    []string `json:"vars"`
		Ignored []string `json:"ignored"`
	}
	json.Unmarshal([]byte(body), &res)
	if code != http.StatusOK || !res.OK || res.Number != 2 || strings.Join(res.Vars, ",") != "TARGET" || strings.Join(res.Ignored, ",") != "FIXED,N" {
		t.Fatalf("webhook = %d %s", code, body)
	}
	done = waitRun(t, ts, res.RunID)
	if log := getText(t, ts, "/api/jobs/"+done.Jobs[0].ID+"/logs"); !strings.Contains(log, "target=prod fixed=keep") {
		t.Errorf("webhook run log:\n%s", log)
	}

	// 最大并发 2：三次同时触发，第三次排队。
	var ids []string
	for range 3 {
		ids = append(ids, post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated).Run.ID)
	}
	var third protocol.RunDetail
	json.Unmarshal([]byte(getText(t, ts, "/api/runs/"+ids[2])), &third)
	if third.Run.Status != model.StatusQueued {
		t.Errorf("third run = %s, want queued", third.Run.Status)
	}
	for _, id := range ids {
		if d := waitRun(t, ts, id); d.Run.Status != model.StatusSuccess {
			t.Errorf("run %s = %s", id, d.Run.Status)
		}
	}
}
