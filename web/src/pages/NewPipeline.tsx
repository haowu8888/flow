import { useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../api'
import { FlowPreview, specTags } from '../components/FlowPreview'
import { BLANK, edit, parse, setField, type PipelineSpec } from '../editor/model'
import { PipelineEditor } from '../editor/PipelineEditor'
import { href, navigate, setLeaveGuard } from '../router'
import type { Environment, HostGroup, Secret, Template } from '../types'

type Step = 'template' | 'info' | 'review'

const BLANK_ID = 'blank'
const blankTemplate: Template = {
  id: BLANK_ID,
  kind: 'pipeline',
  name: '空白流水线',
  category: '通用',
  version: 0,
  description: '从「构建 → 部署」两个阶段开始，自己编排任务',
  yaml: BLANK,
  builtin: true,
  created_at: '',
  updated_at: '',
}

/** 向导第二步收集的信息，应用到模板 YAML 上。 */
interface Info {
  name: string
  group: string
  useSource: boolean
  url: string
  branch: string
  tokenSecret: string
  /** 部署任务名 → 主机组名 */
  hosts: Record<string, string>
  pushTrigger: boolean
}

function initialInfo(spec?: PipelineSpec): Info {
  const hosts: Record<string, string> = {}
  for (const st of spec?.stages ?? []) for (const j of st.jobs) if (j.deploy) hosts[j.name] = ''
  return {
    name: '',
    group: '',
    useSource: !!spec?.source,
    url: '',
    branch: spec?.source?.branch || 'main',
    tokenSecret: spec?.source?.token_secret ?? '',
    hosts,
    pushTrigger: false,
  }
}

function applyInfo(yaml: string, spec: PipelineSpec, info: Info): string {
  return edit(yaml, (doc) => {
    setField(doc, ['name'], info.name.trim())
    if (info.useSource) {
      setField(doc, ['source', 'url'], info.url.trim())
      setField(doc, ['source', 'branch'], info.branch.trim() || undefined)
      setField(doc, ['source', 'token_secret'], info.tokenSecret || undefined)
    } else if (doc.hasIn(['source'])) {
      doc.deleteIn(['source'])
    }
    spec.stages.forEach((st, si) =>
      st.jobs.forEach((j, ji) => {
        if (j.deploy && info.hosts[j.name]) setField(doc, ['stages', si, 'jobs', ji, 'deploy', 'hosts'], info.hosts[j.name])
      }),
    )
  })
}

const STEPS: [Step, string][] = [
  ['template', '选择模板'],
  ['info', '基本信息'],
  ['review', '确认编排'],
]

/** 新建流水线向导：选模板 → 填写名称、代码库、部署目标 → 在画布中确认后创建。 */
export function NewPipeline({ templateId }: { templateId?: string }) {
  const [templates, setTemplates] = useState<Template[]>()
  const [step, setStep] = useState<Step>('template')
  const [picked, setPicked] = useState<Template>()
  const [info, setInfo] = useState<Info>(initialInfo())
  const [yaml, setYaml] = useState('')
  const [editorKey, setEditorKey] = useState(0)
  const [error, setError] = useState<string>()
  const [saving, setSaving] = useState(false)
  const dirty = useRef(false)
  dirty.current = step !== 'template' && !saving

  useEffect(() => setLeaveGuard(() => dirty.current), [])
  useEffect(() => {
    api.listTemplates('pipeline').then(
      (ts) => {
        const all = [blankTemplate, ...ts]
        setTemplates(all)
        const t = all.find((x) => x.id === templateId)
        if (t) choose(t)
      },
      (e: Error) => setError(e.message),
    )
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [templateId])

  const pickedSpec = useMemo(() => (picked ? parse(picked.yaml).spec : undefined), [picked])

  function choose(t: Template) {
    const spec = parse(t.yaml).spec
    setPicked(t)
    setInfo((cur) => ({ ...initialInfo(spec), name: cur.name, group: cur.group, url: cur.url || '', tokenSecret: cur.tokenSecret || spec?.source?.token_secret || '' }))
    setStep('info')
    setError(undefined)
  }

  function toReview() {
    if (!picked || !pickedSpec) return
    try {
      setYaml(applyInfo(picked.yaml, pickedSpec, info))
      setEditorKey((k) => k + 1)
      setStep('review')
      setError(undefined)
    } catch (e) {
      setError((e as Error).message)
    }
  }

  async function create() {
    setSaving(true)
    setError(undefined)
    try {
      const p = await api.createPipeline(yaml)
      dirty.current = false
      if (info.group.trim()) await api.setPipelineGroup(p.id, info.group.trim()).catch(() => {})
      if (info.pushTrigger && info.useSource) {
        await api.createTrigger(p.id, 'webhook', true, { events: ['push'], branches: info.branch.trim() ? [info.branch.trim()] : [] })
        navigate(href.edit(p.id, 'triggers'))
      } else {
        navigate(href.pipeline(p.id))
      }
    } catch (e) {
      setError((e as Error).message)
      setSaving(false)
    }
  }

  const stepIndex = STEPS.findIndex(([s]) => s === step)
  return (
    <section>
      <nav className="crumbs">
        <a href={href.list}>流水线</a> / 新建
      </nav>
      <div className="page-head">
        <h1>新建流水线</h1>
        <ol className="stepper">
          {STEPS.map(([s, label], i) => (
            <li key={s} className={i === stepIndex ? 'active' : i < stepIndex ? 'done' : ''}>
              <button disabled={i > stepIndex} onClick={() => i < stepIndex && setStep(s)}>
                <span className="stepper-no">{i < stepIndex ? '✓' : i + 1}</span>
                {label}
              </button>
            </li>
          ))}
        </ol>
      </div>
      {error && <div className="alert">{error}</div>}
      {step === 'template' && <TemplateGallery templates={templates} onPick={choose} />}
      {step === 'info' && picked && pickedSpec && (
        <InfoForm template={picked} spec={pickedSpec} info={info} onChange={setInfo} onBack={() => setStep('template')} onNext={toReview} />
      )}
      {step === 'review' && (
        <>
          <p className="muted">
            这是根据模板和你填写的信息生成的流程，可以直接创建，也可以先在画布上调整（添加任务、修改脚本、拖动顺序）。创建后随时可以继续修改。
          </p>
          <PipelineEditor
            key={editorKey}
            value={yaml}
            onChange={setYaml}
            actions={
              <>
                <button className="btn" onClick={() => setStep('info')}>
                  上一步
                </button>
                <button className="btn primary" disabled={saving} onClick={create}>
                  {saving ? '创建中…' : '创建流水线'}
                </button>
              </>
            }
          />
        </>
      )}
    </section>
  )
}

// ---- 第一步：模板库 ----

function TemplateGallery({ templates, onPick }: { templates?: Template[]; onPick: (t: Template) => void }) {
  const [category, setCategory] = useState('全部')
  const [q, setQ] = useState('')
  const specs = useMemo(() => new Map(templates?.map((t) => [t.id, parse(t.yaml).spec])), [templates])
  const categoryOf = (t: Template) => (t.builtin ? t.category || '通用' : '团队模板')
  const categories = useMemo(() => {
    const cats = new Set<string>()
    for (const t of templates ?? []) cats.add(categoryOf(t))
    const order = ['Java', 'Go', 'Node.js', 'Python', '容器', '通用', '团队模板']
    return ['全部', '部署到主机', ...order.filter((c) => cats.has(c)), ...[...cats].filter((c) => !order.includes(c))]
  }, [templates])
  if (!templates) return <p className="muted">加载模板…</p>
  const query = q.trim().toLowerCase()
  const shown = templates.filter((t) => {
    if (query) return `${t.name} ${t.description} ${t.category ?? ''}`.toLowerCase().includes(query)
    if (category === '全部') return true
    if (category === '部署到主机') return specTags(specs.get(t.id)).includes('部署到主机')
    return categoryOf(t) === category
  })
  return (
    <div className="gallery">
      <nav className="picker-cats">
        {categories.map((c) => (
          <button key={c} className={c === category && !query ? 'active' : ''} onClick={() => (setCategory(c), setQ(''))}>
            {c}
          </button>
        ))}
        <a className="btn link small" href={href.templates}>
          管理模板 →
        </a>
      </nav>
      <div>
        <input className="picker-search" placeholder="搜索模板，如 java、部署、docker" value={q} onChange={(e) => setQ(e.target.value)} />
        <div className="gallery-grid">
          {shown.map((t) => {
            const spec = specs.get(t.id)
            return (
              <button key={t.id} className={`gallery-card${t.id === BLANK_ID ? ' blank' : ''}`} onClick={() => onPick(t)}>
                <div className="gallery-title">
                  <strong>{t.name}</strong>
                  {!t.builtin && <span className="tag">团队</span>}
                </div>
                <span className="muted small">{t.description}</span>
                <FlowPreview spec={spec} />
                <div className="gallery-tags">
                  {t.builtin && t.category && t.id !== BLANK_ID && <span className="tag">{t.category}</span>}
                  {specTags(spec).map((x) => (
                    <span key={x} className={`tag${x === '部署到主机' ? ' accent' : ''}`}>
                      {x}
                    </span>
                  ))}
                </div>
              </button>
            )
          })}
          {shown.length === 0 && <p className="muted">没有匹配的模板。</p>}
        </div>
      </div>
    </div>
  )
}

// ---- 第二步：基本信息 ----

function InfoForm({
  template,
  spec,
  info,
  onChange,
  onBack,
  onNext,
}: {
  template: Template
  spec: PipelineSpec
  info: Info
  onChange: (i: Info) => void
  onBack: () => void
  onNext: () => void
}) {
  const [secrets, setSecrets] = useState<Secret[]>([])
  const [groups, setGroups] = useState<HostGroup[]>()
  const [envs, setEnvs] = useState<Environment[]>([])
  const [listGroups, setListGroups] = useState<string[]>([])
  const set = <K extends keyof Info>(k: K, v: Info[K]) => onChange({ ...info, [k]: v })
  const deployJobs = Object.keys(info.hosts)

  const loadHosts = () => {
    api.listHostGroups().then(setGroups, () => setGroups([]))
    api.listEnvironments().then(setEnvs, () => {})
  }
  useEffect(() => {
    api.listSecrets().then(setSecrets, () => {})
    api.pipelineSummary(1).then((rows) => setListGroups([...new Set(rows.map((r) => r.group).filter((g): g is string => !!g))]), () => {})
    loadHosts()
  }, [])

  // 部署任务按名称猜测默认主机组：名称含 test / prod 时选对应环境下的第一个组。
  useEffect(() => {
    if (!groups?.length) return
    const next = { ...info.hosts }
    let changed = false
    for (const job of deployJobs) {
      if (next[job]) continue
      const envHint = /test|qa|测试/i.test(job) ? /测试|test/i : /prod|生产/i.test(job) ? /生产|prod/i : undefined
      const env = envHint && envs.find((e) => envHint.test(e.name))
      const g = (env && groups.find((x) => x.environment_id === env.id)) ?? (deployJobs.length === 1 ? groups[0] : undefined)
      if (g) {
        next[job] = g.name
        changed = true
      }
    }
    if (changed) set('hosts', next)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [groups, envs])

  const missing: string[] = []
  if (!info.name.trim()) missing.push('流水线名称')
  if (info.useSource && !info.url.trim()) missing.push('仓库地址')
  for (const j of deployJobs) if (!info.hosts[j]) missing.push(`「${j}」的主机组`)

  return (
    <div className="wizard-form">
      <div className="card wizard-template">
        <div className="muted small">模板</div>
        <strong>{template.name}</strong>
        <FlowPreview spec={spec} maxJobs={4} />
        <button className="btn link small" onClick={onBack}>
          更换模板
        </button>
      </div>

      <div className="card">
        <h2>基本信息</h2>
        <div className="form-grid">
          <label className="field">
            <span>流水线名称 *</span>
            <input autoFocus value={info.name} onChange={(e) => set('name', e.target.value)} placeholder={spec.name} />
          </label>
          <label className="field">
            <span>分组（可选）</span>
            <input list="pipeline-groups" value={info.group} onChange={(e) => set('group', e.target.value)} placeholder="如：后端服务" />
            <datalist id="pipeline-groups">
              {listGroups.map((g) => (
                <option key={g} value={g} />
              ))}
            </datalist>
          </label>
        </div>
      </div>

      <div className="card">
        <h2>代码源</h2>
        <label className="check">
          <input type="checkbox" checked={info.useSource} onChange={(e) => set('useSource', e.target.checked)} /> 运行时从 Git 仓库拉取代码
        </label>
        {info.useSource && (
          <div className="form-grid">
            <label className="field span2">
              <span>仓库地址 *（HTTPS）</span>
              <input className="mono" value={info.url} onChange={(e) => set('url', e.target.value)} placeholder="https://github.com/team/app.git" />
            </label>
            <label className="field">
              <span>默认分支</span>
              <input className="mono" value={info.branch} onChange={(e) => set('branch', e.target.value)} />
            </label>
            <label className="field">
              <span>访问令牌（私有仓库）</span>
              <select value={info.tokenSecret} onChange={(e) => set('tokenSecret', e.target.value)}>
                <option value="">无需令牌（公开仓库）</option>
                {info.tokenSecret && !secrets.some((s) => s.name === info.tokenSecret) && <option value={info.tokenSecret}>{info.tokenSecret}（未创建）</option>}
                {secrets.map((s) => (
                  <option key={s.id} value={s.name}>
                    密钥 {s.name}
                  </option>
                ))}
              </select>
              <small className="muted">
                令牌保存在 <a href={href.secrets} target="_blank" rel="noreferrer">密钥</a> 中，这里选择密钥名
              </small>
            </label>
            <label className="check span2">
              <input type="checkbox" checked={info.pushTrigger} onChange={(e) => set('pushTrigger', e.target.checked)} /> 推送代码到默认分支时自动运行（创建后显示要配置到 Git 平台的 Webhook 地址）
            </label>
          </div>
        )}
      </div>

      {deployJobs.length > 0 && (
        <div className="card">
          <div className="row">
            <h2>部署目标</h2>
            <div className="spacer" />
            <a className="btn link small" href={href.hosts()} target="_blank" rel="noreferrer">
              管理环境与主机 ↗
            </a>
            <button className="btn small" onClick={loadHosts}>
              刷新
            </button>
          </div>
          <p className="muted small">模板中的部署任务会把产物上传到所选主机组的每台服务器并执行脚本。</p>
          {groups && groups.length === 0 && (
            <div className="alert subtle">
              还没有主机组。请先在 <a href={href.hosts('hosts')} target="_blank" rel="noreferrer">环境与主机</a> 中添加服务器并建立主机组，然后点「刷新」。
            </div>
          )}
          <div className="form-grid">
            {deployJobs.map((job) => (
              <label key={job} className="field">
                <span>
                  任务 <span className="mono">{job}</span> 部署到 *
                </span>
                <select value={info.hosts[job]} onChange={(e) => set('hosts', { ...info.hosts, [job]: e.target.value })}>
                  <option value="">请选择主机组</option>
                  {groups?.map((g) => {
                    const env = envs.find((e) => e.id === g.environment_id)
                    return (
                      <option key={g.id} value={g.name}>
                        {env ? `${env.name} / ` : ''}
                        {g.name}
                        {env?.require_approval ? '（需审批）' : ''}
                      </option>
                    )
                  })}
                </select>
              </label>
            ))}
          </div>
        </div>
      )}

      <div className="row wizard-actions">
        <button className="btn" onClick={onBack}>
          上一步
        </button>
        <div className="spacer" />
        {missing.length > 0 && <span className="muted small">还需填写：{missing.join('、')}</span>}
        <button className="btn primary" disabled={missing.length > 0} onClick={onNext}>
          下一步：确认编排
        </button>
      </div>
    </div>
  )
}
