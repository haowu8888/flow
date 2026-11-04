import {
  applyEdgeChanges,
  applyNodeChanges,
  Background,
  BaseEdge,
  Controls,
  EdgeLabelRenderer,
  getSmoothStepPath,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  type Connection,
  type Edge,
  type EdgeChange,
  type EdgeProps,
  type Node,
  type NodeChange,
  type NodeProps,
} from '@xyflow/react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useEditor } from './context'
import { isApproval, newStageNames, ops, type JobRef, type JobSpec } from './model'

const COL_W = 280
const CARD_W = 232
const GAP = COL_W - CARD_W
const JOB_Y0 = 56
const ROW = 92
const STAGE_H = 40

type JobData = { ref: JobRef; job: JobSpec; issues: string[]; selected: boolean; explicitDeps: boolean }
type StageData = { si: number; name: string; count: number; issues: string[]; selected: boolean }
type AddData = { si: number }
type GapData = { at: number }
type DropData = { vertical: boolean; height: number }
type EdgeData = { ref: JobRef; source: string; explicit: boolean }

type JobNodeT = Node<JobData, 'job'>
type StageNodeT = Node<StageData, 'stage'>
type AddNodeT = Node<AddData, 'add'>
type GapNodeT = Node<GapData, 'gap'>
type NewStageNodeT = Node<Record<string, never>, 'newstage'>
type DropNodeT = Node<DropData, 'drop'>
type AnyNode = JobNodeT | StageNodeT | AddNodeT | GapNodeT | NewStageNodeT | DropNodeT
type DepEdgeT = Edge<EdgeData, 'dep'>

const jobId = ({ si, ji }: JobRef) => `job:${si}:${ji}`
const parseJobId = (id: string): JobRef | undefined => {
  const m = /^job:(\d+):(\d+)$/.exec(id)
  return m ? { si: +m[1], ji: +m[2] } : undefined
}

function JobCard({ data }: NodeProps<JobNodeT>) {
  const { readOnly, hostGroups, environments } = useEditor()
  const { job, issues, selected } = data
  const approval = isApproval(job)
  const deploy = job.deploy
  const group = deploy && hostGroups.find((g) => g.name === deploy.hosts)
  const env = group && environments.find((e) => e.id === group.environment_id)
  const matrixSize = Object.values(job.matrix ?? {}).reduce((n, v) => n * Math.max(v.length, 1), 1)
  return (
    <div className={`ed-job${selected ? ' selected' : ''}${issues.length ? ' invalid' : ''}${approval ? ' approval' : ''}${deploy ? ' deploy' : ''}`} title={issues.join('\n')}>
      <Handle type="target" position={Position.Left} isConnectable={!readOnly} />
      <div className="ed-job-head">
        <span className="ed-job-icon" aria-hidden>
          {approval ? '✋' : deploy ? '⇪' : job.image ? '⬢' : '▸'}
        </span>
        <span className="ed-job-name">{job.name || <em className="muted">未命名</em>}</span>
        {issues.length > 0 && <span className="ed-job-err">!</span>}
      </div>
      <div className="ed-job-meta">
        {approval ? (
          <span>人工卡点{job.approvers?.length ? ` · ${job.approvers.join(', ')}` : ''}</span>
        ) : deploy ? (
          <>
            <span className="ed-job-env" title="部署到主机组">
              {env ? `${env.name} / ` : ''}
              {deploy.hosts || '未选择主机组'}
            </span>
            <span>· {job.steps.length} 个步骤</span>
          </>
        ) : (
          <>
            <span className="ed-job-env" title={job.image || 'Runner 主机'}>
              {job.image || '主机'}
            </span>
            <span>· {job.steps.length} 个步骤</span>
          </>
        )}
      </div>
      <div className="ed-job-tags">
        {matrixSize > 1 && <span className="tag">矩阵 ×{matrixSize}</span>}
        {job.when === 'always' && <span className="tag">总是执行</span>}
        {job.when === 'on_failure' && <span className="tag">失败时执行</span>}
        {!!job.retry && <span className="tag">重试 {job.retry}</span>}
        {job.runsOn?.length ? <span className="tag">{job.runsOn.join(',')}</span> : null}
        {deploy && (deploy.batch ?? 1) > 1 && <span className="tag">{(deploy.batch ?? 1) >= 1000 ? '全部并行' : `每批 ${deploy.batch} 台`}</span>}
        {env?.require_approval && <span className="tag">需审批</span>}
      </div>
      <Handle type="source" position={Position.Right} isConnectable={!readOnly} />
    </div>
  )
}

