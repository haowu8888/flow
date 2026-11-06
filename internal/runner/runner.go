// Package runner 实现执行节点：轮询领取 Job、执行 Step、上报日志/心跳/结果。
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/secret"
	"flow/internal/version"
)

type Config struct {
	ServerURL         string
	Token             string
	RunnerID          string   // 使用共享 Token 时的标识；注册 Runner 以 Server 分配的 ID 为准
	Labels            []string // 上报的标签，Job 的 runs-on 须是其子集
	WorkDir           string
	CacheDir          string // 本地缓存目录，默认 WorkDir/cache
	NoCache           bool   // 不使用缓存（如 Kubernetes 执行器未配置缓存卷）
	Concurrency       int
	PollInterval      time.Duration
	HeartbeatInterval time.Duration // 须明显小于 Server 的租约时长
	LogFlushInterval  time.Duration
	// Kubernetes 非空时使用 Kubernetes 执行器：每个 Job 在独立 Pod 中执行（忽略 WorkDir / CacheDir）。
	Kubernetes *KubernetesConfig
	// Once：领取并执行一个 Job 后退出（GitHub 节点）；OnceWait 内没有可领取的 Job 也退出。
	Once     bool
	OnceWait time.Duration
	// Agent：作为主机 Agent 运行，只执行部署到本机的任务（主机以 Agent 方式接入）。
	Agent bool
	// Info 是上报给 Server 的节点环境（为空时自动探测）。
	Info *model.NodeInfo
}

type Runner struct {
	cfg    Config
	client *Client
}

func New(cfg Config) *Runner {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 5 * time.Second
	}
	if cfg.LogFlushInterval <= 0 {
		cfg.LogFlushInterval = 500 * time.Millisecond
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = filepath.Join(cfg.WorkDir, "cache")
	}
	if cfg.Info == nil {
		host, _ := os.Hostname()
		cfg.Info = &model.NodeInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, Hostname: host, Version: version.Version, Concurrency: cfg.Concurrency}
	}
	return &Runner{cfg: cfg, client: NewClient(cfg.ServerURL, cfg.Token)}
}

// Run 启动 Concurrency 个 worker，阻塞直到 ctx 取消且所有 Job 收尾。
func (r *Runner) Run(ctx context.Context) error {
	if r.cfg.Kubernetes != nil {
		r.cleanupOrphanPods(ctx)
	} else if err := os.MkdirAll(r.cfg.WorkDir, 0o755); err != nil {
		return err
	}
	switch {
	case r.cfg.Agent:
		return r.agentLoop(ctx)
	case r.cfg.Once:
		return r.once(ctx)
	}
	var wg sync.WaitGroup
	for i := 0; i < r.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.worker(ctx)
		}()
	}
	wg.Wait()
	return nil
}

func (r *Runner) worker(ctx context.Context) {
	for ctx.Err() == nil {
		a, err := r.client.Claim(ctx, r.cfg.RunnerID, r.cfg.Labels, r.cfg.Info)
		if err != nil && ctx.Err() == nil {
			log.Printf("claim: %v", err)
		}
		if a == nil {
			sleep(ctx, r.cfg.PollInterval)
			continue
		}
		r.execute(ctx, a)
	}
}

// once 领取一个 Job 执行后返回；OnceWait 内没有可领取的 Job 时返回 ErrNoJob。
func (r *Runner) once(ctx context.Context) error {
	wait := r.cfg.OnceWait
	if wait <= 0 {
		wait = 5 * time.Minute
	}
	deadline := time.Now().Add(wait)
	for ctx.Err() == nil && time.Now().Before(deadline) {
		a, err := r.client.Claim(ctx, r.cfg.RunnerID, r.cfg.Labels, r.cfg.Info)
		if err != nil && ctx.Err() == nil {
			log.Printf("claim: %v", err)
		}
		if a != nil {
			r.execute(ctx, a)
			return nil
		}
		sleep(ctx, r.cfg.PollInterval)
	}
	if ctx.Err() != nil {
		return nil
	}
	return ErrNoJob
}

// ErrNoJob 表示 -once 模式在等待时间内没有领取到 Job。
var ErrNoJob = errors.New("no job to claim")

