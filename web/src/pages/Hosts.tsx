import { useMemo, useState } from 'react'
import { api } from '../api'
import { useSession } from '../auth'
import { CopyButton } from '../components/CopyButton'
import { Modal } from '../components/Modal'
import { StatusBadge } from '../components/StatusBadge'
import { fmtDuration, fmtTime, shortSha } from '../format'
import { usePolling } from '../hooks'
import { KvEditor } from '../editor/fields'
import { href, navigate, type HostsTab } from '../router'
import type { Deployment, Environment, Host, HostAuthType, HostCheck, HostGroup, HostInput, HostMode } from '../types'
import { installCommand, online } from './Runners'

const tabs: [HostsTab, string][] = [
  ['envs', '环境'],
  ['hosts', '主机'],
  ['deployments', '部署记录'],
]

type Data = { envs: Environment[]; groups: HostGroup[]; hosts: Host[] }

async function load(): Promise<Data> {
  const [envs, groups, hosts] = await Promise.all([api.listEnvironments(), api.listHostGroups(), api.listHosts()])
  return { envs, groups, hosts }
}

/** 环境与主机：部署环境、主机组、服务器与部署历史。 */
export function Hosts({ tab }: { tab: HostsTab }) {
  const { data, error, refresh } = usePolling(load, [], () => null)
  return (
    <section>
      <div className="page-head">
        <h1>环境与主机</h1>
      </div>
      <p className="muted">
        在这里维护部署目标：服务器按<strong>主机组</strong>组织，主机组归属于<strong>环境</strong>（如测试、生产）。流水线中的「主机部署」任务选择主机组，Runner 通过 SSH
        上传产物并执行脚本，或由主机上的 <strong>Agent</strong> 主动拉取执行（无需开放 SSH）；生产等环境可要求部署前审批。
      </p>
      <nav className="tabs">
        {tabs.map(([t, label]) => (
          <a key={t} href={href.hosts(t)} className={tab === t ? 'active' : ''}>
            {label}
            {data && t === 'hosts' && <span className="count"> {data.hosts.length}</span>}
          </a>
        ))}
      </nav>
      {error && <div className="alert">{error}</div>}
      {!data ? (
        !error && <p className="muted">加载中…</p>
      ) : tab === 'envs' ? (
        <EnvironmentsTab data={data} refresh={refresh} />
      ) : tab === 'hosts' ? (
        <HostsTab data={data} refresh={refresh} />
      ) : (
        <DeploymentsTab envs={data.envs} />
      )}
    </section>
  )
}

// ---- 环境 ----

const PRESET_ENVS: Pick<Environment, 'name' | 'description' | 'rank' | 'require_approval'>[] = [
  { name: '开发', description: '开发联调', rank: 1, require_approval: false },
  { name: '测试', description: '测试验证', rank: 2, require_approval: false },
  { name: '预发', description: '上线前验证，配置与生产一致', rank: 3, require_approval: false },
  { name: '生产', description: '线上环境，部署前需审批', rank: 4, require_approval: true },
]

