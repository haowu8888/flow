import { useState, type FormEvent } from 'react'
import { api } from '../api'
import type { Me } from '../types'

export function Login({ onLogin }: { onLogin: (me: Me) => void }) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()

  async function submit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      onLogin(await api.login(username, password))
    } catch (err) {
      setError((err as Error).message === 'invalid username or password' ? '用户名或密码错误' : (err as Error).message)
      setBusy(false)
    }
  }

  return (
    <div className="login-wrap">
      <form className="card login-card" onSubmit={submit}>
        <div className="brand login-brand">
          <span className="logo">◆</span> Flow
        </div>
        <label className="field">
          <span>用户名</span>
          <input autoFocus autoComplete="username" value={username} onChange={(e) => setUsername(e.target.value)} />
        </label>
        <label className="field">
          <span>密码</span>
          <input type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />
        </label>
        {error && <div className="alert subtle">{error}</div>}
        <button className="btn primary" disabled={busy || !username || !password}>
          {busy ? '登录中…' : '登录'}
        </button>
        <p className="muted small">首次启动时 Server 日志会打印管理员 admin 的初始密码（或使用 FLOW_ADMIN_PASSWORD 指定）。</p>
      </form>
    </div>
  )
}
