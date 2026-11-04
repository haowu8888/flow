package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

// 内置步骤、步骤间变量与任务输出、运行条件、命名构建物、任务插件、审批消息链接。
func TestStepKindsOutputsPlugins(t *testing.T) { storetest.ForEach(t, testStepKindsOutputsPlugins) }

func testStepKindsOutputsPlugins(t *testing.T, s store.Store) {
	ts := startStack(t, s)
	ts.eng.SetPublicURL(ts.URL)

	var mu sync.Mutex
	var hooks []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		hooks = append(hooks, r.URL.Path+" "+string(b))
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	defer sink.Close()
	postRaw(t, ts, "/api/connections", `{"type":"telegram","name":"tg","values":{"bot_token":"123:abc","chat_id":"-1","api_base":"`+sink.URL+`"}}`, http.StatusCreated)

	yaml := `name: kinds
stages:
  - name: build
    jobs:
      - name: build
        steps:
          - kind: set-variable
            with: {vars: "VERSION=1.2.${FLOW_RUN_NUMBER}"}
          - name: export
            run: |
              echo "FROM_STEP=hello" >> "$FLOW_ENV"
              mkdir -p bin dist && printf '#!/bin/sh\necho tool-ok\n' > bin/mytool && chmod +x bin/mytool
              echo "$FLOW_WORKSPACE/bin" >> "$FLOW_PATH"
              echo "app $VERSION" > dist/app.txt
          - name: use
            run: 'echo "step=$FROM_STEP version=$VERSION run=$FLOW_RUN_NUMBER"; mytool'
          - kind: artifact-upload
            with: {name: app, paths: dist}
        plugins:
          - {type: webhook, url: "` + "${HOOK}" + `", on: [success], title: "${pipeline} #${run} ${task} ${status}"}
  - name: gate
    jobs:
      - name: approve
        type: approval
        notify: [{connection: tg}]
  - name: deploy
    jobs:
      - name: deploy
        if: "VERSION =~ '^1\\.2\\.' && FLOW_TRIGGER == 'manual'"
        steps:
          - kind: artifact-download
            with: {artifact: app, target_dir: out}
          - run: 'cat out/app.txt; echo "upstream=$VERSION id=$ARTIFACT_APP"'
      - name: never
        if: "VERSION == 'x'"
        steps: [{run: 'exit 1'}]
  - name: after
    jobs:
      - name: after-never
        steps: [{run: 'echo after'}]
`
	put(t, ts, "/api/secrets/HOOK", `{"value":"`+sink.URL+`/hook"}`, http.StatusOK)
	bad := strings.Replace(yaml, "kind: set-variable", "kind: no-such-kind", 1)
	if code, b := ts.do(t, http.MethodPost, "/api/pipelines", bad); code != http.StatusBadRequest {
		t.Fatalf("unknown kind = %d %s", code, b)
	}
	p := createPipeline(t, ts, yaml)
	run := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated).Run

	// 等审批消息
	var link string
	deadline := time.Now().Add(20 * time.Second)
	for link == "" && time.Now().Before(deadline) {
		ts.eng.DeliverJobNotifications(context.Background())
		mu.Lock()
		for _, h := range hooks {
			if strings.HasPrefix(h, "/bot123:abc/sendMessage") {
				link = regexp.MustCompile(`"url":"([^"]*action=approve[^"]*)"`).FindStringSubmatch(h)[1]
			}
		}
		mu.Unlock()
		time.Sleep(200 * time.Millisecond)
	}
	if link == "" {
		t.Fatalf("no approval message; hooks = %v", hooks)
	}
	link = strings.ReplaceAll(link, `\u0026`, "&")
	resp, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), "确认通过") {
		t.Fatalf("confirm page = %s", page)
	}
	if resp, err = http.Post(link, "", nil); err != nil || resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("approve via link %s = %v %v %s", link, resp.StatusCode, err, b)
	}
	if resp, _ = http.Post(strings.Replace(link, "sig=", "sig=x", 1), "", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("bad signature = %d", resp.StatusCode)
	}

	done := waitRun(t, ts, run.ID)
	jobs := map[string]*model.Job{}
	for _, j := range done.Jobs {
		jobs[j.Name] = j
	}
	buildLog := getText(t, ts, "/api/jobs/"+jobs["build"].ID+"/logs")
	deployLog := getText(t, ts, "/api/jobs/"+jobs["deploy"].ID+"/logs")
	if done.Run.Status != model.StatusSuccess || !strings.Contains(buildLog, "step=hello version=1.2.1 run=1") || !strings.Contains(buildLog, "tool-ok") {
		t.Fatalf("status %s, build log:\n%s\ndeploy log:\n%s", done.Run.Status, buildLog, deployLog)
	}
	if jobs["build"].Outputs["VERSION"] != "1.2.1" || jobs["build"].Outputs["ARTIFACT_APP"] != "app@v1" {
		t.Errorf("outputs = %v", jobs["build"].Outputs)
	}
	if !strings.Contains(deployLog, "app 1.2.1") || !strings.Contains(deployLog, "upstream=1.2.1 id=app@v1") {
		t.Errorf("deploy log:\n%s", deployLog)
	}
	if jobs["never"].Status != model.StatusSkipped || !strings.Contains(jobs["never"].Error, "运行条件不满足") {
		t.Errorf("never = %s %s", jobs["never"].Status, jobs["never"].Error)
	}
	// 条件跳过的上游不阻塞下游
	if jobs["after-never"].Status != model.StatusSuccess {
		t.Errorf("after-never = %s %s", jobs["after-never"].Status, jobs["after-never"].Error)
	}
	if jobs["approve"].DecidedBy != "审批链接" {
		t.Errorf("decided by = %q", jobs["approve"].DecidedBy)
	}
	var arts []model.Artifact
	json.Unmarshal([]byte(getText(t, ts, "/api/runs/"+run.ID+"/artifacts")), &arts)
	if len(arts) != 1 || arts[0].Name != "app" || arts[0].Version != "v1" || arts[0].Format != "zip" {
		t.Errorf("artifacts = %+v", arts)
	}

	// 插件：成功时发送，标题按模板展开
	ts.eng.DeliverJobNotifications(context.Background())
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, h := range hooks {
		if strings.HasPrefix(h, "/hook ") && strings.Contains(h, `"title":"kinds #1 build 成功"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("plugin not sent; hooks = %v", hooks)
	}
	if kinds := getText(t, ts, "/api/step-kinds"); !strings.Contains(kinds, `"kind":"setup-node"`) {
		t.Errorf("step kinds = %.200s", kinds)
	}
}
