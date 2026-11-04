package engine

import (
	"context"
	"math"
	"slices"
	"sort"
	"time"

	"flow/internal/model"
	"flow/internal/store"
)

// PipelineStats 是流水线在一段时间内的运行统计。
type PipelineStats struct {
	Days        int     `json:"days"`
	Runs        int     `json:"runs"`
	Success     int     `json:"success"`
	Failed      int     `json:"failed"`
	Canceled    int     `json:"canceled"`
	SuccessRate float64 `json:"success_rate"` // 成功 / (成功 + 失败)，无数据为 -1
	AvgDuration float64 `json:"avg_duration"` // 秒，已结束的运行
	P50Duration float64 `json:"p50_duration"`
	P90Duration float64 `json:"p90_duration"`
	// MeanRecovery 是平均恢复时间：从失败运行结束到之后第一次成功运行结束（秒），没有可计算的为 0。
	MeanRecovery float64      `json:"mean_recovery"`
	Daily        []DayStat    `json:"daily"`
	Recent       []RunPoint   `json:"recent"` // 时间正序
	Jobs         []JobStat    `json:"jobs"`
	Deploys      []DeployStat `json:"deploys"`
	Tests        *TestTrend   `json:"tests,omitempty"`
	// ByTrigger / ByUser 按触发方式 / 触发人汇总运行结果；Heatmap[星期一=0][时段] 为各 3 小时时段的运行次数。
	ByTrigger []CountStat `json:"by_trigger"`
	ByUser    []CountStat `json:"by_user"`
	Heatmap   [7][8]int   `json:"heatmap"`
	// P95Duration 为已结束运行耗时的 95 分位（秒）。
	P95Duration float64 `json:"p95_duration"`
	Truncated   bool    `json:"truncated,omitempty"` // 运行过多，只统计了最近的一部分
}

// CountStat 是按某个维度汇总的运行结果。
type CountStat struct {
	Key      string `json:"key"`
	Runs     int    `json:"runs"`
	Success  int    `json:"success"`
	Failed   int    `json:"failed"`
	Canceled int    `json:"canceled"`
}

func addCount(m map[string]*CountStat, order *[]string, key string, s model.Status) {
	c := m[key]
	if c == nil {
		c = &CountStat{Key: key}
		m[key] = c
		*order = append(*order, key)
	}
	c.Runs++
	switch s {
	case model.StatusSuccess:
		c.Success++
	case model.StatusFailed:
		c.Failed++
	case model.StatusCanceled:
		c.Canceled++
	}
}

