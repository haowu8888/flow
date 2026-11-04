package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"flow/internal/auth"
	"flow/internal/blob"
	"flow/internal/gitstatus"
	"flow/internal/model"
	"flow/internal/notify"
	"flow/internal/pipeline"
	"flow/internal/store"
)

const (
	maxSecretBytes     = 64 << 10
	runnerTokenPrefix  = "fr_"
	runnerSeenInterval = 15 * time.Second
)

// ---- 密钥 ----

func secretAAD(tenantID, pipelineID, name string) string {
	return tenantID + "/" + pipelineID + "/" + name
}

// PutSecret 新建或覆盖密钥；pipelineID 为空表示租户级。值只写不读。
func (e *Engine) PutSecret(ctx context.Context, tenantID, pipelineID, name, value string) (*model.Secret, error) {
	if !pipeline.ValidEnvName(name) {
		return nil, fmt.Errorf("%w: secret name must be a valid variable name (letters, digits, _)", ErrInvalid)
	}
	if strings.HasPrefix(strings.ToUpper(name), "FLOW_") {
		return nil, fmt.Errorf("%w: the FLOW_ prefix is reserved for built-in variables", ErrInvalid)
	}
	if value == "" || len(value) > maxSecretBytes || !utf8.ValidString(value) {
		return nil, fmt.Errorf("%w: secret value must be non-empty UTF-8 text up to 64KB", ErrInvalid)
	}
	now := e.now()
	s := &model.Secret{
		ID: newID(), TenantID: tenantID, PipelineID: pipelineID, Name: name,
		Value: e.cipher.Seal([]byte(value), secretAAD(tenantID, pipelineID, name)), CreatedAt: now, UpdatedAt: now,
	}
	if err := e.store.PutSecret(ctx, s); err != nil {
		return nil, err
	}
	s.Value = nil
	return s, nil
}

func (e *Engine) DeleteSecret(ctx context.Context, tenantID, pipelineID, name string) error {
	return e.store.DeleteSecret(ctx, tenantID, pipelineID, name)
}

// Secrets 列出某一作用域的密钥（不含值）。
func (e *Engine) Secrets(ctx context.Context, tenantID, pipelineID string) ([]*model.Secret, error) {
	list, err := e.store.ListSecrets(ctx, tenantID, pipelineID)
	for _, s := range list {
		s.Value = nil
	}
	return list, err
}

// resolveSecrets 解密租户级与流水线级密钥，同名时流水线级优先。
// group 为通用变量组中的私密变量，优先级介于租户级与流水线级密钥之间。
func (e *Engine) resolveSecrets(ctx context.Context, tenantID, pipelineID string, group map[string]string) (map[string]string, error) {
	out := map[string]string{}
	var errs []error
	for _, scope := range []string{"", pipelineID} {
		if scope == pipelineID {
			maps.Copy(out, group)
		}
		list, err := e.store.ListSecrets(ctx, tenantID, scope)
		if err != nil {
			return out, err
		}
		for _, s := range list {
			plain, err := e.cipher.Open(s.Value, secretAAD(s.TenantID, s.PipelineID, s.Name))
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", s.Name, err))
				continue
			}
			out[s.Name] = string(plain)
		}
	}
	return out, errors.Join(errs...)
}

// ---- Runner ----

// RegisterRunner 创建租户专属 Runner，返回只展示一次的 Token。
func (e *Engine) RegisterRunner(ctx context.Context, tenantID, name string) (*model.Runner, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return nil, "", fmt.Errorf("%w: runner name is required (max 64 chars)", ErrInvalid)
	}
	plain, hash, _ := auth.NewToken(runnerTokenPrefix)
	r := &model.Runner{ID: newID(), TenantID: tenantID, Name: name, TokenHash: hash, CreatedAt: e.now()}
	if err := e.store.CreateRunner(ctx, r); err != nil {
		return nil, "", err
	}
	return r, plain, nil
}

