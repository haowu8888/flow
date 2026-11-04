import { useState } from 'react'
import { copyText } from '../format'

export function CopyButton({ text }: { text: string }) {
  const [state, setState] = useState<'idle' | 'ok' | 'fail'>('idle')
  async function copy() {
    try {
      await copyText(text)
      setState('ok')
    } catch {
      setState('fail')
    }
    setTimeout(() => setState('idle'), 1500)
  }
  return (
    <button className="btn link" onClick={copy}>
      {state === 'ok' ? '已复制' : state === 'fail' ? '复制失败' : '复制'}
    </button>
  )
}
