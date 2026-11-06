// 与 internal/model 的 JSON 结构保持一致。

export type Status = 'pending' | 'queued' | 'running' | 'waiting' | 'success' | 'failed' | 'skipped' | 'canceled'

export const isTerminal = (s: Status) =>
  s === 'success' || s === 'failed' || s === 'skipped' || s === 'canceled'

export interface Pipeline {
  id: string
  tenant_id: string
  name: string
  yaml: string
  version: number
  updated_by?: string
  /** 列表分组，不属于配置 */
  group?: string
  /** 以下同为列表元信息 */
  created_by?: string
  description?: string
  /** 归档：只是标记，照常运行和编辑 */
  archived?: boolean
  /** 移入回收站的时间 */
  deleted_at?: string
  /** 公开：租户内所有人可查看 */
  is_public?: boolean
  /** 当前用户在这条流水线上的权限（列表与详情接口返回） */
  permissions?: Perm[]
  created_at: string
  updated_at: string
}

export interface PeriodStat {
  runs: number
  success: number
  failed: number
  canceled: number
  running: number
  queued: number
  /** 成功 / (成功 + 失败)，无数据为 -1 */
  success_rate: number
  /** 秒，成功的运行 */
  avg_duration: number
  success_runs: number
}

export interface DashRun extends Run {
  pipeline_name: string
}

export interface Dashboard {
  days: number
  pipelines: number
  mine: number
  current: PeriodStat
  previous: PeriodStat
  runners_online: number
  runners_total: number
  daily: { date: string; success: number; failed: number; canceled: number }[]
  active: DashRun[]
  recent: DashRun[]
  ranking: { id: string; name: string; runs: number; success_rate: number; avg_duration: number; last_at: string; last_status: Status }[]
}

/** 列表页的一行。 */
export interface PipelineSummary extends Pipeline {
  favorite: boolean
  /** 最近的运行，新的在前 */
  recent_runs: Run[]
}

/** 模板某次保存的内容；列表接口不含 yaml。 */
export interface TemplateRevision {
  template_id: string
  version: number
  name: string
  description: string
  yaml?: string
  created_by?: string
  restored_from?: number
  created_at: string
}

/** 流水线某次保存的定义；列表接口不含 yaml。 */
export interface PipelineRevision {
  pipeline_id: string
  version: number
  name: string
  yaml?: string
  created_by?: string
  /** 回退到该版本时生成 */
  restored_from?: number
  created_at: string
}

/** Webhook 触发的详情。 */
export interface TriggerInfo {
  provider?: string
  event?: string
  pusher?: string
  repo?: string
  repo_url?: string
  ref?: string
  branch?: string
  before?: string
  after?: string
  compare_url?: string
  commits?: { id: string; message?: string; author?: string; url?: string }[]
  total_commits?: number
  /** 流水线 Webhook 覆盖的变量名 */
  vars?: string[]
}

/** 运行的排队原因。 */
export interface QueueWait {
  group?: string
  running?: number
  limit?: number
  ahead?: number
  jobs?: { job: string; labels?: string[]; online: number; reason: string }[]
}

export interface Run {
  trigger_info?: TriggerInfo
  id: string
  pipeline_id: string
  number: number
  status: Status
  trigger: string
  created_by?: string
  pipeline_version?: number
  /** 检出的代码提交 */
  commit?: string
  /** 回滚运行：重放的原运行 ID */
  rollback_of?: string
  /** 按环境重新部署：产物取自该运行 */
  artifacts_from?: string
  /** 并发组；状态为 queued 时表示在等同组的其他运行结束 */
  concurrency_group?: string
  /** 运行备注 */
  remark?: string
  env?: Record<string, string>
  created_at: string
  started_at?: string
  finished_at?: string
}

export interface Step {
  name: string
  run: string
}

