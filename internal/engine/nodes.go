package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"flow/internal/auth"
	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/store"
)

// 构建节点：自建节点（在机器上执行安装命令接入）、GitHub 节点（任务排队时触发 GitHub Actions 工作流，
// 工作流中的 flow-runner -once 领取一个任务后退出）、主机 Agent（部署任务直接在主机上执行，无需 SSH）。

const (
	NodeSelf   = "self"
	NodeGitHub = "github"
	NodeAgent  = "agent"

	// githubRedispatch：触发工作流后这么久仍未被领取，重新触发（工作流排队失败或被取消）。
	githubRedispatch = 10 * time.Minute
	hostTaskLease    = 60 * time.Second
)

// NodeInput 是构建节点的可编辑字段；GitHub 节点的 Token 为空表示保持不变。
type NodeInput struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Repo     string `json:"repo"`
	Workflow string `json:"workflow"`
	Ref      string `json:"ref"`
	Token    string `json:"token"`
}

var githubRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func runnerAAD(tenantID, id string) string { return "runner/" + tenantID + "/" + id }

func (e *Engine) applyNodeInput(r *model.Runner, in NodeInput, creating bool) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 64 || strings.ContainsAny(in.Name, " \t,") {
		return fmt.Errorf("%w: node name is required (max 64 chars, no spaces or commas)", ErrInvalid)
	}
	r.Name = in.Name
	if r.Kind != NodeGitHub {
		return nil
	}
	in.Repo, in.Workflow, in.Ref = strings.TrimSpace(in.Repo), strings.TrimSpace(in.Workflow), strings.TrimSpace(in.Ref)
	if !githubRepo.MatchString(in.Repo) {
		return fmt.Errorf("%w: repository must be owner/name", ErrInvalid)
	}
	if in.Workflow == "" {
		in.Workflow = "flow-runner.yml"
	}
	if in.Ref == "" {
		in.Ref = "main"
	}
	if creating && in.Token == "" {
		return fmt.Errorf("%w: a GitHub token with actions:write is required", ErrInvalid)
	}
	r.Config = map[string]string{"repo": in.Repo, "workflow": in.Workflow, "ref": in.Ref}
	if in.Token != "" {
		r.Secret = base64.StdEncoding.EncodeToString(e.cipher.Seal([]byte(strings.TrimSpace(in.Token)), runnerAAD(r.TenantID, r.ID)))
	}
	return nil
}

// CreateNode 创建自建或 GitHub 节点，返回只展示一次的 Runner Token。
func (e *Engine) CreateNode(ctx context.Context, tenantID string, in NodeInput) (*model.Runner, string, error) {
	if in.Kind == "" {
		in.Kind = NodeSelf
	}
	if in.Kind != NodeSelf && in.Kind != NodeGitHub {
		return nil, "", fmt.Errorf("%w: kind must be self or github", ErrInvalid)
	}
	plain, hash, _ := auth.NewToken(runnerTokenPrefix)
	r := &model.Runner{ID: newID(), TenantID: tenantID, Kind: in.Kind, TokenHash: hash, CreatedAt: e.now()}
	if err := e.applyNodeInput(r, in, true); err != nil {
		return nil, "", err
	}
	if err := e.store.CreateRunner(ctx, r); err != nil {
		return nil, "", nameConflict(err, "node", r.Name)
	}
	return r, plain, nil
}

