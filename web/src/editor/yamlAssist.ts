import { isMap, isScalar, isSeq, parseDocument, type Document, type Node } from 'yaml'
import { isApproval, normalize } from './model'

/** 编辑器中的一个问题；from / to 为字符偏移。 */
export interface Problem {
  from: number
  to: number
  line: number
  message: string
  severity: 'error' | 'warning'
}

type Range = [number, number]

// ---- 字段说明（补全与悬停提示共用） ----

type Keys = Record<string, string>

const ROOT: Keys = {
  name: '流水线名称',
  env: '全局环境变量',
  vars: '变量：name / value / description / runtime（运行时可覆盖）',
  var_groups: '关联的通用变量组名称；省略注入全部',
  cache_dirs: '缓存目录：容器内绝对路径，按流水线持久化',
  params: '运行参数：手动运行时填写的表单',
  source: '代码源：Job 执行前自动拉取',
  notify: '运行结束通知（webhook / 飞书 / 钉钉 / 企业微信）',
  retention: '产物与运行记录的保留策略',
  concurrency: '并发控制：同组运行依次执行，其余排队',
  stages: '阶段列表，按顺序执行',
}
const STAGE: Keys = { name: '阶段名称', jobs: '阶段内的任务，默认并行' }
const JOB: Keys = {
  name: '任务名称（全流水线唯一）',
  type: 'approval 为人工卡点',
  image: '容器镜像；省略在 Runner 主机上执行',
  'runs-on': 'Runner 标签，全部匹配才会领取',
  needs: '依赖的任务（可跨阶段，只能依赖前面的）',
  when: 'on_success（默认） / on_failure / always',
  env: '任务环境变量',
  matrix: '矩阵：按变量取值组合展开为多个任务',
  timeout: '超时，如 30m、1h30m',
  retry: '失败后自动重试次数',
  checkout: 'false 不拉取代码',
  artifacts: '成功后上传的产物路径，下游任务自动下载',
  cache: '缓存：key 与 paths',
  approvers: '人工卡点的审批人用户名',
  deploy: '主机部署：步骤通过 SSH 在主机组每台主机上执行',
  reports: '测试报告：junit 为 JUnit XML 路径（支持通配符）',
  if: "运行条件，如 FLOW_BRANCH == 'main'",
  plugins: '任务插件：结束时按状态发送通知',
  notify: '人工卡点的审批消息：connection / to',
  steps: '步骤列表，依次执行',
}
const DEPLOY: Keys = {
  hosts: '主机组名（在「环境与主机」中维护）',
  upload: '上传工作目录中的目录或文件（如上游产物 dist）',
  target: '远程目录：上传到这里并作为脚本的工作目录',
  batch: '每批主机数，默认 1；前一批成功才继续',
}
const STEP: Keys = {
  name: '步骤名称',
  run: 'Shell 命令',
  kind: '内置步骤类型，如 setup-node、docker-build',
  uses: '引用团队步骤模板；name@N 锁定第 N 版',
  with: '内置步骤 / 模板的输入',
  connection: '服务连接名称，字段注入为 CONN_*',
  env: '步骤环境变量',
}
const VAR: Keys = { name: '变量名', value: '默认值', description: '说明', runtime: 'true 时运行时可覆盖' }
const PLUGIN: Keys = {
  name: '插件名称',
  type: 'webhook / dingtalk / feishu / wecom / bark / telegram / email',
  connection: '服务连接（wecom / bark / telegram / email）',
  url: '地址（webhook / dingtalk / feishu）',
  secret: '加签密钥',
  on: 'success / failure / cancel / always',
  to: '邮件收件人',
  title: '标题模板',
  content: '内容模板',
}
const PARAM: Keys = {
  name: '参数名，注入为环境变量',
  type: 'string（默认） / choice / boolean',
  options: 'choice 的可选值',
  default: '默认值',
  required: '手动运行时必须填写',
  description: '表单中的说明',
}
const SOURCE: Keys = {
  url: '仓库地址',
  branch: '默认分支',
  username: 'HTTP 用户名，默认 oauth2',
  token_secret: '存放访问令牌的密钥名',
  depth: '浅克隆深度，默认 50',
  report_status: 'true 时把运行结果回写为提交状态',
  provider: 'github / gitlab / gitea，省略时按地址推断',
}
const CONCURRENCY: Keys = { group: '并发组名，可含 ${参数}；省略为本流水线', cancel_previous: 'true 时新运行取消之前未结束的运行', max: '同时执行的运行数上限，默认 1' }
const NOTIFY: Keys = { type: 'webhook / feishu / dingtalk / wecom', url: '回调地址，可含 ${密钥名}', secret: '加签密钥', on: '触发状态，如 [failed]' }
const RETENTION: Keys = { artifacts: '产物保留时长，如 7d；0 为永久', runs: '最多保留的运行数' }
const CACHE: Keys = { key: '缓存键，可含 ${变量}', paths: '缓存的路径' }

