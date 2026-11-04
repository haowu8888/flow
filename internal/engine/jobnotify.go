package engine

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"flow/internal/connect"
	"flow/internal/model"
	"flow/internal/notify"
	"flow/internal/store"
)

// 任务级通知：任务插件（任务结束时）与审批消息（人工卡点进入待审批时）。
// 在事务中锁住任务再发送，多实例不会重复发送；失败只记录到任务日志，不重试。

// DeliverJobNotifications 处理待发送的任务通知，返回处理的任务数。
func (e *Engine) DeliverJobNotifications(ctx context.Context) (int, error) {
	n := 0
	for range 20 {
		var done bool
		var logs []string
		var jobID string
		err := e.store.Atomic(ctx, func(tx store.Tx) error {
			done, logs = false, nil
			j, err := tx.NextJobNotify(ctx)
			if err != nil || j == nil {
				done = true
				return err
			}
			jobID = j.ID
			if j.NotifyWaiting && j.Status == model.StatusWaiting {
				logs = append(logs, e.sendApprovalMessages(ctx, j)...)
				j.NotifyWaiting = false
			}
			if j.NotifyDone && j.FinishedAt != nil {
				logs = append(logs, e.sendPlugins(ctx, j)...)
				j.NotifyDone = false
			}
			return tx.UpdateJob(ctx, j)
		})
		if err != nil {
			return n, err
		}
		if done {
			return n, nil
		}
		if len(logs) > 0 {
			_ = e.appendLog(ctx, jobID, []byte(strings.Join(logs, "")))
		}
		n++
	}
	return n, nil
}

// jobContext 是插件消息模板可用的变量。
func (e *Engine) jobContext(ctx context.Context, j *model.Job) (*model.Run, *model.Pipeline, map[string]string) {
	run, err := e.store.GetRun(ctx, j.RunID)
	if err != nil {
		return nil, nil, nil
	}
	p, _ := e.store.GetPipeline(ctx, run.PipelineID)
	name := ""
	group := ""
	if p != nil {
		name, group = p.Name, p.Group
	}
	executor := run.CreatedBy
	if executor == "" {
		executor = jobTriggerText[run.Trigger]
	}
	vars := map[string]string{
		"pipeline": name, "group": group, "run": strconv.Itoa(run.Number), "task": j.Name, "stage": j.Stage,
		"status": statusText(j.Status), "trigger": jobTriggerText[run.Trigger], "executor": executor, "remark": run.Remark,
		"branch": j.Env["FLOW_BRANCH"], "commit": shortCommit(run.Commit), "url": e.runURL(run.ID), "error": j.Error,
	}
	if j.StartedAt != nil && j.FinishedAt != nil {
		vars["duration"] = j.FinishedAt.Sub(*j.StartedAt).Round(time.Second).String()
	}
	return run, p, vars
}

var jobTriggerText = map[string]string{"manual": "手动触发", "webhook": "代码推送", "cron": "定时触发", "api": "API 触发", "rollback": "回滚", "generic": "流水线 Webhook"}

func statusText(s model.Status) string {
	switch s {
	case model.StatusSuccess:
		return "成功"
	case model.StatusFailed:
		return "失败"
	case model.StatusCanceled:
		return "已取消"
	case model.StatusSkipped:
		return "跳过"
	case model.StatusWaiting:
		return "待审批"
	}
	return string(s)
}

func shortCommit(c string) string {
	if len(c) > 8 {
		return c[:8]
	}
	return c
}

func (e *Engine) runURL(runID string) string {
	if e.publicURL == "" {
		return ""
	}
	return strings.TrimRight(e.publicURL, "/") + "/#/runs/" + runID
}

// render 展开模板中的 ${变量}：插件变量（pipeline、task、status …）与任务环境变量。
func render(tpl string, vars, env map[string]string) string {
	return secretRef.ReplaceAllStringFunc(tpl, func(ref string) string {
		k := ref[2 : len(ref)-1]
		if v, ok := vars[k]; ok {
			return v
		}
		return env[k]
	})
}

// pluginHits 判断插件是否关注该结束状态。
func pluginHits(p model.Plugin, s model.Status) bool {
	for _, on := range p.On {
		switch {
		case on == "always",
			on == "success" && s == model.StatusSuccess,
			on == "failure" && s == model.StatusFailed,
			on == "cancel" && s == model.StatusCanceled:
			return true
		}
	}
	return false
}