// execute 执行一个 Job：后台心跳续租并感知取消，日志按密钥值脱敏后上传，最后上报结果。
// 后续请求使用 Server 在 Job.RunnerID 中确认的标识。
func (r *Runner) execute(parent context.Context, a *protocol.ClaimResponse) {
	job := a.Job
	log.Printf("job %s (%s) started", job.ID, job.Name)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var canceled, leaseLost atomic.Bool
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(r.cfg.HeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			cancelReq, err := r.client.Heartbeat(ctx, job.ID, job.RunnerID)
			switch {
			case errors.Is(err, ErrLeaseLost):
				leaseLost.Store(true)
				cancel()
				return
			case err != nil:
				if ctx.Err() == nil {
					log.Printf("job %s heartbeat: %v", job.ID, err)
				}
			case cancelReq:
				canceled.Store(true)
				cancel()
				return
			}
		}
	}()

	logs := newLogUploader(func(c context.Context, data []byte) error {
		return r.client.AppendLog(c, job.ID, job.RunnerID, data)
	}, r.cfg.LogFlushInterval)
	a.ApplyConnections()
	out := secret.NewMasker(logs, a.MaskValues())

	execCtx, execCancel := ctx, context.CancelFunc(func() {})
	if job.TimeoutSec > 0 {
		execCtx, execCancel = context.WithTimeout(ctx, time.Duration(job.TimeoutSec)*time.Second)
	}
	run := r.runJob
	switch {
	case job.Deploy != nil:
		run = r.runDeploy // 部署任务总在 Runner 进程中通过 SSH 执行
	case r.cfg.Kubernetes != nil:
		run = r.runPod
	}
	exitCode, err := run(execCtx, a, out)
	timedOut := errors.Is(execCtx.Err(), context.DeadlineExceeded)
	execCancel()
	cancel()
	<-hbDone

	req := protocol.CompleteRequest{RunnerID: job.RunnerID, Status: model.StatusSuccess, ExitCode: exitCode, Outputs: a.Outputs}
	switch {
	case canceled.Load():
		req.Status, req.Error = model.StatusCanceled, "canceled by user"
	case parent.Err() != nil:
		req.Status, req.Error = model.StatusFailed, "runner shutting down"
	case timedOut:
		req.Status, req.Error = model.StatusFailed, fmt.Sprintf("timed out after %s", time.Duration(job.TimeoutSec)*time.Second)
	case err != nil:
		req.Status, req.Error = model.StatusFailed, err.Error()
	}
	if req.Error != "" {
		fmt.Fprintf(out, "\n==> %s: %s\n", req.Status, req.Error)
	} else {
		fmt.Fprintf(out, "\n==> %s\n", req.Status)
	}
	out.Flush()
	logs.Close()

	if leaseLost.Load() {
		log.Printf("job %s: lease lost, result discarded", job.ID)
		return
	}
	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	if err := r.client.Complete(cctx, job.ID, req); err != nil {
		log.Printf("job %s complete: %v", job.ID, err)
		return
	}
	log.Printf("job %s (%s) %s", job.ID, job.Name, req.Status)
}

// runJob 在独立工作目录中准备工作区并执行 Step。
func (r *Runner) runJob(ctx context.Context, a *protocol.ClaimResponse, out io.Writer) (int, error) {
	dir, err := os.MkdirTemp(r.cfg.WorkDir, "job-")
	if err != nil {
		return -1, fmt.Errorf("create workdir: %w", err)
	}
	defer os.RemoveAll(dir)
	if err := r.prepareWorkspace(ctx, a, dir, out); err != nil {
		return -1, err
	}
	code, err := r.runSteps(ctx, a, dir, out)
	r.uploadReports(a.Job, dir, out)
	return code, err
}

// jobEnv 是 Step 的环境变量：密钥覆盖 YAML 中的同名变量（与 GitLab 的项目变量优先级一致）。
func jobEnv(a *protocol.ClaimResponse) map[string]string {
	env := maps.Clone(a.Job.Env)
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, a.Env) // 部署环境的变量
	maps.Copy(env, a.Secrets)
	return env
}

