package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/sshx"
)

// 主机 Agent：以 Agent 方式接入的主机上运行 flow-runner -agent，轮询领取部署到本机的任务，
// 解压上传内容、执行部署脚本并回传日志。执行部署任务的 Runner 经由 Server 下发任务并转发日志，主机不需要开放 SSH。

func (r *Runner) agentLoop(ctx context.Context) error {
	log.Printf("agent polling %s", r.cfg.ServerURL)
	for ctx.Err() == nil {
		t, err := r.client.AgentClaim(ctx, r.cfg.Info)
		if err != nil && ctx.Err() == nil {
			log.Printf("agent claim: %v", err)
		}
		if t == nil {
			sleep(ctx, r.cfg.PollInterval)
			continue
		}
		r.runHostTask(ctx, t)
	}
	return nil
}

func (r *Runner) runHostTask(parent context.Context, t *model.HostTask) {
	log.Printf("host task %s (job %s) started", t.ID, t.JobID)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	logs := newLogUploader(func(c context.Context, data []byte) error {
		stop, err := r.client.AgentLog(c, t.ID, data)
		if stop {
			cancel()
		}
		return err
	}, r.cfg.LogFlushInterval)
	// 没有输出时也定期续租，并感知取消。
	go func() {
		tk := time.NewTicker(r.cfg.HeartbeatInterval)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				if stop, err := r.client.AgentLog(ctx, t.ID, nil); stop {
					cancel()
				} else if err != nil && ctx.Err() == nil {
					log.Printf("host task %s heartbeat: %v", t.ID, err)
				}
			}
		}
	}()
	code, err := r.execHostTask(ctx, t, logs)
	logs.Close()
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer fcancel()
	if err := r.client.AgentFinish(fctx, t.ID, code, msg); err != nil {
		log.Printf("host task %s finish: %v", t.ID, err)
	}
	log.Printf("host task %s finished: exit %d %s", t.ID, code, msg)
}

func (r *Runner) execHostTask(ctx context.Context, t *model.HostTask, out io.Writer) (int, error) {
	if t.Target == "" || !filepath.IsAbs(t.Target) {
		return -1, fmt.Errorf("deploy target must be an absolute path, got %q", t.Target)
	}
	if t.HasUpload {
		fmt.Fprintf(out, "uploading to %s\n", t.Target)
		if err := os.MkdirAll(t.Target, 0o755); err != nil {
			return -1, err
		}
		rc, err := r.client.AgentUpload(ctx, t.ID)
		if err != nil {
			return -1, err
		}
		_, err = extractTarGz(t.Target, rc)
		rc.Close()
		if err != nil {
			return -1, fmt.Errorf("upload: %w", err)
		}
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", t.Script)
	setProcessGroup(cmd)
	cmd.Env = os.Environ()
	for k, v := range t.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ctx.Err() != nil {
		return -1, errors.New("canceled")
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), fmt.Errorf("exit code %d", exitErr.ExitCode())
	}
	return -1, err
}

// agentScript 把部署步骤拼成一个脚本：每个步骤在子 shell 中执行，失败即停止。
func agentScript(env map[string]string, h protocol.DeployHost, job *model.Job, d *model.DeployTarget) string {
	var b strings.Builder
	b.WriteString(remotePrefix(env, h, d))
	b.WriteString(exports(env))
	for i, step := range job.Steps {
		fmt.Fprintf(&b, "echo %s\n(\n%s%s\n)\n", sshx.Quote(fmt.Sprintf("==> [%d/%d] %s", i+1, len(job.Steps), step.Name)),
			exports(step.Env), step.Run)
	}
	return b.String()
}

// deployAgent 通过 Server 把部署交给主机上的 Agent 执行，并转发其日志。
func (r *Runner) deployAgent(ctx context.Context, h protocol.DeployHost, job *model.Job, d *model.DeployTarget, bundle []byte,
	env map[string]string, out io.Writer) (int, error) {
	fmt.Fprintf(out, "dispatching to agent on %s\n", h.Name)
	t, err := r.client.CreateHostTask(ctx, job.ID, job.RunnerID, protocol.HostTaskRequest{
		Host: h.Name, Script: agentScript(env, h, job, d), Target: d.Target, Bundle: bundle})
	if err != nil {
		return -1, err
	}
	offset, warned := 0, false
	start := time.Now()
	for {
		canceled := ctx.Err() != nil
		pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		p, err := r.client.HostTaskProgress(pctx, t.ID, job.RunnerID, offset, canceled)
		cancel()
		if err != nil {
			if canceled {
				return -1, ctx.Err()
			}
			log.Printf("host task %s progress: %v", t.ID, err)
		} else {
			if len(p.Log) > 0 {
				out.Write(p.Log)
				offset += len(p.Log)
			}
			switch p.Status {
			case model.StatusSuccess:
				return 0, nil
			case model.StatusFailed:
				return p.ExitCode, errors.New(p.Error)
			case model.StatusCanceled:
				return -1, context.Canceled
			case model.StatusQueued:
				if !p.Online && !warned && time.Since(start) > 15*time.Second {
					fmt.Fprintf(out, "waiting for the agent on %s (it is offline; check the flow-agent service)\n", h.Name)
					warned = true
				}
			}
		}
		if canceled {
			return -1, ctx.Err()
		}
		sleep(ctx, time.Second)
	}
}
