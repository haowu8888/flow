import { useMemo, useState } from 'react'
import { Modal } from '../components/Modal'
import { href } from '../router'
import type { StepInput, StepKind } from '../types'
import { categoryLabel, useConnections, useStepCatalog } from './catalog'
import { useEditor } from './context'
import { LazyInput } from './fields'
import { jobPath, ops, type JobRef, type JobSpec, type StepSpec } from './model'

/** 添加步骤：按分类选择内置步骤、执行命令或团队步骤模板。 */
export function StepPicker({ onPick, onClose }: { onPick: (s: StepSpec) => void; onClose: () => void }) {
  const catalog = useStepCatalog()
  const { stepTemplates } = useEditor()
  const [cat, setCat] = useState('build')
  const [q, setQ] = useState('')
  const query = q.trim().toLowerCase()
  const cats = [...(catalog?.categories ?? []), ...(stepTemplates.length ? [{ value: 'team', label: '团队模板' }] : [])]
  const kinds = (catalog?.kinds ?? []).filter((k) => k.kind !== 'run-command').filter((k) => (query ? `${k.label} ${k.description} ${k.kind}`.toLowerCase().includes(query) : k.category === cat))

  function pick(k: StepKind) {
    const w = Object.fromEntries(k.inputs.filter((i) => i.required && !i.default).map((i) => [i.key, '']))
    onPick({ name: k.label, kind: k.kind, with: Object.keys(w).length ? w : undefined })
  }
  return (
    <Modal title="添加步骤" onClose={onClose} wide>
      <div className="picker">
        <nav className="picker-cats">
          {cats.map((c) => (
            <button key={c.value} className={c.value === cat && !query ? 'active' : ''} onClick={() => (setCat(c.value), setQ(''))}>
              {c.label}
            </button>
          ))}
        </nav>
        <div className="picker-main">
          <input className="picker-search" autoFocus placeholder="搜索步骤，如 node、docker、上传" value={q} onChange={(e) => setQ(e.target.value)} />
          {!catalog && <p className="muted">加载中…</p>}
          <div className="picker-grid">
            {((!query && cat === 'build') || (query && '执行命令 shell 脚本 run command'.includes(query))) && (
              <button className="picker-item" onClick={() => onPick({ name: '执行命令', run: 'echo hello' })}>
                <span className="picker-text">
                  <strong>执行命令（Shell）</strong>
                  <span className="muted small">直接写 Shell 命令，工作目录为代码目录</span>
                </span>
              </button>
            )}
            {cat === 'team' && !query
              ? stepTemplates.map((t) => (
                  <button key={t.id} className="picker-item" onClick={() => onPick({ name: t.name, uses: t.name })}>
                    <span className="picker-text">
                      <strong>{t.name}</strong>
                      <span className="muted small">{t.description || '团队步骤模板'}</span>
                    </span>
                  </button>
                ))
              : kinds.map((k) => (
                  <button key={k.kind} className="picker-item" onClick={() => pick(k)}>
                    <span className="picker-text">
                      <strong>{k.label}</strong>
                      <span className="muted small">{k.description}</span>
                    </span>
                  </button>
                ))}
          </div>
        </div>
      </div>
    </Modal>
  )
}

/** 内置步骤的输入表单：按步骤库的定义渲染，需要服务连接的显示连接下拉。 */
export function KindForm({ r, k, step }: { r: JobRef; k: number; step: StepSpec }) {
  const catalog = useStepCatalog()
  const { apply, readOnly } = useEditor()
  const [advanced, setAdvanced] = useState(false)
  const def = catalog?.kinds.find((x) => x.kind === step.kind)
  const p = [...jobPath(r), 'steps', k]
  const setInput = (key: string, v: string) => {
    const next = { ...(step.with ?? {}) }
    if (v === '') delete next[key]
    else next[key] = v
    apply(ops.set([...p, 'with'], Object.keys(next).length ? next : undefined), `step:${r.si}:${r.ji}:${k}:with:${key}`)
  }
  if (!catalog) return <p className="muted small">加载步骤库…</p>
  if (!def) return <div className="alert subtle">未知的步骤类型 {step.kind}</div>
  const basic = def.inputs.filter((i) => !i.advanced)
  const adv = def.inputs.filter((i) => i.advanced)
  return (
    <div className="kind-form">
      <div className="muted small">
        {categoryLabel(catalog, def.category)} · {def.label}
        {def.image && <> · 建议镜像 {def.image}</>}
      </div>
      <div className="muted small">{def.description}</div>
      {def.connection && (
        <label className="field">
          <span>{def.connection_optional ? '服务连接（选填）' : '服务连接 *'}</span>
          <ConnectionSelect type={def.connection} value={step.connection} disabled={readOnly} onChange={(v) => apply(ops.set([...p, 'connection'], v || undefined))} />
        </label>
      )}
      {basic.map((i) => (
        <InputField key={i.key} input={i} value={step.with?.[i.key]} onChange={(v) => setInput(i.key, v)} disabled={readOnly} />
      ))}
      {adv.length > 0 && (
        <button type="button" className="btn link small" onClick={() => setAdvanced((a) => !a)}>
          {advanced ? '收起高级选项' : `高级选项（${adv.length}）`}
        </button>
      )}
      {advanced && adv.map((i) => <InputField key={i.key} input={i} value={step.with?.[i.key]} onChange={(v) => setInput(i.key, v)} disabled={readOnly} />)}
      {def.outputs && def.outputs.length > 0 && <div className="muted small">输出：{def.outputs.join('、')}（后续步骤与下游任务可用）</div>}
    </div>
  )
}

