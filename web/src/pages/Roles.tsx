import { useState } from 'react'
import { api } from '../api'
import { useSession } from '../auth'
import { Modal } from '../components/Modal'
import { usePolling } from '../hooks'
import { permLabels, type Perm, type PipelineRole } from '../types'

const perms = Object.keys(permLabels) as Perm[]

/** 角色管理：流水线成员按角色授权，所有流水线共用；内置角色不可修改，租户管理员可新建自定义角色。 */
export function Roles() {
  const { can } = useSession()
  const { data, error, refresh } = usePolling(() => api.listPipelineRoles(), [], () => null)
  const [editing, setEditing] = useState<PipelineRole | 'new'>()
  const [actionError, setActionError] = useState<string>()

  async function remove(r: PipelineRole) {
    if (!confirm(`删除角色「${r.name}」？`)) return
    setActionError(undefined)
    try {
      await api.deletePipelineRole(r.id)
      refresh()
    } catch (e) {
      setActionError((e as Error).message)
    }
  }

  return (
    <section>
      <div className="page-head">
        <h1>角色管理</h1>
        <div className="spacer" />
        {can('admin') && (
          <button className="btn primary" onClick={() => setEditing('new')}>
            新建角色
          </button>
        )}
      </div>
      <p className="muted">流水线成员按角色授权，所有流水线共用这里的角色；创建人自动成为拥有者，租户管理员拥有全部流水线的全部权限。只有租户管理员能修改角色。</p>
      {(error || actionError) && <div className="alert">{error ?? actionError}</div>}
      <div className="role-list">
        {data?.map((r) => (
          <div key={r.id} className="card role-card">
            <div className="role-head">
              <span className="strong">{r.name}</span>
              {r.builtin && <span className="tag">内置</span>}
              {r.id === 'member' && <span className="tag">默认角色</span>}
              <div className="spacer" />
              {!r.builtin && can('admin') && (
                <>
                  <button className="btn small" onClick={() => setEditing(r)}>
                    编辑
                  </button>
                  <button className="btn small danger" disabled={r.pipelines > 0} title={r.pipelines > 0 ? '仍有流水线在用' : undefined} onClick={() => remove(r)}>
                    删除
                  </button>
                </>
              )}
            </div>
            <div className="muted small">{r.description || '—'}</div>
            <div className="role-perms">
              {perms.map((p) => (
                <span key={p} className={`perm${r.permissions.includes(p) ? ' on' : ''}`} title={permLabels[p].hint}>
                  {r.permissions.includes(p) ? '✓' : '–'} {permLabels[p].label}
                </span>
              ))}
            </div>
            {r.pipelines > 0 && <div className="muted small">{r.pipelines} 条流水线在用</div>}
          </div>
        ))}
      </div>
      {editing && (
        <RoleDialog
          role={editing === 'new' ? undefined : editing}
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

function RoleDialog({ role, onClose, onSaved }: { role?: PipelineRole; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState(role?.name ?? '')
  const [description, setDescription] = useState(role?.description ?? '')
  const [chosen, setChosen] = useState<Perm[]>(role?.permissions ?? ['view', 'run'])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  async function save() {
    setBusy(true)
    setError(undefined)
    try {
      await api.savePipelineRole({ name, description, permissions: chosen }, role?.id)
      onSaved()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }
  return (
    <Modal
      title={role ? `编辑角色「${role.name}」` : '新建角色'}
      onClose={onClose}
      footer={
        <>
          <div className="spacer" />
          <button className="btn" onClick={onClose}>
            取消
          </button>
          <button className="btn primary" disabled={busy || !name.trim()} onClick={save}>
            保存
          </button>
        </>
      }
    >
      <label className="field">
        <span>角色名称</span>
        <input autoFocus maxLength={32} value={name} onChange={(e) => setName(e.target.value)} placeholder="如：发布员" />
      </label>
      <label className="field">
        <span>说明</span>
        <input maxLength={200} value={description} onChange={(e) => setDescription(e.target.value)} />
      </label>
      <div className="field">
        <span>权限</span>
        {perms.map((p) => (
          <label key={p} className="check perm-check">
            <input
              type="checkbox"
              disabled={p === 'view'}
              checked={p === 'view' || chosen.includes(p)}
              onChange={(e) => setChosen((c) => (e.target.checked ? [...c, p] : c.filter((x) => x !== p)))}
            />
            <span>
              {permLabels[p].label}
              <span className="muted small"> · {permLabels[p].hint}</span>
            </span>
          </label>
        ))}
      </div>
      {error && <div className="alert">{error}</div>}
    </Modal>
  )
}