/** 按光标所在位置的 YAML 路径（序列项记为 -）给出可用的键。 */
export function keysFor(path: string[]): Keys | undefined {
  const p = path.join('.')
  if (p === '') return ROOT
  if (p === 'stages.-') return STAGE
  if (p === 'stages.-.jobs.-') return JOB
  if (p === 'stages.-.jobs.-.steps.-') return STEP
  if (p === 'stages.-.jobs.-.cache') return CACHE
  if (p === 'stages.-.jobs.-.deploy') return DEPLOY
  if (p === 'stages.-.jobs.-.reports') return { junit: 'JUnit XML 路径列表，如 [target/surefire-reports/*.xml]' }
  if (p === 'params.-') return PARAM
  if (p === 'vars.-') return VAR
  if (p === 'stages.-.jobs.-.plugins.-') return PLUGIN
  if (p === 'stages.-.jobs.-.notify.-') return { connection: '服务连接名称', to: '邮件收件人' }
  if (p === 'source') return SOURCE
  if (p === 'notify.-') return NOTIFY
  if (p === 'retention') return RETENTION
  if (p === 'concurrency') return CONCURRENCY
}

/** 键的可选取值。 */
export function valuesFor(path: string[], key: string, templates: string[]): string[] | undefined {
  const keys = keysFor(path)
  if (keys === JOB && key === 'type') return ['approval']
  if (keys === JOB && key === 'when') return ['on_success', 'on_failure', 'always']
  if (keys === JOB && key === 'checkout') return ['false', 'true']
  if (keys === PARAM && key === 'type') return ['string', 'choice', 'boolean']
  if (keys === PARAM && key === 'required') return ['true', 'false']
  if (keys === NOTIFY && key === 'type') return ['webhook', 'feishu', 'dingtalk', 'wecom']
  if (keys === SOURCE && key === 'provider') return ['github', 'gitlab', 'gitea']
  if ((keys === SOURCE && key === 'report_status') || (keys === CONCURRENCY && key === 'cancel_previous')) return ['true', 'false']
  if (keys === STEP && key === 'uses') return templates
}

export function describeKey(path: string[], key: string): string | undefined {
  return keysFor(path)?.[key]
}

// ---- 光标处的上下文（按缩进推断，容忍尚未写完、无法解析的 YAML） ----

interface LineInfo {
  dash: number // "- " 的列，-1 表示不是序列项
  key: number // 键（或值）开始的列，空行为 -1
  name?: string // 键名
  open: boolean // 形如 "key:" 且值在下面的行
}

const KEY_RE = /^([A-Za-z_][\w-]*)\s*:(\s|$)/

