package model

import (
	"slices"
	"time"
)

// Environment 是部署环境（如开发、测试、预发、生产）：主机组归属于环境，部署记录按环境汇总。
type Environment struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Rank        int    `json:"rank"` // 展示顺序，越小越靠前
	// RequireApproval 为 true 时，部署到该环境的任务执行前需人工审批（Approvers 为空则开发者及以上均可审批）。
	RequireApproval bool     `json:"require_approval"`
	Approvers       []string `json:"approvers,omitempty"`
	// Variables 与 Secrets 注入到部署到该环境的任务：覆盖流水线中的同名变量，密钥优先级最高。
	Variables   map[string]string `json:"variables,omitempty"`
	Secrets     map[string][]byte `json:"-"`            // 名称 → 密文
	SecretNames []string          `json:"secret_names"` // 只返回名称
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// HostGroup 是一组部署目标主机，在流水线中以名称引用（deploy.hosts）。
type HostGroup struct {
	ID            string    `json:"id"`
	TenantID      string    `json:"tenant_id"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	EnvironmentID string    `json:"environment_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// 主机认证方式。
const (
	HostAuthKey      = "key"      // SSH 私钥
	HostAuthPassword = "password" // 密码
)

// 主机接入方式。
const (
	HostModeSSH   = "ssh"
	HostModeAgent = "agent"
)

// Host 是一台部署目标服务器，Runner 通过 SSH 连接。
type Host struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id"`
	Name        string `json:"name"`
	Address     string `json:"address"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	AuthType    string `json:"auth_type"`
	Credential  []byte `json:"-"` // 加密后的私钥或密码，接口不返回
	Description string `json:"description,omitempty"`
	// HostKey 是已信任的主机公钥（authorized_keys 格式）：首次连接测试成功时记录，之后部署时校验，防止中间人。
	HostKey  string   `json:"host_key,omitempty"`
	GroupIDs []string `json:"group_ids"`
	// JumpHostID 非空时经由该主机（跳板机 / 堡垒机）连接。
	JumpHostID string     `json:"jump_host_id,omitempty"`
	CheckedAt  *time.Time `json:"checked_at,omitempty"`
	// CheckError 为最近一次连接测试的错误，空表示成功。
	CheckError string `json:"check_error,omitempty"`
	// Mode：ssh（Runner 通过 SSH 连接，默认）/ agent（主机上的 Agent 主动拉取部署任务，不需要 IP 与 SSH 账号）。
	Mode string `json:"mode"`
	// AgentSeenAt / AgentInfo：Agent 主机最近一次上报的时间与环境（不存储，列表时填入）。
	AgentSeenAt *time.Time `json:"agent_seen_at,omitempty"`
	AgentInfo   *NodeInfo  `json:"agent_info,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// HostTask 是部署到 Agent 主机的单台任务。
type HostTask struct {
	ID         string            `json:"id"`
	TenantID   string            `json:"tenant_id"`
	JobID      string            `json:"job_id"`
	HostID     string            `json:"host_id"`
	Script     string            `json:"script"`
	Env        map[string]string `json:"env"`
	Target     string            `json:"target"`
	UploadKey  string            `json:"-"`
	HasUpload  bool              `json:"has_upload"`
	Status     Status            `json:"status"`
	ExitCode   int               `json:"exit_code"`
	Error      string            `json:"error,omitempty"`
	Log        []byte            `json:"-"`
	LeaseUntil *time.Time        `json:"-"`
	CreatedAt  time.Time         `json:"created_at"`
	StartedAt  *time.Time        `json:"started_at,omitempty"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`
}

func (h *Host) Clone() *Host {
	c := *h
	c.Credential = slices.Clone(h.Credential)
	c.GroupIDs = slices.Clone(h.GroupIDs)
	return &c
}

// Deployment 是一次部署任务的执行记录。
type Deployment struct {
	ID              string     `json:"id"`
	TenantID        string     `json:"tenant_id"`
	PipelineID      string     `json:"pipeline_id"`
	PipelineName    string     `json:"pipeline_name"`
	RunID           string     `json:"run_id"`
	RunNumber       int        `json:"run_number"`
	JobID           string     `json:"job_id"`
	JobName         string     `json:"job_name"`
	EnvironmentID   string     `json:"environment_id,omitempty"`
	EnvironmentName string     `json:"environment_name,omitempty"`
	GroupName       string     `json:"group_name"`
	Hosts           []string   `json:"hosts"`
	Commit          string     `json:"commit,omitempty"`
	Status          Status     `json:"status"`
	CreatedBy       string     `json:"created_by,omitempty"` // 运行的触发人
	// Artifact 是本次部署的构建物（deploy.artifact），构建物被清理后仍保留记录。
	Artifact *DeployedArtifact `json:"artifact,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

// DeployedArtifact 是部署记录中的构建物。
type DeployedArtifact struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Format  string `json:"format,omitempty"`
	Size    int64  `json:"size"`
}

// HostState 是部署任务在一台主机上的进度。
type HostState struct {
	Name       string     `json:"name"`
	Address    string     `json:"address"`
	Status     Status     `json:"status"` // pending / running / success / failed / canceled / skipped
	Error      string     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// DeployTarget 是部署任务的目标与方式（创建运行时从流水线定义快照）。
type DeployTarget struct {
	Group  string `json:"group"`            // 主机组名
	Upload string `json:"upload,omitempty"` // 上传工作目录中的路径（文件或目录）
	// Artifact 是部署的上游命名构建物，下载后解压到 Target。
	Artifact string `json:"artifact,omitempty"`
	Target string `json:"target,omitempty"` // 远程目录：上传到这里，并作为脚本的工作目录
	Batch  int    `json:"batch,omitempty"`  // 每批主机数，0 为全部同时
	// Approval 为 true 时任务就绪后先进入待审批（环境要求审批）。
	Approval bool `json:"approval,omitempty"`
}
