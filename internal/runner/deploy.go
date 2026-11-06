package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"flow/internal/model"
	"flow/internal/pipeline"
	"flow/internal/protocol"
	"flow/internal/sshx"
)

// runDeploy 执行主机部署任务：在本机准备工作区（代码、上游产物），打包 artifact 构建物或 upload 路径，
// 按批次通过 SSH 上传到每台主机的目标目录并依次执行步骤。前一批全部成功才继续下一批。
func (r *Runner) runDeploy(ctx context.Context, a *protocol.ClaimResponse, out io.Writer) (int, error) {
	job := a.Job
	d := job.Deploy
	if a.DeployError != "" {
		return -1, errors.New(a.DeployError)
	}
	dir, err := os.MkdirTemp(r.cfg.WorkDir, "deploy-")
	if err != nil {
		return -1, fmt.Errorf("create workdir: %w", err)
	}
	defer os.RemoveAll(dir)
	if err := r.prepareWorkspace(ctx, a, dir, out); err != nil {
		return -1, err
	}
	var bundle []byte
	if d.Artifact != "" {
		ref, err := findNamed(a, d.Artifact)
		if err != nil {
			return 1, fmt.Errorf("deploy.artifact: %w", err)
		}
		pkg := filepath.Join(dir, ".flow-deploy-artifact")
		if err := r.fetchNamed(ctx, ref, pkg); err != nil {
			return -1, fmt.Errorf("download artifact %s: %w", ref.Name, err)
		}
		if bundle, err = packUpload(pkg, "."); err != nil {
			return -1, fmt.Errorf("deploy.artifact: %w", err)
		}
		fmt.Fprintf(out, "==> 部署构建物 %s %s（%s），解压到每台主机的 %s\n", ref.Name, ref.Version, humanSize(ref.Size), d.Target)
	} else if d.Upload != "" {
		if bundle, err = packUpload(dir, d.Upload); err != nil {
			return -1, fmt.Errorf("deploy.upload: %w", err)
		}
		fmt.Fprintf(out, "==> packed %s (%s)\n", d.Upload, humanSize(int64(len(bundle))))
	}

	batch := max(d.Batch, 1)
	st := &hostProgress{r: r, job: job, hosts: make([]model.HostState, len(a.Hosts))}
	for i, h := range a.Hosts {
		st.hosts[i] = model.HostState{Name: h.Name, Address: h.Address, Status: model.StatusPending}
	}
	st.report(ctx)
	fmt.Fprintf(out, "==> deploy to %d host(s) in group %s, %d per batch, target %s\n", len(a.Hosts), d.Group, batch, d.Target)
	env := jobEnv(a)
	lw := &lockedWriter{w: out}
	for start := 0; start < len(a.Hosts); start += batch {
		end := min(start+batch, len(a.Hosts))
		if len(a.Hosts) > batch {
			fmt.Fprintf(lw, "==> batch %d/%d\n", start/batch+1, (len(a.Hosts)+batch-1)/batch)
		}
		var wg sync.WaitGroup
		errs := make([]error, end-start)
		codes := make([]int, end-start)
		for i := start; i < end; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				h := a.Hosts[i]
				pw := &prefixWriter{w: lw, prefix: "[" + h.Name + "] "}
				st.set(ctx, i, model.StatusRunning, "")
				if h.Agent {
					codes[i-start], errs[i-start] = r.deployAgent(ctx, h, job, d, bundle, env, pw)
				} else {
					codes[i-start], errs[i-start] = deployHost(ctx, h, job, d, bundle, env, pw)
				}
				pw.Flush()
				if errs[i-start] != nil {
					fmt.Fprintf(lw, "==> [%s] failed: %v\n", h.Name, errs[i-start])
					st.set(ctx, i, model.StatusFailed, errs[i-start].Error())
				} else {
					fmt.Fprintf(lw, "==> [%s] done\n", h.Name)
					st.set(ctx, i, model.StatusSuccess, "")
				}
			}(i)
		}
		wg.Wait()
		for k, err := range errs {
			if err != nil {
				for i := end; i < len(a.Hosts); i++ {
					st.set(ctx, i, model.StatusSkipped, "")
				}
				if ctx.Err() != nil {
					return -1, ctx.Err()
				}
				return codes[k], fmt.Errorf("host %s: %w", a.Hosts[start+k].Name, err)
			}
		}
	}
	return 0, nil
}

