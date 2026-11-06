// Package protocol 定义 Server 与 Runner / 客户端之间的请求与响应体。
package protocol

import "flow/internal/model"

type TriggerRequest struct {
	Env    map[string]string `json:"env,omitempty"`
	Remark string            `json:"remark,omitempty"` // 运行备注
}

// TriggerSpec 是创建 / 更新触发器的请求体；Enabled 缺省为 true，更新时 Kind 被忽略。
type TriggerSpec struct {
	Kind    model.TriggerKind `json:"kind"`
	Enabled *bool             `json:"enabled,omitempty"`
	model.TriggerConfig
}

type RunDetail struct {
	Run  *model.Run   `json:"run"`
	Jobs []*model.Job `json:"jobs"`
}

type ClaimRequest struct {
	RunnerID string          `json:"runner_id"` // 使用独立 Token 时以注册 ID 为准，此字段被忽略
	Labels   []string        `json:"labels,omitempty"`
	Info     *model.NodeInfo `json:"info,omitempty"` // 节点环境信息（构建节点列表展示）
}

// ClaimResponse 是下发给 Runner 的任务。后续心跳 / 日志 / 完成请求使用 Job.RunnerID 作为 runner_id。
type ClaimResponse struct {
	Job       *model.Job        `json:"job"`
	Secrets   map[string]string `json:"secrets,omitempty"`   // 注入为环境变量并在日志中脱敏
	Artifacts []ArtifactRef     `json:"artifacts,omitempty"` // 上游 Job 的产物，执行前解压到工作目录
	Hosts     []DeployHost      `json:"hosts,omitempty"`     // 部署任务的目标主机
	Env       map[string]string `json:"env,omitempty"`       // 部署环境的变量，覆盖 Job 中的同名变量
	// DeployError 非空表示无法解析部署目标（如主机组不存在或为空），Runner 应直接以此失败。
	DeployError string `json:"deploy_error,omitempty"`
	// Connections 是步骤引用的服务连接（名称 → CONN_* 变量），只注入引用它的步骤。
	Connections map[string]map[string]string `json:"connections,omitempty"`
	// Masks 是需要在日志中脱敏、但不作为环境变量注入的值（如服务连接的私密字段）。
	Masks []string `json:"masks,omitempty"`
	// Named 是运行中可下载的命名构建物（「下载构建物」步骤按名称取）。
	Named []ArtifactRef `json:"named,omitempty"`
	// Outputs 是 Runner 执行后收集的任务输出（不序列化，随完成请求上报）。
	Outputs map[string]string `json:"-"`
}

// ApplyConnections 把服务连接变量并入引用它的步骤的环境变量（步骤里显式设置的同名变量优先）。
func (a *ClaimResponse) ApplyConnections() {
	for i := range a.Job.Steps {
		st := &a.Job.Steps[i]
		vars := a.Connections[st.Connection]
		if st.Connection == "" || len(vars) == 0 {
			continue
		}
		env := make(map[string]string, len(vars)+len(st.Env))
		for k, v := range vars {
			env[k] = v
		}
		for k, v := range st.Env {
			env[k] = v
		}
		st.Env = env
	}
}

// MaskValues 返回日志中要脱敏的全部值：密钥、主机密码与服务连接的私密字段。
func (a *ClaimResponse) MaskValues() []string {
	values := make([]string, 0, len(a.Secrets)+len(a.Masks))
	for _, v := range a.Secrets {
		values = append(values, v)
	}
	for _, h := range a.Hosts {
		values = append(values, h.Password)
		if h.Jump != nil {
			values = append(values, h.Jump.Password)
		}
	}
	return append(values, a.Masks...)
}

// DeployHost 是部署任务的一台目标主机（含解密后的凭据，仅随任务下发给 Runner）。
type DeployHost struct {
	Name       string      `json:"name"`
	Address    string      `json:"address"`
	Port       int         `json:"port"`
	User       string      `json:"user"`
	Password   string      `json:"password,omitempty"`
	PrivateKey string      `json:"private_key,omitempty"`
	HostKey    string      `json:"host_key,omitempty"` // 已信任的主机公钥，为空时不校验
	Jump       *DeployHost `json:"jump,omitempty"`     // 跳板机
	Agent      bool        `json:"agent,omitempty"`    // 主机 Agent 接入：经由 Server 下发任务，不使用 SSH
}

type ArtifactRef struct {
	ID      string `json:"id"`
	JobName string `json:"job_name"`
	Size    int64  `json:"size"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	Format  string `json:"format,omitempty"`
}

// ApprovalRequest 是人工卡点的审批请求。
type ApprovalRequest struct {
	Approve bool   `json:"approve"`
	Comment string `json:"comment,omitempty"`
}

type SecretRequest struct {
	Value string `json:"value"`
}

type HeartbeatRequest struct {
	RunnerID string `json:"runner_id"`
}

type HeartbeatResponse struct {
	Cancel bool `json:"cancel"`
}

// StepRequest 上报步骤进度：Status 为 running 表示开始，success / failed 表示结束。
type StepRequest struct {
	RunnerID string       `json:"runner_id"`
	Index    int          `json:"index"`
	Status   model.Status `json:"status"`
	ExitCode int          `json:"exit_code,omitempty"`
	// Outputs 是步骤成功结束时本步骤写入的任务输出。
	Outputs map[string]string `json:"outputs,omitempty"`
}

// HostsRequest 上报部署任务各主机的完整进度。
type HostsRequest struct {
	RunnerID string            `json:"runner_id"`
	Hosts    []model.HostState `json:"hosts"`
}

// TestReportRequest 上传 Job 的测试报告（Runner 已解析并汇总）。
type TestReportRequest struct {
	RunnerID string `json:"runner_id"`
	model.TestSummary
	Cases []model.TestCase `json:"cases"`
}

// CommitRequest 上报 Job 实际检出的代码提交。
type CommitRequest struct {
	RunnerID string `json:"runner_id"`
	Commit   string `json:"commit"`
}

type CompleteRequest struct {
	RunnerID string            `json:"runner_id"`
	Status   model.Status      `json:"status"`
	ExitCode int               `json:"exit_code"`
	Error    string            `json:"error,omitempty"`
	Outputs  map[string]string `json:"outputs,omitempty"` // 步骤写入 $FLOW_OUTPUT 的任务输出
}

// HostTaskRequest 是部署 Runner 为一台 Agent 主机创建的任务（Bundle 为上传内容 tar.gz）。
type HostTaskRequest struct {
	Host   string            `json:"host"`
	Script string            `json:"script"`
	Env    map[string]string `json:"env,omitempty"`
	Target string            `json:"target"`
	Bundle []byte            `json:"bundle,omitempty"`
}

// HostTaskProgress 是 Agent 主机任务的状态与 offset 之后的新日志。
type HostTaskProgress struct {
	Status   model.Status `json:"status"`
	ExitCode int          `json:"exit_code"`
	Error    string       `json:"error,omitempty"`
	Log      []byte       `json:"log"`
	Online   bool         `json:"online"`
}
