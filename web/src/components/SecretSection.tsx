import { useState } from 'react'
import { api } from '../api'
import { fmtTime } from '../format'
import { usePolling } from '../hooks'

/** 密钥列表与编辑：pipelineId 为空时为租户级。值只写不读，运行时注入为同名环境变量并在日志中脱敏。 */
export function SecretSection({ pipelineId, canEdit }: { pipelineId?: string; canEdit: boolean }) {
  const { data: secrets, error, refresh } = usePolling(() => api.listSecrets(pipelineId), [pipelineId], () => null)
  const [editing, setEditing] = useState<string | null>(null) // 正在设置的密钥名；'' 为新建
  const [actionError, setActionError] = useState<string>()

  async function remove(name: string) {
    if (!confirm(`确定删除密钥 ${name}？引用它的流水线将拿不到该变量。`)) return
    setActionError(undefined)
    try {
      await api.deleteSecret(name, pipelineId)
      refresh()
    } catch (e) {
      setActionError((e as Error).message)
    }
  }

  return (
    <>
      {(error || actionError) && <div className="alert">{error ?? actionError}</div>}
      {editing !== null && (
        <SecretForm
          pipelineId={pipelineId}
          name={editing}
          onDone={() => {
            setEditing(null)
            refresh()
          }}
        />
      )}
      {secrets && secrets.length === 0 && editing === null && <p className="muted">还没有密钥。</p>}
      {secrets && secrets.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>名称</th>
                <th>值</th>
                <th>更新时间</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {secrets.map((s) => (
                <tr key={s.id}>
                  <td className="mono strong">{s.name}</td>
                  <td className="muted">••••••••</td>
                  <td className="muted">{fmtTime(s.updated_at)}</td>
                  <td className="actions">
                    {canEdit && (
                      <>
                        <button className="btn" onClick={() => setEditing(s.name)}>
                          更新
                        </button>
                        <button className="btn danger" onClick={() => remove(s.name)}>
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
      {canEdit && editing === null && (
        <button className="btn link" onClick={() => setEditing('')}>
          + 添加密钥
        </button>
      )}
    </>
  )
}

function SecretForm({ pipelineId, name: initial, onDone }: { pipelineId?: string; name: string; onDone: () => void }) {
  const [name, setName] = useState(initial)
  const [value, setValue] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string>()

  async function save() {
    setSaving(true)
    setError(undefined)
    try {
      await api.putSecret(name.trim(), value, pipelineId)
      onDone()
    } catch (e) {
      setError((e as Error).message)
      setSaving(false)
    }
  }

  return (
    <div className="card">
      <label className="field">
        <span>名称</span>
        <input className="mono" value={name} disabled={initial !== ''} onChange={(e) => setName(e.target.value)} placeholder="例如 DEPLOY_TOKEN" />
      </label>
      <label className="field">
        <span>{initial ? '新值' : '值'}</span>
        <textarea className="env-editor mono" spellCheck={false} value={value} onChange={(e) => setValue(e.target.value)} />
      </label>
      <p className="muted small">保存后无法再查看。运行时以同名环境变量注入，日志中出现的值会显示为 ***。</p>
      <div className="row">
        <button className="btn primary" disabled={saving || !name.trim() || !value} onClick={save}>
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
