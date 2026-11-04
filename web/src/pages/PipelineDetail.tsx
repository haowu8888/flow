import { useEffect, useRef, useState } from 'react'
import { api, ApiError } from '../api'
import { Icon } from '../components/Icon'
import { RunButton } from '../components/RunDialog'
import { SecretSection } from '../components/SecretSection'
import { StatusBadge } from '../components/StatusBadge'
import { GenericWebhook, TriggerEditContext, TriggerSection } from '../components/TriggerSection'
import { PipelineMembers } from '../components/PipelineMembers'
import { PipelineStats } from './PipelineStats'
import { DeploymentsTab } from './Hosts'
import { RunDetail } from './RunDetail'
import { pipelineSource, RevisionHistory } from '../editor/History'
import { SettingsCard } from '../editor/Panels'
import { PipelineEditor, type EditorHandle } from '../editor/PipelineEditor'
import { fmtDuration, fmtShort, fmtTime, shortSha, triggerLabel } from '../format'
import { usePolling, useTicker } from '../hooks'
import { href, navigate, setLeaveGuard, type EditSection, type PipelineTab } from '../router'
import { hasPerm, isTerminal, type Environment, type Pipeline, type Run } from '../types'

const tabs: [PipelineTab, string][] = [
  ['latest', '最近运行'],
  ['runs', '运行历史'],
  ['deploys', '部署历史'],
  ['stats', '统计报表'],
]

const sections: [EditSection, string][] = [
  ['basic', '基本信息'],
  ['flow', '流程配置'],
  ['triggers', '触发设置'],
  ['vars', '变量和缓存'],
]

export function PipelineDetail({ id, tab, section }: { id: string; tab: PipelineTab; section: EditSection }) {
  const [pipeline, setPipeline] = useState<Pipeline>()
  const [loadError, setLoadError] = useState<string>()
  useEffect(() => {
    setPipeline(undefined)
    setLoadError(undefined)
    api.getPipeline(id).then(setPipeline, (e: Error) => setLoadError(e.message))
  }, [id])

  if (loadError) return <div className="alert">{loadError}</div>
  if (!pipeline) return <p className="muted">加载中…</p>

  if (tab === 'edit') return <EditTab pipeline={pipeline} section={section} onSaved={setPipeline} readOnly={!hasPerm(pipeline, 'edit') || !!pipeline.deleted_at} />

  return (
    <section>
      <div className="page-head detail-head">
        <a className="icon-btn back" href={href.list} title="返回流水线列表">
          <Icon name="back" />
        </a>
        <h1 className="ellipsis">{pipeline.name}</h1>
        {pipeline.archived && <span className="tag">已归档</span>}
        <nav className="pills">
          {tabs.map(([t, label]) => (
            <a key={t} href={href.pipeline(id, t)} className={tab === t ? 'active' : ''}>
              {label}
            </a>
          ))}
        </nav>
        <div className="spacer" />
        <a className="btn" href={href.edit(id)}>
          <Icon name="edit" /> 编辑
        </a>
        {hasPerm(pipeline, 'run') && !pipeline.deleted_at && <RunButton pipeline={pipeline} className="btn primary" compact play />}
      </div>
      {pipeline.deleted_at && <TrashNotice pipeline={pipeline} onRecovered={setPipeline} />}
      {pipeline.description && <p className="muted detail-desc">{pipeline.description}</p>}

      {tab === 'latest' && <LatestRun pipeline={pipeline} canRun={hasPerm(pipeline, 'run')} />}
      {tab === 'runs' && <RunsTab pipeline={pipeline} />}
      {tab === 'deploys' && <Deploys pipelineId={id} />}
      {tab === 'stats' && <PipelineStats pipelineId={id} />}
    </section>
  )
}

