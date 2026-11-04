package store

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"flow/internal/model"
)

// Memory 是进程内存储，重启即丢失，仅用于开发和测试。它同时实现 Store 与 Tx。
type Memory struct {
	txMu          sync.Mutex // 串行化 Atomic
	mu            sync.RWMutex
	pipelines     map[string]*model.Pipeline
	pipelineOrder []string
	revisions     map[string][]*model.PipelineRevision // 按版本正序
	runs          map[string]*model.Run
	runOrder      []string
	runCount      map[string]int
	jobs          map[string]*model.Job
	jobOrder      []string
	runJobs       map[string][]string
	logs          map[string][]byte
	triggers      map[string]*model.Trigger
	triggerOrder  []string
	deliveries    map[deliveryKey]delivery
	templates     map[string]*model.Template
	templateRevs  map[string][]*model.TemplateRevision // 按版本正序
	secrets       map[string]*model.Secret             // key: tenant/pipeline/name
	runners       map[string]*model.Runner
	runnerOrder   []string
	artifacts     map[string]*model.Artifact
	artOrder      []string
	tenants       map[string]*model.Tenant
	tenantOrder   []string
	users         map[string]*model.User
	userOrder     []string
	members       map[string]*model.Membership // key: tenant/user
	tokens        map[string]*model.Token
	tokenOrder    []string
	favorites     map[[2]string]time.Time      // key: user / pipeline
	hostData      *hostData                    // 环境、主机组、主机、部署记录（hosts.go）
	accessData    *accessData                  // 流水线成员与自定义角色（access.go）
	varData       *varData                     // 变量组与服务连接（vars.go）
	groups        map[[2]string]bool           // 显式创建的流水线分组：tenant / name（groups.go）
	hostTasks     map[string]*model.HostTask   // Agent 主机的部署任务（hosttasks.go）
	reports       map[string]*model.TestReport // key: job
	audit         []*model.AuditEvent          // 按时间正序
}

func NewMemory() *Memory {
	now := time.Now()
	return &Memory{
		pipelines:    make(map[string]*model.Pipeline),
		revisions:    make(map[string][]*model.PipelineRevision),
		runs:         make(map[string]*model.Run),
		runCount:     make(map[string]int),
		jobs:         make(map[string]*model.Job),
		runJobs:      make(map[string][]string),
		logs:         make(map[string][]byte),
		triggers:     make(map[string]*model.Trigger),
		deliveries:   make(map[deliveryKey]delivery),
		templates:    make(map[string]*model.Template),
		templateRevs: make(map[string][]*model.TemplateRevision),
		secrets:      make(map[string]*model.Secret),
		runners:      make(map[string]*model.Runner),
		artifacts:    make(map[string]*model.Artifact),
		// 与迁移脚本一致，预置默认租户。
		tenants:     map[string]*model.Tenant{"default": {ID: "default", Name: "Default", CreatedAt: now}},
		tenantOrder: []string{"default"},
		users:       make(map[string]*model.User),
		members:     make(map[string]*model.Membership),
		tokens:      make(map[string]*model.Token),
		favorites:   make(map[[2]string]time.Time),
	}
}

func (m *Memory) Atomic(_ context.Context, fn func(Tx) error) error {
	m.txMu.Lock()
	defer m.txMu.Unlock()
	return fn(m)
}

func clone[T any](v *T) *T {
	c := *v
	return &c
}

func (m *Memory) nameTaken(p *model.Pipeline) bool {
	for _, other := range m.pipelines {
		if other.ID != p.ID && other.TenantID == p.TenantID && other.Name == p.Name {
			return true
		}
	}
	return false
}

func (m *Memory) CreatePipeline(_ context.Context, p *model.Pipeline) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tenants[p.TenantID] == nil {
		return ErrNotFound
	}
	if m.nameTaken(p) {
		return ErrConflict
	}
	p.Version = 1
	m.pipelines[p.ID] = clone(p)
	m.pipelineOrder = append(m.pipelineOrder, p.ID)
	m.addRevision(p, p.CreatedAt)
	return nil
}

