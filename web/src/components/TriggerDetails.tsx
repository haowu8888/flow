import { useState } from 'react'
import { commitURL } from '../format'
import type { TriggerInfo } from '../types'

const providers: Record<string, string> = { github: 'GitHub', gitlab: 'GitLab', gitea: 'Gitea', gogs: 'Gogs', gitee: '码云', generic: '流水线 Webhook' }

/** 触发信息的展开详情：推送人、分支、提交列表与对比链接；流水线 Webhook 覆盖的变量。 */
export function TriggerDetails({ info, label }: { info: TriggerInfo; label: string }) {
  const [open, setOpen] = useState(false)
  const commits = info.commits ?? []
  const total = info.total_commits ?? commits.length
  const source = providers[info.provider ?? ''] ?? info.provider ?? 'Webhook'
  return (
    <span className="trigger-details">
      <button className="btn link" onClick={() => setOpen((o) => !o)} title="查看触发详情">
        {label} {open ? '▴' : '▾'}
      </button>
      {open && (
        <div className="trigger-pop card">
          <div>
            <span className="strong">{info.pusher ?? source}</span>
            <span className="muted">
              {info.pusher && ` · ${source}`}
              {info.event && info.event !== 'push' && ` · ${info.event}`}
              {info.repo &&
                (info.repo_url ? (
                  <>
                    {' · '}
                    <a href={info.repo_url} target="_blank" rel="noreferrer">
                      {info.repo}
                    </a>
                  </>
                ) : (
                  ` · ${info.repo}`
                ))}
            </span>
          </div>
          {(info.branch || info.ref) && (
            <div className="muted small row-between">
              <span className="mono" title={info.ref}>
                {info.branch || info.ref}
              </span>
              {info.before && info.after && (
                <span className="mono">
                  {info.before.slice(0, 7)} → {info.after.slice(0, 7)}
                </span>
              )}
            </div>
          )}
          {commits.length > 0 && (
            <ul className="trigger-commits">
              {commits.map((c) => {
                const url = c.url ?? commitURL(info.repo_url, c.id)
                return (
                  <li key={c.id}>
                    {url ? (
                      <a className="mono" href={url} target="_blank" rel="noreferrer">
                        {c.id.slice(0, 8)}
                      </a>
                    ) : (
                      <span className="mono">{c.id.slice(0, 8)}</span>
                    )}
                    <span className="ellipsis" title={c.message}>
                      {c.message || '（无提交说明）'}
                    </span>
                    {c.author && c.author !== info.pusher && <span className="muted">{c.author}</span>}
                  </li>
                )
              })}
            </ul>
          )}
          {(total > commits.length || info.compare_url) && (
            <div className="muted small row-between">
              <span>{total > commits.length ? `共 ${total} 个提交` : ''}</span>
              {info.compare_url && (
                <a href={info.compare_url} target="_blank" rel="noreferrer">
                  查看对比
                </a>
              )}
            </div>
          )}
          {info.vars && info.vars.length > 0 && (
            <div className="muted small">
              覆盖变量 <span className="mono">{info.vars.join(', ')}</span>
            </div>
          )}
          {info.provider === 'generic' && !info.vars?.length && <div className="muted small">没有覆盖变量，按流水线配置运行</div>}
        </div>
      )}
    </span>
  )
}
