import type { Status } from '../types'

export const statusLabel: Record<Status, string> = {
  pending: '等待',
  queued: '排队',
  running: '运行中',
  waiting: '待审批',
  success: '成功',
  failed: '失败',
  skipped: '跳过',
  canceled: '已取消',
}

export function StatusBadge({ status }: { status: Status }) {
  return (
    <span className={`badge status-${status}`}>
      <span className="dot" />
      {statusLabel[status]}
    </span>
  )
}
