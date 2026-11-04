import { autocompletion, closeBrackets, closeBracketsKeymap, completionKeymap, type CompletionContext, type CompletionResult } from '@codemirror/autocomplete'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { yaml } from '@codemirror/lang-yaml'
import { bracketMatching, foldGutter, foldKeymap, HighlightStyle, indentOnInput, indentUnit, syntaxHighlighting } from '@codemirror/language'
import { lintGutter, lintKeymap, setDiagnostics } from '@codemirror/lint'
import { highlightSelectionMatches, searchKeymap } from '@codemirror/search'
import { Annotation, Compartment, EditorSelection, EditorState } from '@codemirror/state'
import {
  drawSelection,
  EditorView,
  highlightActiveLine,
  highlightActiveLineGutter,
  highlightSpecialChars,
  hoverTooltip,
  keymap,
  lineNumbers,
} from '@codemirror/view'
import { tags as t } from '@lezer/highlight'
import { useEffect, useImperativeHandle, useRef, type Ref } from 'react'
import { describeKey, keyOfLine, keysFor, pathAt, valuesFor, type Problem } from './yamlAssist'

export interface YamlEditorHandle {
  /** 选中 [from, to] 并滚动到可见处。 */
  reveal: (from: number, to: number) => void
}

/** 外部（撤销、画布修改）写入的内容，不再回传 onChange。 */
const external = Annotation.define<boolean>()

const highlight = HighlightStyle.define([
  { tag: [t.propertyName, t.definition(t.propertyName)], color: 'var(--syn-key)' },
  { tag: [t.string, t.special(t.string)], color: 'var(--syn-str)' },
  { tag: [t.number, t.bool, t.null, t.atom], color: 'var(--syn-num)' },
  { tag: [t.comment, t.lineComment], color: 'var(--muted)', fontStyle: 'italic' },
  { tag: [t.keyword, t.meta, t.labelName, t.typeName], color: 'var(--syn-meta)' },
  { tag: [t.punctuation, t.separator, t.squareBracket, t.brace], color: 'var(--muted)' },
])