func (m *Memory) addRevision(p *model.Pipeline, at time.Time) {
	m.revisions[p.ID] = append(m.revisions[p.ID], &model.PipelineRevision{
		PipelineID: p.ID, Version: p.Version, Name: p.Name, YAML: p.YAML, CreatedBy: p.UpdatedBy,
		RestoredFrom: p.RestoredFrom, CreatedAt: at,
	})
}

func (m *Memory) UpdatePipeline(_ context.Context, p *model.Pipeline) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.pipelines[p.ID]
	if !ok {
		return ErrNotFound
	}
	if m.nameTaken(p) || cur.Version != p.Version {
		return ErrConflict
	}
	if cur.DeletedAt != nil {
		return ErrNotFound
	}
	p.Version++
	// 列表元信息不随配置保存
	p.Group, p.CreatedBy, p.Description, p.Archived, p.DeletedAt, p.IsPublic = cur.Group, cur.CreatedBy, cur.Description, cur.Archived, nil, cur.IsPublic
	m.pipelines[p.ID] = clone(p)
	m.pipelines[p.ID].RestoredFrom = 0
	m.addRevision(p, p.UpdatedAt)
	return nil
}

func (m *Memory) SetPipelineGroup(_ context.Context, id, group string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pipelines[id]
	if !ok {
		return ErrNotFound
	}
	p.Group = group
	return nil
}

func (m *Memory) SetPipelineInfo(_ context.Context, id, description string, archived bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pipelines[id]
	if !ok {
		return ErrNotFound
	}
	p.Description, p.Archived = description, archived
	return nil
}

func (m *Memory) SetPipelineDeleted(_ context.Context, id string, at *time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pipelines[id]
	if !ok {
		return ErrNotFound
	}
	p.DeletedAt = nil
	if at != nil {
		p.DeletedAt = clone(at)
	}
	return nil
}

func (m *Memory) SetFavorite(_ context.Context, userID, pipelineID string, on bool, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pipelines[pipelineID]; !ok {
		return ErrNotFound
	}
	if on {
		if _, ok := m.favorites[[2]string{userID, pipelineID}]; !ok {
			m.favorites[[2]string{userID, pipelineID}] = at
		}
	} else {
		delete(m.favorites, [2]string{userID, pipelineID})
	}
	return nil
}

func (m *Memory) ListFavorites(_ context.Context, userID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0)
	for k := range m.favorites {
		if k[0] == userID {
			out = append(out, k[1])
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *Memory) RecentRuns(_ context.Context, tenantID string, perPipeline int) (map[string][]*model.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string][]*model.Run)
	for i := len(m.runOrder) - 1; i >= 0; i-- {
		r := m.runs[m.runOrder[i]]
		if r.TenantID == tenantID && len(out[r.PipelineID]) < perPipeline {
			out[r.PipelineID] = append(out[r.PipelineID], r.Clone())
		}
	}
	return out, nil
}

func (m *Memory) ListRevisions(_ context.Context, pipelineID string, limit int) ([]*model.PipelineRevision, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	revs := m.revisions[pipelineID]
	out := make([]*model.PipelineRevision, 0, min(limit, len(revs)))
	for i := len(revs) - 1; i >= 0 && len(out) < limit; i-- {
		r := clone(revs[i])
		r.YAML = ""
		out = append(out, r)
	}
	return out, nil
}

