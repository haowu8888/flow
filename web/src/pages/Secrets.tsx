import { useState } from 'react'
import { api } from '../api'
import { useSession } from '../auth'
import { Modal } from '../components/Modal'
import { SecretSection } from '../components/SecretSection'
import { fmtShort } from '../format'
import { usePolling } from '../hooks'
import { SECRET_MASK, type GroupVar, type VarGroup } from '../types'

/** 通用变量：变量组（运行时注入为环境变量，流水线自己的变量优先）与租户级密钥。 */
export function Secrets() {
  const { can } = useSession()
  const { data, error, refresh } = usePolling(() => api.listVarGroups(), [], () => null)
  const [editing, setEditing] = useState<VarGroup | 'new'>()
  const [actionError, setActionError] = useState<string>()
  async function remove(g: VarGroup) {
    if (!confirm(`删除变量组「${g.name}」？关联它的流水线下次运行会报错。`)) return
    setActionError(undefined)
    try {
      await api.deleteVarGroup(g.id)
      refresh()
    } catch (e) {
      setActionError((e as Error).message)
    }
  }
  return (
    <section>
      <div className="page-head">
        <h1>通用变量</h1>
        <div className="spacer" />
        {can('admin') && (
          <button className="btn primary" onClick={() => setEditing('new')}>
            新建变量组
          </button>
        )}
      </div>
      <p className="muted">所有变量组的变量在每次运行时注入为环境变量（流水线可只关联部分组；流水线自己的变量优先）；私密变量只在保存时可见，日志中自动脱敏。</p>
      {(error || actionError) && <div className="alert">{error ?? actionError}</div>}
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>名称</th>
              <th>ID</th>
              <th>变量</th>
              <th>创建人</th>
              <th>修改人</th>
              <th>说明</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {data?.length === 0 && (
              <tr>
                <td colSpan={7} className="muted center-cell">
                  还没有变量组，新建一个并添加变量，如 NPM_TOKEN、DOCKER_REGISTRY
                </td>
              </tr>
            )}
            {data?.map((g) => (
              <tr key={g.id}>
                <td className="strong">{g.name}</td>
                <td className="mono muted small">{g.id}</td>
                <td className="vars-cell">
                  {g.vars.length === 0 ? (
                    <span className="muted">空</span>
                  ) : (
                    g.vars.map((v) => (
                      <span key={v.name} className="tag mono" title={v.secret ? '私密' : v.value}>
                        {v.secret ? '🔒 ' : ''}
                        {v.name}
                      </span>
                    ))
                  )}
                </td>
                <td className="muted">{g.created_by || '—'}</td>
                <td className="muted" title={fmtShort(g.updated_at)}>
                  {g.updated_by || '—'}
                </td>
                <td className="muted">{g.description || '—'}</td>
                <td className="actions">
                  {can('admin') && (
                    <>
                      <button className="btn small" onClick={() => setEditing(g)}>
                        编辑
                      </button>{' '}
                      <button className="btn small danger" onClick={() => remove(g)}>
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

      <h2>租户密钥</h2>
      <p className="muted small">
        单个密钥，对所有流水线可见并注入为同名环境变量；通知地址与代码源令牌也可通过 <code>{'${NAME}'}</code> / <code>token_secret</code> 引用。
        {!can('admin') && ' 只有管理员可以修改。'}
      </p>
      <SecretSection canEdit={can('admin')} />

      {editing && (
        <GroupDialog
          group={editing === 'new' ? undefined : editing}
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

function GroupDialog({ group, onClose, onSaved }: { group?: VarGroup; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState(group?.name ?? '')
  const [description, setDescription] = useState(group?.description ?? '')
  const [vars, setVars] = useState<GroupVar[]>(() => group?.vars.map((v) => (v.secret ? { ...v, value: SECRET_MASK } : v)) ?? [{ name: '', value: '' }])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const update = (i: number, patch: Partial<GroupVar>) => setVars((vs) => vs.map((v, j) => (j === i ? { ...v, ...patch } : v)))
  async function save() {
    setBusy(true)
    setError(undefined)
    try {
      await api.saveVarGroup({ name, description, vars: vars.filter((v) => v.name.trim()) }, group?.id)
      onSaved()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }
  return (
    <Modal
      wide
      title={group ? `编辑变量组「${group.name}」` : '新建变量组'}
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
      <p className="muted small">变量以环境变量注入每次运行；私密变量保存后不再回显，留着掩码不动即保持原值。</p>
      <div className="row">
        <label className="field grow">
          <span>变量组名称</span>
          <input autoFocus maxLength={64} value={name} onChange={(e) => setName(e.target.value)} />
        </label>
        <label className="field grow">
          <span>说明</span>
          <input maxLength={200} value={description} onChange={(e) => setDescription(e.target.value)} />
        </label>
      </div>
      <table className="vars-table">
        <thead>
          <tr>
            <th>变量名</th>
            <th>值</th>
            <th>私密</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {vars.map((v, i) => (
            <tr key={i}>
              <td>
                <input className="mono" value={v.name} placeholder="NPM_TOKEN" onChange={(e) => update(i, { name: e.target.value.trim() })} />
              </td>
              <td>
                <input
                  className="mono"
                  type={v.secret ? 'password' : 'text'}
                  value={v.value}
                  onFocus={() => v.secret && v.value === SECRET_MASK && update(i, { value: '' })}
                  onChange={(e) => update(i, { value: e.target.value })}
                />
              </td>
              <td>
                <input type="checkbox" className="switch" checked={!!v.secret} onChange={(e) => update(i, { secret: e.target.checked })} />
              </td>
              <td>
                <button className="icon-btn" title="删除" onClick={() => setVars((vs) => vs.filter((_, j) => j !== i))}>
                  ×
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <button className="btn small" onClick={() => setVars((vs) => [...vs, { name: '', value: '' }])}>
        + 添加变量
      </button>
      {error && <div className="alert">{error}</div>}
    </Modal>
  )
}
