import { Fragment, useState } from 'react'
import { api } from '../api'
import { statusLabel } from '../components/StatusBadge'
import { fmtTime } from '../format'
import { usePolling } from '../hooks'
import { href } from '../router'
import type { PipelineStats as Stats } from '../types'

const dur = (s: number) => {
  if (!s) return '—'
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${Math.round(s % 60)}s`
  const h = Math.floor(m / 60)
  return h < 24 ? `${h}h ${m % 60}m` : `${Math.floor(h / 24)}d ${h % 24}h`
}
const pct = (x: number) => (x < 0 ? '—' : `${(x * 100).toFixed(x === 1 ? 0 : 1)}%`)

/** 流水线统计：成功率、耗时分布、恢复时间、最慢 / 不稳定的任务、部署频率。 */
export function PipelineStats({ pipelineId }: { pipelineId: string }) {
  const [days, setDays] = useState(30)
  const { data: st, error } = usePolling(() => api.pipelineStats(pipelineId, days), [pipelineId, days], () => 60000)
  return (
    <>
      <div className="row stats-head">
        <div className="spacer" />
        <div className="pills">
          {[7, 30, 90].map((d) => (
            <button key={d} className={days === d ? 'active' : ''} onClick={() => setDays(d)}>
              最近{d} 天
            </button>
          ))}
        </div>
        {st?.truncated && <span className="muted small">运行较多，只统计了最近 2000 次</span>}
      </div>
      {error && <div className="alert">{error}</div>}
      {!st ? (
        !error && <p className="muted">统计中…</p>
      ) : st.runs === 0 ? (
        <div className="empty">
          <p className="muted">近 {days} 天没有运行。</p>
        </div>
      ) : (
        <StatsBody st={st} />
      )}
    </>
  )
}

function StatsBody({ st }: { st: Stats }) {
  const deploys = st.deploys.reduce((n, d) => n + d.count, 0)
  return (
    <div className="stats">
      <div className="kpis">
        <Kpi label="运行次数" value={String(st.runs)} hint={`成功 ${st.success} · 失败 ${st.failed}${st.canceled ? ` · 取消 ${st.canceled}` : ''}`} />
        <Kpi label="成功率" value={pct(st.success_rate)} tone={st.success_rate < 0 ? undefined : st.success_rate >= 0.9 ? 'ok' : st.success_rate < 0.7 ? 'bad' : 'warn'} hint="成功 /（成功 + 失败）" />
        <Kpi label="平均耗时" value={dur(st.avg_duration)} hint={`P50 ${dur(st.p50_duration)} · P90 ${dur(st.p90_duration)}`} />
        <Kpi label="平均恢复时间" value={dur(st.mean_recovery)} hint="失败后到下一次成功" />
        <Kpi label="部署次数" value={String(deploys)} hint={st.deploys.map((d) => `${d.environment} ${d.count}`).join(' · ') || '没有主机部署'} />
      </div>

      <div className="card">
        <h3>每日运行</h3>
        <DailyChart st={st} />
      </div>

      <div className="stats-grid">
        <div className="card">
          <h3>流水线运行分布</h3>
          <p className="muted small">最近 {st.recent.length} 次运行的耗时；虚线为均值，点线为 95% 基线（P95 {dur(st.p95_duration)}）</p>
          <DurationChart st={st} />
        </div>
        <div className="card">
          <h3>运行的时段分布</h3>
          <Heatmap data={st.heatmap} />
        </div>
        <div className="card">
          <h3>运行按触发方式排名</h3>
          <Ranking rows={st.by_trigger} label={(k) => triggerNames[k] ?? k} />
        </div>
        <div className="card">
          <h3>运行按触发人排名</h3>
          <Ranking rows={st.by_user} label={(k) => k} />
        </div>
      </div>

      {st.tests && (
        <div className="card">
          <h3>测试</h3>
          <p className="muted small">
            {st.tests.runs} 次运行有测试报告，平均 {st.tests.avg_total.toFixed(0)} 个用例，最近一次 {st.tests.latest_total} 个；
            <span className={st.tests.failed_runs ? 'error-text' : 'ok-text'}> {st.tests.failed_runs} 次有失败用例</span>
          </p>
          <TestChart st={st} />
        </div>
      )}

      <div className="card">
        <h3>任务</h3>
        <p className="muted small">按平均耗时排序；同时出现过成功和失败的任务标为「不稳定」，优先排查。</p>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>任务</th>
                <th>执行次数</th>
                <th>失败率</th>
                <th>平均耗时</th>
                <th>P90</th>
              </tr>
            </thead>
            <tbody>
              {st.jobs.map((j) => {
                const rate = j.runs ? j.failed / j.runs : 0
                const maxAvg = st.jobs[0]?.avg_duration || 1
                return (
                  <tr key={j.name}>
                    <td>
                      <span className="strong">{j.name}</span>
                      {j.failed > 0 && j.failed < j.runs && <span className="tag warn">不稳定</span>}
                    </td>
                    <td>{j.runs}</td>
                    <td>
                      <span className="rate-bar">
                        <span style={{ width: `${rate * 100}%` }} />
                      </span>
                      <span className={rate > 0 ? 'error-text small' : 'muted small'}> {pct(rate)}</span>
                    </td>
                    <td>
                      <span className="rate-bar time">
                        <span style={{ width: `${(j.avg_duration / maxAvg) * 100}%` }} />
                      </span>{' '}
                      {dur(j.avg_duration)}
                    </td>
                    <td className="muted">{dur(j.p90_duration)}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      </div>

      {st.deploys.length > 0 && (
        <div className="card">
          <h3>部署</h3>
          <div className="kpis">
            {st.deploys.map((d) => (
              <Kpi key={d.environment} label={d.environment} value={`${d.count} 次`} hint={`成功 ${d.success} · 最近 ${fmtTime(d.last_at)}`} />
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

function Kpi({ label, value, hint, tone }: { label: string; value: string; hint?: string; tone?: 'ok' | 'warn' | 'bad' }) {
  return (
    <div className={`kpi${tone ? ` ${tone}` : ''}`}>
      <span className="muted small">{label}</span>
      <strong>{value}</strong>
      {hint && <span className="muted small">{hint}</span>}
    </div>
  )
}

/** 每日运行：成功（绿）/ 失败（红）/ 其他（灰）堆叠柱。 */
function DailyChart({ st }: { st: Stats }) {
  const max = Math.max(1, ...st.daily.map((d) => d.total))
  const w = 100 / st.daily.length
  return (
    <div className="chart">
      <svg viewBox="0 0 100 40" preserveAspectRatio="none" role="img" aria-label="每日运行次数">
        {st.daily.map((d, i) => {
          const h = (n: number) => (n / max) * 38
          const other = d.total - d.success - d.failed
          let y = 40
          const bars = [
            ['ok', d.success],
            ['bad', d.failed],
            ['other', other],
          ] as const
          return (
            <g key={d.date}>
              <title>{`${d.date}：${d.total} 次（成功 ${d.success}，失败 ${d.failed}）`}</title>
              {bars.map(([cls, n]) => {
                if (!n) return null
                y -= h(n)
                return <rect key={cls} className={cls} x={i * w + w * 0.15} y={y} width={w * 0.7} height={h(n)} />
              })}
            </g>
          )
        })}
      </svg>
      <div className="chart-axis muted small">
        <span>{st.daily[0]?.date}</span>
        <span>最多 {max} 次/天</span>
        <span>{st.daily[st.daily.length - 1]?.date}</span>
      </div>
    </div>
  )
}

/** 最近运行的耗时：柱高为耗时，颜色为结果，虚线为平均值；点击柱子进入运行。 */
function DurationChart({ st }: { st: Stats }) {
  const max = Math.max(1, ...st.recent.map((r) => r.duration))
  const n = Math.max(st.recent.length, 20)
  const w = 100 / n
  const avgY = 40 - (st.avg_duration / max) * 38
  return (
    <div className="chart">
      <svg viewBox="0 0 100 40" preserveAspectRatio="none" role="img" aria-label="运行耗时">
        {st.recent.map((r, i) => (
          <a key={r.id} href={href.run(r.id)}>
            <title>{`#${r.number} ${statusLabel[r.status]} · ${dur(r.duration)} · ${fmtTime(r.at)}`}</title>
            <rect className={`status-${r.status} dur`} x={i * w + w * 0.15} y={40 - Math.max(1, (r.duration / max) * 38)} width={w * 0.7} height={Math.max(1, (r.duration / max) * 38)} />
          </a>
        ))}
        {st.avg_duration > 0 && <line className="avg" x1={0} x2={100} y1={avgY} y2={avgY} />}
        {st.p95_duration > 0 && st.p95_duration <= max && (
          <line className="p95" x1={0} x2={100} y1={40 - (st.p95_duration / max) * 38} y2={40 - (st.p95_duration / max) * 38} />
        )}
      </svg>
      <div className="chart-axis muted small">
        <span>#{st.recent[0]?.number}</span>
        <span>
          最长 {dur(max)} · 平均 {dur(st.avg_duration)}（虚线）
        </span>
        <span>#{st.recent[st.recent.length - 1]?.number}</span>
      </div>
    </div>
  )
}

