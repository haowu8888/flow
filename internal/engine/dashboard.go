package engine

import (
	"context"
	"sort"
	"time"

	"flow/internal/model"
)

// Dashboard 是租户的运行概览（仪表盘页）。
type Dashboard struct {
	Days      int `json:"days"`
	Pipelines int `json:"pipelines"`
	Mine      int `json:"mine"` // 当前用户创建的流水线数

	Current  PeriodStat `json:"current"`  // 最近 days 天
	Previous PeriodStat `json:"previous"` // 再往前 days 天，用于环比

	RunnersOnline int `json:"runners_online"`
	RunnersTotal  int `json:"runners_total"`

	Daily   []DashDay        `json:"daily"`   // 时间正序，含没有运行的日期
	Active  []DashRun        `json:"active"`  // 运行中 / 排队 / 待审批，早的在前
	Recent  []DashRun        `json:"recent"`  // 最近创建的运行
	Ranking []PipelineRanked `json:"ranking"` // 按运行次数排序
}

type PeriodStat struct {
	Runs        int     `json:"runs"`
	Success     int     `json:"success"`
	Failed      int     `json:"failed"`
	Canceled    int     `json:"canceled"`
	Running     int     `json:"running"`      // 含待审批
	Queued      int     `json:"queued"`       // 含等待上游的 pending
	SuccessRate float64 `json:"success_rate"` // 成功 / (成功 + 失败)，无数据为 -1
	AvgDuration float64 `json:"avg_duration"` // 秒，成功的运行；无数据为 0
	SuccessRuns int     `json:"success_runs"` // 计算 AvgDuration 用到的运行数
}

type DashDay struct {
	Date     string `json:"date"`
	Success  int    `json:"success"`
	Failed   int    `json:"failed"`
	Canceled int    `json:"canceled"`
}

type DashRun struct {
	*model.Run
	PipelineName string `json:"pipeline_name"`
}

type PipelineRanked struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Runs        int          `json:"runs"`
	SuccessRate float64      `json:"success_rate"`
	AvgDuration float64      `json:"avg_duration"`
	LastAt      time.Time    `json:"last_at"`
	LastStatus  model.Status `json:"last_status"`
}

const (
	dashboardList    = 10
	runnerOnlineTime = time.Minute // 与界面上 Runner 的「在线」判定一致
)

// Dashboard 汇总租户最近 days 天的运行；user 为当前用户名（统计「我创建的」），visible 筛选用户能看到的流水线（nil 为全部）。
func (e *Engine) Dashboard(ctx context.Context, tenantID, user string, days int, visible func(*model.Pipeline) bool) (*Dashboard, error) {
	now := e.now()
	since := now.AddDate(0, 0, -days)
	prevSince := since.AddDate(0, 0, -days)
	all, err := e.store.ListPipelines(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	ps := all[:0]
	for _, p := range all {
		if visible == nil || visible(p) {
			ps = append(ps, p)
		}
	}
	d := &Dashboard{Days: days, Pipelines: len(ps), Active: []DashRun{}, Recent: []DashRun{}, Ranking: []PipelineRanked{}}

	daily := make(map[string]*DashDay, days)
	for i := days - 1; i >= 0; i-- {
		date := now.AddDate(0, 0, -i).Format(time.DateOnly)
		d.Daily = append(d.Daily, DashDay{Date: date})
	}
	for i := range d.Daily {
		daily[d.Daily[i].Date] = &d.Daily[i]
	}

	var cur, prev periodAcc
	var recent []DashRun
	for _, p := range ps {
		if p.CreatedBy != "" && p.CreatedBy == user {
			d.Mine++
		}
		runs, err := e.store.RunsSince(ctx, p.ID, prevSince, maxStatsRuns)
		if err != nil {
			return nil, err
		}
		var rank periodAcc
		for _, r := range runs {
			if r.CreatedAt.Before(since) {
				prev.add(r)
				continue
			}
			cur.add(r)
			rank.add(r)
			if day := daily[r.CreatedAt.In(now.Location()).Format(time.DateOnly)]; day != nil {
				switch r.Status {
				case model.StatusSuccess:
					day.Success++
				case model.StatusFailed:
					day.Failed++
				case model.StatusCanceled:
					day.Canceled++
				}
			}
			dr := DashRun{Run: r, PipelineName: p.Name}
			if !r.Status.Terminal() {
				d.Active = append(d.Active, dr)
			}
			recent = append(recent, dr)
		}
		if rank.stat.Runs > 0 {
			st := rank.result()
			// runs 新的在前
			d.Ranking = append(d.Ranking, PipelineRanked{ID: p.ID, Name: p.Name, Runs: st.Runs, SuccessRate: st.SuccessRate,
				AvgDuration: st.AvgDuration, LastAt: runs[0].CreatedAt, LastStatus: runs[0].Status})
		}
	}
	d.Current, d.Previous = cur.result(), prev.result()

	sort.SliceStable(d.Active, func(i, j int) bool { return d.Active[i].CreatedAt.Before(d.Active[j].CreatedAt) })
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].CreatedAt.After(recent[j].CreatedAt) })
	if len(recent) > dashboardList {
		recent = recent[:dashboardList]
	}
	if recent != nil {
		d.Recent = recent
	}
	sort.SliceStable(d.Ranking, func(i, j int) bool {
		if d.Ranking[i].Runs != d.Ranking[j].Runs {
			return d.Ranking[i].Runs > d.Ranking[j].Runs
		}
		return d.Ranking[i].LastAt.After(d.Ranking[j].LastAt)
	})
	if len(d.Ranking) > dashboardList {
		d.Ranking = d.Ranking[:dashboardList]
	}

	runners, err := e.store.ListRunners(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	d.RunnersTotal = len(runners)
	for _, r := range runners {
		if r.LastSeenAt != nil && now.Sub(*r.LastSeenAt) < runnerOnlineTime {
			d.RunnersOnline++
		}
	}
	return d, nil
}

type periodAcc struct {
	stat  PeriodStat
	total float64
}

func (a *periodAcc) add(r *model.Run) {
	a.stat.Runs++
	switch r.Status {
	case model.StatusSuccess:
		a.stat.Success++
		if r.StartedAt != nil && r.FinishedAt != nil {
			a.total += r.FinishedAt.Sub(*r.StartedAt).Seconds()
			a.stat.SuccessRuns++
		}
	case model.StatusFailed:
		a.stat.Failed++
	case model.StatusCanceled:
		a.stat.Canceled++
	case model.StatusRunning, model.StatusWaiting:
		a.stat.Running++
	default:
		a.stat.Queued++
	}
}

func (a *periodAcc) result() PeriodStat {
	st := a.stat
	st.SuccessRate = -1
	if n := st.Success + st.Failed; n > 0 {
		st.SuccessRate = float64(st.Success) / float64(n)
	}
	if st.SuccessRuns > 0 {
		st.AvgDuration = a.total / float64(st.SuccessRuns)
	}
	return st
}
