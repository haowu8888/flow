import { useMemo, useState } from 'react'
import { Modal } from '../components/Modal'
import { useEditor } from './context'
import { allJobNames, ops, uniqueName } from './model'
import { CATEGORIES, PRESETS, templatePreset, type Preset } from './presets'

/** 添加任务：按分类浏览或搜索组件，选中后插入到阶段 si 的 at 位置并选中新任务。 */
export function TaskPicker({ si, at, onClose }: { si: number; at: number; onClose: () => void }) {
  const { spec, apply, select, hostGroups, stepTemplates } = useEditor()
  const all = useMemo(() => [...PRESETS, ...stepTemplates.map(templatePreset)], [stepTemplates])
  const categories = useMemo(() => [...CATEGORIES, ...(stepTemplates.length ? ['团队模板'] : [])], [stepTemplates])
  const [category, setCategory] = useState<string>('全部')
  const [q, setQ] = useState('')

  const query = q.trim().toLowerCase()
  const shown = all.filter(
    (p) =>
      (category === '全部' || p.category === category) &&
      (!query || `${p.title} ${p.description} ${p.name}`.toLowerCase().includes(query)),
  )

  function add(p: Preset) {
    const name = uniqueName(p.name, allJobNames(spec))
    const job = structuredClone(p.job)
    const deploy = job.deploy as { hosts?: string } | undefined
    if (deploy && !deploy.hosts) deploy.hosts = hostGroups[0]?.name ?? ''
    apply(ops.insertJob(si, at, { name, ...job }))
    select({ kind: 'job', ref: { si, ji: at }, name })
    onClose()
  }

  return (
    <Modal title={`添加任务到「${spec.stages[si]?.name ?? ''}」`} onClose={onClose} wide>
      <div className="picker">
        <nav className="picker-cats">
          {['全部', ...categories].map((c) => (
            <button key={c} className={c === category ? 'active' : ''} onClick={() => setCategory(c)}>
              {c}
            </button>
          ))}
        </nav>
        <div className="picker-main">
          <input
            className="picker-search"
            autoFocus
            placeholder="搜索组件，如 go、docker、部署"
            value={q}
            onChange={(e) => {
              setQ(e.target.value)
              if (e.target.value.trim()) setCategory('全部') // 搜索范围是全部组件
            }}
            onKeyDown={(e) => e.key === 'Enter' && shown[0] && add(shown[0])}
          />
          {category === '主机部署' && hostGroups.length === 0 && (
            <div className="alert subtle">
              还没有主机组。先在 <a href="#/hosts" target="_blank" rel="noreferrer">环境与主机</a> 添加服务器并加入主机组，部署任务按主机组选择目标。
            </div>
          )}
          {shown.length === 0 ? (
            <p className="muted">没有匹配的组件，可以选择「Shell 脚本」自行编写命令。</p>
          ) : (
            <div className="picker-grid">
              {shown.map((p) => (
                <button key={p.id} className="picker-item" onClick={() => add(p)}>
                  <span className="picker-icon" aria-hidden>
                    {p.icon}
                  </span>
                  <span className="picker-text">
                    <strong>{p.title}</strong>
                    <span className="muted small">{p.description}</span>
                  </span>
                </button>
              ))}
            </div>
          )}
        </div>
      </div>
    </Modal>
  )
}
