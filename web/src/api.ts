import type {
  ApiToken,
  Approval,
  Artifact,
  AuditEvent,
  Dashboard,
  Deployment,
  Environment,
  Host,
  HostCheck,
  HostGroup,
  HostInput,
  Job,
  Me,
  Member,
  Pipeline,
  PipelineMember,
  PipelineRevision,
  PipelineRole,
  Perm,
  PipelineStats,
  PipelineSummary,
  Role,
  Run,
  RunDetail,
  NodeInput,
  RunnerInfo,
  Secret,
  Session,
  ServiceConnection,
  VarGroup,
  StepCatalog,
  GroupVar,
  ConnType,
  Template,
  TemplateKind,
  TemplateRevision,
  TestReport,
  Trigger,
  TriggerConfig,
  TriggerKind,
} from './types'

/** 会话失效（401）时触发，App 据此回到登录页。 */
export const UNAUTHORIZED_EVENT = 'flow:unauthorized'

/** 接口错误；status 为 HTTP 状态码（如 409 表示并发修改冲突）。 */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message)
  }
}

// 会话保存在 HttpOnly Cookie 中；写请求须带此头（Server 的 CSRF 防护）。
const baseHeaders = { 'X-Flow-Request': '1' }

async function request<T>(method: string, path: string, body?: BodyInit, contentType?: string): Promise<T> {
  const res = await fetch(path, {
    method,
    body,
    headers: contentType ? { ...baseHeaders, 'Content-Type': contentType } : baseHeaders,
  })
  if (res.status === 401 && path !== '/api/auth/login') {
    window.dispatchEvent(new Event(UNAUTHORIZED_EVENT))
  }
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`
    try {
      msg = (await res.json()).error ?? msg
    } catch {
      // 非 JSON 错误体，保留状态行
    }
    throw new ApiError(msg, res.status)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

const enc = encodeURIComponent
const json = (v: unknown) => JSON.stringify(v)
const J = 'application/json'

export interface ConnInput {
  type: string
  name: string
  description?: string
  values: Record<string, string>
  visibility: string
  allowed_users?: string[]
}

export const api = {
  login: (username: string, password: string) => request<Me>('POST', '/api/auth/login', json({ username, password }), J),
  logout: () => request<void>('POST', '/api/auth/logout'),
  me: () => request<Me>('GET', '/api/me'),
  switchTenant: (id: string) => request<Me>('POST', '/api/me/tenant', json({ id }), J),
  changePassword: (old_password: string, password: string) =>
    request<void>('POST', '/api/me/password', json({ old_password, password }), J),
  listTokens: () => request<ApiToken[]>('GET', '/api/me/tokens'),
  createToken: (name: string, expires_in_days = 0) => request<ApiToken & { token: string }>('POST', '/api/me/tokens', json({ name, expires_in_days }), J),
  updateProfile: (p: { display_name: string; email: string; gender: string; bio: string }) => request<Me>('PUT', '/api/me/profile', json(p), J),
  listSessions: () => request<Session[]>('GET', '/api/me/sessions'),
  revokeSession: (id: string) => request<void>('DELETE', `/api/me/sessions/${enc(id)}`),
  deleteAccount: (password: string) => request<void>('DELETE', '/api/me', json({ password }), J),
  listPipelineRoles: () => request<PipelineRole[]>('GET', '/api/pipeline-roles'),
  savePipelineRole: (r: { name: string; description?: string; permissions: Perm[] }, id?: string) =>
    request<PipelineRole>(id ? 'PUT' : 'POST', id ? `/api/pipeline-roles/${enc(id)}` : '/api/pipeline-roles', json(r), J),
  deletePipelineRole: (id: string) => request<void>('DELETE', `/api/pipeline-roles/${enc(id)}`),
  listPipelineMembers: (id: string) => request<PipelineMember[]>('GET', `/api/pipelines/${enc(id)}/members`),
  setPipelineMember: (id: string, userId: string, roleId: string) =>
    request<void>('PUT', `/api/pipelines/${enc(id)}/members/${enc(userId)}`, json({ role_id: roleId }), J),
  removePipelineMember: (id: string, userId: string) => request<void>('DELETE', `/api/pipelines/${enc(id)}/members/${enc(userId)}`),
  deleteRun: (id: string) => request<void>('DELETE', `/api/runs/${enc(id)}`),
  deleteFinishedRuns: (pipelineId: string) => request<{ deleted: number }>('DELETE', `/api/pipelines/${enc(pipelineId)}/runs`),
  copyPipeline: (id: string, name: string) => request<Pipeline>('POST', `/api/pipelines/${enc(id)}/copy`, json({ name }), J),
  listGroups: () => request<string[]>('GET', '/api/pipeline-groups'),
  createGroup: (name: string) => request<void>('POST', '/api/pipeline-groups', json({ name }), J),
  renameGroup: (from: string, to: string) => request<void>('PUT', `/api/pipeline-groups/${enc(from)}`, json({ name: to }), J),
  deleteGroup: (name: string) => request<void>('DELETE', `/api/pipeline-groups/${enc(name)}`),
  logURL: (jobId: string, download = false) => `/api/jobs/${enc(jobId)}/logs${download ? '?download=1' : ''}`,
  stepKinds: () => request<StepCatalog>('GET', '/api/step-kinds'),
  listVarGroups: () => request<VarGroup[]>('GET', '/api/var-groups'),
  saveVarGroup: (g: { name: string; description?: string; vars: GroupVar[] }, id?: string) =>
    request<VarGroup>(id ? 'PUT' : 'POST', id ? `/api/var-groups/${enc(id)}` : '/api/var-groups', json(g), J),
  deleteVarGroup: (id: string) => request<void>('DELETE', `/api/var-groups/${enc(id)}`),
  connectionTypes: () => request<ConnType[]>('GET', '/api/connection-types'),
  listConnections: () => request<ServiceConnection[]>('GET', '/api/connections'),
  saveConnection: (c: ConnInput, id?: string) =>
    request<ServiceConnection>(id ? 'PUT' : 'POST', id ? `/api/connections/${enc(id)}` : '/api/connections', json(c), J),
  deleteConnection: (id: string) => request<void>('DELETE', `/api/connections/${enc(id)}`),
  testConnection: (c: ConnInput & { id?: string }) => request<void>('POST', '/api/connections/test', json(c), J),
  setRunRemark: (id: string, remark: string) => request<RunDetail>('PUT', `/api/runs/${enc(id)}/remark`, json({ remark }), J),
  setPipelinePublic: (id: string, pub: boolean) => request<Pipeline>('PUT', `/api/pipelines/${enc(id)}/public`, json({ public: pub }), J),
  deleteToken: (id: string) => request<void>('DELETE', `/api/me/tokens/${enc(id)}`),
  createTenant: (id: string, name: string) => request<unknown>('POST', '/api/tenants', json({ id, name }), J),

  listMembers: () => request<Member[]>('GET', '/api/members'),
  addMember: (m: { username: string; display_name?: string; password?: string; role: Role }) =>
    request<Member>('POST', '/api/members', json(m), J),
  setMemberRole: (userId: string, role: Role) => request<void>('PUT', `/api/members/${enc(userId)}`, json({ role }), J),
  removeMember: (userId: string) => request<void>('DELETE', `/api/members/${enc(userId)}`),
  resetPassword: (userId: string, password: string) =>
    request<void>('POST', `/api/members/${enc(userId)}/password`, json({ password }), J),

  /** pipelineId 为空时操作租户级密钥。 */
  listSecrets: (pipelineId?: string) => request<Secret[]>('GET', secretsPath(pipelineId)),
  putSecret: (name: string, value: string, pipelineId?: string) =>
    request<Secret>('PUT', `${secretsPath(pipelineId)}/${enc(name)}`, json({ value }), J),
  deleteSecret: (name: string, pipelineId?: string) => request<void>('DELETE', `${secretsPath(pipelineId)}/${enc(name)}`),

  listAudit: (f: { category?: string; actor?: string; before?: string; limit?: number }) =>
    request<AuditEvent[]>(
      'GET',
      `/api/audit?${new URLSearchParams(Object.entries(f).filter(([, v]) => v !== undefined && v !== '').map(([k, v]) => [k, String(v)]))}`,
    ),
  listRunners: () => request<RunnerInfo[]>('GET', '/api/runners'),
  registerRunner: (n: NodeInput) => request<RunnerInfo & { token: string }>('POST', '/api/runners', json(n), J),
  updateRunner: (id: string, n: NodeInput) => request<RunnerInfo>('PUT', `/api/runners/${enc(id)}`, json(n), J),
  resetRunnerToken: (id: string) => request<RunnerInfo & { token: string }>('POST', `/api/runners/${enc(id)}/token`),
  runnerWorkflow: async (id: string) => {
    const res = await fetch(`/api/runners/${enc(id)}/workflow`, { headers: baseHeaders })
    if (!res.ok) throw new ApiError(`${res.status} ${res.statusText}`, res.status)
    return res.text()
  },
  hostAgentToken: (id: string) => request<{ token: string }>('POST', `/api/hosts/${enc(id)}/agent-token`),
  deleteRunner: (id: string) => request<void>('DELETE', `/api/runners/${enc(id)}`),

  listEnvironments: () => request<Environment[]>('GET', '/api/environments'),
  saveEnvironment: (
    e: Pick<Environment, 'name' | 'description' | 'rank' | 'require_approval' | 'approvers'> & {
      variables?: Record<string, string>
      secrets?: Record<string, string>
      remove_secrets?: string[]
    },
    id?: string,
  ) =>
    request<Environment>(id ? 'PUT' : 'POST', id ? `/api/environments/${enc(id)}` : '/api/environments', json(e), J),
  deleteEnvironment: (id: string) => request<void>('DELETE', `/api/environments/${enc(id)}`),
  listHostGroups: () => request<HostGroup[]>('GET', '/api/host-groups'),
  saveHostGroup: (g: Pick<HostGroup, 'name' | 'description' | 'environment_id'>, id?: string) =>
    request<HostGroup>(id ? 'PUT' : 'POST', id ? `/api/host-groups/${enc(id)}` : '/api/host-groups', json(g), J),
  deleteHostGroup: (id: string) => request<void>('DELETE', `/api/host-groups/${enc(id)}`),
  hostGroupUsers: (id: string) => request<{ id: string; name: string }[]>('GET', `/api/host-groups/${enc(id)}/users`),
  listHosts: () => request<Host[]>('GET', '/api/hosts'),
  saveHost: (h: HostInput, id?: string) => request<Host>(id ? 'PUT' : 'POST', id ? `/api/hosts/${enc(id)}` : '/api/hosts', json(h), J),
  deleteHost: (id: string) => request<void>('DELETE', `/api/hosts/${enc(id)}`),
  testHost: (id: string) => request<HostCheck>('POST', `/api/hosts/${enc(id)}/test`),
  resetHostKey: (id: string) => request<Host>('POST', `/api/hosts/${enc(id)}/reset-key`),
  listDeployments: (f: { environment?: string; pipeline?: string; limit?: number } = {}) =>
    request<Deployment[]>(
      'GET',
      `/api/deployments?${new URLSearchParams(Object.entries(f).filter(([, v]) => v !== undefined && v !== '').map(([k, v]) => [k, String(v)]))}`,
    ),

  listPipelines: () => request<Pipeline[]>('GET', '/api/pipelines'),
  /** 列表页：分组、收藏与每条流水线最近 runs 次运行。 */
  pipelineSummary: (runs = 10) => request<PipelineSummary[]>('GET', `/api/pipelines/summary?runs=${runs}`),
  setPipelineGroup: (id: string, group: string) => request<Pipeline>('PUT', `/api/pipelines/${enc(id)}/group`, json({ group }), J),
  setFavorite: (id: string, on: boolean) => request<void>(on ? 'PUT' : 'DELETE', `/api/pipelines/${enc(id)}/favorite`),
  getPipeline: (id: string) => request<Pipeline>('GET', `/api/pipelines/${enc(id)}`),
  createPipeline: (yaml: string) => request<Pipeline>('POST', '/api/pipelines', yaml, 'application/yaml'),
  /** baseVersion：编辑起点的版本，期间被他人保存过时返回 409。 */
  updatePipeline: (id: string, yaml: string, baseVersion?: number) =>
    request<Pipeline>('PUT', `/api/pipelines/${enc(id)}${baseVersion ? `?base_version=${baseVersion}` : ''}`, yaml, 'application/yaml'),
  pipelineStats: (id: string, days: number) => request<PipelineStats>('GET', `/api/pipelines/${enc(id)}/stats?days=${days}`),
  listRevisions: (id: string) => request<PipelineRevision[]>('GET', `/api/pipelines/${enc(id)}/revisions`),
  getRevision: (id: string, version: number) => request<PipelineRevision>('GET', `/api/pipelines/${enc(id)}/revisions/${version}`),
  /** 回退到历史版本：以其定义生成新版本并立即生效。 */
  restoreRevision: (id: string, version: number, baseVersion: number) =>
    request<Pipeline>('POST', `/api/pipelines/${enc(id)}/revisions/${version}/restore?base_version=${baseVersion}`),
  /** 按保存规则校验，不保存；不合法时抛出与保存相同的错误。 */
  validatePipeline: (yaml: string) => request<void>('POST', '/api/pipelines/validate', yaml, 'application/yaml'),
  /** 移入回收站；purge 为彻底删除（须已在回收站中） */
  deletePipeline: (id: string, purge = false) => request<void>('DELETE', `/api/pipelines/${enc(id)}${purge ? '?purge=true' : ''}`),
  recoverPipeline: (id: string) => request<Pipeline>('POST', `/api/pipelines/${enc(id)}/recover`),
  listTrash: () => request<Pipeline[]>('GET', '/api/pipelines/trash'),
  setPipelineInfo: (id: string, info: { description: string; archived: boolean }) => request<Pipeline>('PUT', `/api/pipelines/${enc(id)}/info`, json(info), J),
  dashboard: (days: number) => request<Dashboard>('GET', `/api/dashboard?days=${days}`),
  listRuns: (pipelineId: string, limit?: number) =>
    request<Run[]>('GET', `/api/pipelines/${enc(pipelineId)}/runs${limit ? `?limit=${limit}` : ''}`),
  /** 筛选与翻页：before 为上一页最后一个运行的编号。 */
  queryRuns: (pipelineId: string, f: { status?: string; trigger?: string; before?: number; limit?: number }) =>
    request<Run[]>(
      'GET',
      `/api/pipelines/${enc(pipelineId)}/runs?${new URLSearchParams(Object.entries(f).filter(([, v]) => v !== undefined && v !== '' && v !== 0).map(([k, v]) => [k, String(v)]))}`,
    ),
  listApprovals: () => request<Approval[]>('GET', '/api/approvals'),
  /** 只重新执行运行中的部署任务（沿用当时的配置与产物）。 */
  redeploy: (runId: string, job: string) => request<RunDetail>('POST', `/api/runs/${enc(runId)}/redeploy`, json({ job }), J),
  triggerRun: (pipelineId: string, env?: Record<string, string>, remark?: string) =>
    request<RunDetail>('POST', `/api/pipelines/${enc(pipelineId)}/runs`, JSON.stringify({ env, remark }), 'application/json'),
  getRun: (id: string) => request<RunDetail>('GET', `/api/runs/${enc(id)}`),
  cancelRun: (id: string) => request<RunDetail>('POST', `/api/runs/${enc(id)}/cancel`),
  retryRun: (id: string) => request<RunDetail>('POST', `/api/runs/${enc(id)}/retry`),
  /** 以该次成功运行的配置版本、参数与代码提交重新运行。 */
  rollbackRun: (id: string) => request<RunDetail>('POST', `/api/runs/${enc(id)}/rollback`),
  listTestReports: (runId: string) => request<TestReport[]>('GET', `/api/runs/${enc(runId)}/tests`),
  listArtifacts: (runId: string) => request<Artifact[]>('GET', `/api/runs/${enc(runId)}/artifacts`),
  artifactURL: (id: string) => `/api/artifacts/${enc(id)}/download`,
  approve: (jobId: string, approve: boolean, comment: string) =>
    request<Job>('POST', `/api/jobs/${enc(jobId)}/approve`, json({ approve, comment }), J),

  listTriggers: (pipelineId: string) => request<Trigger[]>('GET', `/api/pipelines/${enc(pipelineId)}/triggers`),
  createTrigger: (pipelineId: string, kind: TriggerKind, enabled: boolean, cfg: TriggerConfig) =>
    request<Trigger>('POST', `/api/pipelines/${enc(pipelineId)}/triggers`, JSON.stringify({ kind, enabled, ...cfg }), 'application/json'),
  updateTrigger: (id: string, enabled: boolean, cfg: TriggerConfig) =>
    request<Trigger>('PUT', `/api/triggers/${enc(id)}`, JSON.stringify({ enabled, ...cfg }), 'application/json'),
  deleteTrigger: (id: string) => request<void>('DELETE', `/api/triggers/${enc(id)}`),
  rotateTriggerSecret: (id: string) => request<Trigger>('POST', `/api/triggers/${enc(id)}/rotate-secret`),

  listTemplates: (kind?: TemplateKind) => request<Template[]>('GET', `/api/templates${kind ? `?kind=${kind}` : ''}`),
  getTemplate: (id: string) => request<Template>('GET', `/api/templates/${enc(id)}`),
  /** versions 为所用版本，0 表示最新版本。 */
  templateUsers: (id: string) => request<{ id: string; name: string; versions: number[] }[]>('GET', `/api/templates/${enc(id)}/users`),
  createTemplate: (kind: TemplateKind, name: string, description: string, yaml: string) =>
    request<Template>('POST', '/api/templates', JSON.stringify({ kind, name, description, yaml }), J),
  updateTemplate: (id: string, name: string, description: string, yaml: string, baseVersion?: number) =>
    request<Template>('PUT', `/api/templates/${enc(id)}${baseVersion ? `?base_version=${baseVersion}` : ''}`, JSON.stringify({ name, description, yaml }), J),
  listTemplateRevisions: (id: string) => request<TemplateRevision[]>('GET', `/api/templates/${enc(id)}/revisions`),
  getTemplateRevision: (id: string, version: number) => request<TemplateRevision>('GET', `/api/templates/${enc(id)}/revisions/${version}`),
  restoreTemplateRevision: (id: string, version: number, baseVersion: number) =>
    request<Template>('POST', `/api/templates/${enc(id)}/revisions/${version}/restore?base_version=${baseVersion}`),
  deleteTemplate: (id: string) => request<void>('DELETE', `/api/templates/${enc(id)}`),

  /** SSE：Run 详情变化推送（event: run）。 */
  runEventsURL: (runId: string) => `/api/runs/${enc(runId)}/events`,
  /** SSE：Job 日志推送（event: log 为 base64 字节，event: end 表示结束）。 */
  logStreamURL: (jobId: string) => `/api/jobs/${enc(jobId)}/logs/stream`,

  /** 读取 offset 之后的增量日志（字节），next 为下一次读取的偏移。 */
  async readLog(jobId: string, offset: number): Promise<{ data: Uint8Array; next: number }> {
    const res = await fetch(`/api/jobs/${enc(jobId)}/logs?offset=${offset}`)
    if (res.status === 401) window.dispatchEvent(new Event(UNAUTHORIZED_EVENT))
    if (!res.ok) throw new Error(`${res.status} ${res.statusText}`)
    const data = new Uint8Array(await res.arrayBuffer())
    const next = Number(res.headers.get('X-Next-Offset') ?? offset + data.length)
    return { data, next }
  },
}

function secretsPath(pipelineId?: string) {
  return pipelineId ? `/api/pipelines/${enc(pipelineId)}/secrets` : '/api/secrets'
}