func (m *Memory) GetRevision(_ context.Context, pipelineID string, version int) (*model.PipelineRevision, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.revisions[pipelineID] {
		if r.Version == version {
			return clone(r), nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) GetPipeline(_ context.Context, id string) (*model.Pipeline, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.pipelines[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(p), nil
}

func (m *Memory) ListPipelines(_ context.Context, tenantID string) ([]*model.Pipeline, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Pipeline, 0)
	for _, id := range m.pipelineOrder {
		if p := m.pipelines[id]; p.TenantID == tenantID && p.DeletedAt == nil {
			out = append(out, clone(p))
		}
	}
	return out, nil
}

func (m *Memory) ListDeletedPipelines(_ context.Context, tenantID string) ([]*model.Pipeline, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Pipeline, 0)
	for _, id := range m.pipelineOrder {
		if p := m.pipelines[id]; p.TenantID == tenantID && p.DeletedAt != nil {
			out = append(out, clone(p))
		}
	}
	slices.SortStableFunc(out, func(a, b *model.Pipeline) int { return b.DeletedAt.Compare(*a.DeletedAt) })
	return out, nil
}

func (m *Memory) DeletePipeline(_ context.Context, id string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pipelines[id]; !ok {
		return nil, ErrNotFound
	}
	delete(m.pipelines, id)
	delete(m.revisions, id)
	m.dropAccess(id, "")
	if m.hostData != nil {
		m.hostData.deployments = slices.DeleteFunc(m.hostData.deployments, func(d *model.Deployment) bool { return d.PipelineID == id })
	}
	for k := range m.favorites {
		if k[1] == id {
			delete(m.favorites, k)
		}
	}
	m.pipelineOrder = slices.DeleteFunc(m.pipelineOrder, func(x string) bool { return x == id })
	var blobs []string
	m.runOrder = slices.DeleteFunc(m.runOrder, func(rid string) bool {
		if m.runs[rid].PipelineID != id {
			return false
		}
		blobs = append(blobs, m.deleteRunData(rid)...)
		return true
	})
	m.triggerOrder = slices.DeleteFunc(m.triggerOrder, func(tid string) bool {
		if m.triggers[tid].PipelineID == id {
			delete(m.triggers, tid)
			m.deleteDeliveries(func(k deliveryKey, _ delivery) bool { return k.trigger == tid })
			return true
		}
		return false
	})
	for k, s := range m.secrets {
		if s.PipelineID == id {
			delete(m.secrets, k)
		}
	}
	return blobs, nil
}

func (m *Memory) CreateRun(_ context.Context, r *model.Run, jobs []*model.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pipelines[r.PipelineID]; !ok {
		return ErrNotFound
	}
	m.runCount[r.PipelineID]++
	r.Number = m.runCount[r.PipelineID]
	m.runs[r.ID] = r.Clone()
	m.runOrder = append(m.runOrder, r.ID)
	for _, j := range jobs {
		m.jobs[j.ID] = j.Clone()
		m.jobOrder = append(m.jobOrder, j.ID)
		m.runJobs[r.ID] = append(m.runJobs[r.ID], j.ID)
	}
	return nil
}

func (m *Memory) GetRun(_ context.Context, id string) (*model.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return r.Clone(), nil
}

// LockRun 在内存实现中等同 GetRun，互斥由 Atomic 保证。
func (m *Memory) LockRun(ctx context.Context, id string) (*model.Run, error) {
	return m.GetRun(ctx, id)
}

func (m *Memory) ListRuns(_ context.Context, pipelineID string, limit int) ([]*model.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Run, 0)
	for i := len(m.runOrder) - 1; i >= 0 && len(out) < limit; i-- {
		if r := m.runs[m.runOrder[i]]; r.PipelineID == pipelineID {
			out = append(out, r.Clone())
		}
	}
	return out, nil
}

// LockConcurrency 在内存实现中无需加锁：Atomic 已串行化所有事务。
func (m *Memory) LockConcurrency(context.Context, string, string) error { return nil }

func (m *Memory) ActiveRuns(_ context.Context, tenantID, group string) ([]*model.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Run, 0)
	for _, id := range m.runOrder {
		if r := m.runs[id]; r.TenantID == tenantID && r.ConcurrencyGroup == group && r.FinishedAt == nil {
			out = append(out, r.Clone())
		}
	}
	return out, nil
}