function TrashNotice({ pipeline, onRecovered }: { pipeline: Pipeline; onRecovered: (p: Pipeline) => void }) {
  const canEdit = hasPerm(pipeline, 'delete')
  return (
    <div className="card waiting-notice trash-notice">
      这条流水线在回收站中（{fmtShort(pipeline.deleted_at)} 移入），不能运行或修改，触发器已暂停。
      {canEdit && (
        <button className="btn small" onClick={() => api.recoverPipeline(pipeline.id).then(onRecovered, (e: Error) => alert(e.message))}>
          <Icon name="restore" /> 恢复
        </button>
      )}
    </div>
  )
}

/** 最近一次运行的详情；有新运行时自动切换过去。 */
function LatestRun({ pipeline, canRun }: { pipeline: Pipeline; canRun: boolean }) {
  const { data, error } = usePolling(() => api.listRuns(pipeline.id, 1), [pipeline.id], () => 5000)
  if (error) return <div className="alert">{error}</div>
  if (!data) return <p className="muted">加载中…</p>
  if (data.length === 0)
    return (
      <div className="empty">
        <p className="muted">还没有运行记录。</p>
        {canRun && !pipeline.deleted_at && <RunButton pipeline={pipeline} className="btn primary" compact play label="运行一次" />}
      </div>
    )
  return <RunDetail key={data[0].id} id={data[0].id} embedded />
}

function Deploys({ pipelineId }: { pipelineId: string }) {
  const [envs, setEnvs] = useState<Environment[]>([])
  useEffect(() => {
    api.listEnvironments().then(setEnvs, () => {})
  }, [])
  return <DeploymentsTab envs={envs} pipelineId={pipelineId} />
}

const RUN_PAGE = 30

const statusFilters: [string, string][] = [
  ['', '全部状态'],
  ['success', '成功'],
  ['failed', '失败'],
  ['canceled', '已取消'],
  ['running', '运行中'],
  ['waiting', '待审批'],
  ['queued', '排队中'],
]
const triggerFilters: [string, string][] = [
  ['', '全部触发方式'],
  ['manual', '手动'],
  ['webhook', 'Webhook'],
  ['cron', '定时'],
  ['api', 'API'],
  ['rollback', '回滚 / 重新部署'],
]

function RunsTab({ pipeline }: { pipeline: Pipeline }) {
  const id = pipeline.id
  const [status, setStatus] = useState('')
  const [trigger, setTrigger] = useState('')
  const [older, setOlder] = useState<Run[]>([]) // 「加载更多」取到的更早运行
  const [more, setMore] = useState<boolean>()
  const { data: runs, error: runsError, refresh } = usePolling(() => api.queryRuns(id, { status, trigger, limit: RUN_PAGE }), [id, status, trigger], (runs) =>
    runs?.some((r) => !isTerminal(r.status)) ? 2000 : 10000,
  )
  useEffect(() => {
    setOlder([])
    setMore(undefined)
  }, [id, status, trigger])
  const all = runs ? [...runs, ...older.filter((o) => !runs.some((r) => r.id === o.id))] : undefined
  const canLoadMore = more ?? (runs?.length === RUN_PAGE)
  async function loadMore() {
    if (!all?.length) return
    const page = await api.queryRuns(id, { status, trigger, before: all[all.length - 1].number, limit: RUN_PAGE })
    setOlder((o) => [...o, ...page])
    setMore(page.length === RUN_PAGE)
  }
  const anyActive = runs?.some((r) => !isTerminal(r.status)) ?? false
  useTicker(anyActive)
  const filtered = status !== '' || trigger !== ''
  // 运行部署到的环境（来自部署记录）
  const { data: deployments } = usePolling(() => api.listDeployments({ pipeline: id, limit: 200 }), [id], () => 30000)
  const envByRun = new Map<string, string[]>()
  for (const d of deployments ?? []) {
    const name = d.environment_name ?? d.group_name
    const list = envByRun.get(d.run_id) ?? []
    if (!list.includes(name)) envByRun.set(d.run_id, [...list, name])
  }
  const canDelete = hasPerm(pipeline, 'deleteRun')
  async function removeAll() {
    if (!confirm('删除所有运行记录？已结束的运行记录连同日志、产物全部删除，运行中的保留；不可恢复。')) return
    try {
      const { deleted } = await api.deleteFinishedRuns(id)
      alert(`已删除 ${deleted} 条运行记录`)
      setOlder([])
      refresh()
    } catch (e) {
      alert((e as Error).message)
    }
  }
  async function removeOne(r: Run) {
    if (!confirm(`删除运行记录 #${r.number}（含日志与产物）？`)) return
    try {
      await api.deleteRun(r.id)
      setOlder((o) => o.filter((x) => x.id !== r.id))
      refresh()
    } catch (e) {
      alert((e as Error).message)
    }
  }
  return (
    <>
      <div className="list-toolbar">
        <div className="spacer" />
        {canDelete && (
          <button className="btn danger" onClick={removeAll}>
            删除所有
          </button>
        )}
        <select value={status} onChange={(e) => setStatus(e.target.value)}>
          {statusFilters.map(([v, l]) => (
            <option key={v} value={v}>
              {l}
            </option>
          ))}
        </select>
        <select value={trigger} onChange={(e) => setTrigger(e.target.value)}>
          {triggerFilters.map(([v, l]) => (
            <option key={v} value={v}>
              {l}
            </option>
          ))}
        </select>
      </div>
      {runsError && <div className="alert">{runsError}</div>}
      {all && all.length === 0 && <p className="muted">{filtered ? '没有符合条件的运行。' : '还没有运行记录。'}</p>}
      {all && all.length > 0 && <RunTable runs={all} envByRun={envByRun} onDelete={canDelete ? removeOne : undefined} />}
      {canLoadMore && (
        <button className="btn load-more" onClick={loadMore}>
          加载更早的运行
        </button>
      )}
    </>
  )
}

