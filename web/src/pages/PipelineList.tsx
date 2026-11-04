import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { parse as parseYaml } from 'yaml'
import { api } from '../api'
import { useSession } from '../auth'
import { Icon, type IconName } from '../components/Icon'
import { Modal } from '../components/Modal'
import { RunButton } from '../components/RunDialog'
import { StatusBadge } from '../components/StatusBadge'
import { fmtDuration, fmtShort, fmtTime, triggerLabel } from '../format'
import { usePolling } from '../hooks'
import { href, navigate } from '../router'
import { hasPerm, isTerminal, type Pipeline, type PipelineSummary, type Run } from '../types'

const TREND = 10
const UNGROUPED = ''

// 左侧视图：全部 / 我创建的 / 收藏 / 最近失败，或某个分组（group: 前缀，空名为未分组）。
type View = 'all' | 'mine' | 'public' | 'favorite' | 'failed' | `group:${string}`

// 个人的列表偏好（视图、折叠的分组）只存在本浏览器。
const PREFS_KEY = 'flow:pipeline-list'
type Prefs = { view: View; collapsed: string[] }
function loadPrefs(): Prefs {
  const d: Prefs = { view: 'all', collapsed: [] }
  try {
    return { ...d, ...JSON.parse(localStorage.getItem(PREFS_KEY) ?? '{}') }
  } catch {
    return d
  }
}
function savePrefs(p: Prefs) {
  try {
    localStorage.setItem(PREFS_KEY, JSON.stringify(p))
  } catch {
    // 隐私模式等不可用时忽略
  }
}

const latestOf = (p: PipelineSummary): Run | undefined => p.recent_runs[0]