// prepareWorkspace 拉取代码、解压上游产物、恢复缓存。
func (r *Runner) prepareWorkspace(ctx context.Context, a *protocol.ClaimResponse, dir string, out io.Writer) error {
	job := a.Job
	if job.Checkout != nil {
		commit, err := checkout(ctx, dir, job.Checkout, a.Secrets, out)
		if err != nil {
			return err
		}
		// 上报实际检出的提交：同一运行的后续 Job 检出同一提交，回滚时也据此重放。失败不影响执行。
		if commit != "" && job.Checkout.Commit == "" {
			if err := r.client.ReportCommit(ctx, job.ID, job.RunnerID, commit); err != nil {
				log.Printf("job %s report commit: %v", job.ID, err)
			}
		}
	}
	if err := r.downloadArtifacts(ctx, dir, a.Artifacts, out); err != nil {
		return err
	}
	if job.Cache != nil {
		if r.cfg.NoCache {
			fmt.Fprintln(out, "==> cache disabled on this runner")
		} else {
			r.restoreCache(job, dir, jobEnv(a), out)
		}
	}
	return nil
}

// runSteps 依次执行 Step，全部成功后保存缓存、上传产物；返回失败 Step 的退出码。
func (r *Runner) runSteps(ctx context.Context, a *protocol.ClaimResponse, dir string, out io.Writer) (int, error) {
	job := a.Job
	env := jobEnv(a)
	sc := r.newStepContext(job, dir, out)
	defer func() { a.Outputs = sc.outputs }()
	for i, step := range job.Steps {
		fmt.Fprintf(out, "==> [%d/%d] %s\n", i+1, len(job.Steps), step.Name)
		r.reportStep(ctx, job, i, model.StatusRunning, 0, nil)
		before := maps.Clone(sc.outputs)
		base := maps.Clone(env)
		maps.Copy(base, sc.vars(job.Image != ""))
		senv := stepEnv(base, step.Env, a.Secrets)
		var code int
		var err error
		if native := nativeSteps[step.Kind]; native != nil {
			if step.Connection != "" {
				senv = maps.Clone(senv)
				senv[stepConnectionEnv] = step.Connection
			}
			code, err = native(ctx, r, a, sc, senv, out)
		} else {
			code, err = runStep(ctx, job, i, dir, senv, sc, out)
		}
		if err != nil {
			// 被取消 / 超时的步骤不在此上报，由 Server 在 Job 结束时按最终状态收尾。
			if ctx.Err() == nil {
				r.reportStep(ctx, job, i, model.StatusFailed, code, nil)
			}
			return code, fmt.Errorf("step %q: %w", step.Name, err)
		}
		sc.collect(out)
		r.reportStep(ctx, job, i, model.StatusSuccess, 0, changedOutputs(before, sc.outputs))
	}
	if job.Cache != nil && !r.cfg.NoCache {
		r.saveCache(job, dir, env, out)
	}
	if len(job.Artifacts) > 0 {
		if err := r.uploadArtifacts(ctx, job, dir, out); err != nil {
			return -1, err
		}
	}
	return 0, nil
}

// reportStep 上报步骤进度，仅用于展示：失败只记录日志，不影响执行。
func (r *Runner) reportStep(ctx context.Context, job *model.Job, index int, status model.Status, exitCode int, outputs map[string]string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req := protocol.StepRequest{RunnerID: job.RunnerID, Index: index, Status: status, ExitCode: exitCode, Outputs: outputs}
	if err := r.client.ReportStep(ctx, job.ID, req); err != nil {
		log.Printf("job %s report step %d: %v", job.ID, index, err)
	}
}

// changedOutputs 返回步骤执行后新增或改变的任务输出。
func changedOutputs(before, after map[string]string) map[string]string {
	var out map[string]string
	for k, v := range after {
		if old, ok := before[k]; !ok || old != v {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// stepEnv 在 Job 变量之上叠加步骤变量（其中的 ${NAME} 按 Job 变量展开，未定义为空），密钥仍然优先。
func stepEnv(jobEnv, step, secrets map[string]string) map[string]string {
	if len(step) == 0 {
		return jobEnv
	}
	env := maps.Clone(jobEnv)
	for k, v := range step {
		env[k] = envRef.ReplaceAllStringFunc(v, func(ref string) string { return jobEnv[ref[2:len(ref)-1]] })
	}
	maps.Copy(env, secrets)
	return env
}