function lineInfo(line: string): LineInfo {
  let i = 0
  while (line[i] === ' ') i++
  if (i >= line.length || line[i] === '#') return { dash: -1, key: -1, open: false }
  let dash = -1
  while (line[i] === '-' && (line[i + 1] === ' ' || i + 1 === line.length)) {
    dash = i
    i++
    while (line[i] === ' ') i++
  }
  const rest = line.slice(i)
  const m = KEY_RE.exec(rest)
  const value = m ? rest.slice(m[0].length).replace(/#.*$/, '').trim() : ''
  return { dash, key: i, name: m?.[1], open: !!m && value === '' }
}

/**
 * 光标所在行的键路径，例如 ['stages', '-', 'jobs', '-'] 表示在某个任务中。
 * lineNo 从 0 开始；col 为光标所在列（用于空行时判断缩进）。
 */
export function pathAt(lines: string[], lineNo: number, col: number): string[] {
  const path: string[] = []
  const cur = lineInfo(lines[lineNo] ?? '')
  let target = cur.key >= 0 ? cur.key : col
  if (cur.dash >= 0) {
    path.push('-')
    target = cur.dash
  }
  for (let n = lineNo - 1; n >= 0 && target > 0; n--) {
    const l = lineInfo(lines[n])
    if (l.key < 0) continue
    if (l.dash >= 0 && l.dash < target && l.key <= target) {
      // 同一序列项中的前面的键（或序列项本身）
      path.push('-')
      target = l.dash
    } else if (l.key < target) {
      if (!l.open || !l.name) return ['?'] // 落在标量 / 流式值中，不提示
      path.push(l.name)
      target = l.key
    }
  }
  return path.reverse()
}

/** 当前行的键名（光标在值上时用于取值补全）。 */
export function keyOfLine(line: string): string | undefined {
  return lineInfo(line).name
}

// ---- 问题定位 ----

function lineOf(text: string, pos: number) {
  let n = 1
  for (let i = 0; i < pos && i < text.length; i++) if (text.charCodeAt(i) === 10) n++
  return n
}

function lineRange(text: string, line: number): Range {
  let start = 0
  for (let n = 1; n < line; n++) {
    const i = text.indexOf('\n', start)
    if (i < 0) return [text.length, text.length]
    start = i + 1
  }
  const end = text.indexOf('\n', start)
  return [start, end < 0 ? text.length : end]
}

/** 节点首行（从键开始到行尾），避免把整个多行任务都标红。 */
function headRange(text: string, node: Node | null | undefined, keyNode?: unknown): Range | undefined {
  const r = (keyNode as Node | undefined)?.range ?? node?.range
  if (!r) return
  const end = text.indexOf('\n', r[0])
  return [r[0], Math.max(r[0] + 1, Math.min(end < 0 ? text.length : end, node?.range?.[1] ?? r[1]))]
}

function pairRange(text: string, doc: Document, path: (string | number)[]): Range | undefined {
  const parentPath = path.slice(0, -1)
  const key = path[path.length - 1]
  const parent = parentPath.length ? doc.getIn(parentPath, true) : doc.contents
  if (isMap(parent)) {
    const pair = parent.items.find((it) => (isScalar(it.key) ? it.key.value : it.key) === key)
    if (pair) return headRange(text, pair.value as Node, pair.key)
  }
  if (isSeq(parent) && typeof key === 'number') return headRange(text, parent.items[key] as Node)
}

function findJob(doc: Document, name: string): (string | number)[] | undefined {
  const stages = doc.get('stages', true)
  if (!isSeq(stages)) return
  for (let si = 0; si < stages.items.length; si++) {
    const jobs = doc.getIn(['stages', si, 'jobs'], true)
    if (!isSeq(jobs)) continue
    for (let ji = 0; ji < jobs.items.length; ji++) {
      if (doc.getIn(['stages', si, 'jobs', ji, 'name']) === name) return ['stages', si, 'jobs', ji]
    }
  }
}

function findStage(doc: Document, name: string): (string | number)[] | undefined {
  const stages = doc.get('stages', true)
  if (!isSeq(stages)) return
  const si = stages.items.findIndex((_, i) => doc.getIn(['stages', i, 'name']) === name)
  return si >= 0 ? ['stages', si] : undefined
}

function findParam(doc: Document, name: string): (string | number)[] | undefined {
  const params = doc.get('params', true)
  if (!isSeq(params)) return
  const i = params.items.findIndex((_, k) => doc.getIn(['params', k, 'name']) === name)
  return i >= 0 ? ['params', i] : undefined
}

/** 把服务端校验错误（英文，含任务 / 阶段名或行号）定位到文档中；定位不到时返回 undefined。 */
export function locateServerError(text: string, doc: Document, message: string): Range | undefined {
  const line = /\bline (\d+)\b/.exec(message)
  if (line) return lineRange(text, Number(line[1]))
  const at = (path: (string | number)[] | undefined, sub?: (string | number)[]) => {
    if (!path) return
    if (sub) {
      const r = pairRange(text, doc, [...path, ...sub])
      if (r) return r
    }
    return pairRange(text, doc, [...path, 'name']) ?? pairRange(text, doc, path)
  }
  // 名称之后提到的第一个字段，如 job "x": invalid timeout → timeout 所在行。
  const rest = message.replace(/^.*?\b(?:job|param) "[^"]+"/, '')
  const step = /steps\[(\d+)\]/.exec(rest)
  const field = /\b(needs|image|when|timeout|retry|artifacts|cache|matrix|type|options|default|approvers|runs-on)\b/.exec(rest)
  const sub = step ? ['steps', Number(step[1])] : field ? [field[1]] : undefined
  let m = /\bjob "([^"]+)"/.exec(message)
  if (m) return at(findJob(doc, m[1]), sub)
  m = /\bstage "([^"]+)"(?: jobs\[(\d+)\])?/.exec(message)
  if (m) return at(findStage(doc, m[1]), m[2] ? ['jobs', Number(m[2])] : undefined)
  m = /\bparam "?([A-Za-z_]\w*)"?/.exec(message)
  if (m) return at(findParam(doc, m[1]), sub)
  m = /\b(stages|params|notify)\[(\d+)\]/.exec(message)
  if (m) return pairRange(text, doc, [m[1], Number(m[2])])
  m = /^(source|retention|notify|params|stages|env|concurrency)\b/.exec(message)
  if (m) return pairRange(text, doc, [m[1]])
}