func (e *Engine) Runners(ctx context.Context, tenantID string) ([]*model.Runner, error) {
	return e.store.ListRunners(ctx, tenantID)
}

func (e *Engine) RunnerByID(ctx context.Context, id string) (*model.Runner, error) {
	return e.store.GetRunner(ctx, id)
}

func (e *Engine) DeleteRunner(ctx context.Context, id string) error {
	return e.store.DeleteRunner(ctx, id)
}

// AuthenticateRunner 按独立 Token 识别 Runner；Runner 的标识固定为其注册 ID，不能冒用他人租约。
func (e *Engine) AuthenticateRunner(ctx context.Context, token string) (*RunnerInfo, error) {
	if !strings.HasPrefix(token, runnerTokenPrefix) {
		return nil, store.ErrNotFound
	}
	r, err := e.store.GetRunnerByTokenHash(ctx, auth.HashToken(token))
	if err != nil {
		return nil, err
	}
	return &RunnerInfo{ID: r.ID, TenantID: r.TenantID, Kind: r.Kind, HostID: r.HostID}, nil
}

// RunnerSeen 记录注册 Runner 的在线时间与上报标签（每个进程每 15s 最多写一次）。
func (e *Engine) RunnerSeen(ctx context.Context, r RunnerInfo) {
	if r.TenantID == "" {
		return
	}
	now := e.now()
	if last, ok := e.runnerSeen.Load(r.ID); ok && now.Sub(last.(time.Time)) < runnerSeenInterval {
		return
	}
	e.runnerSeen.Store(r.ID, now)
	if err := e.store.TouchRunner(ctx, r.ID, r.Labels, r.Info, now); err != nil && !errors.Is(err, store.ErrNotFound) {
		e.logf("touch runner %s: %v", r.ID, err)
	}
}

// ---- 产物 ----

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// UploadArtifact 保存运行中 Job 上传的产物归档（tar.gz），重试时覆盖旧归档。
func (e *Engine) UploadArtifact(ctx context.Context, jobID, runnerID string, r io.Reader) (*model.Artifact, error) {
	return e.UploadNamedArtifact(ctx, jobID, runnerID, ArtifactMeta{}, r)
}

// ArtifactMeta 描述「构建物上传」步骤上传的命名构建物；Name 为空时是任务的默认归档。
type ArtifactMeta struct {
	Name    string
	Version string // 为空时自动递增为 vN
	Format  string // zip / tar.gz
}

var artifactNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// UploadNamedArtifact 上传构建物；同一任务同名的构建物（如重试）覆盖之前的。
func (e *Engine) UploadNamedArtifact(ctx context.Context, jobID, runnerID string, meta ArtifactMeta, r io.Reader) (*model.Artifact, error) {
	j, err := e.leasedJob(ctx, jobID, runnerID)
	if err != nil {
		return nil, err
	}
	if meta.Format == "" {
		meta.Format = "tar.gz"
	}
	if meta.Format != "zip" && meta.Format != "tar.gz" {
		return nil, fmt.Errorf("%w: artifact format must be zip or tar.gz", ErrInvalid)
	}
	if meta.Name != "" {
		if !artifactNameRE.MatchString(meta.Name) {
			return nil, fmt.Errorf("%w: invalid artifact name %q (letters, digits, . _ -)", ErrInvalid, meta.Name)
		}
		if len(meta.Version) > 64 {
			return nil, fmt.Errorf("%w: artifact version is too long", ErrInvalid)
		}
		if meta.Version == "" {
			run, err := e.store.GetRun(ctx, j.RunID)
			if err != nil {
				return nil, err
			}
			n, err := e.store.NextArtifactVersion(ctx, run.PipelineID, meta.Name)
			if err != nil {
				return nil, err
			}
			meta.Version = fmt.Sprintf("v%d", n)
		}
	}
	key := fmt.Sprintf("artifacts/%s/%s/%s.%s", j.TenantID, j.RunID, newID(), meta.Format)
	cr := &countingReader{r: r}
	if err := e.blobs.Put(ctx, key, cr, -1); err != nil {
		return nil, err
	}
	now := e.now()
	a := &model.Artifact{ID: newID(), TenantID: j.TenantID, RunID: j.RunID, JobID: j.ID, JobName: j.Name,
		Size: cr.n, BlobKey: key, CreatedAt: now, Name: meta.Name, Version: meta.Version, Format: meta.Format}
	ttl := e.artifactTTL
	switch {
	case j.ArtifactTTLSec < 0:
		ttl = 0
	case j.ArtifactTTLSec > 0:
		ttl = time.Duration(j.ArtifactTTLSec) * time.Second
	}
	if ttl > 0 {
		exp := now.Add(ttl)
		a.ExpiresAt = &exp
	}
	old, err := e.store.PutArtifact(ctx, a)
	if err != nil {
		_ = e.blobs.Delete(ctx, key)
		return nil, err
	}
	if old != "" {
		_ = e.blobs.Delete(ctx, old)
	}
	e.runChanged(j.RunID)
	return a, nil
}

