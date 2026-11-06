import { useState, type ReactNode } from 'react'
import { api } from '../api'
import { useSession } from '../auth'
import { CopyButton } from './CopyButton'
import { Icon } from './Icon'
import { StatusBadge } from './StatusBadge'
import { commitURL, fmtExpiry, fmtShort, fmtSize, shortSha } from '../format'
import { usePolling } from '../hooks'
import { href, navigate } from '../router'
import { hasPerm, isTerminal, type Delivery, type Pipeline } from '../types'

const PAGE = 20

interface Item {
  key: string
  kind: string
  content: ReactNode
  size?: number
  action?: ReactNode
}

/** 流水线的部署历史：每次运行交付的产物包、网盘 / 对象存储上传与主机部署。 */
export function Deliveries({ pipeline, repo }: { pipeline: Pipeline; repo?: { url: string; branch?: string } }) {
  const [page, setPage] = useState(0)
  const [busy, setBusy] = useState<string>()
  const canDeploy = useSession().can('developer') && hasPerm(pipeline, 'run')
  const { data, error } = usePolling(() => api.listDeliveries(pipeline.id, page * PAGE, PAGE), [pipeline.id, page], (d) =>
    d?.items.some((x) => !isTerminal(x.run.status)) ? 3000 : 15000,
  )

  async function redeploy(d: Delivery, job: string, target: string) {
    if (!confirm(`把「${target}」重新部署为 #${d.run.number} 的版本？\n只执行部署任务 ${job}，沿用当时的配置、参数与构建产物，不重新构建。`)) return
    setBusy(d.run.id + job)
    try {
      const res = await api.redeploy(d.run.id, job)
      navigate(href.run(res.run.id))
    } catch (e) {
      alert((e as Error).message)
      setBusy(undefined)
    }
  }

  function items(d: Delivery): Item[] {
    const out: Item[] = []
    for (const a of d.artifacts) {
      const file = a.name ? `${a.name}.${a.format}` : `${a.job_name}.tar.gz`
      out.push({
        key: a.id,
        kind: '产物包',
        size: a.size,
        content: (
          <a className="delivery-file" href={api.artifactURL(a.id)} download title={`点击下载（${fmtExpiry(a.expires_at)}）`}>
            <Icon name="download" size={14} />
            {file}
            {a.version && <span className="tag">{/^v/i.test(a.version) ? a.version : `v${a.version}`}</span>}
            {a.storage && <span className="muted small">存于 {a.storage}</span>}
          </a>
        ),
        action: a.url && <CopyButton text={a.url} label="复制下载链接" title="免登录下载地址（带签名），构建物清理后失效" />,
      })
    }
    d.drives.forEach((x, i) =>
      out.push({
        key: `drive${i}`,
        kind: '网盘',
        size: x.size || undefined,
        content: (
          <span className="delivery-file">
            <Icon name="folder" size={14} />
            {x.url ? (
              <a href={x.url} target="_blank" rel="noreferrer" title="在网盘中打开">
                {x.dir}
              </a>
            ) : (
              x.dir
            )}
            <span className="muted small">{x.files} 个文件</span>
            {x.share_url && (
              <a className="small" href={x.share_url} target="_blank" rel="noreferrer">
                分享链接
              </a>
            )}
          </span>
        ),
        action: x.share_url && <CopyButton text={x.share_url} label="复制分享链接" />,
      }),
    )
    d.objects.forEach((x, i) =>
      out.push({
        key: `oss${i}`,
        kind: '对象存储',
        content: (
          <span className="delivery-file">
            <Icon name="box" size={14} />
            {x.url ? (
              <a href={x.url} target="_blank" rel="noreferrer">
                {x.bucket ? `${x.bucket}/` : ''}
                {x.path}
              </a>
            ) : (
              <span className="mono">
                {x.bucket ? `${x.bucket}/` : ''}
                {x.path}
              </span>
            )}
          </span>
        ),
        action: x.url && <CopyButton text={x.url} label="复制地址" />,
      }),
    )
    for (const dep of d.deployments) {
      const target = dep.environment_name || dep.group_name
      out.push({
        key: dep.id,
        kind: '主机部署',
        content: (
          <span className="delivery-file" title={dep.hosts.join('\n')}>
            <Icon name="server" size={14} />
            {dep.environment_name && <span>{dep.environment_name} /</span>}
            <span className="mono">{dep.group_name}</span>
            <span className="muted small">{dep.hosts.length} 台</span>
            {dep.status !== d.run.status && <StatusBadge status={dep.status} />}
          </span>
        ),
        action: canDeploy && dep.status === 'success' && (
          <button className="btn small" disabled={busy === d.run.id + dep.job_name} onClick={() => redeploy(d, dep.job_name, target)} title="回退或重放到这个版本">
            重新部署
          </button>
        ),
      })
    }
    return out
  }

  const total = data?.total ?? 0
  const pages = Math.ceil(total / PAGE)
  return (
    <>
      {error && <div className="alert">{error}</div>}
      {data && total === 0 && (
        <p className="muted">暂无部署 / 产物记录。流水线中的「构建物上传」「网盘上传」「OSS 上传」「主机部署」任务执行后会记录在这里。</p>
      )}
      {data && data.items.length > 0 && (
        <div className="table-wrap">
          <table className="deliveries">
            <thead>
              <tr>
                <th>记录</th>
                <th>运行记录</th>
                <th>状态</th>
                <th>代码版本</th>
                <th>类型</th>
                <th>产物包 / 部署目标</th>
                <th>大小</th>
                <th>开始部署时间</th>
                <th>执行人</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {data.items.map((d) => {
                const rows = items(d)
                const span = rows.length
                const branch = d.run.env?.FLOW_GIT_BRANCH ?? d.run.env?.FLOW_GIT_TAG ?? repo?.branch
                const link = commitURL(repo?.url, d.run.commit)
                return rows.map((it, i) => (
                  <tr key={d.run.id + it.key} className={i > 0 ? 'sub-row' : undefined}>
                    {i === 0 && (
                      <>
                        <td rowSpan={span} className="muted">
                          #{d.seq}
                        </td>
                        <td rowSpan={span}>
                          <a className="strong" href={href.run(d.run.id)}>
                            #{d.run.number}
                          </a>
                        </td>
                        <td rowSpan={span}>
                          <StatusBadge status={d.run.status} />
                        </td>
                        <td rowSpan={span} className="small">
                          {branch && <span className="mono">{branch} </span>}
                          {d.run.commit ? (
                            link ? (
                              <a className="mono" href={link} target="_blank" rel="noreferrer" title={`在仓库中查看提交 ${d.run.commit}`}>
                                <Icon name="commit" size={12} /> {shortSha(d.run.commit)}
                              </a>
                            ) : (
                              <span className="mono" title={d.run.commit}>
                                {shortSha(d.run.commit)}
                              </span>
                            )
                          ) : (
                            !branch && <span className="muted">—</span>
                          )}
                        </td>
                      </>
                    )}
                    <td className="muted small">{it.kind}</td>
                    <td>{it.content}</td>
                    <td className="muted">{it.size !== undefined ? fmtSize(it.size) : '—'}</td>
                    {i === 0 && (
                      <>
                        <td rowSpan={span} className="muted">
                          {fmtShort(d.started_at ?? d.run.started_at ?? d.run.created_at)}
                        </td>
                        <td rowSpan={span} className="muted">
                          {d.run.created_by || '自动'}
                        </td>
                      </>
                    )}
                    <td className="actions">{it.action}</td>
                  </tr>
                ))
              })}
            </tbody>
          </table>
        </div>
      )}
      {pages > 1 && (
        <div className="pager">
          <button className="btn small" disabled={page === 0} onClick={() => setPage(page - 1)}>
            上一页
          </button>
          <span className="muted small">
            {page + 1} / {pages} · 共 {total} 条
          </span>
          <button className="btn small" disabled={page + 1 >= pages} onClick={() => setPage(page + 1)}>
            下一页
          </button>
        </div>
      )}
    </>
  )
}