export interface Job {
  id: string
  run_id: string
  seq: number
  name: string
  stage: string
  stage_index: number
  needs: string[] | null
  image?: string
  runs_on?: string[]
  env?: Record<string, string>
  steps: Step[]
  /** 与 steps 一一对应的执行进度 */
  step_states?: StepState[]
  /** 主机部署任务 */
  deploy?: { group: string; upload?: string; target?: string; batch?: number; approval?: boolean }
  host_states?: HostState[]
  kind?: 'approval'
  when?: string
  timeout_sec?: number
  retries?: number
  artifacts?: string[]
  approvers?: string[]
  matrix?: string[]
  status: Status
  attempt: number
  decided_by?: string
  runner_id?: string
  cancel_requested?: boolean
  exit_code: number
  error?: string
  /** 任务输出（$FLOW_OUTPUT） */
  outputs?: Record<string, string>
  created_at: string
  started_at?: string
  finished_at?: string
}

export interface StepState {
  status: Status
  exit_code?: number
  started_at?: string
  finished_at?: string
  /** 本步骤写入的任务输出 */
  outputs?: Record<string, string>
}

/** 部署环境。 */
export interface Environment {
  id: string
  name: string
  description?: string
  rank: number
  require_approval: boolean
  approvers?: string[]
  /** 注入部署任务的变量 */
  variables?: Record<string, string>
  /** 密钥只返回名称 */
  secret_names: string[]
  created_at: string
  updated_at: string
}

export interface HostGroup {
  id: string
  name: string
  description?: string
  environment_id?: string
  created_at: string
  updated_at: string
}

export type HostAuthType = 'key' | 'password'

export interface Host {
  id: string
  name: string
  address: string
  port: number
  username: string
  auth_type: HostAuthType
  description?: string
  /** 已信任的主机公钥 */
  host_key?: string
  group_ids: string[]
  /** 跳板机 */
  jump_host_id?: string
  checked_at?: string
  check_error?: string
  /** ssh：Runner 通过 SSH 连接；agent：主机上的 Agent 主动拉取部署任务 */
  mode: HostMode
  agent_seen_at?: string
  agent_info?: NodeEnv
  created_at: string
  updated_at: string
}

export type HostMode = 'ssh' | 'agent'

export interface HostInput {
  mode?: HostMode
  name: string
  address: string
  port: number
  username: string
  auth_type: HostAuthType
  /** 私钥或密码；编辑时留空保持不变 */
  credential?: string
  description?: string
  group_ids: string[]
  jump_host_id?: string
}

export interface HostCheck {
  host: Host
  ok: boolean
  error?: string
  output?: string
  fingerprint?: string
  new_host_key?: boolean
}

export interface HostState {
  name: string
  address: string
  status: Status
  error?: string
  started_at?: string
  finished_at?: string
}

export interface Deployment {
  id: string
  pipeline_id: string
  pipeline_name: string
  run_id: string
  run_number: number
  job_id: string
  job_name: string
  environment_id?: string
  environment_name?: string
  group_name: string
  hosts: string[]
  commit?: string
  status: Status
  created_by?: string
  started_at: string
  finished_at?: string
}

export interface TestCase {
  suite?: string
  class?: string
  name: string
  status: 'passed' | 'failed' | 'skipped'
  time?: number
  message?: string
}

/** Job 的测试报告；cases 含全部失败与跳过的用例及最慢的通过用例。 */
export interface TestReport {
  job_id: string
  run_id: string
  job_name: string
  total: number
  passed: number
  failed: number
  skipped: number
  duration: number
  cases: TestCase[]
  created_at: string
}

export interface PipelineStats {
  days: number
  runs: number
  success: number
  failed: number
  canceled: number
  success_rate: number
  avg_duration: number
  p50_duration: number
  p90_duration: number
  mean_recovery: number
  daily: { date: string; total: number; success: number; failed: number }[]
  recent: { id: string; number: number; status: Status; duration: number; at: string; tests?: { total: number; passed: number; failed: number; skipped: number } }[]
  jobs: { name: string; runs: number; failed: number; avg_duration: number; p90_duration: number }[]
  deploys: { environment: string; count: number; success: number; last_at: string }[]
  tests?: { runs: number; avg_total: number; failed_runs: number; latest_total: number }
  truncated?: boolean
  by_trigger: CountStat[]
  by_user: CountStat[]
  /** [星期一..星期日][0-3 点, 3-6 点 …] 的运行次数 */
  heatmap: number[][]
  p95_duration: number
}

export interface CountStat {
  key: string
  runs: number
  success: number
  failed: number
  canceled: number
}

