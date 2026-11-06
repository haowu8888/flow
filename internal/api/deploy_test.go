package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flow/internal/engine"
	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/sshx/sshtest"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

// 主机部署：环境要求审批 → 审批后按批次通过 SSH 上传产物并执行脚本，记录部署历史。
func TestDeployToHosts(t *testing.T) { storetest.ForEach(t, testDeployToHosts) }

func testDeployToHosts(t *testing.T, s store.Store) {
	ts := startStack(t, s)
	srv := sshtest.Start(t)
	target := filepath.Join(t.TempDir(), "app")

	env := post[model.Environment](t, ts, "/api/environments", `{"name":"生产","rank":3,"require_approval":true}`, http.StatusCreated)
	grp := post[model.HostGroup](t, ts, "/api/host-groups", fmt.Sprintf(`{"name":"prod-web","environment_id":%q}`, env.ID), http.StatusCreated)
	keyJSON, _ := json.Marshal(srv.ClientKey)
	h1 := post[model.Host](t, ts, "/api/hosts", fmt.Sprintf(`{"name":"web-1","address":%q,"port":%d,"username":%q,"auth_type":"key","credential":%s,"group_ids":[%q]}`,
		srv.Host, srv.Port, srv.User, keyJSON, grp.ID), http.StatusCreated)
	post[model.Host](t, ts, "/api/hosts", fmt.Sprintf(`{"name":"web-2","address":%q,"port":%d,"username":%q,"auth_type":"password","credential":%q,"group_ids":[%q]}`,
		srv.Host, srv.Port, srv.User, srv.Password, grp.ID), http.StatusCreated)
	if strings.Contains(getText(t, ts, "/api/hosts"), "BEGIN") || strings.Contains(getText(t, ts, "/api/hosts"), srv.Password) {
		t.Fatal("credential leaked in host list")
	}
	// 连接测试记录主机公钥。
	check := post[engine.HostCheck](t, ts, "/api/hosts/"+h1.ID+"/test", "", http.StatusOK)
	if !check.OK || !check.NewHostKey || check.Host.HostKey != srv.HostKey || !strings.Contains(check.Output, "@") {
		t.Fatalf("check = %+v", check)
	}

	put(t, ts, "/api/secrets/DB_PASS", `{"value":"hunter22"}`, http.StatusOK)
	p := createPipeline(t, ts, `
name: shop
stages:
  - name: build
    jobs:
      - name: build
        artifacts: [dist]
        steps: [{run: "mkdir -p dist && echo v42 > dist/app.txt"}]
  - name: deploy
    jobs:
      - name: deploy-prod
        deploy: {hosts: prod-web, upload: dist, target: "`+target+`", batch: 1}
        steps:
          - {name: install, run: "cat app.txt > \"deployed-$FLOW_DEPLOY_HOST.txt\" && echo pass=$DB_PASS && pwd"}
`)
	// 引用不存在的主机组时拒绝保存。
	if code, body := ts.do(t, http.MethodPost, "/api/pipelines/validate", strings.Replace(p.YAML, "prod-web", "nope", 1)); code != http.StatusBadRequest || !strings.Contains(body, "host group") {
		t.Errorf("unknown group = %d %s", code, body)
	}
	// 被引用的主机组不能删除。
	if code, _ := ts.do(t, http.MethodDelete, "/api/host-groups/"+grp.ID, ""); code != http.StatusBadRequest {
		t.Errorf("delete used group = %d", code)
	}

	d := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	var job *model.Job
	for range 200 {
		job = jobsByName(protocol.RunDetail{Jobs: jobsOf(t, ts, d.Run.ID)})["deploy-prod"]
		if job.Status == model.StatusWaiting {
			break
		}
		waitShort()
	}
	if job.Status != model.StatusWaiting {
		t.Fatalf("deploy job = %s, want waiting for approval", job.Status)
	}
	post[model.Job](t, ts, "/api/jobs/"+job.ID+"/approve", `{"approve":true}`, http.StatusOK)
	d = waitRun(t, ts, d.Run.ID)
	job = jobsByName(d)["deploy-prod"]
	log := getText(t, ts, "/api/jobs/"+job.ID+"/logs")
	if d.Run.Status != model.StatusSuccess {
		t.Fatalf("run = %s, log:\n%s", d.Run.Status, log)
	}
	for _, name := range []string{"web-1", "web-2"} {
		if b, err := os.ReadFile(filepath.Join(target, "deployed-"+name+".txt")); err != nil || string(b) != "v42\n" {
			t.Errorf("%s deployed = %q, %v", name, b, err)
		}
	}
	if !strings.Contains(log, "[web-1] pass=***") || strings.Contains(log, "hunter22") || !strings.Contains(log, "[web-2] "+target) {
		t.Errorf("log:\n%s", log)
	}
	if len(job.HostStates) != 2 || job.HostStates[0].Status != model.StatusSuccess || job.HostStates[1].Status != model.StatusSuccess {
		t.Errorf("host states = %+v", job.HostStates)
	}
	var deps []model.Deployment
	json.Unmarshal([]byte(getText(t, ts, "/api/deployments?environment="+env.ID)), &deps)
	if len(deps) != 1 || deps[0].Status != model.StatusSuccess || deps[0].EnvironmentName != "生产" || deps[0].RunNumber != 1 ||
		len(deps[0].Hosts) != 2 || deps[0].FinishedAt == nil {
		t.Errorf("deployments = %+v", deps)
	}
	// 流水线的部署历史里有这次主机部署
	var hist struct {
		Items []struct {
			Deployments []model.Deployment `json:"deployments"`
		} `json:"items"`
	}
	if len(deps) == 1 {
		getJSON(t, ts, "/api/pipelines/"+deps[0].PipelineID+"/deliveries", &hist)
	}
	if len(deps) != 1 || len(hist.Items) != 1 || len(hist.Items[0].Deployments) != 1 || hist.Items[0].Deployments[0].ID != deps[0].ID {
		t.Errorf("deliveries = %+v", hist)
	}

	// 主机公钥不一致时拒绝连接，后续主机跳过。
	other := sshtest.Start(t)
	put(t, ts, "/api/hosts/"+h1.ID, fmt.Sprintf(`{"name":"web-1","address":%q,"port":%d,"username":%q,"auth_type":"key","group_ids":[%q]}`,
		srv.Host, other.Port, srv.User, grp.ID), http.StatusOK) // 改端口会清除指纹
	check = post[engine.HostCheck](t, ts, "/api/hosts/"+h1.ID+"/test", "", http.StatusOK)
	if check.OK || !strings.Contains(check.Error, "认证失败") {
		t.Errorf("other server check = %+v", check) // 客户端公钥未在 other 上授权
	}
}

