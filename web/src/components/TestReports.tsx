import { useMemo, useState } from 'react'
import type { TestCase, TestReport } from '../types'

type View = 'failed' | 'skipped' | 'slowest'

const pct = (n: number, total: number) => (total ? `${((n / total) * 100).toFixed(n === total ? 0 : 1)}%` : '—')
const secs = (s: number) => (s >= 60 ? `${Math.floor(s / 60)}m ${Math.round(s % 60)}s` : `${s.toFixed(s < 10 ? 2 : 1)}s`)

/** 运行的测试结果：汇总条、失败 / 跳过 / 最慢用例。 */
export function TestReports({ reports }: { reports: TestReport[] }) {
  const sum = useMemo(
    () =>
      reports.reduce(
        (a, r) => ({ total: a.total + r.total, passed: a.passed + r.passed, failed: a.failed + r.failed, skipped: a.skipped + r.skipped, duration: a.duration + r.duration }),
        { total: 0, passed: 0, failed: 0, skipped: 0, duration: 0 },
      ),
    [reports],
  )
  const cases = useMemo(() => reports.flatMap((r) => r.cases.map((c) => ({ ...c, job: r.job_name }))), [reports])
  const [view, setView] = useState<View>(sum.failed > 0 ? 'failed' : 'slowest')
  const [open, setOpen] = useState(sum.failed > 0)
  const shown = cases
    .filter((c) => (view === 'failed' ? c.status === 'failed' : view === 'skipped' ? c.status === 'skipped' : c.status === 'passed'))
    .sort((a, b) => (view === 'slowest' ? (b.time ?? 0) - (a.time ?? 0) : 0))

  return (
    <div className={`card tests${sum.failed ? ' has-failed' : ''}`}>
      <button className="tests-head" onClick={() => setOpen(!open)} aria-expanded={open}>
        <span className="caret">{open ? '▾' : '▸'}</span>
        <strong>测试</strong>
        <span>{sum.total} 个用例</span>
        <span className="ok-text">✓ {sum.passed}</span>
        {sum.failed > 0 && <span className="error-text">✗ {sum.failed} 失败</span>}
        {sum.skipped > 0 && <span className="muted">⊘ {sum.skipped} 跳过</span>}
        <span className="muted">通过率 {pct(sum.passed, sum.passed + sum.failed)}</span>
        <span className="muted">· {secs(sum.duration)}</span>
        <span className="tests-bar" aria-hidden>
          <span className="ok" style={{ flex: sum.passed }} />
          <span className="bad" style={{ flex: sum.failed }} />
          <span className="skip" style={{ flex: sum.skipped }} />
        </span>
      </button>
      {open && (
        <>
          {reports.length > 1 && (
            <div className="tests-jobs muted small">
              {reports.map((r) => (
                <span key={r.job_id}>
                  {r.job_name}：{r.passed}/{r.total}
                  {r.failed > 0 && <span className="error-text"> · {r.failed} 失败</span>}
                </span>
              ))}
            </div>
          )}
          <div className="segmented">
            <button className={view === 'failed' ? 'active' : ''} onClick={() => setView('failed')}>
              失败 {sum.failed}
            </button>
            <button className={view === 'skipped' ? 'active' : ''} onClick={() => setView('skipped')}>
              跳过 {sum.skipped}
            </button>
            <button className={view === 'slowest' ? 'active' : ''} onClick={() => setView('slowest')}>
              最慢的用例
            </button>
          </div>
          {shown.length === 0 ? (
            <p className="muted small">{view === 'failed' ? '没有失败的用例 🎉' : view === 'skipped' ? '没有跳过的用例' : '没有通过的用例'}</p>
          ) : (
            <ul className="test-cases">
              {shown.slice(0, 200).map((c, i) => (
                <CaseRow key={i} c={c} multiJob={reports.length > 1} />
              ))}
              {shown.length > 200 && <li className="muted small">还有 {shown.length - 200} 个</li>}
            </ul>
          )}
        </>
      )}
    </div>
  )
}

function CaseRow({ c, multiJob }: { c: TestCase & { job: string }; multiJob: boolean }) {
  const title = (
    <>
      <span className={c.status === 'failed' ? 'error-text' : c.status === 'skipped' ? 'muted' : 'ok-text'}>{c.status === 'failed' ? '✗' : c.status === 'skipped' ? '⊘' : '✓'}</span>
      <span className="test-name">
        {c.class && <span className="muted">{c.class} › </span>}
        {c.name}
      </span>
      {multiJob && <span className="tag">{c.job}</span>}
      {!!c.time && <span className="muted small">{secs(c.time)}</span>}
    </>
  )
  if (!c.message) return <li className="test-case">{title}</li>
  return (
    <li className="test-case">
      <details open={c.status === 'failed'}>
        <summary>{title}</summary>
        <pre className="test-message">{c.message}</pre>
      </details>
    </li>
  )
}
