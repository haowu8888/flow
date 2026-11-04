import { createContext, useContext, useState } from 'react'
import { api } from '../api'
import { fmtTime, formatEnv, parseEnv, splitList } from '../format'
import { usePolling } from '../hooks'
import { CopyButton } from './CopyButton'
import type { Trigger, TriggerConfig, TriggerKind, WebhookEvent } from '../types'

const kindLabel: Record<TriggerKind, string> = { webhook: 'Git Webhook', cron: '定时', generic: '流水线 Webhook' }

const cronPresets = [
  { label: '每小时', expr: '0 * * * *' },
  { label: '每天 02:00', expr: '0 2 * * *' },
  { label: '工作日 09:00', expr: '0 9 * * 1-5' },
  { label: '每周一 08:00', expr: '0 8 * * 1' },
]

const browserTz = Intl.DateTimeFormat().resolvedOptions().timeZone

const webhookEvents: [WebhookEvent, string][] = [
  ['push', '推送（分支 / 标签）'],
  ['pull_request', '合并请求（PR / MR）'],
]

export const webhookURL = (id: string) => `${window.location.origin}/api/hooks/${id}`

/** 流水线详情页的触发器管理：Git Webhook 与 cron 定时触发。 */
/** 编辑页按流水线权限传入是否可修改。 */
export const TriggerEditContext = createContext(false)

export function TriggerSection({ pipelineId }: { pipelineId: string }) {
  // cron 的下次触发时间与最近触发结果会变化，低频刷新。
  const { data: triggers, error, refresh } = usePolling(() => api.listTriggers(pipelineId), [pipelineId], () => 15000)
  const [adding, setAdding] = useState(false)
  const canEdit = useContext(TriggerEditContext)

  return (
    <>
      <div className="section-head">
        <h2>触发器</h2>
        {!adding && canEdit && (
          <button className="btn link" onClick={() => setAdding(true)}>
            + 添加触发器
          </button>
        )}
      </div>
      {error && <div className="alert">{error}</div>}
      {adding && (
        <TriggerForm
          pipelineId={pipelineId}
          onDone={() => {
            setAdding(false)
            refresh()
          }}
        />
      )}
      {triggers && triggers.filter((t) => t.kind !== 'generic').length === 0 && !adding && (
        <p className="muted">没有自动触发规则，只能手动运行。可添加 Git 推送触发或定时触发。</p>
      )}
      <div className="trigger-list">
        {triggers?.filter((t) => t.kind !== 'generic').map((t) => <TriggerCard key={t.id} trigger={t} onChange={refresh} />)}
      </div>
    </>
  )
}

