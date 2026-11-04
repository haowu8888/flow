import { lazy, Suspense, useCallback, useEffect, useImperativeHandle, useMemo, useRef, useState, type ReactNode, type Ref } from 'react'
import type { Document } from 'yaml'
import { api } from '../api'
import type { Environment, HostGroup, Template } from '../types'
import { Canvas } from './Canvas'
import { EditorContext, type EditorApi, type Selection } from './context'
import { dependencies, edit, issues, parse, type PipelineSpec } from './model'
import { PipelineSettings, SidePanel, type SettingsSection } from './Panels'
import { TaskPicker } from './TaskPicker'
import type { YamlEditorHandle } from './YamlEditor'
import { findProblems, type Problem } from './yamlAssist'

// CodeMirror 较大，切到 YAML 模式时再加载。
const YamlEditor = lazy(() => import('./YamlEditor').then((m) => ({ default: m.YamlEditor })))

const HISTORY_LIMIT = 200
const COALESCE_MS = 1500

/** 撤销 / 重做：以整份 YAML 为快照；同一字段的连续输入合并为一步。 */
function useHistory(value: string, onChange: (v: string) => void) {
  const past = useRef<string[]>([])
  const future = useRef<string[]>([])
  const last = useRef<{ key?: string; at: number }>({ at: 0 })
  const [, bump] = useState(0)

  const commit = useCallback(
    (next: string, key?: string) => {
      if (next === value) return
      const now = Date.now()
      if (!(key && last.current.key === key && now - last.current.at < COALESCE_MS)) {
        past.current = [...past.current.slice(-HISTORY_LIMIT + 1), value]
      }
      future.current = []
      last.current = { key, at: now }
      onChange(next)
      bump((x) => x + 1)
    },
    [value, onChange],
  )
  const undo = useCallback(() => {
    const prev = past.current.pop()
    if (prev === undefined) return
    future.current.push(value)
    last.current = { at: 0 }
    onChange(prev)
    bump((x) => x + 1)
  }, [value, onChange])
  const redo = useCallback(() => {
    const next = future.current.pop()
    if (next === undefined) return
    past.current.push(value)
    last.current = { at: 0 }
    onChange(next)
    bump((x) => x + 1)
  }, [value, onChange])
  return { commit, undo, redo, canUndo: past.current.length > 0, canRedo: future.current.length > 0 }
}

type Check = { state: 'idle' } | { state: 'checking' } | { state: 'ok' } | { state: 'error'; message: string }

/** 停止输入片刻后调用服务端校验（与保存规则一致，含步骤模板引用）。 */
function useValidation(value: string, enabled: boolean): Check {
  const [check, setCheck] = useState<Check>({ state: 'idle' })
  useEffect(() => {
    if (!enabled) return
    let stale = false
    setCheck({ state: 'checking' })
    const t = setTimeout(() => {
      api.validatePipeline(value).then(
        () => !stale && setCheck({ state: 'ok' }),
        (e: Error) => !stale && setCheck({ state: 'error', message: e.message }),
      )
    }, 600)
    return () => {
      stale = true
      clearTimeout(t)
    }
  }, [value, enabled])
  return check
}

function findJob(spec: PipelineSpec, name: string) {
  for (let si = 0; si < spec.stages.length; si++) {
    const ji = spec.stages[si].jobs.findIndex((j) => j.name === name)
    if (ji >= 0) return { si, ji }
  }
}

/** 数据变化后按名称重新定位选中项（拖动、撤销会改变下标）。 */
function resolveSelection(sel: Selection, spec: PipelineSpec): Selection {
  if (sel?.kind === 'job') {
    const at = spec.stages[sel.ref.si]?.jobs[sel.ref.ji]
    if (at?.name === sel.name) return sel
    const found = findJob(spec, sel.name)
    if (found) return { ...sel, ref: found }
    return at ? { ...sel, name: at.name } : null
  }
  if (sel?.kind === 'stage') {
    const at = spec.stages[sel.si]
    if (at?.name === sel.name) return sel
    const si = spec.stages.findIndex((s) => s.name === sel.name)
    if (si >= 0) return { ...sel, si }
    return at ? { ...sel, name: at.name } : null
  }
  return sel
}

