import { createContext, useContext } from 'react'
import { roleRank, type Me, type Role } from './types'

export interface Session {
  me: Me
  /** 当前租户内角色是否不低于 role。 */
  can: (role: Role) => boolean
  /** 切换租户、修改资料后重新加载。 */
  setMe: (me: Me) => void
}

export const SessionContext = createContext<Session | null>(null)

export function useSession(): Session {
  const s = useContext(SessionContext)
  if (!s) throw new Error('useSession outside SessionContext')
  return s
}

export function makeSession(me: Me, setMe: (me: Me) => void): Session {
  return { me, setMe, can: (role) => roleRank[me.role] >= roleRank[role] }
}