func (m *Memory) UpdateRun(_ context.Context, r *model.Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[r.ID]; !ok {
		return ErrNotFound
	}
	m.runs[r.ID] = r.Clone()
	return nil
}

func (m *Memory) NextPendingNotify(_ context.Context) (*model.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range m.runOrder {
		if r := m.runs[id]; !r.NotifyDone && r.Status.Terminal() {
			return r.Clone(), nil
		}
	}
	return nil, nil
}

func (m *Memory) GetJob(_ context.Context, id string) (*model.Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return j.Clone(), nil
}

func (m *Memory) ListJobs(_ context.Context, runID string) ([]*model.Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := m.runJobs[runID]
	out := make([]*model.Job, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.jobs[id].Clone())
	}
	return out, nil
}

func (m *Memory) NextQueuedJob(_ context.Context, f ClaimFilter) (*model.Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range m.jobOrder {
		j := m.jobs[id]
		if j.Status != model.StatusQueued || (f.TenantID != "" && j.TenantID != f.TenantID) {
			continue
		}
		if !subset(j.RunsOn, f.Labels) {
			continue
		}
		return j.Clone(), nil
	}
	return nil, nil
}

func subset(need, have []string) bool {
	for _, l := range need {
		if !slices.Contains(have, l) {
			return false
		}
	}
	return true
}

func (m *Memory) ExpiredJobRunIDs(_ context.Context, now time.Time) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := make(map[string]bool)
	var out []string
	for _, id := range m.jobOrder {
		j := m.jobs[id]
		if j.Status == model.StatusRunning && j.LeaseUntil.Before(now) && !seen[j.RunID] {
			seen[j.RunID] = true
			out = append(out, j.RunID)
		}
	}
	return out, nil
}

func (m *Memory) UpdateJob(_ context.Context, j *model.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[j.ID]; !ok {
		return ErrNotFound
	}
	m.jobs[j.ID] = j.Clone()
	return nil
}

func (m *Memory) AppendLog(_ context.Context, jobID string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[jobID]; !ok {
		return ErrNotFound
	}
	m.logs[jobID] = append(m.logs[jobID], data...)
	return nil
}

func (m *Memory) ReadLog(_ context.Context, jobID string, offset int) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.jobs[jobID]; !ok {
		return nil, ErrNotFound
	}
	data := m.logs[jobID]
	if offset < 0 || offset >= len(data) {
		return []byte{}, nil
	}
	return bytes.Clone(data[offset:min(len(data), offset+MaxLogRead)]), nil
}

func (m *Memory) CreateTrigger(_ context.Context, t *model.Trigger) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pipelines[t.PipelineID]; !ok {
		return ErrNotFound
	}
	if _, ok := m.triggers[t.ID]; ok {
		return ErrConflict
	}
	m.triggers[t.ID] = t.Clone()
	m.triggerOrder = append(m.triggerOrder, t.ID)
	return nil
}

func (m *Memory) UpdateTrigger(_ context.Context, t *model.Trigger) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.triggers[t.ID]
	if !ok {
		return ErrNotFound
	}
	c := t.Clone()
	c.LastFiredAt, c.LastError = cur.LastFiredAt, cur.LastError
	m.triggers[t.ID] = c
	return nil
}

func (m *Memory) DeleteTrigger(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.triggers[id]; !ok {
		return ErrNotFound
	}
	delete(m.triggers, id)
	m.triggerOrder = slices.DeleteFunc(m.triggerOrder, func(x string) bool { return x == id })
	m.deleteDeliveries(func(k deliveryKey, _ delivery) bool { return k.trigger == id })
	return nil
}

func (m *Memory) GetTrigger(_ context.Context, id string) (*model.Trigger, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.triggers[id]
	if !ok {
		return nil, ErrNotFound
	}
	return t.Clone(), nil
}