func (e *Engine) sendPlugins(ctx context.Context, j *model.Job) []string {
	run, _, vars := e.jobContext(ctx, j)
	if run == nil {
		return nil
	}
	var logs []string
	var secrets map[string]string
	for i, p := range j.Plugins {
		if !pluginHits(p, j.Status) {
			continue
		}
		title := render(p.Title, vars, j.Env)
		if title == "" {
			title = fmt.Sprintf("%s #%s · %s %s", vars["pipeline"], vars["run"], vars["task"], vars["status"])
		}
		body := render(p.Content, vars, j.Env)
		if body == "" {
			body = fmt.Sprintf("任务：%s（%s）\n状态：%s\n触发：%s · %s", vars["task"], vars["stage"], vars["status"], vars["trigger"], vars["executor"])
			if vars["branch"] != "" {
				body += "\n分支：" + vars["branch"] + " " + vars["commit"]
			}
			if vars["remark"] != "" {
				body += "\n备注：" + vars["remark"]
			}
			if j.Error != "" && j.Status != model.StatusSuccess {
				body += "\n错误：" + j.Error
			}
		}
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		var err error
		switch p.Type {
		case "webhook", "dingtalk", "feishu":
			if secrets == nil {
				secrets, _ = e.resolveSecrets(ctx, j.TenantID, run.PipelineID, nil)
			}
			t := notify.Target{Type: p.Type}
			if t.URL, err = expandSecrets(p.URL, secrets); err == nil {
				t.Secret, err = expandSecrets(p.Secret, secrets)
			}
			if err == nil {
				err = e.sender.Send(sctx, t, notify.Message{Pipeline: vars["pipeline"], Number: run.Number, Status: string(j.Status),
					Trigger: run.Trigger, Duration: vars["duration"], URL: vars["url"], Job: j.Name, CustomTitle: title, CustomBody: body})
			}
		default:
			err = e.sendViaConnection(sctx, j.TenantID, p.Connection, connect.Message{Title: title, Body: body, URL: vars["url"]}, p.To)
		}
		cancel()
		name := p.Name
		if name == "" {
			name = fmt.Sprintf("%s 插件 %d", p.Type, i+1)
		}
		if err != nil {
			logs = append(logs, fmt.Sprintf("==> plugin %s: failed: %v\n", name, err))
			e.logf("job %s plugin %s: %v", j.ID, name, err)
		} else {
			logs = append(logs, fmt.Sprintf("==> plugin %s: sent\n", name))
		}
	}
	return logs
}

func (e *Engine) sendViaConnection(ctx context.Context, tenantID, name string, m connect.Message, to []string) error {
	c, err := e.store.GetConnectionByName(ctx, tenantID, name)
	if err != nil {
		return fmt.Errorf("service connection %q: %w", name, err)
	}
	vals, err := e.connValues(c)
	if err != nil {
		return err
	}
	return connect.Send(ctx, c.Type, vals, m, to)
}

// ---- 审批消息 ----

func approvalMAC(jobID string, attempt int, action string) string {
	return fmt.Sprintf("approve:%s:%d:%s", jobID, attempt, action)
}

// ApprovalLink 返回带签名的审批链接（未配置 FLOW_PUBLIC_URL 时为空）。
func (e *Engine) ApprovalLink(j *model.Job, action string) string {
	if e.publicURL == "" {
		return ""
	}
	sig := e.cipher.MAC(approvalMAC(j.ID, j.Attempt, action))
	return fmt.Sprintf("%s/api/open/approve/%s?action=%s&sig=%s", strings.TrimRight(e.publicURL, "/"), j.ID, action, url.QueryEscape(sig))
}

func (e *Engine) sendApprovalMessages(ctx context.Context, j *model.Job) []string {
	_, _, vars := e.jobContext(ctx, j)
	if vars == nil {
		return nil
	}
	m := connect.Message{
		Title: fmt.Sprintf("待审批：%s #%s · %s", vars["pipeline"], vars["run"], j.Name),
		Body:  fmt.Sprintf("流水线 %s 的第 %s 次运行在「%s」等待审批。\n触发：%s · %s", vars["pipeline"], vars["run"], j.Name, vars["trigger"], vars["executor"]),
		URL:   vars["url"],
	}
	if vars["remark"] != "" {
		m.Body += "\n备注：" + vars["remark"]
	}
	if len(j.Approvers) > 0 {
		m.Body += "\n审批人：" + strings.Join(j.Approvers, "、")
	}
	if ok := e.ApprovalLink(j, "approve"); ok != "" {
		m.Links = []connect.Link{{Text: "通过", URL: ok}, {Text: "驳回", URL: e.ApprovalLink(j, "reject")}}
	} else {
		m.Body += "\n（Server 未配置 FLOW_PUBLIC_URL，请到 Flow 中审批）"
	}
	var logs []string
	for _, n := range j.ApprovalNotify {
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := e.sendViaConnection(sctx, j.TenantID, n.Connection, m, n.To)
		cancel()
		if err != nil {
			logs = append(logs, fmt.Sprintf("==> approval message via %s: failed: %v\n", n.Connection, err))
		} else {
			logs = append(logs, fmt.Sprintf("==> approval message sent via %s\n", n.Connection))
		}
	}
	return logs
}

// ApproveByLink 处理审批消息中的链接：校验签名后以「外部审批」身份通过或驳回（链接持有者即审批人）。
func (e *Engine) ApproveByLink(ctx context.Context, jobID, action, sig string) (*model.Job, error) {
	j, err := e.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if !slices.Contains([]string{"approve", "reject"}, action) || !e.cipher.VerifyMAC(approvalMAC(j.ID, j.Attempt, action), sig) {
		return nil, ErrUnauthorized
	}
	if j.Status != model.StatusWaiting {
		return j, fmt.Errorf("%w: job is %s, not waiting for approval", ErrInvalid, j.Status)
	}
	return e.Approve(ctx, jobID, "审批链接", true, action == "approve", "通过消息中的链接审批")
}

// PublicURL 返回界面的外部地址（未配置时为空）。
func (e *Engine) PublicURL() string { return e.publicURL }

// SetPublicURL 设置界面的外部地址（用于通知与审批链接），等同 WithPublicURL。
func (e *Engine) SetPublicURL(u string) { e.publicURL = u }
