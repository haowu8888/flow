import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { parse as parseYaml } from 'yaml'
import { api } from '../api'
import { href } from '../router'
import type { RunnerInfo, TemplateRevision, VarGroup } from '../types'
import { useEditor } from './context'
import { ApprovalNotifyEditor, KindForm, PluginsEditor, StepPicker } from './StepKinds'
import { Field, Group, KvEditor, LazyInput, TagInput } from './fields'
import { isApproval, jobKind, jobPath, newStageNames, ops, type JobKind, type JobRef, type JobSpec, type ParamType, type StepSpec } from './model'

/** 受控文本框：本地保存草稿，只在外部值变化（撤销、改 YAML）时同步，避免光标跳动。 */
function useDraft(value: string, onChange: (v: string) => void) {
  const [draft, setDraft] = useState(value)
  const emitted = useRef(value)
  useEffect(() => {
    if (value !== emitted.current) {
      emitted.current = value
      setDraft(value)
    }
  }, [value])
  return [
    draft,
    (v: string) => {
      setDraft(v)
      emitted.current = v
      onChange(v)
    },
  ] as const
}

function Text({ value, onChange, mono, placeholder, disabled }: { value?: string; onChange: (v: string) => void; mono?: boolean; placeholder?: string; disabled?: boolean }) {
  const [draft, set] = useDraft(value ?? '', onChange)
  return <input className={mono ? 'mono' : undefined} value={draft} placeholder={placeholder} disabled={disabled} onChange={(e) => set(e.target.value)} />
}

function Script({ value, onChange, disabled }: { value?: string; onChange: (v: string) => void; disabled?: boolean }) {
  const [draft, set] = useDraft(value ?? '', onChange)
  const rows = Math.min(Math.max(draft.split('\n').length + 1, 3), 18)
  return <textarea className="ed-script" spellCheck={false} rows={rows} value={draft} placeholder="Shell 命令，如 make build" disabled={disabled} onChange={(e) => set(e.target.value)} />
}

export function SidePanel() {
  const { selection, select, spec } = useEditor()
  let body = <PipelinePanel />
  let title = '流水线设置'
  if (selection?.kind === 'job' && spec.stages[selection.ref.si]?.jobs[selection.ref.ji]) {
    body = <JobPanel key={`${selection.ref.si}:${selection.ref.ji}`} r={selection.ref} />
    title = isApproval(spec.stages[selection.ref.si].jobs[selection.ref.ji]) ? '人工卡点' : '任务'
  } else if (selection?.kind === 'stage' && spec.stages[selection.si]) {
    body = <StagePanel si={selection.si} />
    title = '阶段'
  }
  return (
    <aside className="ed-panel">
      <div className="ed-panel-head">
        <strong>{title}</strong>
        <div className="spacer" />
        {selection && (
          <button className="icon-btn" title="关闭" onClick={() => select(null)}>
            ×
          </button>
        )}
      </div>
      <div className="ed-panel-body">{body}</div>
    </aside>
  )
}

// ---- 任务 ----