func (m *Memory) ListTriggers(_ context.Context, pipelineID string) ([]*model.Trigger, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Trigger, 0)
	for _, id := range m.triggerOrder {
		if t := m.triggers[id]; t.PipelineID == pipelineID {
			out = append(out, t.Clone())
		}
	}
	return out, nil
}

func (m *Memory) NextDueTrigger(_ context.Context, now time.Time) (*model.Trigger, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var due *model.Trigger
	for _, id := range m.triggerOrder {
		t := m.triggers[id]
		if t.Kind == model.TriggerCron && t.Enabled && t.NextRunAt != nil && !t.NextRunAt.After(now) &&
			(due == nil || t.NextRunAt.Before(*due.NextRunAt)) {
			due = t
		}
	}
	if due == nil {
		return nil, nil
	}
	return due.Clone(), nil
}

func (m *Memory) MarkTriggerFired(_ context.Context, id string, next *time.Time, at time.Time, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.triggers[id]
	if !ok {
		return ErrNotFound
	}
	t.NextRunAt, t.LastFiredAt, t.LastError = next, &at, errMsg
	return nil
}

type deliveryKey struct{ trigger, id string }

type delivery struct {
	runID string
	at    time.Time
}

func (m *Memory) ClaimDelivery(_ context.Context, triggerID, deliveryID string, at time.Time) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.triggers[triggerID]; !ok {
		return "", false, ErrNotFound
	}
	k := deliveryKey{triggerID, deliveryID}
	if d, ok := m.deliveries[k]; ok {
		return d.runID, true, nil
	}
	m.deliveries[k] = delivery{at: at}
	return "", false, nil
}

func (m *Memory) SetDeliveryRun(_ context.Context, triggerID, deliveryID, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := deliveryKey{triggerID, deliveryID}
	d, ok := m.deliveries[k]
	if !ok {
		return ErrNotFound
	}
	d.runID = runID
	m.deliveries[k] = d
	return nil
}

func (m *Memory) DeleteDeliveriesBefore(_ context.Context, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteDeliveries(func(_ deliveryKey, d delivery) bool { return d.at.Before(before) })
	return nil
}

// deleteDeliveries 调用方须持有 m.mu 写锁。
func (m *Memory) deleteDeliveries(del func(deliveryKey, delivery) bool) {
	maps.DeleteFunc(m.deliveries, del)
}

func secretKey(tenantID, pipelineID, name string) string {
	return tenantID + "/" + pipelineID + "/" + name
}

func (m *Memory) PutSecret(_ context.Context, s *model.Secret) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.PipelineID != "" && m.pipelines[s.PipelineID] == nil {
		return ErrNotFound
	}
	k := secretKey(s.TenantID, s.PipelineID, s.Name)
	c := clone(s)
	c.Value = bytes.Clone(s.Value)
	if cur, ok := m.secrets[k]; ok {
		c.ID, c.CreatedAt = cur.ID, cur.CreatedAt
		s.ID, s.CreatedAt = cur.ID, cur.CreatedAt
	}
	m.secrets[k] = c
	return nil
}

func (m *Memory) DeleteSecret(_ context.Context, tenantID, pipelineID, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := secretKey(tenantID, pipelineID, name)
	if _, ok := m.secrets[k]; !ok {
		return ErrNotFound
	}
	delete(m.secrets, k)
	return nil
}

func (m *Memory) ListSecrets(_ context.Context, tenantID, pipelineID string) ([]*model.Secret, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Secret, 0)
	for _, s := range m.secrets {
		if s.TenantID == tenantID && s.PipelineID == pipelineID {
			c := clone(s)
			c.Value = bytes.Clone(s.Value)
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Memory) CreateRunner(_ context.Context, r *model.Runner) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.runners {
		if (o.TenantID == r.TenantID && o.Name == r.Name) || o.TokenHash == r.TokenHash {
			return ErrConflict
		}
	}
	c := clone(r)
	c.Labels, c.Config = slices.Clone(r.Labels), maps.Clone(r.Config)
	if c.Kind == "" {
		c.Kind, r.Kind = "self", "self"
	}
	m.runners[r.ID] = c
	m.runnerOrder = append(m.runnerOrder, r.ID)
	return nil
}