/** 每次运行的用例数（通过 / 失败）。 */
function TestChart({ st }: { st: Stats }) {
  const pts = st.recent.filter((r) => r.tests)
  const max = Math.max(1, ...pts.map((r) => r.tests!.total))
  const w = 100 / Math.max(pts.length, 20)
  return (
    <div className="chart small-chart">
      <svg viewBox="0 0 100 24" preserveAspectRatio="none" role="img" aria-label="测试用例">
        {pts.map((r, i) => {
          const t = r.tests!
          const hp = (t.passed / max) * 22
          const hf = (t.failed / max) * 22
          return (
            <a key={r.id} href={href.run(r.id)}>
              <title>{`#${r.number}：${t.total} 个用例，失败 ${t.failed}`}</title>
              <rect className="ok" x={i * w + w * 0.15} y={24 - hp} width={w * 0.7} height={hp} />
              {t.failed > 0 && <rect className="bad" x={i * w + w * 0.15} y={24 - hp - hf} width={w * 0.7} height={hf} />}
            </a>
          )
        })}
      </svg>
    </div>
  )
}

const triggerNames: Record<string, string> = { manual: '手动触发', webhook: '代码推送', cron: '定时触发', api: 'API', rollback: '回滚 / 重新部署', generic: '流水线 Webhook' }
const weekdays = ['周一', '周二', '周三', '周四', '周五', '周六', '周日']
const slots = ['0-3', '3-6', '6-9', '9-12', '12-15', '15-18', '18-21', '21-24']