/**
 * 编辑器中的问题：YAML 语法错误、结构问题（与画布上的标注一致）、服务端校验错误。
 * serverError 为当前内容的服务端校验结果。
 */
export function findProblems(text: string, serverError?: string): Problem[] {
  const out: Problem[] = []
  const add = (r: Range | undefined, message: string, severity: Problem['severity'] = 'error') => {
    const [from, to] = r ?? lineRange(text, 1)
    out.push({ from, to: Math.max(to, from), line: lineOf(text, from), message, severity })
  }
  const doc = parseDocument(text, { prettyErrors: false })
  if (doc.errors.length > 0) {
    for (const e of doc.errors) add([e.pos[0], Math.max(e.pos[1], e.pos[0] + 1)], e.message.split('\n')[0])
    return out
  }
  const root = doc.toJS()
  if (root !== null && root !== undefined && (typeof root !== 'object' || Array.isArray(root))) {
    add(undefined, '顶层必须是映射（name / stages …）')
    return out
  }
  const spec = normalize(root ?? {})
  for (const pair of isMap(doc.contents) ? doc.contents.items : []) {
    const k = isScalar(pair.key) ? String(pair.key.value) : ''
    if (k && !(k in ROOT)) add(headRange(text, pair.value as Node, pair.key), `未知的字段「${k}」`)
  }

  const seen = new Map<string, number>()
  spec.stages.forEach((st, si) => {
    const stagePath = ['stages', si]
    if (!st.name.trim()) add(pairRange(text, doc, stagePath), '阶段名称不能为空')
    if (st.jobs.length === 0) add(pairRange(text, doc, [...stagePath, 'name']) ?? pairRange(text, doc, stagePath), '阶段内至少需要一个任务')
    st.jobs.forEach((j, ji) => {
      const jp = [...stagePath, 'jobs', ji]
      const at = (sub?: (string | number)[]) => (sub && pairRange(text, doc, [...jp, ...sub])) || pairRange(text, doc, [...jp, 'name']) || pairRange(text, doc, jp)
      if (!j.name.trim()) add(at(), '任务名称不能为空')
      else if (seen.has(j.name)) add(at(['name']), `任务名称「${j.name}」重复`)
      else seen.set(j.name, si)
      if (isApproval(j)) {
        if (j.steps.length > 0) add(at(['steps']), '人工卡点不能包含步骤')
      } else if (j.steps.length === 0) add(at(), '至少需要一个步骤')
      j.steps.forEach((s, k) => {
        if (!s.uses && !(s as { kind?: string }).kind && !s.run?.trim()) add(at(['steps', k]), `步骤 ${k + 1} 缺少命令`)
      })
    })
  })
  spec.stages.forEach((st, si) =>
    st.jobs.forEach((j, ji) => {
      for (const n of j.needs ?? []) {
        const ds = seen.get(n)
        const msg = n === j.name ? '不能依赖自身' : ds === undefined ? `依赖的任务「${n}」不存在` : ds > si ? `不能依赖后面阶段的任务「${n}」` : ''
        if (msg) add(pairRange(text, doc, ['stages', si, 'jobs', ji, 'needs']), msg)
      }
    }),
  )
  // 本地规则都满足时才显示服务端错误，避免同一问题报两次。
  if (serverError && out.length === 0) add(locateServerError(text, doc, serverError), serverError.replace(/^invalid request: /, ''))
  return out
}
