import { useMemo, useState } from 'react'
import { api } from '../api'
import { parse, type ParamSpec, type VarSpec } from '../editor/model'
import { parseEnv } from '../format'
import { href, navigate } from '../router'
import type { Pipeline } from '../types'
import { Icon } from './Icon'
import { Modal } from './Modal'

const BRANCH = 'FLOW_GIT_BRANCH'

function runForm(p: Pipeline) {
  const spec = parse(p.yaml).spec
  const params = spec?.params ?? []
  // 配置了代码源时允许临时指定分支（除非参数里已声明同名变量）。
  const branch = spec?.source?.url && !params.some((x) => x.name === BRANCH) ? (spec.source.branch ?? '') : undefined
  const vars = (spec?.vars ?? []).filter((v) => v.runtime)
  return { params, branch, vars }
}

/** 运行按钮：流水线声明了运行参数或代码源时先弹出表单，否则直接运行。 */
/** icon：只显示 ▷ 图标（列表行尾）；play：文字前加图标；compact：不显示「自定义变量」。 */
export function RunButton({
  pipeline,
  className = 'btn',
  label = '运行',
  compact,
  icon,
  play,
  before,
}: {
  pipeline: Pipeline
  className?: string
  label?: string
  compact?: boolean
  icon?: boolean
  play?: boolean
  /** 运行前的准备（如先保存），返回 false 时不运行 */
  before?: () => Promise<boolean>
}) {
  const form = useMemo(() => runForm(pipeline), [pipeline])
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const needsForm = form.params.length > 0 || form.branch !== undefined || form.vars.length > 0

  async function click() {
    if (before && !(await before())) return
    if (needsForm) setOpen(true)
    else quick()
  }

  async function quick() {
    setBusy(true)
    try {
      const { run } = await api.triggerRun(pipeline.id)
      navigate(href.run(run.id))
    } catch (e) {
      alert((e as Error).message)
      setBusy(false)
    }
  }

  return (
    <>
      {icon ? (
        <button className="icon-btn" disabled={busy} title={needsForm ? `${label}（填写参数）` : label} onClick={click}>
          <Icon name="play" />
        </button>
      ) : (
        <button className={className} disabled={busy} onClick={click}>
          {play && <Icon name="play" />}
          {busy ? '触发中…' : needsForm ? `${label}…` : label}
        </button>
      )}
      {!needsForm && !compact && !icon && (
        <button className="btn link" onClick={() => setOpen(true)} title="临时设置环境变量后运行">
          自定义变量
        </button>
      )}
      {open && <RunDialog pipeline={pipeline} {...form} onClose={() => setOpen(false)} />}
    </>
  )
}

function initial(params: ParamSpec[]) {
  return Object.fromEntries(params.map((p) => [p.name, p.default ?? (p.type === 'boolean' ? 'false' : '')]))
}

function RunDialog({ pipeline, params, branch, vars, onClose }: { pipeline: Pipeline; params: ParamSpec[]; branch?: string; vars: VarSpec[]; onClose: () => void }) {
  const [values, setValues] = useState(() => ({ ...Object.fromEntries(vars.map((v) => [v.name, v.value ?? ''])), ...initial(params) }))
  const [remark, setRemark] = useState('')
  const [ref, setRef] = useState(branch ?? '')
  const [extra, setExtra] = useState('')
  const [showExtra, setShowExtra] = useState(params.length === 0 && branch === undefined)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const set = (k: string, v: string) => setValues((x) => ({ ...x, [k]: v }))
  const missing = params.filter((p) => p.required && !values[p.name]).map((p) => p.name)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (missing.length) return setError(`请填写：${missing.join('、')}`)
    setBusy(true)
    setError(undefined)
    try {
      const env: Record<string, string> = { ...parseEnv(extra) }
      for (const [k, v] of Object.entries(values)) {
        const def = vars.find((x) => x.name === k)
        if (v !== '' && (!def || v !== (def.value ?? ''))) env[k] = v
      }
      if (branch !== undefined && ref.trim() && ref.trim() !== branch) env[BRANCH] = ref.trim()
      const { run } = await api.triggerRun(pipeline.id, Object.keys(env).length ? env : undefined, remark.trim() || undefined)
      navigate(href.run(run.id))
    } catch (err) {
      setError((err as Error).message)
      setBusy(false)
    }
  }

  return (
    <Modal
      title={`运行「${pipeline.name}」`}
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose}>
            取消
          </button>
          <button type="submit" form="run-form" className="btn primary" disabled={busy}>
            {busy ? '触发中…' : '运行'}
          </button>
        </>
      }
    >
      <form id="run-form" className="run-form" onSubmit={submit}>
        {branch !== undefined && (
          <label className="run-field">
            <span className="run-label">分支</span>
            <input className="mono" value={ref} placeholder="默认分支" onChange={(e) => setRef(e.target.value)} />
            <span className="muted small">留空使用默认分支{branch ? ` ${branch}` : ''}</span>
          </label>
        )}
        {params.map((p) => (
          <div key={p.name} className="run-field">
            <span className="run-label">
              <code>{p.name}</code>
              {p.required && <span className="error-text"> *</span>}
            </span>
            <ParamInput p={p} value={values[p.name] ?? ''} onChange={(v) => set(p.name, v)} />
            {p.description && <span className="muted small">{p.description}</span>}
          </div>
        ))}
        {vars.map((v) => (
          <label key={v.name} className="run-field">
            <span className="run-label">
              <code>{v.name}</code>
            </span>
            <input className="mono" value={values[v.name] ?? ''} onChange={(e) => set(v.name, e.target.value)} />
            {v.description && <span className="muted small">{v.description}</span>}
          </label>
        ))}
        <label className="run-field">
          <span className="run-label">运行备注</span>
          <input maxLength={200} value={remark} placeholder="选填，显示在运行历史中" onChange={(e) => setRemark(e.target.value)} />
        </label>
        {showExtra ? (
          <label className="run-field">
            <span className="run-label">其他变量</span>
            <textarea className="env-editor" spellCheck={false} placeholder={'每行一个，覆盖同名变量，例如：\nLOG_LEVEL=debug'} value={extra} onChange={(e) => setExtra(e.target.value)} />
          </label>
        ) : (
          <button type="button" className="btn link small" onClick={() => setShowExtra(true)}>
            + 其他变量
          </button>
        )}
        {error && <div className="alert">{error}</div>}
      </form>
    </Modal>
  )
}

function ParamInput({ p, value, onChange }: { p: ParamSpec; value: string; onChange: (v: string) => void }) {
  if (p.type === 'boolean') {
    return (
      <label className="check">
        <input type="checkbox" checked={value === 'true'} onChange={(e) => onChange(String(e.target.checked))} />
        {value === 'true' ? '是' : '否'}
      </label>
    )
  }
  if (p.type === 'choice') {
    const options = p.options ?? []
    // 选项少时平铺，一眼可见；多时用下拉。
    if (options.length <= 5 && options.every((o) => o.length <= 16)) {
      return (
        <div className="segmented">
          {options.map((o) => (
            <button type="button" key={o} className={value === o ? 'active' : ''} onClick={() => onChange(o)}>
              {o}
            </button>
          ))}
        </div>
      )
    }
    return (
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        {!p.default && <option value="">请选择</option>}
        {options.map((o) => (
          <option key={o} value={o}>
            {o}
          </option>
        ))}
      </select>
    )
  }
  return <input className="mono" value={value} onChange={(e) => onChange(e.target.value)} />
}