const sameSel = (a: Selection, b: Selection) => JSON.stringify(a) === JSON.stringify(b)

function isTyping(el: EventTarget | null) {
  return el instanceof HTMLElement && (el.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(el.tagName))
}

/** 外部替换整份内容（如载入历史版本），作为一步可撤销的修改。 */
export interface EditorHandle {
  replace: (yaml: string) => void
}

/**
 * 流水线编辑器：画布（阶段 / 任务卡片、拖拽、连线依赖）+ 属性面板，可切换到 YAML。
 * value 是完整 YAML；所有修改都通过 onChange 返回新的 YAML。
 */
export function PipelineEditor({
  value,
  onChange,
  readOnly = false,
  actions,
  handle,
  section,
}: {
  value: string
  onChange: (v: string) => void
  readOnly?: boolean
  actions?: ReactNode
  handle?: Ref<EditorHandle>
  /** 编辑页的分区：flow 为画布（任务 / 阶段属性在右侧抽屉中），其余只显示对应的流水线设置；不传为画布 + 常驻属性面板。 */
  section?: 'flow' | SettingsSection
}) {
  const [mode, setMode] = useState<'canvas' | 'yaml'>('canvas')
  const history = useHistory(value, onChange)
  useImperativeHandle(handle, () => ({ replace: (v) => history.commit(v) }), [history])
  const parsed = useMemo(() => parse(value), [value])
  const check = useValidation(value, !readOnly && !parsed.error)
  const [selection, setSelection] = useState<Selection>(null)
  const [stepTemplates, setStepTemplates] = useState<Template[]>([])
  const [hostGroups, setHostGroups] = useState<HostGroup[]>([])
  const [environments, setEnvironments] = useState<Environment[]>([])
  const [error, setError] = useState<string>()
  const [picking, setPicking] = useState<{ si: number; at: number }>()
  const yamlView = useRef<YamlEditorHandle>(null)
  const serverError = check.state === 'error' ? check.message : undefined
  const problems = useMemo(() => (mode === 'yaml' ? findProblems(value, serverError) : []), [mode, value, serverError])

  useEffect(() => {
    api.listTemplates('steps').then(setStepTemplates, () => {})
    api.listHostGroups().then(setHostGroups, () => {})
    api.listEnvironments().then(setEnvironments, () => {})
  }, [])

  // YAML 无法解析时不能切到画布。
  const yamlBroken = !!parsed.error
  useEffect(() => {
    if (yamlBroken) setMode('yaml')
  }, [yamlBroken])

  const spec = parsed.spec
  useEffect(() => {
    if (!spec) return
    setSelection((s) => {
      const next = resolveSelection(s, spec)
      return sameSel(s, next) ? s : next
    })
  }, [spec])

  const apply = useCallback(
    (fn: (doc: Document) => void, coalesce?: string) => {
      try {
        setError(undefined)
        history.commit(edit(value, fn), coalesce)
      } catch (e) {
        setError((e as Error).message)
      }
    },
    [value, history],
  )

  useEffect(() => {
    if (readOnly) return
    const onKey = (e: KeyboardEvent) => {
      if (!(e.ctrlKey || e.metaKey) || isTyping(e.target)) return
      const k = e.key.toLowerCase()
      if (k === 'z' && !e.shiftKey) history.undo()
      else if ((k === 'z' && e.shiftKey) || k === 'y') history.redo()
      else return
      e.preventDefault()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [history, readOnly])

  const ctx = useMemo((): EditorApi | null => {
    if (!spec) return null
    const { stageIssues, jobIssues } = issues(spec)
    const pickTask = (si: number, at: number) => setPicking({ si, at })
    return { spec, deps: dependencies(spec), stageIssues, jobIssues, selection, select: setSelection, readOnly, apply, stepTemplates, hostGroups, environments, pickTask }
  }, [spec, selection, readOnly, apply, stepTemplates, hostGroups, environments])

  if (section && section !== 'flow') {
    if (!ctx) return <div className="alert">YAML 有语法错误：{parsed.error}。请在「流程配置」的 YAML 模式中修复后再编辑设置。</div>
    return (
      <EditorContext.Provider value={ctx}>
        {error && <div className="alert subtle">{error}</div>}
        <PipelineSettings section={section} />
      </EditorContext.Provider>
    )
  }

  const drawer = section === 'flow'
  return (
    <div className="ed">
      <div className="ed-toolbar">
        <div className="segmented">
          <button className={mode === 'canvas' ? 'active' : ''} disabled={yamlBroken} title={yamlBroken ? 'YAML 有语法错误，修复后才能切换' : undefined} onClick={() => setMode('canvas')}>
            可视化
          </button>
          <button className={mode === 'yaml' ? 'active' : ''} onClick={() => setMode('yaml')}>
            YAML
          </button>
        </div>
        {!readOnly && (
          <>
            <button className="btn icon" title="撤销 (Ctrl+Z)" disabled={!history.canUndo} onClick={history.undo}>
              ↶
            </button>
            <button className="btn icon" title="重做 (Ctrl+Shift+Z)" disabled={!history.canRedo} onClick={history.redo}>
              ↷
            </button>
            <ValidationStatus check={yamlBroken ? { state: 'error', message: parsed.error! } : check} />
          </>
        )}
        <div className="spacer" />
        {actions}
      </div>
      {error && <div className="alert subtle">{error}</div>}
      {mode === 'canvas' && ctx ? (
        <EditorContext.Provider value={ctx}>
          <div className={`ed-body${drawer ? ' drawer' : ''}`}>
            <Canvas />
            {(!drawer || selection) && <SidePanel />}
          </div>
          {picking && !readOnly && <TaskPicker {...picking} onClose={() => setPicking(undefined)} />}
        </EditorContext.Provider>
      ) : (
        <>
          <Suspense fallback={<div className="ed-yaml cm-host muted ed-loading">加载编辑器…</div>}>
            <YamlEditor
              handle={yamlView}
              value={value}
              onChange={(v) => history.commit(v, 'yaml')}
              readOnly={readOnly}
              problems={problems}
              templates={stepTemplates.map((t) => t.name)}
            />
          </Suspense>
          {!readOnly && <ProblemList problems={problems} onPick={(p) => yamlView.current?.reveal(p.from, p.to)} />}
        </>
      )}
    </div>
  )
}

/** YAML 模式下的问题列表，点击定位到对应行。 */
function ProblemList({ problems, onPick }: { problems: Problem[]; onPick: (p: Problem) => void }) {
  if (problems.length === 0) return null
  return (
    <ul className="ed-problems">
      {problems.slice(0, 20).map((p, i) => (
        <li key={i}>
          <button onClick={() => onPick(p)}>
            <span className={p.severity === 'error' ? 'error-text' : 'muted'}>{p.severity === 'error' ? '✗' : '!'}</span>
            <span className="ed-problem-line">第 {p.line} 行</span>
            <span>{p.message}</span>
          </button>
        </li>
      ))}
      {problems.length > 20 && <li className="muted small">还有 {problems.length - 20} 个问题</li>}
    </ul>
  )
}

function ValidationStatus({ check }: { check: Check }) {
  if (check.state === 'idle') return null
  if (check.state === 'checking') return <span className="ed-status muted">校验中…</span>
  if (check.state === 'ok') return <span className="ed-status ok-text">✓ 配置有效</span>
  const message = check.message.replace(/^invalid request: /, '')
  return (
    <span className="ed-status error-text" title={message}>
      ✗ {message}
    </span>
  )
}