function StageHeader({ data }: NodeProps<StageNodeT>) {
  return (
    <div className={`ed-stage${data.selected ? ' selected' : ''}${data.issues.length ? ' invalid' : ''}`} title={data.issues.join('\n') || '拖动调整阶段顺序'}>
      <span className="ed-stage-name">{data.name || '未命名阶段'}</span>
      <span className="ed-stage-count">{data.count}</span>
    </div>
  )
}

function AddJob({ data }: NodeProps<AddNodeT>) {
  const { spec, pickTask } = useEditor()
  return (
    <div className="ed-add nodrag">
      <button onClick={() => pickTask(data.si, spec.stages[data.si].jobs.length)}>+ 添加任务</button>
    </div>
  )
}

function GapButton({ data }: NodeProps<GapNodeT>) {
  const { spec, apply, select } = useEditor()
  return (
    <button
      className="ed-gap nodrag"
      title="在此插入阶段"
      onClick={() => {
        const names = newStageNames(spec)
        apply(ops.addStage(data.at, names))
        select({ kind: 'stage', si: data.at, name: names.stage })
      }}
    >
      +
    </button>
  )
}

function NewStage() {
  const { spec, apply, select } = useEditor()
  return (
    <button
      className="ed-newstage nodrag"
      onClick={() => {
        const names = newStageNames(spec)
        apply(ops.addStage(spec.stages.length, names))
        select({ kind: 'job', ref: { si: spec.stages.length, ji: 0 }, name: names.job })
      }}
    >
      + 新阶段
      <small>或把任务拖到这里</small>
    </button>
  )
}

function DropMark({ data }: NodeProps<DropNodeT>) {
  return <div className={`ed-drop${data.vertical ? ' vertical' : ''}`} style={data.vertical ? { height: data.height } : undefined} />
}

function DepEdge({ id, data, selected, markerEnd, ...p }: EdgeProps<DepEdgeT>) {
  const { readOnly, deps, apply } = useEditor()
  const [path, lx, ly] = getSmoothStepPath({ ...p, borderRadius: 8 })
  const remove = () => {
    if (!data) return
    const cur = deps[data.ref.si]?.[data.ref.ji]?.deps ?? []
    apply(ops.setNeeds(data.ref, cur.filter((n) => n !== data.source)))
  }
  return (
    <>
      <BaseEdge id={id} path={path} markerEnd={markerEnd} className={`ed-edge${data?.explicit ? ' explicit' : ''}${selected ? ' selected' : ''}`} />
      {selected && !readOnly && (
        <EdgeLabelRenderer>
          <button
            className="ed-edge-del nodrag nopan"
            style={{ transform: `translate(-50%, -50%) translate(${lx}px, ${ly}px)` }}
            title="删除依赖"
            onClick={remove}
          >
            ×
          </button>
        </EdgeLabelRenderer>
      )}
    </>
  )
}

const nodeTypes = { job: JobCard, stage: StageHeader, add: AddJob, gap: GapButton, newstage: NewStage, drop: DropMark }
const edgeTypes = { dep: DepEdge }

type Slot = { kind: 'job'; si: number; ji: number } | { kind: 'stage'; to: number } | null

