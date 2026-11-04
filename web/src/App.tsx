import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { api, UNAUTHORIZED_EVENT } from './api'
import { makeSession, SessionContext, useSession } from './auth'
import { Account, DangerZone, Security, Tokens } from './pages/Account'
import { Roles } from './pages/Roles'
import { Connections } from './pages/Connections'
import { Icon, type IconName } from './components/Icon'
import { Dashboard } from './pages/Dashboard'
import { Approvals } from './pages/Approvals'
import { Audit } from './pages/Audit'
import { Login } from './pages/Login'
import { Members } from './pages/Members'
import { NewPipeline } from './pages/NewPipeline'
import { PipelineDetail } from './pages/PipelineDetail'
import { PipelineList } from './pages/PipelineList'
import { RunDetail } from './pages/RunDetail'
import { Hosts } from './pages/Hosts'
import { Runners } from './pages/Runners'
import { Secrets } from './pages/Secrets'
import { TemplateEdit } from './pages/TemplateEdit'
import { Templates } from './pages/Templates'
import { usePolling } from './hooks'
import { href, useRoute, type Route } from './router'
import type { Me } from './types'

export function App() {
  // undefined：加载中；null：未登录。
  const [me, setMe] = useState<Me | null>()

  useEffect(() => {
    api.me().then(setMe, () => setMe(null))
    const onUnauthorized = () => setMe(null)
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized)
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized)
  }, [])

  const session = useMemo(() => (me ? makeSession(me, setMe) : null), [me])

  if (me === undefined) return <p className="muted center">加载中…</p>
  if (!session) return <Login onLogin={setMe} />
  return (
    <SessionContext.Provider value={session}>
      {/* 切换租户后重新挂载页面，避免展示上一个租户的数据 */}
      <Shell key={session.me.tenant_id} />
    </SessionContext.Provider>
  )
}

const sections: { label: string; href: string; icon: IconName; match: Route['page'][] }[] = [
  { label: '仪表盘', href: href.dashboard, icon: 'dashboard', match: ['dashboard'] },
  { label: '流水线', href: href.list, icon: 'pipeline', match: ['list', 'trash', 'new', 'pipeline', 'run', 'approvals'] },
  { label: 'API Key', href: href.tokens, icon: 'key', match: ['tokens'] },
  { label: '账户设置', href: href.account, icon: 'settings', match: ['account', 'runners', 'hosts', 'secrets', 'templates', 'template', 'members', 'audit', 'security', 'danger', 'roles', 'connections'] },
]

// 「账户设置」左侧的分区。
const settings: { page: Route['page'][]; label: string; href: string; icon: IconName; admin?: boolean }[] = [
  { page: ['account'], label: '常规', href: href.account, icon: 'user' },
  { page: ['runners'], label: '构建节点', href: href.runners, icon: 'server' },
  { page: ['hosts'], label: '环境与主机', href: href.hosts(), icon: 'host' },
  { page: ['secrets'], label: '通用变量', href: href.secrets, icon: 'list' },
  { page: ['connections'], label: '服务连接', href: href.connections, icon: 'box' },
  { page: ['templates'], label: '模板', href: href.templates, icon: 'template' },
  { page: ['members'], label: '团队成员', href: href.members, icon: 'users' },
  { page: ['roles'], label: '角色管理', href: href.roles, icon: 'key' },
  { page: ['audit'], label: '审计日志', href: href.audit, icon: 'list', admin: true },
  { page: ['security'], label: '安全', href: href.security, icon: 'shield' },
  { page: ['danger'], label: '危险操作', href: href.danger, icon: 'alert' },
]

function SettingsLayout({ page, children }: { page: Route['page']; children: ReactNode }) {
  const { can } = useSession()
  return (
    <div className="side-layout">
      <nav className="side-nav">
        {settings
          .filter((s) => !s.admin || can('admin'))
          .map((s) => (
            <a key={s.href} href={s.href} className={s.page.includes(page) ? 'active' : ''}>
              <Icon name={s.icon} />
              {s.label}
            </a>
          ))}
      </nav>
      <div className="side-main">{children}</div>
    </div>
  )
}

