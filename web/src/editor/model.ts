// 可视化编辑器的数据层：所有修改都作用在 YAML 文档树上再序列化，
// 未改动部分的注释、引号和块样式得以保留，画布与 YAML 文本始终是同一份数据。
import { isMap, isScalar, isSeq, parseDocument, Scalar, visit, type Document } from 'yaml'

export type Dict = Record<string, string>

export interface StepSpec {
  name?: string
  run?: string
  uses?: string
  with?: Dict
  env?: Dict
  /** 服务连接名称 */
  connection?: string
  /** 内置步骤类型（with 为其输入） */
  kind?: string
}

/** 任务插件：任务结束时按状态发送通知。 */
export interface PluginSpec {
  name?: string
  type: string
  connection?: string
  url?: string
  secret?: string
  on?: string[]
  to?: string[]
  title?: string
  content?: string
}

/** 流水线变量：runtime 为 true 的可在运行时覆盖默认值。 */
export interface VarSpec {
  name: string
  value?: string
  description?: string
  runtime?: boolean
}

export interface CacheSpec {
  key?: string
  paths?: string[]
}

export interface JobSpec {
  name: string
  type?: string
  image?: string
  runsOn?: string[]
  needs?: string[] // undefined：默认依赖上一阶段全部 Job；[]：无依赖
  when?: string
  env?: Dict
  matrix?: Record<string, string[]>
  timeout?: string
  retry?: number
  checkout?: boolean
  artifacts?: string[]
  cache?: CacheSpec
  approvers?: string[]
  /** 主机部署：步骤通过 SSH 在主机组的每台主机上执行 */
  deploy?: DeploySpec
  /** JUnit 测试报告路径 */
  reports?: { junit?: string[] }
  /** 运行条件表达式 */
  if?: string
  plugins: PluginSpec[]
  /** 人工卡点的审批消息 */
  notify: { connection: string; to?: string[] }[]
  steps: StepSpec[]
}

export interface DeploySpec {
  hosts?: string
  /** 部署的上游构建物名称（「构建物上传」步骤的 name） */
  artifact?: string
  upload?: string
  target?: string
  batch?: number
}

export type JobKind = 'run' | 'deploy' | 'approval'
export const jobKind = (j: JobSpec): JobKind => (j.type === 'approval' ? 'approval' : j.deploy ? 'deploy' : 'run')

export interface StageSpec {
  name: string
  jobs: JobSpec[]
}

export interface SourceSpec {
  url?: string
  branch?: string
  username?: string
  token_secret?: string
  depth?: number
  /** 把运行结果回写为提交状态 */
  report_status?: boolean
  provider?: string
}

/** 并发控制：同组运行依次执行。 */
export interface ConcurrencySpec {
  group?: string
  cancel_previous?: boolean
  /** 同时执行的上限，默认 1 */
  max?: number
}

export interface NotifySpec {
  type?: string
  url?: string
  secret?: string
  on?: string[]
}

export type ParamType = 'string' | 'choice' | 'boolean'

/** 运行参数：手动运行时在表单中填写，注入为同名环境变量。 */
export interface ParamSpec {
  name: string
  type: ParamType
  default?: string
  options?: string[]
  description?: string
  required?: boolean
}

export interface PipelineSpec {
  name: string
  env?: Dict
  vars: VarSpec[]
  /** 关联的通用变量组；为空时注入全部 */
  var_groups?: string[]
  /** 缓存目录（容器内绝对路径） */
  cache_dirs?: string[]
  params: ParamSpec[]
  source?: SourceSpec
  notify: NotifySpec[]
  retention?: { artifacts?: string; runs?: number }
  concurrency?: ConcurrencySpec
  stages: StageSpec[]
}

export type Path = (string | number)[]
export type JobRef = { si: number; ji: number }

export const jobPath = ({ si, ji }: JobRef): Path => ['stages', si, 'jobs', ji]

// 不折行；流式集合写成 [a, b] 而非 [ a, b ]，与手写风格一致。
const FORMAT = { lineWidth: 0, flowCollectionPadding: false }

