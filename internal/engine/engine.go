// Package engine 实现调度核心：Run 实例化、DAG 推进、Runner 租约与取消、人工卡点、重试、通知。
//
// 并发模型：每个状态迁移都在 store.Atomic 事务中执行，修改某个 Run 的 Job 前先锁住该 Run 行，
// 因此可以多实例部署（PostgreSQL），无需进程内锁。
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"flow/internal/blob"
	"flow/internal/gitstatus"
	"flow/internal/model"
	"flow/internal/notify"
	"flow/internal/pipeline"
	"flow/internal/protocol"
	"flow/internal/secret"
	"flow/internal/store"
	"flow/internal/trigger"
)

var (
	ErrInvalid = errors.New("invalid request")
	// ErrInTrash：流水线在回收站中，恢复后才能保存或运行。
	ErrInTrash   = fmt.Errorf("%w: pipeline is in the recycle bin; restore it first", ErrInvalid)
	ErrLeaseLost = errors.New("job is not leased by this runner")
	ErrForbidden = errors.New("permission denied")
)

// DefaultTenant 在未指定租户时使用（由迁移脚本预置）。
const DefaultTenant = "default"

const defaultCloneDepth = 50

type Engine struct {
	store     store.Store
	now       func() time.Time
	leaseTTL  time.Duration
	cipher    *secret.Cipher
	blobs     blob.Store
	sender    *notify.Sender
	gitStatus *gitstatus.Client
	publicURL string
	logf      func(format string, args ...any)
	// artifactTTL 是产物默认保留时长，0 为永久保留；流水线 retention.artifacts 可覆盖。
	artifactTTL time.Duration

	runnerSeen sync.Map // runner id -> time.Time，限制 last_seen 写入频率
	hub        hub      // 日志 / 状态变更广播，供 SSE 推送
}

type Option func(*Engine)

// WithCipher 设置密钥加密主密钥；未设置时使用进程内随机密钥（重启后无法解密）。
func WithCipher(c *secret.Cipher) Option { return func(e *Engine) { e.cipher = c } }

// WithBlobs 设置产物存储；未设置时存于内存。
func WithBlobs(b blob.Store) Option { return func(e *Engine) { e.blobs = b } }

// WithPublicURL 设置界面访问地址，用于通知中的详情链接。
func WithPublicURL(u string) Option { return func(e *Engine) { e.publicURL = u } }

func WithSender(s *notify.Sender) Option { return func(e *Engine) { e.sender = s } }

// WithArtifactRetention 设置产物默认保留时长，0 为永久保留。
func WithArtifactRetention(d time.Duration) Option { return func(e *Engine) { e.artifactTTL = d } }

func New(s store.Store, leaseTTL time.Duration, opts ...Option) *Engine {
	e := &Engine{store: s, now: time.Now, leaseTTL: leaseTTL, logf: log.Printf}
	for _, o := range opts {
		o(e)
	}
	if e.cipher == nil {
		e.cipher = secret.NewRandomCipher()
	}
	if e.blobs == nil {
		e.blobs = blob.NewMemory()
	}
	if e.gitStatus == nil {
		e.gitStatus = &gitstatus.Client{HTTP: &http.Client{Timeout: 10 * time.Second}}
	}
	if e.sender == nil {
		e.sender = notify.NewSender()
	}
	return e
}

