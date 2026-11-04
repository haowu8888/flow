import { useEffect, useState } from 'react'
import { api } from '../api'
import type { ServiceConnection, StepCatalog } from '../types'

// 步骤库与服务连接在编辑会话内共用，按需加载一次。
let catalogP: Promise<StepCatalog> | undefined
let connsP: Promise<ServiceConnection[]> | undefined

export function useStepCatalog() {
  const [c, setC] = useState<StepCatalog>()
  useEffect(() => {
    catalogP ??= api.stepKinds()
    catalogP.then(setC, () => (catalogP = undefined))
  }, [])
  return c
}

export function useConnections() {
  const [c, setC] = useState<ServiceConnection[]>()
  useEffect(() => {
    connsP ??= api.listConnections()
    connsP.then(setC, () => (connsP = undefined))
  }, [])
  return c
}

/** 新建服务连接后刷新缓存。 */
export function reloadConnections() {
  connsP = undefined
}

export const categoryLabel = (c: StepCatalog | undefined, v: string) => c?.categories.find((x) => x.value === v)?.label ?? v
