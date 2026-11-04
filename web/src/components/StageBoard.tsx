import { fmtDuration } from '../format'
import { isTerminal, type Job, type Status } from '../types'
import { statusLabel } from './StatusBadge'

const icon: Partial<Record<Status, string>> = { success: '✓', failed: '✗', running: '●', canceled: '⊘', waiting: '✋', skipped: '–' }

/** 运行的阶段看板：每个阶段一列（耗时、完成进度），任务卡片列出步骤状态；点任务或步骤打开日志。 */
export function StageBoard({ jobs, source, onOpen }: { jobs: Job[]; source?: { repo: string; branch?: string; commit?: string }; onOpen: (jobId: string, step?: number) => void }) {
  const stages: { name: string; jobs: Job[] }[] = []
  for (const j of [...jobs].sort((a, b) => a.stage_index - b.stage_index || a.seq - b.seq)) {
    const last = stages[stages.length - 1]
    if (last && last.name === j.stage) last.jobs.push(j)
    else stages.push({ name: j.stage, jobs: [j] })
  }
  return (
    <div className="board">
      {source && (
        <div className="board-col source">
          <div className="board-head">流水线源</div>
          <div className="board-body">
            <div className="board-card source-card" title={source.repo}>
              <div className="strong ellipsis">{source.repo}</div>
              {source.branch && <div className="muted small">{source.branch}</div>}
              {source.commit && <div className="mono small link-text">{source.commit.slice(0, 8)}</div>}
            </div>
          </div>
        </div>
      )}
      {stages.map((st, si) => {
        const done = st.jobs.filter((j) => isTerminal(j.status)).length
        const start = st.jobs.map((j) => j.started_at).filter(Boolean).sort()[0]
        const ends = st.jobs.map((j) => j.finished_at)
        const end = done === st.jobs.length ? ends.filter(Boolean).sort().pop() : undefined
        const status = stageStatus(st.jobs)
        return (
          <div key={si} className="board-col">
            <div className={`board-head status-${status}`}>
              <span className="dot" />
              <span className="ellipsis">{st.name}</span>
              {start && <span className="muted small">{fmtDuration(start, end)}</span>}
              <span className="spacer" />
              <span className="muted small">
                {done}/{st.jobs.length}
              </span>
              <div className="board-progress">
                <div style={{ width: `${(done / st.jobs.length) * 100}%` }} />
              </div>
            </div>
            <div className="board-body">
              {st.jobs.map((j) => (
                <div key={j.id} className={`board-card status-${j.status}`}>
                  <button className="board-job" onClick={() => onOpen(j.id)} title="查看日志">
                    <span className={`step-icon${j.status === 'running' ? ' live' : ''}`}>{icon[j.status] ?? '○'}</span>
                    <span className="strong ellipsis">{j.name}</span>
                    <span className="spacer" />
                    <span className="muted small">{j.started_at ? fmtDuration(j.started_at, j.finished_at) : statusLabel[j.status]}</span>
                  </button>
                  {(j.runs_on?.length || j.image) && <div className="muted small board-meta ellipsis">{[j.runs_on?.join(', '), j.image].filter(Boolean).join(' · ')}</div>}
                  {j.steps.length > 0 && !j.deploy && (
                    <ol className="board-steps">
                      {j.steps.map((s, i) => {
                        const ss = j.step_states?.[i]?.status ?? (isTerminal(j.status) && j.status !== 'success' ? 'skipped' : 'pending')
                        return (
                          <li key={i}>
                            <button className={`status-${ss}`} disabled={!j.step_states?.[i]?.started_at} onClick={() => onOpen(j.id, i)}>
                              <span className={`step-icon${ss === 'running' ? ' live' : ''}`}>{icon[ss] ?? '○'}</span>
                              <span className="ellipsis">{s.name || `步骤 ${i + 1}`}</span>
                            </button>
                          </li>
                        )
                      })}
                    </ol>
                  )}
                  {j.deploy && <div className="muted small board-meta">部署到主机组 {j.deploy.group}</div>}
                  {j.status === 'waiting' && (
                    <button className="btn small board-approve" onClick={() => onOpen(j.id)}>
                      去审批
                    </button>
                  )}
                </div>
              ))}
            </div>
          </div>
        )
      })}
    </div>
  )
}

function stageStatus(jobs: Job[]): Status {
  for (const s of ['failed', 'waiting', 'running', 'queued'] as Status[]) if (jobs.some((j) => j.status === s)) return s
  if (jobs.every((j) => j.status === 'success' || j.status === 'skipped')) return jobs.some((j) => j.status === 'success') ? 'success' : 'skipped'
  if (jobs.some((j) => j.status === 'canceled')) return 'canceled'
  return 'pending'
}
