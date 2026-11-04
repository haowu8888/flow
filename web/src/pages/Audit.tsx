import { useEffect, useState } from 'react'
import { api } from '../api'
import { fmtTime } from '../format'
import { href } from '../router'
import type { AuditEvent } from '../types'

const actionLabels: Record<string, string> = {
  'pipeline.create': '新建流水线',
  'pipeline.update': '修改流水线配置',
  'pipeline.delete': '删除流水线',
  'pipeline.restore': '回退流水线配置',
  'template.restore': '回退模板版本',
  'pipeline.group': '设置流水线分组',
  'pipeline.info': '修改流水线说明 / 归档',
  'pipeline.recover': '恢复流水线',
  'run.trigger': '运行流水线',
  'run.cancel': '取消运行',
  'run.retry': '重试运行',
  'run.rollback': '回滚运行',
  'job.approve': '审批通过',
  'job.reject': '审批驳回',
  'trigger.create': '新建触发器',
  'trigger.update': '修改触发器',
  'trigger.delete': '删除触发器',
  'trigger.rotate_secret': '重置触发器密钥',
  'secret.put': '设置密钥',
  'secret.delete': '删除密钥',
  'template.create': '新建模板',
  'template.update': '修改模板',
  'template.delete': '删除模板',
  'environment.create': '新建环境',
  'environment.update': '修改环境',
  'environment.delete': '删除环境',
  'host_group.create': '新建主机组',
  'host_group.update': '修改主机组',
  'host_group.delete': '删除主机组',
  'host.create': '添加主机',
  'host.update': '修改主机',
  'host.delete': '删除主机',
  'host.test': '测试主机连接',
  'host.reset_key': '重置主机指纹',
  'runner.register': '注册 Runner',
  'runner.delete': '删除 Runner',
  'runner.update': '修改构建节点',
  'runner.token': '重置节点令牌',
  'host.agent_token': '生成主机 Agent 令牌',
  'member.add': '添加成员',
  'member.role': '修改成员角色',
  'member.remove': '移除成员',
  'member.reset_password': '重置成员密码',
  'token.create': '创建 API 令牌',
  'token.delete': '删除 API 令牌',
  'account.change_password': '修改自己的密码',
  'tenant.create': '新建租户',
}

const categories: [string, string][] = [
  ['', '全部操作'],
  ['pipeline', '流水线'],
  ['run', '运行'],
  ['job', '审批'],
  ['trigger', '触发器'],
  ['secret', '密钥'],
  ['host', '主机'],
  ['host_group', '主机组'],
  ['environment', '环境'],
  ['template', '模板'],
  ['member', '成员'],
  ['runner', 'Runner'],
  ['token', 'API 令牌'],
]

const PAGE = 50

/** 审计日志：成员的写操作记录，保留一年。 */
export function Audit() {
  const [category, setCategory] = useState('')
  const [actor, setActor] = useState('')
  const [events, setEvents] = useState<AuditEvent[]>()
  const [more, setMore] = useState(false)
  const [error, setError] = useState<string>()

  async function load(before?: string) {
    try {
      const page = await api.listAudit({ category, actor: actor.trim(), before, limit: PAGE })
      setEvents((cur) => (before ? [...(cur ?? []), ...page] : page))
      setMore(page.length === PAGE)
      setError(undefined)
    } catch (e) {
      setError((e as Error).message)
    }
  }
  useEffect(() => {
    setEvents(undefined)
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [category])

  return (
    <>
      <p className="muted">成员在本租户中的写操作（配置修改、运行、审批、密钥与主机变更等），保留一年。</p>
      <div className="list-toolbar">
        <select value={category} onChange={(e) => setCategory(e.target.value)}>
          {categories.map(([v, l]) => (
            <option key={v} value={v}>
              {l}
            </option>
          ))}
        </select>
        <form
          className="row"
          onSubmit={(e) => {
            e.preventDefault()
            setEvents(undefined)
            load()
          }}
        >
          <input className="list-search" placeholder="操作人用户名（回车筛选）" value={actor} onChange={(e) => setActor(e.target.value)} />
        </form>
      </div>
      {error && <div className="alert">{error}</div>}
      {!events ? (
        !error && <p className="muted">加载中…</p>
      ) : events.length === 0 ? (
        <p className="muted">没有记录。</p>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>时间</th>
                <th>操作人</th>
                <th>操作</th>
                <th>对象</th>
                <th>来源 IP</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e) => (
                <tr key={e.id}>
                  <td className="muted">{fmtTime(e.created_at)}</td>
                  <td className="strong">{e.actor}</td>
                  <td>
                    <span className={/delete|remove|reject|reset/.test(e.action) ? 'error-text' : undefined}>{actionLabels[e.action] ?? e.action}</span>
                  </td>
                  <td>
                    <Target e={e} />
                    {e.detail && <div className="muted small">{e.detail}</div>}
                  </td>
                  <td className="muted mono small">{e.ip}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {more && (
        <button className="btn" onClick={() => events && load(events[events.length - 1].created_at)}>
          加载更多
        </button>
      )}
    </>
  )
}

function Target({ e }: { e: AuditEvent }) {
  const name = e.target_name || e.target_id
  if (!name) return <span className="muted">—</span>
  const deleted = /delete|remove/.test(e.action)
  if (!deleted && e.target_id) {
    if (e.target_type === 'pipeline') return <a href={href.pipeline(e.target_id)}>{name}</a>
    if (e.target_type === 'run') return <a href={href.run(e.target_id)}>{name}</a>
    if (e.target_type === 'template') return <a href={href.template(e.target_id)}>{name}</a>
  }
  if (e.target_type === 'trigger') return <span>{name === 'cron' ? '定时触发器' : name === 'webhook' ? 'Git Webhook 触发器' : name}</span>
  return <span className={e.target_type === 'secret' ? 'mono' : undefined}>{name}</span>
}