export function parse(text: string): { doc?: Document; spec?: PipelineSpec; error?: string } {
  const doc = parseDocument(text)
  if (doc.errors.length > 0) return { error: doc.errors[0].message }
  const root = doc.toJS() ?? {}
  if (typeof root !== 'object' || Array.isArray(root)) return { error: '顶层必须是映射（name / stages …）' }
  return { doc, spec: normalize(root) }
}

/** 解析 → 修改文档树 → 序列化。text 无法解析时抛错。 */
export function edit(text: string, fn: (doc: Document) => void): string {
  const doc = parseDocument(text)
  if (doc.errors.length > 0) throw new Error(doc.errors[0].message)
  if (!doc.contents) doc.contents = doc.createNode({}) as never
  fn(doc)
  return doc.toString(FORMAT)
}

// ---- 归一化：YAML 中的值可能是数字、null 或单个字符串，统一成编辑器使用的形状 ----

const str = (v: unknown) => (v === null || v === undefined ? undefined : String(v))
const strList = (v: unknown): string[] | undefined =>
  v === null || v === undefined ? undefined : (Array.isArray(v) ? v : [v]).map((x) => String(x ?? ''))
const obj = (v: unknown): Record<string, unknown> | undefined =>
  v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined
const dict = (v: unknown): Dict | undefined => {
  const o = obj(v)
  return o && Object.fromEntries(Object.entries(o).map(([k, x]) => [k, str(x) ?? '']))
}
const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : [])

export function normalize(root: Record<string, unknown>): PipelineSpec {
  const src = obj(root.source)
  const ret = obj(root.retention)
  return {
    name: str(root.name) ?? '',
    env: dict(root.env),
    vars: arr(root.vars).map((v) => {
      const o = obj(v) ?? {}
      return { name: str(o.name) ?? '', value: str(o.value), description: str(o.description), runtime: o.runtime === true }
    }),
    var_groups: strList(root.var_groups),
    cache_dirs: strList(root.cache_dirs),
    params: arr(root.params).map((p) => {
      const o = obj(p) ?? {}
      const type = str(o.type)
      return {
        name: str(o.name) ?? '',
        type: type === 'choice' || type === 'boolean' ? type : 'string',
        default: str(o.default),
        options: strList(o.options),
        description: str(o.description),
        required: o.required === true,
      }
    }),
    source: src && { ...src, url: str(src.url), branch: str(src.branch), token_secret: str(src.token_secret) },
    notify: arr(root.notify).map((n) => {
      const o = obj(n) ?? {}
      return { type: str(o.type), url: str(o.url), secret: str(o.secret), on: strList(o.on) }
    }),
    retention: ret && { artifacts: str(ret.artifacts), runs: typeof ret.runs === 'number' ? ret.runs : undefined },
    concurrency: root.concurrency === undefined || root.concurrency === null ? undefined : { group: str(obj(root.concurrency)?.group), cancel_previous: obj(root.concurrency)?.cancel_previous === true, max: typeof obj(root.concurrency)?.max === 'number' ? (obj(root.concurrency)!.max as number) : undefined },
    stages: arr(root.stages).map((s) => {
      const st = obj(s) ?? {}
      return { name: str(st.name) ?? '', jobs: arr(st.jobs).map(normalizeJob) }
    }),
  }
}

function normalizeJob(v: unknown): JobSpec {
  const j = obj(v) ?? {}
  const m = obj(j.matrix)
  const c = obj(j.cache)
  return {
    name: str(j.name) ?? '',
    type: str(j.type),
    image: str(j.image),
    runsOn: strList(j['runs-on']),
    needs: strList(j.needs),
    when: str(j.when),
    env: dict(j.env),
    matrix: m && Object.fromEntries(Object.entries(m).map(([k, x]) => [k, strList(x) ?? []])),
    timeout: str(j.timeout),
    retry: typeof j.retry === 'number' ? j.retry : undefined,
    checkout: typeof j.checkout === 'boolean' ? j.checkout : undefined,
    artifacts: strList(j.artifacts),
    cache: c && { key: str(c.key), paths: strList(c.paths) },
    approvers: strList(j.approvers),
    deploy: obj(j.deploy) && {
      hosts: str(obj(j.deploy)!.hosts),
      artifact: str(obj(j.deploy)!.artifact),
      upload: str(obj(j.deploy)!.upload),
      target: str(obj(j.deploy)!.target),
      batch: typeof obj(j.deploy)!.batch === 'number' ? (obj(j.deploy)!.batch as number) : undefined,
    },
    reports: obj(j.reports) && { junit: strList(obj(j.reports)!.junit) },
    if: str(j.if),
    plugins: arr(j.plugins).map((x) => {
      const o = obj(x) ?? {}
      return { name: str(o.name), type: str(o.type) ?? 'webhook', connection: str(o.connection), url: str(o.url), secret: str(o.secret), on: strList(o.on), to: strList(o.to), title: str(o.title), content: str(o.content) }
    }),
    notify: arr(j.notify).map((x) => ({ connection: str(obj(x)?.connection) ?? '', to: strList(obj(x)?.to) })),
    steps: arr(j.steps).map((s) => {
      const o = obj(s) ?? {}
      return { name: str(o.name), run: str(o.run), uses: str(o.uses), with: dict(o.with), env: dict(o.env), connection: str(o.connection), kind: str(o.kind) }
    }),
  }
}