function EnvironmentsTab({ data, refresh }: { data: Data; refresh: () => Promise<void> }) {
  const isAdmin = useSession().can('admin')
  const [editEnv, setEditEnv] = useState<Environment | 'new'>()
  const [editGroup, setEditGroup] = useState<{ group?: HostGroup; envId?: string }>()
  const [busy, setBusy] = useState(false)
  const { data: deployments } = usePolling(() => api.listDeployments({ limit: 200 }), [], () => 15000)
  const ungrouped = data.groups.filter((g) => !g.environment_id || !data.envs.some((e) => e.id === g.environment_id))

  async function initPresets() {
    setBusy(true)
    try {
      for (const e of PRESET_ENVS) await api.saveEnvironment({ ...e, approvers: [] })
      await refresh()
    } catch (e) {
      alert((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      {isAdmin && (
        <div className="row toolbar">
          <button className="btn primary" onClick={() => setEditEnv('new')}>
            新建环境
          </button>
          <button className="btn" onClick={() => setEditGroup({})}>
            新建主机组
          </button>
        </div>
      )}
      {data.envs.length === 0 && (
        <div className="empty">
          <p>还没有环境。环境用来区分部署目标（开发、测试、生产…），并可要求部署前审批。</p>
          {isAdmin && (
            <button className="btn primary" disabled={busy} onClick={initPresets}>
              创建常用环境（开发 / 测试 / 预发 / 生产）
            </button>
          )}
        </div>
      )}
      <div className="env-grid">
        {data.envs.map((env) => (
          <EnvCard
            key={env.id}
            env={env}
            groups={data.groups.filter((g) => g.environment_id === env.id)}
            hosts={data.hosts}
            deployments={deployments?.filter((d) => d.environment_id === env.id)}
            isAdmin={isAdmin}
            onEdit={() => setEditEnv(env)}
            onEditGroup={(group) => setEditGroup({ group, envId: env.id })}
          />
        ))}
        {ungrouped.length > 0 && (
          <EnvCard groups={ungrouped} hosts={data.hosts} isAdmin={isAdmin} onEditGroup={(group) => setEditGroup({ group })} />
        )}
      </div>
      {editEnv && (
        <EnvDialog
          env={editEnv === 'new' ? undefined : editEnv}
          nextRank={Math.max(0, ...data.envs.map((e) => e.rank)) + 1}
          onClose={() => setEditEnv(undefined)}
          onSaved={() => {
            setEditEnv(undefined)
            refresh()
          }}
        />
      )}
      {editGroup && (
        <GroupDialog
          group={editGroup.group}
          envId={editGroup.envId}
          data={data}
          onClose={() => setEditGroup(undefined)}
          onSaved={() => {
            setEditGroup(undefined)
            refresh()
          }}
        />
      )}
    </>
  )
}

/** 一个环境：主机组、主机健康状况、各流水线最近一次部署。env 为空表示未归属环境的主机组。 */
function EnvCard({
  env,
  groups,
  hosts,
  deployments,
  isAdmin,
  onEdit,
  onEditGroup,
}: {
  env?: Environment
  groups: HostGroup[]
  hosts: Host[]
  deployments?: Deployment[]
  isAdmin: boolean
  onEdit?: () => void
  onEditGroup: (g?: HostGroup) => void
}) {
  // 每条流水线在该环境的最近一次部署。
  const latest = useMemo(() => {
    const seen = new Map<string, Deployment>()
    for (const d of deployments ?? []) if (!seen.has(d.pipeline_id)) seen.set(d.pipeline_id, d)
    return [...seen.values()]
  }, [deployments])
  return (
    <div className={`card env-card${env?.require_approval ? ' protected' : ''}`}>
      <div className="env-head">
        <h2>{env ? env.name : '未归属环境'}</h2>
        {env?.require_approval && (
          <span className="tag warn" title={env.approvers?.length ? `审批人：${env.approvers.join('、')}` : '开发者及以上成员可审批'}>
            部署需审批
          </span>
        )}
        <div className="spacer" />
        {isAdmin && env && (
          <button className="btn link small" onClick={onEdit}>
            编辑
          </button>
        )}
      </div>
      {env?.description && <p className="muted small">{env.description}</p>}
      <div className="env-groups">
        {groups.length === 0 && <p className="muted small">没有主机组</p>}
        {groups.map((g) => {
          const members = hosts.filter((h) => h.group_ids.includes(g.id))
          const bad = members.filter((h) => h.check_error).length
          const unknown = members.filter((h) => !h.checked_at).length
          return (
            <button key={g.id} className="env-group" onClick={() => onEditGroup(g)} title={isAdmin ? '编辑主机组' : '查看主机组'}>
              <span className="mono strong">{g.name}</span>
              <span className="muted small">{members.length} 台</span>
              {bad > 0 && <span className="error-text small">{bad} 台连接失败</span>}
              {bad === 0 && unknown > 0 && <span className="muted small">{unknown} 台未测试</span>}
              {members.length > 0 && bad === 0 && unknown === 0 && <span className="ok-text small">● 全部可连接</span>}
            </button>
          )
        })}
        {isAdmin && env && (
          <button className="env-group add" onClick={() => onEditGroup()}>
            + 主机组
          </button>
        )}
      </div>
      {env && (
        <div className="env-deploys">
          <div className="muted small">最近部署</div>
          {deployments === undefined ? (
            <p className="muted small">加载中…</p>
          ) : latest.length === 0 ? (
            <p className="muted small">还没有部署到这个环境</p>
          ) : (
            latest.slice(0, 6).map((d) => (
              <a key={d.id} className="env-deploy" href={href.run(d.run_id)}>
                <StatusBadge status={d.status} />
                <span className="strong">{d.pipeline_name}</span>
                <span className="muted">#{d.run_number}</span>
                {d.commit && <span className="mono muted small">{shortSha(d.commit)}</span>}
                <span className="spacer" />
                <span className="muted small">{fmtTime(d.started_at)}</span>
              </a>
            ))
          )}
        </div>
      )}
    </div>
  )
}

function EnvDialog({ env, nextRank, onClose, onSaved }: { env?: Environment; nextRank: number; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState(env?.name ?? '')
  const [description, setDescription] = useState(env?.description ?? '')
  const [rank, setRank] = useState(env?.rank ?? nextRank)
  const [approval, setApproval] = useState(env?.require_approval ?? false)
  const [approvers, setApprovers] = useState((env?.approvers ?? []).join(', '))
  const [variables, setVariables] = useState<Record<string, string>>(env?.variables ?? {})
  const [removed, setRemoved] = useState<string[]>([])
  const [newSecrets, setNewSecrets] = useState<{ name: string; value: string }[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()

  async function run(fn: () => Promise<unknown>) {
    setBusy(true)
    setError(undefined)
    try {
      await fn()
      onSaved()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }
  const save = () =>
    run(() =>
      api.saveEnvironment(
        {
          name,
          description,
          rank,
          require_approval: approval,
          approvers: approvers.split(/[,，\s]+/).filter(Boolean),
          variables,
          secrets: Object.fromEntries(newSecrets.filter((s) => s.name.trim() && s.value).map((s) => [s.name.trim(), s.value])),
          remove_secrets: removed,
        },
        env?.id,
      ),
    )
  return (
    <Modal
      title={env ? `编辑环境「${env.name}」` : '新建环境'}
      onClose={onClose}
      footer={
        <>
          {env && (
            <button className="btn danger" disabled={busy} onClick={() => confirm(`删除环境「${env.name}」？其主机组将变为未归属环境，部署记录保留。`) && run(() => api.deleteEnvironment(env.id))}>
              删除
            </button>
          )}
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
      <div className="form-grid">
        <label className="field">
          <span>名称</span>
          <input autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="如：测试、生产" />
        </label>
        <label className="field">
          <span>排序</span>
          <input type="number" value={rank} onChange={(e) => setRank(+e.target.value)} />
        </label>
        <label className="field span2">
          <span>说明</span>
          <input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="可选" />
        </label>
        <label className="check span2">
          <input type="checkbox" checked={approval} onChange={(e) => setApproval(e.target.checked)} /> 部署到此环境前需要人工审批
        </label>
        {approval && (
          <label className="field span2">
            <span>审批人（用户名，逗号分隔；留空为开发者及以上成员，管理员始终可审批）</span>
            <input className="mono" value={approvers} onChange={(e) => setApprovers(e.target.value)} placeholder="alice, bob" />
          </label>
        )}
      </div>
      <div className="field">
        <span>环境变量（注入部署到此环境的任务，覆盖流水线中的同名变量）</span>
        <KvEditor value={variables} onChange={setVariables} keyPlaceholder="DB_HOST" valuePlaceholder="db.prod.internal" />
      </div>
      <div className="field">
        <span>环境密钥（加密保存、只写不读，日志中脱敏；优先级高于租户和流水线的同名密钥）</span>
        <div className="env-secrets">
          {(env?.secret_names ?? []).map((n) => (
            <div key={n} className={`row env-secret${removed.includes(n) ? ' removed' : ''}`}>
              <span className="mono">{n}</span>
              <span className="muted small">{removed.includes(n) ? '保存后删除' : '••••••'}</span>
              <div className="spacer" />
              <button type="button" className="btn link small" onClick={() => setRemoved(removed.includes(n) ? removed.filter((x) => x !== n) : [...removed, n])}>
                {removed.includes(n) ? '撤销' : '删除'}
              </button>
            </div>
          ))}
          {newSecrets.map((s, i) => (
            <div key={i} className="row env-secret">
              <input
                className="mono"
                placeholder="DB_PASSWORD"
                value={s.name}
                onChange={(e) => setNewSecrets(newSecrets.map((x, k) => (k === i ? { ...x, name: e.target.value } : x)))}
              />
              <input
                type="password"
                autoComplete="new-password"
                placeholder={env?.secret_names.includes(s.name.trim()) ? '新值（覆盖）' : '值'}
                value={s.value}
                onChange={(e) => setNewSecrets(newSecrets.map((x, k) => (k === i ? { ...x, value: e.target.value } : x)))}
              />
              <button type="button" className="icon-btn" title="移除" onClick={() => setNewSecrets(newSecrets.filter((_, k) => k !== i))}>
                ×
              </button>
            </div>
          ))}
          <button type="button" className="btn link small" onClick={() => setNewSecrets([...newSecrets, { name: '', value: '' }])}>
            + 添加或覆盖密钥
          </button>
        </div>
      </div>
      {error && <div className="alert">{error}</div>}
    </Modal>
  )
}

function GroupDialog({ group, envId, data, onClose, onSaved }: { group?: HostGroup; envId?: string; data: Data; onClose: () => void; onSaved: () => void }) {
  const isAdmin = useSession().can('admin')
  const [name, setName] = useState(group?.name ?? '')
  const [description, setDescription] = useState(group?.description ?? '')
  const [environment, setEnvironment] = useState(group?.environment_id ?? envId ?? '')
  const initial = useMemo(() => new Set(group ? data.hosts.filter((h) => h.group_ids.includes(group.id)).map((h) => h.id) : []), [group, data.hosts])
  const [members, setMembers] = useState(initial)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const { data: users } = usePolling(() => (group ? api.hostGroupUsers(group.id) : Promise.resolve([])), [group?.id], () => null)

  async function save() {
    setBusy(true)
    setError(undefined)
    try {
      const g = await api.saveHostGroup({ name, description, environment_id: environment }, group?.id)
      // 成员关系保存在主机上：只更新有变化的主机。
      for (const h of data.hosts) {
        const want = members.has(h.id)
        if (want === h.group_ids.includes(g.id)) continue
        const ids = want ? [...h.group_ids, g.id] : h.group_ids.filter((x) => x !== g.id)
        await api.saveHost(hostInput(h, ids), h.id)
      }
      onSaved()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }
  async function remove() {
    if (!group || !confirm(`删除主机组「${group.name}」？主机本身不会删除。`)) return
    setBusy(true)
    try {
      await api.deleteHostGroup(group.id)
      onSaved()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }
  return (
    <Modal
      title={group ? `主机组「${group.name}」` : '新建主机组'}
      onClose={onClose}
      wide
      footer={
        isAdmin && (
          <>
            {group && (
              <button className="btn danger" disabled={busy || (users?.length ?? 0) > 0} title={users?.length ? '仍被流水线使用' : undefined} onClick={remove}>
                删除
              </button>
            )}
            <div className="spacer" />
            <button className="btn" onClick={onClose}>
              取消
            </button>
            <button className="btn primary" disabled={busy || !name.trim()} onClick={save}>
              {busy ? '保存中…' : '保存'}
            </button>
          </>
        )
      }
    >
      <fieldset className="form-grid" disabled={!isAdmin}>
        <label className="field">
          <span>名称（在流水线中引用，不含空格）</span>
          <input autoFocus className="mono" value={name} onChange={(e) => setName(e.target.value)} placeholder="如 prod-web" />
        </label>
        <label className="field">
          <span>所属环境</span>
          <select value={environment} onChange={(e) => setEnvironment(e.target.value)}>
            <option value="">不归属环境</option>
            {data.envs.map((e) => (
              <option key={e.id} value={e.id}>
                {e.name}
                {e.require_approval ? '（需审批）' : ''}
              </option>
            ))}
          </select>
        </label>
        <label className="field span2">
          <span>说明</span>
          <input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="可选" />
        </label>
      </fieldset>
      <div className="field">
        <span className="muted small">
          组内主机（{members.size} 台）{data.hosts.length === 0 && '：还没有主机，先到「主机」页添加'}
        </span>
        <div className="host-pick">
          {data.hosts.map((h) => (
            <label key={h.id} className="check">
              <input
                type="checkbox"
                disabled={!isAdmin}
                checked={members.has(h.id)}
                onChange={(e) => {
                  const next = new Set(members)
                  e.target.checked ? next.add(h.id) : next.delete(h.id)
                  setMembers(next)
                }}
              />
              <span className="strong">{h.name}</span>
              <span className="mono muted small">
                {h.username}@{h.address}
              </span>
            </label>
          ))}
        </div>
      </div>
      {users && users.length > 0 && (
        <p className="muted small">
          被以下流水线使用（不能改名或删除）：
          {users.map((u, i) => (
            <span key={u.id}>
              {i > 0 && '、'}
              <a href={href.pipeline(u.id)}>{u.name}</a>
            </span>
          ))}
        </p>
      )}
      {error && <div className="alert">{error}</div>}
    </Modal>
  )
}

const hostInput = (h: Host, groupIds = h.group_ids): HostInput => ({
  name: h.name,
  address: h.address,
  port: h.port,
  username: h.username,
  auth_type: h.auth_type,
  description: h.description,
  group_ids: groupIds,
  jump_host_id: h.jump_host_id,
  mode: h.mode,
})

// ---- 主机 ----

function HostsTab({ data, refresh }: { data: Data; refresh: () => Promise<void> }) {
  const { can } = useSession()
  const isAdmin = can('admin')
  const [q, setQ] = useState('')
  const [groupFilter, setGroupFilter] = useState('')
  const [editing, setEditing] = useState<Host | 'new'>()
  const [checks, setChecks] = useState<Record<string, HostCheck | 'running'>>({})
  const [agent, setAgent] = useState<{ host: Host; token: string }>()
  const [agentError, setAgentError] = useState<string>()
  async function connectAgent(h: Host, confirmReset = true) {
    if (confirmReset && h.agent_seen_at && !confirm(`重新生成「${h.name}」的 Agent 令牌？旧令牌立即失效，需在主机上重新执行接入命令。`)) return
    setAgentError(undefined)
    try {
      setAgent({ host: h, token: (await api.hostAgentToken(h.id)).token })
    } catch (e) {
      setAgentError((e as Error).message)
    }
  }
  const groupName = (id: string) => data.groups.find((g) => g.id === id)?.name ?? id

  const shown = data.hosts.filter(
    (h) =>
      (!groupFilter || (groupFilter === '-' ? h.group_ids.length === 0 : h.group_ids.includes(groupFilter))) &&
      (!q.trim() || `${h.name} ${h.address} ${h.username}`.toLowerCase().includes(q.trim().toLowerCase())),
  )

  async function test(h: Host) {
    setChecks((c) => ({ ...c, [h.id]: 'running' }))
    try {
      const res = await api.testHost(h.id)
      setChecks((c) => ({ ...c, [h.id]: res }))
    } catch (e) {
      setChecks((c) => ({ ...c, [h.id]: { host: h, ok: false, error: (e as Error).message } }))
    }
    await refresh()
  }
  async function testAll() {
    await Promise.all(shown.map(test))
  }

  return (
    <>
      <div className="list-toolbar">
        <input className="list-search" type="search" placeholder="搜索名称、地址或用户" value={q} onChange={(e) => setQ(e.target.value)} />
        <select value={groupFilter} onChange={(e) => setGroupFilter(e.target.value)}>
          <option value="">全部主机组</option>
          {data.groups.map((g) => (
            <option key={g.id} value={g.id}>
              {g.name}
            </option>
          ))}
          <option value="-">未加入主机组</option>
        </select>
        <div className="spacer" />
        {can('developer') && shown.length > 0 && (
          <button className="btn" onClick={testAll}>
            测试全部连接
          </button>
        )}
        {isAdmin && (
          <button className="btn primary" onClick={() => setEditing('new')}>
            添加主机
          </button>
        )}
      </div>
      {agentError && <div className="alert">{agentError}</div>}
      {data.hosts.length === 0 ? (
        <div className="empty">
          <p>还没有主机。添加服务器的 SSH 地址与登录凭据（私钥或密码，加密保存），再加入主机组即可在流水线中部署。</p>
          {isAdmin && (
            <button className="btn primary" onClick={() => setEditing('new')}>
              添加第一台主机
            </button>
          )}
        </div>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>名称</th>
                <th>接入方式</th>
                <th>主机组</th>
                <th>连接状态</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {shown.map((h) => {
                const c = checks[h.id]
                return (
                  <tr key={h.id}>
                    <td>
                      <span className="strong">{h.name}</span>
                      {h.description && <div className="muted small">{h.description}</div>}
                    </td>
                    {h.mode === 'agent' ? (
                      <td className="small">
                        <span className={`badge ${online(h.agent_seen_at) ? 'status-success' : 'status-skipped'}`}>
                          <span className="dot" />
                          Agent {online(h.agent_seen_at) ? '在线' : h.agent_seen_at ? '离线' : '未接入'}
                        </span>
                        {h.agent_info?.hostname && (
                          <div className="muted mono">
                            {h.agent_info.hostname} · {h.agent_info.os}/{h.agent_info.arch}
                          </div>
                        )}
                      </td>
                    ) : (
                      <td className="mono small">
                        {h.username}@{h.address}
                        {h.port !== 22 && `:${h.port}`}
                        <div className="muted">
                          {h.auth_type === 'key' ? '私钥认证' : '密码认证'}
                          {h.jump_host_id && ` · 经跳板机 ${data.hosts.find((x) => x.id === h.jump_host_id)?.name ?? '?'}`}
                        </div>
                      </td>
                    )}
                    <td>
                      {h.group_ids.length === 0 ? <span className="muted small">—</span> : h.group_ids.map((id) => <span key={id} className="tag mono">{groupName(id)}</span>)}
                    </td>
                    <td className="small host-status">
                      {c === 'running' ? (
                        <span className="muted">测试中…</span>
                      ) : !h.checked_at ? (
                        <span className="muted">未测试</span>
                      ) : h.check_error ? (
                        <span className="error-text" title={h.check_error}>
                          ✗ {h.check_error.length > 60 ? h.check_error.slice(0, 60) + '…' : h.check_error}
                        </span>
                      ) : (
                        <span className="ok-text" title={c?.output}>
                          ✓ 可连接 <span className="muted">{fmtTime(h.checked_at)}</span>
                        </span>
                      )}
                      {c && c !== 'running' && c.ok && c.output && <div className="muted mono">{c.output.split('\n')[0]}</div>}
                      {h.host_key && <div className="muted" title={`主机公钥已信任（${h.host_key.split(' ')[0]}）`}>🔒 指纹已记录</div>}
                    </td>
                    <td className="actions">
                      {can('developer') && (
                        <button className="btn small" disabled={c === 'running'} onClick={() => test(h)}>
                          测试连接
                        </button>
                      )}
                      {isAdmin && h.mode === 'agent' && (
                        <button className="btn link small" onClick={() => connectAgent(h)}>
                          接入命令
                        </button>
                      )}
                      {isAdmin && (
                        <button className="btn link small" onClick={() => setEditing(h)}>
                          编辑
                        </button>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
      {editing && (
        <HostDialog
          host={editing === 'new' ? undefined : editing}
          groups={data.groups}
          hosts={data.hosts}
          onClose={() => setEditing(undefined)}
          onDeleted={() => {
            setEditing(undefined)
            refresh()
          }}
          onSaved={async (h, created) => {
            setEditing(undefined)
            await refresh()
            if (created && h.mode === 'agent') connectAgent(h, false)
            else if (created) test(h) // 新主机立即测试连接并记录指纹
          }}
        />
      )}
      {agent && <AgentDialog {...agent} onClose={() => (setAgent(undefined), refresh())} />}
    </>
  )
}

function HostDialog({
  host,
  groups,
  hosts,
  onClose,
  onSaved,
  onDeleted,
}: {
  host?: Host
  groups: HostGroup[]
  hosts: Host[]
  onClose: () => void
  onSaved: (h: Host, created: boolean) => void
  onDeleted: () => void
}) {
  const [form, setForm] = useState<HostInput>(
    host ? hostInput(host) : { mode: 'ssh', name: '', address: '', port: 22, username: 'root', auth_type: 'key', credential: '', description: '', group_ids: [] },
  )
  const isAgent = form.mode === 'agent'

  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const set = <K extends keyof HostInput>(k: K, v: HostInput[K]) => setForm((f) => ({ ...f, [k]: v }))
  const authChanged = !!host && form.auth_type !== host.auth_type

  async function run(fn: () => Promise<void>) {
    setBusy(true)
    setError(undefined)
    try {
      await fn()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }
  const save = () => run(async () => onSaved(await api.saveHost(form, host?.id), !host))
  const remove = () =>
    host && confirm(`删除主机「${host.name}」？`) && run(async () => {
      await api.deleteHost(host.id)
      onDeleted()
    })
  const resetKey = () =>
    host && confirm('清除已信任的主机公钥？下次连接测试会重新记录。仅在确认主机重装过时操作。') && run(async () => {
      onSaved(await api.resetHostKey(host.id), false)
    })

  return (
    <Modal
      title={host ? `编辑主机「${host.name}」` : '添加主机'}
      onClose={onClose}
      wide
      footer={
        <>
          {host && (
            <button className="btn danger" disabled={busy} onClick={remove}>
              删除
            </button>
          )}
          {host?.host_key && (
            <button className="btn" disabled={busy} onClick={resetKey}>
              重置指纹
            </button>
          )}
          <div className="spacer" />
          <button className="btn" onClick={onClose}>
            取消
          </button>
          <button
            className="btn primary"
            disabled={busy || (isAgent ? !form.name.trim() : !form.address.trim() || (!host && !form.credential) || (authChanged && !form.credential))}
            onClick={save}
          >
            {busy ? '保存中…' : host ? '保存' : isAgent ? '添加并生成接入命令' : '添加并测试连接'}
          </button>
        </>
      }
    >
      {!host && (
        <div className="kind-cards">
          {(
            [
              ['ssh', 'SSH 连接', 'Runner 通过 SSH 登录主机上传并执行，需要 Runner 能访问主机的 SSH 端口'],
              ['agent', 'Agent 接入', '在主机上执行一条命令安装 Agent，主动向 Flow 拉取部署任务；主机无需公网 IP 或开放 SSH'],
            ] as [HostMode, string, string][]
          ).map(([m, l, d]) => (
            <label key={m} className={`radio-card${form.mode === m ? ' active' : ''}`}>
              <input type="radio" checked={form.mode === m} onChange={() => set('mode', m)} />
              <span>
                <span className="strong">{l}</span>
                <span className="muted small">{d}</span>
              </span>
            </label>
          ))}
        </div>
      )}
      {isAgent ? (
        <div className="form-grid">
          <label className="field">
            <span>名称 *</span>
            <input autoFocus value={form.name} onChange={(e) => set('name', e.target.value)} placeholder="如 edge-1" />
          </label>
          <label className="field">
            <span>说明</span>
            <input value={form.description} onChange={(e) => set('description', e.target.value)} placeholder="可选" />
          </label>
          <div className="field span2">
            <span>主机组</span>
            {groups.length === 0 ? (
              <span className="muted small">还没有主机组，可在「环境」页新建后再加入。</span>
            ) : (
              <div className="host-pick inline">
                {groups.map((g) => (
                  <label key={g.id} className="check">
                    <input
                      type="checkbox"
                      checked={form.group_ids.includes(g.id)}
                      onChange={(e) => set('group_ids', e.target.checked ? [...form.group_ids, g.id] : form.group_ids.filter((x) => x !== g.id))}
                    />
                    <span className="mono">{g.name}</span>
                  </label>
                ))}
              </div>
            )}
          </div>
        </div>
      ) : (
      <div className="form-grid">
        <label className="field">
          <span>地址（IP 或域名）</span>
          <input autoFocus className="mono" value={form.address} onChange={(e) => set('address', e.target.value)} placeholder="10.0.0.12" />
        </label>
        <label className="field">
          <span>SSH 端口</span>
          <input type="number" value={form.port} onChange={(e) => set('port', +e.target.value)} />
        </label>
        <label className="field">
          <span>名称（留空取地址）</span>
          <input value={form.name} onChange={(e) => set('name', e.target.value)} placeholder="如 web-1" />
        </label>
        <label className="field">
          <span>登录用户</span>
          <input className="mono" value={form.username} onChange={(e) => set('username', e.target.value)} />
        </label>
        <div className="field span2">
          <span>认证方式</span>
          <div className="segmented">
            {(
              [
                ['key', 'SSH 私钥（推荐）'],
                ['password', '密码'],
              ] as [HostAuthType, string][]
            ).map(([k, l]) => (
              <button key={k} type="button" className={form.auth_type === k ? 'active' : ''} onClick={() => set('auth_type', k)}>
                {l}
              </button>
            ))}
          </div>
        </div>
        <label className="field span2">
          <span>
            {form.auth_type === 'key' ? '私钥（OpenSSH / PEM 格式，不支持带口令的私钥）' : '密码'}
            {host && !authChanged && '：留空保持不变'}
          </span>
          {form.auth_type === 'key' ? (
            <textarea
              className="mono small-text"
              rows={5}
              spellCheck={false}
              value={form.credential}
              onChange={(e) => set('credential', e.target.value)}
              placeholder={host && !authChanged ? '已保存，留空保持不变' : '-----BEGIN OPENSSH PRIVATE KEY-----\n…'}
            />
          ) : (
            <input type="password" autoComplete="new-password" value={form.credential} onChange={(e) => set('credential', e.target.value)} placeholder={host && !authChanged ? '已保存，留空保持不变' : ''} />
          )}
        </label>
        <div className="field span2">
          <span>主机组</span>
          {groups.length === 0 ? (
            <span className="muted small">还没有主机组，可在「环境」页新建后再加入。</span>
          ) : (
            <div className="host-pick inline">
              {groups.map((g) => (
                <label key={g.id} className="check">
                  <input
                    type="checkbox"
                    checked={form.group_ids.includes(g.id)}
                    onChange={(e) => set('group_ids', e.target.checked ? [...form.group_ids, g.id] : form.group_ids.filter((x) => x !== g.id))}
                  />
                  <span className="mono">{g.name}</span>
                </label>
              ))}
            </div>
          )}
        </div>
        <label className="field">
          <span>跳板机（可选）</span>
          <select value={form.jump_host_id ?? ''} onChange={(e) => set('jump_host_id', e.target.value || undefined)}>
            <option value="">直接连接</option>
            {hosts
              .filter((x) => x.id !== host?.id && !x.jump_host_id)
              .map((x) => (
                <option key={x.id} value={x.id}>
                  经 {x.name}（{x.username}@{x.address}）
                </option>
              ))}
          </select>
          <small className="muted">Runner 无法直连目标主机时，经由堡垒机转发（等同 ssh -J）</small>
        </label>
        <label className="field">
          <span>说明</span>
          <input value={form.description} onChange={(e) => set('description', e.target.value)} placeholder="可选，如：华东 2 区 · 4C8G" />
        </label>
      </div>
      )}
      {isAgent ? (
        <p className="muted small">部署时执行任务的 Runner 把上传内容与脚本经 Flow 转交给 Agent，Agent 在主机上解压到目标目录并执行，日志实时回传。</p>
      ) : (
      <p className="muted small">凭据加密保存，只在部署时随任务下发给 Runner，接口和日志中都不会出现。连接测试从 Flow 服务端发起，首次成功时记录主机公钥，之后部署会校验，防止中间人。</p>
      )}
      {error && <div className="alert">{error}</div>}
    </Modal>
  )
}

function AgentDialog({ host, token, onClose }: { host: Host; token: string; onClose: () => void }) {
  const cmd = installCommand(token, true)
  return (
    <Modal
      wide
      title={`接入主机「${host.name}」`}
      onClose={onClose}
      footer={
        <>
          <div className="spacer" />
          <button className="btn primary" onClick={onClose}>
            完成
          </button>
        </>
      }
    >
      <div className="conn-form">
        <div className="field">
          <span>在主机上以 root（或有 sudo 权限的用户）执行</span>
          <div className="row">
            <code className="block grow">{cmd}</code> <CopyButton text={cmd} />
          </div>
          <span className="muted small">安装 flow-runner 并注册为 flow-agent 服务（systemd），开机自启；令牌只显示这一次</span>
        </div>
        <p className="muted small">执行完成后点「测试连接」确认 Agent 在线。部署脚本以 Agent 进程的用户身份执行。</p>
      </div>
    </Modal>
  )
}

// ---- 部署记录 ----

/** 部署记录；pipelineId 非空时只看该流水线的（流水线详情的「部署历史」）。 */
export function DeploymentsTab({ envs, pipelineId }: { envs: Environment[]; pipelineId?: string }) {
  const canDeploy = useSession().can('developer')
  const [env, setEnv] = useState('')
  const [busy, setBusy] = useState<string>()
  async function redeploy(d: Deployment) {
    if (!confirm(`把「${d.environment_name || d.group_name}」重新部署为 ${d.pipeline_name} #${d.run_number} 的版本？\n只执行部署任务 ${d.job_name}，沿用当时的配置、参数与构建产物，不重新构建。`)) return
    setBusy(d.id)
    try {
      const res = await api.redeploy(d.run_id, d.job_name)
      navigate(href.run(res.run.id))
    } catch (e) {
      alert((e as Error).message)
      setBusy(undefined)
    }
  }
  const { data, error } = usePolling(() => api.listDeployments({ environment: env, pipeline: pipelineId, limit: 200 }), [env, pipelineId], (d) =>
    d?.some((x) => x.status === 'running') ? 3000 : 15000,
  )
  return (
    <>
      <div className="list-toolbar">
        <div className="segmented">
          <button className={env === '' ? 'active' : ''} onClick={() => setEnv('')}>
            全部环境
          </button>
          {envs.map((e) => (
            <button key={e.id} className={env === e.id ? 'active' : ''} onClick={() => setEnv(e.id)}>
              {e.name}
            </button>
          ))}
        </div>
      </div>
      {error && <div className="alert">{error}</div>}
      {data && data.length === 0 && <p className="muted">还没有部署记录。流水线中的「主机部署」任务执行后会记录在这里。</p>}
      {data && data.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>状态</th>
                <th>流水线</th>
                <th>环境 / 主机组</th>
                <th>代码</th>
                <th>开始时间</th>
                <th>耗时</th>
                <th>触发人</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {data.map((d) => (
                <tr key={d.id}>
                  <td>
                    <StatusBadge status={d.status} />
                  </td>
                  <td>
                    <a href={href.run(d.run_id)} className="strong">
                      {d.pipeline_name} #{d.run_number}
                    </a>
                    <div className="muted small">{d.job_name}</div>
                  </td>
                  <td>
                    {d.environment_name ?? <span className="muted">—</span>} / <span className="mono">{d.group_name}</span>
                    <div className="muted small" title={d.hosts.join('\n')}>
                      {d.hosts.length} 台：{d.hosts.slice(0, 3).join('、')}
                      {d.hosts.length > 3 && ' …'}
                    </div>
                  </td>
                  <td className="mono small">{d.commit ? shortSha(d.commit) : <span className="muted">—</span>}</td>
                  <td className="muted">{fmtTime(d.started_at)}</td>
                  <td className="muted">{fmtDuration(d.started_at, d.finished_at)}</td>
                  <td className="muted">{d.created_by || '自动'}</td>
                  <td className="actions">
                    {canDeploy && d.status === 'success' && (
                      <button className="btn small" disabled={busy === d.id} onClick={() => redeploy(d)} title="回退或重放到这个版本">
                        重新部署此版本
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}