/** 把当前 YAML 保存为团队的流水线模板。 */
function SaveAsTemplate({ pipeline }: { pipeline: Pipeline }) {
  const [state, setState] = useState<'idle' | 'busy' | 'done'>('idle')
  async function save() {
    const name = prompt('模板名称', pipeline.name)?.trim()
    if (!name) return
    setState('busy')
    try {
      await api.createTemplate('pipeline', name, `由流水线「${pipeline.name}」保存`, pipeline.yaml)
      setState('done')
    } catch (e) {
      alert((e as Error).message)
      setState('idle')
    }
  }
  if (state === 'done') {
    return (
      <a className="btn link" href={href.templates}>
        已保存为模板 →
      </a>
    )
  }
  return (
    <button className="btn" disabled={state === 'busy'} onClick={save}>
      另存为模板
    </button>
  )
}

function TrashButton({ pipeline }: { pipeline: Pipeline }) {
  const [busy, setBusy] = useState(false)
  async function remove() {
    if (!confirm(`把「${pipeline.name}」移到回收站？\n运行记录与产物保留，触发器暂停；可在回收站恢复或彻底删除。`)) return
    setBusy(true)
    try {
      await api.deletePipeline(pipeline.id)
      navigate(href.trash)
    } catch (e) {
      alert((e as Error).message)
      setBusy(false)
    }
  }
  return (
    <button className="btn danger" disabled={busy} onClick={remove}>
      <Icon name="trash" /> 移到回收站
    </button>
  )
}