// ---- 依赖与校验 ----

export interface Dep {
  deps: string[]
  explicit: boolean
}

/** 与服务端 Spec.Dependencies 相同的语义，按位置（stage/job 下标）索引。 */
export function dependencies(spec: PipelineSpec): Dep[][] {
  let prev: string[] = []
  return spec.stages.map((st) => {
    const out = st.jobs.map((j) => (j.needs ? { deps: j.needs, explicit: true } : { deps: prev, explicit: false }))
    prev = st.jobs.map((j) => j.name)
    return out
  })
}

export const isApproval = (j: JobSpec) => j.type === 'approval'

/** 结构性问题（就地标注到卡片上）；其余规则由服务端校验接口给出。 */
export function issues(spec: PipelineSpec) {
  const stageIssues: string[][] = spec.stages.map(() => [])
  const jobIssues: string[][][] = spec.stages.map((st) => st.jobs.map(() => []))
  const stageOf = new Map<string, number>()
  const stageNames = new Set<string>()
  spec.stages.forEach((st, si) => {
    if (!st.name.trim()) stageIssues[si].push('阶段名称不能为空')
    else if (stageNames.has(st.name)) stageIssues[si].push(`阶段名称「${st.name}」重复`)
    stageNames.add(st.name)
    if (st.jobs.length === 0) stageIssues[si].push('阶段内至少需要一个任务')
    st.jobs.forEach((j, ji) => {
      const out = jobIssues[si][ji]
      if (!j.name.trim()) out.push('任务名称不能为空')
      else if (stageOf.has(j.name)) out.push(`任务名称「${j.name}」重复`)
      else stageOf.set(j.name, si)
      if (isApproval(j)) {
        if (j.steps.length > 0) out.push('人工卡点不能包含步骤')
      } else if (j.steps.length === 0) out.push('至少需要一个步骤')
      j.steps.forEach((s, k) => {
        if (!s.uses && !s.kind && !s.run?.trim()) out.push(`步骤 ${k + 1} 缺少命令`)
      })
    })
  })
  spec.stages.forEach((st, si) =>
    st.jobs.forEach((j, ji) => {
      for (const n of j.needs ?? []) {
        const ds = stageOf.get(n)
        if (n === j.name) jobIssues[si][ji].push('不能依赖自身')
        else if (ds === undefined) jobIssues[si][ji].push(`依赖的任务「${n}」不存在`)
        else if (ds > si) jobIssues[si][ji].push(`不能依赖后面阶段的任务「${n}」`)
      }
    }),
  )
  return { stageIssues, jobIssues }
}

export function uniqueName(base: string, taken: Iterable<string>) {
  const set = new Set(taken)
  if (!set.has(base)) return base
  for (let i = 2; ; i++) if (!set.has(`${base}-${i}`)) return `${base}-${i}`
}

export const allJobNames = (spec: PipelineSpec) => spec.stages.flatMap((s) => s.jobs.map((j) => j.name))

export const newStageNames = (spec: PipelineSpec) => ({
  stage: uniqueName(`stage-${spec.stages.length + 1}`, spec.stages.map((s) => s.name)),
  job: uniqueName('job', allJobNames(spec)),
})


// ---- 文档树操作 ----