function JobPanel({ r }: { r: JobRef }) {
  const { spec, deps, jobIssues, apply, select, readOnly } = useEditor()
  const job = spec.stages[r.si].jobs[r.ji]
  const p = jobPath(r)
  const key = (f: string) => `${r.si}:${r.ji}:${f}`
  const set = (f: string | (string | number)[], v: unknown) => apply(ops.set([...p, ...(Array.isArray(f) ? f : [f])], v), key(String(f)))
  const approval = isApproval(job)
  const kind = jobKind(job)
  const issues = jobIssues[r.si][r.ji]
  const { hostGroups } = useEditor()

  function changeKind(to: JobKind) {
    if (to === kind) return
    if (to === 'approval' && job.steps.length > 0 && !confirm('人工卡点不执行命令，将清除该任务的步骤、镜像、产物、缓存与矩阵配置。继续？')) return
    if (to === 'deploy' && (job.image || job.cache || job.matrix) && !confirm('主机部署的步骤在目标主机上执行，将清除镜像、缓存与矩阵配置。继续？')) return
    apply(ops.setKind(r, to, hostGroups[0]?.name))
  }

  return (
    <fieldset className="ed-form" disabled={readOnly}>
      {issues.length > 0 && (
        <div className="alert subtle">
          {issues.map((m) => (
            <div key={m}>{m}</div>
          ))}
        </div>
      )}
      <Field label="名称" hint="同一流水线内唯一；改名会同步更新其他任务的依赖">
        <LazyInput value={job.name} onCommit={(v) => {
          apply(ops.renameJob(spec, r, v))
          select({ kind: 'job', ref: r, name: v })
        }} />
      </Field>
      <div className="segmented full">
        {(
          [
            ['run', '执行命令'],
            ['deploy', '主机部署'],
            ['approval', '人工卡点'],
          ] as [JobKind, string][]
        ).map(([k, label]) => (
          <button key={k} type="button" className={kind === k ? 'active' : ''} onClick={() => changeKind(k)}>
            {label}
          </button>
        ))}
      </div>

      {approval ? (
        <>
          <Field label="审批人" block hint="用户名；留空为任意开发者及以上成员">
            <TagInput value={job.approvers ?? []} onChange={(v) => set('approvers', v.length ? v : undefined)} placeholder="输入用户名后回车" disabled={readOnly} />
          </Field>
          <ApprovalNotifyEditor r={r} job={job} />
        </>
      ) : kind === 'deploy' ? (
        <>
          <DeployFields r={r} job={job} />
          <Steps r={r} job={job} />
        </>
      ) : (
        <>
          <RunEnvFields r={r} job={job} />
          <Steps r={r} job={job} />
        </>
      )}

      <Group title="依赖与执行条件" open={deps[r.si][r.ji].explicit || !!job.when || !!job.if}>
        <Needs r={r} />
        <Field
          label="运行条件"
          hint="为空总是运行；支持 == != =~(正则) && || ! 和括号，变量用 FLOW_BRANCH、运行参数、上游任务的输出等，开始执行前求值，为假则跳过本任务"
        >
          <LazyInput className="mono" value={job.if ?? ''} placeholder="FLOW_BRANCH == 'main'" onCommit={(v) => set('if', v.trim() || undefined)} />
        </Field>
        <Field label="上游状态">
          <select value={job.when ?? ''} onChange={(e) => set('when', e.target.value)}>
            <option value="">上游全部成功时（默认）</option>
            <option value="on_failure">已有任务失败时</option>
            <option value="always">总是执行（不论上游成败）</option>
          </select>
        </Field>
      </Group>

      {!approval && (
        <>
          <Group title="环境变量" open={!!job.env && Object.keys(job.env).length > 0}>
            <KvEditor value={job.env} onChange={(d) => set('env', d)} disabled={readOnly} />
            <small className="muted">值中可用 $NAME 引用流水线变量；同名密钥会覆盖这里的值。</small>
          </Group>
          {kind !== 'deploy' && (
            <Group title="矩阵" open={!!job.matrix} extra={job.matrix ? undefined : '按变量组合并行执行'}>
              <Matrix r={r} job={job} />
            </Group>
          )}
          <Group title="产物、报告与缓存" open={!!job.artifacts?.length || !!job.cache || !!job.reports}>
            <Field label="产物路径" block hint="成功后上传，所有下游任务自动下载；相对工作目录，支持通配符">
              <TagInput mono value={job.artifacts ?? []} onChange={(v) => set('artifacts', v.length ? v : undefined)} placeholder="如 dist、build/*.jar" disabled={readOnly} />
            </Field>
            {kind === 'run' && (
              <Field label="JUnit 测试报告" block hint="步骤结束后（失败也会）解析，运行详情中展示用例统计与失败原因">
                <TagInput
                  mono
                  value={job.reports?.junit ?? []}
                  onChange={(v) => set(['reports', 'junit'], v.length ? v : undefined)}
                  placeholder="如 target/surefire-reports/*.xml"
                  disabled={readOnly}
                />
              </Field>
            )}
            <Field label="缓存 key" hint="可用变量，如 deps-$GOOS">
              <Text mono value={job.cache?.key} onChange={(v) => set(['cache', 'key'], v)} />
            </Field>
            <Field label="缓存路径" block>
              <TagInput mono value={job.cache?.paths ?? []} onChange={(v) => set(['cache', 'paths'], v.length ? v : undefined)} placeholder="如 node_modules、.m2" disabled={readOnly} />
            </Field>
          </Group>
        </>
      )}

      <Group title="任务插件" open={job.plugins.length > 0} extra={job.plugins.length ? `${job.plugins.length} 个` : undefined}>
        <PluginsEditor r={r} job={job} />
      </Group>

      <Group title="高级" open={!!job.timeout || !!job.retry || job.checkout === false}>
        <Field label="超时" hint="如 30m、1h30m；留空不限">
          <Text mono value={job.timeout} onChange={(v) => set('timeout', v)} placeholder="30m" />
        </Field>
        {!approval && (
          <>
            <Field label="失败重试次数">
              <select value={job.retry ?? 0} onChange={(e) => set('retry', +e.target.value || undefined)}>
                {[0, 1, 2, 3, 4, 5].map((n) => (
                  <option key={n} value={n}>
                    {n === 0 ? '不重试' : `${n} 次`}
                  </option>
                ))}
              </select>
            </Field>
            {kind === 'deploy' ? (
              <label className="check">
                <input type="checkbox" checked={job.checkout === true} onChange={(e) => set('checkout', e.target.checked || undefined)} />
                执行前在 Runner 上拉取代码（默认只下载上游产物）
              </label>
            ) : null}
          </>
        )}
      </Group>

      {!readOnly && (
        <div className="row ed-danger-zone">
          <button type="button" className="btn" onClick={() => apply(ops.duplicateJob(spec, r))}>
            复制任务
          </button>
          <button
            type="button"
            className="btn danger"
            onClick={() => {
              apply(ops.deleteJob(spec, r))
              select(null)
            }}
          >
            删除任务
          </button>
        </div>
      )}
    </fieldset>
  )
}

/** 主机部署的目标：主机组、上传内容、远程目录、分批。 */
function DeployFields({ r, job }: { r: JobRef; job: JobSpec }) {
  const { apply, hostGroups, environments, readOnly } = useEditor()
  const d = job.deploy ?? {}
  const p = [...jobPath(r), 'deploy']
  const set = (k: string, v: unknown) => apply(ops.set([...p, k], v), `${r.si}:${r.ji}:deploy.${k}`)
  const group = hostGroups.find((g) => g.name === d.hosts)
  const env = environments.find((e) => e.id === group?.environment_id)
  return (
    <div className="ed-deploy">
      <Field
        label="部署到主机组"
        hint={
          hostGroups.length === 0 ? (
            <>
              还没有主机组，先到 <a href="#/hosts" target="_blank" rel="noreferrer">环境与主机</a> 添加服务器
            </>
          ) : env ? (
            <>
              属于环境「{env.name}」{env.require_approval ? '，部署前需要审批' : ''}
            </>
          ) : (
            '步骤在组内每台主机上通过 SSH 执行'
          )
        }
      >
        <select value={d.hosts ?? ''} onChange={(e) => set('hosts', e.target.value)} disabled={readOnly}>
          {!group && <option value={d.hosts ?? ''}>{d.hosts ? `${d.hosts}（不存在）` : '请选择主机组'}</option>}
          {hostGroups.map((g) => {
            const e = environments.find((x) => x.id === g.environment_id)
            return (
              <option key={g.id} value={g.name}>
                {e ? `${e.name} / ` : ''}
                {g.name}
              </option>
            )
          })}
        </select>
      </Field>
      <Field label="上传内容" hint="工作目录中的目录或文件（通常是上游任务的产物），目录会上传其中的内容">
        <Text mono value={d.upload} onChange={(v) => set('upload', v)} placeholder="如 dist、target/app.jar" />
      </Field>
      <Field label="远程目录" hint="上传到这里，并作为脚本的工作目录">
        <Text mono value={d.target} onChange={(v) => set('target', v)} placeholder="/tmp/flow-deploy/流水线名" />
      </Field>
      <Field label="分批部署" hint="前一批全部成功才继续，失败即停止，剩余主机不受影响">
        <select value={d.batch ?? 1} onChange={(e) => set('batch', +e.target.value === 1 ? undefined : +e.target.value)}>
          <option value={1}>逐台部署（每批 1 台）</option>
          {[2, 3, 5, 10].map((n) => (
            <option key={n} value={n}>
              每批 {n} 台
            </option>
          ))}
          <option value={1000}>全部同时部署</option>
        </select>
      </Field>
      <p className="muted small">
        脚本可用变量：<code>$FLOW_DEPLOY_HOST</code>（主机名）、<code>$FLOW_DEPLOY_DIR</code>（远程目录），以及流水线变量与密钥。
      </p>
    </div>
  )
}