export function Canvas() {
  const { spec, deps, jobIssues, stageIssues, selection, select, readOnly, apply } = useEditor()
  const n = spec.stages.length

  const layout = useMemo(() => {
    const nodes: AnyNode[] = []
    spec.stages.forEach((st, si) => {
      const x = si * COL_W
      nodes.push({
        id: `stage:${si}`,
        type: 'stage',
        position: { x, y: 0 },
        data: { si, name: st.name, count: st.jobs.length, issues: stageIssues[si], selected: selection?.kind === 'stage' && selection.si === si },
        draggable: !readOnly,
        selectable: false,
        deletable: false,
      })
      st.jobs.forEach((job, ji) => {
        const ref = { si, ji }
        nodes.push({
          id: jobId(ref),
          type: 'job',
          position: { x, y: JOB_Y0 + ji * ROW },
          data: { ref, job, issues: jobIssues[si][ji], selected: selection?.kind === 'job' && selection.ref.si === si && selection.ref.ji === ji, explicitDeps: deps[si][ji].explicit },
          selected: selection?.kind === 'job' && selection.ref.si === si && selection.ref.ji === ji,
          draggable: !readOnly,
          deletable: !readOnly,
        })
      })
      if (!readOnly) {
        nodes.push({ id: `add:${si}`, type: 'add', position: { x, y: JOB_Y0 + st.jobs.length * ROW }, data: { si }, draggable: false, selectable: false, deletable: false })
        nodes.push({ id: `gap:${si}`, type: 'gap', position: { x: x - GAP / 2 - 11, y: STAGE_H / 2 - 11 }, data: { at: si }, draggable: false, selectable: false, deletable: false })
      }
    })
    if (!readOnly) {
      nodes.push({ id: 'newstage', type: 'newstage', position: { x: n * COL_W, y: 0 }, data: {}, draggable: false, selectable: false, deletable: false })
    }
    return nodes
  }, [spec, deps, jobIssues, stageIssues, selection, readOnly, n])

  const edges = useMemo(() => {
    const where = new Map<string, JobRef>()
    spec.stages.forEach((st, si) => st.jobs.forEach((j, ji) => !where.has(j.name) && where.set(j.name, { si, ji })))
    const out: DepEdgeT[] = []
    deps.forEach((col, si) =>
      col.forEach((d, ji) => {
        for (const name of new Set(d.deps)) {
          const src = where.get(name)
          if (!src) continue
          out.push({
            id: `e:${si}:${ji}:${name}`,
            source: jobId(src),
            target: jobId({ si, ji }),
            type: 'dep',
            data: { ref: { si, ji }, source: name, explicit: d.explicit },
            markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14 },
            deletable: !readOnly,
          })
        }
      }),
    )
    return out
  }, [spec, deps, readOnly])

  // 拖动期间节点位置由 React Flow 管理，结束后按数据重新布局。
  const [liveNodes, setLiveNodes] = useState<AnyNode[]>(layout)
  const [liveEdges, setLiveEdges] = useState<DepEdgeT[]>(edges)
  const [slot, setSlot] = useState<Slot>(null)
  useEffect(() => setLiveNodes(layout), [layout])
  useEffect(() => setLiveEdges(edges), [edges])

  const onNodesChange = useCallback(
    (changes: NodeChange<AnyNode>[]) => setLiveNodes((ns) => applyNodeChanges(changes.filter((c) => c.type === 'position' || c.type === 'dimensions'), ns)),
    [],
  )
  const onEdgesChange = useCallback(
    (changes: EdgeChange<DepEdgeT>[]) => setLiveEdges((es) => applyEdgeChanges(changes.filter((c) => c.type === 'select'), es)),
    [],
  )

  function slotFor(node: AnyNode): Slot {
    if (node.type === 'stage') {
      const to = Math.min(Math.max(Math.round(node.position.x / COL_W), 0), n - 1)
      return { kind: 'stage', to }
    }
    const si = Math.min(Math.max(Math.floor((node.position.x + CARD_W / 2 + GAP / 2) / COL_W), 0), n)
    const count = si < n ? spec.stages[si].jobs.length : 0
    const ji = Math.min(Math.max(Math.round((node.position.y - JOB_Y0) / ROW), 0), count)
    return { kind: 'job', si, ji }
  }

  const dropNode = useMemo((): DropNodeT | null => {
    if (!slot) return null
    if (slot.kind === 'stage') {
      const maxJobs = Math.max(1, ...spec.stages.map((s) => s.jobs.length))
      return { id: 'drop', type: 'drop', position: { x: slot.to * COL_W - 6, y: -4 }, data: { vertical: true, height: JOB_Y0 + maxJobs * ROW }, selectable: false, draggable: false }
    }
    return { id: 'drop', type: 'drop', position: { x: slot.si * COL_W, y: JOB_Y0 + slot.ji * ROW - 12 }, data: { vertical: false, height: 0 }, selectable: false, draggable: false }
  }, [slot, spec])

  const shown = useMemo(() => (dropNode ? [...liveNodes, dropNode] : liveNodes), [liveNodes, dropNode])

  function onDragStop(node: AnyNode) {
    const s = slotFor(node)
    setSlot(null)
    setLiveNodes(layout)
    if (!s) return
    if (node.type === 'stage' && s.kind === 'stage') {
      const from = node.data.si
      if (s.to !== from) {
        apply(ops.moveStage(from, s.to))
        select({ kind: 'stage', si: s.to, name: spec.stages[from].name })
      }
    } else if (node.type === 'job' && s.kind === 'job') {
      const from = node.data.ref
      apply(ops.moveJob(spec, from, { si: s.si, ji: s.ji }))
      // 选中项按名称重新定位。
      select({ kind: 'job', ref: from, name: node.data.job.name })
    }
  }

  function isValidConnection(c: Connection | Edge) {
    const s = parseJobId(c.source)
    const t = parseJobId(c.target)
    if (!s || !t || c.source === c.target || s.si > t.si) return false
    const name = spec.stages[s.si].jobs[s.ji].name
    return !deps[t.si][t.ji].deps.includes(name)
  }

  function onConnect(c: Connection) {
    const s = parseJobId(c.source)
    const t = parseJobId(c.target)
    if (!s || !t || !isValidConnection(c)) return
    const name = spec.stages[s.si].jobs[s.ji].name
    apply(ops.setNeeds(t, [...deps[t.si][t.ji].deps, name]))
  }

  async function onBeforeDelete({ nodes, edges }: { nodes: AnyNode[]; edges: DepEdgeT[] }) {
    const jobs = nodes.flatMap((x) => (x.type === 'job' ? [x.data.ref] : []))
    if (jobs.length) {
      // 从后往前删，下标不受影响。
      const sorted = [...jobs].sort((a, b) => b.si - a.si || b.ji - a.ji)
      apply((doc) => {
        let s = spec
        for (const ref of sorted) {
          ops.deleteJob(s, ref)(doc)
          s = structuredClone(s)
          s.stages[ref.si].jobs.splice(ref.ji, 1)
        }
      })
      select(null)
    } else {
      const byTarget = new Map<string, { ref: JobRef; drop: Set<string> }>()
      for (const e of edges) {
        if (!e.data) continue
        const k = `${e.data.ref.si}:${e.data.ref.ji}`
        const entry = byTarget.get(k) ?? { ref: e.data.ref, drop: new Set<string>() }
        entry.drop.add(e.data.source)
        byTarget.set(k, entry)
      }
      apply((doc) => {
        for (const { ref, drop } of byTarget.values()) ops.setNeeds(ref, deps[ref.si][ref.ji].deps.filter((x) => !drop.has(x)))(doc)
      })
    }
    return false
  }

  // 初始视口：左上对齐；放不下时缩小，但不小于可读的 0.75，其余横向拖动查看。
  const box = useRef<HTMLDivElement>(null)
  const initialZoom = () => {
    const w = box.current?.clientWidth ?? 800
    return Math.min(1, Math.max(0.75, (w - 80) / ((n + 1) * COL_W)))
  }

  return (
    <div className="ed-canvas" ref={box}>
      <ReactFlow<AnyNode, DepEdgeT>
        nodes={shown}
        edges={liveEdges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        onNodesChange={onNodesChange}
        onEdgesChange={onEdgesChange}
        onNodeClick={(_, node) => {
          if (node.type === 'job') select({ kind: 'job', ref: node.data.ref, name: node.data.job.name })
          if (node.type === 'stage') select({ kind: 'stage', si: node.data.si, name: node.data.name })
        }}
        onPaneClick={() => select(null)}
        onNodeDrag={(_, node) => setSlot(slotFor(node))}
        onNodeDragStop={(_, node) => onDragStop(node)}
        onConnect={onConnect}
        isValidConnection={isValidConnection}
        onBeforeDelete={onBeforeDelete}
        deleteKeyCode={readOnly ? null : ['Delete', 'Backspace']}
        nodesConnectable={!readOnly}
        nodesDraggable={!readOnly}
        elementsSelectable
        connectionRadius={30}
        onInit={(rf) => rf.setViewport({ x: 48, y: 48, zoom: initialZoom() })}
        fitViewOptions={{ padding: 0.15, maxZoom: 1 }}
        minZoom={0.3}
        maxZoom={1.5}
        panOnScroll
        zoomActivationKeyCode={['Control', 'Meta']}
        proOptions={{ hideAttribution: true }}
      >
        <Background gap={20} size={1} />
        <Controls showInteractive={false} position="bottom-left" />
      </ReactFlow>
      {!readOnly && <div className="ed-canvas-hint muted small">拖动卡片调整顺序或阶段 · 从卡片右侧圆点拖到另一张卡片添加依赖 · 选中后按 Delete 删除</div>}
    </div>
  )
}