func (e *Engine) UpdateNode(ctx context.Context, id string, in NodeInput) (*model.Runner, error) {
	r, err := e.store.GetRunner(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.Kind == NodeAgent {
		return nil, fmt.Errorf("%w: host agents are managed on the host page", ErrInvalid)
	}
	if err := e.applyNodeInput(r, in, false); err != nil {
		return nil, err
	}
	if err := e.store.UpdateRunner(ctx, r); err != nil {
		return nil, nameConflict(err, "node", r.Name)
	}
	return r, nil
}

// ResetNodeToken 重新生成节点 Token，旧 Token 立即失效。
func (e *Engine) ResetNodeToken(ctx context.Context, id string) (*model.Runner, string, error) {
	r, err := e.store.GetRunner(ctx, id)
	if err != nil {
		return nil, "", err
	}
	plain, hash, _ := auth.NewToken(runnerTokenPrefix)
	r.TokenHash = hash
	return r, plain, e.store.UpdateRunner(ctx, r)
}

// ---- GitHub 节点 ----

// DispatchGitHub 为排队等待 GitHub 节点的任务触发工作流，返回触发次数。
func (e *Engine) DispatchGitHub(ctx context.Context) (int, error) {
	nodes, err := e.store.ListRunnersByKind(ctx, NodeGitHub)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, r := range nodes {
		jobs, err := e.store.QueuedJobsWithLabel(ctx, r.TenantID, NodeLabel(r.Name), 20)
		if err != nil {
			return n, err
		}
		for _, j := range jobs {
			if j.DispatchedAt != nil && e.now().Sub(*j.DispatchedAt) < githubRedispatch {
				continue
			}
			err := e.githubDispatch(ctx, r, j)
			if err != nil {
				errs = append(errs, fmt.Errorf("node %s: %w", r.Name, err))
			}
			// 失败也记录时间，避免每轮重试刷屏；错误写入任务日志。
			now := e.now()
			uerr := e.store.Atomic(ctx, func(tx store.Tx) error {
				if _, err := tx.LockRun(ctx, j.RunID); err != nil {
					return err
				}
				cur, err := tx.GetJob(ctx, j.ID)
				if err != nil || cur.Status != model.StatusQueued {
					return err
				}
				cur.DispatchedAt = &now
				return tx.UpdateJob(ctx, cur)
			})
			if uerr != nil && !errors.Is(uerr, store.ErrNotFound) {
				return n, uerr
			}
			msg := fmt.Sprintf("==> dispatched GitHub workflow %s on %s@%s\n", r.Config["workflow"], r.Config["repo"], r.Config["ref"])
			if err != nil {
				msg = fmt.Sprintf("==> GitHub workflow dispatch failed: %v (retry in %s)\n", err, githubRedispatch)
			} else {
				n++
			}
			_ = e.appendLog(ctx, j.ID, []byte(msg))
		}
	}
	return n, errors.Join(errs...)
}

// GitHubAPI 是 GitHub REST API 地址（测试中替换）。
var GitHubAPI = "https://api.github.com"

func (e *Engine) githubDispatch(ctx context.Context, r *model.Runner, j *model.Job) error {
	sealed, err := base64.StdEncoding.DecodeString(r.Secret)
	if err != nil {
		return fmt.Errorf("decode token: %w", err)
	}
	tok, err := e.cipher.Open(sealed, runnerAAD(r.TenantID, r.ID))
	if err != nil {
		return fmt.Errorf("decrypt token: %w", err)
	}
	body, _ := json.Marshal(map[string]any{"ref": r.Config["ref"], "inputs": map[string]string{"job": j.ID}})
	u := fmt.Sprintf("%s/repos/%s/actions/workflows/%s/dispatches", GitHubAPI, r.Config["repo"], r.Config["workflow"])
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+string(tok))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// GitHubWorkflow 返回 GitHub 节点使用的工作流文件（放到仓库的 .github/workflows/ 下）。
func GitHubWorkflow(serverURL string) string {
	return `# Flow 构建节点：Flow 在任务排队时触发此工作流，flow-runner 领取一个任务执行后退出。
# 在仓库 Settings → Secrets and variables → Actions 中添加 FLOW_RUNNER_TOKEN（节点 Token）。
name: flow-runner
on:
  workflow_dispatch:
    inputs:
      job:
        description: Flow job id
        required: false
jobs:
  run:
    runs-on: ubuntu-latest
    timeout-minutes: 360
    steps:
      - name: Run Flow job ${{ inputs.job }}
        env:
          FLOW_SERVER: ` + serverURL + `
          FLOW_RUNNER_TOKEN: ${{ secrets.FLOW_RUNNER_TOKEN }}
        run: |
          curl -fsSL "$FLOW_SERVER/api/runner/download/linux/amd64" -o flow-runner
          chmod +x flow-runner
          ./flow-runner -once -wait 5m -concurrency 1
`
}

// ---- 主机 Agent ----

// HostAgentToken 为 Agent 接入的主机生成（或重置）Agent Token；Agent 以 kind=agent 的 Runner 身份连接。
func (e *Engine) HostAgentToken(ctx context.Context, hostID string) (string, error) {
	h, err := e.store.GetHost(ctx, hostID)
	if err != nil {
		return "", err
	}
	if h.Mode != model.HostModeAgent {
		return "", fmt.Errorf("%w: host %s is not connected through an agent", ErrInvalid, h.Name)
	}
	plain, hash, _ := auth.NewToken(runnerTokenPrefix)
	if r, err := e.hostAgent(ctx, h); err == nil {
		r.TokenHash = hash
		return plain, e.store.UpdateRunner(ctx, r)
	}
	r := &model.Runner{ID: newID(), TenantID: h.TenantID, Name: "agent-" + h.ID, Kind: NodeAgent, HostID: h.ID, TokenHash: hash, CreatedAt: e.now()}
	return plain, e.store.CreateRunner(ctx, r)
}

func (e *Engine) hostAgent(ctx context.Context, h *model.Host) (*model.Runner, error) {
	list, err := e.store.ListRunners(ctx, h.TenantID)
	if err != nil {
		return nil, err
	}
	for _, r := range list {
		if r.Kind == NodeAgent && r.HostID == h.ID {
			return r, nil
		}
	}
	return nil, store.ErrNotFound
}

// withAgents 为 Agent 主机填入 Agent 的在线时间与环境信息。
func (e *Engine) withAgents(ctx context.Context, tenantID string, hosts []*model.Host) {
	list, err := e.store.ListRunners(ctx, tenantID)
	if err != nil {
		return
	}
	for _, h := range hosts {
		for _, r := range list {
			if r.Kind == NodeAgent && r.HostID == h.ID {
				h.AgentSeenAt, h.AgentInfo = r.LastSeenAt, &r.Info
			}
		}
	}
}

// agentOnline：Agent 每次轮询都会上报，超过此时长未上报视为离线。
const agentOnline = time.Minute

// CreateHostTask 由执行部署任务的 Runner 调用：把一台 Agent 主机的部署脚本排入队列。
func (e *Engine) CreateHostTask(ctx context.Context, jobID, runnerID string, in protocol.HostTaskRequest) (*model.HostTask, error) {
	j, err := e.leasedJob(ctx, jobID, runnerID)
	if err != nil {
		return nil, err
	}
	hosts, err := e.store.ListHosts(ctx, j.TenantID)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(hosts, func(h *model.Host) bool { return h.Name == in.Host && h.Mode == model.HostModeAgent })
	if i < 0 {
		return nil, fmt.Errorf("%w: agent host %q not found", ErrInvalid, in.Host)
	}
	h := hosts[i]
	t := &model.HostTask{ID: newID(), TenantID: j.TenantID, JobID: j.ID, HostID: h.ID, Script: in.Script, Env: in.Env,
		Target: in.Target, Status: model.StatusQueued, CreatedAt: e.now()}
	if len(in.Bundle) > 0 {
		t.UploadKey = fmt.Sprintf("hosttasks/%s/%s.tar.gz", j.TenantID, t.ID)
		if err := e.blobs.Put(ctx, t.UploadKey, bytes.NewReader(in.Bundle), int64(len(in.Bundle))); err != nil {
			return nil, err
		}
	}
	return t, e.store.CreateHostTask(ctx, t)
}

// HostTaskProgressOf 返回任务状态与 offset 之后的日志；cancel 为 true 时先取消任务（部署任务被取消）。
func (e *Engine) HostTaskProgressOf(ctx context.Context, taskID, runnerID string, offset int, cancel bool) (*protocol.HostTaskProgress, error) {
	t, err := e.ownedHostTask(ctx, taskID, runnerID)
	if err != nil {
		return nil, err
	}
	if cancel && t.FinishedAt == nil {
		if err := e.store.FinishHostTask(ctx, t.ID, model.StatusCanceled, -1, "canceled", e.now()); err != nil {
			return nil, err
		}
		t.Status = model.StatusCanceled
	}
	data, err := e.store.ReadHostTaskLog(ctx, t.ID, offset)
	if err != nil {
		return nil, err
	}
	p := &protocol.HostTaskProgress{Status: t.Status, ExitCode: t.ExitCode, Error: t.Error, Log: data}
	if h, err := e.store.GetHost(ctx, t.HostID); err == nil {
		if r, err := e.hostAgent(ctx, h); err == nil && r.LastSeenAt != nil {
			p.Online = e.now().Sub(*r.LastSeenAt) < agentOnline
		}
	}
	if t.FinishedAt != nil && t.UploadKey != "" {
		_ = e.blobs.Delete(ctx, t.UploadKey)
	}
	return p, nil
}

// ownedHostTask 校验任务属于该 Runner 正在执行的部署任务。
func (e *Engine) ownedHostTask(ctx context.Context, taskID, runnerID string) (*model.HostTask, error) {
	t, err := e.store.GetHostTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	j, err := e.store.GetJob(ctx, t.JobID)
	if err != nil {
		return nil, err
	}
	if j.RunnerID != runnerID {
		return nil, ErrLeaseLost
	}
	return t, nil
}

// AgentClaim 为主机 Agent 领取一个部署任务；无任务返回 nil, nil。
func (e *Engine) AgentClaim(ctx context.Context, r RunnerInfo) (*model.HostTask, error) {
	if r.Kind != NodeAgent || r.HostID == "" {
		return nil, ErrUnauthorized
	}
	now := e.now()
	return e.store.ClaimHostTask(ctx, r.HostID, now, now.Add(hostTaskLease))
}

func (e *Engine) agentTask(ctx context.Context, r RunnerInfo, taskID string) (*model.HostTask, error) {
	t, err := e.store.GetHostTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if r.Kind != NodeAgent || t.HostID != r.HostID {
		return nil, store.ErrNotFound
	}
	return t, nil
}

// AgentLog 追加日志并续租；返回 cancel 表示任务已被取消，Agent 应停止执行。
func (e *Engine) AgentLog(ctx context.Context, r RunnerInfo, taskID string, data []byte) (cancel bool, err error) {
	t, err := e.agentTask(ctx, r, taskID)
	if err != nil {
		return false, err
	}
	if t.Status != model.StatusRunning {
		return true, nil
	}
	if len(data) > 0 {
		if err := e.store.AppendHostTaskLog(ctx, t.ID, data); err != nil {
			return false, err
		}
	}
	if err := e.store.TouchHostTask(ctx, t.ID, e.now().Add(hostTaskLease)); err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	return false, nil
}

func (e *Engine) AgentFinish(ctx context.Context, r RunnerInfo, taskID string, exitCode int, errMsg string) error {
	t, err := e.agentTask(ctx, r, taskID)
	if err != nil {
		return err
	}
	status := model.StatusSuccess
	if exitCode != 0 || errMsg != "" {
		status = model.StatusFailed
	}
	return e.store.FinishHostTask(ctx, t.ID, status, exitCode, errMsg, e.now())
}

// AgentUpload 打开任务的上传内容（tar.gz）。
func (e *Engine) AgentUpload(ctx context.Context, r RunnerInfo, taskID string) (io.ReadCloser, error) {
	t, err := e.agentTask(ctx, r, taskID)
	if err != nil {
		return nil, err
	}
	if t.UploadKey == "" {
		return nil, store.ErrNotFound
	}
	return e.blobs.Open(ctx, t.UploadKey)
}

// ExpireHostTasks 把 Agent 失联的任务置为失败。
func (e *Engine) ExpireHostTasks(ctx context.Context) (int, error) {
	return e.store.ExpireHostTasks(ctx, e.now())
}
