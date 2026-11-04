// Package model 定义流水线运行时的核心领域对象。
package model

import (
	"maps"
	"slices"
	"time"
)

// Status 是 Run / Job 的状态。
type Status string

const (
	StatusPending  Status = "pending" // 等待上游依赖
	StatusQueued   Status = "queued"  // Job：依赖已满足，等待 Runner 领取；Run：等待同一并发组的其他运行结束
	StatusRunning  Status = "running" // 已被 Runner 领取执行
	StatusWaiting  Status = "waiting" // 人工卡点：等待审批
	StatusSuccess  Status = "success"
	StatusFailed   Status = "failed"
	StatusSkipped  Status = "skipped"  // 上游未成功，不再执行
	StatusCanceled Status = "canceled" // 用户取消
)

// Terminal 表示状态已结束、不会再变化。
func (s Status) Terminal() bool {
	switch s {
	case StatusSuccess, StatusFailed, StatusSkipped, StatusCanceled:
		return true
	}
	return false
}

// Pipeline 是用户保存的流水线定义（YAML 原文）。
type Pipeline struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	Name      string `json:"name"`
	YAML      string `json:"yaml"`
	Version   int    `json:"version"`              // 每次保存加 1，从 1 开始
	UpdatedBy string `json:"updated_by,omitempty"` // 最后保存者的用户名
	Group     string `json:"group,omitempty"`      // 列表中的分组，不属于配置、不产生版本
	// 以下同为列表元信息：创建人、说明、归档标记（只是标记，照常运行和编辑）、移入回收站的时间。
	CreatedBy   string     `json:"created_by,omitempty"`
	Description string     `json:"description,omitempty"`
	Archived    bool       `json:"archived,omitempty"`
	IsPublic    bool       `json:"is_public"` // 公开：租户内所有人可查看（只读）
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// RestoredFrom 仅在保存时使用：本次保存回退自哪个版本（0 为普通保存），写入修订记录。
	RestoredFrom int `json:"-"`
}

