import { useEffect, useMemo, useState } from 'react'
import { api, type ConnInput } from '../api'
import { useSession } from '../auth'
import { Modal } from '../components/Modal'
import { fmtShort } from '../format'
import { usePolling } from '../hooks'
import { SECRET_MASK, type ConnType, type Member, type ServiceConnection } from '../types'

const optionLabels: Record<string, string> = {
  generic: '通用',
  gitea: 'Gitea',
  gitee: '码云',
  github: 'GitHub',
  acr: '阿里云 ACR',
  dockerhub: 'Docker Hub',
  harbor: 'Harbor',
  ghcr: 'GitHub Packages',
  aliyun: '阿里云 OSS',
  minio: 'MinIO',
  r2: 'Cloudflare R2',
  s3: 'AWS S3',
  other: '其他',
}

/** 下拉字段默认取第一个选项（否则不选也不会提交）。 */
function defaults(ct?: ConnType) {
  return Object.fromEntries((ct?.fields ?? []).filter((f) => f.options).map((f) => [f.key, f.options![0]]))
}

const visibilityLabels = { private: '仅自己', public: '所有人', custom: '指定成员' }

/** 服务连接：集中维护 Git、镜像仓库、对象存储、消息推送等第三方凭据，流水线步骤按名称引用。 */
export function Connections() {
  const { me, can } = useSession()
  const { data, error, refresh } = usePolling(() => api.listConnections(), [], () => null)
  const [types, setTypes] = useState<ConnType[]>([])
  const [tab, setTab] = useState('')
  const [editing, setEditing] = useState<ServiceConnection | 'new'>()
  const [actionError, setActionError] = useState<string>()
  useEffect(() => {
    api.connectionTypes().then(setTypes, () => {})
  }, [])
  const label = (t: string) => types.find((x) => x.type === t)?.label ?? t
  const rows = (data ?? []).filter((c) => !tab || c.type === tab)
  const mine = (c: ServiceConnection) => can('admin') || c.created_by === me.user.username

  async function remove(c: ServiceConnection) {
    if (!confirm(`删除服务连接「${c.name}」？引用它的步骤会运行失败。`)) return
    setActionError(undefined)
    try {
      await api.deleteConnection(c.id)
      refresh()
    } catch (e) {
      setActionError((e as Error).message)
    }
  }

  return (
    <section>
      <div className="page-head">
        <h1>服务连接</h1>
        <div className="spacer" />
        {can('developer') && (
          <button className="btn primary" onClick={() => setEditing('new')}>
            新建服务连接
          </button>
        )}
      </div>
      <p className="muted">集中维护 Git、镜像仓库、对象存储、Bark 等第三方凭据，流水线步骤按名称选择；私密字段只在保存时可见，注入步骤时自动脱敏。</p>
      <div className="pills type-tabs">
        <button className={tab === '' ? 'active' : ''} onClick={() => setTab('')}>
          全部 <span className="count">{data?.length ?? 0}</span>
        </button>
        {types.map((t) => (
          <button key={t.type} className={tab === t.type ? 'active' : ''} onClick={() => setTab(t.type)}>
            {t.label} <span className="count">{data?.filter((c) => c.type === t.type).length ?? 0}</span>
          </button>
        ))}
      </div>
      {(error || actionError) && <div className="alert">{error ?? actionError}</div>}
      {data && rows.length === 0 ? (
        <div className="empty">
          <p className="strong">还没有服务连接</p>
          <p className="muted small">新建一个 Git 凭据或镜像仓库连接，流水线的拉取 / 推送步骤就能直接选用</p>
        </div>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>名称</th>
                <th>类型</th>
                <th>可见范围</th>
                <th>创建人</th>
                <th>更新时间</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((c) => (
                <tr key={c.id}>
                  <td>
                    <span className="strong">{c.name}</span>
                    {c.description && <div className="muted small">{c.description}</div>}
                  </td>
                  <td>
                    {label(c.type)}
                    {c.config.platform && <span className="muted small"> · {optionLabels[c.config.platform] ?? c.config.platform}</span>}
                  </td>
                  <td className="muted">{visibilityLabels[c.visibility]}</td>
                  <td className="muted">{c.created_by || '—'}</td>
                  <td className="muted">{fmtShort(c.updated_at)}</td>
                  <td className="actions">
                    {mine(c) && (
                      <>
                        <button className="btn small" onClick={() => setEditing(c)}>
                          编辑
                        </button>{' '}
                        <button className="btn small danger" onClick={() => remove(c)}>
                          删除
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
      {editing && types.length > 0 && (
        <ConnectionDialog
          conn={editing === 'new' ? undefined : editing}
          types={types}
          initialType={tab || 'git'}
          onClose={() => setEditing(undefined)}
          onSaved={() => {
            setEditing(undefined)
            refresh()
          }}
        />
      )}
    </section>
  )
}

function ConnectionDialog({ conn, types, initialType, onClose, onSaved }: { conn?: ServiceConnection; types: ConnType[]; initialType: string; onClose: () => void; onSaved: () => void }) {
  const [tab, setTab] = useState<'basic' | 'access'>('basic')
  const [type, setType] = useState(conn?.type ?? initialType)
  const [name, setName] = useState(conn?.name ?? '')
  const [description, setDescription] = useState(conn?.description ?? '')
  const [values, setValues] = useState<Record<string, string>>(() => {
    const v = { ...defaults(types.find((t) => t.type === (conn?.type ?? initialType))), ...(conn?.config ?? {}) }
    for (const k of conn?.secrets ?? []) v[k] = SECRET_MASK
    return v
  })
  const [visibility, setVisibility] = useState<string>(conn?.visibility ?? 'private')
  const [allowed, setAllowed] = useState<string[]>(conn?.allowed_users ?? [])
  const [team, setTeam] = useState<Member[]>([])
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<{ ok: boolean; text: string }>()
  const ct = useMemo(() => types.find((t) => t.type === type)!, [types, type])
  useEffect(() => {
    api.listMembers().then(setTeam, () => {})
  }, [])

  const input = (): ConnInput => ({ type, name, description, values, visibility, allowed_users: allowed })
  async function save() {
    setBusy(true)
    setMsg(undefined)
    try {
      await api.saveConnection(input(), conn?.id)
      onSaved()
    } catch (e) {
      setMsg({ ok: false, text: (e as Error).message })
      setBusy(false)
    }
  }
  async function test() {
    setBusy(true)
    setMsg(undefined)
    try {
      await api.testConnection({ ...input(), id: conn?.id })
      setMsg({ ok: true, text: ct.type === 'npm' || ct.type === 'git' || ct.type === 'oss' || ct.type === 'cloudreve' ? '连接正常' : '已发送，请查收' })
    } catch (e) {
      setMsg({ ok: false, text: (e as Error).message })
    } finally {
      setBusy(false)
    }
  }
  const platform = values.platform ?? ct.fields.find((f) => f.key === 'platform')?.options?.[0]
  const visibleFields = ct.fields.filter((f) => !(ct.type === 'git' && f.key === 'server' && platform !== 'gitea'))

  return (
    <Modal
      wide
      title={conn ? `编辑服务连接「${conn.name}」` : '新建服务连接'}
      onClose={onClose}
      footer={
        <>
          {ct.testable && (
            <button className="btn" disabled={busy} onClick={test}>
              {ct.type === 'npm' ? '测试登录' : ct.type === 'git' || ct.type === 'oss' || ct.type === 'cloudreve' ? '测试连接' : '测试发送'}
            </button>
          )}
          {msg && <span className={msg.ok ? 'ok-text small' : 'error-text small'}>{msg.text}</span>}
          <div className="spacer" />
          <button className="btn" onClick={onClose}>
            取消
          </button>
          <button className="btn primary" disabled={busy || !name.trim()} onClick={save}>
            {conn ? '保存' : '创建'}
          </button>
        </>
      }
    >
      <p className="muted small">流水线节点按类型选择服务连接；私密字段保存后不再回显，留空即保持原值</p>
      <div className="pills">
        <button className={tab === 'basic' ? 'active' : ''} onClick={() => setTab('basic')}>
          基本信息
        </button>
        <button className={tab === 'access' ? 'active' : ''} onClick={() => setTab('access')}>
          成员权限
        </button>
      </div>
      {tab === 'basic' ? (
        <div className="conn-form">
          <label className="field">
            <span>服务连接名称 *</span>
            <input autoFocus maxLength={64} value={name} onChange={(e) => setName(e.target.value)} placeholder="在流水线里按这个名字引用" />
          </label>
          <label className="field">
            <span>类型</span>
            <select value={type} disabled={!!conn} onChange={(e) => (setType(e.target.value), setValues(defaults(types.find((t) => t.type === e.target.value))))}>
              {types.map((t) => (
                <option key={t.type} value={t.type}>
                  {t.label}
                </option>
              ))}
            </select>
            <span className="muted small">{ct.description}</span>
          </label>
          <label className="field">
            <span>说明</span>
            <input maxLength={200} value={description} onChange={(e) => setDescription(e.target.value)} />
          </label>
          {visibleFields.map((f) => (
            <label key={f.key} className={f.bool ? 'check' : 'field'}>
              {f.bool ? (
                <>
                  <input type="checkbox" checked={values[f.key] === 'true'} onChange={(e) => setValues((v) => ({ ...v, [f.key]: e.target.checked ? 'true' : 'false' }))} />
                  {f.label}
                </>
              ) : (
                <>
                  <span>
                    {f.label}
                    {f.required && ' *'}
                  </span>
                  {f.options ? (
                    <select value={values[f.key] ?? f.options[0]} onChange={(e) => setValues((v) => ({ ...v, [f.key]: e.target.value }))}>
                      {f.options.map((o) => (
                        <option key={o} value={o}>
                          {optionLabels[o] ?? o}
                        </option>
                      ))}
                    </select>
                  ) : (
                    <input
                      className={f.secret ? 'mono' : undefined}
                      type={f.secret ? 'password' : 'text'}
                      autoComplete="off"
                      value={values[f.key] ?? ''}
                      placeholder={f.placeholder}
                      onFocus={() => f.secret && values[f.key] === SECRET_MASK && setValues((v) => ({ ...v, [f.key]: '' }))}
                      onChange={(e) => setValues((v) => ({ ...v, [f.key]: e.target.value }))}
                    />
                  )}
                </>
              )}
              {f.description && <span className="muted small">{f.description}</span>}
            </label>
          ))}
          {ct.type !== 'git' || platform === 'generic' ? null : (
            <p className="muted small">选具体平台后，流水线源里可以直接下拉选择仓库与分支。</p>
          )}
          <details className="hint">
            <summary>在步骤里怎么用</summary>
            <p className="small">
              步骤写 <code>connection: {name || '名称'}</code> 后，字段以 <code>CONN_字段名</code> 注入该步骤，如{' '}
              {ct.fields
                .slice(0, 3)
                .map((f) => `CONN_${f.key.toUpperCase()}`)
                .join('、')}
              ；私密字段在日志中显示为 ***。
            </p>
          </details>
        </div>
      ) : (
        <div className="conn-form">
          {(['private', 'public', 'custom'] as const).map((v) => (
            <label key={v} className="radio-card">
              <input type="radio" checked={visibility === v} onChange={() => setVisibility(v)} />
              <span>
                <span className="strong">{v === 'private' ? '仅自己可见（默认）' : v === 'public' ? '公开，所有人可见' : '自定义，仅指定成员可见'}</span>
                <span className="muted small">
                  {v === 'private' ? '只有你能在流水线里选到它' : v === 'public' ? '所有成员都能在流水线里引用；修改和删除仍限创建人和管理员' : '下面勾选的成员可以引用'}
                </span>
              </span>
            </label>
          ))}
          {visibility === 'custom' && (
            <div className="group-checks">
              {team.map((m) => (
                <label key={m.user_id} className="check">
                  <input
                    type="checkbox"
                    checked={allowed.includes(m.username)}
                    onChange={(e) => setAllowed((a) => (e.target.checked ? [...a, m.username] : a.filter((x) => x !== m.username)))}
                  />
                  {m.display_name || m.username} <span className="muted small">@{m.username}</span>
                </label>
              ))}
            </div>
          )}
        </div>
      )}
    </Modal>
  )
}