/** 一周 × 每 3 小时的运行次数热力图（颜色越深越多）。 */
function Heatmap({ data }: { data: number[][] }) {
  const max = Math.max(1, ...data.flat())
  return (
    <div className="heatmap">
      <span />
      {weekdays.map((d) => (
        <span key={d} className="muted small center-text">
          {d}
        </span>
      ))}
      {slots.map((slot, si) => (
        <Fragment key={slot}>
          <span className="muted small">{slot} 点</span>
          {weekdays.map((d, di) => {
            const n = data[di]?.[si] ?? 0
            return <span key={d} className="heat-cell" style={{ opacity: n ? 0.25 + (0.75 * n) / max : 1 }} data-on={n > 0 || undefined} title={`${d} ${slot} 点：${n} 次`} />
          })}
        </Fragment>
      ))}
    </div>
  )
}

/** 横向堆叠条：成功 / 失败（斜纹）/ 取消。 */
function Ranking({ rows, label }: { rows: { key: string; runs: number; success: number; failed: number; canceled: number }[]; label: (k: string) => string }) {
  const max = Math.max(1, ...rows.map((r) => r.runs))
  if (rows.length === 0) return <p className="muted small">没有数据</p>
  return (
    <div className="ranking">
      {rows.slice(0, 8).map((r) => (
        <div key={r.key} className="rank-row" title={`${label(r.key)}：${r.runs} 次（成功 ${r.success}，失败 ${r.failed}，取消 ${r.canceled}）`}>
          <span className="rank-label ellipsis">{label(r.key)}</span>
          <span className="rank-bar">
            <span className="status-success" style={{ width: `${(r.success / max) * 100}%` }} />
            <span className="status-failed hatched" style={{ width: `${(r.failed / max) * 100}%` }} />
            <span className="status-canceled" style={{ width: `${(r.canceled / max) * 100}%` }} />
          </span>
          <span className="muted small">{r.runs} 次</span>
        </div>
      ))}
      <div className="chart-legend">
        <span className="status-success">
          <i />
          成功
        </span>
        <span className="status-failed">
          <i className="hatched" />
          失败
        </span>
        <span className="status-canceled">
          <i />
          已取消
        </span>
      </div>
    </div>
  )
}
