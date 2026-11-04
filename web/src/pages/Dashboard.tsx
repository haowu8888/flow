import { useState, type ReactNode } from 'react'
import { api } from '../api'
import { useSession } from '../auth'
import { Icon, type IconName } from '../components/Icon'
import { StatusBadge } from '../components/StatusBadge'
import { fmtAgo, fmtDuration, fmtSeconds, triggerLabel } from '../format'
import { usePolling, useTicker } from '../hooks'
import { href, navigate } from '../router'
import type { Dashboard as Data, PeriodStat } from '../types'

const ranges = [7, 30, 90]
const RANGE_KEY = 'flow:dashboard-days'

function loadDays() {
  try {
    const d = Number(localStorage.getItem(RANGE_KEY))
    return ranges.includes(d) ? d : 30
  } catch {
    return 30
  }
}

export function Dashboard() {
  const [days, setDays] = useState(loadDays)
  const { can } = useSession()
  const { data, error } = usePolling(() => api.dashboard(days), [days], (d) => (d?.active.length ? 3000 : 15000))
  useTicker(!!data?.active.length)

  function pick(d: number) {
    setDays(d)
    try {
      localStorage.setItem(RANGE_KEY, String(d))
    } catch {
      // 隐私模式等不可用时忽略
    }
  }

  return (
    <section className="dash">
      <div className="page-head">
        <h1>概览</h1>
        <div className="spacer" />
        <div className="pills">
          {ranges.map((d) => (
            <button key={d} className={d === days ? 'active' : ''} onClick={() => pick(d)}>
              最近{d} 天
            </button>
          ))}
        </div>
        {can('developer') && (
          <a className="btn primary" href={href.newPipeline}>
            <Icon name="plus" /> 新建流水线
          </a>
        )}
      </div>
      {error && <div className="alert">{error}</div>}
      {!data ? (
        !error && <p className="muted">加载中…</p>
      ) : (
        <>
          <Tiles d={data} />
          <div className="dash-row">
            <div className="card dash-card grow">
              <div className="dash-card-head">
                <h3>运行趋势</h3>
                <Legend />
              </div>
              <Trend daily={data.daily} />
            </div>
            <div className="card dash-card side">
              <h3>状态分布</h3>
              <Distribution st={data.current} />
            </div>
          </div>
          <div className="dash-row">
            <div className="card dash-card side">
              <div className="dash-card-head">
                <h3>正在运行</h3>
                <span className="muted small">
                  运行中 {data.active.filter((r) => r.status === 'running' || r.status === 'waiting').length} · 排队{' '}
                  {data.active.filter((r) => r.status !== 'running' && r.status !== 'waiting').length}
                </span>
              </div>
              {data.active.length === 0 ? (
                <p className="dash-empty muted">当前没有运行中的任务</p>
              ) : (
                <ul className="dash-active">
                  {data.active.map((r) => (
                    <li key={r.id}>
                      <a href={href.run(r.id)}>
                        <span className="strong">{r.pipeline_name}</span> <span className="muted">#{r.number}</span>
                      </a>
                      <StatusBadge status={r.status} />
                      <span className="muted small">{fmtDuration(r.started_at ?? r.created_at)}</span>
                    </li>
                  ))}
                </ul>
              )}
            </div>
            <div className="card dash-card grow flush">
              <h3>最近运行</h3>
              {data.recent.length === 0 ? (
                <p className="dash-empty muted">这段时间还没有运行</p>
              ) : (
                <table>
                  <thead>
                    <tr>
                      <th>流水线</th>
                      <th>状态</th>
                      <th>触发信息</th>
                      <th>耗时</th>
                      <th>开始时间</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.recent.map((r) => (
                      <tr key={r.id} className="clickable" onClick={() => navigate(href.run(r.id))}>
                        <td>
                          <a href={href.run(r.id)} className="strong">
                            {r.pipeline_name}
                          </a>{' '}
                          <span className="muted">#{r.number}</span>
                        </td>
                        <td>
                          <StatusBadge status={r.status} />
                        </td>
                        <td className="muted">{triggerLabel(r.trigger, r.env, r.created_by)}</td>
                        <td className="muted">{fmtDuration(r.started_at, r.finished_at)}</td>
                        <td className="muted">{fmtAgo(r.started_at ?? r.created_at)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          </div>
          <div className="card dash-card flush">
            <h3>流水线排行（最近 {data.days} 天）</h3>
            {data.ranking.length === 0 ? (
              <p className="dash-empty muted">这段时间还没有运行</p>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>#</th>
                    <th>流水线</th>
                    <th>运行</th>
                    <th>成功率</th>
                    <th>平均耗时</th>
                    <th>最近一次</th>
                  </tr>
                </thead>
                <tbody>
                  {data.ranking.map((p, i) => (
                    <tr key={p.id} className="clickable" onClick={() => navigate(href.pipeline(p.id))}>
                      <td className="muted">{i + 1}</td>
                      <td>
                        <a href={href.pipeline(p.id)} className="strong">
                          {p.name}
                        </a>
                      </td>
                      <td>{p.runs} 次</td>
                      <td>{p.success_rate < 0 ? '—' : `${Math.round(p.success_rate * 100)}%`}</td>
                      <td className="muted">{p.avg_duration ? fmtSeconds(p.avg_duration) : '—'}</td>
                      <td>
                        <StatusBadge status={p.last_status} /> <span className="muted small">{fmtAgo(p.last_at)}</span>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </>
      )}
    </section>
  )
}

function Tile({ label, icon, value, unit, foot }: { label: string; icon: IconName; value: string; unit?: string; foot: ReactNode }) {
  return (
    <div className="card tile">
      <div className="tile-head">
        <span>{label}</span>
        <span className="tile-icon">
          <Icon name={icon} />
        </span>
      </div>
      <div className="tile-value">
        {value}
        {unit && <span className="tile-unit">{unit}</span>}
      </div>
      <div className="tile-foot muted small">{foot}</div>
    </div>
  )
}

function Tiles({ d }: { d: Data }) {
  const c = d.current
  const p = d.previous
  const change = p.runs > 0 ? Math.round(((c.runs - p.runs) / p.runs) * 100) : c.runs > 0 ? 100 : 0
  const rate = (st: PeriodStat) => (st.success_rate < 0 ? undefined : Math.round(st.success_rate * 100))
  return (
    <div className="tiles">
      <Tile label="流水线" icon="pipeline" value={String(d.pipelines)} unit="条" foot={`我创建 ${d.mine} 条`} />
      <Tile
        label={`运行次数（${d.days} 天）`}
        icon="activity"
        value={String(c.runs)}
        unit="次"
        foot={
          <>
            <span className={change > 0 ? 'ok-text' : change < 0 ? 'error-text' : undefined}>
              {change > 0 ? '+' : ''}
              {change}%
            </span>{' '}
            较上周期
          </>
        }
      />
      <Tile
        label="成功率"
        icon="check"
        value={rate(c) === undefined ? '—' : String(rate(c))}
        unit={rate(c) === undefined ? undefined : '%'}
        foot={rate(p) === undefined ? '上周期无数据' : `上周期 ${rate(p)}%`}
      />
      <Tile
        label="平均耗时"
        icon="timer"
        value={c.success_runs ? fmtSeconds(c.avg_duration) : '—'}
        foot={c.success_runs ? `按 ${c.success_runs} 次成功运行计算` : '还没有成功的运行'}
      />
      <Tile
        label="构建节点"
        icon="server"
        value={String(d.runners_online)}
        unit={`/ ${d.runners_total} 在线`}
        foot={d.runners_total === 0 ? '还没有注册节点' : d.runners_online > 0 ? '节点正常' : <span className="error-text">没有在线节点</span>}
      />
    </div>
  )
}

const series = [
  { key: 'success', label: '运行成功', cls: 'status-success' },
  { key: 'failed', label: '运行失败', cls: 'status-failed' },
  { key: 'canceled', label: '已取消', cls: 'status-canceled' },
] as const

function Legend() {
  return (
    <div className="chart-legend">
      {series.map((s) => (
        <span key={s.key} className={s.cls}>
          <i className={s.key === 'failed' ? 'hatched' : undefined} />
          {s.label}
        </span>
      ))}
    </div>
  )
}

/** 按天堆叠的柱状图（成功 / 失败 / 取消）；失败段带斜纹，红绿色弱时也能区分。 */
function Trend({ daily }: { daily: Data['daily'] }) {
  const [hover, setHover] = useState<number>()
  const W = 640
  const H = 200
  const pad = { l: 32, r: 8, t: 8, b: 22 }
  const max = Math.max(1, ...daily.map((d) => d.success + d.failed + d.canceled))
  const step = max <= 4 ? 1 : Math.ceil(max / 4)
  const top = Math.ceil(max / step) * step
  const ticks = Array.from({ length: top / step + 1 }, (_, i) => i * step)
  const band = (W - pad.l - pad.r) / daily.length
  const bw = Math.min(24, Math.max(2, band - 2))
  const y = (v: number) => pad.t + (H - pad.t - pad.b) * (1 - v / top)
  const labelEvery = Math.ceil(daily.length / 8)
  const h = hover !== undefined ? daily[hover] : undefined
  return (
    <div className="trend-chart" onMouseLeave={() => setHover(undefined)}>
      <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label="每天的运行次数">
        <defs>
          <pattern id="hatch" width="4" height="4" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
            <rect width="4" height="4" className="hatch-bg" />
            <line x1="0" y1="0" x2="0" y2="4" className="hatch-line" />
          </pattern>
        </defs>
        {ticks.map((t) => (
          <g key={t}>
            <line x1={pad.l} x2={W - pad.r} y1={y(t)} y2={y(t)} className="grid" />
            <text x={pad.l - 6} y={y(t) + 4} className="axis" textAnchor="end">
              {t}
            </text>
          </g>
        ))}
        {daily.map((d, i) => {
          const x = pad.l + band * i + (band - bw) / 2
          let acc = 0
          const segs = series
            .map((s) => ({ s, v: d[s.key] }))
            .filter((x) => x.v > 0)
          return (
            <g key={d.date}>
              <rect x={pad.l + band * i} y={pad.t} width={band} height={H - pad.t - pad.b} className={`hit${hover === i ? ' on' : ''}`} onMouseEnter={() => setHover(i)} />
              {segs.map(({ s, v }, k) => {
                const y0 = y(acc)
                acc += v
                const y1 = y(acc)
                const last = k === segs.length - 1
                const gap = k > 0 ? 2 : 0 // 段之间留 2px 背景色
                const hgt = Math.max(1, y0 - y1 - gap)
                return (
                  <path
                    key={s.key}
                    className={`bar ${s.cls}${s.key === 'failed' ? ' hatched' : ''}`}
                    d={last ? roundTop(x, y1, bw, hgt, Math.min(4, bw / 2, hgt)) : `M${x},${y1}h${bw}v${hgt}h${-bw}z`}
                    pointerEvents="none"
                  />
                )
              })}
              {i % labelEvery === 0 && (
                <text x={pad.l + band * i + band / 2} y={H - 6} className="axis" textAnchor="middle">
                  {d.date.slice(5)}
                </text>
              )}
            </g>
          )
        })}
      </svg>
      {h && hover !== undefined && (
        <div className="chart-tip" style={{ left: `${((pad.l + band * (hover + 0.5)) / W) * 100}%` }}>
          <div className="strong">{h.date}</div>
          {series.map((s) => (
            <div key={s.key} className={`tip-row ${s.cls}`}>
              <i className={s.key === 'failed' ? 'hatched' : undefined} />
              {s.label}
              <span>{h[s.key]}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

function roundTop(x: number, y: number, w: number, h: number, r: number) {
  return `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}z`
}

function Distribution({ st }: { st: PeriodStat }) {
  const rows = [
    { label: '运行成功', v: st.success, cls: 'status-success' },
    { label: '运行失败', v: st.failed, cls: 'status-failed', hatched: true },
    { label: '已取消', v: st.canceled, cls: 'status-canceled' },
    { label: '运行中', v: st.running, cls: 'status-running' },
    { label: '排队中', v: st.queued, cls: 'status-queued' },
  ]
  const total = Math.max(1, st.runs)
  return (
    <div className="dist">
      <div className="dist-hero">
        <span className="dist-value">{st.success_rate < 0 ? '—' : `${Math.round(st.success_rate * 100)}%`}</span>
        <span className="muted small">成功率</span>
      </div>
      {rows.map((r) => (
        <div key={r.label} className={`dist-row ${r.cls}`} title={`${r.label} ${r.v} 次`}>
          <div className="dist-label">
            <i className={r.hatched ? 'hatched' : undefined} />
            {r.label}
            <span className="spacer" />
            <span>{r.v}</span>
            <span className="muted dist-pct">{((r.v / total) * 100).toFixed(1)}%</span>
          </div>
          <div className="dist-track">
            <div className={`dist-fill${r.hatched ? ' hatched' : ''}`} style={{ width: `${(r.v / total) * 100}%` }} />
          </div>
        </div>
      ))}
    </div>
  )
}
