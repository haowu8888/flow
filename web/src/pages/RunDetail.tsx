import { Fragment, useEffect, useMemo, useRef, useState } from 'react'
import { parse as parseYaml } from 'yaml'
import { api } from '../api'
import { useSession } from '../auth'
import { Icon } from '../components/Icon'
import { StageBoard } from '../components/StageBoard'
import { LogViewer, type LogViewerHandle } from '../components/LogViewer'
import { Modal } from '../components/Modal'
import { TestReports } from '../components/TestReports'
import { TriggerDetails } from '../components/TriggerDetails'
import { StatusBadge, statusLabel } from '../components/StatusBadge'
import { commitURL, fmtDuration, fmtExpiry, fmtSize, fmtTime, shortSha, triggerLabel } from '../format'
import { usePolling, useRunStream, useTicker } from '../hooks'
import { href, navigate } from '../router'
import { hasPerm, isTerminal, type Job, type Pipeline, type Run, type Status } from '../types'

/** 自动选中最值得看的 Job：待审批的 > 失败的 > 运行中的 > 当前选中的 > 第一个。 */
function pickDefault(jobs: Job[], current?: string) {
  return (
    jobs.find((j) => j.status === 'waiting') ??
    jobs.find((j) => j.status === 'failed') ??
    jobs.find((j) => j.status === 'running') ??
    jobs.find((j) => j.id === current) ??
    jobs[0]
  )?.id
}

/** 排队原因：并发组名额已满，或任务在等构建节点领取。 */
function QueueNotice({ run, jobs }: { run: Run; jobs: Job[] }) {
  const waiting = run.status === 'queued' || jobs.some((j) => j.status === 'queued')
  const key = jobs.map((j) => j.status).join() + run.status
  const { data: qw } = usePolling(() => (waiting ? api.runQueue(run.id) : Promise.resolve(null)), [run.id, waiting, key], () => (waiting ? 5000 : null))
  if (!waiting || !qw) return null
  if (qw.group) {
    const scope = qw.group.startsWith('pipeline/') ? '本流水线' : `并发组 ${qw.group}`
    return (
      <div className="card waiting-notice" title="上限可在编辑页「触发设置 / 并发」调整">
        排队中：{scope}并发已满 {qw.running}/{qw.limit}
        {qw.ahead ? `，前面还有 ${qw.ahead} 个排队` : ''}，等正在执行的运行结束后自动开始。
      </div>
    )
  }
  return (
    <>
      {qw.jobs?.map((j) => (
        <div key={j.job} className="card waiting-notice">
          「{j.job}」{j.reason}
        </div>
      ))}
    </>
  )
}