function TriggerCard({ trigger: t, onChange }: { trigger: Trigger; onChange: () => void }) {
  const [editing, setEditing] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const canEdit = useContext(TriggerEditContext)

  async function act(fn: () => Promise<unknown>) {
    setBusy(true)
    setError(undefined)
    try {
      await fn()
      onChange()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  if (editing) {
    return (
      <TriggerForm
        pipelineId={t.pipeline_id}
        trigger={t}
        onDone={() => {
          setEditing(false)
          onChange()
        }}
      />
    )
  }

  const toggle = () => act(() => api.updateTrigger(t.id, !t.enabled, configOf(t)))
  const remove = () => {
    if (confirm(`确定删除这个${kindLabel[t.kind]}触发器？${t.kind === 'webhook' ? 'Git 平台上配置的回调地址将失效。' : ''}`)) {
      act(() => api.deleteTrigger(t.id))
    }
  }

  return (
    <div className={`card trigger-card${t.enabled ? '' : ' disabled'}`}>
      <div className="row">
        <span className="strong">{kindLabel[t.kind]}</span>
        <span className={`badge ${t.enabled ? 'status-success' : 'status-skipped'}`}>{t.enabled ? '已启用' : '已停用'}</span>
        <div className="spacer" />
        {canEdit && (
          <>
            <button className="btn" disabled={busy} onClick={toggle}>
              {t.enabled ? '停用' : '启用'}
            </button>
            <button className="btn" disabled={busy} onClick={() => setEditing(true)}>
              编辑
            </button>
            <button className="btn danger" disabled={busy} onClick={remove}>
              删除
            </button>
          </>
        )}
      </div>

      <dl className="kv">
        {t.kind === 'webhook' ? (
          <>
            <dt>回调地址</dt>
            <dd>
              <code>{webhookURL(t.id)}</code> <CopyButton text={webhookURL(t.id)} />
            </dd>
            <dt>密钥</dt>
            <dd>
              {t.secret ? <Secret value={t.secret} /> : <span className="muted">仅开发者及以上可见</span>}
              {canEdit && (
              <button
                className="btn link"
                disabled={busy}
                onClick={() => confirm('重新生成后旧密钥立即失效，需同步更新 Git 平台配置。继续？') && act(() => api.rotateTriggerSecret(t.id))}
              >
                重新生成
              </button>
              )}
            </dd>
            <dt>触发条件</dt>
            <dd>{describeRefs(t)}</dd>
          </>
        ) : (
          <>
            <dt>表达式</dt>
            <dd>
              <code>{t.schedule}</code>
              <span className="muted"> · {t.timezone || 'Server 本地时区'}</span>
            </dd>
            <dt>下次触发</dt>
            <dd>{t.enabled ? fmtTime(t.next_run_at) : '-'}</dd>
          </>
        )}
        {t.env && Object.keys(t.env).length > 0 && (
          <>
            <dt>注入变量</dt>
            <dd className="mono">{formatEnv(t.env).split('\n').join('  ')}</dd>
          </>
        )}
        <dt>最近触发</dt>
        <dd>
          {fmtTime(t.last_fired_at)}
          {t.last_error && <div className="error-text">{t.last_error}</div>}
        </dd>
      </dl>
      {t.kind === 'webhook' && (
        <details className="hint">
          <summary>如何在 Git 平台配置</summary>
          <ul>
            <li>
              <b>GitHub / Gitea / Gogs</b>：Payload URL 填回调地址，Content type 选 <code>application/json</code>，Secret 填密钥，事件选
              Push / Pull requests。
            </li>
            <li>
              <b>GitLab</b>：URL 填回调地址，Secret token 填密钥，勾选 Push events / Tag push events / Merge request events。
            </li>
            <li>
              <b>Gitee</b>：URL 填回调地址，WebHook 密码 / 签名密钥填密钥，勾选 Push / Tag Push / Pull Request。
            </li>
          </ul>
          <p className="muted">
            运行中可使用 <code>FLOW_GIT_URL</code>、<code>FLOW_GIT_COMMIT</code>、<code>FLOW_GIT_BRANCH</code> / <code>FLOW_GIT_TAG</code> 等变量；
            合并请求运行另有 <code>FLOW_GIT_PR_NUMBER</code>、<code>FLOW_GIT_PR_SOURCE_BRANCH</code>、<code>FLOW_GIT_PR_TARGET_BRANCH</code>（不设置{' '}
            <code>FLOW_GIT_BRANCH</code>）。平台重试同一投递不会重复运行。
          </p>
        </details>
      )}
      {error && <div className="alert subtle">{error}</div>}
    </div>
  )
}

const eventsOf = (t?: Trigger): WebhookEvent[] => (t?.events?.length ? t.events : ['push'])

function describeRefs(t: Trigger) {
  const branches = t.branches ?? []
  const tags = t.tags ?? []
  const events = eventsOf(t)
  const parts = []
  if (events.includes('push')) {
    if (branches.length === 0 && tags.length === 0) parts.push('推送到任意分支')
    if (branches.length > 0) parts.push(`推送到分支 ${branches.join('、')}`)
    if (tags.length > 0) parts.push(`推送标签 ${tags.join('、')}`)
    const filters = [t.paths?.length && `改动了 ${t.paths.join('、')}`, t.paths_ignore?.length && `不只是改动 ${t.paths_ignore.join('、')}`].filter(Boolean)
    if (filters.length && parts.length) parts[parts.length - 1] += `（且${filters.join('，且')}）`
  }
  if (events.includes('pull_request')) {
    parts.push(
      (branches.length > 0 ? `目标分支为 ${branches.join('、')} 的合并请求` : '任意合并请求') +
        `（新建、重新打开、有新提交${t.allow_forks ? '，含 fork' : '，不含 fork'}）`,
    )
  }
  return parts.join('，或')
}

function configOf(t: Trigger): TriggerConfig {
  return {
    events: t.events,
    allow_forks: t.allow_forks,
    branches: t.branches,
    tags: t.tags,
    paths: t.paths,
    paths_ignore: t.paths_ignore,
    schedule: t.schedule,
    timezone: t.timezone,
    env: t.env,
  }
}

function TriggerForm({ pipelineId, trigger, onDone }: { pipelineId: string; trigger?: Trigger; onDone: () => void }) {
  const [kind, setKind] = useState<TriggerKind>(trigger?.kind ?? 'webhook')
  const [enabled, setEnabled] = useState(trigger?.enabled ?? true)
  const [events, setEvents] = useState<WebhookEvent[]>(eventsOf(trigger))
  const [allowForks, setAllowForks] = useState(trigger?.allow_forks ?? false)
  const [branches, setBranches] = useState((trigger?.branches ?? []).join(', '))
  const [tags, setTags] = useState((trigger?.tags ?? []).join(', '))
  const [paths, setPaths] = useState((trigger?.paths ?? []).join(', '))
  const [pathsIgnore, setPathsIgnore] = useState((trigger?.paths_ignore ?? []).join(', '))
  const [schedule, setSchedule] = useState(trigger?.schedule ?? '0 2 * * *')
  const [timezone, setTimezone] = useState(trigger ? (trigger.timezone ?? '') : browserTz)
  const [envText, setEnvText] = useState(formatEnv(trigger?.env))
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string>()

  async function save() {
    setSaving(true)
    setError(undefined)
    try {
      const env = parseEnv(envText)
      const cfg: TriggerConfig =
        kind === 'webhook'
          ? {
              events,
              allow_forks: events.includes('pull_request') && allowForks,
              branches: splitList(branches),
              tags: events.includes('push') ? splitList(tags) : [],
              paths: events.includes('push') ? splitList(paths) : [],
              paths_ignore: events.includes('push') ? splitList(pathsIgnore) : [],
              env,
            }
          : { schedule, timezone, env }
      if (trigger) await api.updateTrigger(trigger.id, enabled, cfg)
      else await api.createTrigger(pipelineId, kind, enabled, cfg)
      onDone()
    } catch (e) {
      setError((e as Error).message)
      setSaving(false)
    }
  }

  return (
    <div className="card trigger-form">
      {!trigger && (
        <div className="segmented">
          {(['webhook', 'cron'] as const).map((k) => (
            <button key={k} className={kind === k ? 'active' : ''} onClick={() => setKind(k)}>
              {kindLabel[k]}
            </button>
          ))}
        </div>
      )}
      {trigger && <div className="strong">编辑{kindLabel[kind]}触发器</div>}

      {kind === 'webhook' ? (
        <>
          <div className="field">
            <span>事件</span>
            <div className="row">
              {webhookEvents.map(([ev, label]) => (
                <label key={ev} className="check">
                  <input
                    type="checkbox"
                    checked={events.includes(ev)}
                    onChange={(e) => {
                      const next = e.target.checked ? [...events, ev] : events.filter((x) => x !== ev)
                      if (next.length > 0) setEvents(next)
                    }}
                  />{' '}
                  {label}
                </label>
              ))}
            </div>
          </div>
          <label className="field">
            <span>{events.includes('pull_request') ? (events.includes('push') ? '分支（推送分支 / 合并请求的目标分支）' : '目标分支') : '分支'}</span>
            <input
              value={branches}
              onChange={(e) => setBranches(e.target.value)}
              placeholder={events.includes('push') ? 'main, release/*（留空且标签也为空时匹配所有分支）' : 'main（留空匹配所有目标分支）'}
            />
          </label>
          {events.includes('push') && (
            <label className="field">
              <span>标签</span>
              <input value={tags} onChange={(e) => setTags(e.target.value)} placeholder="v*（留空则推送标签不触发）" />
            </label>
          )}
          {events.includes('push') && (
            <div className="form-grid">
              <label className="field">
                <span>仅当这些路径有改动时触发</span>
                <input className="mono" value={paths} onChange={(e) => setPaths(e.target.value)} placeholder="services/api/, go.mod（留空不限）" />
              </label>
              <label className="field">
                <span>只改动这些路径时不触发</span>
                <input className="mono" value={pathsIgnore} onChange={(e) => setPathsIgnore(e.target.value)} placeholder="docs/, **/*.md" />
              </label>
            </div>
          )}
          <p className="muted small">
            逗号分隔；<code>*</code> 匹配一级（不含 /），<code>**</code> 匹配任意层级，以 <code>/</code> 结尾表示整个目录。路径过滤只作用于推送事件（一次推送超过 20 个提交时不过滤）。提交说明含{' '}
            <code>[skip ci]</code> 时不触发。保存后会生成回调地址和密钥。
          </p>
          {events.includes('pull_request') && (
            <>
              <label className="check">
                <input type="checkbox" checked={allowForks} onChange={(e) => setAllowForks(e.target.checked)} /> 运行来自 fork 仓库的合并请求
              </label>
              {allowForks && (
                <div className="alert subtle">fork 中的代码会拿到本流水线可用的全部密钥，只在信任所有可提交合并请求的人时开启。</div>
              )}
            </>
          )}
        </>
      ) : (
        <>
          <label className="field">
            <span>cron 表达式</span>
            <input className="mono" value={schedule} onChange={(e) => setSchedule(e.target.value)} placeholder="分 时 日 月 周" />
          </label>
          <div className="row small">
            {cronPresets.map((p) => (
              <button key={p.expr} className="btn link" onClick={() => setSchedule(p.expr)}>
                {p.label}
              </button>
            ))}
          </div>
          <label className="field">
            <span>时区</span>
            <input value={timezone} onChange={(e) => setTimezone(e.target.value)} placeholder="Asia/Shanghai（留空为 Server 本地时区）" />
          </label>
        </>
      )}
      <label className="field">
        <span>注入变量</span>
        <textarea className="env-editor" spellCheck={false} value={envText} onChange={(e) => setEnvText(e.target.value)} placeholder="每行一个 KEY=VALUE，可选" />
      </label>
      <label className="check">
        <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} /> 启用
      </label>
      <div className="row">
        <button className="btn primary" disabled={saving} onClick={save}>
          {saving ? '保存中…' : '保存'}
        </button>
        <button className="btn" disabled={saving} onClick={onDone}>
          取消
        </button>
        {error && <span className="error-text">{error}</span>}
      </div>
    </div>
  )
}

function Secret({ value }: { value: string }) {
  const [shown, setShown] = useState(false)
  return (
    <>
      <code>{shown ? value : '•'.repeat(16)}</code>{' '}
      <button className="btn link" onClick={() => setShown(!shown)}>
        {shown ? '隐藏' : '显示'}
      </button>
      <CopyButton text={value} />
    </>
  )
}

/** 流水线 Webhook：外部系统 POST 即可触发，请求体传入运行参数与开启了运行时设置的变量。 */
export function GenericWebhook({ pipelineId }: { pipelineId: string }) {
  const { data: triggers, refresh } = usePolling(() => api.listTriggers(pipelineId), [pipelineId], () => null)
  const canEdit = useContext(TriggerEditContext)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const t = triggers?.find((x) => x.kind === 'generic')
  const on = !!t?.enabled
  async function act(fn: () => Promise<unknown>) {
    setBusy(true)
    setError(undefined)
    try {
      await fn()
      await refresh()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  const toggle = (enabled: boolean) => act(() => (t ? api.updateTrigger(t.id, enabled, { env: t.env }) : api.createTrigger(pipelineId, 'generic', true, {})))
  const url = `${window.location.origin}/api/open/flow/webhook/${pipelineId}?token=${t?.secret ?? '<令牌>'}`
  const example = `# 按变量默认值触发
curl -X POST -H 'Content-Type: application/json' -d '{}' '${url}'

# 传入参数 / 变量覆盖默认值（参数化构建的参数，以及开启了「运行时设置」的变量）
curl -X POST -H 'Content-Type: application/json' -d '{"BUILD_ENV":"prod"}' '${url}'

# 返回
# {"ok":true,"runId":"…","buildNumber":5,"vars":["BUILD_ENV"],"ignored":[]}`
  return (
    <>
      <label className="switch-row">
        <span>
          <span className="strong">流水线 Webhook</span>
          <span className="muted small"> {on ? '已开启' : '未开启'}</span>
        </span>
        <input type="checkbox" className="switch" checked={on} disabled={!canEdit || busy || !triggers} onChange={(e) => toggle(e.target.checked)} />
      </label>
      {on && (
        <>
          <div className="row">
            <span className="muted small">调用示例</span>
            <div className="spacer" />
            <CopyButton text={example} />
            {canEdit && (
              <button
                className="btn small"
                disabled={busy}
                onClick={() => confirm('重置令牌后旧地址立即失效，继续？') && act(() => api.rotateTriggerSecret(t!.id))}
              >
                重置令牌
              </button>
            )}
          </div>
          <pre className="code-block">{example}</pre>
        </>
      )}
      {error && <div className="alert">{error}</div>}
    </>
  )
}
