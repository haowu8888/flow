import { useEffect, useState } from 'react'

// 基于 hash 的路由：Server 只需托管静态文件，无需 SPA 回退。
export type Route =
  | { page: 'dashboard' }
  | { page: 'list' }
  | { page: 'trash' }
  | { page: 'tokens' }
  | { page: 'new'; template?: string }
  | { page: 'pipeline'; id: string; tab: PipelineTab; section: EditSection }
  | { page: 'run'; id: string }
  | { page: 'hosts'; tab: HostsTab }
  | { page: 'runners' }
  | { page: 'secrets' }
  | { page: 'templates' }
  | { page: 'template'; id: string }
  | { page: 'members' }
  | { page: 'audit' }
  | { page: 'approvals' }
  | { page: 'account' }
  | { page: 'security' }
  | { page: 'danger' }
  | { page: 'roles' }
  | { page: 'connections' }

export type HostsTab = 'envs' | 'hosts' | 'deployments'
const hostsTabs: HostsTab[] = ['envs', 'hosts', 'deployments']

/** latest：最近一次运行；edit 为独立的编辑页，section 区分其中的分区。 */
export type PipelineTab = 'latest' | 'runs' | 'deploys' | 'stats' | 'edit'
const pipelineTabs: PipelineTab[] = ['latest', 'runs', 'deploys', 'stats', 'edit']
export type EditSection = 'flow' | 'basic' | 'triggers' | 'vars'
const editSections: EditSection[] = ['flow', 'basic', 'triggers', 'vars']
// 旧链接：触发器、密钥原是详情页的标签页
const legacyTabs: Record<string, EditSection> = { triggers: 'triggers', secrets: 'vars' }

export const href = {
  dashboard: '#/',
  list: '#/pipelines',
  trash: '#/trash',
  tokens: '#/tokens',
  newPipeline: '#/new',
  newFromTemplate: (id: string) => `#/new/${encodeURIComponent(id)}`,
  templates: '#/templates',
  /** id 为 new-steps / new-pipeline 时新建。 */
  template: (id: string) => `#/templates/${encodeURIComponent(id)}`,
  pipeline: (id: string, tab?: PipelineTab) => `#/pipelines/${id}${tab && tab !== 'latest' ? `/${tab}` : ''}`,
  edit: (id: string, section?: EditSection) => `#/pipelines/${id}/edit${section && section !== 'flow' ? `/${section}` : ''}`,
  run: (id: string) => `#/runs/${id}`,
  hosts: (tab?: HostsTab) => `#/hosts${tab && tab !== 'envs' ? `/${tab}` : ''}`,
  runners: '#/runners',
  secrets: '#/secrets',
  members: '#/members',
  audit: '#/audit',
  approvals: '#/approvals',
  account: '#/account',
  security: '#/security',
  danger: '#/danger',
  roles: '#/roles',
  connections: '#/connections',
}

export function navigate(to: string) {
  window.location.hash = to
}

function parse(hash: string): Route {
  const parts = hash.replace(/^#\/?/, '').split('/').filter(Boolean)
  if (parts[0] === 'new') return { page: 'new', template: parts[1] && decodeURIComponent(parts[1]) }
  if (parts[0] === 'pipelines' && !parts[1]) return { page: 'list' }
  if (parts[0] === 'pipelines') {
    const id = decodeURIComponent(parts[1])
    if (legacyTabs[parts[2]]) return { page: 'pipeline', id, tab: 'edit', section: legacyTabs[parts[2]] }
    const tab = pipelineTabs.find((t) => t === parts[2]) ?? 'latest'
    return { page: 'pipeline', id, tab, section: editSections.find((s) => s === parts[3]) ?? 'flow' }
  }
  if (parts[0] === 'runs' && parts[1]) return { page: 'run', id: decodeURIComponent(parts[1]) }
  if (parts[0] === 'templates' && parts[1]) return { page: 'template', id: decodeURIComponent(parts[1]) }
  if (parts[0] === 'hosts') return { page: 'hosts', tab: hostsTabs.find((t) => t === parts[1]) ?? 'envs' }
  if (parts[0] === 'runners' || parts[0] === 'secrets' || parts[0] === 'templates' || parts[0] === 'members' || parts[0] === 'audit' || parts[0] === 'approvals' || parts[0] === 'account' || parts[0] === 'trash' || parts[0] === 'tokens' ||
    parts[0] === 'security' || parts[0] === 'danger' || parts[0] === 'roles' || parts[0] === 'connections') {
    return { page: parts[0] }
  }
  return { page: 'dashboard' }
}

// 离开守卫：编辑器有未保存修改时，切换页面前确认。
let leaveGuard: (() => boolean) | null = null
let leaveScope: string | undefined // 在此前缀内切换（如编辑页的分区）不算离开
const LEAVE_MESSAGE = '有未保存的修改，确定离开？'

/** guard 返回 true 表示有未保存的修改；scope 为不需要确认的地址前缀；返回的函数用于解除。 */
export function setLeaveGuard(guard: () => boolean, scope?: string) {
  leaveGuard = guard
  leaveScope = scope
  const onBeforeUnload = (e: BeforeUnloadEvent) => {
    if (guard()) e.preventDefault()
  }
  window.addEventListener('beforeunload', onBeforeUnload)
  return () => {
    if (leaveGuard === guard) {
      leaveGuard = null
      leaveScope = undefined
    }
    window.removeEventListener('beforeunload', onBeforeUnload)
  }
}

/** 页面内切换（如标签页）前的确认；没有未保存修改时直接返回 true。 */
export function confirmLeave() {
  return !leaveGuard?.() || confirm(LEAVE_MESSAGE)
}

export function useRoute(): Route {
  const [route, setRoute] = useState(() => parse(window.location.hash))
  useEffect(() => {
    let current = window.location.hash
    const onChange = () => {
      if (window.location.hash === current) return
      const within = !!leaveScope && current.startsWith(leaveScope) && window.location.hash.startsWith(leaveScope)
      if (!within && !confirmLeave()) {
        // 恢复地址栏，不触发新的 hashchange。
        history.replaceState(null, '', current || '#/')
        return
      }
      current = window.location.hash
      setRoute(parse(current))
    }
    window.addEventListener('hashchange', onChange)
    return () => window.removeEventListener('hashchange', onChange)
  }, [])
  return route
}
