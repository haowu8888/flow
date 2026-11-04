import { useEffect, useState } from 'react'
import { api } from '../api'
import { useSession } from '../auth'
import { CopyButton } from '../components/CopyButton'
import { Modal } from '../components/Modal'
import { fmtTime } from '../format'
import { usePolling } from '../hooks'
import type { NodeInput, NodeKind, RunnerInfo } from '../types'

const ONLINE_MS = 60_000

const kindLabels: Record<NodeKind, string> = { self: '自建节点', github: 'GitHub 节点', agent: '主机 Agent' }

export const online = (seen?: string) => !!seen && Date.now() - new Date(seen).getTime() < ONLINE_MS

/** 安装命令：在机器上执行即下载 flow-runner 并注册为系统服务。 */
export function installCommand(token: string, agent = false) {
  return `curl -fsSL ${window.location.origin}/api/runner/install.sh | FLOW_RUNNER_TOKEN=${token} sh${agent ? ' -s -- agent' : ''}`
}

type Created = RunnerInfo & { token: string }

export function Runners() {
  const { can } = useSession()
  const { data, error, refresh } = usePolling(() => api.listRunners(), [], () => 10000)
  const [tab, setTab] = useState<'' | NodeKind>('')
  const [editing, setEditing] = useState<RunnerInfo | 'new'>()
  const [created, setCreated] = useState<Created>()
  const [actionError, setActionError] = useState<string>()
  const nodes = (data ?? []).filter((r) => r.kind !== 'agent')
  const rows = nodes.filter((r) => !tab || r.kind === tab)

  async function remove(r: RunnerInfo) {
    if (!confirm(`确定删除构建节点 ${r.name}？其令牌立即失效，正在执行的任务会因租约过期而失败。`)) return
    setActionError(undefined)
    try {
      await api.deleteRunner(r.id)
      refresh()
    } catch (e) {
      setActionError((e as Error).message)
    }
  }

  async function resetToken(r: RunnerInfo) {
    if (!confirm(`重置 ${r.name} 的令牌？旧令牌立即失效，需要用新令牌重新接入。`)) return
    setActionError(undefined)
    try {
      setCreated(await api.resetRunnerToken(r.id))
    } catch (e) {
      setActionError((e as Error).message)
    }
  }

  return (
    <section>
      <div className="page-head">
        <h1>构建节点</h1>
        <div className="spacer" />
        {can('admin') && (
          <button className="btn primary" onClick={() => setEditing('new')}>
            添加节点
          </button>
        )}
      </div>
      <p className="muted">
        节点主动向 Flow 拉取任务，可放在内网。任务的「构建机器」选择 <code>node:节点名称</code> 即只在该节点执行，或用标签匹配多台节点；
        部署目标主机在「环境与主机」中以 SSH 或 Agent 接入。
      </p>
      <div className="pills type-tabs">
        {(['', 'self', 'github'] as const).map((k) => (
          <button key={k} className={tab === k ? 'active' : ''} onClick={() => setTab(k)}>
            {k ? kindLabels[k] : '全部'} <span className="count">{k ? nodes.filter((r) => r.kind === k).length : nodes.length}</span>
          </button>
        ))}
      </div>
      {(error || actionError) && <div className="alert">{error ?? actionError}</div>}

      {data && rows.length === 0 ? (
        <div className="empty">
          <p className="strong">还没有构建节点</p>
          <p className="muted small">添加一台自建节点（一条命令接入），或使用 GitHub Actions 托管的机器按需执行任务</p>
        </div>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>节点名称</th>
                <th>类型</th>
                <th>状态</th>
                <th>并发</th>
                <th>系统 / 主机名</th>
                <th>版本</th>
                <th>最近心跳</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => {
                const on = online(r.last_seen_at)
                return (
                  <tr key={r.id}>
                    <td>
                      <span className="strong">{r.name}</span>
                      <div>
                        {(r.labels ?? []).map((l) => (
                          <span key={l} className="tag">
                            {l}
                          </span>
                        ))}
                      </div>
                    </td>
                    <td>
                      {kindLabels[r.kind] ?? r.kind}
                      {r.kind === 'github' && r.config && <div className="muted small mono">{r.config.repo}</div>}
                    </td>
                    <td>
                      {r.kind === 'github' && !on ? (
                        <span className="badge status-pending" title="任务排队时触发工作流，执行完即释放">
                          <span className="dot" />
                          按需
                        </span>
                      ) : (
                        <span className={`badge ${on ? 'status-success' : 'status-skipped'}`}>
                          <span className="dot" />
                          {on ? '在线' : '离线'}
                        </span>
                      )}
                    </td>
                    <td>{r.info?.concurrency || '—'}</td>
                    <td className="muted">
                      {r.info?.os ? `${r.info.os}/${r.info.arch}` : '—'}
                      {r.info?.hostname && <div className="small mono">{r.info.hostname}</div>}
                    </td>
                    <td className="muted mono small">{r.info?.version || '—'}</td>
                    <td className="muted">{r.last_seen_at ? fmtTime(r.last_seen_at) : '从未连接'}</td>
                    <td className="actions nowrap">
                      {can('admin') && (
                        <>
                          <button className="btn link small" onClick={() => setEditing(r)}>
                            编辑
                          </button>
                          <button className="btn link small" onClick={() => resetToken(r)}>
                            重置令牌
                          </button>
                          <button className="btn link small danger-text" onClick={() => remove(r)}>
                            删除
                          </button>
                        </>
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
        <NodeDialog
          node={editing === 'new' ? undefined : editing}
          onClose={() => setEditing(undefined)}
          onSaved={(c) => {
            setEditing(undefined)
            if (c) setCreated(c)
            refresh()
          }}
        />
      )}
      {created && <TokenDialog node={created} onClose={() => (setCreated(undefined), refresh())} />}
    </section>
  )
}

function NodeDialog({ node, onClose, onSaved }: { node?: RunnerInfo; onClose: () => void; onSaved: (created?: Created) => void }) {
  const [kind, setKind] = useState<NodeKind>(node?.kind ?? 'self')
  const [form, setForm] = useState<NodeInput>({
    name: node?.name ?? '',
    repo: node?.config?.repo ?? '',
    workflow: node?.config?.workflow ?? 'flow-runner.yml',
    ref: node?.config?.ref ?? 'main',
    token: '',
  })
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string>()
  const set = (k: keyof NodeInput) => (e: React.ChangeEvent<HTMLInputElement>) => setForm((f) => ({ ...f, [k]: e.target.value }))

  async function save() {
    setBusy(true)
    setErr(undefined)
    try {
      if (node) {
        await api.updateRunner(node.id, { ...form, kind })
        onSaved()
      } else {
        onSaved(await api.registerRunner({ ...form, kind }))
      }
    } catch (e) {
      setErr((e as Error).message)
      setBusy(false)
    }
  }

  return (
    <Modal
      wide
      title={node ? `编辑节点「${node.name}」` : '添加构建节点'}
      onClose={onClose}
      footer={
        <>
          {err && <span className="error-text small">{err}</span>}
          <div className="spacer" />
          <button className="btn" onClick={onClose}>
            取消
          </button>
          <button className="btn primary" disabled={busy || !form.name.trim() || (kind === 'github' && (!form.repo || (!node && !form.token)))} onClick={save}>
            {node ? '保存' : '添加'}
          </button>
        </>
      }
    >
      {!node && (
        <div className="kind-cards">
          {(['self', 'github'] as const).map((k) => (
            <label key={k} className={`radio-card${kind === k ? ' active' : ''}`}>
              <input type="radio" checked={kind === k} onChange={() => setKind(k)} />
              <span>
                <span className="strong">{kindLabels[k]}</span>
                <span className="muted small">
                  {k === 'self' ? '在你的服务器、开发机或内网机器上执行一条命令接入，常驻并行执行任务' : '使用 GitHub Actions 托管的机器：任务排队时触发仓库中的工作流，执行完即释放'}
                </span>
              </span>
            </label>
          ))}
        </div>
      )}
      <div className="conn-form">
        <label className="field">
          <span>节点名称 *</span>
          <input autoFocus maxLength={64} value={form.name} onChange={set('name')} placeholder="例如 build-01，任务里用 node:build-01 指定" />
        </label>
        {kind === 'github' && (
          <>
            <label className="field">
              <span>仓库 *</span>
              <input className="mono" value={form.repo} onChange={set('repo')} placeholder="owner/name，用来运行工作流的仓库" />
            </label>
            <label className="field">
              <span>工作流文件</span>
              <input className="mono" value={form.workflow} onChange={set('workflow')} placeholder="flow-runner.yml" />
            </label>
            <label className="field">
              <span>分支</span>
              <input className="mono" value={form.ref} onChange={set('ref')} placeholder="main" />
            </label>
            <label className="field">
              <span>GitHub 访问令牌{node ? '' : ' *'}</span>
              <input
                className="mono"
                type="password"
                autoComplete="off"
                value={form.token}
                onChange={set('token')}
                placeholder={node ? '留空保持不变' : 'Fine-grained token，对该仓库授予 Actions: Read and write'}
              />
              <span className="muted small">只用于触发工作流（workflow_dispatch），加密保存</span>
            </label>
          </>
        )}
      </div>
    </Modal>
  )
}

function TokenDialog({ node, onClose }: { node: Created; onClose: () => void }) {
  const [workflow, setWorkflow] = useState<string>()
  useEffect(() => {
    if (node.kind === 'github') api.runnerWorkflow(node.id).then(setWorkflow, () => {})
  }, [node])
  const cmd = installCommand(node.token)
  const manual = `FLOW_SERVER=${window.location.origin} FLOW_RUNNER_TOKEN=${node.token} flow-runner -labels linux -concurrency 2`
  return (
    <Modal
      wide
      title={`接入节点「${node.name}」`}
      onClose={onClose}
      footer={
        <>
          <div className="spacer" />
          <button className="btn primary" onClick={onClose}>
            我已保存
          </button>
        </>
      }
    >
      <div className="conn-form">
        <div className="field">
          <span>节点令牌（只显示这一次）</span>
          <div className="row">
            <code className="block grow">{node.token}</code> <CopyButton text={node.token} />
          </div>
        </div>
        {node.kind === 'self' ? (
          <>
            <div className="field">
              <span>在节点机器上执行（Linux / macOS，自动注册为 systemd 服务）</span>
              <div className="row">
                <code className="block grow">{cmd}</code> <CopyButton text={cmd} />
              </div>
              <span className="muted small">可加 FLOW_RUNNER_LABELS=docker,linux、FLOW_CONCURRENCY=2 设置标签与并发</span>
            </div>
            <details className="hint">
              <summary>手动启动</summary>
              <div className="row">
                <code className="block grow">{manual}</code> <CopyButton text={manual} />
              </div>
              <p className="muted small">
                二进制可从 <code>{window.location.origin}/api/runner/download/linux/amd64</code> 下载（os：linux / darwin，arch：amd64 / arm64）。
              </p>
            </details>
          </>
        ) : (
          <>
            <ol className="small steps-list">
              <li>
                在仓库 <span className="mono">{node.config?.repo}</span> 的 Settings → Secrets and variables → Actions 中新增 Secret{' '}
                <code>FLOW_RUNNER_TOKEN</code>，值为上面的令牌
              </li>
              <li>
                把下面的文件提交到 <code>.github/workflows/{node.config?.workflow}</code>（分支 {node.config?.ref}）
              </li>
              <li>
                流水线任务的「构建机器」选择 <code>node:{node.name}</code>，任务排队时 Flow 自动触发工作流
              </li>
            </ol>
            {workflow && (
              <div className="field">
                <div className="row">
                  <span className="grow">工作流文件</span>
                  <CopyButton text={workflow} />
                </div>
                <pre className="code-block">{workflow}</pre>
              </div>
            )}
          </>
        )}
      </div>
    </Modal>
  )
}