/** 配置里的代码仓库，显示为 owner/repo。 */
function repoOf(yaml: string) {
  try {
    const url = (parseYaml(yaml) as { source?: { url?: string } })?.source?.url
    if (typeof url !== 'string' || !url) return undefined
    return { url, short: url.replace(/^[a-z]+:\/\/[^/]+\//i, '').replace(/^git@[^:]+:/, '').replace(/\.git$/, '') }
  } catch {
    return undefined
  }
}

const viewLabels: Record<string, string> = { all: '全部流水线', mine: '我的流水线', public: '公开流水线', favorite: '我的收藏', failed: '最近失败' }

export function PipelineList({ trash }: { trash?: boolean }) {
  const { data, error, refresh } = usePolling(() => api.pipelineSummary(TREND), [], (rows) =>
    rows?.some((p) => p.recent_runs.some((r) => !isTerminal(r.status))) ? 2000 : 10000,
  )
  const { me, can } = useSession()
  const canEdit = can('developer')
  const [prefs, setPrefs] = useState(loadPrefs)
  const [query, setQuery] = useState('')
  const [favs, setFavs] = useState<Record<string, boolean>>({}) // 乐观更新，刷新后以服务端为准
  const [grouping, setGrouping] = useState<PipelineSummary>()
  const [actionError, setActionError] = useState<string>()
  const search = useRef<HTMLInputElement>(null)

  useEffect(() => savePrefs(prefs), [prefs])
  useEffect(() => setFavs({}), [data])
  // 按 / 聚焦搜索框。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement
      if (e.key === '/' && !['INPUT', 'TEXTAREA', 'SELECT'].includes(el.tagName) && !el.isContentEditable) {
        e.preventDefault()
        search.current?.focus()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  const rows = useMemo(() => data?.map((p) => (p.id in favs ? { ...p, favorite: favs[p.id] } : p)), [data, favs])
  const user = me.user.username
  const counts = useMemo(
    () => ({
      all: rows?.length ?? 0,
      mine: rows?.filter((p) => p.created_by === user).length ?? 0,
      public: rows?.filter((p) => p.is_public).length ?? 0,
      favorite: rows?.filter((p) => p.favorite).length ?? 0,
      failed: rows?.filter((p) => latestOf(p)?.status === 'failed').length ?? 0,
    }),
    [rows, user],
  )
  const { data: savedGroups, refresh: refreshGroups } = usePolling(() => api.listGroups(), [], () => null)
  const groups = useMemo(
    () => [...new Set([...(savedGroups ?? []), ...(rows?.map((p) => p.group ?? UNGROUPED) ?? [])].filter(Boolean))].sort((a, b) => a.localeCompare(b, 'zh-CN')),
    [rows, savedGroups],
  )
  async function groupAction(fn: () => Promise<unknown>) {
    try {
      await fn()
      refreshGroups()
      refresh()
    } catch (e) {
      setActionError((e as Error).message)
    }
  }
  const newGroup = () => {
    const name = prompt('新分组名称')?.trim()
    if (name) groupAction(() => api.createGroup(name))
  }
  const renameGroup = (g: string) => {
    const name = prompt('重命名分组', g)?.trim()
    if (name && name !== g) groupAction(() => api.renameGroup(g, name)).then(() => view === `group:${g}` && setView(`group:${name}`))
  }
  const deleteGroup = (g: string) => {
    if (confirm(`删除分组「${g}」？其中的流水线变为未分组，不会被删除。`)) groupAction(() => api.deleteGroup(g))
  }
  async function copy(p: PipelineSummary) {
    const name = prompt('新流水线名称', `${p.name} 副本`)?.trim()
    if (!name) return
    try {
      const np = await api.copyPipeline(p.id, name)
      navigate(href.edit(np.id))
    } catch (e) {
      setActionError((e as Error).message)
    }
  }
  const groupCount = (g: string) => rows?.filter((p) => (p.group ?? UNGROUPED) === g).length ?? 0
  // 分组被清空后回到全部
  const view: View = prefs.view.startsWith('group:') && !groups.includes(prefs.view.slice(6)) && prefs.view !== 'group:' && rows && savedGroups ? 'all' : prefs.view
  const setView = (v: View) => {
    setPrefs((p) => ({ ...p, view: v }))
    if (trash) navigate(href.list)
  }

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase()
    return (rows ?? [])
      .filter((p) =>
        view === 'mine'
          ? p.created_by === user
          : view === 'public'
            ? p.is_public
            : view === 'favorite'
            ? p.favorite
            : view === 'failed'
              ? latestOf(p)?.status === 'failed'
              : view.startsWith('group:')
                ? (p.group ?? UNGROUPED) === view.slice(6)
                : true,
      )
      .filter((p) => !q || p.name.toLowerCase().includes(q) || (p.group ?? '').toLowerCase().includes(q) || (repoOf(p.yaml)?.short.toLowerCase().includes(q) ?? false))
      .sort((a, b) => Number(b.favorite) - Number(a.favorite)) // 收藏的排在前面（稳定排序保持创建顺序）
  }, [rows, view, query, user])

  const sections = useMemo(() => {
    if (groups.length === 0 || view.startsWith('group:')) return [{ name: undefined, rows: visible }]
    const by = new Map<string, PipelineSummary[]>()
    for (const p of visible) by.set(p.group ?? UNGROUPED, [...(by.get(p.group ?? UNGROUPED) ?? []), p])
    return [...groups, UNGROUPED].filter((g) => by.has(g)).map((g) => ({ name: g, rows: by.get(g)! }))
  }, [visible, groups, view])

  async function toggleFavorite(p: PipelineSummary) {
    setFavs((f) => ({ ...f, [p.id]: !p.favorite }))
    try {
      await api.setFavorite(p.id, !p.favorite)
      refresh()
    } catch (e) {
      setFavs((f) => ({ ...f, [p.id]: p.favorite }))
      setActionError((e as Error).message)
    }
  }

  async function moveToTrash(p: PipelineSummary) {
    if (!confirm(`把「${p.name}」移到回收站？\n运行记录与产物保留，触发器暂停；可在回收站恢复。`)) return
    try {
      await api.deletePipeline(p.id)
      refresh()
    } catch (e) {
      setActionError((e as Error).message)
    }
  }

  const toggleSection = (name: string) =>
    setPrefs((p) => ({ ...p, collapsed: p.collapsed.includes(name) ? p.collapsed.filter((x) => x !== name) : [...p.collapsed, name] }))
  const searching = query.trim() !== ''
  const title = view.startsWith('group:') ? view.slice(6) || '未分组' : viewLabels[view]

  const side = (
    <nav className="side-nav list-nav">
      {(
        [
          ['mine', '我的流水线', 'user'],
          ['all', '全部流水线', 'layers'],
          ['public', '公开流水线', 'users'],
          ['favorite', '我的收藏', 'star'],
          ['failed', '最近失败', 'alert'],
        ] as [View, string, IconName][]
      ).map(([v, label, icon]) => (
        <button key={v} className={!trash && view === v ? 'active' : ''} onClick={() => setView(v)}>
          <Icon name={icon} />
          {label}
          <span className="count">{counts[v as keyof typeof counts]}</span>
        </button>
      ))}
      <div className="side-title">
        <Icon name="folder" /> 分组
        {canEdit && (
          <button className="icon-btn side-add" title="新建分组" onClick={newGroup}>
            <Icon name="plus" size={14} />
          </button>
        )}
      </div>
      {groups.map((g) => (
        <div key={g} className={`side-group${!trash && view === `group:${g}` ? ' active' : ''}`}>
          <button className="sub" onClick={() => setView(`group:${g}`)}>
            <span className="ellipsis">{g}</span>
            <span className="count">{groupCount(g)}</span>
          </button>
          {canEdit && (
            <span className="side-group-actions">
              <button className="icon-btn" title="重命名" onClick={() => renameGroup(g)}>
                <Icon name="edit" size={12} />
              </button>
              <button className="icon-btn" title="删除分组" onClick={() => deleteGroup(g)}>
                <Icon name="trash" size={12} />
              </button>
            </span>
          )}
        </div>
      ))}
      {groupCount(UNGROUPED) > 0 && (
        <button className={!trash && view === 'group:' ? 'active sub' : 'sub'} onClick={() => setView('group:')}>
          未分组
          <span className="count">{groupCount(UNGROUPED)}</span>
        </button>
      )}
      <div className="spacer" />
      <a href={href.trash} className={`side-foot${trash ? ' active' : ''}`}>
        <Icon name="trash" />
        回收站
      </a>
    </nav>
  )

  return (
    <div className="side-layout">
      {side}
      <div className="side-main">
        {trash ? (
          <Trash onChanged={refresh} />
        ) : (
          <section>
            <div className="page-head">
              <h1>{title}</h1>
              <span className="muted">{visible.length} 条</span>
              <div className="spacer" />
              <label className="search-box">
                <Icon name="search" />
                <input
                  ref={search}
                  type="search"
                  placeholder="搜索流水线名称 / 仓库"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  onKeyDown={(e) => e.key === 'Escape' && setQuery('')}
                />
              </label>
              {canEdit && (
                <a className="btn primary" href={href.newPipeline}>
                  <Icon name="plus" /> 新建流水线
                </a>
              )}
            </div>
            {error && <div className="alert">{error}</div>}
            {actionError && <div className="alert">{actionError}</div>}
            {!rows ? (
              !error && <p className="muted">加载中…</p>
            ) : rows.length === 0 ? (
              <div className="empty">
                <p>还没有流水线。</p>
                {canEdit && (
                  <a className="btn primary" href={href.newPipeline}>
                    新建第一条流水线
                  </a>
                )}
              </div>
            ) : visible.length === 0 ? (
              <div className="empty">
                <p className="muted">
                  {searching
                    ? `没有名称、分组或仓库包含「${query.trim()}」的流水线。`
                    : view === 'favorite'
                      ? '还没有收藏的流水线，点击行尾的 ☆ 收藏。'
                      : view === 'mine'
                        ? '还没有你创建的流水线。'
                        : '最近一次运行失败的流水线：无。'}
                </p>
              </div>
            ) : (
              <div className="table-wrap">
                <table className="pipeline-table">
                  <thead>
                    <tr>
                      <th>流水线名称</th>
                      <th>代码源</th>
                      <th>最近运行</th>
                      <th>最近 {TREND} 次</th>
                      <th>触发信息</th>
                      <th>最近运行开始时间</th>
                      <th />
                    </tr>
                  </thead>
                  {sections.map((sec) => {
                    const collapsed = sec.name !== undefined && !searching && prefs.collapsed.includes(sec.name)
                    return (
                      <tbody key={sec.name ?? '-'}>
                        {sec.name !== undefined && (
                          <tr className="group-row">
                            <td colSpan={7}>
                              <button onClick={() => toggleSection(sec.name!)} aria-expanded={!collapsed}>
                                <span className="caret">{collapsed ? '▸' : '▾'}</span>
                                {sec.name || '未分组'}
                                <span className="muted">（{sec.rows.length}）</span>
                                <GroupHealth rows={sec.rows} />
                              </button>
                            </td>
                          </tr>
                        )}
                        {!collapsed &&
                          sec.rows.map((p) => (
                            <PipelineRow
                              key={p.id}
                              p={p}
                              onFavorite={() => toggleFavorite(p)}
                              onGroup={() => setGrouping(p)}
                              onTrash={() => moveToTrash(p)}
                              onCopy={() => copy(p)}
                            />
                          ))}
                      </tbody>
                    )
                  })}
                </table>
              </div>
            )}
          </section>
        )}
      </div>
      {grouping && (
        <GroupDialog
          pipeline={grouping}
          groups={groups}
          onClose={() => setGrouping(undefined)}
          onSaved={() => {
            setGrouping(undefined)
            refresh()
          }}
        />
      )}
    </div>
  )
}

function PipelineRow({
  p,
  onFavorite,
  onGroup,
  onTrash,
  onCopy,
}: {
  p: PipelineSummary
  onFavorite: () => void
  onGroup: () => void
  onTrash: () => void
  onCopy: () => void
}) {
  const latest = latestOf(p)
  const repo = useMemo(() => repoOf(p.yaml), [p.yaml])
  return (
    <tr className={p.archived ? 'archived' : undefined}>
      <td className="name-col">
        <a href={href.pipeline(p.id)} className="strong">
          {p.name}
        </a>
        {p.archived && <span className="tag">已归档</span>}
        {p.description && <div className="muted small ellipsis" title={p.description}>{p.description}</div>}
      </td>
      <td className="repo-col">
        {repo ? (
          <span className="mono small" title={repo.url}>
            <Icon name="branch" size={13} /> {repo.short}
          </span>
        ) : (
          <span className="muted">—</span>
        )}
      </td>
      <td>
        {latest ? (
          <a href={href.run(latest.id)} className="run-link">
            <StatusBadge status={latest.status} /> <span className="muted">#{latest.number}</span>
          </a>
        ) : (
          <span className="muted">从未运行</span>
        )}
      </td>
      <td>
        <RunTrend runs={p.recent_runs} />
      </td>
      <td className="muted">{latest ? triggerLabel(latest.trigger, latest.env, latest.created_by) : '—'}</td>
      <td className="muted">{latest ? fmtShort(latest.started_at ?? latest.created_at) : '—'}</td>
      <td className="actions">
        <span className="row-actions">
          <button className={`icon-btn star${p.favorite ? ' on' : ''}`} title={p.favorite ? '取消收藏' : '收藏'} aria-pressed={p.favorite} onClick={onFavorite}>
            <Icon name="star" fill={p.favorite} />
          </button>
          {hasPerm(p, 'run') && <RunButton pipeline={p} icon />}
          {(hasPerm(p, 'edit') || hasPerm(p, 'delete')) && (
            <Menu>
              {hasPerm(p, 'edit') && <a href={href.edit(p.id)}>编辑</a>}
              {hasPerm(p, 'edit') && <button onClick={onGroup}>设置分组</button>}
              {hasPerm(p, 'edit') && <button onClick={onCopy}>复制流水线</button>}
              {hasPerm(p, 'delete') && (
                <button className="danger" onClick={onTrash}>
                  移到回收站
                </button>
              )}
            </Menu>
          )}
        </span>
      </td>
    </tr>
  )
}

/** 行尾的「⋯」菜单；点击外部或选择后关闭。 */
function Menu({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLSpanElement>(null)
  useEffect(() => {
    if (!open) return
    const close = (e: MouseEvent) => !ref.current?.contains(e.target as Node) && setOpen(false)
    document.addEventListener('mousedown', close)
    return () => document.removeEventListener('mousedown', close)
  }, [open])
  return (
    <span className="menu" ref={ref}>
      <button className="icon-btn" title="更多" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
        <Icon name="more" />
      </button>
      {open && (
        <div className="menu-pop" onClick={() => setOpen(false)}>
          {children}
        </div>
      )}
    </span>
  )
}

/** 回收站：恢复或彻底删除。 */
function Trash({ onChanged }: { onChanged: () => void }) {
  const { data, error, refresh } = usePolling(() => api.listTrash(), [], () => null)
  const [busy, setBusy] = useState<string>()
  const [actionError, setActionError] = useState<string>()
  async function act(p: Pipeline, purge: boolean) {
    if (purge && prompt(`彻底删除后运行记录、日志、产物、触发器与流水线密钥都会被清除，且无法恢复。\n请输入流水线名称 ${p.name} 确认：`) !== p.name) return
    setBusy(p.id)
    setActionError(undefined)
    try {
      await (purge ? api.deletePipeline(p.id, true) : api.recoverPipeline(p.id))
      await refresh()
      onChanged()
    } catch (e) {
      setActionError((e as Error).message)
    } finally {
      setBusy(undefined)
    }
  }
  return (
    <section>
      <div className="page-head">
        <h1>回收站</h1>
        <span className="muted">{data?.length ?? 0} 条</span>
      </div>
      <p className="muted small">移到回收站的流水线保留运行记录与产物，触发器不再触发；恢复后照常使用。</p>
      {(error || actionError) && <div className="alert">{error ?? actionError}</div>}
      {data && data.length === 0 && (
        <div className="empty">
          <p className="muted">回收站是空的。</p>
        </div>
      )}
      {data && data.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>流水线名称</th>
                <th>分组</th>
                <th>创建人</th>
                <th>删除时间</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {data.map((p) => (
                <tr key={p.id}>
                  <td>
                    <a href={href.pipeline(p.id)} className="strong">
                      {p.name}
                    </a>
                  </td>
                  <td className="muted">{p.group || '未分组'}</td>
                  <td className="muted">{p.created_by || '—'}</td>
                  <td className="muted">{fmtShort(p.deleted_at)}</td>
                  <td className="actions">
                    {hasPerm(p, 'delete') && (
                      <>
                        <button className="btn small" disabled={busy === p.id} onClick={() => act(p, false)}>
                          <Icon name="restore" /> 恢复
                        </button>{' '}
                        <button className="btn small danger" disabled={busy === p.id} onClick={() => act(p, true)}>
                          彻底删除
                        </button>
                      </>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}

/** 最近几次运行：柱高为耗时，颜色为结果，旧的在左；右侧为成功率。 */
export function RunTrend({ runs }: { runs: Run[] }) {
  if (runs.length === 0) return <span className="muted small">—</span>
  const ordered = [...runs].reverse()
  const secs = ordered.map((r) => (r.started_at ? ((r.finished_at ? Date.parse(r.finished_at) : Date.now()) - Date.parse(r.started_at)) / 1000 : 0))
  const max = Math.max(...secs, 1)
  const done = runs.filter((r) => r.status === 'success' || r.status === 'failed')
  const ok = done.filter((r) => r.status === 'success').length
  const rate = done.length ? Math.round((ok / done.length) * 100) : undefined
  return (
    <span className="trend">
      <span className="trend-bars" aria-label={`最近 ${runs.length} 次运行`}>
        {Array.from({ length: TREND - ordered.length }, (_, i) => (
          <span key={`e${i}`} className="trend-bar slot" />
        ))}
        {ordered.map((r, i) => (
          <a
            key={r.id}
            href={href.run(r.id)}
            className={`trend-bar status-${r.status}${isTerminal(r.status) ? '' : ' live'}`}
            style={{ height: `${30 + (70 * secs[i]) / max}%` }}
            title={`#${r.number} · ${statusText[r.status] ?? r.status} · ${fmtDuration(r.started_at, r.finished_at)} · ${fmtTime(r.created_at)}`}
          />
        ))}
      </span>
      {rate !== undefined && (
        <span className={`trend-rate ${rate >= 80 ? 'ok-text' : rate < 50 ? 'error-text' : ''}`} title={`最近 ${done.length} 次已结束的运行中成功 ${ok} 次`}>
          {rate}%
        </span>
      )}
    </span>
  )
}

const statusText: Record<string, string> = {
  pending: '等待中',
  queued: '排队中',
  running: '运行中',
  waiting: '待审批',
  success: '成功',
  failed: '失败',
  skipped: '跳过',
  canceled: '已取消',
}

/** 分组标题上的汇总：最近一次运行失败的流水线数。 */
function GroupHealth({ rows }: { rows: PipelineSummary[] }) {
  const failed = rows.filter((p) => latestOf(p)?.status === 'failed').length
  const running = rows.filter((p) => latestOf(p) && !isTerminal(latestOf(p)!.status)).length
  return (
    <span className="group-health small">
      {failed > 0 && <span className="error-text">{failed} 条失败</span>}
      {running > 0 && <span className="muted">{running} 条运行中</span>}
    </span>
  )
}

function GroupDialog({ pipeline, groups, onClose, onSaved }: { pipeline: PipelineSummary; groups: string[]; onClose: () => void; onSaved: () => void }) {
  const [value, setValue] = useState(pipeline.group ?? '')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  async function save(group: string) {
    setBusy(true)
    setError(undefined)
    try {
      await api.setPipelineGroup(pipeline.id, group)
      onSaved()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }
  return (
    <Modal
      title={`设置分组：${pipeline.name}`}
      onClose={onClose}
      footer={
        <>
          {pipeline.group && (
            <button className="btn" disabled={busy} onClick={() => save('')}>
              移出分组
            </button>
          )}
          <div className="spacer" />
          <button className="btn" onClick={onClose}>
            取消
          </button>
          <button className="btn primary" disabled={busy || value.trim() === (pipeline.group ?? '')} onClick={() => save(value)}>
            保存
          </button>
        </>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault()
          if (value.trim() !== (pipeline.group ?? '')) save(value)
        }}
      >
        <label className="field">
          <span>分组名称</span>
          <input autoFocus maxLength={40} value={value} placeholder="如：后端服务、前端、基础设施" onChange={(e) => setValue(e.target.value)} />
        </label>
      </form>
      {groups.length > 0 && (
        <div className="group-choices">
          {groups.map((g) => (
            <button key={g} className={`chip-btn${g === value.trim() ? ' active' : ''}`} onClick={() => setValue(g)}>
              {g}
            </button>
          ))}
        </div>
      )}
      <p className="muted small">分组只影响列表展示，不会生成新的配置版本。</p>
      {error && <div className="alert">{error}</div>}
    </Modal>
  )
}