/** embedded：嵌在流水线详情「最近运行」中，不显示面包屑与流水线名。 */
export function RunDetail({ id, embedded }: { id: string; embedded?: boolean }) {
  const { data, error, refresh } = useRunStream(id)
  const [pipeline, setPipeline] = useState<Pipeline>()
  const [selected, setSelected] = useState<string>()
  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState<string>()
  const { can } = useSession()
  const active = !!data && !isTerminal(data.run.status)
  useTicker(active)
  const { data: artifacts, refresh: refreshArtifacts } = usePolling(() => api.listArtifacts(id), [id], () => null)
  // 产物在 Job 结束前上传，任一 Job 状态变化时重新拉取即可。
  const { data: reports, refresh: refreshReports } = usePolling(() => api.listTestReports(id), [id], () => null)
  const jobStates = data?.jobs.map((j) => j.status).join()
  useEffect(() => {
    if (jobStates) {
      refreshArtifacts()
      refreshReports()
    }
  }, [jobStates, refreshArtifacts, refreshReports])

  const pipelineId = data?.run.pipeline_id
  useEffect(() => {
    if (pipelineId) api.getPipeline(pipelineId).then(setPipeline, () => {})
  }, [pipelineId])
  const logView = useRef<LogViewerHandle>(null)
  const [rollbackOf, setRollbackOf] = useState<Run>()
  const [confirmRollback, setConfirmRollback] = useState(false)
  useEffect(() => setConfirmRollback(false), [id])
  const sourceId = data?.run.rollback_of
  useEffect(() => {
    setRollbackOf(undefined)
    if (sourceId) api.getRun(sourceId).then((d) => setRollbackOf(d.run), () => {})
  }, [sourceId])

  // 用户手动点选前，选中项跟随运行进度（例如卡点进入待审批时自动切过去）。
  const [manual, setManual] = useState(false)
  useEffect(() => {
    setSelected(undefined)
    setManual(false)
  }, [id])
  useEffect(() => {
    if (data && !manual) setSelected((cur) => pickDefault(data.jobs, cur))
  }, [data, manual])
  const select = (jobId: string) => {
    setManual(true)
    setSelected(jobId)
  }
  // 日志窗口：打开时可指定跳到的步骤（日志加载中时稍后重试）
  const [logOpen, setLogOpen] = useState(false)
  const [jumpTo, setJumpTo] = useState<{ step: number; at: number }>()
  const openLog = (jobId: string, step?: number) => {
    select(jobId)
    setLogOpen(true)
    setJumpTo(step === undefined ? undefined : { step, at: Date.now() })
  }
  useEffect(() => {
    if (!jumpTo) return
    let n = 0
    const t = setInterval(() => {
      if (logView.current?.scrollToStep(jumpTo.step) || ++n > 20) clearInterval(t)
    }, 250)
    return () => clearInterval(t)
  }, [jumpTo])
  const source = useMemo(() => {
    try {
      const src = (parseYaml(pipeline?.yaml ?? '') as { source?: { url?: string; branch?: string } } | null)?.source
      return src?.url ? { repo: src.url.replace(/^[a-z]+:\/\/[^/]+\//i, '').replace(/\.git$/, ''), url: src.url, branch: src.branch } : undefined
    } catch {
      return undefined
    }
  }, [pipeline?.yaml])

  if (error && !data) return <div className="alert">{error}</div>
  if (!data) return <p className="muted">加载中…</p>

  const { run, jobs } = data
  // 已请求取消、但还有任务在等构建节点确认停止
  const cancelPending = jobs.some((j) => j.status === 'running' && j.cancel_requested)
  const job = jobs.find((j) => j.id === selected)
  // webhook 注入的 FLOW_GIT_* 单独展示为提交信息，其余为用户参数。
  const git = run.trigger === 'webhook' ? run.env : undefined
  const params = Object.entries(run.env ?? {}).filter(([k]) => !k.startsWith('FLOW_GIT_'))

  async function act(fn: () => Promise<unknown>) {
    setBusy(true)
    setActionError(undefined)
    try {
      await fn()
      await refresh()
    } catch (e) {
      setActionError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="run-detail">
      {!embedded && (
        <div className="page-head">
          <a className="icon-btn back" href={pipeline ? href.pipeline(pipeline.id) : href.list} title="返回流水线">
            <Icon name="back" />
          </a>
          <h1>
            {pipeline?.name ?? '运行'} <span className="muted">运行记录 #{run.number}</span>
          </h1>
        </div>
      )}
      <div className="page-head run-head">
        <h2>#{run.number}</h2>
        <StatusBadge status={run.status} />
        <div className="run-counts">
          <span>
            <strong>{run.commit ? 1 : 0}</strong>
            <span className="muted small">代码变更</span>
          </span>
          <span>
            <strong>{artifacts?.length ?? 0}</strong>
            <span className="muted small">运行产物</span>
          </span>
          <span>
            <strong>{Object.keys(run.env ?? {}).length}</strong>
            <span className="muted small">环境变量</span>
          </span>
        </div>
        <div className="spacer" />
        {active && hasPerm(pipeline, 'run') && !cancelPending && (
          <button
            className="btn danger"
            disabled={busy}
            title={run.status === 'queued' ? '还在排队，取消后不会被领取' : '正在执行的任务会被构建节点终止，确认停下后才标记为已取消；未开始的任务直接取消'}
            onClick={() => confirm(`取消运行 #${run.number}？`) && act(() => api.cancelRun(id))}
          >
            取消运行
          </button>
        )}
        {active && hasPerm(pipeline, 'run') && cancelPending && (
          <button
            className="btn danger"
            disabled={busy}
            title="不再等构建节点确认停止，直接把运行标记为已取消"
            onClick={() =>
              confirm(`强制结束 #${run.number}？\n不再等构建节点确认停止，直接把运行标记为已取消（节点离线或卡住时使用）。`) && act(() => api.cancelRun(id, true))
            }
          >
            强制结束
          </button>
        )}
        {(run.status === 'failed' || run.status === 'canceled') && hasPerm(pipeline, 'run') && (
          <button
            className="btn"
            disabled={busy}
            title="成功的 Job 保留结果，其余 Job 按本次运行的定义重新执行"
            onClick={() => act(() => api.retryRun(id))}
          >
            重试失败的 Job
          </button>
        )}
        {!active && hasPerm(pipeline, 'run') && (
          <button
            className="btn"
            disabled={busy}
            title={params.length ? '沿用这次运行填写的运行参数，按流水线当前配置运行，代码拉取当前最新提交' : '这次运行没有运行参数，按流水线当前配置运行，代码拉取当前最新提交'}
            onClick={() => {
              if (!confirm(params.length ? `用 #${run.number} 的参数重新构建？\n${params.map(([k, v]) => `${k}=${v}`).join('\n')}` : `重新构建？按流水线当前配置运行，代码拉取最新提交。`)) return
              act(async () => {
                const res = await api.triggerRun(run.pipeline_id, params.length ? Object.fromEntries(params) : undefined, `重新构建 #${run.number}`)
                navigate(href.run(res.run.id))
              })
            }}
          >
            重新构建
          </button>
        )}
        {run.status === 'success' && !!run.pipeline_version && hasPerm(pipeline, 'run') && (
          <button className="btn" disabled={busy} title="用本次的配置版本、参数和代码提交重新运行" onClick={() => setConfirmRollback(true)}>
            回滚到此次运行
          </button>
        )}
      </div>
      {confirmRollback && <RollbackDialog run={run} pipeline={pipeline} onClose={() => setConfirmRollback(false)} />}
      {actionError && <div className="alert">{actionError}</div>}

      <dl className="facts inline">
        <div>
          <dt>触发信息</dt>
          <dd>
            {run.trigger_info ? (
              <TriggerDetails info={run.trigger_info} label={triggerLabel(run.trigger, run.env, run.created_by)} />
            ) : (
              triggerLabel(run.trigger, run.env, run.created_by)
            )}
          </dd>
        </div>
        <div>
          <dt>开始时间</dt>
          <dd>{fmtTime(run.started_at ?? run.created_at)}</dd>
        </div>
        <div>
          <dt>持续时间</dt>
          <dd>{fmtDuration(run.started_at, run.finished_at)}</dd>
        </div>
        {run.rollback_of && (
          <div>
            <dt>{run.artifacts_from ? '重新部署' : '回滚自'}</dt>
            <dd>
              <a href={href.run(run.rollback_of)}>{rollbackOf ? `#${rollbackOf.number}` : '原运行'}</a>
              {rollbackOf && <span className="muted"> · {fmtTime(rollbackOf.started_at ?? rollbackOf.created_at)}</span>}
              {run.artifacts_from && <span className="muted"> · 只执行部署任务，沿用其构建产物</span>}
            </dd>
          </div>
        )}
        {!!run.pipeline_version && (
          <div>
            <dt>配置版本</dt>
            <dd>
              <a href={href.pipeline(run.pipeline_id, 'edit')}>v{run.pipeline_version}</a>
              {pipeline && pipeline.version !== run.pipeline_version && <span className="muted"> · 当前已是 v{pipeline.version}</span>}
            </dd>
          </div>
        )}
        {git?.FLOW_GIT_PR_NUMBER && (
          <div>
            <dt>合并请求</dt>
            <dd>
              {git.FLOW_GIT_PR_URL ? (
                <a href={git.FLOW_GIT_PR_URL} target="_blank" rel="noreferrer">
                  #{git.FLOW_GIT_PR_NUMBER}
                </a>
              ) : (
                `#${git.FLOW_GIT_PR_NUMBER}`
              )}{' '}
              <span className="mono">
                {git.FLOW_GIT_PR_SOURCE_BRANCH} → {git.FLOW_GIT_PR_TARGET_BRANCH}
              </span>
            </dd>
          </div>
        )}
        {git?.FLOW_GIT_COMMIT && (
          <div>
            <dt>提交</dt>
            <dd>
              {git.FLOW_GIT_REPO && <span className="mono">{git.FLOW_GIT_REPO} </span>}
              {git.FLOW_GIT_MESSAGE || <span className="mono">{git.FLOW_GIT_COMMIT.slice(0, 12)}</span>}
              {git.FLOW_GIT_PUSHER && <span className="muted"> · {git.FLOW_GIT_PUSHER}</span>}
            </dd>
          </div>
        )}
        {run.commit && !git?.FLOW_GIT_COMMIT && (
          <div>
            <dt>代码提交</dt>
            <dd>
              {commitURL(source?.url, run.commit) ? (
                <a className="mono" href={commitURL(source?.url, run.commit)} target="_blank" rel="noreferrer" title={run.commit}>
                  {shortSha(run.commit)}
                </a>
              ) : (
                <span className="mono" title={run.commit}>{shortSha(run.commit)}</span>
              )}
              {run.env?.FLOW_GIT_BRANCH && <span className="muted"> · {run.env.FLOW_GIT_BRANCH}</span>}
            </dd>
          </div>
        )}
        {(run.remark || hasPerm(pipeline, 'run')) && (
          <div>
            <dt>备注</dt>
            <dd>
              {run.remark || <span className="muted">—</span>}
              {hasPerm(pipeline, 'run') && (
                <button
                  className="btn link small"
                  onClick={() => {
                    const v = prompt('运行备注', run.remark ?? '')
                    if (v !== null) act(() => api.setRunRemark(run.id, v))
                  }}
                >
                  编辑
                </button>
              )}
            </dd>
          </div>
        )}
        {params.length > 0 && (
          <div>
            <dt>运行参数</dt>
            <dd className="mono">{params.map(([k, v]) => `${k}=${v}`).join('  ')}</dd>
          </div>
        )}
      </dl>

      <QueueNotice run={run} jobs={jobs} />

      <StageBoard jobs={jobs} source={source && { ...source, commit: run.commit, commitUrl: commitURL(source.url, run.commit) }} onOpen={openLog} />

      {jobs
        .filter((j) => j.status === 'waiting')
        .map((j) => (
          <div key={j.id} className="card waiting-notice">
            「{j.name}」等待人工审批
            <button className="btn link" onClick={() => openLog(j.id)}>
              去审批
            </button>
          </div>
        ))}

      {reports && reports.length > 0 && <TestReports key={reports.map((r) => r.created_at).join()} reports={reports} />}

      {artifacts && artifacts.length > 0 && (
        <div className="artifacts">
          <span className="muted">运行产物</span>
          {artifacts.map((a) => (
            <a key={a.id} className="btn" href={api.artifactURL(a.id)} download title={a.expires_at && `过期时间 ${fmtTime(a.expires_at)}`}>
              <Icon name="box" size={14} /> {a.name ? `${a.name}.${a.format}` : `${a.job_name}.tar.gz`}
              {a.version && <span className="tag">{a.version}</span>}
              <span className="muted">{fmtSize(a.size)}</span>
              <span className="muted small">· {fmtExpiry(a.expires_at)}</span>
            </a>
          ))}
        </div>
      )}

      {logOpen && job && (
        <Modal xl title={<><StatusBadge status={run.status} /> #{run.number} 运行日志</>} onClose={() => setLogOpen(false)}>
        <div className="log-dialog">
          <nav className="log-nav">
            {jobs.map((j, i) => (
              <Fragment key={j.id}>
                {(i === 0 || jobs[i - 1].stage !== j.stage) && <div className="side-title">{j.stage}</div>}
                <button className={`status-${j.status}${j.id === job.id ? ' active' : ''}`} onClick={() => { select(j.id); setJumpTo(undefined) }}>
                  <span className="dot" />
                  <span className="ellipsis">{j.name}</span>
                  <span className="muted small">{j.started_at ? fmtDuration(j.started_at, j.finished_at) : ''}</span>
                </button>
              </Fragment>
            ))}
          </nav>
        <div className="job-panel">
          <div className="job-head">
            <h2>{job.name}</h2>
            <StatusBadge status={job.status} />
            <span className="muted">
              {job.stage}
              {job.matrix && job.matrix.length > 0 && <> · {job.matrix.join(' ')}</>}
              {job.image && <> · 镜像 {job.image}</>}
              {job.runs_on && job.runs_on.length > 0 && <> · 标签 {job.runs_on.join(', ')}</>}
              {job.runner_id && <> · Runner {job.runner_id}</>}
              {job.attempt > 1 && <> · 第 {job.attempt} 次执行</>}
              {job.decided_by && <> · 审批人 {job.decided_by}</>}
              {job.started_at && <> · {fmtDuration(job.started_at, job.finished_at)}</>}
              {job.status === 'failed' && job.exit_code !== 0 && <> · 退出码 {job.exit_code}</>}
            </span>
            {job.deploy && job.status === 'success' && isTerminal(run.status) && hasPerm(pipeline, 'run') && <RedeployButton run={run} job={job} />}
          </div>
          <div className="log-links">
            <a className="btn link small" href={api.logURL(job.id)} target="_blank" rel="noreferrer">
              <Icon name="file" size={13} /> 查看原始日志
            </a>
            <a className="btn link small" href={api.logURL(job.id, true)} download>
              <Icon name="download" size={13} /> 下载日志
            </a>
          </div>
          {job.error && <div className="alert subtle">{job.error}</div>}
          {job.outputs && Object.keys(job.outputs).length > 0 && (
            <div className="job-outputs">
              <span className="muted small">任务输出</span>
              {Object.entries(job.outputs).map(([k, v]) => (
                <code key={k} title={v}>
                  {k}={v.length > 40 ? v.slice(0, 40) + '…' : v}
                </code>
              ))}
            </div>
          )}
          {job.status === 'waiting' ? (
            <ApprovalPanel key={job.id} job={job} canApprove={can('developer')} onDone={refresh} />
          ) : job.status === 'pending' || job.status === 'queued' ? (
            <p className="muted waiting">
              {job.status === 'pending'
                ? `等待上游：${(job.needs ?? []).join('、')}`
                : job.runs_on && job.runs_on.length > 0
                  ? `等待带有标签 ${job.runs_on.join(', ')} 的 Runner 领取…`
                  : '等待 Runner 领取…'}
            </p>
          ) : job.status === 'skipped' || (job.status === 'canceled' && !job.started_at) ? (
            <p className="muted waiting">未执行</p>
          ) : (
            <div className="job-body">
              {job.deploy ? (
                <HostList job={job} onPick={(name) => logView.current?.scrollToPrefix(`[${name}] `)} />
              ) : (
                job.steps.length > 0 && <StepList job={job} onPick={(i) => logView.current?.scrollToStep(i)} />
              )}
              <LogViewer key={job.id} job={job} handle={logView} />
            </div>
          )}
        </div>
        </div>
        </Modal>
      )}
    </section>
  )
}

/** 步骤进度：准备环境（拉取代码、下载产物、恢复缓存）+ 各步骤的状态与耗时，点击跳到对应日志。 */
function StepList({ job, onPick }: { job: Job; onPick: (index: number) => void }) {
  const states = job.step_states ?? []
  const firstStart = states.find((s) => s.started_at)?.started_at
  const jobDone = isTerminal(job.status)
  const prepare: Status = firstStart ? 'success' : jobDone ? (job.status === 'success' ? 'success' : job.status) : 'running'
  const rows = [
    { key: -1, name: '准备环境', status: prepare, start: job.started_at, end: firstStart ?? (jobDone ? job.finished_at : undefined), exit: 0 },
    ...job.steps.map((s, i) => {
      const st = states[i]
      return { key: i, name: s.name || `步骤 ${i + 1}`, status: st?.status ?? 'pending', start: st?.started_at, end: st?.finished_at, exit: st?.exit_code ?? 0 }
    }),
  ]
  return (
    <ol className="step-list">
      {rows.map((r) => (
        <li key={r.key}>
          <button className={`status-${r.status}`} disabled={!r.start} onClick={() => onPick(r.key)} title={r.start ? '跳到这一步的日志' : '未执行'}>
            <span className={`step-icon${r.status === 'running' ? ' live' : ''}`}>{stepIcon[r.status] ?? '○'}</span>
            <span className="step-name">{r.name}</span>
            {r.status === 'failed' && r.exit !== 0 && <span className="error-text small">退出码 {r.exit}</span>}
            <span className="muted small step-time">{r.start ? fmtDuration(r.start, r.end) : jobDone ? '未执行' : statusLabel[r.status]}</span>
          </button>
        </li>
      ))}
    </ol>
  )
}

/** 部署任务：各主机的进度，点击跳到该主机的日志。 */
function HostList({ job, onPick }: { job: Job; onPick: (name: string) => void }) {
  const hosts = job.host_states ?? []
  const done = hosts.filter((h) => h.status === 'success').length
  return (
    <div className="step-list host-list">
      <div className="muted small host-list-head">
        主机组 <span className="mono">{job.deploy!.group}</span>
        {hosts.length > 0 && ` · ${done}/${hosts.length} 台完成`}
        {(job.deploy!.batch ?? 1) > 1 && ` · 每批 ${(job.deploy!.batch ?? 1) >= 1000 ? '全部' : job.deploy!.batch} 台`}
      </div>
      {hosts.length === 0 && <p className="muted small">准备中…</p>}
      <ol>
        {hosts.map((h) => (
          <li key={h.name}>
            <button className={`status-${h.status}`} disabled={!h.started_at} onClick={() => onPick(h.name)} title={h.error || h.address}>
              <span className={`step-icon${h.status === 'running' ? ' live' : ''}`}>{stepIcon[h.status] ?? '○'}</span>
              <span className="step-name">
                {h.name}
                <span className="muted small mono"> {h.address}</span>
              </span>
              <span className="muted small step-time">{h.started_at ? fmtDuration(h.started_at, h.finished_at) : statusLabel[h.status]}</span>
            </button>
          </li>
        ))}
      </ol>
    </div>
  )
}

const stepIcon: Partial<Record<Status, string>> = { success: '✓', failed: '✗', running: '●', canceled: '⊘', pending: '○', skipped: '–' }

function ApprovalPanel({ job, canApprove, onDone }: { job: Job; canApprove: boolean; onDone: () => Promise<void> }) {
  const { me } = useSession()
  const [comment, setComment] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const approvers = job.approvers ?? []
  const allowed = canApprove && (approvers.length === 0 || approvers.includes(me.user.username) || me.role === 'admin')

  async function decide(approve: boolean) {
    setBusy(true)
    setError(undefined)
    try {
      await api.approve(job.id, approve, comment.trim())
      await onDone()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  return (
    <div className="card approval">
      <div className="strong">{job.deploy ? '部署审批' : '等待人工审批'}</div>
      <p className="muted">
        {job.deploy && (
          <>
            即将部署到主机组 <code>{job.deploy.group}</code>（环境要求部署前审批）。
          </>
        )}
        {approvers.length > 0 ? `审批人：${approvers.join('、')}（管理员也可审批）` : '开发者及以上成员均可审批'}
        。{job.deploy ? '通过后开始部署' : '通过后继续执行下游 Job'}，驳回则本次运行失败。
      </p>
      {allowed ? (
        <>
          <input value={comment} onChange={(e) => setComment(e.target.value)} placeholder="备注（可选）" />
          <div className="row">
            <button className="btn primary" disabled={busy} onClick={() => decide(true)}>
              通过
            </button>
            <button className="btn danger" disabled={busy} onClick={() => decide(false)}>
              驳回
            </button>
            {error && <span className="error-text">{error}</span>}
          </div>
        </>
      ) : (
        <p className="muted">你没有审批这个卡点的权限。</p>
      )}
    </div>
  )
}

/** 只重新执行这个部署任务（把对应环境回退 / 重放到本次运行的版本）。 */
function RedeployButton({ run, job }: { run: Run; job: Job }) {
  const [busy, setBusy] = useState(false)
  async function go() {
    if (!confirm(`重新部署 #${run.number} 的「${job.name}」？\n只执行这个部署任务（主机组 ${job.deploy!.group}），沿用当时的配置、参数与构建产物，不重新构建、不影响其他环境。`)) return
    setBusy(true)
    try {
      const d = await api.redeploy(run.id, job.name)
      navigate(href.run(d.run.id))
    } catch (e) {
      alert((e as Error).message)
      setBusy(false)
    }
  }
  return (
    <button className="btn small" disabled={busy} onClick={go} title="把该环境回退 / 重放到这个版本">
      重新部署此版本
    </button>
  )
}

/** 回滚确认：说明将重放的配置版本、代码提交与参数。 */
function RollbackDialog({ run, pipeline, onClose }: { run: Run; pipeline?: Pipeline; onClose: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const params = Object.entries(run.env ?? {}).filter(([k]) => !k.startsWith('FLOW_GIT_'))
  const branch = run.env?.FLOW_GIT_BRANCH ?? run.env?.FLOW_GIT_TAG
  async function go() {
    setBusy(true)
    setError(undefined)
    try {
      const d = await api.rollbackRun(run.id)
      onClose()
      navigate(href.run(d.run.id))
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }
  return (
    <Modal
      title={`回滚到 #${run.number}`}
      onClose={onClose}
      footer={
        <>
          <div className="spacer" />
          <button className="btn" onClick={onClose}>
            取消
          </button>
          <button className="btn primary" disabled={busy} onClick={go}>
            {busy ? '正在创建…' : '开始回滚'}
          </button>
        </>
      }
    >
      <p>将新建一次运行，按 #{run.number} 当时的状态重新执行全部任务：</p>
      <dl className="facts rollback-facts">
        <div>
          <dt>配置版本</dt>
          <dd>
            v{run.pipeline_version}
            {pipeline && pipeline.version !== run.pipeline_version && <span className="muted">（当前为 v{pipeline.version}，不会被修改）</span>}
          </dd>
        </div>
        <div>
          <dt>代码</dt>
          <dd>
            {run.commit ? (
              <>
                提交 <span className="mono">{shortSha(run.commit)}</span>
                {branch && <span className="muted"> · {branch}</span>}
              </>
            ) : (
              <span className="muted">本次运行未记录提交，将拉取{branch ? `分支 ${branch} ` : '默认分支'}的最新代码</span>
            )}
          </dd>
        </div>
        {params.length > 0 && (
          <div>
            <dt>运行参数</dt>
            <dd className="mono">{params.map(([k, v]) => `${k}=${v}`).join('  ')}</dd>
          </div>
        )}
      </dl>
      <p className="muted small">密钥使用当前值；人工卡点需要重新审批。</p>
      {error && <div className="alert">{error}</div>}
    </Modal>
  )
}