// deployHost 在一台主机上：建目录、上传、依次执行步骤。
func deployHost(ctx context.Context, h protocol.DeployHost, job *model.Job, d *model.DeployTarget, bundle []byte,
	env map[string]string, out io.Writer) (int, error) {
	t := sshTarget(h)
	if h.Jump != nil {
		fmt.Fprintf(out, "connecting to %s@%s:%d via jump host %s@%s:%d\n", h.User, h.Address, h.Port, h.Jump.User, h.Jump.Address, h.Jump.Port)
	} else {
		fmt.Fprintf(out, "connecting to %s@%s:%d\n", h.User, h.Address, h.Port)
	}
	c, err := sshx.Dial(ctx, t)
	if err != nil {
		return -1, err
	}
	defer c.Close()
	if h.HostKey == "" {
		fmt.Fprintf(out, "warning: host key not verified (run a connection test to trust %s)\n", sshx.Fingerprint(c.HostKey))
	}
	target := sshx.Quote(d.Target)
	if bundle != nil {
		fmt.Fprintf(out, "uploading to %s\n", d.Target)
		if _, err := c.Run(ctx, "set -e; mkdir -p "+target+" && tar -xzf - -C "+target, bytes.NewReader(bundle), out); err != nil {
			return -1, fmt.Errorf("upload: %w", err)
		}
	}
	prefix := remotePrefix(env, h, d)
	for i, step := range job.Steps {
		fmt.Fprintf(out, "==> [%d/%d] %s\n", i+1, len(job.Steps), step.Name)
		script := prefix + exports(stepEnv(env, step.Env, nil)) + step.Run
		if code, err := c.Run(ctx, script, nil, out); err != nil {
			return code, fmt.Errorf("step %q: %w", step.Name, err)
		}
	}
	return 0, nil
}

func sshTarget(h protocol.DeployHost) sshx.Target {
	t := sshx.Target{Address: h.Address, Port: h.Port, User: h.User, Password: h.Password, PrivateKey: h.PrivateKey, HostKey: h.HostKey}
	if h.Jump != nil {
		j := sshTarget(*h.Jump)
		t.Jump = &j
	}
	return t
}

// remotePrefix 让远程脚本与本地一致：出错即停、进入目标目录，并提供部署相关变量。
func remotePrefix(env map[string]string, h protocol.DeployHost, d *model.DeployTarget) string {
	vars := map[string]string{"FLOW_DEPLOY_HOST": h.Name, "FLOW_DEPLOY_ADDRESS": h.Address, "FLOW_DEPLOY_DIR": d.Target}
	return "set -e\n" + exports(vars) + "mkdir -p " + sshx.Quote(d.Target) + " && cd " + sshx.Quote(d.Target) + "\n"
}

// exports 生成 export 语句（只导出合法变量名，按名称排序）。
func exports(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		if pipeline.ValidEnvName(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("export " + k + "=" + sshx.Quote(env[k]) + "\n")
	}
	return b.String()
}

// packUpload 打包工作目录中的 upload：目录上传其内容，文件上传其本身。
func packUpload(dir, upload string) ([]byte, error) {
	full := filepath.Join(dir, filepath.FromSlash(upload))
	info, err := os.Stat(full)
	if err != nil {
		return nil, fmt.Errorf("%s not found in workspace (is it an artifact of an upstream job?)", upload)
	}
	root, pattern := full, "."
	if !info.IsDir() {
		root, pattern = filepath.Dir(full), filepath.Base(full)
	}
	paths, err := collectPaths(root, []string{pattern})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeTarGz(&buf, root, paths); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// hostProgress 维护各主机状态并上报 Server（失败只记日志）。
type hostProgress struct {
	r     *Runner
	job   *model.Job
	mu    sync.Mutex
	hosts []model.HostState
}

func (p *hostProgress) set(ctx context.Context, i int, status model.Status, errMsg string) {
	p.mu.Lock()
	now := time.Now()
	h := &p.hosts[i]
	h.Status, h.Error = status, errMsg
	switch status {
	case model.StatusRunning:
		h.StartedAt = &now
	case model.StatusSuccess, model.StatusFailed:
		h.FinishedAt = &now
	}
	p.mu.Unlock()
	p.report(ctx)
}

func (p *hostProgress) report(ctx context.Context) {
	p.mu.Lock()
	req := protocol.HostsRequest{RunnerID: p.job.RunnerID, Hosts: append([]model.HostState(nil), p.hosts...)}
	p.mu.Unlock()
	if ctx.Err() != nil {
		return // 取消后由 Server 收尾
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.r.client.ReportHosts(ctx, p.job.ID, req); err != nil {
		log.Printf("job %s report hosts: %v", p.job.ID, err)
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// prefixWriter 给每行加上主机名前缀，整行写出，避免多台主机的输出交错在同一行。
type prefixWriter struct {
	w      io.Writer
	prefix string
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		if _, err := p.w.Write(append([]byte(p.prefix), p.buf[:i+1]...)); err != nil {
			return 0, err
		}
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
}

// Flush 写出最后不完整的一行。
func (p *prefixWriter) Flush() {
	if len(p.buf) > 0 {
		p.w.Write(append(append([]byte(p.prefix), p.buf...), '\n'))
		p.buf = nil
	}
}