func (m *Memory) GetRunner(_ context.Context, id string) (*model.Runner, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runners[id]
	if !ok {
		return nil, ErrNotFound
	}
	c := clone(r)
	c.Labels = slices.Clone(r.Labels)
	return c, nil
}

func (m *Memory) GetRunnerByTokenHash(ctx context.Context, hash string) (*model.Runner, error) {
	m.mu.RLock()
	var id string
	for _, r := range m.runners {
		if r.TokenHash == hash {
			id = r.ID
		}
	}
	m.mu.RUnlock()
	if id == "" {
		return nil, ErrNotFound
	}
	return m.GetRunner(ctx, id)
}

func (m *Memory) ListRunners(_ context.Context, tenantID string) ([]*model.Runner, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Runner, 0)
	for _, id := range m.runnerOrder {
		if r := m.runners[id]; r.TenantID == tenantID {
			c := clone(r)
			c.Labels = slices.Clone(r.Labels)
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *Memory) DeleteRunner(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runners[id]; !ok {
		return ErrNotFound
	}
	delete(m.runners, id)
	m.runnerOrder = slices.DeleteFunc(m.runnerOrder, func(x string) bool { return x == id })
	return nil
}

func (m *Memory) TouchRunner(_ context.Context, id string, labels []string, info *model.NodeInfo, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runners[id]
	if !ok {
		return ErrNotFound
	}
	r.Labels, r.LastSeenAt = slices.Clone(labels), &at
	if info != nil {
		r.Info = *info
	}
	return nil
}

func (m *Memory) UpdateRunner(_ context.Context, r *model.Runner) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.runners[r.ID]
	if !ok {
		return ErrNotFound
	}
	for _, x := range m.runners {
		if x.ID != r.ID && x.TenantID == r.TenantID && x.Name == r.Name {
			return ErrConflict
		}
	}
	o.Name, o.Config, o.Secret, o.TokenHash = r.Name, maps.Clone(r.Config), r.Secret, r.TokenHash
	return nil
}

func (m *Memory) PutArtifact(_ context.Context, a *model.Artifact) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs[a.JobID] == nil {
		return "", ErrNotFound
	}
	if a.Format == "" {
		a.Format = "tar.gz"
	}
	for id, o := range m.artifacts {
		if o.JobID == a.JobID && o.Name == a.Name {
			delete(m.artifacts, id)
			m.artifacts[a.ID] = clone(a)
			m.artOrder[slices.Index(m.artOrder, id)] = a.ID
			return o.BlobKey, nil
		}
	}
	m.artifacts[a.ID] = clone(a)
	m.artOrder = append(m.artOrder, a.ID)
	return "", nil
}

// deleteRunData 删除运行的 Job、日志、产物与运行本身（不含 runOrder），返回产物 blob key。调用方须持有写锁。
func (m *Memory) deleteRunData(rid string) []string {
	var blobs []string
	for _, jid := range m.runJobs[rid] {
		delete(m.jobs, jid)
		delete(m.logs, jid)
		delete(m.reports, jid)
	}
	m.jobOrder = slices.DeleteFunc(m.jobOrder, func(jid string) bool { return m.jobs[jid] == nil })
	m.artOrder = slices.DeleteFunc(m.artOrder, func(aid string) bool {
		if a := m.artifacts[aid]; a.RunID == rid {
			blobs = append(blobs, a.BlobKey)
			delete(m.artifacts, aid)
			return true
		}
		return false
	})
	delete(m.runJobs, rid)
	delete(m.runs, rid)
	return blobs
}