export interface AuditEvent {
  id: string
  actor: string
  action: string
  target_type?: string
  target_id?: string
  target_name?: string
  detail?: string
  ip?: string
  created_at: string
}

/** 等待审批的任务。 */
export interface Approval {
  job: Job
  run: Run
  pipeline_name: string
  environment?: string
}

export interface RunDetail {
  run: Run
  jobs: Job[]
}

export type TriggerKind = 'webhook' | 'cron' | 'generic'

export type WebhookEvent = 'push' | 'pull_request'

/** 用户可编辑的触发器配置，对应 model.TriggerConfig。 */
export interface TriggerConfig {
  /** webhook 监听的事件，空为仅 push */
  events?: WebhookEvent[]
  /** 是否运行来自 fork 仓库的合并请求 */
  allow_forks?: boolean
  /** push：推送分支；pull_request：目标分支 */
  branches?: string[]
  tags?: string[]
  /** 推送改动的文件须匹配其一（通配符，以 / 结尾表示目录） */
  paths?: string[]
  /** 改动的文件全部匹配时不触发 */
  paths_ignore?: string[]
  schedule?: string
  timezone?: string
  env?: Record<string, string>
}

export interface Trigger extends TriggerConfig {
  id: string
  pipeline_id: string
  kind: TriggerKind
  enabled: boolean
  secret?: string
  next_run_at?: string
  last_fired_at?: string
  last_error?: string
  created_at: string
  updated_at: string
}

export type Role = 'viewer' | 'developer' | 'admin'

export const roleRank: Record<Role, number> = { viewer: 1, developer: 2, admin: 3 }

export const roleLabel: Record<Role, string> = { viewer: '只读', developer: '开发者', admin: '管理员' }

export interface User {
  id: string
  username: string
  display_name?: string
  is_admin: boolean
  email?: string
  /** male / female，空为保密 */
  gender?: string
  bio?: string
}

export interface TenantView {
  id: string
  name: string
  role: Role
}

/** 当前登录用户、当前租户与角色。 */
export interface Me {
  user: User
  tenant_id: string
  role: Role
  tenants: TenantView[]
}

export interface Member {
  user_id: string
  username: string
  display_name?: string
  role: Role
  is_admin: boolean
}

export interface ApiToken {
  id: string
  name?: string
  prefix: string
  created_at: string
  last_used_at?: string
  expires_at?: string
}

/** 登录会话（安全 → 活动会话） */
export interface Session extends ApiToken {
  user_agent?: string
  ip?: string
  current: boolean
}

/** 流水线权限 */
export type Perm = 'view' | 'run' | 'edit' | 'delete' | 'deleteRun' | 'manageMembers'

export const permLabels: Record<Perm, { label: string; hint: string }> = {
  view: { label: '查看流水线', hint: '流水线配置、运行记录、日志和产物' },
  run: { label: '运行 / 取消运行', hint: '手动触发运行、取消正在进行的运行' },
  edit: { label: '编辑配置', hint: '修改流程、触发、变量等配置，复制流水线，移动分组' },
  delete: { label: '删除流水线', hint: '移到回收站、恢复、彻底删除' },
  deleteRun: { label: '删除运行记录', hint: '删除单条或全部运行记录（含日志和产物）' },
  manageMembers: { label: '管理成员', hint: '添加 / 移除成员，修改成员角色（不能把人设成拥有者），设置公开' },
}

export interface PipelineRole {
  id: string
  name: string
  description?: string
  permissions: Perm[]
  builtin?: boolean
  /** 使用该角色的流水线数 */
  pipelines: number
  created_at: string
}

export interface PipelineMember {
  pipeline_id: string
  user_id: string
  role_id: string
  username: string
  display_name?: string
  email?: string
  created_at: string
}

export interface Secret {
  id: string
  pipeline_id?: string
  name: string
  created_at: string
  updated_at: string
}

export type NodeKind = 'self' | 'github' | 'agent'

/** 节点上报的环境信息 */
export interface NodeEnv {
  os?: string
  arch?: string
  hostname?: string
  version?: string
  concurrency?: number
}