function Shell() {
  const route = useRoute()
  const { me } = useSession()
  const name = me.user.display_name || me.user.username
  const inSettings = settings.some((s) => s.page.includes(route.page))
  const page = (
    <>
      {route.page === 'dashboard' && <Dashboard />}
      {route.page === 'list' && <PipelineList />}
      {route.page === 'trash' && <PipelineList trash />}
      {route.page === 'new' && <NewPipeline key={route.template} templateId={route.template} />}
      {route.page === 'pipeline' && <PipelineDetail id={route.id} tab={route.tab} section={route.section} />}
      {route.page === 'run' && <RunDetail id={route.id} />}
      {route.page === 'tokens' && <Tokens />}
      {route.page === 'hosts' && <Hosts tab={route.tab} />}
      {route.page === 'runners' && <Runners />}
      {route.page === 'secrets' && <Secrets />}
      {route.page === 'audit' && <Audit />}
      {route.page === 'approvals' && <Approvals />}
      {route.page === 'templates' && <Templates />}
      {route.page === 'template' && <TemplateEdit key={route.id} id={route.id} />}
      {route.page === 'members' && <Members />}
      {route.page === 'account' && <Account />}
      {route.page === 'security' && <Security />}
      {route.page === 'danger' && <DangerZone />}
      {route.page === 'roles' && <Roles />}
      {route.page === 'connections' && <Connections />}
    </>
  )
  // 编辑器页面放宽内容宽度，给画布留出空间
  const wide = route.page === 'new' || route.page === 'template'
  return (
    <>
      <header className="topbar">
        <div className="topbar-row">
          <a className="brand" href={href.dashboard}>
            <span className="logo">
              <Icon name="pipeline" size={14} />
            </span>
            Flow
          </a>
          <div className="spacer" />
          <ApprovalBadge active={route.page === 'approvals'} />
          <TenantSwitcher />
          <a href={href.account} className="avatar" title={`${name} · 账户设置`}>
            {name.slice(0, 2)}
          </a>
        </div>
        <nav className="topnav">
          {sections.map((s) => (
            <a key={s.href} href={s.href} className={s.match.includes(route.page) ? 'active' : ''}>
              <Icon name={s.icon} />
              {s.label}
            </a>
          ))}
        </nav>
      </header>
      <main className={wide ? 'wide' : undefined}>{inSettings ? <SettingsLayout page={route.page}>{page}</SettingsLayout> : page}</main>
    </>
  )
}

/** 顶栏的待审批数量；没有时不显示（审批页仍可从链接进入）。 */
function ApprovalBadge({ active }: { active: boolean }) {
  const { can } = useSession()
  const { data } = usePolling(() => (can('developer') ? api.listApprovals() : Promise.resolve([])), [], () => 30000)
  if (!data?.length && !active) return null
  return (
    <a href={href.approvals} className={`approval-badge${active ? ' active' : ''}`} title="等待你审批的任务">
      待审批 <span className="count">{data?.length ?? 0}</span>
    </a>
  )
}

function TenantSwitcher() {
  const { me, setMe } = useSession()
  const [error, setError] = useState<string>()
  if (me.tenants.length < 2) return null

  async function change(id: string) {
    setError(undefined)
    try {
      setMe(await api.switchTenant(id))
      window.location.hash = href.list
    } catch (e) {
      setError((e as Error).message)
    }
  }

  return (
    <select className="tenant-select" value={me.tenant_id} title={error ?? '切换租户'} onChange={(e) => change(e.target.value)}>
      {me.tenants.map((t) => (
        <option key={t.id} value={t.id}>
          {t.name}
        </option>
      ))}
    </select>
  )
}