func sortedCounts(m map[string]*CountStat, order []string) []CountStat {
	out := make([]CountStat, 0, len(order))
	for _, k := range order {
		out = append(out, *m[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Runs > out[j].Runs })
	return out
}

type DayStat struct {
	Date    string `json:"date"` // YYYY-MM-DD（Server 时区）
	Total   int    `json:"total"`
	Success int    `json:"success"`
	Failed  int    `json:"failed"`
}

type RunPoint struct {
	ID       string             `json:"id"`
	Number   int                `json:"number"`
	Status   model.Status       `json:"status"`
	Duration float64            `json:"duration"`
	At       time.Time          `json:"at"`
	Tests    *model.TestSummary `json:"tests,omitempty"`
}

type JobStat struct {
	Name        string  `json:"name"`
	Runs        int     `json:"runs"` // 执行过的次数（不含跳过 / 取消前未开始）
	Failed      int     `json:"failed"`
	AvgDuration float64 `json:"avg_duration"`
	P90Duration float64 `json:"p90_duration"`
}

type DeployStat struct {
	Environment string    `json:"environment"`
	Count       int       `json:"count"`
	Success     int       `json:"success"`
	LastAt      time.Time `json:"last_at"`
}

type TestTrend struct {
	Runs        int     `json:"runs"`         // 有测试报告的运行数
	AvgTotal    float64 `json:"avg_total"`    // 平均用例数
	FailedRuns  int     `json:"failed_runs"`  // 有失败用例的运行数
	LatestTotal int     `json:"latest_total"` // 最近一次的用例数
}

const maxStatsRuns = 2000

// Stats 统计流水线最近 days 天的运行。
func (e *Engine) Stats(ctx context.Context, p *model.Pipeline, days int) (*PipelineStats, error) {
	now := e.now()
	since := now.AddDate(0, 0, -days)
	runs, err := e.store.RunsSince(ctx, p.ID, since, maxStatsRuns)
	if err != nil {
		return nil, err
	}
	st := &PipelineStats{Days: days, Runs: len(runs), SuccessRate: -1, Truncated: len(runs) == maxStatsRuns}
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	jobs, err := e.store.JobsOfRuns(ctx, ids)
	if err != nil {
		return nil, err
	}
	tests, err := e.store.TestSummaries(ctx, ids)
	if err != nil {
		return nil, err
	}

	// 按天汇总（含没有运行的日期，便于画图）。
	daily := map[string]*DayStat{}
	for d := 0; d < days; d++ {
		date := now.AddDate(0, 0, -d).Format(time.DateOnly)
		daily[date] = &DayStat{Date: date}
	}
	var durations []float64
	byTrigger, byUser := map[string]*CountStat{}, map[string]*CountStat{}
	var triggerOrder, userOrder []string
	chrono := slices.Clone(runs)
	slices.Reverse(chrono)
	var failedAt *time.Time
	var recoveries []float64
	for _, r := range chrono {
		day := daily[r.CreatedAt.In(now.Location()).Format(time.DateOnly)]
		if day != nil {
			day.Total++
		}
		switch r.Status {
		case model.StatusSuccess:
			st.Success++
			if day != nil {
				day.Success++
			}
			if failedAt != nil && r.FinishedAt != nil {
				recoveries = append(recoveries, r.FinishedAt.Sub(*failedAt).Seconds())
				failedAt = nil
			}
		case model.StatusFailed:
			st.Failed++
			if day != nil {
				day.Failed++
			}
			if failedAt == nil && r.FinishedAt != nil {
				failedAt = r.FinishedAt
			}
		case model.StatusCanceled:
			st.Canceled++
		}
		addCount(byTrigger, &triggerOrder, r.Trigger, r.Status)
		user := r.CreatedBy
		if user == "" {
			user = "（自动触发）"
		}
		addCount(byUser, &userOrder, user, r.Status)
		local := r.CreatedAt.In(now.Location())
		st.Heatmap[(int(local.Weekday())+6)%7][local.Hour()/3]++
		pt := RunPoint{ID: r.ID, Number: r.Number, Status: r.Status, At: r.CreatedAt}
		if r.StartedAt != nil && r.FinishedAt != nil {
			pt.Duration = r.FinishedAt.Sub(*r.StartedAt).Seconds()
			if r.Status == model.StatusSuccess || r.Status == model.StatusFailed {
				durations = append(durations, pt.Duration)
			}
		}
		if t, ok := tests[r.ID]; ok {
			pt.Tests = &t
		}
		st.Recent = append(st.Recent, pt)
	}
	if n := len(st.Recent); n > 60 {
		st.Recent = st.Recent[n-60:]
	}
	if st.Success+st.Failed > 0 {
		st.SuccessRate = float64(st.Success) / float64(st.Success+st.Failed)
	}
	st.AvgDuration, st.P50Duration, st.P90Duration = mean(durations), percentile(durations, 0.5), percentile(durations, 0.9)
	st.P95Duration = percentile(durations, 0.95)
	st.ByTrigger, st.ByUser = sortedCounts(byTrigger, triggerOrder), sortedCounts(byUser, userOrder)
	st.MeanRecovery = mean(recoveries)
	for _, d := range daily {
		st.Daily = append(st.Daily, *d)
	}
	sort.Slice(st.Daily, func(i, j int) bool { return st.Daily[i].Date < st.Daily[j].Date })

	// 任务：按名称汇总执行次数、失败与耗时（矩阵展开的任务各自统计）。
	type acc struct {
		runs, failed int
		durs         []float64
	}
	byJob := map[string]*acc{}
	var order []string
	for _, j := range jobs {
		if j.StartedAt == nil || j.FinishedAt == nil || j.Kind == model.JobKindApproval {
			continue
		}
		a := byJob[j.Name]
		if a == nil {
			a = &acc{}
			byJob[j.Name] = a
			order = append(order, j.Name)
		}
		a.runs++
		if j.Status == model.StatusFailed {
			a.failed++
		}
		a.durs = append(a.durs, j.FinishedAt.Sub(*j.StartedAt).Seconds())
	}
	for _, name := range order {
		a := byJob[name]
		st.Jobs = append(st.Jobs, JobStat{Name: name, Runs: a.runs, Failed: a.failed, AvgDuration: mean(a.durs), P90Duration: percentile(a.durs, 0.9)})
	}
	sort.SliceStable(st.Jobs, func(i, j int) bool { return st.Jobs[i].AvgDuration > st.Jobs[j].AvgDuration })

	// 部署：按环境汇总。
	deps, err := e.store.ListDeployments(ctx, p.TenantID, store.DeploymentFilter{PipelineID: p.ID, Limit: 1000})
	if err != nil {
		return nil, err
	}
	byEnv := map[string]*DeployStat{}
	for _, d := range deps {
		if d.StartedAt.Before(since) {
			continue
		}
		name := d.EnvironmentName
		if name == "" {
			name = d.GroupName
		}
		ds := byEnv[name]
		if ds == nil {
			ds = &DeployStat{Environment: name, LastAt: d.StartedAt}
			byEnv[name] = ds
		}
		ds.Count++
		if d.Status == model.StatusSuccess {
			ds.Success++
		}
	}
	for _, ds := range byEnv {
		st.Deploys = append(st.Deploys, *ds)
	}
	sort.Slice(st.Deploys, func(i, j int) bool { return st.Deploys[i].LastAt.After(st.Deploys[j].LastAt) })

	if len(tests) > 0 {
		tt := &TestTrend{Runs: len(tests)}
		total := 0
		for _, r := range runs { // 新的在前
			if t, ok := tests[r.ID]; ok {
				if tt.LatestTotal == 0 {
					tt.LatestTotal = t.Total
				}
				total += t.Total
				if t.Failed > 0 {
					tt.FailedRuns++
				}
			}
		}
		tt.AvgTotal = float64(total) / float64(len(tests))
		st.Tests = tt
	}
	if st.Daily == nil {
		st.Daily = []DayStat{}
	}
	if st.Recent == nil {
		st.Recent = []RunPoint{}
	}
	if st.Jobs == nil {
		st.Jobs = []JobStat{}
	}
	if st.Deploys == nil {
		st.Deploys = []DeployStat{}
	}
	return st, nil
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// percentile 用最近秩法计算分位数。
func percentile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}