export interface RunnerInfo {
  id: string
  name: string
  labels: string[] | null
  last_seen_at?: string
  created_at: string
  /** self 自建节点 / github GitHub 节点 / agent 主机 Agent */
  kind: NodeKind
  /** github：repo / workflow / ref */
  config?: Record<string, string>
  info: NodeEnv
  host_id?: string
}

export interface NodeInput {
  name: string
  kind?: NodeKind
  repo?: string
  workflow?: string
  ref?: string
  /** GitHub 访问令牌；编辑时留空保持不变 */
  token?: string
}

export interface Artifact {
  /** 命名构建物（「构建物上传」步骤）；为空是任务的默认归档 */
  name?: string
  version?: string
  format?: string
  id: string
  run_id: string
  job_id: string
  job_name: string
  size: number
  created_at: string
  /** 之后按保留策略清理；为空表示永久保留 */
  expires_at?: string
  /** 免登录下载地址（带签名） */
  url?: string
  /** 存到对象存储连接时为「连接名 / Bucket」 */
  storage?: string
}

/** 网盘上传（cloudreve-upload 步骤）的结果。 */
export interface DriveUpload {
  job_name: string
  step: string
  dir: string
  files: number
  size?: number
  url?: string
  share_url?: string
}

/** 对象存储上传（oss-upload 步骤）的结果。 */
export interface ObjectUpload {
  job_name: string
  step: string
  bucket?: string
  path: string
  url?: string
}

/** 部署历史的一行：一次运行交付的构建物、网盘 / 对象存储上传与主机部署。 */
export interface Delivery {
  seq: number
  run: Run
  started_at?: string
  artifacts: Artifact[]
  drives: DriveUpload[]
  objects: ObjectUpload[]
  deployments: Deployment[]
}

export type TemplateKind = 'steps' | 'pipeline'

/** 模板：steps 在流水线中以 uses 引用；pipeline 是新建流水线的起点。内置模板只读。 */
export interface Template {
  id: string
  kind: TemplateKind
  name: string
  /** 内置流水线模板的分类（语言 / 技术栈） */
  category?: string
  description: string
  yaml: string
  builtin?: boolean
  /** 每次保存加 1；内置模板为 0（无历史） */
  version: number
  updated_by?: string
  created_at: string
  updated_at: string
}

/** 当前用户在流水线上是否有某项权限（接口未返回权限时按无权限处理）。 */
export function hasPerm(p: { permissions?: Perm[] } | undefined, perm: Perm) {
  return !!p?.permissions?.includes(perm)
}

export interface GroupVar {
  name: string
  /** 私密变量不回显（为空） */
  value: string
  secret?: boolean
}

export interface VarGroup {
  id: string
  name: string
  description?: string
  vars: GroupVar[]
  created_by?: string
  updated_by?: string
  created_at: string
  updated_at: string
}

export interface ConnField {
  key: string
  label: string
  secret?: boolean
  required?: boolean
  placeholder?: string
  description?: string
  options?: string[]
  bool?: boolean
}

export interface ConnType {
  type: string
  label: string
  description: string
  fields: ConnField[]
  testable?: boolean
}

export interface ServiceConnection {
  id: string
  type: string
  name: string
  description?: string
  config: Record<string, string>
  /** 已设置的私密字段 */
  secrets: string[]
  visibility: 'private' | 'public' | 'custom'
  allowed_users?: string[]
  created_by?: string
  created_at: string
  updated_at: string
}

/** 私密字段未修改时提交的占位值 */
export const SECRET_MASK = '••••••'

export interface StepInput {
  key: string
  label: string
  default?: string
  placeholder?: string
  description?: string
  widget?: 'text' | 'textarea' | 'select' | 'checkbox' | 'code'
  options?: { value: string; label: string }[]
  required?: boolean
  advanced?: boolean
}

/** 内置步骤类型 */
export interface StepKind {
  kind: string
  category: string
  label: string
  description: string
  connection?: string
  /** 服务连接选填 */
  connection_optional?: boolean
  image?: string
  inputs: StepInput[]
  outputs?: string[]
  native?: boolean
}

export interface StepCatalog {
  categories: { value: string; label: string }[]
  kinds: StepKind[]
  /** 插件类型 → 需要的服务连接类型（空为直接填地址） */
  plugins: Record<string, string>
}