function isEmpty(v: unknown) {
  if (v === undefined || v === null || v === '') return true
  if (Array.isArray(v)) return false // [] 有意义（如 needs: [] 表示无依赖），由调用方决定是否删除
  if (typeof v === 'object') return Object.keys(v).length === 0
  return false
}

/** 新建节点：标量列表用流式 [a, b]，多行字符串用 | 块。 */
function makeNode(doc: Document, value: unknown) {
  const node = doc.createNode(value)
  if (isSeq(node) && node.items.every(isScalar)) node.flow = true
  visit(node, {
    Seq(_, n) {
      if (n.items.every(isScalar)) n.flow = true
    },
    Scalar(_, n) {
      if (typeof n.value === 'string' && n.value.includes('\n')) n.type = Scalar.BLOCK_LITERAL
    },
  })
  return node
}

/** 设置字段；空值删除该字段。已有标量节点只改值，保留原有引号 / 块样式。 */
export function setField(doc: Document, path: Path, value: unknown) {
  if (isEmpty(value)) {
    if (doc.hasIn(path)) doc.deleteIn(path)
    // 清空最后一个字段时连同空的父映射一起删除（如 source: {}、cache: {}），列表项除外。
    for (let p = path.slice(0, -1); p.length > 0; p = p.slice(0, -1)) {
      const parent = doc.getIn(p, true)
      const grand = p.length > 1 ? doc.getIn(p.slice(0, -1), true) : doc.contents
      if (!isMap(parent) || parent.items.length > 0 || !isMap(grand)) break
      doc.deleteIn(p)
    }
    return
  }
  const cur = doc.getIn(path, true)
  if (isScalar(cur) && (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean')) {
    cur.value = value
    if (typeof value === 'string' && value.includes('\n')) cur.type = Scalar.BLOCK_LITERAL
    else if (cur.type === Scalar.BLOCK_LITERAL || cur.type === Scalar.BLOCK_FOLDED) cur.type = undefined
    return
  }
  const isNew = !doc.hasIn(path)
  doc.setIn(path, makeNode(doc, value))
  if (isNew) placeKey(doc, path)
}

// 新增字段按惯用顺序插入（而不是追加在 steps 之后），保持 YAML 易读。
const KEY_ORDER: Record<string, string[]> = {
  root: ['name', 'env', 'vars', 'var_groups', 'cache_dirs', 'params', 'source', 'notify', 'retention', 'concurrency', 'stages'],
  var: ['name', 'value', 'runtime', 'description'],
  param: ['name', 'type', 'options', 'default', 'required', 'description'],
  job: ['name', 'type', 'image', 'runs-on', 'needs', 'when', 'if', 'deploy', 'env', 'matrix', 'timeout', 'retry', 'checkout', 'artifacts', 'reports', 'cache', 'approvers', 'notify', 'plugins', 'steps'],
  step: ['name', 'kind', 'uses', 'connection', 'with', 'env', 'run'],
}

function placeKey(doc: Document, path: Path) {
  const parentPath = path.slice(0, -1)
  const key = path[path.length - 1]
  const order =
    parentPath.length === 0 ? KEY_ORDER.root
    : parentPath.length === 4 && parentPath[0] === 'stages' && parentPath[2] === 'jobs' ? KEY_ORDER.job
    : parentPath.length === 6 && parentPath[4] === 'steps' ? KEY_ORDER.step
    : parentPath.length === 2 && parentPath[0] === 'params' ? KEY_ORDER.param
    : parentPath.length === 2 && parentPath[0] === 'vars' ? KEY_ORDER.var
    : undefined
  const parent = parentPath.length ? doc.getIn(parentPath, true) : doc.contents
  if (!order || !isMap(parent) || typeof key !== 'string' || !order.includes(key)) return
  // setIn 新建的键是原始字符串，解析得到的键是 Scalar。
  const keyOf = (k: unknown) => String(isScalar(k) ? k.value : k)
  const items = parent.items
  const from = items.findIndex((it) => keyOf(it.key) === key)
  if (from < 0) return
  const [pair] = items.splice(from, 1)
  const rank = order.indexOf(key)
  const at = items.findIndex((it) => order.indexOf(keyOf(it.key)) > rank)
  items.splice(at < 0 ? items.length : at, 0, pair)
}

function seqAt(doc: Document, path: Path) {
  let node = doc.getIn(path, true)
  if (!isSeq(node)) {
    const isNew = !doc.hasIn(path)
    doc.setIn(path, doc.createNode([]))
    if (isNew) placeKey(doc, path)
    node = doc.getIn(path, true)
  }
  if (!isSeq(node)) throw new Error(`${path.join('.')} 不是列表`)
  // 块样式便于后续插入多行项。
  node.flow = false
  return node
}

function insertAt(doc: Document, path: Path, index: number, value: unknown) {
  const seq = seqAt(doc, path)
  const node = isMap(value) || isSeq(value) || isScalar(value) ? value : makeNode(doc, value)
  seq.items.splice(Math.min(Math.max(index, 0), seq.items.length), 0, node as never)
}

function removeAt(doc: Document, path: Path, index: number) {
  const seq = seqAt(doc, path)
  return seq.items.splice(index, 1)[0]
}

/** 把所有 needs 中的 oldName 改名或删除（newName 为 undefined）。 */
function rewriteNeeds(doc: Document, spec: PipelineSpec, oldName: string, newName?: string) {
  spec.stages.forEach((st, si) =>
    st.jobs.forEach((j, ji) => {
      if (!j.needs?.includes(oldName)) return
      const next = newName === undefined ? j.needs.filter((n) => n !== oldName) : j.needs.map((n) => (n === oldName ? newName : n))
      // 删除唯一依赖时退回默认依赖，避免意外变成「无依赖」而提前运行。
      if (next.length === 0) doc.deleteIn([...jobPath({ si, ji }), 'needs'])
      else setField(doc, [...jobPath({ si, ji }), 'needs'], next)
    }),
  )
}

export const ops = {
  set: (path: Path, value: unknown) => (doc: Document) => setField(doc, path, value),

  /** 新阶段带一个空任务（服务端要求阶段内至少一个任务）。名称由 newStageNames 生成。 */
  addStage: (at: number, names: { stage: string; job: string }) => (doc: Document) => {
    insertAt(doc, ['stages'], at, { name: names.stage, jobs: [newJob(names.job)] })
  },

  deleteStage: (spec: PipelineSpec, si: number) => (doc: Document) => {
    removeAt(doc, ['stages'], si)
    const rest = { ...spec, stages: spec.stages.filter((_, i) => i !== si) }
    for (const j of spec.stages[si].jobs) rewriteNeeds(doc, rest, j.name)
  },

  moveStage: (from: number, to: number) => (doc: Document) => {
    if (from === to) return
    const node = removeAt(doc, ['stages'], from)
    insertAt(doc, ['stages'], to, node)
  },

  /** 插入组件库生成的任务定义（含 name）。 */
  insertJob: (si: number, at: number, job: Record<string, unknown>) => (doc: Document) => {
    insertAt(doc, ['stages', si, 'jobs'], at, job)
  },

  duplicateJob: (spec: PipelineSpec, ref: JobRef) => (doc: Document) => {
    const node = doc.getIn(jobPath(ref), true)
    if (!isMap(node)) return
    const copy = node.clone()
    copy.set('name', uniqueName(`${spec.stages[ref.si].jobs[ref.ji].name}-copy`, allJobNames(spec)))
    insertAt(doc, ['stages', ref.si, 'jobs'], ref.ji + 1, copy)
  },

  deleteJob: (spec: PipelineSpec, ref: JobRef) => (doc: Document) => {
    const name = spec.stages[ref.si].jobs[ref.ji].name
    removeAt(doc, ['stages', ref.si, 'jobs'], ref.ji)
    const rest = structuredClone(spec)
    rest.stages[ref.si].jobs.splice(ref.ji, 1)
    rewriteNeeds(doc, rest, name)
  },

  /**
   * 移动 Job；to.si === stages.length 时新建阶段。移空的阶段随之删除。
   * to.ji 是目标阶段中「移除源之前」的插入位置。
   */
  moveJob: (spec: PipelineSpec, from: JobRef, to: JobRef) => (doc: Document) => {
    let toJi = to.ji
    if (from.si === to.si && from.ji < toJi) toJi--
    if (from.si === to.si && from.ji === toJi) return
    // 阶段里唯一的任务拖到「新阶段」：等同于把整个阶段移到末尾，保留阶段名。
    if (to.si >= spec.stages.length && spec.stages[from.si].jobs.length === 1) {
      ops.moveStage(from.si, spec.stages.length - 1)(doc)
      return
    }
    const node = removeAt(doc, ['stages', from.si, 'jobs'], from.ji)
    if (to.si >= spec.stages.length) {
      insertAt(doc, ['stages'], spec.stages.length, { name: newStageNames(spec).stage, jobs: [] })
      insertAt(doc, ['stages', spec.stages.length, 'jobs'], 0, node)
    } else {
      insertAt(doc, ['stages', to.si, 'jobs'], toJi, node)
    }
    if (spec.stages[from.si].jobs.length === 1) removeAt(doc, ['stages'], from.si)
  },

  renameJob: (spec: PipelineSpec, ref: JobRef, name: string) => (doc: Document) => {
    const old = spec.stages[ref.si].jobs[ref.ji].name
    setField(doc, [...jobPath(ref), 'name'], name)
    // 重名时不改引用，避免把两个任务的依赖混在一起。
    if (old && name && !allJobNames(spec).includes(name)) rewriteNeeds(doc, spec, old, name)
  },

  /** 设置依赖；undefined 恢复默认（依赖上一阶段全部任务）。 */
  setNeeds: (ref: JobRef, needs: string[] | undefined) => (doc: Document) => {
    const path = [...jobPath(ref), 'needs']
    if (needs === undefined) {
      if (doc.hasIn(path)) doc.deleteIn(path)
      return
    }
    const isNew = !doc.hasIn(path)
    doc.setIn(path, makeNode(doc, needs))
    if (isNew) placeKey(doc, path)
  },

  /** 切换任务类型：执行命令 / 主机部署 / 人工卡点。hostGroup 为切到部署时默认的主机组。 */
  setKind: (ref: JobRef, kind: JobKind, hostGroup?: string) => (doc: Document) => {
    const p = jobPath(ref)
    const drop = (...keys: string[]) => keys.forEach((k) => doc.hasIn([...p, k]) && doc.deleteIn([...p, k]))
    const ensureSteps = (run: string) => {
      const steps = doc.getIn([...p, 'steps'], true)
      if (!isSeq(steps) || steps.items.length === 0) setField(doc, [...p, 'steps'], [{ name: 'step-1', run }])
    }
    if (kind === 'approval') {
      drop('steps', 'image', 'artifacts', 'cache', 'matrix', 'retry', 'deploy', 'runs-on')
      setField(doc, [...p, 'type'], 'approval')
    } else if (kind === 'deploy') {
      drop('type', 'approvers', 'image', 'cache', 'matrix')
      const node = doc.createNode({ hosts: hostGroup ?? '' })
      doc.setIn([...p, 'deploy'], node)
      placeKey(doc, [...p, 'deploy'])
      ensureSteps('echo "deploy to $FLOW_DEPLOY_HOST"')
    } else {
      drop('type', 'approvers', 'deploy')
      ensureSteps('echo hello')
    }
  },

  addStep: (ref: JobRef, index: number, step: StepSpec) => (doc: Document) => insertAt(doc, [...jobPath(ref), 'steps'], index, step),

  deleteStep: (ref: JobRef, k: number) => (doc: Document) => {
    removeAt(doc, [...jobPath(ref), 'steps'], k)
  },

  moveStep: (ref: JobRef, from: number, to: number) => (doc: Document) => {
    if (from === to) return
    const node = removeAt(doc, [...jobPath(ref), 'steps'], from)
    insertAt(doc, [...jobPath(ref), 'steps'], to, node)
  },

  addParam: (spec: PipelineSpec) => (doc: Document) =>
    insertAt(doc, ['params'], 1e9, { name: uniqueName('PARAM', spec.params.map((p) => p.name)), type: 'string' }),
  deleteParam: (i: number) => (doc: Document) => {
    removeAt(doc, ['params'], i)
    const seq = doc.getIn(['params'], true)
    if (isSeq(seq) && seq.items.length === 0) doc.deleteIn(['params'])
  },
  moveParam: (from: number, to: number) => (doc: Document) => {
    if (from === to) return
    insertAt(doc, ['params'], to, removeAt(doc, ['params'], from))
  },
  /** 切换类型时去掉新类型不适用的选项和默认值。 */
  setParamType: (spec: PipelineSpec, i: number, type: ParamType) => (doc: Document) => {
    const p = spec.params[i]
    const base = ['params', i]
    setField(doc, [...base, 'type'], type)
    if (type !== 'choice' && doc.hasIn([...base, 'options'])) doc.deleteIn([...base, 'options'])
    // 有默认值时把它作为第一个可选值；否则留空，由表单提示添加。
    if (type === 'choice' && !p.options?.length && p.default) setField(doc, [...base, 'options'], [p.default])
    const d = p.default
    const keep =
      !d || type === 'string' ? true
      : type === 'boolean' ? d === 'true' || d === 'false'
      : !p.options?.length || p.options.includes(d)
    if (!keep) doc.deleteIn([...base, 'default'])
  },

  addNotify: () => (doc: Document) => insertAt(doc, ['notify'], 1e9, { type: 'dingtalk', url: '', on: ['failed'] }),
  /** 开启（写入 concurrency: {}）或关闭并发控制。 */
  setConcurrency: (on: boolean) => (doc: Document) => {
    if (!on) {
      doc.deleteIn(['concurrency'])
      return
    }
    if (doc.hasIn(['concurrency'])) return
    const node = doc.createNode({})
    node.flow = true
    doc.setIn(['concurrency'], node)
    placeKey(doc, ['concurrency'])
  },
  /** 设置并发控制的字段；清空字段时保留 concurrency: {}（仍表示开启）。 */
  setConcurrencyField: (key: 'group' | 'cancel_previous' | 'max', value: unknown) => (doc: Document) => {
    if (isEmpty(value) || value === false) {
      doc.deleteIn(['concurrency', key])
      const node = doc.getIn(['concurrency'], true)
      if (isMap(node) && node.items.length === 0) node.flow = true
      return
    }
    setField(doc, ['concurrency', key], value)
  },

  addVar: (spec: PipelineSpec) => (doc: Document) => {
    let n = spec.vars.length + 1
    while (spec.vars.some((v) => v.name === `VAR_${n}`) || spec.env?.[`VAR_${n}`] !== undefined) n++
    insertAt(doc, ['vars'], 1e9, { name: `VAR_${n}`, value: '' })
  },
  deleteVar: (i: number) => (doc: Document) => {
    removeAt(doc, ['vars'], i)
    const seq = doc.getIn(['vars'], true)
    if (isSeq(seq) && seq.items.length === 0) doc.deleteIn(['vars'])
  },
  /** 把 env 中的变量移到 vars（以便设置说明与运行时开关）。 */
  envToVar: (name: string, value: string) => (doc: Document) => {
    doc.deleteIn(['env', name])
    const env = doc.getIn(['env'], true)
    if (isMap(env) && env.items.length === 0) doc.deleteIn(['env'])
    insertAt(doc, ['vars'], 1e9, { name, value })
  },

  addPlugin: (ref: JobRef) => (doc: Document) => insertAt(doc, [...jobPath(ref), 'plugins'], 1e9, { type: 'webhook', url: '', on: ['failure'] }),
  deletePlugin: (ref: JobRef, i: number) => (doc: Document) => {
    removeAt(doc, [...jobPath(ref), 'plugins'], i)
    const seq = doc.getIn([...jobPath(ref), 'plugins'], true)
    if (isSeq(seq) && seq.items.length === 0) doc.deleteIn([...jobPath(ref), 'plugins'])
  },

  deleteNotify: (i: number) => (doc: Document) => {
    removeAt(doc, ['notify'], i)
    const seq = doc.getIn(['notify'], true)
    if (isSeq(seq) && seq.items.length === 0) doc.deleteIn(['notify'])
  },
}

function newJob(name: string) {
  return { name, steps: [{ name: 'step-1', run: 'echo hello' }] }
}

/** 新建流水线时的初始 YAML。 */
export const BLANK = `name: my-pipeline
stages:
  - name: build
    jobs:
      - name: build
        steps:
          - name: build
            run: echo "building"
  - name: deploy
    jobs:
      - name: deploy
        steps:
          - name: deploy
            run: echo "deploying"
`