// CreatePipeline 校验并保存新流水线（版本 1）；user 为保存者用户名。
func (e *Engine) CreatePipeline(ctx context.Context, tenantID, yamlText, user string) (*model.Pipeline, error) {
	spec, err := pipeline.Parse([]byte(yamlText))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := e.expandTemplates(ctx, e.store, tenantID, spec); err != nil {
		return nil, err
	}
	if err := checkHostGroups(ctx, e.store.GetHostGroupByName, tenantID, spec); err != nil {
		return nil, err
	}
	if err := checkKinds(spec); err != nil {
		return nil, err
	}
	if err := e.checkSaveConnections(ctx, tenantID, spec, user); err != nil {
		return nil, err
	}
	if _, err := e.runVarGroups(ctx, tenantID, spec.VarGroups); err != nil {
		return nil, err
	}
	now := e.now()
	p := &model.Pipeline{ID: newID(), TenantID: tenantID, Name: spec.Name, YAML: yamlText, UpdatedBy: user, CreatedBy: user, CreatedAt: now, UpdatedAt: now}
	if err := e.store.CreatePipeline(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// ValidatePipeline 按创建 / 更新时的规则校验 YAML（含步骤模板引用），不保存。
func (e *Engine) ValidatePipeline(ctx context.Context, tenantID, yamlText string) error {
	spec, err := pipeline.Parse([]byte(yamlText))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := e.expandTemplates(ctx, e.store, tenantID, spec); err != nil {
		return err
	}
	if err := checkHostGroups(ctx, e.store.GetHostGroupByName, tenantID, spec); err != nil {
		return err
	}
	if _, err := e.runVarGroups(ctx, tenantID, spec.VarGroups); err != nil {
		return err
	}
	if err := checkKinds(spec); err != nil {
		return err
	}
	return e.checkConnections(ctx, tenantID, spec, "", false)
}

// UpdatePipeline 保存新版本。baseVersion 为编辑起点的版本号（0 表示不检查）：
// 期间已被他人保存时返回 ErrConflict，避免静默覆盖。
func (e *Engine) UpdatePipeline(ctx context.Context, id, yamlText, user string, baseVersion int) (*model.Pipeline, error) {
	return e.savePipeline(ctx, id, yamlText, user, baseVersion, 0)
}

// RestoreRevision 把流水线回退到历史版本 version：以该版本的定义保存为新版本（历史不删除），立即生效。
// baseVersion 含义同 UpdatePipeline；内容与当前版本相同时不产生新版本。
// 该版本引用的步骤模板已删除等导致校验失败时返回 ErrInvalid。
func (e *Engine) RestoreRevision(ctx context.Context, id string, version int, user string, baseVersion int) (*model.Pipeline, error) {
	rev, err := e.store.GetRevision(ctx, id, version)
	if err != nil {
		return nil, err
	}
	p, err := e.savePipeline(ctx, id, rev.YAML, user, baseVersion, version)
	if errors.Is(err, ErrInvalid) {
		return nil, fmt.Errorf("version %d no longer passes validation: %w", version, err)
	}
	return p, err
}

func (e *Engine) savePipeline(ctx context.Context, id, yamlText, user string, baseVersion, restoredFrom int) (*model.Pipeline, error) {
	spec, err := pipeline.Parse([]byte(yamlText))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	p, err := e.store.GetPipeline(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.DeletedAt != nil {
		return nil, ErrInTrash
	}
	if baseVersion != 0 && baseVersion != p.Version {
		return nil, conflictErr(p)
	}
	if err := e.expandTemplates(ctx, e.store, p.TenantID, spec); err != nil {
		return nil, err
	}
	if err := checkHostGroups(ctx, e.store.GetHostGroupByName, p.TenantID, spec); err != nil {
		return nil, err
	}
	if err := checkKinds(spec); err != nil {
		return nil, err
	}
	if err := e.checkSaveConnections(ctx, p.TenantID, spec, user); err != nil {
		return nil, err
	}
	if _, err := e.runVarGroups(ctx, p.TenantID, spec.VarGroups); err != nil {
		return nil, err
	}
	if p.Name == spec.Name && p.YAML == yamlText {
		return p, nil // 无变化不产生新版本
	}
	p.Name, p.YAML, p.UpdatedBy, p.UpdatedAt, p.RestoredFrom = spec.Name, yamlText, user, e.now(), restoredFrom
	if err := e.store.UpdatePipeline(ctx, p); err != nil {
		if errors.Is(err, store.ErrConflict) {
			if cur, gerr := e.store.GetPipeline(ctx, id); gerr == nil && cur.Version != p.Version {
				return nil, conflictErr(cur)
			}
			return nil, fmt.Errorf("%w: a pipeline named %q already exists", store.ErrConflict, spec.Name)
		}
		return nil, err
	}
	return p, nil
}

func conflictErr(cur *model.Pipeline) error {
	by := cur.UpdatedBy
	if by == "" {
		by = "someone else"
	}
	return fmt.Errorf("%w: pipeline was saved by %s as version %d after you started editing; reload to see the changes",
		store.ErrConflict, by, cur.Version)
}

// Revisions 按版本倒序返回流水线的保存记录（不含 YAML）。
func (e *Engine) Revisions(ctx context.Context, pipelineID string, limit int) ([]*model.PipelineRevision, error) {
	return e.store.ListRevisions(ctx, pipelineID, limit)
}

func (e *Engine) Revision(ctx context.Context, pipelineID string, version int) (*model.PipelineRevision, error) {
	return e.store.GetRevision(ctx, pipelineID, version)
}

// TrashPipeline 把流水线移入回收站：运行记录、产物、触发器与密钥都保留，但不再出现在列表中，
// 触发器不再触发、不能保存或运行；RestorePipeline 恢复，PurgePipeline 彻底删除。
func (e *Engine) TrashPipeline(ctx context.Context, id string) error {
	now := e.now()
	return e.store.SetPipelineDeleted(ctx, id, &now)
}

func (e *Engine) RestorePipeline(ctx context.Context, id string) error {
	return e.store.SetPipelineDeleted(ctx, id, nil)
}

func (e *Engine) TrashedPipelines(ctx context.Context, tenantID string) ([]*model.Pipeline, error) {
	return e.store.ListDeletedPipelines(ctx, tenantID)
}

// PurgePipeline 彻底删除回收站中的流水线（不在回收站时返回 ErrInvalid）。
func (e *Engine) PurgePipeline(ctx context.Context, id string) error {
	p, err := e.store.GetPipeline(ctx, id)
	if err != nil {
		return err
	}
	if p.DeletedAt == nil {
		return fmt.Errorf("%w: move the pipeline to the recycle bin first", ErrInvalid)
	}
	return e.DeletePipeline(ctx, id)
}

// DeletePipeline 删除流水线及其全部运行、日志、触发器、流水线级密钥和产物。
func (e *Engine) DeletePipeline(ctx context.Context, id string) error {
	keys, err := e.store.DeletePipeline(ctx, id)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := e.removeBlob(ctx, k); err != nil {
			e.logf("delete artifact blob %s: %v", k, err)
		}
	}
	return nil
}

// TriggerRun 基于流水线当前定义创建一次运行，env 覆盖流水线和 Job 中的同名变量。
// createdBy 为手动触发的用户名（自动触发为空）。
func (e *Engine) TriggerRun(ctx context.Context, pipelineID, trigger, createdBy string, env map[string]string) (*model.Run, error) {
	return e.TriggerRunWithRemark(ctx, pipelineID, trigger, createdBy, env, "")
}

const maxRemarkLen = 200

// TriggerRunWithRemark 同 TriggerRun，并记录运行备注。
func (e *Engine) TriggerRunWithRemark(ctx context.Context, pipelineID, trigger, createdBy string, env map[string]string, remark string) (*model.Run, error) {
	remark = strings.TrimSpace(remark)
	if utf8.RuneCountInString(remark) > maxRemarkLen {
		return nil, fmt.Errorf("%w: remark must be at most %d characters", ErrInvalid, maxRemarkLen)
	}
	for k := range env {
		if !pipeline.ValidEnvName(k) {
			return nil, fmt.Errorf("%w: invalid env name %q", ErrInvalid, k)
		}
	}
	var run *model.Run
	err := e.store.Atomic(ctx, func(tx store.Tx) error {
		var err error
		run, err = e.startRun(ctx, tx, pipelineID, trigger, createdBy, env, nil, remark)
		return err
	})
	if err != nil {
		return nil, err
	}
	return e.store.GetRun(ctx, run.ID)
}

// RollbackRun 重放一次成功的运行，用于回滚部署：使用该运行当时的配置版本、运行参数与代码提交
// （未记录提交的旧运行按原分支拉取最新代码）。
func (e *Engine) RollbackRun(ctx context.Context, runID, username string) (*model.Run, error) {
	src, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if src.Status != model.StatusSuccess {
		return nil, fmt.Errorf("%w: only successful runs can be rolled back to (status %s)", ErrInvalid, src.Status)
	}
	if src.PipelineVersion == 0 {
		return nil, fmt.Errorf("%w: run #%d has no recorded pipeline version", ErrInvalid, src.Number)
	}
	rev, err := e.store.GetRevision(ctx, src.PipelineID, src.PipelineVersion)
	if err != nil {
		return nil, err
	}
	env := maps.Clone(src.Env)
	if env == nil {
		env = map[string]string{}
	}
	if src.Commit != "" {
		env["FLOW_GIT_COMMIT"] = src.Commit
	}
	var run *model.Run
	err = e.store.Atomic(ctx, func(tx store.Tx) error {
		var err error
		run, err = e.startRun(ctx, tx, src.PipelineID, "rollback", username, env, &replay{rev: rev, of: src.ID}, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	return e.store.GetRun(ctx, run.ID)
}

// replay 指定运行使用的历史配置版本（回滚运行）。
type replay struct {
	rev *model.PipelineRevision
	of  string // 原运行 ID
	// only 非空时只执行这个任务（按环境重新部署），其余任务跳过，上游产物取自原运行。
	only string
}

// Redeploy 把一次运行中的某个部署任务按当时的配置、参数与产物重新执行（只执行该任务），
// 用于把某个环境回退到这个版本，不重新构建、不影响其他环境。
func (e *Engine) Redeploy(ctx context.Context, runID, jobName, username string) (*model.Run, error) {
	src, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !src.Status.Terminal() {
		return nil, fmt.Errorf("%w: run #%d has not finished", ErrInvalid, src.Number)
	}
	if src.PipelineVersion == 0 {
		return nil, fmt.Errorf("%w: run #%d has no recorded pipeline version", ErrInvalid, src.Number)
	}
	jobs, err := e.store.ListJobs(ctx, runID)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(jobs, func(j *model.Job) bool { return j.Name == jobName })
	if i < 0 || jobs[i].Deploy == nil {
		return nil, fmt.Errorf("%w: run #%d has no deploy job %q", ErrInvalid, src.Number, jobName)
	}
	// 上游产物必须还在（过期清理后无法重新部署）。
	arts, err := e.upstreamArtifactsIn(ctx, jobs, src.ID, jobName)
	if err != nil {
		return nil, err
	}
	for _, name := range upstreamNames(jobs, jobName) {
		up := jobs[slices.IndexFunc(jobs, func(j *model.Job) bool { return j.Name == name })]
		if len(up.Artifacts) > 0 && !slices.ContainsFunc(arts, func(a *model.Artifact) bool { return a.JobName == name }) {
			return nil, fmt.Errorf("%w: artifacts of %q in run #%d are no longer available (expired or never uploaded)", ErrInvalid, name, src.Number)
		}
	}
	rev, err := e.store.GetRevision(ctx, src.PipelineID, src.PipelineVersion)
	if err != nil {
		return nil, err
	}
	env := maps.Clone(src.Env)
	if env == nil {
		env = map[string]string{}
	}
	if src.Commit != "" {
		env["FLOW_GIT_COMMIT"] = src.Commit
	}
	var run *model.Run
	err = e.store.Atomic(ctx, func(tx store.Tx) error {
		var err error
		run, err = e.startRun(ctx, tx, src.PipelineID, "rollback", username, env, &replay{rev: rev, of: src.ID, only: jobName}, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	return e.store.GetRun(ctx, run.ID)
}

// startRun 在事务中实例化 Run 及其 Job 并推进到首批可执行的 Job。
// rp 非空时使用其中的历史版本而非当前定义。
func (e *Engine) startRun(ctx context.Context, tx store.Tx, pipelineID, trigger, createdBy string, env map[string]string, rp *replay, remark string) (*model.Run, error) {
	p, err := tx.GetPipeline(ctx, pipelineID)
	if err != nil {
		return nil, err
	}
	if p.DeletedAt != nil {
		return nil, ErrInTrash
	}
	yamlText, version, rollbackOf, artifactsFrom := p.YAML, p.Version, "", ""
	if rp != nil {
		yamlText, version, rollbackOf = rp.rev.YAML, rp.rev.Version, rp.of
		if rp.only != "" {
			artifactsFrom = rp.of
		}
	}
	spec, err := pipeline.Parse([]byte(yamlText))
	if err != nil {
		return nil, fmt.Errorf("%w: stored pipeline is invalid: %v", ErrInvalid, err)
	}
	env, err = spec.ApplyParams(env, trigger == "manual" || trigger == "api")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	// 步骤模板在此时展开并随 Job 保存：之后修改模板不影响本次运行（含重试）。
	if err := e.expandTemplates(ctx, tx, p.TenantID, spec); err != nil {
		return nil, err
	}
	if err := checkHostGroups(ctx, tx.GetHostGroupByName, p.TenantID, spec); err != nil {
		return nil, err
	}
	if err := e.checkConnections(ctx, p.TenantID, spec, "", false); err != nil {
		return nil, err
	}
	groups, err := e.runVarGroups(ctx, p.TenantID, spec.VarGroups)
	if err != nil {
		return nil, err
	}
	groupVars := groupPlainVars(groups)
	var groupIDs []string
	for _, g := range groups {
		if slices.ContainsFunc(g.Vars, func(v model.GroupVar) bool { return v.Secret }) {
			groupIDs = append(groupIDs, g.ID)
		}
	}

	now := e.now()
	run := &model.Run{
		ID: newID(), TenantID: p.TenantID, PipelineID: p.ID, Status: model.StatusPending,
		Trigger: trigger, CreatedBy: createdBy, PipelineVersion: version, Env: maps.Clone(env), CreatedAt: now, NotifyDone: true,
		Commit: env["FLOW_GIT_COMMIT"], RollbackOf: rollbackOf, ArtifactsFrom: artifactsFrom, Remark: remark,
	}
	if src := spec.Source; src != nil && src.ReportStatus {
		// 提交状态随运行结束通知一起发送：Type 携带平台，Secret 引用令牌密钥。
		run.Notify = append(run.Notify, model.NotifyTarget{
			Type: commitStatusType + src.Provider, URL: src.URL, Secret: "${" + src.TokenSecret + "}",
			On: []model.Status{model.StatusSuccess, model.StatusFailed, model.StatusCanceled},
		})
	}
	for _, n := range spec.Notify {
		t := model.NotifyTarget{Type: n.Type, URL: n.URL, Secret: n.Secret}
		for _, on := range n.On {
			t.On = append(t.On, model.Status(on))
		}
		run.Notify = append(run.Notify, t)
	}
	deps := spec.Dependencies()
	artifactTTL := 0 // 用 Server 默认值
	if r := spec.Retention; r != nil && r.ArtifactTTL == pipeline.ForeverTTL {
		artifactTTL = -1
	} else if r != nil && r.ArtifactTTL > 0 {
		artifactTTL = int(r.ArtifactTTL / time.Second)
	}
	var jobs []*model.Job
	for si, st := range spec.Stages {
		for _, j := range st.Jobs {
			builtin := map[string]string{
				"FLOW_PIPELINE": spec.Name, "FLOW_RUN_ID": run.ID, "FLOW_TRIGGER": trigger,
				"FLOW_STAGE": st.Name, "FLOW_JOB": j.Name,
			}
			if b := env["FLOW_GIT_BRANCH"]; b != "" {
				builtin["FLOW_BRANCH"] = b
			} else if spec.Source != nil && spec.Source.Branch != "" {
				builtin["FLOW_BRANCH"] = spec.Source.Branch
			}
			steps := make([]model.Step, len(j.Steps))
			for k, s := range j.Steps {
				if steps[k], err = expandKind(s); err != nil {
					return nil, fmt.Errorf("job %q step %d: %w", j.Name, k+1, err)
				}
			}
			opts := model.JobOptions{
				Kind: j.Type, When: j.When, TimeoutSec: int(j.TimeoutDuration / time.Second), Retries: j.Retry,
				Artifacts: j.Artifacts, Approvers: j.Approvers, MatrixValues: j.MatrixValues, VarGroups: groupIDs,
				If: j.If, CacheDirs: spec.CacheDirs, PipelineID: p.ID,
			}
			for _, pl := range j.Plugins {
				opts.Plugins = append(opts.Plugins, model.Plugin{Name: pl.Name, Type: pl.Type, Connection: pl.Connection, URL: pl.URL,
					Secret: pl.Secret, On: pl.On, To: pl.To, Title: pl.Title, Content: pl.Content})
			}
			for _, n := range j.Notify {
				opts.ApprovalNotify = append(opts.ApprovalNotify, model.ApprovalNotify{Connection: n.Connection, To: n.To})
			}
			if len(j.Artifacts) > 0 {
				opts.ArtifactTTLSec = artifactTTL
			}
			if j.Cache != nil {
				opts.Cache = &model.Cache{Key: j.Cache.Key, Paths: j.Cache.Paths}
			}
			if j.Reports != nil {
				opts.Reports = j.Reports.JUnit
			}
			// 部署任务默认不拉取代码（通常部署上游产物），可用 checkout: true 开启。
			wantCode := j.Checkout == nil && j.Deploy == nil || j.Checkout != nil && *j.Checkout
			if spec.Source != nil && !j.IsApproval() && wantCode {
				opts.Checkout = checkoutFor(spec.Source, env)
			}
			if d := j.Deploy; d != nil {
				target := d.Target
				if target == "" {
					target = "/tmp/flow-deploy/" + spec.Name
				}
				opts.Deploy = &model.DeployTarget{Group: d.Hosts, Upload: d.Upload, Artifact: d.Artifact, Target: target, Batch: d.Batch}
				// 部署到要求审批的环境：任务就绪后先等待审批。
				if g, err := tx.GetHostGroupByName(ctx, p.TenantID, d.Hosts); err == nil && g.EnvironmentID != "" {
					if envr, err := tx.GetEnvironment(ctx, g.EnvironmentID); err == nil && envr.RequireApproval {
						opts.Deploy.Approval = true
						if len(opts.Approvers) == 0 {
							opts.Approvers = envr.Approvers
						}
					}
				}
			}
			job := &model.Job{
				ID: newID(), TenantID: p.TenantID, RunID: run.ID, Seq: len(jobs),
				Name: j.Name, Stage: st.Name, StageIndex: si, Needs: deps[j.Name], RunsOn: j.RunsOn,
				Image: j.Image, Env: mergeEnv(groupVars, spec.Env, j.Env, env, builtin), Steps: steps, JobOptions: opts,
				Status: model.StatusPending, Attempt: 1, CreatedAt: now,
				NotifyDone: len(opts.Plugins) > 0, NotifyWaiting: len(opts.ApprovalNotify) > 0,
			}
			job.ResetSteps()
			jobs = append(jobs, job)
		}
	}

	// 按环境重新部署：只执行指定任务，其余直接跳过；该任务的依赖已由原运行满足。
	if rp != nil && rp.only != "" {
		found := false
		for _, j := range jobs {
			if j.Name == rp.only {
				j.Needs, found = nil, true
				continue
			}
			j.Status, j.FinishedAt, j.Error = model.StatusSkipped, &now, "redeploy: only "+rp.only+" runs"
		}
		if !found {
			return nil, fmt.Errorf("%w: job %q not found in version %d", ErrInvalid, rp.only, version)
		}
	}

	// 并发控制：同组已有运行未结束时排队；cancel_previous 时先取消它们。
	if run.ConcurrencyGroup = spec.ConcurrencyGroup(p.ID, env); run.ConcurrencyGroup != "" {
		if err := tx.LockConcurrency(ctx, run.TenantID, run.ConcurrencyGroup); err != nil {
			return nil, err
		}
		active, err := tx.ActiveRuns(ctx, run.TenantID, run.ConcurrencyGroup)
		if err != nil {
			return nil, err
		}
		if spec.Concurrency.CancelPrevious {
			for _, r := range active {
				if err := e.cancelInTx(ctx, tx, r.ID, now); err != nil {
					return nil, err
				}
				e.runChanged(r.ID)
			}
			// 已排队的被直接取消；运行中的要等 Runner 中止后才结束，期间新运行继续排队。
			if active, err = tx.ActiveRuns(ctx, run.TenantID, run.ConcurrencyGroup); err != nil {
				return nil, err
			}
		}
		run.ConcurrencyMax = spec.MaxConcurrent()
		if !admit(active, run.ConcurrencyMax) {
			run.Status = model.StatusQueued
			return run, tx.CreateRun(ctx, run, jobs)
		}
	}
	if err := tx.CreateRun(ctx, run, jobs); err != nil {
		return nil, err
	}
	return run, e.advance(ctx, tx, run.ID)
}

// commitStatusType 是提交状态回写在 Run.Notify 中的类型前缀，后接平台名（可为空，按地址推断）。
const commitStatusType = "commit-status:"

// startNextInGroup 在运行结束后启动同一并发组中最早排队的运行。调用方已在事务中。
func (e *Engine) startNextInGroup(ctx context.Context, tx store.Tx, tenantID, group string) error {
	if err := tx.LockConcurrency(ctx, tenantID, group); err != nil {
		return err
	}
	active, err := tx.ActiveRuns(ctx, tenantID, group)
	if err != nil {
		return err
	}
	// 按触发顺序启动排队的运行，直到达到上限（以最早排队运行快照的上限为准）。
	for {
		var next *model.Run
		running := 0
		for _, r := range active {
			if r.Status != model.StatusQueued {
				running++
			} else if next == nil {
				next = r
			}
		}
		if next == nil || running >= max(next.ConcurrencyMax, 1) {
			return nil
		}
		next, err = tx.LockRun(ctx, next.ID)
		if err != nil {
			return err
		}
		if next.Status == model.StatusQueued {
			next.Status = model.StatusPending
			if err := tx.UpdateRun(ctx, next); err != nil {
				return err
			}
			e.runChanged(next.ID)
			if err := e.advance(ctx, tx, next.ID); err != nil {
				return err
			}
		}
		if active, err = tx.ActiveRuns(ctx, tenantID, group); err != nil {
			return err
		}
		// 刚启动的运行可能已直接结束（例如全部任务被跳过），不再算作执行中；防止死循环
		if slices.ContainsFunc(active, func(r *model.Run) bool { return r.ID == next.ID && r.Status == model.StatusQueued }) {
			return nil
		}
	}
}

// admit 判断新运行能否立即执行：组内没有排队的运行且执行中的少于上限。
func admit(active []*model.Run, limit int) bool {
	running := 0
	for _, r := range active {
		if r.Status == model.StatusQueued {
			return false
		}
		running++
	}
	return running < max(limit, 1)
}

// checkoutFor 决定拉取哪份代码：Webhook 注入的 PR 引用 / 标签 / 分支 / 提交优先，手动运行可传 FLOW_GIT_BRANCH 覆盖默认分支。
func checkoutFor(src *pipeline.Source, env map[string]string) *model.Checkout {
	c := &model.Checkout{URL: src.URL, Ref: src.Branch, Username: src.Username, TokenSecret: src.TokenSecret, Depth: src.Depth}
	if c.Depth == 0 {
		c.Depth = defaultCloneDepth
	}
	switch {
	case env["FLOW_GIT_EVENT"] == trigger.KindPullRequest && env["FLOW_GIT_REF"] != "":
		c.Ref = env["FLOW_GIT_REF"] // refs/pull/N/head 等，fork 的提交也能从目标仓库拉到
	case env["FLOW_GIT_TAG"] != "":
		c.Ref = "refs/tags/" + env["FLOW_GIT_TAG"]
	case env["FLOW_GIT_BRANCH"] != "":
		c.Ref = env["FLOW_GIT_BRANCH"]
	}
	c.Commit = env["FLOW_GIT_COMMIT"]
	return c
}

// RunnerInfo 是已认证的 Runner：TenantID 为空表示共享 Runner（使用全局 Token，可领取所有租户的 Job）。
type RunnerInfo struct {
	ID       string
	TenantID string
	Labels   []string
	Info     *model.NodeInfo // 节点上报的环境信息（可为空）
	Kind     string          // 注册 Runner 的类型：self / github / agent
	HostID   string          // agent：绑定的主机
}

// Assignment 是下发给 Runner 的任务：Job 本身、解密后的密钥、上游 Job 的产物。
type Assignment struct {
	Job         *model.Job
	Secrets     map[string]string
	Artifacts   []*model.Artifact
	Hosts       []protocol.DeployHost        // 部署任务的目标主机
	Env         map[string]string            // 部署环境的变量
	DeployError string                       // 无法解析部署目标时的原因
	Connections map[string]map[string]string // 步骤引用的服务连接变量
	Named       []*model.Artifact            // 上游的命名构建物
	Masks       []string                     // 只脱敏、不注入的值
}

// Claim 为 Runner 领取一个标签匹配的排队中 Job；无任务时返回 nil, nil。
// NodeLabel 是注册 Runner 的隐含标签：任务 runs-on 写 node:<名称> 即只在该构建节点上执行。
func NodeLabel(name string) string { return "node:" + name }

func (e *Engine) Claim(ctx context.Context, r RunnerInfo) (*Assignment, error) {
	if r.Kind == NodeAgent {
		return nil, nil // 主机 Agent 只执行部署到本机的任务（AgentClaim）
	}
	labels := r.Labels
	if r.TenantID != "" {
		if rr, err := e.store.GetRunner(ctx, r.ID); err == nil {
			labels = append(slices.Clone(labels), NodeLabel(rr.Name))
		}
	}
	var claimed *model.Job
	var pipelineID string
	var started *model.Run // 本次领取使运行开始执行
	err := e.store.Atomic(ctx, func(tx store.Tx) error {
		claimed, started = nil, nil
		j, err := tx.NextQueuedJob(ctx, store.ClaimFilter{TenantID: r.TenantID, Labels: labels})
		if err != nil || j == nil {
			return err
		}
		run, err := tx.LockRun(ctx, j.RunID)
		if err != nil {
			return err
		}
		now := e.now()
		j.Status, j.RunnerID, j.StartedAt, j.LeaseUntil = model.StatusRunning, r.ID, &now, now.Add(e.leaseTTL)
		if j.Checkout != nil && j.Checkout.Commit == "" && run.Commit != "" {
			j.Checkout.Commit = run.Commit // 与先拉取代码的 Job 保持同一提交
		}
		if err := tx.UpdateJob(ctx, j); err != nil {
			return err
		}
		if run.Status == model.StatusPending {
			started = run
		}
		if err := e.markRunStarted(ctx, tx, run, now); err != nil {
			return err
		}
		claimed, pipelineID = j, run.PipelineID
		claimed.Env = mergeEnv(claimed.Env, map[string]string{"FLOW_RUN_NUMBER": strconv.Itoa(run.Number)})
		return nil
	})
	if err != nil || claimed == nil {
		return nil, err
	}
	e.runChanged(claimed.RunID)
	if started != nil && started.Commit != "" {
		go e.reportRunning(started)
	}
	a := &Assignment{Job: claimed}
	// 密钥解密失败时仍下发 Job，由 Runner 在需要时报错；但记录到日志便于排查主密钥配置。
	group, gerr := e.groupSecrets(ctx, claimed)
	if a.Secrets, err = e.resolveSecrets(ctx, claimed.TenantID, pipelineID, group); err != nil || gerr != nil {
		err = errors.Join(err, gerr)
		e.logf("job %s: resolve secrets: %v", claimed.ID, err)
		_ = e.appendLog(ctx, claimed.ID, []byte("==> warning: failed to decrypt secrets: "+err.Error()+"\n"))
	}
	if msg := e.resolveConnections(ctx, claimed, a); msg != "" {
		_ = e.appendLog(ctx, claimed.ID, []byte("==> warning: "+msg+"\n"))
	}
	// 无依赖的任务也查询：重新部署的任务依赖已清空，产物来自原运行。
	arts, err := e.upstreamArtifacts(ctx, claimed)
	if err != nil {
		return nil, err
	}
	for _, art := range arts {
		if art.Name == "" {
			a.Artifacts = append(a.Artifacts, art)
		} else {
			a.Named = append(a.Named, art)
		}
	}
	if claimed.Deploy != nil {
		var g *model.HostGroup
		a.Hosts, g, a.DeployError = e.resolveDeploy(ctx, claimed)
		if g != nil && g.EnvironmentID != "" && a.DeployError == "" {
			// 注入部署环境的变量与密钥（环境密钥覆盖流水线 / 租户的同名密钥）。
			if env, err := e.store.GetEnvironment(ctx, g.EnvironmentID); err == nil {
				vars, secrets, err := e.envInjection(env)
				if err != nil {
					a.DeployError = err.Error()
				}
				a.Env = vars
				if a.Secrets == nil {
					a.Secrets = map[string]string{}
				}
				maps.Copy(a.Secrets, secrets)
			}
		}
		var deployed *model.DeployedArtifact
		if name := claimed.Deploy.Artifact; name != "" && a.DeployError == "" {
			if deployed = namedArtifact(a.Named, name); deployed == nil {
				a.DeployError = namedArtifactMissing(a.Named, name)
			}
		}
		if a.DeployError == "" {
			e.recordDeployment(ctx, claimed, g, a.Hosts, deployed)
		}
	}
	// 上游任务的输出作为环境变量（覆盖流水线与任务中的同名变量，密钥仍然优先）。
	if outs, err := e.upstreamOutputs(ctx, claimed); err == nil && len(outs) > 0 {
		claimed.Env = mergeEnv(claimed.Env, outs)
	}
	return a, nil
}

// upstreamArtifacts 返回 Job 全部上游（含间接上游，例如隔着人工卡点）的产物，按 Job 定义顺序排列，后者覆盖前者的同名文件。
func (e *Engine) upstreamArtifacts(ctx context.Context, j *model.Job) ([]*model.Artifact, error) {
	runID := j.RunID
	if run, err := e.store.GetRun(ctx, j.RunID); err != nil {
		return nil, err
	} else if run.ArtifactsFrom != "" {
		runID = run.ArtifactsFrom // 重新部署：依赖关系与产物都取自原运行
	}
	jobs, err := e.store.ListJobs(ctx, runID)
	if err != nil {
		return nil, err
	}
	return e.upstreamArtifactsIn(ctx, jobs, runID, j.Name)
}

// upstreamOutputs 合并全部上游（含间接上游）成功任务的输出，按任务定义顺序后者覆盖前者。
func (e *Engine) upstreamOutputs(ctx context.Context, j *model.Job) (map[string]string, error) {
	jobs, err := e.store.ListJobs(ctx, j.RunID)
	if err != nil {
		return nil, err
	}
	return outputsOf(jobs, upstreamNames(jobs, j.Name)), nil
}

func outputsOf(jobs []*model.Job, names []string) map[string]string {
	out := map[string]string{}
	for _, x := range jobs {
		if slices.Contains(names, x.Name) && x.Status == model.StatusSuccess {
			maps.Copy(out, x.Outputs)
		}
	}
	return out
}

// condHolds 对任务的运行条件求值：变量为任务变量叠加上游任务的输出。
func condHolds(j *model.Job, jobs []*model.Job) (bool, error) {
	c, err := pipeline.ParseCond(j.If)
	if err != nil {
		return false, err
	}
	return c.Eval(mergeEnv(j.Env, outputsOf(jobs, upstreamNames(jobs, j.Name))))
}

// upstreamNames 返回任务的全部上游（含间接上游）。
func upstreamNames(jobs []*model.Job, name string) []string {
	needs := make(map[string][]string, len(jobs))
	for _, x := range jobs {
		needs[x.Name] = x.Needs
	}
	seen := map[string]bool{}
	var out []string
	var visit func(name string)
	visit = func(name string) {
		for _, dep := range needs[name] {
			if !seen[dep] {
				seen[dep] = true
				out = append(out, dep)
				visit(dep)
			}
		}
	}
	visit(name)
	return out
}

func (e *Engine) upstreamArtifactsIn(ctx context.Context, jobs []*model.Job, runID, name string) ([]*model.Artifact, error) {
	upstream := upstreamNames(jobs, name)
	arts, err := e.store.ListArtifacts(ctx, runID)
	if err != nil {
		return nil, err
	}
	var out []*model.Artifact
	for _, art := range arts {
		if slices.Contains(upstream, art.JobName) {
			out = append(out, art)
		}
	}
	return out, nil
}

func (e *Engine) markRunStarted(ctx context.Context, tx store.Tx, run *model.Run, now time.Time) error {
	if run.Status != model.StatusPending {
		return nil
	}
	run.Status, run.StartedAt = model.StatusRunning, &now
	return tx.UpdateRun(ctx, run)
}

// Heartbeat 续租并返回是否被请求取消。
func (e *Engine) Heartbeat(ctx context.Context, jobID, runnerID string) (cancel bool, err error) {
	err = e.store.Atomic(ctx, func(tx store.Tx) error {
		j, err := e.lockLeasedJob(ctx, tx, jobID, runnerID)
		if err != nil {
			return err
		}
		j.LeaseUntil = e.now().Add(e.leaseTTL)
		cancel = j.CancelRequested
		return tx.UpdateJob(ctx, j)
	})
	return cancel, err
}

// AppendLog 只做轻量的归属校验，不加锁：日志追加与状态迁移互不影响。
func (e *Engine) AppendLog(ctx context.Context, jobID, runnerID string, data []byte) error {
	if _, err := e.leasedJob(ctx, jobID, runnerID); err != nil {
		return err
	}
	return e.appendLog(ctx, jobID, data)
}

func (e *Engine) leasedJob(ctx context.Context, jobID, runnerID string) (*model.Job, error) {
	j, err := e.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j.Status != model.StatusRunning || j.RunnerID != runnerID {
		return nil, ErrLeaseLost
	}
	return j, nil
}

func (e *Engine) Complete(ctx context.Context, jobID, runnerID string, status model.Status, exitCode int, errMsg string) error {
	return e.CompleteWithOutputs(ctx, jobID, runnerID, status, exitCode, errMsg, nil)
}

const (
	maxJobOutputs     = 100
	maxJobOutputBytes = 64 << 10
)

// CompleteWithOutputs 同 Complete，并记录任务输出（只在成功时保存；非法变量名与超限的输出被丢弃）。
func (e *Engine) CompleteWithOutputs(ctx context.Context, jobID, runnerID string, status model.Status, exitCode int, errMsg string, outputs map[string]string) error {
	clean := map[string]string{}
	size := 0
	for _, k := range slices.Sorted(maps.Keys(outputs)) {
		if !pipeline.ValidEnvName(k) || len(clean) >= maxJobOutputs || size+len(k)+len(outputs[k]) > maxJobOutputBytes {
			continue
		}
		clean[k] = outputs[k]
		size += len(k) + len(outputs[k])
	}
	switch status {
	case model.StatusSuccess, model.StatusFailed, model.StatusCanceled:
	default:
		return fmt.Errorf("%w: unsupported final status %q", ErrInvalid, status)
	}
	var note, runID string
	var finished *model.Job
	err := e.store.Atomic(ctx, func(tx store.Tx) error {
		note, finished = "", nil
		j, err := e.lockLeasedJob(ctx, tx, jobID, runnerID)
		if err != nil {
			return err
		}
		now := e.now()
		if status == model.StatusFailed {
			note = e.failOrRetry(j, now, exitCode, errMsg)
		} else {
			j.Status, j.ExitCode, j.Error, j.FinishedAt = status, exitCode, errMsg, &now
			finishSteps(j, now)
		}
		if status == model.StatusSuccess {
			j.Outputs = clean
		}
		if err := tx.UpdateJob(ctx, j); err != nil {
			return err
		}
		runID = j.RunID
		if j.Status.Terminal() {
			finished = j
		}
		return e.advance(ctx, tx, j.RunID)
	})
	if err != nil {
		return err
	}
	if finished != nil && finished.Deploy != nil {
		if err := e.store.FinishDeployment(ctx, finished.ID, finished.Status, *finished.FinishedAt); err != nil {
			e.logf("finish deployment %s: %v", finished.ID, err)
		}
	}
	if note != "" {
		_ = e.appendLog(ctx, jobID, []byte(note))
	}
	e.runChanged(runID)
	return nil
}

// RecordCommit 记录 Job 实际检出的提交；运行尚未确定提交时以此为准，之后领取的 Job 检出同一提交。
func (e *Engine) RecordCommit(ctx context.Context, jobID, runnerID, commit string) error {
	if !validCommit(commit) {
		return fmt.Errorf("%w: invalid commit %q", ErrInvalid, commit)
	}
	var updated *model.Run
	err := e.store.Atomic(ctx, func(tx store.Tx) error {
		updated = nil
		j, err := e.lockLeasedJob(ctx, tx, jobID, runnerID)
		if err != nil {
			return err
		}
		run, err := tx.LockRun(ctx, j.RunID)
		if err != nil || run.Commit != "" {
			return err
		}
		run.Commit, updated = commit, run
		return tx.UpdateRun(ctx, run)
	})
	if err == nil && updated != nil {
		e.runChanged(updated.ID)
		go e.reportRunning(updated)
	}
	return err
}

func validCommit(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// failOrRetry 将 Job 置为失败；还有重试次数且未被取消时改为重新入队，返回写入日志的提示。
func (e *Engine) failOrRetry(j *model.Job, now time.Time, exitCode int, errMsg string) string {
	if j.Attempt <= j.Retries && !j.CancelRequested {
		j.Attempt++
		j.Status, j.RunnerID, j.LeaseUntil = model.StatusQueued, "", time.Time{}
		j.StartedAt, j.FinishedAt, j.ExitCode, j.Error = nil, nil, 0, ""
		j.ResetSteps()
		return fmt.Sprintf("\n==> attempt failed (%s), retrying: attempt %d of %d\n\n", errMsg, j.Attempt, j.Retries+1)
	}
	j.Status, j.ExitCode, j.Error, j.FinishedAt = model.StatusFailed, exitCode, errMsg, &now
	finishSteps(j, now)
	return ""
}

// finishSteps 在 Job 结束时收尾仍在执行的步骤与部署主机（如被取消、超时、Runner 失联）。
func finishSteps(j *model.Job, now time.Time) {
	for i := range j.StepStates {
		if st := &j.StepStates[i]; st.Status == model.StatusRunning {
			st.Status, st.FinishedAt = j.Status, &now
		}
	}
	for i := range j.HostStates {
		switch h := &j.HostStates[i]; h.Status {
		case model.StatusRunning:
			h.Status, h.FinishedAt = j.Status, &now
		case model.StatusPending:
			h.Status = model.StatusSkipped
		}
	}
}

const maxHostStates = 1000

// ReportHosts 记录部署任务各主机的进度（Runner 每次上报完整列表）。
func (e *Engine) ReportHosts(ctx context.Context, jobID, runnerID string, hosts []model.HostState) error {
	if len(hosts) > maxHostStates {
		return fmt.Errorf("%w: too many hosts", ErrInvalid)
	}
	var runID string
	err := e.store.Atomic(ctx, func(tx store.Tx) error {
		j, err := e.lockLeasedJob(ctx, tx, jobID, runnerID)
		if err != nil {
			return err
		}
		if j.Deploy == nil {
			return fmt.Errorf("%w: not a deploy job", ErrInvalid)
		}
		j.HostStates, runID = hosts, j.RunID
		return tx.UpdateJob(ctx, j)
	})
	if err == nil {
		e.runChanged(runID)
	}
	return err
}

// ReportStep 记录步骤进度：status 为 running 表示开始，success / failed 表示结束。
func (e *Engine) ReportStep(ctx context.Context, jobID, runnerID string, index int, status model.Status, exitCode int, outputs map[string]string) error {
	switch status {
	case model.StatusRunning, model.StatusSuccess, model.StatusFailed:
	default:
		return fmt.Errorf("%w: unsupported step status %q", ErrInvalid, status)
	}
	var runID string
	err := e.store.Atomic(ctx, func(tx store.Tx) error {
		j, err := e.lockLeasedJob(ctx, tx, jobID, runnerID)
		if err != nil {
			return err
		}
		if index < 0 || index >= len(j.Steps) {
			return fmt.Errorf("%w: step %d out of range", ErrInvalid, index)
		}
		if len(j.StepStates) != len(j.Steps) {
			j.ResetSteps() // 早于步骤进度功能创建的 Job
		}
		now := e.now()
		st := &j.StepStates[index]
		if status == model.StatusRunning {
			*st = model.StepState{Status: status, StartedAt: &now}
		} else {
			st.Status, st.ExitCode, st.FinishedAt = status, exitCode, &now
			if len(outputs) > 0 {
				st.Outputs = outputs
			}
			if st.StartedAt == nil {
				st.StartedAt = &now
			}
		}
		runID = j.RunID
		return tx.UpdateJob(ctx, j)
	})
	if err == nil {
		e.runChanged(runID)
	}
	return err
}

// CancelRun 立即取消未开始的 Job（含等待审批的卡点），并通知 Runner 中止运行中的 Job（经由下一次心跳）。
func (e *Engine) CancelRun(ctx context.Context, runID string) (*model.Run, error) {
	err := e.store.Atomic(ctx, func(tx store.Tx) error {
		return e.cancelInTx(ctx, tx, runID, e.now())
	})
	if err != nil {
		return nil, err
	}
	e.runChanged(runID)
	return e.store.GetRun(ctx, runID)
}

func (e *Engine) cancelInTx(ctx context.Context, tx store.Tx, runID string, now time.Time) error {
	run, err := tx.LockRun(ctx, runID)
	if err != nil || run.Status.Terminal() {
		return err
	}
	jobs, err := tx.ListJobs(ctx, runID)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		switch j.Status {
		case model.StatusPending, model.StatusQueued, model.StatusWaiting:
			j.Status, j.FinishedAt = model.StatusCanceled, &now
		case model.StatusRunning:
			j.CancelRequested = true
		default:
			continue
		}
		if err := tx.UpdateJob(ctx, j); err != nil {
			return err
		}
	}
	return e.advance(ctx, tx, runID)
}

// Approve 处理人工卡点：通过则视为成功继续下游，驳回则视为失败。
// approvers 非空时只有名单中的用户（或 isAdmin）可以审批。
func (e *Engine) Approve(ctx context.Context, jobID, username string, isAdmin, approve bool, comment string) (*model.Job, error) {
	var out *model.Job
	err := e.store.Atomic(ctx, func(tx store.Tx) error {
		j, err := tx.GetJob(ctx, jobID)
		if err != nil {
			return err
		}
		run, err := tx.LockRun(ctx, j.RunID)
		if err != nil {
			return err
		}
		if j, err = tx.GetJob(ctx, jobID); err != nil {
			return err
		}
		if j.Status != model.StatusWaiting {
			return fmt.Errorf("%w: job is not waiting for approval (status %s)", ErrInvalid, j.Status)
		}
		if len(j.Approvers) > 0 && !slices.Contains(j.Approvers, username) && !isAdmin {
			return fmt.Errorf("%w: only %v can approve this job", ErrForbidden, j.Approvers)
		}
		now := e.now()
		j.DecidedBy, j.FinishedAt = username, &now
		switch {
		case approve && j.Deploy != nil:
			j.Status, j.FinishedAt = model.StatusQueued, nil // 部署审批通过：等待 Runner 执行
		case approve:
			j.Status, j.Error = model.StatusSuccess, comment
		default:
			j.Status, j.ExitCode, j.Error = model.StatusFailed, 1, "rejected by "+username
			if comment != "" {
				j.Error += ": " + comment
			}
		}
		if err := tx.UpdateJob(ctx, j); err != nil {
			return err
		}
		if err := e.markRunStarted(ctx, tx, run, now); err != nil {
			return err
		}
		out = j
		return e.advance(ctx, tx, j.RunID)
	})
	if err != nil {
		return nil, err
	}
	verb := "approved"
	if !approve {
		verb = "rejected"
	}
	msg := fmt.Sprintf("==> %s by %s", verb, username)
	if comment != "" {
		msg += ": " + comment
	}
	_ = e.appendLog(ctx, jobID, []byte(msg+"\n"))
	e.runChanged(out.RunID)
	return out, nil
}

// RetryRun 重试已结束的失败 / 取消运行：成功的 Job 保留结果，其余 Job 重置后重新调度。
func (e *Engine) RetryRun(ctx context.Context, runID, username string) (*model.Run, error) {
	var reset []string
	err := e.store.Atomic(ctx, func(tx store.Tx) error {
		reset = nil
		run, err := tx.LockRun(ctx, runID)
		if err != nil {
			return err
		}
		if run.Status != model.StatusFailed && run.Status != model.StatusCanceled {
			return fmt.Errorf("%w: only failed or canceled runs can be retried (status %s)", ErrInvalid, run.Status)
		}
		jobs, err := tx.ListJobs(ctx, runID)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.Status == model.StatusSuccess {
				continue
			}
			if j.StartedAt != nil {
				j.Attempt++
				reset = append(reset, j.ID)
			}
			j.Status, j.RunnerID, j.LeaseUntil, j.CancelRequested = model.StatusPending, "", time.Time{}, false
			j.StartedAt, j.FinishedAt, j.ExitCode, j.Error, j.DecidedBy = nil, nil, 0, "", ""
			j.ResetSteps()
			if err := tx.UpdateJob(ctx, j); err != nil {
				return err
			}
		}
		run.Status, run.FinishedAt, run.NotifyDone = model.StatusRunning, nil, true
		if run.StartedAt == nil {
			run.Status = model.StatusPending
		}
		// 同一并发组中有其他运行在执行时，重试的运行排队等待。
		if run.ConcurrencyGroup != "" {
			if err := tx.LockConcurrency(ctx, run.TenantID, run.ConcurrencyGroup); err != nil {
				return err
			}
			active, err := tx.ActiveRuns(ctx, run.TenantID, run.ConcurrencyGroup)
			if err != nil {
				return err
			}
			if slices.ContainsFunc(active, func(r *model.Run) bool { return r.ID != run.ID && r.Status != model.StatusQueued }) {
				run.Status = model.StatusQueued
				return tx.UpdateRun(ctx, run)
			}
		}
		if err := tx.UpdateRun(ctx, run); err != nil {
			return err
		}
		return e.advance(ctx, tx, runID)
	})
	if err != nil {
		return nil, err
	}
	for _, id := range reset {
		_ = e.appendLog(ctx, id, []byte(fmt.Sprintf("\n==> retried by %s\n\n", username)))
	}
	e.runChanged(runID)
	return e.store.GetRun(ctx, runID)
}

// ReapExpired 将租约过期（Runner 失联）的 Job 置为失败或按重试配置重新入队，返回受影响的 Run 数量。
// 多实例同时回收是安全的：锁住 Run 后会重新检查租约。
func (e *Engine) ReapExpired(ctx context.Context) (int, error) {
	now := e.now()
	runIDs, err := e.store.ExpiredJobRunIDs(ctx, now)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, runID := range runIDs {
		notes := map[string]string{}
		var deploys []string // 因此失败的部署任务
		err := e.store.Atomic(ctx, func(tx store.Tx) error {
			clear(notes)
			deploys = nil
			if _, err := tx.LockRun(ctx, runID); err != nil {
				return err
			}
			jobs, err := tx.ListJobs(ctx, runID)
			if err != nil {
				return err
			}
			reaped := false
			for _, j := range jobs {
				if j.Status != model.StatusRunning || now.Before(j.LeaseUntil) {
					continue
				}
				notes[j.ID] = e.failOrRetry(j, now, -1, "runner lost: lease expired")
				if err := tx.UpdateJob(ctx, j); err != nil {
					return err
				}
				if j.Deploy != nil && j.Status.Terminal() {
					deploys = append(deploys, j.ID)
				}
				reaped = true
			}
			if !reaped {
				return nil
			}
			return e.advance(ctx, tx, runID)
		})
		if err != nil {
			return n, err
		}
		if len(notes) > 0 {
			n++
			e.runChanged(runID)
		}
		for _, id := range deploys {
			_ = e.store.FinishDeployment(ctx, id, model.StatusFailed, now)
		}
		for id, note := range notes {
			if note != "" {
				_ = e.appendLog(ctx, id, []byte(note))
			}
		}
	}
	return n, nil
}

// lockLeasedJob 锁住 Job 所属的 Run 后重新读取 Job，并校验其仍由该 Runner 持有。
func (e *Engine) lockLeasedJob(ctx context.Context, tx store.Tx, jobID, runnerID string) (*model.Job, error) {
	j, err := tx.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.LockRun(ctx, j.RunID); err != nil {
		return nil, err
	}
	if j, err = tx.GetJob(ctx, jobID); err != nil {
		return nil, err
	}
	if j.Status != model.StatusRunning || j.RunnerID != runnerID {
		return nil, ErrLeaseLost
	}
	return j, nil
}

// advance 推进 Run，按 when 条件决定上游结束后的 Job 入队、进入审批或跳过（可级联），
// 所有 Job 结束后收尾 Run。调用方须已在事务中锁住该 Run（或刚创建它）。
func (e *Engine) advance(ctx context.Context, tx store.Tx, runID string) error {
	jobs, err := tx.ListJobs(ctx, runID)
	if err != nil {
		return err
	}
	byName := make(map[string]*model.Job, len(jobs))
	for _, j := range jobs {
		byName[j.Name] = j
	}
	now := e.now()
	for changed := true; changed; {
		changed = false
		anyFailed := slices.ContainsFunc(jobs, func(j *model.Job) bool { return j.Status == model.StatusFailed })
		for _, j := range jobs {
			if j.Status != model.StatusPending {
				continue
			}
			finished, allOK := true, true
			for _, dep := range j.Needs {
				switch byName[dep].Status {
				case model.StatusSuccess:
				case model.StatusSkipped:
					// 因自身条件（if / on_failure）跳过的上游视为满足，只有失败导致的连带跳过向下传递。
					allOK = allOK && skippedByCondition(byName[dep])
				case model.StatusFailed, model.StatusCanceled:
					allOK = false
				default:
					finished = false
				}
			}
			var run bool
			switch j.When {
			case pipeline.WhenAlways:
				run = true
			case pipeline.WhenOnFailure:
				run = anyFailed
			default:
				// 默认条件下，任一上游失败即可提前判定跳过，无需等待其余上游。
				if !allOK {
					finished = true
				}
				run = allOK
			}
			if !finished {
				continue
			}
			var condErr error
			if run && j.If != "" {
				ok, err := condHolds(j, jobs)
				if err != nil {
					condErr = err
				} else if !ok {
					j.Status, j.FinishedAt, j.Error = model.StatusSkipped, &now, skipIfPrefix+j.If
					if err := tx.UpdateJob(ctx, j); err != nil {
						return err
					}
					changed = true
					continue
				}
			}
			switch {
			case condErr != nil:
				j.Status, j.FinishedAt, j.Error = model.StatusFailed, &now, "运行条件无法求值："+condErr.Error()
			case !run && j.When == pipeline.WhenOnFailure:
				j.Status, j.FinishedAt, j.Error = model.StatusSkipped, &now, skipNoFailure
			case !run:
				j.Status, j.FinishedAt, j.Error = model.StatusSkipped, &now, "upstream job did not succeed"
			case j.Kind == model.JobKindApproval:
				j.Status, j.StartedAt = model.StatusWaiting, &now
			case j.Deploy != nil && j.Deploy.Approval && j.DecidedBy == "":
				j.Status = model.StatusWaiting // 部署前审批，通过后入队
			default:
				j.Status = model.StatusQueued
			}
			if err := tx.UpdateJob(ctx, j); err != nil {
				return err
			}
			changed = true
		}
	}
	return e.finalizeRun(ctx, tx, runID, jobs)
}

// 条件跳过的原因：这类跳过不影响下游任务。
const (
	skipIfPrefix  = "运行条件不满足："
	skipNoFailure = "condition not met: no job failed"
)

func skippedByCondition(j *model.Job) bool {
	return j.Status == model.StatusSkipped && (strings.HasPrefix(j.Error, skipIfPrefix) || j.Error == skipNoFailure)
}

func (e *Engine) finalizeRun(ctx context.Context, tx store.Tx, runID string, jobs []*model.Job) error {
	run, err := tx.LockRun(ctx, runID)
	if err != nil || run.Status.Terminal() {
		return err
	}
	status := model.StatusSuccess
	for _, j := range jobs {
		switch j.Status {
		case model.StatusFailed:
			status = model.StatusFailed
		case model.StatusCanceled:
			if status == model.StatusSuccess {
				status = model.StatusCanceled
			}
		case model.StatusSuccess, model.StatusSkipped:
		default:
			return nil // 仍有 Job 未结束
		}
	}
	now := e.now()
	run.Status, run.FinishedAt = status, &now
	if run.StartedAt == nil {
		run.StartedAt = &now
	}
	run.NotifyDone = !slices.ContainsFunc(run.Notify, func(t model.NotifyTarget) bool { return slices.Contains(t.On, status) })
	if err := tx.UpdateRun(ctx, run); err != nil {
		return err
	}
	if run.ConcurrencyGroup != "" {
		return e.startNextInGroup(ctx, tx, run.TenantID, run.ConcurrencyGroup)
	}
	return nil
}

// 以下为只读查询。

func (e *Engine) Pipeline(ctx context.Context, id string) (*model.Pipeline, error) {
	return e.store.GetPipeline(ctx, id)
}

// maxGroupLen 是分组名的最大字符数。
const maxGroupLen = 40

// SetPipelineGroup 设置流水线在列表中的分组（空为未分组）。
func (e *Engine) SetPipelineGroup(ctx context.Context, id, group string) error {
	group = strings.TrimSpace(group)
	if utf8.RuneCountInString(group) > maxGroupLen || strings.ContainsFunc(group, unicode.IsControl) {
		return fmt.Errorf("%w: group must be at most %d characters without control characters", ErrInvalid, maxGroupLen)
	}
	if group != "" {
		if p, err := e.store.GetPipeline(ctx, id); err == nil {
			_ = e.store.CreateGroup(ctx, p.TenantID, group, e.now())
		}
	}
	return e.store.SetPipelineGroup(ctx, id, group)
}

const maxDescriptionLen = 500

// SetPipelineInfo 设置流水线说明与归档标记。
func (e *Engine) SetPipelineInfo(ctx context.Context, id, description string, archived bool) error {
	description = strings.TrimSpace(description)
	if utf8.RuneCountInString(description) > maxDescriptionLen {
		return fmt.Errorf("%w: description must be at most %d characters", ErrInvalid, maxDescriptionLen)
	}
	return e.store.SetPipelineInfo(ctx, id, description, archived)
}

func (e *Engine) SetFavorite(ctx context.Context, userID, pipelineID string, on bool) error {
	return e.store.SetFavorite(ctx, userID, pipelineID, on, e.now())
}

func (e *Engine) Favorites(ctx context.Context, userID string) ([]string, error) {
	return e.store.ListFavorites(ctx, userID)
}

// RecentRuns 返回租户内各流水线最近 n 次运行（新的在前）。
func (e *Engine) RecentRuns(ctx context.Context, tenantID string, n int) (map[string][]*model.Run, error) {
	return e.store.RecentRuns(ctx, tenantID, n)
}

func (e *Engine) Pipelines(ctx context.Context, tenantID string) ([]*model.Pipeline, error) {
	return e.store.ListPipelines(ctx, tenantID)
}

func (e *Engine) Run(ctx context.Context, id string) (*model.Run, error) {
	return e.store.GetRun(ctx, id)
}

func (e *Engine) Runs(ctx context.Context, pipelineID string, limit int) ([]*model.Run, error) {
	return e.store.ListRuns(ctx, pipelineID, limit)
}

func (e *Engine) Job(ctx context.Context, id string) (*model.Job, error) {
	return e.store.GetJob(ctx, id)
}

func (e *Engine) Jobs(ctx context.Context, runID string) ([]*model.Job, error) {
	return e.store.ListJobs(ctx, runID)
}

func (e *Engine) ReadLog(ctx context.Context, jobID string, offset int) ([]byte, error) {
	return e.store.ReadLog(ctx, jobID, offset)
}

func mergeEnv(layers ...map[string]string) map[string]string {
	out := make(map[string]string)
	for _, l := range layers {
		maps.Copy(out, l)
	}
	return out
}

func newID() string { return randHex(12) }

func newSecret() string { return randHex(20) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
