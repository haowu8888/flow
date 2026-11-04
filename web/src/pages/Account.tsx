import { useState } from 'react'
import { api } from '../api'
import { useSession } from '../auth'
import { CopyButton } from '../components/CopyButton'
import { Modal } from '../components/Modal'
import { fmtShort, fmtTime } from '../format'
import { usePolling } from '../hooks'
import { roleLabel, type Session } from '../types'

export function Account() {
  const { me, setMe } = useSession()
  const [form, setForm] = useState({ display_name: me.user.display_name ?? '', email: me.user.email ?? '', gender: me.user.gender ?? '', bio: me.user.bio ?? '' })
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<{ ok: boolean; text: string }>()
  const set = (k: keyof typeof form) => (e: { target: { value: string } }) => setForm((f) => ({ ...f, [k]: e.target.value }))

  async function save() {
    setBusy(true)
    setMsg(undefined)
    try {
      setMe(await api.updateProfile(form))
      setMsg({ ok: true, text: '已保存' })
    } catch (e) {
      setMsg({ ok: false, text: (e as Error).message })
    } finally {
      setBusy(false)
    }
  }

  return (
    <section>
      <div className="page-head">
        <h1>常规</h1>
        <span className="muted">
          @{me.user.username} · {roleLabel[me.role]}
          {me.user.is_admin && ' · 系统管理员'}
        </span>
      </div>
      <div className="card narrow form-card">
        <div className="profile-head">
          <span className="avatar large">{(form.display_name || me.user.username).slice(0, 2)}</span>
          <div>
            <div className="strong">{form.display_name || me.user.username}</div>
            <div className="muted small">{form.email || '未填写邮箱'}</div>
          </div>
        </div>
        <label className="field">
          <span>姓名</span>
          <input maxLength={32} value={form.display_name} onChange={set('display_name')} placeholder={me.user.username} />
        </label>
        <label className="field">
          <span>邮箱</span>
          <input type="email" value={form.email} onChange={set('email')} placeholder="用于邮件通知与审批" />
        </label>
        <label className="field">
          <span>性别</span>
          <select value={form.gender} onChange={set('gender')}>
            <option value="">保密</option>
            <option value="male">男</option>
            <option value="female">女</option>
          </select>
        </label>
        <label className="field">
          <span>简介</span>
          <textarea rows={3} maxLength={500} value={form.bio} onChange={set('bio')} />
        </label>
        <div className="row">
          <button className="btn primary" disabled={busy} onClick={save}>
            保存
          </button>
          {msg && <span className={msg.ok ? 'ok-text' : 'error-text'}>{msg.text}</span>}
        </div>
      </div>
      <PasswordForm />
    </section>
  )
}