function InputField({ input: i, value, onChange, disabled }: { input: StepInput; value?: string; onChange: (v: string) => void; disabled?: boolean }) {
  const v = value ?? ''
  if (i.widget === 'checkbox') {
    const on = (value ?? i.default ?? '') === 'true'
    return (
      <label className="check">
        <input type="checkbox" checked={on} disabled={disabled} onChange={(e) => onChange(e.target.checked === (i.default === 'true') ? '' : String(e.target.checked))} />
        {i.label}
      </label>
    )
  }
  return (
    <label className="field">
      <span>
        {i.label}
        {i.required && ' *'}
      </span>
      {i.widget === 'select' ? (
        <select value={value ?? i.default ?? ''} disabled={disabled} onChange={(e) => onChange(e.target.value === (i.default ?? '') ? '' : e.target.value)}>
          {i.options?.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
      ) : i.widget === 'textarea' || i.widget === 'code' ? (
        <LazyTextarea className={i.widget === 'code' ? 'ed-script' : 'mono'} value={v} placeholder={i.default || i.placeholder} disabled={disabled} onCommit={onChange} />
      ) : (
        <LazyInput className="mono" value={v} placeholder={i.default ? `默认 ${i.default}` : i.placeholder} disabled={disabled} onCommit={onChange} />
      )}
      {i.description && <span className="muted small">{i.description}</span>}
    </label>
  )
}

function LazyTextarea({ value, onCommit, className, placeholder, disabled }: { value: string; onCommit: (v: string) => void; className?: string; placeholder?: string; disabled?: boolean }) {
  const [draft, setDraft] = useState(value)
  const [prev, setPrev] = useState(value)
  if (value !== prev) {
    setPrev(value)
    setDraft(value)
  }
  const rows = Math.min(Math.max(draft.split('\n').length + 1, 3), 14)
  return (
    <textarea
      className={className}
      spellCheck={false}
      rows={rows}
      value={draft}
      placeholder={placeholder}
      disabled={disabled}
      onChange={(e) => setDraft(e.target.value)}
      onBlur={() => draft !== value && onCommit(draft)}
    />
  )
}

/** 服务连接下拉，按类型过滤；types 为多个可选类型（审批消息可用的连接）。 */
export function ConnectionSelect({ type, types, value, onChange, disabled }: { type?: string; types?: string[]; value?: string; onChange: (v: string) => void; disabled?: boolean }) {
  const conns = useConnections()
  const list = useMemo(() => (conns ?? []).filter((c) => (type ? c.type === type : types ? types.includes(c.type) : true)), [conns, type, types])
  return (
    <span className="conn-select">
      <select value={value ?? ''} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
        <option value="">选择服务连接</option>
        {value && !list.some((c) => c.name === value) && <option value={value}>{value}（不可用）</option>}
        {list.map((c) => (
          <option key={c.id} value={c.name}>
            {c.name}
          </option>
        ))}
      </select>
      <a className="small" href={href.connections} target="_blank" rel="noreferrer">
        管理
      </a>
    </span>
  )
}

const pluginLabels: Record<string, string> = {
  webhook: 'Webhook',
  dingtalk: '钉钉',
  feishu: '飞书',
  wecom: '企业微信',
  bark: 'Bark',
  telegram: 'Telegram',
  email: '邮件',
}
const pluginEvents: [string, string][] = [
  ['success', '成功后'],
  ['failure', '失败后'],
  ['cancel', '取消后'],
  ['always', '结束后'],
]

/** 任务插件：任务结束时按状态推送通知。 */
export function PluginsEditor({ r, job }: { r: JobRef; job: JobSpec }) {
  const catalog = useStepCatalog()
  const { apply, readOnly } = useEditor()
  const base = [...jobPath(r), 'plugins']
  const set = (i: number, f: string, v: unknown) => apply(ops.set([...base, i, f], v), `plugin:${r.si}:${r.ji}:${i}:${f}`)
  return (
    <>
      {job.plugins.length === 0 && <p className="muted small">步骤成功、失败等状态变化时推送通知（Webhook / 钉钉 / 飞书 / 企业微信 / Bark / Telegram / 邮件）</p>}
      {job.plugins.map((pl, i) => {
        const connType = catalog?.plugins[pl.type]
        return (
          <div key={i} className="ed-notify">
            <div className="row">
              <select value={pl.type} disabled={readOnly} onChange={(e) => set(i, 'type', e.target.value)}>
                {Object.keys(pluginLabels).map((t) => (
                  <option key={t} value={t}>
                    {pluginLabels[t]}
                  </option>
                ))}
              </select>
              <LazyInput value={pl.name ?? ''} placeholder="插件名称" disabled={readOnly} onCommit={(v) => set(i, 'name', v || undefined)} />
              {!readOnly && (
                <button type="button" className="icon-btn" title="删除插件" onClick={() => apply(ops.deletePlugin(r, i))}>
                  ×
                </button>
              )}
            </div>
            {connType ? (
              <ConnectionSelect type={connType} value={pl.connection} disabled={readOnly} onChange={(v) => set(i, 'connection', v || undefined)} />
            ) : (
              <>
                <LazyInput className="mono" value={pl.url ?? ''} placeholder="地址，可用 ${SECRET} 引用密钥" disabled={readOnly} onCommit={(v) => set(i, 'url', v)} />
                {pl.type !== 'webhook' && <LazyInput className="mono" value={pl.secret ?? ''} placeholder="加签密钥（可选），如 ${DING_SECRET}" disabled={readOnly} onCommit={(v) => set(i, 'secret', v || undefined)} />}
              </>
            )}
            {pl.type === 'email' && (
              <LazyInput value={(pl.to ?? []).join(', ')} placeholder="收件人，逗号分隔" disabled={readOnly} onCommit={(v) => set(i, 'to', v.split(/[,，\s]+/).filter(Boolean))} />
            )}
            <div className="row">
              <span className="muted small">运行状态</span>
              {pluginEvents.map(([v, l]) => {
                const on = pl.on ?? ['failure']
                return (
                  <label key={v} className="check">
                    <input type="checkbox" disabled={readOnly} checked={on.includes(v)} onChange={(e) => set(i, 'on', e.target.checked ? [...on, v] : on.filter((x) => x !== v))} />
                    {l}
                  </label>
                )
              })}
            </div>
            <LazyInput value={pl.title ?? ''} placeholder="标题（可选），如 ${pipeline} #${run} ${task} ${status}" disabled={readOnly} onCommit={(v) => set(i, 'title', v || undefined)} />
            <LazyInput value={pl.content ?? ''} placeholder="内容（可选），变量：pipeline task stage status branch commit trigger executor remark url" disabled={readOnly} onCommit={(v) => set(i, 'content', v || undefined)} />
          </div>
        )
      })}
      {!readOnly && (
        <button type="button" className="btn link small" onClick={() => apply(ops.addPlugin(r))}>
          + 添加插件
        </button>
      )}
    </>
  )
}

const messageTypes = ['email', 'bark', 'telegram', 'wecom-bot']

/** 人工卡点的审批消息：进入待审批时通过服务连接推送，消息带「通过 / 驳回」链接。 */
export function ApprovalNotifyEditor({ r, job }: { r: JobRef; job: JobSpec }) {
  const { apply, readOnly } = useEditor()
  const base = [...jobPath(r), 'notify']
  const list = job.notify
  const save = (next: { connection: string; to?: string[] }[]) => apply(ops.set(base, next.length ? next.map((n) => ({ connection: n.connection, ...(n.to?.length ? { to: n.to } : {}) })) : undefined))
  return (
    <div className="field">
      <span>审批消息</span>
      <span className="muted small">进入待审批时发送带「通过 / 驳回」链接的消息（邮件 / Bark / Telegram / 企业微信），点链接即可审批；需要 Server 配置 FLOW_PUBLIC_URL</span>
      {list.map((n, i) => (
        <div key={i} className="row">
          <ConnectionSelect types={messageTypes} value={n.connection} disabled={readOnly} onChange={(v) => save(list.map((x, j) => (j === i ? { ...x, connection: v } : x)))} />
          <LazyInput value={(n.to ?? []).join(', ')} placeholder="邮件收件人（邮件连接时填写）" disabled={readOnly} onCommit={(v) => save(list.map((x, j) => (j === i ? { ...x, to: v.split(/[,，\s]+/).filter(Boolean) } : x)))} />
          {!readOnly && (
            <button type="button" className="icon-btn" onClick={() => save(list.filter((_, j) => j !== i))}>
              ×
            </button>
          )}
        </div>
      ))}
      {!readOnly && (
        <button type="button" className="btn link small" onClick={() => save([...list, { connection: '' }])}>
          + 添加审批消息
        </button>
      )}
    </div>
  )
}