function Needs({ r }: { r: JobRef }) {
  const { spec, deps, apply } = useEditor()
  const d = deps[r.si][r.ji]
  const self = spec.stages[r.si].jobs[r.ji].name
  // 只能依赖当前及之前阶段的任务。
  const candidates = spec.stages.slice(0, r.si + 1).flatMap((st, si) => st.jobs.filter((j) => j.name && j.name !== self).map((j) => ({ name: j.name, stage: st.name, si })))
  const prevStage = r.si > 0 ? spec.stages[r.si - 1].name : undefined
  return (
    <div className="field">
      <span>依赖</span>
      <label className="check">
        <input type="radio" checked={!d.explicit} onChange={() => apply(ops.setNeeds(r, undefined))} />
        默认：{prevStage ? `等待上一阶段「${prevStage}」全部完成` : '第一阶段，无依赖'}
      </label>
      <label className="check">
        <input type="radio" checked={d.explicit} onChange={() => apply(ops.setNeeds(r, d.deps))} />
        自定义：只等待选中的任务（可提前启动）
      </label>
      {d.explicit && (
        <div className="ed-needs">
          {candidates.length === 0 && <span className="muted small">没有可依赖的任务</span>}
          {candidates.map((c) => (
            <label key={c.name} className="check">
              <input
                type="checkbox"
                checked={d.deps.includes(c.name)}
                onChange={(e) => apply(ops.setNeeds(r, e.target.checked ? [...d.deps, c.name] : d.deps.filter((x) => x !== c.name)))}
              />
              {c.name} <span className="muted small">{c.stage}</span>
            </label>
          ))}
          {d.deps.length === 0 && <span className="muted small">未选择：运行开始即执行</span>}
        </div>
      )}
    </div>
  )
}

function Matrix({ r, job }: { r: JobRef; job: JobSpec }) {
  const { apply, readOnly } = useEditor()
  const entries = Object.entries(job.matrix ?? {})
  const save = (next: [string, string[]][]) => apply(ops.set([...jobPath(r), 'matrix'], Object.fromEntries(next.filter(([k]) => k))))
  const total = entries.reduce((n, [, v]) => n * Math.max(v.length, 1), 1)
  return (
    <div className="ed-matrix">
      {entries.map(([k, vals], i) => (
        <div key={i} className="kv-row">
          <LazyInput className="mono" value={k} placeholder="变量名" onCommit={(nk) => save(entries.map((e, x) => (x === i ? [nk, e[1]] : e)))} />
          <TagInput mono value={vals} onChange={(nv) => save(entries.map((e, x) => (x === i ? [e[0], nv] : e)))} placeholder="取值，回车添加" disabled={readOnly} />
          {!readOnly && (
            <button type="button" className="icon-btn" aria-label="删除" onClick={() => save(entries.filter((_, x) => x !== i))}>
              ×
            </button>
          )}
        </div>
      ))}
      {!readOnly && (
        <button type="button" className="btn link small" onClick={() => {
          let name = 'OS'
          for (let n = 2; entries.some(([k]) => k === name); n++) name = `VAR${n}`
          save([...entries, [name, ['linux']]])
        }}>
          + 添加维度
        </button>
      )}
      {entries.length > 0 && <small className="muted">展开为 {total} 个并行任务，取值注入为同名环境变量</small>}
    </div>
  )
}

// ---- 步骤 ----

function Steps({ r, job }: { r: JobRef; job: JobSpec }) {
  const { apply, readOnly } = useEditor()
  const [picking, setPicking] = useState(false)
  const [dragFrom, setDragFrom] = useState<number | null>(null)
  const [dragOver, setDragOver] = useState<number | null>(null)
  return (
    <div className="field">
      <span>步骤（按顺序执行，任一失败即停止）</span>
      <div className="ed-steps">
        {job.steps.map((s, k) => (
          <div
            key={k}
            className={`ed-step${dragOver === k && dragFrom !== null && dragFrom !== k ? ' drag-over' : ''}`}
            onDragOver={(e) => {
              if (dragFrom === null) return
              e.preventDefault()
              setDragOver(k)
            }}
            onDrop={(e) => {
              e.preventDefault()
              if (dragFrom !== null) apply(ops.moveStep(r, dragFrom, k))
              setDragFrom(null)
              setDragOver(null)
            }}
          >
            <Step r={r} k={k} step={s} count={job.steps.length} onDragStart={() => setDragFrom(k)} onDragEnd={() => { setDragFrom(null); setDragOver(null) }} />
          </div>
        ))}
      </div>
      {!readOnly && (
        <button type="button" className="btn ed-add-step" onClick={() => setPicking(true)}>
          + 添加步骤
        </button>
      )}
      {picking && (
        <StepPicker
          onClose={() => setPicking(false)}
          onPick={(st) => {
            apply(ops.addStep(r, job.steps.length, st))
            setPicking(false)
          }}
        />
      )}
    </div>
  )
}

type Inputs = Record<string, { description?: string; default?: unknown; required?: boolean }>

function inputsOf(yaml: string): Inputs {
  try {
    return (parseYaml(yaml) as { inputs?: Inputs })?.inputs ?? {}
  } catch {
    return {}
  }
}

/** 解析 uses：name 或 name@N（N 为锁定的版本，0 表示最新）。 */
export function splitUses(uses?: string): { name: string; version: number } {
  const m = /^(.*)@(\d+)$/.exec(uses ?? '')
  return m ? { name: m[1], version: Number(m[2]) } : { name: uses ?? '', version: 0 }
}

