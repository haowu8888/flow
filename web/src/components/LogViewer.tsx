import { FitAddon } from '@xterm/addon-fit'
import { Terminal } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'
import { useEffect, useImperativeHandle, useRef, type Ref } from 'react'
import { api } from '../api'
import type { Job } from '../types'

/** 断线后服务端返回错误（EventSource 不会自动重试）时的重连间隔。 */
const RETRY_MS = 3000

function decodeBase64(s: string) {
  const bin = atob(s)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

/**
 * 通过 SSE 接收并渲染 Job 日志。网络中断时 EventSource 带 Last-Event-ID（字节偏移）自动续传；
 * 服务端在 Job 结束且日志读完后发送 end 事件。
 */
export interface LogViewerHandle {
  /** 滚动到第 index 个步骤的开头（Runner 输出的 "==> [i/n]" 行）；-1 为日志开头。返回是否找到。 */
  scrollToStep: (index: number) => boolean
  /** 滚动到第一行以 prefix 开头的日志。 */
  scrollToPrefix: (prefix: string) => boolean
}

export function LogViewer({ job, handle }: { job: Job; handle?: Ref<LogViewerHandle> }) {
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal>()

  useImperativeHandle(handle, () => {
    const find = (prefix: string) => {
      const term = termRef.current
      if (!term) return false
      const buf = term.buffer.active
      for (let y = 0; y < buf.length; y++) {
        const text = buf.getLine(y)?.translateToString(true) ?? ''
        if (text.startsWith(prefix)) {
          term.scrollToLine(y)
          term.select(0, y, text.length)
          return true
        }
      }
      return false
    }
    return {
      scrollToStep(index) {
        if (index < 0) {
          termRef.current?.scrollToTop()
          return true
        }
        return find(`==> [${index + 1}/`)
      },
      scrollToPrefix: find,
    }
  })

  useEffect(() => {
    const el = containerRef.current!
    const term = new Terminal({
      convertEol: true,
      disableStdin: true,
      cursorStyle: 'bar',
      cursorInactiveStyle: 'none',
      fontSize: 13,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
      scrollback: 50_000,
      theme: { background: '#0f1115', foreground: '#d6d9e0' },
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(el)
    termRef.current = term
    fit.fit()
    const ro = new ResizeObserver(() => fit.fit())
    ro.observe(el)

    let offset = 0
    let es: EventSource | undefined
    let timer: ReturnType<typeof setTimeout>
    const open = () => {
      // 手动重连时用 offset 参数续传；EventSource 自身重连会带 Last-Event-ID。
      const source = new EventSource(`${api.logStreamURL(job.id)}?offset=${offset}`)
      es = source
      source.addEventListener('log', (ev) => {
        const e = ev as MessageEvent<string>
        term.write(decodeBase64(e.data)) // 字节写入，xterm 自行处理跨块的 UTF-8
        offset = Number(e.lastEventId) || offset
      })
      source.addEventListener('end', () => {
        source.close()
        if (offset === 0) term.write('\x1b[90m(无日志)\x1b[0m')
      })
      source.onerror = () => {
        if (source.readyState !== EventSource.CLOSED) return
        // 401 时由普通请求触发回到登录页
        api.readLog(job.id, offset).catch(() => {})
        timer = setTimeout(open, RETRY_MS)
      }
    }
    open()

    return () => {
      es?.close()
      clearTimeout(timer)
      ro.disconnect()
      termRef.current = undefined
      term.dispose()
    }
  }, [job.id])

  return <div className="log-viewer" ref={containerRef} />
}
