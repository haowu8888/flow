// 行级差异（最长公共子序列），用于对比流水线配置版本。

export type DiffLine = { type: 'same' | 'add' | 'del'; text: string; a?: number; b?: number }
export type Hunk = { lines: DiffLine[] } | { skipped: number }

const MAX_CELLS = 25_000_000 // 超过时退化为整体替换，避免卡住页面

export function diffLines(before: string, after: string): DiffLine[] {
  const a = before.split('\n')
  const b = after.split('\n')
  if (a[a.length - 1] === '' && b[b.length - 1] === '') {
    a.pop()
    b.pop()
  }
  // 先去掉公共前后缀，通常只剩很小的中间段。
  let start = 0
  while (start < a.length && start < b.length && a[start] === b[start]) start++
  let ea = a.length
  let eb = b.length
  while (ea > start && eb > start && a[ea - 1] === b[eb - 1]) {
    ea--
    eb--
  }
  const out: DiffLine[] = []
  for (let i = 0; i < start; i++) out.push({ type: 'same', text: a[i], a: i + 1, b: i + 1 })

  const n = ea - start
  const m = eb - start
  if (n * m > MAX_CELLS) {
    for (let i = start; i < ea; i++) out.push({ type: 'del', text: a[i], a: i + 1 })
    for (let j = start; j < eb; j++) out.push({ type: 'add', text: b[j], b: j + 1 })
  } else {
    // lcs[i][j]：a[start+i..ea) 与 b[start+j..eb) 的 LCS 长度。
    const w = m + 1
    const lcs = new Uint32Array((n + 1) * w)
    for (let i = n - 1; i >= 0; i--)
      for (let j = m - 1; j >= 0; j--)
        lcs[i * w + j] = a[start + i] === b[start + j] ? lcs[(i + 1) * w + j + 1] + 1 : Math.max(lcs[(i + 1) * w + j], lcs[i * w + j + 1])
    let i = 0
    let j = 0
    while (i < n || j < m) {
      if (i < n && j < m && a[start + i] === b[start + j]) {
        out.push({ type: 'same', text: a[start + i], a: start + i + 1, b: start + j + 1 })
        i++
        j++
      } else if (i < n && (j >= m || lcs[(i + 1) * w + j] >= lcs[i * w + j + 1])) {
        out.push({ type: 'del', text: a[start + i], a: start + i + 1 })
        i++
      } else {
        out.push({ type: 'add', text: b[start + j], b: start + j + 1 })
        j++
      }
    }
  }
  for (let k = 0; k < a.length - ea; k++) out.push({ type: 'same', text: a[ea + k], a: ea + k + 1, b: eb + k + 1 })
  return out
}

/** 只保留改动附近 context 行，其余折叠为 skipped。 */
export function hunks(lines: DiffLine[], context = 3): Hunk[] {
  const keep = lines.map(() => false)
  lines.forEach((l, i) => {
    if (l.type === 'same') return
    for (let k = Math.max(0, i - context); k <= Math.min(lines.length - 1, i + context); k++) keep[k] = true
  })
  const out: Hunk[] = []
  let cur: DiffLine[] = []
  let skipped = 0
  lines.forEach((l, i) => {
    if (keep[i]) {
      if (skipped) out.push({ skipped })
      skipped = 0
      cur.push(l)
    } else {
      if (cur.length) out.push({ lines: cur })
      cur = []
      skipped++
    }
  })
  if (cur.length) out.push({ lines: cur })
  if (skipped) out.push({ skipped })
  return out
}