// 模板的历史版本列表与各版本内容，按需加载并在会话内缓存。
const revCache = new Map<string, Promise<TemplateRevision[]>>()
const revYamlCache = new Map<string, Promise<string>>()
function useAsync<T>(key: string | undefined, load: () => Promise<T>): T | undefined {
  const [v, setV] = useState<{ key: string; value: T }>()
  useEffect(() => {
    if (!key) return
    let live = true
    load().then((value) => live && setV({ key, value }), () => {})
    return () => {
      live = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key])
  return v && v.key === key ? v.value : undefined
}
function cached<T>(m: Map<string, Promise<T>>, key: string, load: () => Promise<T>) {
  let p = m.get(key)
  if (!p) {
    p = load()
    p.catch(() => m.delete(key))
    m.set(key, p)
  }
  return p
}

/** 所引用模板（及锁定版本）的输入参数、可选版本。 */
function useTemplateRef(uses?: string) {
  const { stepTemplates } = useEditor()
  const { name, version } = splitUses(uses)
  const template = stepTemplates.find((x) => x.name === name)
  const revs = useAsync(template && `${template.id}@${template.version}`, () => cached(revCache, `${template!.id}@${template!.version}`, () => api.listTemplateRevisions(template!.id)))
  const pinnedKey = template && version && version !== template.version ? `${template.id}@${version}` : undefined
  const pinnedYaml = useAsync(pinnedKey, () => cached(revYamlCache, pinnedKey!, () => api.getTemplateRevision(template!.id, version).then((r) => r.yaml ?? '')))
  const inputs = useMemo(() => {
    if (!template) return undefined
    if (!pinnedKey) return inputsOf(template.yaml)
    return pinnedYaml === undefined ? undefined : inputsOf(pinnedYaml)
  }, [template, pinnedKey, pinnedYaml])
  return { name, version, template, revs, inputs }
}

function Step({ r, k, step, count, onDragStart, onDragEnd }: { r: JobRef; k: number; step: StepSpec; count: number; onDragStart: () => void; onDragEnd: () => void }) {
  const { apply, readOnly, stepTemplates } = useEditor()
  const p = [...jobPath(r), 'steps', k]
  const key = (f: string) => `step:${r.si}:${r.ji}:${k}:${f}`
  const set = (f: string, v: unknown) => apply(ops.set([...p, f], v), key(f))
  const usesTemplate = step.uses !== undefined
  const builtin = step.kind !== undefined
  const tmpl = useTemplateRef(step.uses)
  const [showEnv, setShowEnv] = useState(!!step.env && Object.keys(step.env).length > 0)

  function setMode(toTemplate: boolean) {
    if (toTemplate === usesTemplate) return
    apply((doc) => {
      if (toTemplate) {
        const t = stepTemplates[0]
        ops.set([...p, 'run'], undefined)(doc)
        ops.set([...p, 'uses'], t?.name ?? 'my-template')(doc)
      } else {
        ops.set([...p, 'uses'], undefined)(doc)
        ops.set([...p, 'with'], undefined)(doc)
        ops.set([...p, 'run'], 'echo hello')(doc)
      }
    })
  }

  /** 切换模板或版本，并预填新版本的必填参数。 */
  async function pickTemplate(name: string, version = 0) {
    const t = stepTemplates.find((x) => x.name === name)
    let yaml = t?.yaml ?? ''
    if (t && version && version !== t.version) {
      try {
        yaml = await cached(revYamlCache, `${t.id}@${version}`, () => api.getTemplateRevision(t.id, version).then((r) => r.yaml ?? ''))
      } catch {
        yaml = ''
      }
    }
    const required = Object.fromEntries(Object.entries(inputsOf(yaml)).filter(([, v]) => v?.required).map(([k2]) => [k2, step.with?.[k2] ?? '']))
    apply((doc) => {
      ops.set([...p, 'uses'], version ? `${name}@${version}` : name)(doc)
      ops.set([...p, 'with'], { ...required, ...step.with })(doc)
    })
  }

  const hints = tmpl.inputs && Object.fromEntries(Object.entries(tmpl.inputs).map(([n, v]) => [n, [v?.required ? '必填' : v?.default !== undefined ? `默认 ${String(v.default)}` : '可选', v?.description].filter(Boolean).join(' · ')]))
  const versions = tmpl.revs ?? (tmpl.template ? [{ version: tmpl.template.version, created_by: tmpl.template.updated_by, created_at: tmpl.template.updated_at }] : [])

  return (
    <>
      <div className="ed-step-head">
        {!readOnly && (
          <span className="ed-drag" draggable onDragStart={(e) => { e.dataTransfer.effectAllowed = 'move'; onDragStart() }} onDragEnd={onDragEnd} title="拖动排序">
            ⠿
          </span>
        )}
        <span className="ed-step-no">{k + 1}</span>
        <Text value={step.name} onChange={(v) => set('name', v)} placeholder={tmpl.name || `step-${k + 1}`} disabled={readOnly} />
        {!readOnly && (
          <>
            <button type="button" className="icon-btn" title="上移" disabled={k === 0} onClick={() => apply(ops.moveStep(r, k, k - 1))}>↑</button>
            <button type="button" className="icon-btn" title="下移" disabled={k === count - 1} onClick={() => apply(ops.moveStep(r, k, k + 1))}>↓</button>
            <button type="button" className="icon-btn" title="删除步骤" onClick={() => apply(ops.deleteStep(r, k))}>×</button>
          </>
        )}
      </div>
      {!builtin && (
        <div className="segmented small">
          <button type="button" className={usesTemplate ? '' : 'active'} onClick={() => setMode(false)}>
            命令
          </button>
          <button type="button" className={usesTemplate ? 'active' : ''} onClick={() => setMode(true)} title={stepTemplates.length ? '' : '还没有步骤模板，可在「模板」页创建'}>
            步骤模板
          </button>
        </div>
      )}
      {builtin ? (
        <KindForm r={r} k={k} step={step} />
      ) : usesTemplate ? (
        <>
          <div className="ed-uses">
            <select value={tmpl.name} onChange={(e) => pickTemplate(e.target.value)}>
              {!tmpl.template && <option value={tmpl.name}>{tmpl.name}（不存在）</option>}
              {stepTemplates.map((t) => (
                <option key={t.id} value={t.name}>
                  {t.name}
                  {t.description ? ` — ${t.description}` : ''}
                </option>
              ))}
            </select>
            <select
              value={tmpl.version}
              title="「最新」随模板更新；锁定版本后，模板之后的修改不影响此流水线"
              disabled={!tmpl.template}
              onChange={(e) => pickTemplate(tmpl.name, Number(e.target.value))}
            >
              <option value={0}>最新{tmpl.template ? `（v${tmpl.template.version}）` : ''}</option>
              {tmpl.version > 0 && !versions.some((v) => v.version === tmpl.version) && <option value={tmpl.version}>v{tmpl.version}（不存在）</option>}
              {versions.map((v) => (
                <option key={v.version} value={v.version}>
                  锁定 v{v.version}
                </option>
              ))}
            </select>
          </div>
          {tmpl.template && tmpl.version > 0 && tmpl.version < tmpl.template.version && (
            <small className="muted">
              已锁定 v{tmpl.version}，模板最新为 v{tmpl.template.version}。
              <a href={href.template(tmpl.template.id)} target="_blank" rel="noreferrer">
                查看变化
              </a>
            </small>
          )}
          <small className="muted">模板参数（with）</small>
          <KvEditor value={step.with} onChange={(d) => set('with', d)} hints={hints} disabled={readOnly} />
        </>
      ) : (
        <Script value={step.run} onChange={(v) => set('run', v)} disabled={readOnly} />
      )}
      {showEnv ? (
        <>
          <small className="muted">本步骤环境变量</small>
          <KvEditor value={step.env} onChange={(d) => set('env', d)} disabled={readOnly} />
        </>
      ) : (
        !readOnly && (
          <button type="button" className="btn link small" onClick={() => setShowEnv(true)}>
            + 本步骤环境变量
          </button>
        )
      )}
    </>
  )
}

// ---- 阶段 ----

function StagePanel({ si }: { si: number }) {
  const { spec, stageIssues, apply, select, readOnly } = useEditor()
  const st = spec.stages[si]
  const n = spec.stages.length
  const move = (to: number) => {
    apply(ops.moveStage(si, to))
    select({ kind: 'stage', si: to, name: st.name })
  }
  return (
    <fieldset className="ed-form" disabled={readOnly}>
      {stageIssues[si].length > 0 && <div className="alert subtle">{stageIssues[si].join('\n')}</div>}
      <Field label="阶段名称">
        <LazyInput value={st.name} onCommit={(v) => {
          apply(ops.set(['stages', si, 'name'], v))
          select({ kind: 'stage', si, name: v })
        }} />
      </Field>
      <p className="muted small">阶段之间串行，阶段内的任务并行执行。未自定义依赖的任务会等待上一阶段全部完成。</p>
      <div className="field">
        <span>任务（{st.jobs.length}）</span>
        <ul className="ed-stage-jobs">
          {st.jobs.map((j, ji) => (
            <li key={ji}>
              <button type="button" className="btn link" onClick={() => select({ kind: 'job', ref: { si, ji }, name: j.name })}>
                {isApproval(j) ? '✋ ' : ''}
                {j.name || '未命名'}
              </button>
            </li>
          ))}
        </ul>
      </div>
      {!readOnly && (
        <>
          <div className="row">
            <button type="button" className="btn" disabled={si === 0} onClick={() => move(si - 1)}>
              ← 左移
            </button>
            <button type="button" className="btn" disabled={si === n - 1} onClick={() => move(si + 1)}>
              右移 →
            </button>
            <button
              type="button"
              className="btn"
              onClick={() => {
                const names = newStageNames(spec)
                apply(ops.addStage(si + 1, names))
                select({ kind: 'stage', si: si + 1, name: names.stage })
              }}
            >
              在右侧插入阶段
            </button>
          </div>
          <div className="row ed-danger-zone">
            <button
              type="button"
              className="btn danger"
              disabled={n === 1}
              title={n === 1 ? '至少保留一个阶段' : undefined}
              onClick={() => {
                if (st.jobs.length > 0 && !confirm(`删除阶段「${st.name}」及其中 ${st.jobs.length} 个任务？`)) return
                apply(ops.deleteStage(spec, si))
                select(null)
              }}
            >
              删除阶段
            </button>
          </div>
        </>
      )}
    </fieldset>
  )
}

// ---- 流水线 ----

const notifyTypes: [string, string][] = [
  ['dingtalk', '钉钉'],
  ['feishu', '飞书'],
  ['wecom', '企业微信'],
  ['webhook', '通用 Webhook'],
]
const notifyEvents: [string, string][] = [
  ['success', '成功'],
  ['failed', '失败'],
  ['canceled', '取消'],
]

function PipelinePanel() {
  const { readOnly, spec } = useEditor()
  return (
    <fieldset className="ed-form" disabled={readOnly}>
      <NameField />
      <EnvField />
      <Params />
      <Group title="代码源" open={!!spec.source}>
        <SourceFields />
      </Group>
      <ConcurrencyGroup />
      <NotifyGroup />
      <Group title="保留策略" open={!!spec.retention}>
        <RetentionFields />
      </Group>
    </fieldset>
  )
}

export type SettingsSection = 'basic' | 'triggers' | 'vars'

/** 编辑页「基本信息 / 触发设置 / 变量和缓存」中属于配置（YAML）的部分，以卡片排列。 */
export function PipelineSettings({ section }: { section: SettingsSection }) {
  const { readOnly, spec } = useEditor()
  return (
    <fieldset className="ed-form ed-settings" disabled={readOnly}>
      {section === 'basic' && (
        <>
          <SettingsCard title="流水线名称">
            <NameField bare />
          </SettingsCard>
          <SettingsCard title="流水线源" desc="配置后每个任务执行前自动拉取代码；Webhook 触发时检出推送的提交">
            <SourceFields />
          </SettingsCard>
          <SettingsCard title="通知" desc="运行结束时按结果推送到机器人或 Webhook">
            <NotifyFields />
          </SettingsCard>
          <SettingsCard title="保留策略" desc="产物与运行记录的清理规则">
            <RetentionFields />
          </SettingsCard>
        </>
      )}
      {section === 'triggers' && (
        <SettingsCard title="并发度限制" desc="同一时间只执行一个运行，超出的触发排队，按触发顺序等前面的运行结束后再跑">
          <ConcurrencyFields />
        </SettingsCard>
      )}
      {section === 'vars' && (
        <>
          <SettingsCard title="变量" desc="以环境变量注入每个步骤，脚本里用 $NAME 读取；开启「运行时设置」的变量可在运行弹窗或流水线 Webhook 中覆盖默认值" extra={`${spec.vars.length + Object.keys(spec.env ?? {}).length} 个`}>
            <VarsTable />
          </SettingsCard>
          <SettingsCard title="通用变量组" desc="关联「账户设置 → 通用变量」里维护的变量组，运行时按组的创建顺序注入；同名时流水线变量优先。未指定关联时默认注入全部变量组">
            <VarGroupsField />
          </SettingsCard>
          <SettingsCard title="缓存目录" desc="把依赖缓存目录挂成按流水线隔离的持久卷，下次运行直接复用，减少安装时间（任务在容器中运行时生效）">
            <CacheDirsField />
          </SettingsCard>
          <SettingsCard title="参数化构建" desc="配置后点「运行」会先弹窗填这些参数再触发；没有参数时直接执行。自动触发时使用默认值" extra={spec.params.length ? `${spec.params.length} 个` : undefined}>
            <ParamFields />
          </SettingsCard>
        </>
      )}
    </fieldset>
  )
}

export function SettingsCard({ title, desc, extra, children }: { title: string; desc?: ReactNode; extra?: ReactNode; children: ReactNode }) {
  return (
    <section className="card settings-card">
      <div className="settings-card-head">
        <h3>{title}</h3>
        {extra && <span className="muted small">{extra}</span>}
      </div>
      {desc && <p className="muted small">{desc}</p>}
      <div className="settings-card-body">{children}</div>
    </section>
  )
}

function useSet() {
  const { apply } = useEditor()
  return (path: (string | number)[], v: unknown) => apply(ops.set(path, v), path.join('.'))
}

function NameField({ bare }: { bare?: boolean }) {
  const { spec } = useEditor()
  const set = useSet()
  const input = <LazyInput value={spec.name} onCommit={(v) => set(['name'], v)} />
  return bare ? input : <Field label="流水线名称">{input}</Field>
}

function EnvField({ bare }: { bare?: boolean }) {
  const { spec, readOnly } = useEditor()
  const set = useSet()
  const editor = <KvEditor value={spec.env} onChange={(d) => set(['env'], d)} disabled={readOnly} />
  return bare ? (
    editor
  ) : (
    <Field label="全局变量" block hint="对所有任务生效；运行时可用同名参数覆盖">
      {editor}
    </Field>
  )
}

function SourceFields() {
  const { spec } = useEditor()
  const set = useSet()
  return (
    <>
      <Field label="仓库地址" hint="配置后每个任务执行前自动拉取代码">
        <Text mono value={spec.source?.url} onChange={(v) => set(['source', 'url'], v)} placeholder="https://git.example.com/team/app.git" />
      </Field>
      <Field label="默认分支">
        <Text mono value={spec.source?.branch} onChange={(v) => set(['source', 'branch'], v)} placeholder="main" />
      </Field>
      <Field label="访问令牌（密钥名）" hint="在「变量和缓存 → 密钥」或「账户设置 → 通用变量」添加，这里填写名称">
        <Text mono value={spec.source?.token_secret} onChange={(v) => set(['source', 'token_secret'], v)} placeholder="GIT_TOKEN" />
      </Field>
      <Field label="克隆深度">
        <input type="number" min={0} value={spec.source?.depth ?? ''} placeholder="50" onChange={(e) => set(['source', 'depth'], e.target.value === '' ? undefined : +e.target.value)} />
      </Field>
      <label className="check">
        <input type="checkbox" checked={!!spec.source?.report_status} onChange={(e) => set(['source', 'report_status'], e.target.checked || undefined)} />
        把运行结果回写到提交 / 合并请求
      </label>
      {spec.source?.report_status && (
        <Field label="代码平台" hint="令牌需要有写提交状态的权限；平台按仓库地址推断，私有部署时请手动选择">
          <select value={spec.source.provider ?? ''} onChange={(e) => set(['source', 'provider'], e.target.value || undefined)}>
            <option value="">自动识别</option>
            <option value="github">GitHub</option>
            <option value="gitlab">GitLab</option>
            <option value="gitea">Gitea</option>
          </select>
        </Field>
      )}
    </>
  )
}

function ConcurrencyFields() {
  const { spec, apply } = useEditor()
  return (
    <>
      <label className="check">
        <input type="checkbox" checked={!!spec.concurrency} onChange={(e) => apply(ops.setConcurrency(e.target.checked))} />
        限制同时执行的运行数，超出的按触发顺序排队
      </label>
      {spec.concurrency && (
        <>
          <Field label="最大并发" hint="同一并发组同时执行的运行数上限，默认 1；调大后排队的运行马上可以开跑">
            <input
              type="number"
              min={1}
              max={20}
              value={spec.concurrency.max ?? 1}
              onChange={(e) => apply(ops.setConcurrencyField('max', e.target.value === '' || +e.target.value <= 1 ? undefined : Math.min(20, +e.target.value)), 'concurrency.max')}
            />
          </Field>
          <Field label="并发组" hint={<>留空表示只与本流水线的运行互斥；填写同一组名的流水线之间也互斥，可用参数，如 <code>deploy-${'{TARGET}'}</code></>}>
            <LazyInput className="mono" value={spec.concurrency.group ?? ''} placeholder="（本流水线）" onCommit={(v) => apply(ops.setConcurrencyField('group', v.trim()), 'concurrency.group')} />
          </Field>
          <label className="check">
            <input
              type="checkbox"
              disabled={(spec.concurrency.max ?? 1) > 1}
              checked={!!spec.concurrency.cancel_previous}
              onChange={(e) => apply(ops.setConcurrencyField('cancel_previous', e.target.checked))}
            />
            新运行开始时取消之前未结束的运行（最大并发为 1 时可用）
          </label>
        </>
      )}
    </>
  )
}

function ConcurrencyGroup() {
  const { spec } = useEditor()
  return (
    <Group title="并发控制" open={!!spec.concurrency} extra={spec.concurrency ? '已开启' : undefined}>
      <ConcurrencyFields />
    </Group>
  )
}

function NotifyFields() {
  const { spec, apply, readOnly } = useEditor()
  const set = useSet()
  return (
    <>
      {spec.notify.map((nt, i) => (
        <div key={i} className="ed-notify">
          <div className="row">
            <select value={nt.type ?? ''} onChange={(e) => set(['notify', i, 'type'], e.target.value)}>
              {notifyTypes.map(([v, l]) => (
                <option key={v} value={v}>
                  {l}
                </option>
              ))}
            </select>
            <div className="spacer" />
            {!readOnly && (
              <button type="button" className="icon-btn" title="删除" onClick={() => apply(ops.deleteNotify(i))}>
                ×
              </button>
            )}
          </div>
          <Text mono value={nt.url} onChange={(v) => set(['notify', i, 'url'], v)} placeholder="机器人 Webhook 地址，可用 ${SECRET}" />
          {(nt.type === 'dingtalk' || nt.type === 'feishu') && (
            <Text mono value={nt.secret} onChange={(v) => set(['notify', i, 'secret'], v)} placeholder="加签密钥（可选），如 ${DING_SECRET}" />
          )}
          <div className="row">
            {notifyEvents.map(([v, l]) => {
              const on = nt.on ?? ['failed']
              return (
                <label key={v} className="check">
                  <input type="checkbox" checked={on.includes(v)} onChange={(e) => set(['notify', i, 'on'], e.target.checked ? [...on, v] : on.filter((x) => x !== v))} />
                  {l}
                </label>
              )
            })}
          </div>
        </div>
      ))}
      {!readOnly && (
        <button type="button" className="btn link small" onClick={() => apply(ops.addNotify())}>
          + 添加通知
        </button>
      )}
    </>
  )
}

function NotifyGroup() {
  const { spec } = useEditor()
  return (
    <Group title="通知" open={spec.notify.length > 0} extra={spec.notify.length ? `${spec.notify.length} 个` : undefined}>
      <NotifyFields />
    </Group>
  )
}

function RetentionFields() {
  const { spec } = useEditor()
  const set = useSet()
  return (
    <>
      <Field label="产物保留时长" hint="如 7d、36h；0 为永久；留空用服务端默认">
        <Text mono value={spec.retention?.artifacts} onChange={(v) => set(['retention', 'artifacts'], v)} placeholder="7d" />
      </Field>
      <Field label="最多保留运行数" hint="超出的旧运行连同日志、产物一起删除">
        <input type="number" min={0} value={spec.retention?.runs ?? ''} placeholder="不限" onChange={(e) => set(['retention', 'runs'], e.target.value === '' ? undefined : +e.target.value)} />
      </Field>
    </>
  )
}

const paramTypes: [ParamType, string][] = [
  ['string', '文本'],
  ['choice', '单选'],
  ['boolean', '开关'],
]

/** 运行参数：手动运行时弹出表单填写，值注入为同名环境变量。 */
function Params() {
  const n = useEditor().spec.params.length
  return (
    <Group title="运行参数" open={n > 0} extra={n ? `${n} 个` : undefined}>
      <p className="muted small">手动运行时填写，作为同名环境变量注入所有任务；自动触发时使用默认值。</p>
      <ParamFields />
    </Group>
  )
}

function ParamFields() {
  const { spec, apply, readOnly } = useEditor()
  const set = (path: (string | number)[], v: unknown) => apply(ops.set(path, v), path.join('.'))
  return (
    <>
      {spec.params.map((p, i) => (
        <div key={i} className="ed-param">
          <div className="row">
            <LazyInput className="mono" value={p.name} placeholder="变量名，如 TARGET" onCommit={(v) => set(['params', i, 'name'], v)} disabled={readOnly} />
            <select value={p.type} onChange={(e) => apply(ops.setParamType(spec, i, e.target.value as ParamType))}>
              {paramTypes.map(([v, l]) => (
                <option key={v} value={v}>
                  {l}
                </option>
              ))}
            </select>
            {!readOnly && (
              <>
                <button type="button" className="icon-btn" title="上移" disabled={i === 0} onClick={() => apply(ops.moveParam(i, i - 1))}>
                  ↑
                </button>
                <button type="button" className="icon-btn" title="删除" onClick={() => apply(ops.deleteParam(i))}>
                  ×
                </button>
              </>
            )}
          </div>
          {p.type === 'choice' && (
            <TagInput mono value={p.options ?? []} onChange={(v) => set(['params', i, 'options'], v)} placeholder="可选值，回车添加" disabled={readOnly} />
          )}
          <div className="row">
            <span className="muted small">默认</span>
            {p.type === 'string' ? (
              <Text mono value={p.default} onChange={(v) => set(['params', i, 'default'], v)} placeholder="（无）" />
            ) : (
              <select value={p.default ?? ''} onChange={(e) => set(['params', i, 'default'], e.target.value)}>
                <option value="">{p.type === 'boolean' ? '（false）' : '（无）'}</option>
                {(p.type === 'boolean' ? ['true', 'false'] : (p.options ?? [])).map((o) => (
                  <option key={o} value={o}>
                    {o}
                  </option>
                ))}
              </select>
            )}
            {p.type !== 'boolean' && (
              <label className="check">
                <input type="checkbox" checked={!!p.required} onChange={(e) => set(['params', i, 'required'], e.target.checked || undefined)} />
                必填
              </label>
            )}
          </div>
          <Text value={p.description} onChange={(v) => set(['params', i, 'description'], v)} placeholder="说明（显示在运行表单中）" />
        </div>
      ))}
      {!readOnly && (
        <button type="button" className="btn link small" onClick={() => apply(ops.addParam(spec))}>
          + 添加参数
        </button>
      )}
    </>
  )
}

/** 变量表：vars（可设说明与运行时开关）；env 中的变量只读显示，可一键转为可编辑的变量。 */
function VarsTable() {
  const { spec, apply, readOnly } = useEditor()
  const set = useSet()
  const env = Object.entries(spec.env ?? {})
  return (
    <>
      {spec.vars.length + env.length === 0 && <p className="muted small">还没有变量，点下方「新建变量」</p>}
      {(spec.vars.length > 0 || env.length > 0) && (
        <table className="vars-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>默认值</th>
              <th title="运行时可在弹窗或 Webhook 中修改">运行时设置</th>
              <th>说明</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {spec.vars.map((v, i) => (
              <tr key={i}>
                <td>
                  <LazyInput className="mono" value={v.name} onCommit={(x) => set(['vars', i, 'name'], x)} disabled={readOnly} />
                </td>
                <td>
                  <Text mono value={v.value} onChange={(x) => set(['vars', i, 'value'], x)} />
                </td>
                <td>
                  <input type="checkbox" className="switch" checked={!!v.runtime} disabled={readOnly} onChange={(e) => set(['vars', i, 'runtime'], e.target.checked || undefined)} />
                </td>
                <td>
                  <Text value={v.description} onChange={(x) => set(['vars', i, 'description'], x)} placeholder="说明" />
                </td>
                <td>
                  {!readOnly && (
                    <button type="button" className="icon-btn" title="删除" onClick={() => apply(ops.deleteVar(i))}>
                      ×
                    </button>
                  )}
                </td>
              </tr>
            ))}
            {env.map(([k, v]) => (
              <tr key={`env:${k}`} className="muted">
                <td className="mono">{k}</td>
                <td className="mono">{v}</td>
                <td colSpan={2}>
                  {!readOnly && (
                    <button type="button" className="btn link small" onClick={() => apply(ops.envToVar(k, v))}>
                      转为可设置说明 / 运行时修改的变量
                    </button>
                  )}
                </td>
                <td />
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {!readOnly && (
        <button type="button" className="btn small" onClick={() => apply(ops.addVar(spec))}>
          + 新建变量
        </button>
      )}
      <p className="muted small">私密变量（如令牌）放在下方「密钥」中，值只写不读、日志自动脱敏。</p>
    </>
  )
}

/** 关联通用变量组：不选表示注入全部变量组。 */
function VarGroupsField() {
  const { spec, apply, readOnly } = useEditor()
  const [groups, setGroups] = useState<VarGroup[]>()
  useEffect(() => {
    api.listVarGroups().then(setGroups, () => setGroups([]))
  }, [])
  const chosen = spec.var_groups ?? []
  const toggle = (name: string, on: boolean) => {
    const next = on ? [...chosen, name] : chosen.filter((x) => x !== name)
    apply(ops.set(['var_groups'], next.length ? next : undefined))
  }
  return (
    <>
      <div className="muted small">{chosen.length === 0 ? '未指定关联：运行时注入全部变量组' : `已关联 ${chosen.length} 个变量组`}</div>
      {groups && groups.length === 0 && (
        <p className="muted small">
          还没有变量组，去 <a href={href.secrets}>账户设置 → 通用变量</a> 新建
        </p>
      )}
      <div className="group-checks">
        {groups?.map((g) => (
          <label key={g.id} className="check">
            <input type="checkbox" disabled={readOnly} checked={chosen.includes(g.name)} onChange={(e) => toggle(g.name, e.target.checked)} />
            <span className="strong">{g.name}</span>
            <span className="muted small">{g.vars.map((v) => v.name).join('、') || '空'}</span>
          </label>
        ))}
        {chosen
          .filter((n) => groups && !groups.some((g) => g.name === n))
          .map((n) => (
            <label key={n} className="check error-text">
              <input type="checkbox" disabled={readOnly} checked onChange={() => toggle(n, false)} />
              {n}（不存在）
            </label>
          ))}
      </div>
    </>
  )
}

/** 运行环境：构建机器（指定节点或按标签匹配）、构建环境（默认 / 容器镜像）、是否下载流水线源。 */
function RunEnvFields({ r, job }: { r: JobRef; job: JobSpec }) {
  const { apply, readOnly } = useEditor()
  const [runners, setRunners] = useState<RunnerInfo[]>()
  useEffect(() => {
    api.listRunners().then((l) => setRunners(l.filter((x) => x.kind !== 'agent')), () => setRunners([]))
  }, [])
  const p = jobPath(r)
  const set = (f: string, v: unknown) => apply(ops.set([...p, f], v), `${r.si}:${r.ji}:${f}`)
  const labels = job.runsOn ?? []
  const node = labels.find((l) => l.startsWith('node:'))?.slice(5)
  const others = labels.filter((l) => !l.startsWith('node:'))
  const [containerMode, setContainerMode] = useState(!!job.image)
  const setNode = (name: string) => {
    const next = name ? [`node:${name}`, ...others] : others
    set('runs-on', next.length ? next : undefined)
  }
  return (
    <>
      <Field label="构建机器" hint="指定构建节点，或「任意节点」按标签匹配；节点在「账户设置 → 构建节点」中添加">
        <select value={node ?? ''} disabled={readOnly} onChange={(e) => setNode(e.target.value)}>
          <option value="">任意节点（按标签匹配）</option>
          {node && !runners?.some((x) => x.name === node) && <option value={node}>{node}（不存在）</option>}
          {runners?.map((x) => (
            <option key={x.id} value={x.name}>
              {x.name}
              {x.kind === 'github' ? '（GitHub）' : x.labels?.length ? `（${x.labels.join(', ')}）` : ''}
            </option>
          ))}
        </select>
      </Field>
      <Field label="节点标签" block hint="只调度到具备全部标签的节点">
        <TagInput mono value={others} onChange={(v) => set('runs-on', [...(node ? [`node:${node}`] : []), ...v].length ? [...(node ? [`node:${node}`] : []), ...v] : undefined)} placeholder="如 linux、docker" disabled={readOnly} />
      </Field>
      <div className="field">
        <span>构建环境</span>
        <div className="row">
          <label className="check">
            <input type="radio" disabled={readOnly} checked={!containerMode} onChange={() => (setContainerMode(false), set('image', undefined))} />
            默认环境（节点本机）
          </label>
          <label className="check">
            <input type="radio" disabled={readOnly} checked={containerMode} onChange={() => setContainerMode(true)} />
            指定容器环境
          </label>
        </div>
      </div>
      {containerMode && (
        <Field label="容器镜像地址" hint="每个步骤在这个镜像的容器中执行，代码目录挂载为 /workspace">
          <Text mono value={job.image} onChange={(v) => set('image', v || undefined)} placeholder="如 node:22、golang:1.23、maven:3-eclipse-temurin-17" />
        </Field>
      )}
      <Field label="下载流水线源">
        <select value={job.checkout === false ? 'none' : 'all'} disabled={readOnly} onChange={(e) => set('checkout', e.target.value === 'none' ? false : undefined)}>
          <option value="all">下载全部流水线源</option>
          <option value="none">不下载</option>
        </select>
      </Field>
    </>
  )
}

const DEFAULT_CACHE_DIRS = ['/root/.m2', '/root/.gradle/caches', '/root/.npm', '/root/.yarn', '/go/pkg/mod', '/root/.cache']

/** 缓存目录：容器内的绝对路径，Runner 把它映射到宿主机 cache/pipelines/<流水线ID>/ 下。 */
function CacheDirsField() {
  const { spec, apply, readOnly } = useEditor()
  const dirs = spec.cache_dirs ?? []
  const save = (next: string[]) => apply(ops.set(['cache_dirs'], next.length ? next : undefined))
  return (
    <>
      <TagInput mono value={dirs} onChange={save} placeholder="容器内的绝对路径，回车添加" disabled={readOnly} />
      {!readOnly && (
        <div className="row">
          <button type="button" className="btn small" onClick={() => save(DEFAULT_CACHE_DIRS)}>
            恢复默认
          </button>
          <span className="muted small">默认：{DEFAULT_CACHE_DIRS.join('、')}</span>
        </div>
      )}
      <p className="muted small">各流水线互不影响，清理工作区不会删它；步骤里安装的工具（安装 Node / Go / Java 等）另有节点级共享缓存。</p>
    </>
  )
}