/** 安全：活动会话。 */
export function Security() {
  const { data, error, refresh } = usePolling(() => api.listSessions(), [], () => null)
  const [actionError, setActionError] = useState<string>()
  async function revoke(se: Session) {
    if (se.current) {
      if (!confirm('退出当前登录？')) return
      await api.logout().catch(() => {})
      window.location.hash = '#/'
      window.location.reload()
      return
    }
    if (!confirm('撤销这个会话？该设备需要重新登录。')) return
    setActionError(undefined)
    try {
      await api.revokeSession(se.id)
      refresh()
    } catch (e) {
      setActionError((e as Error).message)
    }
  }
  return (
    <section>
      <div className="page-head">
        <h1>活动会话</h1>
      </div>
      <p className="muted">以下是您账户的所有活动会话。点击「撤销」可结束其他设备上的会话，当前会话点「退出登录」。</p>
      {(error || actionError) && <div className="alert">{error ?? actionError}</div>}
      {data && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>设备 / 浏览器</th>
                <th>IP 地址</th>
                <th>登录时间</th>
                <th>过期时间</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {data.map((se) => (
                <tr key={se.id}>
                  <td title={se.user_agent}>
                    {deviceOf(se.user_agent)}
                    {se.current && <span className="tag">当前</span>}
                  </td>
                  <td className="muted mono">{se.ip || '—'}</td>
                  <td className="muted">{fmtShort(se.created_at)}</td>
                  <td className="muted">{fmtShort(se.expires_at)}</td>
                  <td>
                    <button className={`btn small${se.current ? '' : ' danger'}`} onClick={() => revoke(se)}>
                      {se.current ? '退出登录' : '撤销'}
                    </button>
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

/** 从 User-Agent 粗略识别浏览器与系统，如「Chrome 130 / Windows 10」。 */
export function deviceOf(ua?: string) {
  if (!ua) return '未知设备'
  if (!/Mozilla|Chrome|Safari|Firefox/.test(ua)) return ua.split(' ')[0]
  const browser =
    ua.match(/Edg\/(\d+)/)?.[1] !== undefined
      ? `Edge ${ua.match(/Edg\/(\d+)/)![1]}`
      : ua.match(/Firefox\/(\d+)/)
        ? `Firefox ${ua.match(/Firefox\/(\d+)/)![1]}`
        : ua.match(/Chrome\/(\d+)/)
          ? `Chrome ${ua.match(/Chrome\/(\d+)/)![1]}`
          : ua.match(/Version\/(\d+).*Safari/)
            ? `Safari ${ua.match(/Version\/(\d+)/)![1]}`
            : '浏览器'
  const os = /Windows NT 10/.test(ua)
    ? 'Windows 10'
    : /Windows/.test(ua)
      ? 'Windows'
      : /iPhone|iPad/.test(ua)
        ? 'iOS'
        : /Mac OS X/.test(ua)
          ? 'macOS'
          : /Android/.test(ua)
            ? 'Android'
            : /Linux/.test(ua)
              ? 'Linux'
              : ''
  return os ? `${browser} / ${os}` : browser
}

/** 危险操作：删除账户。 */
export function DangerZone() {
  const [open, setOpen] = useState(false)
  const [pw, setPw] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  async function remove() {
    setBusy(true)
    setError(undefined)
    try {
      await api.deleteAccount(pw)
      window.location.hash = '#/'
      window.location.reload()
    } catch (e) {
      const t = (e as Error).message
      setError(t === 'invalid username or password' ? '密码错误' : t)
      setBusy(false)
    }
  }
  return (
    <section>
      <div className="page-head">
        <h1>危险操作</h1>
      </div>
      <div className="card danger-card">
        <div>
          <div className="strong">删除账户</div>
          <div className="muted small">删除账户将永久清除您的登录会话、API Key、成员关系与收藏；您创建的流水线会保留。</div>
        </div>
        <button className="btn danger" onClick={() => setOpen(true)}>
          删除账户
        </button>
      </div>
      {open && (
        <Modal
          title="确认删除账户？"
          onClose={() => setOpen(false)}
          footer={
            <>
              <div className="spacer" />
              <button className="btn" onClick={() => setOpen(false)}>
                取消
              </button>
              <button className="btn danger" disabled={busy || !pw} onClick={remove}>
                {busy ? '删除中…' : '删除账户'}
              </button>
            </>
          }
        >
          <p>此操作不可恢复，将永久删除你的账号和所有相关数据。</p>
          <label className="field">
            <span>输入当前密码确认</span>
            <input type="password" autoFocus autoComplete="current-password" value={pw} onChange={(e) => setPw(e.target.value)} />
          </label>
          {error && <div className="alert">{error}</div>}
        </Modal>
      )}
    </section>
  )
}

function PasswordForm() {
  const [oldPw, setOldPw] = useState('')
  const [pw, setPw] = useState('')
  const [pw2, setPw2] = useState('')
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<{ ok: boolean; text: string }>()

  async function save() {
    if (pw !== pw2) {
      setMsg({ ok: false, text: '两次输入的新密码不一致' })
      return
    }
    setBusy(true)
    setMsg(undefined)
    try {
      await api.changePassword(oldPw, pw)
      setMsg({ ok: true, text: '已修改，其他设备上的登录已失效' })
      setOldPw('')
      setPw('')
      setPw2('')
    } catch (e) {
      const text = (e as Error).message
      setMsg({ ok: false, text: text === 'invalid username or password' ? '当前密码错误' : text })
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <h2>修改密码</h2>
      <div className="card narrow">
        <label className="field">
          <span>当前密码</span>
          <input type="password" autoComplete="current-password" value={oldPw} onChange={(e) => setOldPw(e.target.value)} />
        </label>
        <label className="field">
          <span>新密码</span>
          <input type="password" autoComplete="new-password" value={pw} onChange={(e) => setPw(e.target.value)} placeholder="至少 8 位" />
        </label>
        <label className="field">
          <span>确认新密码</span>
          <input type="password" autoComplete="new-password" value={pw2} onChange={(e) => setPw2(e.target.value)} />
        </label>
        <div className="row">
          <button className="btn primary" disabled={busy || !oldPw || !pw} onClick={save}>
            保存
          </button>
          {msg && <span className={msg.ok ? 'ok-text' : 'error-text'}>{msg.text}</span>}
        </div>
      </div>
    </>
  )
}

/** API Key：个人访问令牌。 */
export function Tokens() {
  const { data: tokens, error, refresh } = usePolling(() => api.listTokens(), [], () => null)
  const [name, setName] = useState('')
  const [days, setDays] = useState(0)
  const [created, setCreated] = useState<string>()
  const [actionError, setActionError] = useState<string>()

  async function create() {
    setActionError(undefined)
    try {
      setCreated((await api.createToken(name.trim(), days)).token)
      setName('')
      refresh()
    } catch (e) {
      setActionError((e as Error).message)
    }
  }

  async function remove(id: string) {
    if (!confirm('确定吊销这个令牌？使用它的脚本将无法再访问。')) return
    setActionError(undefined)
    try {
      await api.deleteToken(id)
      refresh()
    } catch (e) {
      setActionError((e as Error).message)
    }
  }

  const example = created ? `curl -X POST -H "Authorization: Bearer ${created}" ${window.location.origin}/api/pipelines/<流水线ID>/runs` : ''

  return (
    <section>
      <div className="page-head">
        <h1>API Key</h1>
      </div>
      <p className="muted">个人访问令牌，用于脚本或其他系统调用 API（如触发运行），权限与你在当前租户的角色相同，且固定在当前租户。</p>
      {(error || actionError) && <div className="alert">{error ?? actionError}</div>}
      {created && (
        <div className="card notice">
          <div className="strong">令牌只显示这一次，请立即保存：</div>
          <div>
            <code>{created}</code> <CopyButton text={created} />
          </div>
          <div>
            <code className="block">{example}</code>
          </div>
          <button className="btn" onClick={() => setCreated(undefined)}>
            我已保存
          </button>
        </div>
      )}
      <div className="card">
        <div className="row">
          <input value={name} maxLength={64} onChange={(e) => setName(e.target.value)} placeholder="名称，例如 发布脚本" />
          <select value={days} onChange={(e) => setDays(+e.target.value)} title="有效期">
            <option value={0}>永不过期</option>
            <option value={7}>7 天</option>
            <option value={30}>30 天</option>
            <option value={90}>90 天</option>
            <option value={365}>1 年</option>
          </select>
          <button className="btn primary" disabled={!name.trim()} onClick={create}>
            创建 API Key
          </button>
          <button className="btn" onClick={() => refresh()}>
            刷新
          </button>
        </div>
      </div>
      {tokens && tokens.length === 0 && <p className="muted">暂无 API Key</p>}
      {tokens && tokens.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>名称</th>
                <th>令牌</th>
                <th>状态</th>
                <th>过期时间</th>
                <th>最后使用</th>
                <th>创建时间</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {tokens.map((t) => (
                <tr key={t.id}>
                  <td className="strong">{t.name}</td>
                  <td className="mono muted">{t.prefix}…</td>
                  <td>
                    {t.expires_at && new Date(t.expires_at).getTime() < Date.now() ? (
                      <span className="badge status-skipped">已过期</span>
                    ) : (
                      <span className="badge status-success">有效</span>
                    )}
                  </td>
                  <td className="muted">{t.expires_at ? fmtShort(t.expires_at) : '永不过期'}</td>
                  <td className="muted">{t.last_used_at ? fmtTime(t.last_used_at) : '-'}</td>
                  <td className="muted">{fmtShort(t.created_at)}</td>
                  <td className="actions">
                    <button className="btn danger" onClick={() => remove(t.id)}>
                      吊销
                    </button>
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
