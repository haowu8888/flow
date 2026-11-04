export function fmtTime(iso?: string) {
  if (!iso) return '-'
  return new Date(iso).toLocaleString('zh-CN', { hour12: false })
}

export function fmtDuration(start?: string, end?: string) {
  if (!start) return '-'
  const ms = (end ? new Date(end) : new Date()).getTime() - new Date(start).getTime()
  const s = Math.max(0, Math.round(ms / 1000))
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${s % 60}s`
  return `${Math.floor(m / 60)}h ${m % 60}m`
}

/** 解析 "KEY=VALUE" 每行一个的文本；空行和 # 开头的行忽略。 */
export function parseEnv(text: string): Record<string, string> {
  const env: Record<string, string> = {}
  for (const raw of text.split('\n')) {
    const line = raw.trim()
    if (!line || line.startsWith('#')) continue
    const i = line.indexOf('=')
    if (i <= 0) throw new Error(`无效的变量行：${line}`)
    env[line.slice(0, i).trim()] = line.slice(i + 1)
  }
  return env
}

/** 将 env 序列化为 parseEnv 可解析的文本。 */
export function formatEnv(env?: Record<string, string>) {
  return Object.entries(env ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .join('\n')
}

/** 拆分逗号或换行分隔的列表。 */
export function splitList(text: string) {
  return text
    .split(/[,\n]/)
    .map((s) => s.trim())
    .filter(Boolean)
}

const triggerNames: Record<string, string> = { manual: '手动', webhook: 'Webhook', cron: '定时', api: 'API', rollback: '回滚' }

/** 运行的触发方式；手动 / API 触发附带操作人，webhook 触发附带分支 / 标签与短提交号。 */
export function triggerLabel(trigger: string, env?: Record<string, string>, createdBy?: string) {
  const name = triggerNames[trigger] ?? trigger
  if ((trigger === 'manual' || trigger === 'api' || trigger === 'rollback') && createdBy) return `${name}（${createdBy}）`
  if (trigger !== 'webhook' || !env) return name
  if (env.FLOW_GIT_EVENT === 'pull_request') {
    const pr = `合并请求 #${env.FLOW_GIT_PR_NUMBER} ${env.FLOW_GIT_PR_SOURCE_BRANCH} → ${env.FLOW_GIT_PR_TARGET_BRANCH}`
    const sha = env.FLOW_GIT_COMMIT?.slice(0, 8)
    return [name, pr, sha && `@${sha}`].filter(Boolean).join(' ')
  }
  const ref = env.FLOW_GIT_BRANCH ?? env.FLOW_GIT_TAG
  const sha = env.FLOW_GIT_COMMIT?.slice(0, 8)
  return [name, ref, sha && `@${sha}`].filter(Boolean).join(' ')
}

/** 复制到剪贴板；非 HTTPS 下 Clipboard API 不可用时退回 execCommand。 */
export async function copyText(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    return
  } catch {
    // 继续尝试降级方案
  }
  const el = document.createElement('textarea')
  el.value = text
  el.style.position = 'fixed'
  el.style.opacity = '0'
  document.body.appendChild(el)
  el.select()
  const ok = document.execCommand('copy')
  el.remove()
  if (!ok) throw new Error('复制失败，请手动选择复制')
}

/** 产物的保留期限说明。过期但尚未清理的，是流水线最近一次成功运行的产物（不会被清理）。 */
export function fmtExpiry(expiresAt?: string, now = Date.now()) {
  if (!expiresAt) return '永久保留'
  const ms = new Date(expiresAt).getTime() - now
  if (ms <= 0) return '最近一次成功运行，保留'
  const hours = ms / 3_600_000
  if (hours < 1) return '1 小时内过期'
  if (hours < 48) return `${Math.floor(hours)} 小时后过期`
  return `${Math.ceil(hours / 24)} 天后过期`
}

export function fmtSize(n: number) {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)} GB`
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`
  return `${n} B`
}

export const shortSha = (sha?: string) => sha?.slice(0, 8)

/** 秒数的中文时长：41秒、2分5秒、1小时3分。 */
export function fmtSeconds(sec: number) {
  const s = Math.max(0, Math.round(sec))
  if (s < 60) return `${s}秒`
  const m = Math.floor(s / 60)
  if (m < 60) return s % 60 ? `${m}分${s % 60}秒` : `${m}分`
  return `${Math.floor(m / 60)}小时${m % 60 ? `${m % 60}分` : ''}`
}

/** 相对时间：刚刚、5 分钟前、7 天前；超过 30 天显示日期。 */
export function fmtAgo(iso?: string, now = Date.now()) {
  if (!iso) return '-'
  const s = Math.round((now - new Date(iso).getTime()) / 1000)
  if (s < 60) return '刚刚'
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`
  if (s < 30 * 86400) return `${Math.floor(s / 86400)} 天前`
  return new Date(iso).toLocaleDateString('zh-CN')
}

/** 紧凑的日期时间：2026-10-26 23:20。 */
export function fmtShort(iso?: string) {
  if (!iso) return '-'
  const d = new Date(iso)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}
