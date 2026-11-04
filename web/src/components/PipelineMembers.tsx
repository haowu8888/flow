import { useEffect, useState } from 'react'
import { api } from '../api'
import { SettingsCard } from '../editor/Panels'
import { usePolling } from '../hooks'
import { hasPerm, type Member, type Pipeline, type PipelineRole } from '../types'

/** 成员信息：公开开关、成员与角色。拥有者不可修改；需要「管理成员」权限才能改动。 */
export function PipelineMembers({ pipeline, onChanged }: { pipeline: Pipeline; onChanged: (p: Pipeline) => void }) {
  const canManage = hasPerm(pipeline, 'manageMembers') && !pipeline.deleted_at
  const { data: members, refresh } = usePolling(() => api.listPipelineMembers(pipeline.id), [pipeline.id], () => null)
  const [roles, setRoles] = useState<PipelineRole[]>([])
  const [team, setTeam] = useState<Member[]>([])
  const [adding, setAdding] = useState({ user: '', role: 'member' })
  const [error, setError] = useState<string>()
  useEffect(() => {
    api.listPipelineRoles().then(setRoles, () => {})
    if (canManage) api.listMembers().then(setTeam, () => {})
  }, [canManage])

  async function act(fn: () => Promise<unknown>) {
    setError(undefined)
    try {
      await fn()
      refresh()
    } catch (e) {
      setError((e as Error).message)
    }
  }
  async function togglePublic(on: boolean) {
    if (on && !confirm('公开后租户内所有人都能在列表里看到这条流水线，并查看运行历史、日志和下载产物（不能运行或修改）。继续？')) return
    setError(undefined)
    try {
      onChanged(await api.setPipelinePublic(pipeline.id, on))
    } catch (e) {
      setError((e as Error).message)
    }
  }

  const assignable = roles.filter((r) => r.id !== 'owner')
  const candidates = team.filter((m) => !members?.some((x) => x.user_id === m.user_id))
  return (
    <SettingsCard
      title="成员信息"
      desc="不公开时只有拥有者、成员和租户管理员能看到这条流水线。成员的权限由角色决定，角色在「账户设置 → 角色管理」里配置；有「管理成员」权限的人才能增删成员、调整角色、设置公开"
    >
      <label className="switch-row">
        <span>
          <span className="strong">公开</span>
          <span className="muted small"> 租户内所有人都能在列表里看到，只能查看运行历史、日志和下载产物，不能运行或修改</span>
        </span>
        <input type="checkbox" className="switch" checked={!!pipeline.is_public} disabled={!canManage} onChange={(e) => togglePublic(e.target.checked)} />
      </label>
      <ul className="member-list">
        {members?.map((m) => (
          <li key={m.user_id}>
            <span className="avatar">{(m.display_name || m.username).slice(0, 2)}</span>
            <span className="member-name">
              <span className="strong">{m.display_name || m.username}</span>
              <span className="muted small">{m.email || `@${m.username}`}</span>
            </span>
            {m.role_id === 'owner' ? (
              <span className="tag">拥有者</span>
            ) : canManage ? (
              <>
                <select value={m.role_id} onChange={(e) => act(() => api.setPipelineMember(pipeline.id, m.user_id, e.target.value))}>
                  {assignable.map((r) => (
                    <option key={r.id} value={r.id}>
                      {r.name}
                    </option>
                  ))}
                </select>
                <button className="icon-btn" title="移除成员" onClick={() => act(() => api.removePipelineMember(pipeline.id, m.user_id))}>
                  ×
                </button>
              </>
            ) : (
              <span className="tag">{roles.find((r) => r.id === m.role_id)?.name ?? m.role_id}</span>
            )}
          </li>
        ))}
      </ul>
      {canManage && (
        <div className="row">
          <select value={adding.user} onChange={(e) => setAdding((a) => ({ ...a, user: e.target.value }))}>
            <option value="">选择要添加的成员</option>
            {candidates.map((m) => (
              <option key={m.user_id} value={m.user_id}>
                {m.display_name || m.username}（@{m.username}）
              </option>
            ))}
          </select>
          <select value={adding.role} onChange={(e) => setAdding((a) => ({ ...a, role: e.target.value }))}>
            {assignable.map((r) => (
              <option key={r.id} value={r.id}>
                {r.name}
              </option>
            ))}
          </select>
          <button
            className="btn"
            disabled={!adding.user}
            onClick={() => act(() => api.setPipelineMember(pipeline.id, adding.user, adding.role)).then(() => setAdding((a) => ({ ...a, user: '' })))}
          >
            添加成员
          </button>
        </div>
      )}
      {error && <div className="alert">{error}</div>}
    </SettingsCard>
  )
}
