import { useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../api'
import { Modal } from '../components/Modal'
import { diffLines, hunks } from '../diff'
import { fmtTime } from '../format'
import type { Pipeline, Template } from '../types'

type Compare = 'prev' | 'draft'

/** 历史列表中的一个版本。 */
export interface RevisionEntry {
  version: number
  created_by?: string
  created_at: string
  restored_from?: number
}

/** 某个版本的内容：YAML 做行级对比，meta（如名称、描述）变化单独列出。 */
export interface RevisionContent {
  yaml: string
  meta?: Record<string, string>
}

/** 有版本历史的对象（流水线、模板）。 */
export interface RevisionSource<T> {
  current: T
  version: number
  list: () => Promise<RevisionEntry[]>
  get: (version: number) => Promise<RevisionContent>
  restore: (version: number, baseVersion: number) => Promise<T>
  /** 回退确认框中说明生效范围。 */
  effect: string
}

export function pipelineSource(p: Pipeline): RevisionSource<Pipeline> {
  return {
    current: p,
    version: p.version,
    list: () => api.listRevisions(p.id),
    get: (v) => api.getRevision(p.id, v).then((r) => ({ yaml: r.yaml ?? '' })),
    restore: (v, base) => api.restoreRevision(p.id, v, base),
    effect: '并立即生效，之后的运行都使用它',
  }
}

export function templateSource(t: Template): RevisionSource<Template> {
  return {
    current: t,
    version: t.version,
    list: () => api.listTemplateRevisions(t.id),
    get: (v) => api.getTemplateRevision(t.id, v).then((r) => ({ yaml: r.yaml ?? '', meta: { 名称: r.name, 描述: r.description } })),
    restore: (v, base) => api.restoreTemplateRevision(t.id, v, base),
    effect: t.kind === 'steps' ? '；引用最新版本的流水线从下次运行起使用它，锁定了版本（@N）的引用不受影响' : '',
  }
}

/**
 * 版本历史：每次保存一个版本，可与上一版本或编辑器中的内容对比；
 * 可把任一版本载入编辑器继续修改，或直接回退（生成新版本并立即生效，原历史保留）。
 */
export function RevisionHistory<T>({
  title = '配置历史',
  source,
  draft,
  dirty,
  initial,
  onRestore,
  onRolledBack,
  onClose,
}: {
  title?: string
  source: RevisionSource<T>
  draft: RevisionContent
  /** 编辑器中有未保存的修改 */
  dirty: boolean
  initial?: { version: number; compare: Compare }
  onRestore?: (c: RevisionContent, version: number) => void
  /** 提供时可直接回退；参数为回退后的对象。 */
  onRolledBack?: (t: T, from: number) => void
  onClose: () => void
}) {
  const [revs, setRevs] = useState<RevisionEntry[]>()
  const [error, setError] = useState<string>()
  const current = source.version
  const [version, setVersion] = useState(initial?.version ?? current)
  const [compare, setCompare] = useState<Compare>(initial?.compare ?? 'prev')
  const [texts, setTexts] = useState<Record<number, RevisionContent>>({})
  const requested = useRef(new Set<number>())
  const src = useRef(source)
  src.current = source

  useEffect(() => {
    src.current.list().then(setRevs, (e: Error) => setError(e.message))
  }, [current])

  // 按需加载所选版本及其上一版本的内容。
  useEffect(() => {
    for (const v of compare === 'prev' && version > 1 ? [version, version - 1] : [version]) {
      if (requested.current.has(v)) continue
      requested.current.add(v)
      src.current.get(v).then(
        (r) => setTexts((t) => ({ ...t, [v]: r })),
        (e: Error) => setError(e.message),
      )
    }
  }, [version, compare])

  const selected = texts[version]
  const base = compare === 'prev' ? (version > 1 ? texts[version - 1] : { yaml: '' }) : selected
  const target = compare === 'prev' ? selected : draft
  const diff = useMemo(() => (base === undefined || target === undefined ? undefined : hunks(diffLines(base.yaml, target.yaml))), [base, target])
  const metaChanges = useMemo(() => {
    if (!base?.meta || !target?.meta) return []
    return Object.keys(target.meta).filter((k) => (base.meta![k] ?? '') !== (target.meta![k] ?? '')).map((k) => ({ key: k, from: base.meta![k] ?? '', to: target.meta![k] ?? '' }))
  }, [base, target])
  const stats = useMemo(() => {
    let add = 0
    let del = 0
    for (const h of diff ?? []) if ('lines' in h) for (const l of h.lines) l.type === 'add' ? add++ : l.type === 'del' && del++
    return { add, del }
  }, [diff])
  const rev = revs?.find((r) => r.version === version)
  const [rolling, setRolling] = useState(false)
  const same = (a?: RevisionContent, b?: RevisionContent) => !!a && !!b && a.yaml === b.yaml && JSON.stringify(a.meta ?? {}) === JSON.stringify(b.meta ?? {})

  async function rollback() {
    const lost = dirty ? '\n编辑器中未保存的修改将被丢弃。' : ''
    if (!confirm(`回退到 v${version}？\n将以 v${version} 的内容生成 v${current + 1}${source.effect}；历史版本不会删除。${lost}`)) return
    setRolling(true)
    setError(undefined)
    try {
      onRolledBack!(await source.restore(version, current), version)
      onClose()
    } catch (e) {
      setError((e as Error).message)
      setRolling(false)
    }
  }

  return (
    <Modal
      title={title}
      wide
      onClose={onClose}
      footer={
        onRestore && (
          <>
            <span className="muted small">载入后可继续修改，保存即生成新版本；回退则直接生效。历史都不会丢失。</span>
            <div className="spacer" />
            {onRolledBack && version !== current && (
              <button className="btn" disabled={rolling || selected === undefined} onClick={rollback}>
                {rolling ? '回退中…' : `回退到 v${version}`}
              </button>
            )}
            <button
              className="btn primary"
              disabled={selected === undefined || same(selected, draft)}
              onClick={() => {
                onRestore(selected!, version)
                onClose()
              }}
            >
              {same(selected, draft) ? '编辑器中已是此版本' : `载入 v${version} 到编辑器`}
            </button>
          </>
        )
      }
    >
      {error && <div className="alert">{error}</div>}
      <div className="history">
        <ol className="history-list">
          {!revs && <li className="muted">加载中…</li>}
          {revs?.length === 0 && <li className="muted small">没有历史版本</li>}
          {revs?.map((r) => (
            <li key={r.version}>
              <button className={r.version === version ? 'active' : ''} onClick={() => setVersion(r.version)}>
                <span className="history-ver">
                  v{r.version}
                  {r.version === current && <span className="tag">当前</span>}
                  {!!r.restored_from && <span className="tag subtle">回退自 v{r.restored_from}</span>}
                </span>
                <span className="muted small">
                  {r.created_by || '—'} · {fmtTime(r.created_at)}
                </span>
              </button>
            </li>
          ))}
          {revs && revs.length >= 100 && <li className="muted small">仅显示最近 100 个版本</li>}
        </ol>
        <div className="history-main">
          <div className="row">
            <div className="segmented">
              <button className={compare === 'prev' ? 'active' : ''} onClick={() => setCompare('prev')}>
                与上一版本对比
              </button>
              <button className={compare === 'draft' ? 'active' : ''} onClick={() => setCompare('draft')}>
                与编辑器内容对比
              </button>
            </div>
            <div className="spacer" />
            {diff && (
              <span className="small">
                <span className="ok-text">+{stats.add}</span> <span className="error-text">−{stats.del}</span>
              </span>
            )}
          </div>
          <p className="muted small">
            {compare === 'prev'
              ? version > 1
                ? `v${version - 1} → v${version}${rev ? `（${rev.created_by || '—'} ${rev.restored_from ? `回退到 v${rev.restored_from}` : '保存'}）` : ''}`
                : 'v1 是创建时的版本'
              : `v${version} → 编辑器中的内容（含未保存的修改）`}
          </p>
          {version > 1 || compare === 'draft'
            ? metaChanges.map((m) => (
                <p key={m.key} className="small history-meta">
                  {m.key}：<del>{m.from || '（空）'}</del> → <ins>{m.to || '（空）'}</ins>
                </p>
              ))
            : null}
          <DiffView diff={diff} />
        </div>
      </div>
    </Modal>
  )
}

function DiffView({ diff }: { diff?: ReturnType<typeof hunks> }) {
  if (!diff) return <p className="muted">加载中…</p>
  if (!diff.some((h) => 'lines' in h)) return <p className="muted">没有差异。</p>
  return (
    <div className="diff">
      {diff.map((h, i) =>
        'skipped' in h ? (
          <div key={i} className="diff-skip">
            ⋯ {h.skipped} 行未改动
          </div>
        ) : (
          h.lines.map((l, k) => (
            <div key={`${i}-${k}`} className={`diff-line ${l.type}`}>
              <span className="diff-no">{l.a ?? ''}</span>
              <span className="diff-no">{l.b ?? ''}</span>
              <span className="diff-sign">{l.type === 'add' ? '+' : l.type === 'del' ? '−' : ' '}</span>
              <span className="diff-text">{l.text || ' '}</span>
            </div>
          ))
        ),
      )}
    </div>
  )
}