// PipelineRevision 是流水线某次保存时的完整定义。
type PipelineRevision struct {
	PipelineID string `json:"pipeline_id"`
	Version    int    `json:"version"`
	Name       string `json:"name"`
	YAML       string `json:"yaml,omitempty"` // 列表接口不返回
	CreatedBy  string `json:"created_by,omitempty"`
	// RestoredFrom 非 0 表示此版本是回退到该版本时生成的。
	RestoredFrom int       `json:"restored_from,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// Run 是流水线的一次运行实例。
type Run struct {
	ID              string `json:"id"`
	TenantID        string `json:"tenant_id"`
	PipelineID      string `json:"pipeline_id"`
	Number          int    `json:"number"`
	Status          Status `json:"status"`
	Trigger         string `json:"trigger"`                    // manual / webhook / cron / api / rollback
	CreatedBy       string `json:"created_by,omitempty"`       // 手动触发的用户名
	PipelineVersion int    `json:"pipeline_version,omitempty"` // 运行所用的流水线配置版本
	// Commit 是本次运行检出的代码提交：Webhook 触发时即确定，否则取首个拉取代码的 Job 上报的提交，
	// 之后领取的 Job 都检出同一提交。
	Commit     string `json:"commit,omitempty"`
	RollbackOf string `json:"rollback_of,omitempty"` // 回滚运行：重放的原运行 ID
	// ArtifactsFrom 非空时，任务的上游产物取自该运行（按环境重新部署：只执行部署任务，沿用原构建产物）。
	ArtifactsFrom string `json:"artifacts_from,omitempty"`
	// ConcurrencyGroup 非空时，租户内同组的运行一次只执行一个，其余以 queued 状态排队。
	ConcurrencyGroup string            `json:"concurrency_group,omitempty"`
	ConcurrencyMax   int               `json:"concurrency_max,omitempty"` // 并发组同时执行的上限（0 视为 1）
	Remark           string            `json:"remark,omitempty"`          // 运行备注
	Env              map[string]string `json:"env,omitempty"`
	Notify           []NotifyTarget    `json:"-"` // 创建时从流水线定义快照，结束时发送
	NotifyDone       bool              `json:"-"`
	CreatedAt        time.Time         `json:"created_at"`
	StartedAt        *time.Time        `json:"started_at,omitempty"`
	FinishedAt       *time.Time        `json:"finished_at,omitempty"`
}

func (r *Run) Clone() *Run {
	c := *r
	c.Env = maps.Clone(r.Env)
	c.Notify = slices.Clone(r.Notify)
	return &c
}

// NotifyTarget 是运行结束时的通知渠道（url / secret 可含 ${密钥名}，发送时替换）。
type NotifyTarget struct {
	Type   string   `json:"type"`
	URL    string   `json:"url"`
	Secret string   `json:"secret,omitempty"`
	On     []Status `json:"on"`
}

// Job 是 Run 中可被单个 Runner 领取执行的最小调度单元。
type Job struct {
	ID         string            `json:"id"`
	TenantID   string            `json:"tenant_id"`
	RunID      string            `json:"run_id"`
	Seq        int               `json:"seq"` // 在流水线中的定义顺序
	Name       string            `json:"name"`
	Stage      string            `json:"stage"`
	StageIndex int               `json:"stage_index"`
	Needs      []string          `json:"needs"` // 已解析的上游 Job 名
	Image      string            `json:"image,omitempty"`
	RunsOn     []string          `json:"runs_on,omitempty"` // Runner 须具备全部标签
	Env        map[string]string `json:"env,omitempty"`
	Steps      []Step            `json:"steps"`
	StepStates []StepState       `json:"step_states,omitempty"` // 与 Steps 一一对应，由 Runner 上报
	HostStates []HostState       `json:"host_states,omitempty"` // 部署任务各主机的进度，由 Runner 上报
	JobOptions
	Status    Status `json:"status"`
	Attempt   int    `json:"attempt"`              // 第几次执行，从 1 开始
	DecidedBy string `json:"decided_by,omitempty"` // 人工卡点的审批人
	// Outputs 是步骤写入 $FLOW_OUTPUT 的输出，下游任务作为环境变量读取。
	Outputs map[string]string `json:"outputs,omitempty"`
	// DispatchedAt 是为该任务触发 GitHub 工作流的时间（GitHub 节点）。
	DispatchedAt *time.Time `json:"-"`
	// NotifyWaiting / NotifyDone 表示进入待审批 / 结束时还有通知要发（审批消息 / 任务插件），发送后清除。
	NotifyWaiting   bool       `json:"-"`
	NotifyDone      bool       `json:"-"`
	RunnerID        string     `json:"runner_id,omitempty"`
	LeaseUntil      time.Time  `json:"-"`
	CancelRequested bool       `json:"cancel_requested,omitempty"`
	ExitCode        int        `json:"exit_code"`
	Error           string     `json:"error,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

func (j *Job) Clone() *Job {
	c := *j
	c.Needs = slices.Clone(j.Needs)
	c.RunsOn = slices.Clone(j.RunsOn)
	c.Env = maps.Clone(j.Env)
	c.Steps = slices.Clone(j.Steps)
	c.StepStates = slices.Clone(j.StepStates)
	c.HostStates = slices.Clone(j.HostStates)
	c.Outputs = maps.Clone(j.Outputs)
	c.JobOptions = j.JobOptions.Clone()
	return &c
}

// JobOptions 是 Job 创建后不再变化的执行选项（PostgreSQL 中存于 options 列）。
type JobOptions struct {
	Kind         string    `json:"kind,omitempty"` // 空为普通 Job；approval 为人工卡点
	When         string    `json:"when,omitempty"` // on_success / on_failure / always
	TimeoutSec   int       `json:"timeout_sec,omitempty"`
	Retries      int       `json:"retries,omitempty"`
	Artifacts    []string  `json:"artifacts,omitempty"`
	Cache        *Cache    `json:"cache,omitempty"`
	Checkout     *Checkout `json:"checkout,omitempty"`
	Approvers    []string  `json:"approvers,omitempty"`
	MatrixValues []string  `json:"matrix,omitempty"`
	// ArtifactTTLSec 是产物保留秒数（创建运行时从流水线 retention 快照）：0 用 Server 默认值，-1 永久保留。
	ArtifactTTLSec int `json:"artifact_ttl_sec,omitempty"`
	// Deploy 非空表示主机部署任务：步骤在主机组的每台主机上通过 SSH 执行。
	Deploy *DeployTarget `json:"deploy,omitempty"`
	// Reports 是 JUnit 测试报告路径（相对工作目录，支持通配符），步骤结束后解析上传。
	Reports []string `json:"reports,omitempty"`
	// VarGroups 是要注入私密变量的通用变量组 ID（非私密变量已在创建运行时并入 Env）。
	VarGroups []string `json:"var_groups,omitempty"`
	// If 是运行条件表达式（见 pipeline.ParseCond），上游结束后求值，为假时跳过。
	If string `json:"if,omitempty"`
	// Plugins 是任务插件：任务结束时按状态发送通知。
	Plugins []Plugin `json:"plugins,omitempty"`
	// ApprovalNotify 是人工卡点进入待审批时发送审批消息（带通过 / 驳回链接）的服务连接。
	ApprovalNotify []ApprovalNotify `json:"approval_notify,omitempty"`
	// CacheDirs 是流水线级缓存目录：容器中的绝对路径，映射到 Runner 上按流水线隔离的持久目录。
	CacheDirs []string `json:"cache_dirs,omitempty"`
	// PipelineID 便于 Runner 定位按流水线隔离的缓存目录。
	PipelineID string `json:"pipeline_id,omitempty"`
}

// Plugin 是任务插件：任务结束且状态命中 On 时发送通知。
type Plugin struct {
	Name       string   `json:"name,omitempty"`
	Type       string   `json:"type"`                 // webhook / dingtalk / feishu / wecom / bark / telegram / email
	Connection string   `json:"connection,omitempty"` // bark / wecom / telegram / email 使用的服务连接
	URL        string   `json:"url,omitempty"`        // webhook / dingtalk / feishu 的地址，可含 ${SECRET}
	Secret     string   `json:"secret,omitempty"`     // 钉钉 / 飞书加签密钥，可含 ${SECRET}
	On         []string `json:"on"`                   // success / failure / cancel / always
	To         []string `json:"to,omitempty"`         // 邮件收件人
	Title      string   `json:"title,omitempty"`      // 自定义标题，可含 ${变量}
	Content    string   `json:"content,omitempty"`    // 自定义正文，可含 ${变量}
}

// ApprovalNotify 是审批消息的发送目标。
type ApprovalNotify struct {
	Connection string   `json:"connection"`
	To         []string `json:"to,omitempty"` // 邮件收件人
}

func (o JobOptions) Clone() JobOptions {
	o.Artifacts = slices.Clone(o.Artifacts)
	o.Approvers = slices.Clone(o.Approvers)
	o.MatrixValues = slices.Clone(o.MatrixValues)
	o.Reports = slices.Clone(o.Reports)
	o.VarGroups = slices.Clone(o.VarGroups)
	o.Plugins = slices.Clone(o.Plugins)
	o.ApprovalNotify = slices.Clone(o.ApprovalNotify)
	o.CacheDirs = slices.Clone(o.CacheDirs)
	if o.Cache != nil {
		c := *o.Cache
		c.Paths = slices.Clone(c.Paths)
		o.Cache = &c
	}
	if o.Deploy != nil {
		d := *o.Deploy
		o.Deploy = &d
	}
	if o.Checkout != nil {
		c := *o.Checkout
		o.Checkout = &c
	}
	return o
}

const JobKindApproval = "approval"

type Cache struct {
	Key   string   `json:"key"`
	Paths []string `json:"paths"`
}

// Checkout 告诉 Runner 在执行 Step 前拉取哪份代码。
type Checkout struct {
	URL         string `json:"url"`
	Ref         string `json:"ref,omitempty"`    // 分支名或 refs/tags/xxx，空为远端默认分支
	Commit      string `json:"commit,omitempty"` // 指定提交（Webhook 触发时）
	Username    string `json:"username,omitempty"`
	TokenSecret string `json:"token_secret,omitempty"` // 访问令牌所在的密钥名
	Depth       int    `json:"depth,omitempty"`
}

// Step 是 Job 内顺序执行的一段 shell 脚本。
type Step struct {
	Name string            `json:"name"`
	Run  string            `json:"run"`
	Env  map[string]string `json:"env,omitempty"` // 仅本步骤可见；值中的 ${NAME} 由 Runner 按 Job 变量与密钥展开
	// Connection 是服务连接名称：领取任务时解析为 ClaimResponse.Connections 中的变量，仅注入本步骤。
	Connection string `json:"connection,omitempty"`
	// Kind 是内置步骤类型（展示用；artifact-upload / artifact-download 由 Runner 直接执行），With 为其输入。
	Kind string            `json:"kind,omitempty"`
	With map[string]string `json:"with,omitempty"`
}

// StepState 是步骤的执行进度。
type StepState struct {
	Status     Status     `json:"status"` // pending / running / success / failed / canceled
	ExitCode   int        `json:"exit_code,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// ResetSteps 把步骤进度恢复为未开始（Job 重新排队时）。
func (j *Job) ResetSteps() {
	j.HostStates = nil
	j.StepStates = make([]StepState, len(j.Steps))
	for i := range j.StepStates {
		j.StepStates[i].Status = StatusPending
	}
}

// TemplateKind 是模板类型。
type TemplateKind string

const (
	TemplateSteps    TemplateKind = "steps"    // 步骤模板：流水线中以 uses 引用，创建运行时展开
	TemplatePipeline TemplateKind = "pipeline" // 流水线模板：新建流水线时的起点
)

// Template 是租户级模板。内置模板 Builtin 为 true、ID 以 builtin: 开头，只读。
type Template struct {
	ID          string       `json:"id"`
	TenantID    string       `json:"tenant_id,omitempty"`
	Kind        TemplateKind `json:"kind"`
	Name        string       `json:"name"`
	Category    string       `json:"category,omitempty"` // 内置流水线模板的分类（语言 / 技术栈）
	Description string       `json:"description"`
	YAML        string       `json:"yaml"`
	Builtin     bool         `json:"builtin,omitempty"`
	// Version 每次保存加 1（内置模板为 0，无历史）。
	Version   int       `json:"version"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// RestoredFrom 仅在保存时使用：非 0 表示此次保存是回退到该版本。
	RestoredFrom int `json:"-"`
}

// TemplateRevision 是模板某次保存时的内容。
type TemplateRevision struct {
	TemplateID   string    `json:"template_id"`
	Version      int       `json:"version"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	YAML         string    `json:"yaml,omitempty"` // 列表接口不返回
	CreatedBy    string    `json:"created_by,omitempty"`
	RestoredFrom int       `json:"restored_from,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// TriggerKind 是触发器类型。
type TriggerKind string

const (
	TriggerWebhook TriggerKind = "webhook" // Git 平台推送事件
	TriggerCron    TriggerKind = "cron"    // 定时
	// TriggerGeneric 是流水线 Webhook：外部系统带令牌 POST 即可触发，请求体传入参数 / 运行时变量。
	TriggerGeneric TriggerKind = "generic"
)

// Trigger 是流水线的自动触发规则。
type Trigger struct {
	ID         string      `json:"id"`
	TenantID   string      `json:"tenant_id"`
	PipelineID string      `json:"pipeline_id"`
	Kind       TriggerKind `json:"kind"`
	Enabled    bool        `json:"enabled"`
	TriggerConfig
	Secret      string     `json:"secret,omitempty"`      // webhook 签名密钥，由 Server 生成
	NextRunAt   *time.Time `json:"next_run_at,omitempty"` // cron 下次触发时间；停用时为空
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"` // 最近一次触发失败的原因，成功后清空
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// TriggerConfig 是用户可编辑的触发器配置（PostgreSQL 中存于 config 列）。
type TriggerConfig struct {
	// Events 是 webhook 监听的事件：push / pull_request（含 GitLab Merge Request），空为仅 push。
	Events []string `json:"events,omitempty"`
	// AllowForks 允许运行来自 fork 仓库的 PR。fork 的代码会拿到流水线密钥，只应在信任所有贡献者时开启。
	AllowForks bool     `json:"allow_forks,omitempty"`
	Branches   []string `json:"branches,omitempty"` // webhook 分支过滤（通配符）；PR 事件按目标分支过滤
	Tags       []string `json:"tags,omitempty"`     // webhook 标签过滤（通配符）
	// Paths / PathsIgnore 按推送改动的文件过滤（通配符，以 / 结尾表示目录）；合并请求事件不按路径过滤。
	Paths       []string          `json:"paths,omitempty"`
	PathsIgnore []string          `json:"paths_ignore,omitempty"`
	Schedule    string            `json:"schedule,omitempty"` // cron 表达式
	Timezone    string            `json:"timezone,omitempty"` // cron 时区，空为 Server 本地时区
	Env         map[string]string `json:"env,omitempty"`      // 触发时注入的变量
}

func (t *Trigger) Clone() *Trigger {
	c := *t
	c.Events = slices.Clone(t.Events)
	c.Branches = slices.Clone(t.Branches)
	c.Tags = slices.Clone(t.Tags)
	c.Paths = slices.Clone(t.Paths)
	c.PathsIgnore = slices.Clone(t.PathsIgnore)
	c.Env = maps.Clone(t.Env)
	return &c
}

// Secret 是加密存储的变量；PipelineID 为空表示租户级共享，同名时流水线级优先。
type Secret struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	PipelineID string    `json:"pipeline_id,omitempty"`
	Name       string    `json:"name"`
	Value      []byte    `json:"-"` // 密文
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Runner 是注册的执行节点，持独立 Token，只领取所属租户的 Job。
type Runner struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenant_id"`
	Name       string     `json:"name"`
	Labels     []string   `json:"labels"` // 最近一次上报的标签
	TokenHash  string     `json:"-"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	// Kind：self（自建节点）/ github（GitHub Actions 机器，任务排队时触发工作流）/ agent（主机 Agent）。
	Kind   string            `json:"kind"`
	Config map[string]string `json:"config,omitempty"` // github：repo / workflow / ref
	Secret string            `json:"-"`                // github：访问令牌密文
	Info   NodeInfo          `json:"info"`
	HostID string            `json:"host_id,omitempty"` // agent：绑定的主机
}

// NodeInfo 是构建节点上报的环境信息。
type NodeInfo struct {
	OS          string `json:"os,omitempty"`
	Arch        string `json:"arch,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	Version     string `json:"version,omitempty"`
	Concurrency int    `json:"concurrency,omitempty"`
}

// Artifact 是 Job 上传的构建产物（tar.gz），内容存于 blob 存储。
type Artifact struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	RunID    string `json:"run_id"`
	JobID    string `json:"job_id"`
	JobName  string `json:"job_name"`
	Size     int64  `json:"size"`
	BlobKey  string `json:"-"`
	// Name 非空为「构建物上传」步骤上传的命名构建物（Version 为版本，Format 为 zip / tar.gz）；空为任务的默认归档。
	Name      string    `json:"name,omitempty"`
	Version   string    `json:"version,omitempty"`
	Format    string    `json:"format,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt 之后由保留策略清理；为空表示永久保留。流水线最近一次成功运行的产物不会被清理。
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Role 是用户在租户内的角色，权限依次递增。
type Role string

const (
	RoleViewer    Role = "viewer"    // 只读
	RoleDeveloper Role = "developer" // 编辑流水线、触发、审批、管理流水线级密钥
	RoleAdmin     Role = "admin"     // 另可管理成员、Runner、租户级密钥
)

func (r Role) Rank() int {
	switch r {
	case RoleViewer:
		return 1
	case RoleDeveloper:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	DisplayName  string    `json:"display_name,omitempty"`
	PasswordHash string    `json:"-"`
	IsAdmin      bool      `json:"is_admin"` // 系统管理员：可管理所有租户与用户
	Email        string    `json:"email,omitempty"`
	Gender       string    `json:"gender,omitempty"` // male / female / 空为保密
	Bio          string    `json:"bio,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type Membership struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	Role     Role   `json:"role"`
}

// Token 是登录会话或个人访问令牌；只保存明文的 SHA-256。
type Token struct {
	ID         string     `json:"id"`
	Hash       string     `json:"-"`
	UserID     string     `json:"user_id"`
	TenantID   string     `json:"tenant_id"` // 当前选中的租户
	Kind       string     `json:"kind"`      // session / api
	Name       string     `json:"name,omitempty"`
	Prefix     string     `json:"prefix"` // 明文前几位，便于用户辨认
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	UserAgent  string     `json:"user_agent,omitempty"` // 登录会话的浏览器
	IP         string     `json:"ip,omitempty"`         // 登录时的来源地址
}

// 流水线权限（成员按角色授权）。
const (
	PermView          = "view"
	PermRun           = "run"
	PermEdit          = "edit"
	PermDelete        = "delete"
	PermDeleteRun     = "deleteRun"
	PermManageMembers = "manageMembers"
)

// AllPermissions 按展示顺序列出全部流水线权限。
var AllPermissions = []string{PermView, PermRun, PermEdit, PermDelete, PermDeleteRun, PermManageMembers}

// 内置角色：拥有者（创建人）、管理者、成员。
const (
	RoleOwnerID  = "owner"
	RoleAdminID  = "admin"
	RoleMemberID = "member"
)

// PipelineRole 是流水线成员的角色；Builtin 为内置角色（不入库、不可修改）。
type PipelineRole struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id,omitempty"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Permissions []string  `json:"permissions"`
	Builtin     bool      `json:"builtin,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// BuiltinRoles 返回内置角色。
func BuiltinRoles() []*PipelineRole {
	return []*PipelineRole{
		{ID: RoleOwnerID, Name: "拥有者", Description: "流水线创建人，拥有全部权限，不可修改", Permissions: AllPermissions, Builtin: true},
		{ID: RoleAdminID, Name: "管理者", Description: "除删除流水线外的全部权限", Builtin: true,
			Permissions: []string{PermView, PermRun, PermEdit, PermDeleteRun, PermManageMembers}},
		{ID: RoleMemberID, Name: "成员", Description: "查看和运行流水线", Permissions: []string{PermView, PermRun}, Builtin: true},
	}
}

// PipelineMember 是流水线的成员。
type PipelineMember struct {
	PipelineID string    `json:"pipeline_id"`
	UserID     string    `json:"user_id"`
	RoleID     string    `json:"role_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// VarGroup 是通用变量组：运行时注入为环境变量（流水线自己的变量优先）。
type VarGroup struct {
	ID          string     `json:"id"`
	TenantID    string     `json:"tenant_id"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Vars        []GroupVar `json:"vars"`
	CreatedBy   string     `json:"created_by,omitempty"`
	UpdatedBy   string     `json:"updated_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// GroupVar 是变量组中的变量；私密变量的 Value 在存储中为密文，接口返回时清空。
type GroupVar struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Secret bool   `json:"secret,omitempty"`
}

// ServiceConnection 是第三方服务的凭据（服务连接），流水线步骤按名称引用。
type ServiceConnection struct {
	ID          string            `json:"id"`
	TenantID    string            `json:"tenant_id"`
	Type        string            `json:"type"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Config      map[string]string `json:"config"`  // 非私密字段
	Secrets     map[string]string `json:"-"`       // 私密字段的密文
	SecretKeys  []string          `json:"secrets"` // 已设置的私密字段（值不返回）
	// Visibility：private 仅创建人可引用；public 所有成员可引用；custom 仅 AllowedUsers 可引用。修改与删除限创建人和管理员。
	Visibility   string    `json:"visibility"`
	AllowedUsers []string  `json:"allowed_users,omitempty"`
	CreatedBy    string    `json:"created_by,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}
