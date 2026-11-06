package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"flow/internal/model"
	"flow/internal/store"
)

// JobWait 是排队中的任务还没被领取的原因。
type JobWait struct {
	Job    string   `json:"job"`
	Labels []string `json:"labels,omitempty"`
	Online int      `json:"online"` // 在线且标签匹配的构建节点数
	Reason string   `json:"reason"`
}

// QueueWait 是运行在等什么：并发组的名额（运行排队）或可领取任务的构建节点（任务排队）。
type QueueWait struct {
	Group   string    `json:"group,omitempty"`
	Running int       `json:"running,omitempty"` // 并发组中正在执行的运行数
	Limit   int       `json:"limit,omitempty"`   // 并发上限
	Ahead   int       `json:"ahead,omitempty"`   // 排在前面的运行数
	Jobs    []JobWait `json:"jobs,omitempty"`
}

// QueueWait 返回运行的排队原因；运行没有在等待时返回 nil。
func (e *Engine) QueueWait(ctx context.Context, runID string) (*QueueWait, error) {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil || run.Status.Terminal() {
		return nil, err
	}
	w := &QueueWait{}
	if run.Status == model.StatusQueued && run.ConcurrencyGroup != "" {
		var active []*model.Run
		err := e.store.Atomic(ctx, func(tx store.Tx) error {
			var err error
			active, err = tx.ActiveRuns(ctx, run.TenantID, run.ConcurrencyGroup)
			return err
		})
		if err != nil {
			return nil, err
		}
		w.Group, w.Limit = run.ConcurrencyGroup, max(run.ConcurrencyMax, 1)
		for _, r := range active {
			switch {
			case r.Status != model.StatusQueued:
				w.Running++
			case r.CreatedAt.Before(run.CreatedAt):
				w.Ahead++
			}
		}
		return w, nil
	}
	jobs, err := e.store.ListJobs(ctx, runID)
	if err != nil {
		return nil, err
	}
	var runners []*model.Runner
	loaded := false
	now := e.now()
	for _, j := range jobs {
		if j.Status != model.StatusQueued {
			continue
		}
		if !loaded {
			if runners, err = e.store.ListRunners(ctx, run.TenantID); err != nil {
				return nil, err
			}
			loaded = true
		}
		jw := JobWait{Job: j.Name, Labels: j.RunsOn}
		github := false
		for _, r := range runners {
			if r.Kind == NodeAgent {
				continue
			}
			labels := append(slices.Clone(r.Labels), NodeLabel(r.Name))
			if !subsetOf(j.RunsOn, labels) {
				continue
			}
			if r.Kind == NodeGitHub {
				github = true
			} else if r.LastSeenAt != nil && now.Sub(*r.LastSeenAt) < runnerOnlineTime {
				jw.Online++
			}
		}
		want := "任意构建节点"
		if len(j.RunsOn) > 0 {
			want = "标签 " + strings.Join(j.RunsOn, ", ")
		}
		switch {
		case j.Deploy != nil && jw.Online == 0:
			jw.Reason = "部署任务等待构建节点领取（" + want + "），没有在线的匹配节点"
		case jw.Online > 0:
			jw.Reason = fmt.Sprintf("等待构建节点领取：%d 个匹配节点在线，节点并发已满时排队", jw.Online)
		case github:
			jw.Reason = "等待 GitHub Actions 机器启动并领取"
		default:
			jw.Reason = "没有在线的构建节点匹配" + want + "，节点上线后自动开始"
		}
		w.Jobs = append(w.Jobs, jw)
	}
	if len(w.Jobs) == 0 {
		return nil, nil
	}
	return w, nil
}

func subsetOf(want, have []string) bool {
	for _, l := range want {
		if !slices.Contains(have, l) {
			return false
		}
	}
	return true
}

// ForceCancelRun 强制结束运行：不等构建节点确认，运行中的任务也立即标记为已取消（节点下次心跳时发现租约失效后中止）。
func (e *Engine) ForceCancelRun(ctx context.Context, runID, username string) (*model.Run, error) {
	var forced []string
	err := e.store.Atomic(ctx, func(tx store.Tx) error {
		forced = nil
		run, err := tx.LockRun(ctx, runID)
		if err != nil || run.Status.Terminal() {
			return err
		}
		jobs, err := tx.ListJobs(ctx, runID)
		if err != nil {
			return err
		}
		now := e.now()
		for _, j := range jobs {
			switch j.Status {
			case model.StatusPending, model.StatusQueued, model.StatusWaiting, model.StatusRunning:
			default:
				continue
			}
			if j.Status == model.StatusRunning {
				forced = append(forced, j.ID)
				for i := range j.StepStates {
					if j.StepStates[i].Status == model.StatusRunning {
						j.StepStates[i].Status, j.StepStates[i].FinishedAt = model.StatusCanceled, &now
					}
				}
			}
			j.Status, j.FinishedAt, j.CancelRequested = model.StatusCanceled, &now, true
			if j.Error == "" {
				j.Error = "强制结束（" + username + "）"
			}
			if err := tx.UpdateJob(ctx, j); err != nil {
				return err
			}
		}
		return e.advance(ctx, tx, runID)
	})
	if err != nil {
		return nil, err
	}
	for _, id := range forced {
		_ = e.appendLog(ctx, id, []byte("==> 已被 "+username+" 强制结束，不再等待构建节点确认\n"))
	}
	e.runChanged(runID)
	return e.store.GetRun(ctx, runID)
}
