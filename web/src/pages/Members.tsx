import { useState } from 'react'
import { api } from '../api'
import { useSession } from '../auth'
import { usePolling } from '../hooks'
import { roleLabel, type Member, type Role } from '../types'

const roles: Role[] = ['viewer', 'developer', 'admin']

const roleHint: Record<Role, string> = {
  viewer: '查看流水线、运行与日志',
  developer: '另可编辑 / 运行流水线、审批卡点、管理流水线级密钥与触发器',
  admin: '另可管理成员、Runner 与租户级密钥',
}

export function Members() {
  const { me, can } = useSession()
  const { data: members, error, refresh } = usePolling(() => api.listMembers(), [], () => null)
  const [adding, setAdding] = useState(false)
  const [actionError, setActionError] = useState<string>()
  const tenant = me.tenants.find((t) => t.id === me.tenant_id)

  async function act(fn: () => Promise<unknown>) {
    setActionError(undefined)
    try {
      await fn()
      refresh()
    } catch (e) {
      setActionError((e as Error).message)
    }
  }

  function resetPassword(m: Member) {
    const pw = prompt(`为 ${m.username} 设置新密码（至少 8 位），其现有登录会话将失效：`)
    if (pw) act(() => api.resetPassword(m.user_id, pw))
  }

  return (
    <section>
      <div className="page-head">
        <h1>团队成员</h1>
        <span className="muted">{tenant?.name ?? me.tenant_id}</span>
        <div className="spacer" />
        {can('admin') && !adding && (
          <button className="btn primary" onClick={() => setAdding(true)}>
            添加成员
          </button>
        )}
      </div>
      <ul className="muted small role-hints">
        {roles.map((r) => (
          <li key={r}>
            <b>{roleLabel[r]}</b>：{roleHint[r]}
          </li>
        ))}
      </ul>
      {(error || actionError) && <div className="alert">{error ?? actionError}</div>}
      {adding && (
        <AddMemberForm
          onDone={() => {
            setAdding(false)
            refresh()
          }}
        />
      )}
      {members && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>用户</th>
                <th>角色</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {members.map((m) => (
                <tr key={m.user_id}>
                  <td>
                    <span className="strong">{m.display_name || m.username}</span>
                    {m.display_name && <span className="muted"> @{m.username}</span>}
                    {m.is_admin && <span className="tag">系统管理员</span>}
                  </td>
                  <td>
                    {can('admin') && m.user_id !== me.user.id ? (
                      <select value={m.role} onChange={(e) => act(() => api.setMemberRole(m.user_id, e.target.value as Role))}>
                        {roles.map((r) => (
                          <option key={r} value={r}>
                            {roleLabel[r]}
                          </option>
                        ))}
                      </select>
                    ) : (
                      roleLabel[m.role]
                    )}
                  </td>
                  <td className="actions">
                    {can('admin') && m.user_id !== me.user.id && (
                      <>
                        {(!m.is_admin || me.user.is_admin) && (
                          <button className="btn" onClick={() => resetPassword(m)}>
                            重置密码
                          </button>
                        )}
                        <button
                          className="btn danger"
                          onClick={() => confirm(`确定将 ${m.username} 移出当前租户？`) && act(() => api.removeMember(m.user_id))}
                        >
                          移除
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
      {me.user.is_admin && <NewTenant />}
    </section>
  )
}

function AddMemberForm({ onDone }: { onDone: () => void }) {
  const [username, setUsername] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [password, setPassword] = useState('')
  const [role, setRole] = useState<Role>('developer')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string>()

  async function save() {
    setSaving(true)
    setError(undefined)
    try {
      await api.addMember({ username: username.trim(), display_name: displayName.trim(), password, role })
      onDone()
    } catch (e) {
      setError((e as Error).message)
      setSaving(false)
    }
  }

  return (
    <div className="card">
      <label className="field">
        <span>用户名</span>
        <input value={username} onChange={(e) => setUsername(e.target.value)} placeholder="已有用户直接加入；新用户需填写初始密码" />
      </label>
      <label className="field">
        <span>显示名</span>
        <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} placeholder="可选，仅新用户" />
      </label>
      <label className="field">
        <span>初始密码</span>
        <input type="password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder="新用户必填，至少 8 位" />
      </label>
      <label className="field">
        <span>角色</span>
        <select value={role} onChange={(e) => setRole(e.target.value as Role)}>
          {roles.map((r) => (
            <option key={r} value={r}>
              {roleLabel[r]}
            </option>
          ))}
        </select>
      </label>
      <div className="row">
        <button className="btn primary" disabled={saving || !username.trim()} onClick={save}>
          {saving ? '保存中…' : '添加'}
        </button>
        <button className="btn" disabled={saving} onClick={onDone}>
          取消
        </button>
        {error && <span className="error-text">{error}</span>}
      </div>
    </div>
  )
}

/** 系统管理员新建租户（创建者自动成为该租户管理员，可在右上角切换）。 */
function NewTenant() {
  const { setMe } = useSession()
  const [id, setId] = useState('')
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<{ ok: boolean; text: string }>()

  async function create() {
    setBusy(true)
    setMsg(undefined)
    try {
      await api.createTenant(id.trim(), name.trim())
      setMe(await api.me())
      setMsg({ ok: true, text: '已创建，可在右上角切换到新租户' })
      setId('')
      setName('')
    } catch (e) {
      setMsg({ ok: false, text: (e as Error).message })
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <h2>新建租户</h2>
      <div className="card">
        <div className="row">
          <input className="mono" value={id} onChange={(e) => setId(e.target.value)} placeholder="标识，如 team-a" />
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="名称，如 A 组" />
          <button className="btn" disabled={busy || !id.trim()} onClick={create}>
            创建
          </button>
          {msg && <span className={msg.ok ? 'ok-text' : 'error-text'}>{msg.text}</span>}
        </div>
      </div>
    </>
  )
}