func waitShort() { time.Sleep(50 * time.Millisecond) }

// 跳板机 + 环境变量与密钥 + 按环境重新部署（沿用原产物，只执行部署任务）。
func TestDeployJumpEnvRedeploy(t *testing.T) { storetest.ForEach(t, testDeployJumpEnvRedeploy) }

func testDeployJumpEnvRedeploy(t *testing.T, s store.Store) {
	ts := startStack(t, s)
	jump, srv := sshtest.Start(t), sshtest.Start(t)
	target := filepath.Join(t.TempDir(), "app")

	env := post[model.Environment](t, ts, "/api/environments",
		`{"name":"测试","variables":{"DB_HOST":"db.test","APP_ENV":"test"},"secrets":{"DB_PASS":"s3cret-pass"}}`, http.StatusCreated)
	if len(env.SecretNames) != 1 || env.SecretNames[0] != "DB_PASS" || strings.Contains(getText(t, ts, "/api/environments"), "s3cret") {
		t.Fatalf("env = %+v", env)
	}
	grp := post[model.HostGroup](t, ts, "/api/host-groups", fmt.Sprintf(`{"name":"test-web","environment_id":%q}`, env.ID), http.StatusCreated)
	keyJSON, _ := json.Marshal(jump.ClientKey)
	bastion := post[model.Host](t, ts, "/api/hosts", fmt.Sprintf(`{"name":"bastion","address":%q,"port":%d,"username":%q,"auth_type":"key","credential":%s}`,
		jump.Host, jump.Port, jump.User, keyJSON), http.StatusCreated)
	h := post[model.Host](t, ts, "/api/hosts", fmt.Sprintf(`{"name":"app-1","address":%q,"port":%d,"username":%q,"auth_type":"password","credential":%q,"group_ids":[%q],"jump_host_id":%q}`,
		srv.Host, srv.Port, srv.User, srv.Password, grp.ID, bastion.ID), http.StatusCreated)
	if check := post[engine.HostCheck](t, ts, "/api/hosts/"+h.ID+"/test", "", http.StatusOK); !check.OK || len(jump.Forwards()) == 0 {
		t.Fatalf("check via jump = %+v forwards %v", check, jump.Forwards())
	}
	// 被引用的跳板机不能删除。
	if code, _ := ts.do(t, http.MethodDelete, "/api/hosts/"+bastion.ID, ""); code != http.StatusBadRequest {
		t.Errorf("delete jump host = %d", code)
	}

	p := createPipeline(t, ts, `
name: svc
params: [{name: TAG, default: v1}]
stages:
  - name: build
    jobs:
      - {name: build, artifacts: [dist], steps: [{run: "mkdir -p dist && echo $TAG > dist/version"}]}
  - name: deploy
    jobs:
      - name: deploy-test
        deploy: {hosts: test-web, upload: dist, target: "`+target+`"}
        steps: [{run: "echo \"$(cat version) $DB_HOST $APP_ENV $DB_PASS\" > deployed.txt"}]
`)
	runs := "/api/pipelines/" + p.ID + "/runs"
	first := waitRun(t, ts, post[protocol.RunDetail](t, ts, runs, `{"env":{"TAG":"v1"}}`, http.StatusCreated).Run.ID)
	read := func() string { b, _ := os.ReadFile(filepath.Join(target, "deployed.txt")); return string(b) }
	if first.Run.Status != model.StatusSuccess || read() != "v1 db.test test s3cret-pass\n" {
		t.Fatalf("first = %s, deployed %q, log:\n%s", first.Run.Status, read(), getText(t, ts, "/api/jobs/"+jobsByName(first)["deploy-test"].ID+"/logs"))
	}
	if log := getText(t, ts, "/api/jobs/"+jobsByName(first)["deploy-test"].ID+"/logs"); !strings.Contains(log, "via jump host") || strings.Contains(log, "s3cret-pass") {
		t.Errorf("log:\n%s", log)
	}
	second := waitRun(t, ts, post[protocol.RunDetail](t, ts, runs, `{"env":{"TAG":"v2"}}`, http.StatusCreated).Run.ID)
	if second.Run.Status != model.StatusSuccess || !strings.HasPrefix(read(), "v2 ") {
		t.Fatalf("second deployed %q", read())
	}

	// 把测试环境退回 v1：只执行部署任务，产物取自第一次运行。
	rd := post[protocol.RunDetail](t, ts, "/api/runs/"+first.Run.ID+"/redeploy", `{"job":"deploy-test"}`, http.StatusCreated)
	rd = waitRun(t, ts, rd.Run.ID)
	jobs := jobsByName(rd)
	if rd.Run.Status != model.StatusSuccess || rd.Run.ArtifactsFrom != first.Run.ID || jobs["build"].Status != model.StatusSkipped ||
		jobs["deploy-test"].Status != model.StatusSuccess || !strings.HasPrefix(read(), "v1 ") {
		t.Fatalf("redeploy = %s, build %s, deployed %q", rd.Run.Status, jobs["build"].Status, read())
	}
	if code, _ := ts.do(t, http.MethodPost, "/api/runs/"+first.Run.ID+"/redeploy", `{"job":"build"}`); code != http.StatusBadRequest {
		t.Errorf("redeploy non-deploy job = %d", code)
	}
	// 删除环境密钥。
	put(t, ts, "/api/environments/"+env.ID, `{"name":"测试","remove_secrets":["DB_PASS"]}`, http.StatusOK)
	var envs []model.Environment
	json.Unmarshal([]byte(getText(t, ts, "/api/environments")), &envs)
	if len(envs[0].SecretNames) != 0 || envs[0].Variables["DB_HOST"] != "db.test" {
		t.Errorf("after remove = %+v", envs[0])
	}
}
