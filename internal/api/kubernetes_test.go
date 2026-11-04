package api

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"flow/internal/k8s"
	"flow/internal/k8s/k8stest"
	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/runner"
	"flow/internal/store"
)

// startK8sRunner 启动一个使用 Kubernetes 执行器的 Runner，Pod 由假 kubelet 在进程内执行。
func startK8sRunner(t *testing.T, st *stack) *k8stest.Fake {
	t.Helper()
	fake := k8stest.New(t, func(ctx context.Context, c k8s.Container, getenv func(string) string) (int, string) {
		if n := len(c.Command); n < 2 || c.Command[n-2] != "pod" {
			return 127, "unexpected command " + strings.Join(c.Command, " ")
		}
		return runner.RunPod(ctx, c.Command[len(c.Command)-1], getenv)
	})
	client, err := k8s.New(k8s.Config{APIURL: fake.URL, Namespace: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	st.startRunner(t, runner.Config{Token: sharedRunnerToken, RunnerID: "k8s-runner", Concurrency: 3,
		Kubernetes: &runner.KubernetesConfig{
			Client: client, HelperImage: "flow-runner:test", DefaultImage: "alpine:3.20", CachePVC: "flow-cache",
			PollInterval: 50 * time.Millisecond, Requests: map[string]string{"cpu": "100m"},
		}})
	return fake
}

func TestKubernetesExecutor(t *testing.T) {
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
	os.WriteFile(filepath.Join(repo, "app.txt"), []byte("source code\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "init")

	ts := newStack(t, store.NewMemory())
	fake := startK8sRunner(t, ts)
	put(t, ts, "/api/secrets/DEPLOY_TOKEN", `{"value":"k8s-secret-value"}`, http.StatusOK)

	p := createPipeline(t, ts, `
name: on-k8s
source: {url: "file://`+repo+`", branch: main}
stages:
  - name: build
    jobs:
      - name: build
        image: golang:1.23
        cache: {key: deps, paths: [.deps]}
        artifacts: [dist]
        steps:
          - run: test -d .deps && echo "cache hit" || (mkdir .deps && echo "cache miss")
          - run: mkdir -p dist && cp app.txt dist/ && echo "token=$DEPLOY_TOKEN"
  - name: deploy
    jobs:
      - name: deploy
        checkout: false
        steps:
          - run: cat dist/app.txt && test ! -e app.txt
`)
	run := func() protocol.RunDetail {
		d := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
		return waitRun(t, ts, d.Run.ID)
	}
	d := run()
	jobs := jobsByName(d)
	buildLog := getText(t, ts, "/api/jobs/"+jobs["build"].ID+"/logs")
	deployLog := getText(t, ts, "/api/jobs/"+jobs["deploy"].ID+"/logs")
	if d.Run.Status != model.StatusSuccess {
		t.Fatalf("run = %s\nbuild:\n%s\ndeploy:\n%s", d.Run.Status, buildLog, deployLog)
	}
	for _, want := range []string{"==> kubernetes pod ci/flow-", "image golang:1.23", "==> HEAD is ", "cache miss", "token=***", "upload artifacts", "==> success"} {
		if !strings.Contains(buildLog, want) {
			t.Errorf("build log missing %q:\n%s", want, buildLog)
		}
	}
	if strings.Contains(buildLog, "k8s-secret-value") {
		t.Error("secret leaked into log")
	}
	if !strings.Contains(deployLog, "image alpine:3.20") || !strings.Contains(deployLog, "source code") {
		t.Errorf("deploy log:\n%s", deployLog)
	}
	// Pod 结束后删除，Secret 随之回收；init 容器把 flow-runner 装进了共享卷。
	if created, deleted := fake.Created(), fake.Deleted(); len(created) != 2 || len(deleted) < 2 {
		t.Errorf("pods created %v, deleted %v", created, deleted)
	}
	if s := fake.Secrets(); len(s) != 0 {
		t.Errorf("secrets left: %v", s)
	}
	if fi, err := os.Stat(filepath.Join(fake.VolumeDir(fake.Created()[0], "flow-bin"), "flow-runner")); err != nil || fi.Mode()&0o100 == 0 {
		t.Errorf("flow-runner not installed into shared volume: %v", err)
	}

	// 缓存卷在 Pod 之间共享：第二次运行命中缓存。
	d = run()
	if log := getText(t, ts, "/api/jobs/"+jobsByName(d)["build"].ID+"/logs"); !strings.Contains(log, "cache hit") {
		t.Errorf("second build log:\n%s", log)
	}
}

func TestKubernetesFailures(t *testing.T) {
	ts := newStack(t, store.NewMemory())
	fake := startK8sRunner(t, ts)
	fake.PullErrors["no/such-image"] = "ImagePullBackOff"
	p := createPipeline(t, ts, `
name: k8s-fail
stages:
  - name: s
    jobs:
      - {name: exit, steps: [{name: boom, run: "echo before; exit 7"}]}
      - {name: badimage, image: no/such-image, steps: [{run: "true"}]}
      - {name: slow, steps: [{run: "sleep 30"}]}
`)
	d := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", "", http.StatusCreated)
	// 等 exit 结束、slow 开始运行后取消。
	deadline := time.Now().Add(10 * time.Second)
	for {
		jobs := jobsByName(protocol.RunDetail{Jobs: jobsOf(t, ts, d.Run.ID)})
		if jobs["exit"].Status == model.StatusFailed && jobs["slow"].Status == model.StatusRunning && fake.Pod("flow-"+jobs["slow"].ID+"-1") != nil &&
			fake.Pod("flow-"+jobs["slow"].ID+"-1").Status.Phase == "Running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exit job did not finish or slow job did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	post[protocol.RunDetail](t, ts, "/api/runs/"+d.Run.ID+"/cancel", "", http.StatusOK)
	d = waitRun(t, ts, d.Run.ID)
	jobs := jobsByName(d)
	if j := jobs["exit"]; j.Status != model.StatusFailed || j.ExitCode != 7 || !strings.Contains(j.Error, `step "boom"`) {
		t.Errorf("exit job = %s code %d err %q", j.Status, j.ExitCode, j.Error)
	}
	if log := getText(t, ts, "/api/jobs/"+jobs["exit"].ID+"/logs"); !strings.Contains(log, "before") {
		t.Errorf("exit log:\n%s", log)
	}
	if j := jobs["badimage"]; j.Status != model.StatusFailed || !strings.Contains(j.Error, "ImagePullBackOff") {
		t.Errorf("badimage job = %s err %q", j.Status, j.Error)
	}
	if j := jobs["slow"]; j.Status != model.StatusCanceled {
		t.Errorf("slow job = %s err %q", j.Status, j.Error)
	}
	time.Sleep(200 * time.Millisecond)
	for _, j := range d.Jobs {
		if name := "flow-" + j.ID + "-1"; !slices.Contains(fake.Deleted(), name) {
			t.Errorf("pod %s not deleted", name)
		}
	}
}

func TestKubernetesOrphanCleanup(t *testing.T) {
	ts := newStack(t, store.NewMemory())
	fake := k8stest.New(t, func(ctx context.Context, c k8s.Container, _ func(string) string) (int, string) {
		<-ctx.Done()
		return 1, "killed"
	})
	client, _ := k8s.New(k8s.Config{APIURL: fake.URL, Namespace: "ci"})
	// 模拟上一次进程遗留的 Pod 与其他 Runner 的 Pod。
	for name, runnerLabel := range map[string]string{"flow-old-1": "k8s-runner", "flow-other-1": "someone-else"} {
		_, err := client.CreatePod(context.Background(), &k8s.Pod{Metadata: k8s.ObjectMeta{Name: name, Labels: map[string]string{
			"app.kubernetes.io/managed-by": "flow-runner", "flow.runner": runnerLabel}},
			Spec: k8s.PodSpec{Containers: []k8s.Container{{Name: "job", Image: "x", Command: []string{"pod", "run"}}}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	ts.startRunner(t, runner.Config{Token: sharedRunnerToken, RunnerID: "k8s-runner",
		Kubernetes: &runner.KubernetesConfig{Client: client, HelperImage: "h", DefaultImage: "d"}})
	deadline := time.Now().Add(5 * time.Second)
	for fake.Pod("flow-old-1") != nil {
		if time.Now().After(deadline) {
			t.Fatal("orphan pod not deleted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fake.Pod("flow-other-1") == nil {
		t.Error("pod of another runner deleted")
	}
}