// latestSuccess 返回流水线最近一次成功运行的 ID。调用方须持有读锁。
func (m *Memory) latestSuccess(pipelineID string) string {
	best, num := "", 0
	for _, r := range m.runs {
		if r.PipelineID == pipelineID && r.Status == model.StatusSuccess && r.Number > num {
			best, num = r.ID, r.Number
		}
	}
	return best
}

func (m *Memory) ExpiredArtifacts(_ context.Context, now time.Time, limit int) ([]*model.Artifact, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Artifact, 0)
	for _, id := range m.artOrder {
		a := m.artifacts[id]
		if a.ExpiresAt == nil || a.ExpiresAt.After(now) || a.RunID == m.latestSuccess(m.runs[a.RunID].PipelineID) {
			continue
		}
		out = append(out, clone(a))
	}
	slices.SortStableFunc(out, func(a, b *model.Artifact) int { return a.ExpiresAt.Compare(*b.ExpiresAt) })
	return out[:min(len(out), limit)], nil
}

func (m *Memory) DeleteArtifacts(_ context.Context, ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		delete(m.artifacts, id)
	}
	m.artOrder = slices.DeleteFunc(m.artOrder, func(id string) bool { return m.artifacts[id] == nil })
	return nil
}

func (m *Memory) PruneRuns(_ context.Context, pipelineID string, keep, limit int) (int, []string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var runs []*model.Run
	for _, r := range m.runs {
		if r.PipelineID == pipelineID {
			runs = append(runs, r)
		}
	}
	slices.SortFunc(runs, func(a, b *model.Run) int { return b.Number - a.Number })
	doomed := map[string]bool{}
	var blobs []string
	for _, r := range runs[min(keep, len(runs)):] {
		if len(doomed) == limit {
			break
		}
		if r.Status.Terminal() {
			doomed[r.ID] = true
			blobs = append(blobs, m.deleteRunData(r.ID)...)
		}
	}
	m.runOrder = slices.DeleteFunc(m.runOrder, func(id string) bool { return doomed[id] })
	return len(doomed), blobs, nil
}

func (m *Memory) GetArtifact(_ context.Context, id string) (*model.Artifact, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.artifacts[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(a), nil
}

func (m *Memory) ListArtifacts(_ context.Context, runID string) ([]*model.Artifact, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Artifact, 0)
	for _, id := range m.artOrder {
		if a := m.artifacts[id]; a.RunID == runID {
			out = append(out, clone(a))
		}
	}
	return out, nil
}

func (m *Memory) CreateTenant(_ context.Context, t *model.Tenant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tenants[t.ID]; ok {
		return ErrConflict
	}
	m.tenants[t.ID] = clone(t)
	m.tenantOrder = append(m.tenantOrder, t.ID)
	return nil
}

func (m *Memory) GetTenant(_ context.Context, id string) (*model.Tenant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tenants[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(t), nil
}

func (m *Memory) ListTenants(_ context.Context) ([]*model.Tenant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Tenant, 0, len(m.tenantOrder))
	for _, id := range m.tenantOrder {
		out = append(out, clone(m.tenants[id]))
	}
	return out, nil
}

func (m *Memory) CreateUser(_ context.Context, u *model.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.users {
		if o.Username == u.Username {
			return ErrConflict
		}
	}
	m.users[u.ID] = clone(u)
	m.userOrder = append(m.userOrder, u.ID)
	return nil
}

func (m *Memory) UpdateUser(_ context.Context, u *model.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[u.ID]; !ok {
		return ErrNotFound
	}
	m.users[u.ID] = clone(u)
	return nil
}