/** 说明、分组、归档：列表元信息，单独保存、不产生配置版本。 */
function MetaCard({ pipeline, onSaved, readOnly }: { pipeline: Pipeline; onSaved: (p: Pipeline) => void; readOnly: boolean }) {
  const [description, setDescription] = useState(pipeline.description ?? '')
  const [archived, setArchived] = useState(!!pipeline.archived)
  const [group, setGroup] = useState(pipeline.group ?? '')
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<{ ok: boolean; text: string }>()
  const dirty = description.trim() !== (pipeline.description ?? '') || archived !== !!pipeline.archived || group.trim() !== (pipeline.group ?? '')
  async function save() {
    setBusy(true)
    setMsg(undefined)
    try {
      let p = pipeline
      if (group.trim() !== (pipeline.group ?? '')) p = await api.setPipelineGroup(pipeline.id, group)
      if (description.trim() !== (pipeline.description ?? '') || archived !== !!pipeline.archived) p = await api.setPipelineInfo(pipeline.id, { description, archived })
      onSaved(p)
      setMsg({ ok: true, text: '已保存' })
    } catch (e) {
      setMsg({ ok: false, text: (e as Error).message })
    } finally {
      setBusy(false)
    }
  }
  return (
    <SettingsCard title="列表信息" desc="不属于流程配置，保存后立即生效，不生成新版本">
      <fieldset className="ed-form" disabled={readOnly}>
        <label className="field">
          <span>ID</span>
          <input className="mono" value={pipeline.id} readOnly />
        </label>
        <label className="field">
          <span>分组</span>
          <input maxLength={40} value={group} placeholder="无" onChange={(e) => setGroup(e.target.value)} />
        </label>
        <label className="field">
          <span>说明</span>
          <textarea rows={2} maxLength={500} value={description} onChange={(e) => setDescription(e.target.value)} />
        </label>
        <label className="check">
          <input type="checkbox" checked={archived} onChange={(e) => setArchived(e.target.checked)} />
          归档：不再维护的流水线，在列表里浅黄色高亮；只是标记，照常可以运行和编辑
        </label>
        <div className="row">
          <button type="button" className="btn primary" disabled={!dirty || busy} onClick={save}>
            保存
          </button>
          {msg && <span className={msg.ok ? 'ok-text' : 'error-text'}>{msg.text}</span>}
          {pipeline.created_by && <span className="muted small">创建人 {pipeline.created_by} · {fmtShort(pipeline.created_at)}</span>}
        </div>
      </fieldset>
    </SettingsCard>
  )
}

