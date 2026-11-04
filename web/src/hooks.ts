import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from './api'
import type { RunDetail } from './types'

/** 加载数据并在 intervalMs 不为 null 时定时刷新；刷新失败保留旧数据。 */
export function usePolling<T>(load: () => Promise<T>, deps: unknown[], intervalMs: (data: T | undefined) => number | null) {
  const [data, setData] = useState<T>()
  const [error, setError] = useState<string>()
  const loadRef = useRef(load)
  loadRef.current = load

  const refresh = useCallback(async () => {
    try {
      setData(await loadRef.current())
      setError(undefined)
    } catch (e) {
      setError((e as Error).message)
    }
  }, [])

  useEffect(() => {
    setData(undefined)
    setError(undefined)
    refresh()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)

  const ms = intervalMs(data)
  useEffect(() => {
    if (ms == null) return
    const t = setInterval(refresh, ms)
    return () => clearInterval(t)
  }, [ms, refresh])

  return { data, error, refresh }
}

/** 每秒触发一次重渲染，用于运行中的计时显示。 */
export function useTicker(active: boolean) {
  const [, setTick] = useState(0)
  useEffect(() => {
    if (!active) return
    const t = setInterval(() => setTick((n) => n + 1), 1000)
    return () => clearInterval(t)
  }, [active])
}

/** 重连失败（HTTP 错误不会被 EventSource 自动重试）后的再次尝试间隔。 */
const STREAM_RETRY_MS = 5000

/**
 * 订阅 Run 详情的 SSE 推送。网络中断由 EventSource 自动重连；
 * 服务端返回错误（如 401 / 404）时拉取一次以获得错误信息（401 会回到登录页），稍后再试。
 * refresh 立即拉取一次，用于操作后马上看到结果。
 */
export function useRunStream(id: string) {
  const [data, setData] = useState<RunDetail>()
  const [error, setError] = useState<string>()
  // 收到推送的次数。refresh 期间若有推送到达，其结果可能比推送更旧，丢弃即可：
  // 服务端在内容变化时总会推送最新状态（只推送变化，所以被旧数据覆盖后不会自动纠正）。
  const pushes = useRef(0)

  const refresh = useCallback(async () => {
    const seen = pushes.current
    try {
      const d = await api.getRun(id)
      if (pushes.current !== seen) return
      setData(d)
      setError(undefined)
    } catch (e) {
      if (pushes.current === seen) setError((e as Error).message)
    }
  }, [id])

  useEffect(() => {
    setData(undefined)
    setError(undefined)
    let es: EventSource | undefined
    let timer: ReturnType<typeof setTimeout>
    const open = () => {
      const source = new EventSource(api.runEventsURL(id))
      es = source
      source.addEventListener('run', (ev) => {
        pushes.current++
        setData(JSON.parse((ev as MessageEvent<string>).data))
        setError(undefined)
      })
      source.onerror = () => {
        if (source.readyState !== EventSource.CLOSED) return
        refresh()
        timer = setTimeout(open, STREAM_RETRY_MS)
      }
    }
    open()
    return () => {
      es?.close()
      clearTimeout(timer)
    }
  }, [id, refresh])

  return { data, error, refresh }
}