const theme = EditorView.theme({
  '&': { height: '100%', backgroundColor: 'var(--surface)', color: 'var(--text)', fontSize: '13px' },
  '&.cm-focused': { outline: 'none' },
  '.cm-scroller': { fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace', lineHeight: '1.55' },
  '.cm-gutters': { backgroundColor: 'var(--bg)', color: 'var(--muted)', borderRight: '1px solid var(--border)' },
  '.cm-activeLine': { backgroundColor: 'color-mix(in srgb, var(--primary) 6%, transparent)' },
  '.cm-activeLineGutter': { backgroundColor: 'color-mix(in srgb, var(--primary) 12%, transparent)', color: 'var(--text)' },
  '.cm-cursor': { borderLeftColor: 'var(--text)' },
  '&.cm-focused .cm-selectionBackground, .cm-selectionBackground, ::selection': {
    backgroundColor: 'color-mix(in srgb, var(--primary) 28%, transparent) !important',
  },
  '.cm-tooltip': { backgroundColor: 'var(--surface)', color: 'var(--text)', border: '1px solid var(--border)', borderRadius: '6px' },
  '.cm-tooltip-autocomplete > ul > li[aria-selected]': { backgroundColor: 'var(--primary)', color: 'var(--primary-text)' },
  '.cm-completionDetail': { marginLeft: '10px', color: 'inherit', opacity: 0.7, fontStyle: 'normal' },
  '.cm-key-hint': { padding: '4px 8px', fontSize: '12px' },
  '.cm-panels': { backgroundColor: 'var(--bg)', color: 'var(--text)' },
  '.cm-searchMatch': { backgroundColor: 'color-mix(in srgb, var(--c-canceled) 30%, transparent)' },
})

const lines = (state: EditorState) => state.doc.toString().split('\n')

/** 按光标处的 YAML 层级补全字段名，以及部分字段的取值（如 when、type、uses 的步骤模板）。 */
function completer(templates: () => string[]) {
  return (ctx: CompletionContext): CompletionResult | null => {
    const line = ctx.state.doc.lineAt(ctx.pos)
    const before = line.text.slice(0, ctx.pos - line.from)
    const all = lines(ctx.state)
    const value = /^\s*(?:-\s+)*([A-Za-z_][\w-]*):\s+([\w.-]*)$/.exec(before)
    if (value) {
      const vals = valuesFor(pathAt(all, line.number - 1, before.length), value[1], templates())
      if (!vals?.length) return null
      return { from: ctx.pos - value[2].length, options: vals.map((v) => ({ label: v, type: 'enum' })), validFor: /^[\w.-]*$/ }
    }
    const key = /^\s*(?:-\s+)*([\w-]*)$/.exec(before)
    if (!key || (!key[1] && !ctx.explicit)) return null
    const keys = keysFor(pathAt(all, line.number - 1, before.length))
    if (!keys) return null
    return {
      from: ctx.pos - key[1].length,
      options: Object.entries(keys).map(([k, detail]) => ({ label: k, detail, type: 'property', apply: `${k}: ` })),
      validFor: /^[\w-]*$/,
    }
  }
}

/** 悬停在字段名上显示说明。 */
const keyHint = hoverTooltip((view, pos) => {
  const line = view.state.doc.lineAt(pos)
  const key = keyOfLine(line.text)
  if (!key) return null
  const start = line.from + line.text.indexOf(key + ':')
  if (pos < start || pos > start + key.length) return null
  const desc = describeKey(pathAt(lines(view.state), line.number - 1, start - line.from), key)
  if (!desc) return null
  return {
    pos: start,
    end: start + key.length,
    above: true,
    create: () => {
      const dom = document.createElement('div')
      dom.className = 'cm-key-hint'
      dom.textContent = desc
      return { dom }
    },
  }
})

/** YAML 代码编辑器：行号、高亮、折叠、搜索、按层级补全；problems 标注在对应行。 */
export function YamlEditor({
  value,
  onChange,
  readOnly,
  problems,
  templates,
  handle,
  assist = true,
}: {
  value: string
  onChange: (v: string) => void
  readOnly: boolean
  problems: Problem[]
  templates: string[]
  handle?: Ref<YamlEditorHandle>
  /** 按流水线结构补全字段与悬停说明；编辑其他 YAML（如步骤模板）时关闭。 */
  assist?: boolean
}) {
  const host = useRef<HTMLDivElement>(null)
  const view = useRef<EditorView>()
  const latest = useRef({ onChange, templates })
  latest.current = { onChange, templates }
  const readOnlyConf = useRef(new Compartment())

  useImperativeHandle(handle, () => ({
    reveal: (from, to) => {
      const v = view.current
      if (!v) return
      const len = v.state.doc.length
      v.dispatch({ selection: EditorSelection.single(Math.min(from, len), Math.min(to, len)), scrollIntoView: true })
      v.focus()
    },
  }))

  useEffect(() => {
    const v = new EditorView({
      parent: host.current!,
      state: EditorState.create({
        doc: value,
        extensions: [
          lineNumbers(),
          highlightActiveLineGutter(),
          foldGutter(),
          lintGutter(),
          highlightSpecialChars(),
          history(),
          drawSelection(),
          indentOnInput(),
          indentUnit.of('  '),
          EditorState.tabSize.of(2),
          bracketMatching(),
          closeBrackets(),
          highlightActiveLine(),
          highlightSelectionMatches(),
          yaml(),
          syntaxHighlighting(highlight),
          ...(assist ? [autocompletion({ override: [completer(() => latest.current.templates)] }), keyHint] : []),
          keymap.of([...closeBracketsKeymap, ...defaultKeymap, ...searchKeymap, ...historyKeymap, ...foldKeymap, ...completionKeymap, ...lintKeymap, indentWithTab]),
          theme,
          readOnlyConf.current.of(EditorState.readOnly.of(readOnly)),
          EditorView.updateListener.of((u) => {
            if (u.docChanged && !u.transactions.some((tr) => tr.annotation(external))) latest.current.onChange(u.state.doc.toString())
          }),
        ],
      }),
    })
    view.current = v
    return () => v.destroy()
    // 只创建一次；value / readOnly 的变化由下面的 effect 同步。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  useEffect(() => {
    view.current?.dispatch({ effects: readOnlyConf.current.reconfigure(EditorState.readOnly.of(readOnly)) })
  }, [readOnly])

  // 外部修改只替换变化的部分，保留光标与滚动位置。
  useEffect(() => {
    const v = view.current
    if (!v) return
    const cur = v.state.doc.toString()
    if (cur === value) return
    let start = 0
    while (start < cur.length && start < value.length && cur[start] === value[start]) start++
    let a = cur.length
    let b = value.length
    while (a > start && b > start && cur[a - 1] === value[b - 1]) a--, b--
    v.dispatch({ changes: { from: start, to: a, insert: value.slice(start, b) }, annotations: external.of(true) })
  }, [value])

  useEffect(() => {
    const v = view.current
    if (!v) return
    const len = v.state.doc.length
    v.dispatch(
      setDiagnostics(
        v.state,
        problems.map((p) => ({ from: Math.min(p.from, len), to: Math.min(p.to, len), severity: p.severity, message: p.message })),
      ),
    )
  }, [problems, value])

  return <div className="ed-yaml cm-host" ref={host} />
}