function RunTable({ runs, envByRun, onDelete }: { runs: Run[]; envByRun: Map<string, string[]>; onDelete?: (r: Run) => void }) {
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>运行编号</th>
            <th>状态</th>
            <th>环境</th>
            <th>触发信息</th>
            <th>配置 / 代码</th>
            <th>开始时间</th>
            <th>持续时间</th>
            <th>备注</th>
            {onDelete && <th>操作</th>}
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => (
            <tr key={r.id} className="clickable" onClick={() => navigate(href.run(r.id))}>
              <td>
                <a href={href.run(r.id)} className="strong">
                  #{r.number}
                </a>
              </td>
              <td>
                <StatusBadge status={r.status} />
              </td>
              <td className="muted">{envByRun.get(r.id)?.join('、') || '—'}</td>
              <td className="muted">{triggerLabel(r.trigger, r.env, r.created_by)}</td>
              <td className="muted">
                {r.pipeline_version ? `v${r.pipeline_version}` : '—'}
                {r.commit && <span className="mono" title={r.commit}> · {shortSha(r.commit)}</span>}
              </td>
              <td className="muted">{fmtTime(r.started_at ?? r.created_at)}</td>
              <td className="muted">{fmtDuration(r.started_at, r.finished_at)}</td>
              <td className="muted ellipsis remark-cell" title={r.remark}>
                {r.remark || '—'}
              </td>
              {onDelete && (
                <td onClick={(e) => e.stopPropagation()}>
                  {isTerminal(r.status) && (
                    <button className="btn link small danger-link" onClick={() => onDelete(r)}>
                      删除
                    </button>
                  )}
                </td>
              )}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/** 可视化编辑；Ctrl+S 保存，有未保存修改时离开页面前确认；保存时检查期间是否被他人修改。 */
function EditTab({ pipeline, section, onSaved, readOnly }: { pipeline: Pipeline; section: EditSection; onSaved: (p: Pipeline) => void; readOnly: boolean }) {
  const [draft, setDraft] = useState(pipeline.yaml)
  const [editorKey, setEditorKey] = useState(0)
  const [saving, setSaving] = useState(false)
  const [msg, setMsg] = useState<{ ok: boolean; text: string }>()
  const [conflict, setConflict] = useState<Pipeline>() // 编辑期间他人保存的最新版本
  const [history, setHistory] = useState<{ version: number; compare: 'prev' | 'draft' }>()
  const editorRef = useRef<EditorHandle>(null)
  const dirty = draft !== pipeline.yaml
  const state = useRef({ dirty, save: () => {} })

  /** 返回是否已保存（无修改也算）。 */
  async function save(base = pipeline.version): Promise<boolean> {
    if (!dirty) return true
    if (saving) return false
    setSaving(true)
    setMsg(undefined)
    try {
      onSaved(await api.updatePipeline(pipeline.id, draft, base))
      setConflict(undefined)
      setMsg({ ok: true, text: '已保存，下次运行生效' })
      return true
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        try {
          const latest = await api.getPipeline(pipeline.id)
          if (latest.version !== pipeline.version) {
            setConflict(latest)
            return false
          }
        } catch {
          // 取不到最新版本时按普通错误提示
        }
      }
      setMsg({ ok: false, text: (e as Error).message })
      return false
    } finally {
      setSaving(false)
    }
  }
  state.current = { dirty, save: () => void save() }

  /** 放弃本地修改，载入他人保存的最新版本（可撤销回自己的修改）。 */
  function loadLatest(latest: Pipeline) {
    onSaved(latest)
    editorRef.current?.replace(latest.yaml)
    setConflict(undefined)
    setMsg(undefined)
  }

  /** 回退已在服务端生效：编辑器换成新版本，撤销历史清空。 */
  function rolledBack(p: Pipeline, from: number) {
    onSaved(p)
    setDraft(p.yaml)
    setEditorKey((k) => k + 1)
    setConflict(undefined)
    setMsg({ ok: true, text: p.version === pipeline.version ? `内容与 v${from} 相同，未生成新版本` : `已回退到 v${from}（生成 v${p.version}），下次运行生效` })
  }

  // 分区之间切换不算离开（草稿保留在本组件中）
  useEffect(() => setLeaveGuard(() => state.current.dirty, href.edit(pipeline.id)), [pipeline.id])
  useEffect(() => {
    if (readOnly) return
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
        e.preventDefault()
        state.current.save()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [readOnly])

  const versionInfo = (
    <button className="btn link small" title="查看历史版本与差异" onClick={() => setHistory({ version: pipeline.version, compare: dirty ? 'draft' : 'prev' })}>
      v{pipeline.version}
      {pipeline.updated_by ? ` · ${pipeline.updated_by}` : ''} · {fmtTime(pipeline.updated_at)}
    </button>
  )
  const status = readOnly ? (
    <>
      {versionInfo}
      <span className="muted small">{pipeline.deleted_at ? '在回收站中，恢复后才能修改' : '只读：需要开发者权限才能修改'}</span>
    </>
  ) : (
    <>
      {msg && <span className={`ed-save-msg ${msg.ok ? 'ok-text' : 'error-text'}`} title={msg.text}>{msg.text}</span>}
      {dirty && !msg && <span className="muted small">有未保存的修改</span>}
      {versionInfo}
      <button
        className="btn"
        disabled={!dirty || saving}
        onClick={() => {
          if (!confirm('放弃所有未保存的修改？')) return
          setDraft(pipeline.yaml)
          setEditorKey((k) => k + 1)
          setMsg(undefined)
        }}
      >
        还原
      </button>
    </>
  )

  const editor = (s: EditSection) => (
    <PipelineEditor
      key={`${editorKey}:${s}`}
      handle={editorRef}
      value={draft}
      onChange={(v) => {
        setDraft(v)
        setMsg(undefined)
      }}
      readOnly={readOnly}
      section={s}
      actions={s === 'flow' ? status : undefined}
    />
  )

  return (
    <section>
      <div className="page-head detail-head">
        <a className="icon-btn back" href={href.pipeline(pipeline.id)} title="返回流水线">
          <Icon name="back" />
        </a>
        <h1 className="ellipsis">{pipeline.name}</h1>
        <nav className="pills">
          {sections.map(([sec, label]) => (
            <a key={sec} href={href.edit(pipeline.id, sec)} className={section === sec ? 'active' : ''}>
              {label}
            </a>
          ))}
        </nav>
        <div className="spacer" />
        {!readOnly && (
          <>
            <button className="btn" disabled={!dirty || saving} onClick={() => save()} title="Ctrl+S">
              <Icon name="save" /> {saving ? '保存中…' : '保存'}
            </button>
            <RunButton pipeline={{ ...pipeline, yaml: draft }} className="btn primary" compact play label="保存并运行" before={() => save()} />
          </>
        )}
      </div>
      {section !== 'flow' && <div className="ed-statusbar">{status}</div>}
      {conflict && (
        <div className="alert conflict">
          <span>
            <strong>{conflict.updated_by || '其他成员'}</strong> 在你编辑期间保存了 v{conflict.version}，直接保存会覆盖对方的修改。
          </span>
          <div className="row">
            <button className="btn" onClick={() => setHistory({ version: conflict.version, compare: 'draft' })}>
              查看差异
            </button>
            <button className="btn" onClick={() => loadLatest(conflict)}>
              载入最新版本
            </button>
            <button
              className="btn danger"
              onClick={() => confirm(`用你的内容覆盖 v${conflict.version}？对方的修改仍保留在历史版本中。`) && save(conflict.version)}
            >
              仍然保存我的版本
            </button>
          </div>
        </div>
      )}
      {section === 'flow' && editor('flow')}
      {section === 'basic' && (
        <div className="settings-stack">
          <MetaCard pipeline={pipeline} onSaved={onSaved} readOnly={readOnly} />
          <PipelineMembers pipeline={pipeline} onChanged={onSaved} />
          {editor('basic')}
          {(!readOnly || hasPerm(pipeline, 'delete')) && !pipeline.deleted_at && (
            <SettingsCard title="更多操作">
              <div className="row">
                {!readOnly && <SaveAsTemplate pipeline={pipeline} />}
                {hasPerm(pipeline, 'delete') && <TrashButton pipeline={pipeline} />}
              </div>
              <p className="muted small">移到回收站后运行记录与产物保留；在回收站「彻底删除」才会清掉。</p>
            </SettingsCard>
          )}
        </div>
      )}
      {section === 'triggers' && (
        <div className="settings-stack">
          <TriggerEditContext.Provider value={!readOnly}>
            <SettingsCard title="Webhook 触发" desc="外部系统通过 Webhook 将环境参数传给流水线并触发运行">
              <GenericWebhook pipelineId={pipeline.id} />
            </SettingsCard>
            <SettingsCard title="代码触发与定时触发" desc="Git 推送 / 合并请求（GitHub、GitLab、Gitea、Gitee 的 Webhook）与按 Cron 表达式定时运行；每个触发器独立的地址与密钥">
              <TriggerSection pipelineId={pipeline.id} />
            </SettingsCard>
          </TriggerEditContext.Provider>
          {editor('triggers')}
        </div>
      )}
      {section === 'vars' && (
        <div className="settings-stack">
          {editor('vars')}
          <SettingsCard title="密钥" desc="仅本流水线可见，与「账户设置 → 通用变量」同名时优先；值只写不读，日志中自动脱敏">
            <SecretSection pipelineId={pipeline.id} canEdit={!readOnly} />
          </SettingsCard>
          <SettingsCard title="缓存" desc={<>在任务的「缓存」中声明 key 与目录（如 <code>~/.npm</code>、<code>/go/pkg/mod</code>），成功后保存在 Runner 本地，下次运行直接复用</>}>
            <span className="muted small">缓存按任务配置，在「流程配置」中点任务卡片编辑。</span>
          </SettingsCard>
        </div>
      )}
      {history && (
        <RevisionHistory
          source={pipelineSource(conflict ?? pipeline)}
          draft={{ yaml: draft }}
          dirty={dirty}
          initial={history}
          onRestore={readOnly ? undefined : (c) => editorRef.current?.replace(c.yaml)}
          onRolledBack={readOnly ? undefined : rolledBack}
          onClose={() => setHistory(undefined)}
        />
      )}
    </section>
  )
}