func (e *Engine) Artifacts(ctx context.Context, runID string) ([]*model.Artifact, error) {
	return e.store.ListArtifacts(ctx, runID)
}

func (e *Engine) Artifact(ctx context.Context, id string) (*model.Artifact, error) {
	return e.store.GetArtifact(ctx, id)
}

func (e *Engine) OpenArtifact(ctx context.Context, a *model.Artifact) (io.ReadCloser, error) {
	rc, err := e.blobs.Open(ctx, a.BlobKey)
	if errors.Is(err, blob.ErrNotFound) {
		return nil, store.ErrNotFound
	}
	return rc, err
}

// ---- 通知 ----

var secretRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandSecrets(s string, secrets map[string]string) (string, error) {
	var missing []string
	out := secretRef.ReplaceAllStringFunc(s, func(m string) string {
		name := secretRef.FindStringSubmatch(m)[1]
		v, ok := secrets[name]
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("secret %s not found", strings.Join(missing, ", "))
	}
	return out, nil
}

// DeliverNotifications 发送已结束运行的通知，返回处理的 Run 数。
// 在事务中锁住 Run 再发送，多实例不会重复发送；发送失败只记录日志不重试，避免刷屏。
func (e *Engine) DeliverNotifications(ctx context.Context) (int, error) {
	n := 0
	for range 20 {
		var done bool
		err := e.store.Atomic(ctx, func(tx store.Tx) error {
			done = false
			run, err := tx.NextPendingNotify(ctx)
			if err != nil || run == nil {
				done = true
				return err
			}
			e.sendRunNotifications(ctx, tx, run)
			run.NotifyDone = true
			return tx.UpdateRun(ctx, run)
		})
		if err != nil {
			return n, err
		}
		if done {
			return n, nil
		}
		n++
	}
	return n, nil
}

func (e *Engine) sendRunNotifications(ctx context.Context, tx store.Tx, run *model.Run) {
	p, err := tx.GetPipeline(ctx, run.PipelineID)
	if err != nil {
		e.logf("notify run %s: %v", run.ID, err)
		return
	}
	jobs, err := tx.ListJobs(ctx, run.ID)
	if err != nil {
		e.logf("notify run %s: %v", run.ID, err)
		return
	}
	msg := notify.Message{Pipeline: p.Name, Number: run.Number, Status: string(run.Status),
		Trigger: triggerText(run), Duration: durationText(run)}
	for _, j := range jobs {
		if j.Status == model.StatusFailed {
			msg.Failed = append(msg.Failed, j.Name)
		}
	}
	if e.publicURL != "" {
		msg.URL = strings.TrimRight(e.publicURL, "/") + "/#/runs/" + run.ID
	}
	secrets, err := e.resolveSecrets(ctx, run.TenantID, run.PipelineID, nil)
	if err != nil {
		e.logf("notify run %s: resolve secrets: %v", run.ID, err)
	}
	for _, t := range run.Notify {
		if !slices.Contains(t.On, run.Status) {
			continue
		}
		if provider, ok := strings.CutPrefix(t.Type, commitStatusType); ok {
			state := map[model.Status]gitstatus.State{model.StatusSuccess: gitstatus.Success, model.StatusFailed: gitstatus.Failed}[run.Status]
			if state == "" {
				state = gitstatus.Canceled
			}
			desc := fmt.Sprintf("#%d %s，耗时 %s", run.Number, statusWord[run.Status], durationText(run))
			if len(msg.Failed) > 0 {
				desc += "，失败任务：" + strings.Join(msg.Failed, "、")
			}
			e.postCommitStatus(ctx, run, p.Name, provider, t, secrets, state, desc)
			continue
		}
		url, err1 := expandSecrets(t.URL, secrets)
		sec, err2 := expandSecrets(t.Secret, secrets)
		if err := errors.Join(err1, err2); err != nil {
			e.logf("notify run %s via %s: %v", run.ID, t.Type, err)
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := e.sender.Send(sctx, notify.Target{Type: t.Type, URL: url, Secret: sec}, msg); err != nil {
			e.logf("notify run %s via %s: %v", run.ID, t.Type, err)
		}
		cancel()
	}
}

func triggerText(r *model.Run) string {
	switch r.Trigger {
	case "manual":
		if r.CreatedBy != "" {
			return "手动（" + r.CreatedBy + "）"
		}
		return "手动"
	case "cron":
		return "定时"
	case "webhook":
		ref := r.Env["FLOW_GIT_BRANCH"]
		if ref == "" {
			ref = r.Env["FLOW_GIT_TAG"]
		}
		commit := r.Env["FLOW_GIT_COMMIT"]
		if len(commit) > 8 {
			commit = commit[:8]
		}
		return strings.TrimSpace("代码提交 " + ref + " " + commit)
	}
	return r.Trigger
}

func durationText(r *model.Run) string {
	if r.StartedAt == nil || r.FinishedAt == nil {
		return "-"
	}
	return r.FinishedAt.Sub(*r.StartedAt).Round(time.Second).String()
}

var statusWord = map[model.Status]string{model.StatusSuccess: "成功", model.StatusFailed: "失败", model.StatusCanceled: "已取消"}

// postCommitStatus 把运行状态写到代码提交上；失败只记录日志。
func (e *Engine) postCommitStatus(ctx context.Context, run *model.Run, pipelineName, provider string, t model.NotifyTarget,
	secrets map[string]string, state gitstatus.State, desc string) {
	if run.Commit == "" {
		return
	}
	token, err := expandSecrets(t.Secret, secrets)
	if err == nil && token == "" {
		err = errors.New("token is empty")
	}
	var repo *gitstatus.Repo
	if err == nil {
		repo, err = gitstatus.Parse(t.URL, provider)
	}
	if err == nil {
		st := gitstatus.Status{State: state, Context: "flow/" + pipelineName, Description: desc}
		if e.publicURL != "" {
			st.TargetURL = strings.TrimRight(e.publicURL, "/") + "/#/runs/" + run.ID
		}
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = e.gitStatus.Post(sctx, repo, token, run.Commit, st)
		cancel()
	}
	if err != nil {
		e.logf("commit status for run %s: %v", run.ID, err)
	}
}

// reportRunning 在运行开始执行且提交已知时，把提交状态置为进行中。
func (e *Engine) reportRunning(run *model.Run) {
	i := slices.IndexFunc(run.Notify, func(t model.NotifyTarget) bool { return strings.HasPrefix(t.Type, commitStatusType) })
	if i < 0 || run.Commit == "" {
		return
	}
	ctx := context.Background()
	p, err := e.store.GetPipeline(ctx, run.PipelineID)
	if err != nil {
		e.logf("commit status for run %s: %v", run.ID, err)
		return
	}
	secrets, err := e.resolveSecrets(ctx, run.TenantID, run.PipelineID, nil)
	if err != nil {
		e.logf("commit status for run %s: resolve secrets: %v", run.ID, err)
	}
	t := run.Notify[i]
	e.postCommitStatus(ctx, run, p.Name, strings.TrimPrefix(t.Type, commitStatusType), t, secrets, gitstatus.Running, fmt.Sprintf("#%d 运行中", run.Number))
}

const maxReportCases = 2000

// SaveTestReport 保存 Job 上传的测试报告（覆盖之前的执行）。
func (e *Engine) SaveTestReport(ctx context.Context, jobID, runnerID string, sum model.TestSummary, cases []model.TestCase) error {
	j, err := e.leasedJob(ctx, jobID, runnerID)
	if err != nil {
		return err
	}
	if len(cases) > maxReportCases {
		cases = cases[:maxReportCases]
	}
	if sum.Total < 0 || sum.Passed+sum.Failed+sum.Skipped != sum.Total {
		return fmt.Errorf("%w: inconsistent test summary", ErrInvalid)
	}
	r := &model.TestReport{JobID: j.ID, RunID: j.RunID, TenantID: j.TenantID, JobName: j.Name, TestSummary: sum, Cases: cases, CreatedAt: e.now()}
	if err := e.store.PutTestReport(ctx, r); err != nil {
		return err
	}
	e.runChanged(j.RunID)
	return nil
}

// TestReports 返回运行中各 Job 的测试报告。
func (e *Engine) TestReports(ctx context.Context, runID string) ([]*model.TestReport, error) {
	return e.store.ListTestReports(ctx, runID)
}

// RecordAudit 写入审计事件；失败只记录日志，不影响操作本身。
func (e *Engine) RecordAudit(ctx context.Context, ev *model.AuditEvent) {
	ev.ID, ev.CreatedAt = newID(), e.now()
	if err := e.store.CreateAuditEvent(ctx, ev); err != nil {
		e.logf("audit %s by %s: %v", ev.Action, ev.Actor, err)
	}
}

// AuditEvents 按时间倒序查询审计日志。
func (e *Engine) AuditEvents(ctx context.Context, tenantID string, f store.AuditFilter) ([]*model.AuditEvent, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	return e.store.ListAuditEvents(ctx, tenantID, f)
}

// auditRetention 是审计日志的保留期。
const auditRetention = 365 * 24 * time.Hour

// Approval 是一个等待审批的任务及其上下文。
type Approval struct {
	Job          *model.Job `json:"job"`
	Run          *model.Run `json:"run"`
	PipelineName string     `json:"pipeline_name"`
	Environment  string     `json:"environment,omitempty"` // 部署审批的目标环境
}

// PendingApprovals 返回 username 可以审批的等待中任务。
func (e *Engine) PendingApprovals(ctx context.Context, tenantID, username string, isAdmin bool) ([]Approval, error) {
	jobs, err := e.store.WaitingJobs(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := []Approval{}
	names := map[string]string{}
	for _, j := range jobs {
		if len(j.Approvers) > 0 && !slices.Contains(j.Approvers, username) && !isAdmin {
			continue
		}
		run, err := e.store.GetRun(ctx, j.RunID)
		if err != nil {
			continue
		}
		if _, ok := names[run.PipelineID]; !ok {
			if p, err := e.store.GetPipeline(ctx, run.PipelineID); err == nil {
				names[run.PipelineID] = p.Name
			}
		}
		a := Approval{Job: j, Run: run, PipelineName: names[run.PipelineID]}
		if j.Deploy != nil {
			if g, err := e.store.GetHostGroupByName(ctx, j.TenantID, j.Deploy.Group); err == nil && g.EnvironmentID != "" {
				if env, err := e.store.GetEnvironment(ctx, g.EnvironmentID); err == nil {
					a.Environment = env.Name
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// QueryRuns 按条件查询流水线的运行。
func (e *Engine) QueryRuns(ctx context.Context, pipelineID string, f store.RunFilter) ([]*model.Run, error) {
	return e.store.QueryRuns(ctx, pipelineID, f)
}