func (m *Memory) DeleteUser(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[id]; !ok {
		return ErrNotFound
	}
	delete(m.users, id)
	m.userOrder = slices.DeleteFunc(m.userOrder, func(x string) bool { return x == id })
	m.dropAccess("", id)
	for k := range m.favorites {
		if k[0] == id {
			delete(m.favorites, k)
		}
	}
	for k, mb := range m.members {
		if mb.UserID == id {
			delete(m.members, k)
		}
	}
	m.tokenOrder = slices.DeleteFunc(m.tokenOrder, func(tid string) bool {
		if m.tokens[tid].UserID == id {
			delete(m.tokens, tid)
			return true
		}
		return false
	})
	return nil
}

func (m *Memory) GetUser(_ context.Context, id string) (*model.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(u), nil
}

func (m *Memory) GetUserByName(_ context.Context, username string) (*model.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, u := range m.users {
		if u.Username == username {
			return clone(u), nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) ListUsers(_ context.Context) ([]*model.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.User, 0, len(m.userOrder))
	for _, id := range m.userOrder {
		out = append(out, clone(m.users[id]))
	}
	return out, nil
}

func (m *Memory) CountUsers(_ context.Context) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.users), nil
}

func (m *Memory) PutMembership(_ context.Context, mb *model.Membership) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tenants[mb.TenantID] == nil || m.users[mb.UserID] == nil {
		return ErrNotFound
	}
	m.members[mb.TenantID+"/"+mb.UserID] = clone(mb)
	return nil
}

func (m *Memory) DeleteMembership(_ context.Context, tenantID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := tenantID + "/" + userID
	if _, ok := m.members[k]; !ok {
		return ErrNotFound
	}
	delete(m.members, k)
	return nil
}

func (m *Memory) listMembers(match func(*model.Membership) bool) []*model.Membership {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Membership, 0)
	for _, mb := range m.members {
		if match(mb) {
			out = append(out, clone(mb))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID < out[j].TenantID
		}
		return out[i].UserID < out[j].UserID
	})
	return out
}

func (m *Memory) ListMembers(_ context.Context, tenantID string) ([]*model.Membership, error) {
	return m.listMembers(func(mb *model.Membership) bool { return mb.TenantID == tenantID }), nil
}

func (m *Memory) UserMemberships(_ context.Context, userID string) ([]*model.Membership, error) {
	return m.listMembers(func(mb *model.Membership) bool { return mb.UserID == userID }), nil
}

func (m *Memory) CreateToken(_ context.Context, t *model.Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.users[t.UserID] == nil {
		return ErrNotFound
	}
	m.tokens[t.ID] = clone(t)
	m.tokenOrder = append(m.tokenOrder, t.ID)
	return nil
}

func (m *Memory) GetTokenByHash(_ context.Context, hash string) (*model.Token, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tokens {
		if t.Hash == hash {
			return clone(t), nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) ListTokens(_ context.Context, userID, kind string) ([]*model.Token, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Token, 0)
	for _, id := range m.tokenOrder {
		if t := m.tokens[id]; t.UserID == userID && t.Kind == kind {
			out = append(out, clone(t))
		}
	}
	return out, nil
}

func (m *Memory) DeleteToken(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tokens[id]; !ok {
		return ErrNotFound
	}
	delete(m.tokens, id)
	m.tokenOrder = slices.DeleteFunc(m.tokenOrder, func(x string) bool { return x == id })
	return nil
}

func (m *Memory) SetTokenTenant(_ context.Context, id, tenantID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok {
		return ErrNotFound
	}
	t.TenantID = tenantID
	return nil
}

func (m *Memory) TouchToken(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok {
		return ErrNotFound
	}
	t.LastUsedAt = &at
	return nil
}

func (m *Memory) DeleteExpiredTokens(_ context.Context, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokenOrder = slices.DeleteFunc(m.tokenOrder, func(id string) bool {
		if t := m.tokens[id]; t.ExpiresAt != nil && !t.ExpiresAt.After(now) {
			delete(m.tokens, id)
			return true
		}
		return false
	})
	return nil
}
