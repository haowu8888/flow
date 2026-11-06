// Package store 定义持久化接口，提供内存实现（开发/测试）与 PostgreSQL 实现（生产）。
package store

import (
	"context"
	"errors"
	"time"

	"flow/internal/model"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// MaxLogRead 是单次 ReadLog 大致返回的上限，调用方按偏移量续读。
const MaxLogRead = 1 << 20

// Store 必须并发安全；读方法返回副本，修改需显式写回。
type Store interface {
	// Atomic 在事务中执行 fn，fn 返回错误则回滚（内存实现仅串行化、不回滚）。
	// PostgreSQL 实现遇到死锁 / 串行化冲突时会整体重试 fn，因此 fn 必须可重入。
	Atomic(ctx context.Context, fn func(Tx) error) error

	// CreatePipeline 以 Version 1 写入流水线及其首个修订记录。
	CreatePipeline(ctx context.Context, p *model.Pipeline) error
	// UpdatePipeline 仅当已存储的版本等于 p.Version 时写入，并把 p.Version 加 1、记录修订；
	// 版本不一致（期间被他人保存）返回 ErrConflict。不修改分组。
	UpdatePipeline(ctx context.Context, p *model.Pipeline) error
	// SetPipelineGroup 设置列表分组（空为未分组），不产生新版本。
	SetPipelineGroup(ctx context.Context, id, group string) error
	// SetPipelineInfo 修改说明与归档标记，同样不产生版本。
	SetPipelineInfo(ctx context.Context, id, description string, archived bool) error
	// SetPipelineDeleted 移入（at 非空）或移出回收站。回收站中的流水线不出现在 ListPipelines 中、不能保存或运行。
	SetPipelineDeleted(ctx context.Context, id string, at *time.Time) error
	ListDeletedPipelines(ctx context.Context, tenantID string) ([]*model.Pipeline, error)
	SetPipelinePublic(ctx context.Context, id string, public bool) error

	// 流水线成员与自定义角色（内置角色不入库）。
	ListPipelineRoles(ctx context.Context, tenantID string) ([]*model.PipelineRole, error)
	// PutPipelineRole 新建或更新自定义角色；同租户重名返回 ErrConflict。
	PutPipelineRole(ctx context.Context, r *model.PipelineRole) error
	DeletePipelineRole(ctx context.Context, tenantID, id string) error
	ListPipelineMembers(ctx context.Context, pipelineID string) ([]*model.PipelineMember, error)
	PutPipelineMember(ctx context.Context, m *model.PipelineMember) error
	DeletePipelineMember(ctx context.Context, pipelineID, userID string) error
	// UserPipelineRoles 返回用户在各流水线中的角色 ID，键为流水线 ID。
	UserPipelineRoles(ctx context.Context, userID string) (map[string]string, error)
	// RoleUsage 返回租户内各角色被多少条流水线使用。
	RoleUsage(ctx context.Context, tenantID string) (map[string]int, error)

	// 通用变量组与服务连接（私密值为密文）；同租户重名返回 ErrConflict。
	ListVarGroups(ctx context.Context, tenantID string) ([]*model.VarGroup, error)
	GetVarGroup(ctx context.Context, id string) (*model.VarGroup, error)
	PutVarGroup(ctx context.Context, g *model.VarGroup) error
	DeleteVarGroup(ctx context.Context, id string) error
	ListConnections(ctx context.Context, tenantID string) ([]*model.ServiceConnection, error)
	GetConnection(ctx context.Context, id string) (*model.ServiceConnection, error)
	GetConnectionByName(ctx context.Context, tenantID, name string) (*model.ServiceConnection, error)
	PutConnection(ctx context.Context, c *model.ServiceConnection) error
	DeleteConnection(ctx context.Context, id string) error
	SetRunRemark(ctx context.Context, id, remark string) error
	NextArtifactVersion(ctx context.Context, pipelineID, name string) (int, error)

	// 流水线分组（分组名在租户内唯一）。
	ListGroups(ctx context.Context, tenantID string) ([]string, error)
	CreateGroup(ctx context.Context, tenantID, name string, at time.Time) error
	RenameGroup(ctx context.Context, tenantID, from, to string, at time.Time) error
	DeleteGroup(ctx context.Context, tenantID, name string) error
	ListRunnersByKind(ctx context.Context, kind string) ([]*model.Runner, error)
	QueuedJobsWithLabel(ctx context.Context, tenantID, label string, limit int) ([]*model.Job, error)
	// Agent 主机的部署任务。
	CreateHostTask(ctx context.Context, t *model.HostTask) error
	GetHostTask(ctx context.Context, id string) (*model.HostTask, error)
	ClaimHostTask(ctx context.Context, hostID string, now, lease time.Time) (*model.HostTask, error)
	TouchHostTask(ctx context.Context, id string, lease time.Time) error
	AppendHostTaskLog(ctx context.Context, id string, data []byte) error
	ReadHostTaskLog(ctx context.Context, id string, offset int) ([]byte, error)
	FinishHostTask(ctx context.Context, id string, status model.Status, exitCode int, errMsg string, at time.Time) error
	ExpireHostTasks(ctx context.Context, now time.Time) (int, error)
	// DeleteRun 删除已结束的运行（未结束返回 ErrConflict），返回要清理的产物 blob key。
	DeleteRun(ctx context.Context, id string) ([]string, error)
	// SetFavorite 收藏 / 取消收藏流水线（重复操作无副作用）。
	SetFavorite(ctx context.Context, userID, pipelineID string, on bool, at time.Time) error
	// ListFavorites 返回用户收藏的流水线 ID。
	ListFavorites(ctx context.Context, userID string) ([]string, error)
	// RecentRuns 返回租户内每条流水线最近 perPipeline 次运行（新的在前），键为流水线 ID。
	RecentRuns(ctx context.Context, tenantID string, perPipeline int) (map[string][]*model.Run, error)
	// ListRevisions 按版本倒序返回最多 limit 条修订（不含 YAML）。
	ListRevisions(ctx context.Context, pipelineID string, limit int) ([]*model.PipelineRevision, error)
	GetRevision(ctx context.Context, pipelineID string, version int) (*model.PipelineRevision, error)
	GetPipeline(ctx context.Context, id string) (*model.Pipeline, error)
	ListPipelines(ctx context.Context, tenantID string) ([]*model.Pipeline, error)
	// DeletePipeline 级联删除运行、触发器、密钥等，返回需要清理的产物 blob key。
	DeletePipeline(ctx context.Context, id string) (blobKeys []string, err error)

	GetRun(ctx context.Context, id string) (*model.Run, error)
	// ListRuns 按创建时间倒序返回最多 limit 条。
	ListRuns(ctx context.Context, pipelineID string, limit int) ([]*model.Run, error)
	GetJob(ctx context.Context, id string) (*model.Job, error)
	ListJobs(ctx context.Context, runID string) ([]*model.Job, error)
	// ExpiredJobRunIDs 返回存在租约早于 now 的运行中 Job 的 Run。
	ExpiredJobRunIDs(ctx context.Context, now time.Time) ([]string, error)

	AppendLog(ctx context.Context, jobID string, data []byte) error
	ReadLog(ctx context.Context, jobID string, offset int) ([]byte, error)

	CreateTrigger(ctx context.Context, t *model.Trigger) error
	// UpdateTrigger 只写用户可编辑字段与 NextRunAt，不覆盖 LastFiredAt / LastError。
	UpdateTrigger(ctx context.Context, t *model.Trigger) error
	DeleteTrigger(ctx context.Context, id string) error
	GetTrigger(ctx context.Context, id string) (*model.Trigger, error)
	// ListTriggers 按创建时间正序返回。
	ListTriggers(ctx context.Context, pipelineID string) ([]*model.Trigger, error)

	// PutSecret 按 (租户, 流水线, 名称) 新建或覆盖。
	PutSecret(ctx context.Context, s *model.Secret) error
	DeleteSecret(ctx context.Context, tenantID, pipelineID, name string) error
	// ListSecrets 返回某一作用域（pipelineID 为空即租户级）的密钥，按名称排序。
	ListSecrets(ctx context.Context, tenantID, pipelineID string) ([]*model.Secret, error)

	CreateRunner(ctx context.Context, r *model.Runner) error
	GetRunner(ctx context.Context, id string) (*model.Runner, error)
	GetRunnerByTokenHash(ctx context.Context, hash string) (*model.Runner, error)
	ListRunners(ctx context.Context, tenantID string) ([]*model.Runner, error)
	DeleteRunner(ctx context.Context, id string) error
	// TouchRunner 记录心跳时间与上报的标签。
	// TouchRunner 记录心跳；info 非空时同时更新节点信息。
	TouchRunner(ctx context.Context, id string, labels []string, info *model.NodeInfo, at time.Time) error
	UpdateRunner(ctx context.Context, r *model.Runner) error

	// PutArtifact 写入或覆盖 Job 的产物记录，返回被覆盖的旧 blob key。
	PutArtifact(ctx context.Context, a *model.Artifact) (oldBlobKey string, err error)
	GetArtifact(ctx context.Context, id string) (*model.Artifact, error)
	ListArtifacts(ctx context.Context, runID string) ([]*model.Artifact, error)
	// ExpiredArtifacts 返回最多 limit 个已过期的产物，不含各流水线最近一次成功运行的产物。
	ExpiredArtifacts(ctx context.Context, now time.Time, limit int) ([]*model.Artifact, error)
	DeleteArtifacts(ctx context.Context, ids []string) error
	// PruneRuns 删除流水线中最新 keep 个之外的已结束运行（含 Job、日志、产物记录），最多 limit 个，
	// 返回删除的运行数与需要清理的产物 blob key。
	PruneRuns(ctx context.Context, pipelineID string, keep, limit int) (deleted int, blobKeys []string, err error)

	CreateTenant(ctx context.Context, t *model.Tenant) error
	GetTenant(ctx context.Context, id string) (*model.Tenant, error)
	ListTenants(ctx context.Context) ([]*model.Tenant, error)

	CreateUser(ctx context.Context, u *model.User) error
	UpdateUser(ctx context.Context, u *model.User) error
	DeleteUser(ctx context.Context, id string) error
	GetUser(ctx context.Context, id string) (*model.User, error)
	GetUserByName(ctx context.Context, username string) (*model.User, error)
	ListUsers(ctx context.Context) ([]*model.User, error)
	CountUsers(ctx context.Context) (int, error)

	PutMembership(ctx context.Context, m *model.Membership) error
	DeleteMembership(ctx context.Context, tenantID, userID string) error
	ListMembers(ctx context.Context, tenantID string) ([]*model.Membership, error)
	UserMemberships(ctx context.Context, userID string) ([]*model.Membership, error)

	CreateToken(ctx context.Context, t *model.Token) error
	GetTokenByHash(ctx context.Context, hash string) (*model.Token, error)
	ListTokens(ctx context.Context, userID, kind string) ([]*model.Token, error)
	DeleteToken(ctx context.Context, id string) error
	SetTokenTenant(ctx context.Context, id, tenantID string) error
	TouchToken(ctx context.Context, id string, at time.Time) error
	DeleteExpiredTokens(ctx context.Context, now time.Time) error

	// CreateTemplate / UpdateTemplate 在同一租户内同类型重名时返回 ErrConflict，并写入修订记录。
	// UpdateTemplate 要求 t.Version 为当前版本（否则 ErrConflict），成功后 t.Version 为新版本；t.RestoredFrom 记入修订。
	CreateTemplate(ctx context.Context, t *model.Template) error
	UpdateTemplate(ctx context.Context, t *model.Template) error
	// ListTemplateRevisions 按版本倒序返回最多 limit 条修订（不含 YAML）。
	ListTemplateRevisions(ctx context.Context, templateID string, limit int) ([]*model.TemplateRevision, error)
	GetTemplateRevision(ctx context.Context, templateID string, version int) (*model.TemplateRevision, error)
	GetTemplateRevisionByName(ctx context.Context, tenantID string, kind model.TemplateKind, name string, version int) (*model.TemplateRevision, error)
	DeleteTemplate(ctx context.Context, id string) error
	GetTemplate(ctx context.Context, id string) (*model.Template, error)
	// ListTemplates 按名称排序；kind 为空时返回全部类型。
	ListTemplates(ctx context.Context, tenantID string, kind model.TemplateKind) ([]*model.Template, error)
	GetTemplateByName(ctx context.Context, tenantID string, kind model.TemplateKind, name string) (*model.Template, error)

	// 部署环境、主机组与主机：租户内名称唯一（重名返回 ErrConflict）。
	// 删除环境时其主机组变为未归属；删除主机组时从主机中移除。
	CreateEnvironment(ctx context.Context, e *model.Environment) error
	UpdateEnvironment(ctx context.Context, e *model.Environment) error
	DeleteEnvironment(ctx context.Context, id string) error
	GetEnvironment(ctx context.Context, id string) (*model.Environment, error)
	// ListEnvironments 按 Rank、名称排序。
	ListEnvironments(ctx context.Context, tenantID string) ([]*model.Environment, error)
	CreateHostGroup(ctx context.Context, g *model.HostGroup) error
	UpdateHostGroup(ctx context.Context, g *model.HostGroup) error
	DeleteHostGroup(ctx context.Context, id string) error
	GetHostGroup(ctx context.Context, id string) (*model.HostGroup, error)
	GetHostGroupByName(ctx context.Context, tenantID, name string) (*model.HostGroup, error)
	ListHostGroups(ctx context.Context, tenantID string) ([]*model.HostGroup, error)
	CreateHost(ctx context.Context, h *model.Host) error
	// UpdateHost 写入全部可变字段（含凭据、连接测试结果）。
	UpdateHost(ctx context.Context, h *model.Host) error
	DeleteHost(ctx context.Context, id string) error
	GetHost(ctx context.Context, id string) (*model.Host, error)
	ListHosts(ctx context.Context, tenantID string) ([]*model.Host, error)
	// CreateDeployment 记录一次部署开始；同一 Job 重试时覆盖原记录。
	CreateDeployment(ctx context.Context, d *model.Deployment) error
	FinishDeployment(ctx context.Context, jobID string, status model.Status, at time.Time) error
	// ListDeployments 按开始时间倒序。
	ListDeployments(ctx context.Context, tenantID string, f DeploymentFilter) ([]*model.Deployment, error)

	// PutTestReport 写入或覆盖 Job 的测试报告；ListTestReports 按任务名排序返回运行的全部报告。
	PutTestReport(ctx context.Context, r *model.TestReport) error
	ListTestReports(ctx context.Context, runID string) ([]*model.TestReport, error)

	// QueryRuns 按条件查询运行（编号倒序）；WaitingJobs 返回租户内等待审批的 Job。
	QueryRuns(ctx context.Context, pipelineID string, f RunFilter) ([]*model.Run, error)
	WaitingJobs(ctx context.Context, tenantID string) ([]*model.Job, error)

	// 统计用：流水线 since 之后的运行（新的在前，最多 limit 个）、这些运行的 Job 与测试汇总。
	RunsSince(ctx context.Context, pipelineID string, since time.Time, limit int) ([]*model.Run, error)
	JobsOfRuns(ctx context.Context, runIDs []string) ([]*model.Job, error)
	TestSummaries(ctx context.Context, runIDs []string) (map[string]model.TestSummary, error)

	// 审计日志：ListAuditEvents 按时间倒序；DeleteAuditBefore 按保留期清理。
	CreateAuditEvent(ctx context.Context, a *model.AuditEvent) error
	ListAuditEvents(ctx context.Context, tenantID string, f AuditFilter) ([]*model.AuditEvent, error)
	DeleteAuditBefore(ctx context.Context, before time.Time) error

	// DeleteDeliveriesBefore 清理早于 before 的 webhook 投递记录。
	DeleteDeliveriesBefore(ctx context.Context, before time.Time) error
}

// ClaimFilter 限定 Runner 可领取的 Job：TenantID 为空表示不限租户；Job 的 runs_on 须是 Labels 的子集。
type ClaimFilter struct {
	TenantID string
	Labels   []string
}

// Tx 是事务内可用的操作。约定：修改某个 Run 下的 Job 前先 LockRun，保证同一 Run 的状态迁移串行。
type Tx interface {
	GetPipeline(ctx context.Context, id string) (*model.Pipeline, error)
	// CreateRun 写入 Run 及其全部 Job，并分配流水线内自增的 Run.Number。
	CreateRun(ctx context.Context, r *model.Run, jobs []*model.Job) error
	LockRun(ctx context.Context, id string) (*model.Run, error)
	UpdateRun(ctx context.Context, r *model.Run) error
	GetJob(ctx context.Context, id string) (*model.Job, error)
	ListJobs(ctx context.Context, runID string) ([]*model.Job, error)
	// NextQueuedJob 锁定并返回符合条件的最早入队的 Job，没有时返回 nil, nil（PostgreSQL 用 SKIP LOCKED）。
	NextQueuedJob(ctx context.Context, f ClaimFilter) (*model.Job, error)
	UpdateJob(ctx context.Context, j *model.Job) error

	// NextDueTrigger 锁定并返回一个已到期的启用中 cron 触发器，没有时返回 nil, nil（PostgreSQL 用 SKIP LOCKED）。
	NextDueTrigger(ctx context.Context, now time.Time) (*model.Trigger, error)
	// MarkTriggerFired 记录一次触发结果并设置下次触发时间（webhook 传 nil）。
	MarkTriggerFired(ctx context.Context, id string, next *time.Time, at time.Time, errMsg string) error
	// ClaimDelivery 记录一次 webhook 投递。该触发器已记录过同一投递 ID 时返回 dup = true 及当时创建的 Run ID（可能为空）。
	// PostgreSQL 中并发到达的相同投递会等先到者的事务结束后再判定。
	ClaimDelivery(ctx context.Context, triggerID, deliveryID string, at time.Time) (runID string, dup bool, err error)
	SetDeliveryRun(ctx context.Context, triggerID, deliveryID, runID string) error
	// GetHostGroupByName / GetEnvironment 供创建运行时解析部署目标。
	GetHostGroupByName(ctx context.Context, tenantID, name string) (*model.HostGroup, error)
	GetEnvironment(ctx context.Context, id string) (*model.Environment, error)
	// GetTemplateByName / GetTemplateRevisionByName 供创建运行时展开步骤模板（后者用于锁定了版本的引用）。
	GetTemplateByName(ctx context.Context, tenantID string, kind model.TemplateKind, name string) (*model.Template, error)
	GetTemplateRevisionByName(ctx context.Context, tenantID string, kind model.TemplateKind, name string, version int) (*model.TemplateRevision, error)

	// SetRunTriggerInfo 记录运行的触发详情。
	SetRunTriggerInfo(ctx context.Context, id string, info *model.TriggerInfo) error

	// LockConcurrency 在事务内独占租户的一个并发组（PostgreSQL 用事务级 advisory lock），事务结束释放。
	LockConcurrency(ctx context.Context, tenantID, group string) error
	// ActiveRuns 返回租户内某个并发组中尚未结束的运行，按创建时间正序。
	ActiveRuns(ctx context.Context, tenantID, group string) ([]*model.Run, error)

	// NextPendingNotify 锁定并返回一个已结束但通知未发送的 Run，没有时返回 nil, nil。
	NextPendingNotify(ctx context.Context) (*model.Run, error)
	// NextJobNotify 锁定一个有待发送通知的任务（进入待审批的审批消息、结束后的任务插件）。
	NextJobNotify(ctx context.Context) (*model.Job, error)
}
