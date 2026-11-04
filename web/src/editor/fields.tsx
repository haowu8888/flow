import { useEffect, useRef, useState, type ReactNode } from 'react'
import type { Dict } from './model'

/** 失焦或回车时才提交的输入框，用于名称等改动会牵连引用的字段。 */
export function LazyInput({
  value,
  onCommit,
  className,
  placeholder,
  disabled,
}: {
  value: string
  onCommit: (v: string) => void
  className?: string
  placeholder?: string
  disabled?: boolean
}) {
  const [draft, setDraft] = useState(value)
  useEffect(() => setDraft(value), [value])
  const commit = () => draft !== value && onCommit(draft.trim())
  return (
    <input
      className={className}
      value={draft}
      placeholder={placeholder}
      disabled={disabled}
      onChange={(e) => setDraft(e.target.value)}
      onBlur={commit}
      onKeyDown={(e) => {
        if (e.key === 'Enter') commit()
        if (e.key === 'Escape') setDraft(value)
      }}
    />
  )
}

/** 标签式列表输入：回车或逗号添加，Backspace 删除最后一项。 */
export function TagInput({
  value,
  onChange,
  placeholder,
  disabled,
  mono,
}: {
  value: string[]
  onChange: (v: string[]) => void
  placeholder?: string
  disabled?: boolean
  mono?: boolean
}) {
  const [draft, setDraft] = useState('')
  const add = (raw: string) => {
    const items = raw
      .split(/[,，\s]+/)
      .map((s) => s.trim())
      .filter((s) => s && !value.includes(s))
    if (items.length) onChange([...value, ...items])
    setDraft('')
  }
  return (
    <div className={`tag-input${disabled ? ' disabled' : ''}${mono ? ' mono' : ''}`}>
      {value.map((v, i) => (
        <span key={`${v}-${i}`} className="chip">
          {v}
          {!disabled && (
            <button type="button" aria-label={`移除 ${v}`} onClick={() => onChange(value.filter((_, k) => k !== i))}>
              ×
            </button>
          )}
        </span>
      ))}
      {!disabled && (
        <input
          value={draft}
          placeholder={value.length ? '' : placeholder}
          onChange={(e) => (/[,，\s]$/.test(e.target.value) ? add(e.target.value) : setDraft(e.target.value))}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              add(draft)
            } else if (e.key === 'Backspace' && !draft && value.length) onChange(value.slice(0, -1))
          }}
          onBlur={() => draft && add(draft)}
        />
      )}
    </div>
  )
}

type Row = { k: string; v: string }
const toRows = (d?: Dict): Row[] => Object.entries(d ?? {}).map(([k, v]) => ({ k, v }))
const toDict = (rows: Row[]): Dict => Object.fromEntries(rows.filter((r) => r.k.trim()).map((r) => [r.k.trim(), r.v]))
const same = (a: Dict, b: Dict) => JSON.stringify(a) === JSON.stringify(b)

/** 键值对编辑；键为空的行只保留在本地，不写入 YAML。 */
export function KvEditor({
  value,
  onChange,
  disabled,
  keyPlaceholder = 'NAME',
  valuePlaceholder = '值',
  hints,
}: {
  value?: Dict
  onChange: (d: Dict) => void
  disabled?: boolean
  keyPlaceholder?: string
  valuePlaceholder?: string
  hints?: Record<string, string>
}) {
  const [rows, setRows] = useState<Row[]>(() => toRows(value))
  const committed = useRef(value ?? {})
  // 外部变化（撤销、改 YAML）时重新同步；自己提交的不重置，以免打断输入。
  useEffect(() => {
    if (!same(value ?? {}, committed.current)) {
      committed.current = value ?? {}
      setRows(toRows(value))
    }
  }, [value])

  const update = (next: Row[]) => {
    setRows(next)
    const d = toDict(next)
    if (!same(d, committed.current)) {
      committed.current = d
      onChange(d)
    }
  }

  return (
    <div className="kv-editor">
      {rows.map((r, i) => (
        <div className="kv-row" key={i}>
          <input
            className="mono"
            value={r.k}
            placeholder={keyPlaceholder}
            disabled={disabled}
            onChange={(e) => update(rows.map((x, k) => (k === i ? { ...x, k: e.target.value } : x)))}
          />
          <input
            value={r.v}
            placeholder={hints?.[r.k] ?? valuePlaceholder}
            title={hints?.[r.k]}
            disabled={disabled}
            onChange={(e) => update(rows.map((x, k) => (k === i ? { ...x, v: e.target.value } : x)))}
          />
          {!disabled && (
            <button type="button" className="icon-btn" aria-label="删除" onClick={() => update(rows.filter((_, k) => k !== i))}>
              ×
            </button>
          )}
        </div>
      ))}
      {!disabled && (
        <button type="button" className="btn link small" onClick={() => setRows([...rows, { k: '', v: '' }])}>
          + 添加
        </button>
      )}
    </div>
  )
}

/** block：内含多个输入（列表、键值对）时用 div，避免 label 把点击转给第一个输入。 */
export function Field({ label, hint, block, children }: { label: string; hint?: ReactNode; block?: boolean; children: ReactNode }) {
  const Tag = block ? 'div' : 'label'
  return (
    <Tag className="field">
      <span>{label}</span>
      {children}
      {hint && <small className="muted">{hint}</small>}
    </Tag>
  )
}

/** 可折叠的分组，有内容时默认展开。 */
export function Group({ title, open, children, extra }: { title: string; open?: boolean; children: ReactNode; extra?: ReactNode }) {
  return (
    <details className="panel-group" open={open}>
      <summary>
        {title}
        {extra && <span className="group-extra">{extra}</span>}
      </summary>
      <div className="panel-group-body">{children}</div>
    </details>
  )
}
