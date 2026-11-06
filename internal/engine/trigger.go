package engine

import (
	"cmp"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"flow/internal/model"
	"flow/internal/pipeline"
	"flow/internal/store"
	"flow/internal/trigger"
)

var ErrUnauthorized = errors.New("unauthorized")

// maxCronPerTick 限制单次调度触发的数量，避免积压时长时间占用调度循环。
const maxCronPerTick = 100

// deliveryRetention 是 webhook 投递记录的保留时长，覆盖各平台的自动重试与手动重发窗口。
const deliveryRetention = 7 * 24 * time.Hour

var webhookEvents = []string{trigger.KindPush, trigger.KindPullRequest}

func (e *Engine) CreateTrigger(ctx context.Context, pipelineID string, kind model.TriggerKind, enabled bool, cfg model.TriggerConfig) (*model.Trigger, error) {
	p, err := e.store.GetPipeline(ctx, pipelineID)
	if err != nil {
		return nil, err
	}
	now := e.now()
	t := &model.Trigger{
		ID: newID(), TenantID: p.TenantID, PipelineID: p.ID, Kind: kind, Enabled: enabled,
		CreatedAt: now, UpdatedAt: now,
	}
	if kind == model.TriggerWebhook || kind == model.TriggerGeneric {
		t.Secret = newSecret()
	}
	if err := e.configure(t, cfg); err != nil {
		return nil, err
	}
	if err := e.store.CreateTrigger(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// UpdateTrigger 修改配置与启停状态；类型不可变。cron 的下次触发时间按新配置重新计算。
func (e *Engine) UpdateTrigger(ctx context.Context, id string, enabled bool, cfg model.TriggerConfig) (*model.Trigger, error) {
	t, err := e.store.GetTrigger(ctx, id)
	if err != nil {
		return nil, err
	}
	t.Enabled, t.UpdatedAt = enabled, e.now()
	if err := e.configure(t, cfg); err != nil {
		return nil, err
	}
	if err := e.store.UpdateTrigger(ctx, t); err != nil {
		return nil, err
	}
	return e.store.GetTrigger(ctx, id)
}

// RotateTriggerSecret 重新生成 webhook 签名密钥，旧密钥立即失效。
func (e *Engine) RotateTriggerSecret(ctx context.Context, id string) (*model.Trigger, error) {
	t, err := e.store.GetTrigger(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.Kind != model.TriggerWebhook && t.Kind != model.TriggerGeneric {
		return nil, fmt.Errorf("%w: only webhook triggers have a secret", ErrInvalid)
	}
	t.Secret, t.UpdatedAt = newSecret(), e.now()
	if err := e.store.UpdateTrigger(ctx, t); err != nil {
		return nil, err
	}
	return e.store.GetTrigger(ctx, id)
}

func (e *Engine) DeleteTrigger(ctx context.Context, id string) error {
	return e.store.DeleteTrigger(ctx, id)
}

func (e *Engine) Trigger(ctx context.Context, id string) (*model.Trigger, error) {
	return e.store.GetTrigger(ctx, id)
}

func (e *Engine) Triggers(ctx context.Context, pipelineID string) ([]*model.Trigger, error) {
	if _, err := e.store.GetPipeline(ctx, pipelineID); err != nil {
		return nil, err
	}
	return e.store.ListTriggers(ctx, pipelineID)
}

// configure 校验并写入配置，清除与类型无关的字段。
func (e *Engine) configure(t *model.Trigger, cfg model.TriggerConfig) error {
	for k := range cfg.Env {
		if !pipeline.ValidEnvName(k) {
			return fmt.Errorf("%w: invalid env name %q", ErrInvalid, k)
		}
	}
	switch t.Kind {
	case model.TriggerWebhook:
		events := trimAll(cfg.Events)
		cfg.Events = nil
		for _, ev := range webhookEvents { // 去重并按固定顺序保存
			if slices.Contains(events, ev) {
				cfg.Events = append(cfg.Events, ev)
			}
		}
		for _, ev := range events {
			if !slices.Contains(webhookEvents, ev) {
				return fmt.Errorf("%w: unknown webhook event %q (want push or pull_request)", ErrInvalid, ev)
			}
		}
		if len(cfg.Events) == 0 {
			cfg.Events = []string{trigger.KindPush}
		}
		if !slices.Contains(cfg.Events, trigger.KindPullRequest) {
			cfg.AllowForks = false
		}
		cfg.Branches, cfg.Tags = trimAll(cfg.Branches), trimAll(cfg.Tags)
		cfg.Paths, cfg.PathsIgnore = trimAll(cfg.Paths), trimAll(cfg.PathsIgnore)
		if err := trigger.ValidatePatterns(slices.Concat(cfg.Branches, cfg.Tags, cfg.Paths, cfg.PathsIgnore)); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		cfg.Schedule, cfg.Timezone = "", ""
		t.NextRunAt = nil
	case model.TriggerCron:
		cfg.Schedule = strings.TrimSpace(cfg.Schedule)
		sched, err := trigger.ParseCron(cfg.Schedule, cfg.Timezone)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		cfg.Branches, cfg.Tags, cfg.Events, cfg.AllowForks, cfg.Paths, cfg.PathsIgnore = nil, nil, nil, false, nil, nil
		t.NextRunAt = nil
		if next := sched.Next(e.now()); t.Enabled && !next.IsZero() {
			t.NextRunAt = &next
		}
	case model.TriggerGeneric:
		cfg = model.TriggerConfig{Env: cfg.Env}
		t.NextRunAt = nil
	default:
		return fmt.Errorf("%w: unknown trigger kind %q (want webhook, cron or generic)", ErrInvalid, t.Kind)
	}
	t.TriggerConfig = cfg
	return nil
}

// WebhookResult 描述一次 webhook 投递的处理结果：创建了 Run，或因过滤等原因被忽略。
// Duplicate 表示该投递 ID 已处理过，Run 为当时创建的运行。
type WebhookResult struct {
	Run       *model.Run `json:"run,omitempty"`
	Skipped   string     `json:"skipped,omitempty"`
	Duplicate bool       `json:"duplicate,omitempty"`
}

// HandleWebhook 校验签名、解析 Git 推送或 PR / MR 事件，按触发器配置过滤后创建 Run。
// 带投递 ID 的重复投递（平台重试）不会重复创建 Run。
func (e *Engine) HandleWebhook(ctx context.Context, triggerID string, h http.Header, body []byte) (*WebhookResult, error) {
	t, err := e.store.GetTrigger(ctx, triggerID)
	if err != nil {
		return nil, err
	}
	if t.Kind != model.TriggerWebhook {
		return nil, store.ErrNotFound
	}
	if err := trigger.Verify(h, body, t.Secret); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if !t.Enabled {
		return &WebhookResult{Skipped: "trigger is disabled"}, nil
	}
	events := t.Events
	if len(events) == 0 {
		events = []string{trigger.KindPush} // 早于事件配置创建的触发器
	}
	// 先按请求头判断是否订阅了该事件，未订阅的事件无需解析载荷。
	if name, kind := trigger.EventKind(h); kind == trigger.KindPing {
		return &WebhookResult{Skipped: "pong"}, nil
	} else if kind != trigger.KindPush && kind != trigger.KindPullRequest {
		return &WebhookResult{Skipped: fmt.Sprintf("event %q is ignored, only push and pull request events trigger runs", name)}, nil
	} else if !slices.Contains(events, kind) {
		return &WebhookResult{Skipped: fmt.Sprintf("%s events are not enabled for this trigger", kind)}, nil
	}
	ev, err := trigger.ParseEvent(h, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var env map[string]string
	var info *model.TriggerInfo
	switch {
	case ev.Push != nil:
		if ev.Push.Deleted {
			return &WebhookResult{Skipped: fmt.Sprintf("ref %s was deleted", ev.Push.Ref)}, nil
		}
		if ok, reason := trigger.MatchRef(t.Branches, t.Tags, ev.Push); !ok {
			return &WebhookResult{Skipped: reason}, nil
		}
		if ok, reason := trigger.MatchPaths(t.Paths, t.PathsIgnore, ev.Push); !ok {
			return &WebhookResult{Skipped: reason}, nil
		}
		if tag := skipCI(ev.Push.FullMessage); tag != "" {
			return &WebhookResult{Skipped: fmt.Sprintf("commit message contains %s", tag)}, nil
		}
		env = ev.Push.Env()
		info = pushInfo(ev.Provider, ev.Push)
	case ev.PR != nil:
		pr := ev.PR
		if !pr.Triggers() {
			return &WebhookResult{Skipped: fmt.Sprintf("pull request action %q does not trigger runs", pr.Action)}, nil
		}
		if pr.Fork && !t.AllowForks {
			return &WebhookResult{Skipped: fmt.Sprintf("pull request #%d comes from fork %q; enable allow_forks to run it", pr.Number, pr.SourceRepo)}, nil
		}
		if ok, reason := trigger.MatchTarget(t.Branches, pr); !ok {
			return &WebhookResult{Skipped: reason}, nil
		}
		if tag := skipCI(pr.Title); tag != "" {
			return &WebhookResult{Skipped: fmt.Sprintf("pull request title contains %s", tag)}, nil
		}
		env = pr.Env()
		info = &model.TriggerInfo{Provider: ev.Provider, Event: trigger.KindPullRequest, Pusher: pr.Author, Repo: pr.Repo, RepoURL: pr.URL,
			Ref: pr.Ref, Branch: pr.SourceBranch + " → " + pr.TargetBranch, After: pr.Commit}
	}

	var run *model.Run
	var invalid error
	var dupRun string
	dup := false
	err = e.store.Atomic(ctx, func(tx store.Tx) error {
		run, invalid, dup = nil, nil, false
		if ev.DeliveryID != "" {
			var err error
			if dupRun, dup, err = tx.ClaimDelivery(ctx, t.ID, ev.DeliveryID, e.now()); err != nil || dup {
				return err
			}
		}
		var err error
		if run, invalid, err = e.fireInTx(ctx, tx, t, nil, mergeEnv(t.Env, env)); err != nil {
			return err
		}
		if run != nil && info != nil {
			if err := tx.SetRunTriggerInfo(ctx, run.ID, info); err != nil {
				return err
			}
		}
		if ev.DeliveryID != "" && run != nil {
			return tx.SetDeliveryRun(ctx, t.ID, ev.DeliveryID, run.ID)
		}
		return nil
	})
	switch {
	case err != nil:
		return nil, err
	case dup:
		res := &WebhookResult{Duplicate: true, Skipped: fmt.Sprintf("delivery %s was already handled", ev.DeliveryID)}
		if dupRun != "" {
			if r, err := e.store.GetRun(ctx, dupRun); err == nil {
				res.Run, res.Skipped = r, ""
			}
		}
		return res, nil
	case invalid != nil:
		return nil, invalid
	}
	if run, err = e.store.GetRun(ctx, run.ID); err != nil {
		return nil, err
	}
	return &WebhookResult{Run: run}, nil
}

// maxTriggerCommits 是触发详情中保存的提交数上限。
const maxTriggerCommits = 20

// pushInfo 把推送事件整理为触发详情。
func pushInfo(provider string, p *trigger.Push) *model.TriggerInfo {
	info := &model.TriggerInfo{Provider: provider, Event: trigger.KindPush, Pusher: p.Pusher, Repo: p.Repo, RepoURL: p.RepoWeb,
		Ref: p.Ref, Branch: cmp.Or(p.Branch(), p.Tag()), Before: p.Before, After: p.Commit, CompareURL: p.CompareURL, TotalCommits: p.TotalCommits}
	if strings.Trim(info.Before, "0") == "" {
		info.Before = "" // 新建分支
	}
	commits := p.Commits
	if len(commits) > maxTriggerCommits {
		commits = commits[len(commits)-maxTriggerCommits:]
	}
	for _, c := range commits {
		info.Commits = append(info.Commits, model.TriggerCommit{ID: c.ID, Message: c.Message, Author: c.Author, URL: c.URL})
	}
	return info
}

// skipCI 返回提交信息中的跳过标记（与 GitHub / GitLab 一致），没有时返回空。
func skipCI(message string) string {
	lower := strings.ToLower(message)
	for _, tag := range []string{"[skip ci]", "[ci skip]", "[no ci]", "[skip flow]"} {
		if strings.Contains(lower, tag) {
			return tag
		}
	}
	return ""
}

// CleanupDeliveries 删除过期的 webhook 投递记录，由后台循环调用。
// 同时清理超过保留期的审计日志。
func (e *Engine) CleanupDeliveries(ctx context.Context) error {
	if err := e.store.DeleteAuditBefore(ctx, e.now().Add(-auditRetention)); err != nil {
		return err
	}
	return e.store.DeleteDeliveriesBefore(ctx, e.now().Add(-deliveryRetention))
}

// FireDueCron 触发所有已到期的 cron 触发器，返回创建的 Run 数量。
// 错过的时刻（如 Server 停机期间）只补触发一次，下次时间从当前时刻起算。
// 多实例同时调度是安全的：到期触发器以 SKIP LOCKED 方式领取，并在同一事务中推进下次时间。
func (e *Engine) FireDueCron(ctx context.Context) (int, error) {
	now := e.now()
	n := 0
	for range maxCronPerTick {
		var fired bool
		err := e.store.Atomic(ctx, func(tx store.Tx) error {
			fired = false
			t, err := tx.NextDueTrigger(ctx, now)
			if err != nil || t == nil {
				return err
			}
			fired = true
			sched, err := trigger.ParseCron(t.Schedule, t.Timezone)
			if err != nil {
				// 配置在写入时已校验，此处仅防御（如时区数据缺失）：停止调度并记录原因。
				return tx.MarkTriggerFired(ctx, t.ID, nil, now, err.Error())
			}
			var next *time.Time
			if v := sched.Next(now); !v.IsZero() {
				next = &v
			}
			_, _, err = e.fireInTx(ctx, tx, t, next, t.Env)
			return err
		})
		if err != nil {
			return n, err
		}
		if !fired {
			return n, nil
		}
		n++
	}
	return n, nil
}

// fireInTx 创建 Run 并记录触发结果。YAML 无效属于用户配置问题：只记录到触发器并通过 invalid 返回，不回滚事务。
func (e *Engine) fireInTx(ctx context.Context, tx store.Tx, t *model.Trigger, next *time.Time, env map[string]string) (run *model.Run, invalid, err error) {
	run, err = e.startRun(ctx, tx, t.PipelineID, string(t.Kind), "", env, nil, "")
	errMsg := ""
	if errors.Is(err, ErrInvalid) {
		invalid, errMsg = err, err.Error()
	} else if err != nil {
		return nil, nil, err
	}
	return run, invalid, tx.MarkTriggerFired(ctx, t.ID, next, e.now(), errMsg)
}

func trimAll(ss []string) []string {
	var out []string
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// GenericResult 是流水线 Webhook 的触发结果：vars 为生效的变量，ignored 为未开启运行时设置而被忽略的变量。
type GenericResult struct {
	Run     *model.Run `json:"-"`
	RunID   string     `json:"runId"`
	Number  int        `json:"buildNumber"`
	Vars    []string   `json:"vars"`
	Ignored []string   `json:"ignored"`
}

// FireGeneric 处理流水线 Webhook：校验令牌，只接受运行参数与开启了运行时设置的变量（其余忽略）。
func (e *Engine) FireGeneric(ctx context.Context, pipelineID, token string, body map[string]string) (*GenericResult, error) {
	ts, err := e.store.ListTriggers(ctx, pipelineID)
	if err != nil {
		return nil, err
	}
	var t *model.Trigger
	for _, x := range ts {
		if x.Kind == model.TriggerGeneric && x.Enabled && subtle.ConstantTimeCompare([]byte(x.Secret), []byte(token)) == 1 {
			t = x
		}
	}
	if t == nil {
		return nil, ErrUnauthorized
	}
	p, err := e.store.GetPipeline(ctx, pipelineID)
	if err != nil {
		return nil, err
	}
	spec, err := pipeline.Parse([]byte(p.YAML))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	allowed := spec.RuntimeNames()
	env := maps.Clone(t.Env)
	if env == nil {
		env = map[string]string{}
	}
	res := &GenericResult{Vars: []string{}, Ignored: []string{}}
	for _, k := range slices.Sorted(maps.Keys(body)) {
		if allowed[k] {
			env[k] = body[k]
			res.Vars = append(res.Vars, k)
		} else {
			res.Ignored = append(res.Ignored, k)
		}
	}
	var run *model.Run
	err = e.store.Atomic(ctx, func(tx store.Tx) error {
		var invalid error
		run, invalid, err = e.fireInTx(ctx, tx, t, nil, env)
		if err == nil && invalid != nil {
			err = invalid
		}
		if err == nil && run != nil {
			err = tx.SetRunTriggerInfo(ctx, run.ID, &model.TriggerInfo{Provider: "generic", Vars: res.Vars})
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	res.Run, res.RunID, res.Number = run, run.ID, run.Number
	return res, nil
}
