import { useState } from 'react'
import { api } from '../api'
import { fmtTime, shortSha, triggerLabel } from '../format'
import { usePolling } from '../hooks'
import { href } from '../router'
import type { Approval } from '../types'

/** 待我审批：等待当前用户审批的人工卡点与部署。 */
export function Approvals() {
  const { data, error, refresh } = usePolling(api.listApprovals, [], () => 10000)
  return (
    <section>
      <div className="page-head">
        <h1>待我审批</h1>
      </div>
      {error && <div className="alert">{error}</div>}
      {!data ? (
        !error && <p className="muted">加载中…</p>
      ) : data.length === 0 ? (
        <div className="empty">
          <p className="muted">没有等待你审批的任务。</p>
        </div>
      ) : (
        <div className="approval-list">
          {data.map((a) => (
            <ApprovalCard key={a.job.id} a={a} onDone={refresh} />
          ))}
        </div>
      )}
    </section>
  )
}

function ApprovalCard({ a, onDone }: { a: Approval; onDone: () => Promise<void> }) {
  const [comment, setComment] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const deploy = a.job.deploy
  async function decide(approve: boolean) {
    if (!approve && !confirm('驳回后本次运行将失败，确定驳回？')) return
    setBusy(true)
    setError(undefined)
    try {
      await api.approve(a.job.id, approve, comment.trim())
      await onDone()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }
  return (
    <div className={`card approval-card${deploy ? ' deploy' : ''}`}>
      <div className="row">
        <span className={`tag${deploy ? ' warn' : ''}`}>{deploy ? `部署到 ${a.environment ?? deploy.group}` : '人工卡点'}</span>
        <a className="strong" href={href.run(a.run.id)}>
          {a.pipeline_name} #{a.run.number}
        </a>
        <span className="muted">· {a.job.name}</span>
        <div className="spacer" />
        <span className="muted small">等待自 {fmtTime(a.job.started_at ?? a.job.created_at)}</span>
      </div>
      <div className="muted small">
        {triggerLabel(a.run.trigger, a.run.env, a.run.created_by)}
        {a.run.commit && <> · 提交 <span className="mono">{shortSha(a.run.commit)}</span></>}
        {a.run.pipeline_version ? ` · 配置 v${a.run.pipeline_version}` : ''}
        {deploy && <> · 主机组 <span className="mono">{deploy.group}</span></>}
        {a.job.approvers?.length ? ` · 审批人 ${a.job.approvers.join('、')}` : ''}
      </div>
      <div className="row">
        <input className="grow" value={comment} onChange={(e) => setComment(e.target.value)} placeholder="审批意见（可选）" />
        <button className="btn primary" disabled={busy} onClick={() => decide(true)}>
          {deploy ? '批准部署' : '通过'}
        </button>
        <button className="btn danger" disabled={busy} onClick={() => decide(false)}>
          驳回
        </button>
      </div>
      {error && <div className="alert subtle">{error}</div>}
    </div>
  )
}
