import { useMemo } from 'react'
import { isMap, isScalar, isSeq, parseDocument, type Document } from 'yaml'
import { LazyInput } from './fields'
import { edit, setField } from './model'

interface Input {
  name: string
  required: boolean
  default?: string
  description?: string
}

interface Parsed {
  description: string
  inputs: Input[]
  steps: { name: string; run: string }[]
}

function read(text: string): Parsed | string {
  const doc = parseDocument(text)
  if (doc.errors.length) return doc.errors[0].message
  const js = (doc.toJS() ?? {}) as Record<string, unknown>
  if (typeof js !== 'object' || Array.isArray(js)) return '顶层必须是映射（description / inputs / steps）'
  const inputs = Object.entries((js.inputs as Record<string, Record<string, unknown> | null>) ?? {}).map(([name, v]) => ({
    name,
    required: v?.required === true,
    default: v?.default === undefined || v?.default === null ? undefined : String(v.default),
    description: typeof v?.description === 'string' ? v.description : undefined,
  }))
  const steps = ((js.steps as Record<string, unknown>[]) ?? []).map((s) => ({ name: String(s?.name ?? ''), run: String(s?.run ?? '') }))
  return { description: typeof js.description === 'string' ? js.description : '', inputs, steps }
}

const keyOf = (k: unknown) => String(isScalar(k) ? k.value : k)

/** 只删除字段本身，不连带删除变空的父映射（输入参数可以没有任何字段）。 */
function put(doc: Document, path: (string | number)[], v: unknown) {
  if (v === undefined || v === '' || v === false) {
    if (doc.hasIn(path)) doc.deleteIn(path)
  } else {
    doc.setIn(path, v)
  }
}

function inputsMap(doc: Document) {
  let m = doc.get('inputs', true)
  if (!isMap(m)) {
    doc.set('inputs', doc.createNode({}))
    m = doc.get('inputs', true)
    // inputs 放在 description 之后、steps 之前
    if (isMap(doc.contents)) {
      const items = doc.contents.items
      const i = items.findIndex((p) => keyOf(p.key) === 'inputs')
      const [pair] = items.splice(i, 1)
      const at = items.findIndex((p) => keyOf(p.key) === 'steps')
      items.splice(at < 0 ? items.length : at, 0, pair)
    }
  }
  if (isMap(m)) m.flow = false
  return m as ReturnType<Document['get']> & { items: { key: unknown; value: unknown }[] }
}

/** 步骤模板的可视化编辑：说明、输入参数、步骤列表；value 是完整 YAML。 */
export function StepTemplateForm({ value, onChange, name, readOnly }: { value: string; onChange: (v: string) => void; name: string; readOnly: boolean }) {
  const parsed = useMemo(() => read(value), [value])
  if (typeof parsed === 'string') return <div className="alert">YAML 有错误，请切换到 YAML 修改：{parsed}</div>
  const mutate = (fn: (doc: Document) => void) => {
    try {
      onChange(edit(value, fn))
    } catch (e) {
      alert((e as Error).message)
    }
  }
  const required = parsed.inputs.filter((i) => i.required)

  return (
    <fieldset className="step-form" disabled={readOnly}>
      <label className="field">
        <span>说明（未填写模板描述时显示）</span>
        <LazyInput value={parsed.description} onCommit={(v) => mutate((d) => put(d, ['description'], v))} placeholder="如：构建并推送 Docker 镜像" />
      </label>

      <section>
        <div className="row">
          <h3>输入参数</h3>
          <span className="muted small">引用时用 with 传入，在步骤中作为环境变量使用（如 $IMAGE）</span>
        </div>
        {parsed.inputs.length === 0 && <p className="muted small">没有输入参数。</p>}
        {parsed.inputs.length > 0 && (
          <table className="input-table">
            <thead>
              <tr>
                <th>名称</th>
                <th>必填</th>
                <th>默认值</th>
                <th>说明</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {parsed.inputs.map((inp) => (
                <tr key={inp.name}>
                  <td>
                    <LazyInput
                      className="mono"
                      value={inp.name}
                      onCommit={(v) =>
                        v.trim() &&
                        v.trim() !== inp.name &&
                        mutate((d) => {
                          const pair = inputsMap(d).items.find((p) => keyOf(p.key) === inp.name)
                          if (pair) pair.key = d.createNode(v.trim())
                        })
                      }
                    />
                  </td>
                  <td>
                    <input
                      type="checkbox"
                      checked={inp.required}
                      onChange={(e) =>
                        mutate((d) => {
                          put(d, ['inputs', inp.name, 'required'], e.target.checked)
                          if (e.target.checked) put(d, ['inputs', inp.name, 'default'], undefined) // 必填参数不能有默认值
                        })
                      }
                    />
                  </td>
                  <td>
                    <LazyInput
                      className="mono"
                      value={inp.default ?? ''}
                      disabled={inp.required}
                      placeholder={inp.required ? '必填无默认值' : '（空）'}
                      onCommit={(v) => mutate((d) => put(d, ['inputs', inp.name, 'default'], v === '' ? undefined : v))}
                    />
                  </td>
                  <td>
                    <LazyInput value={inp.description ?? ''} onCommit={(v) => mutate((d) => put(d, ['inputs', inp.name, 'description'], v))} />
                  </td>
                  <td>
                    {!readOnly && (
                      <button type="button" className="icon-btn" title="删除" onClick={() => mutate((d) => d.deleteIn(['inputs', inp.name]))}>
                        ×
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {!readOnly && (
          <button
            type="button"
            className="btn link small"
            onClick={() =>
              mutate((d) => {
                const m = inputsMap(d)
                let n = 1
                while (m.items.some((p) => keyOf(p.key) === `INPUT_${n}`)) n++
                const node = d.createNode({})
                node.flow = true
                ;(m as unknown as { set: (k: string, v: unknown) => void }).set(`INPUT_${n}`, node)
              })
            }
          >
            + 添加参数
          </button>
        )}
      </section>

      <section>
        <div className="row">
          <h3>步骤</h3>
          <span className="muted small">按顺序执行，任一失败即停止</span>
        </div>
        {parsed.steps.map((s, i) => (
          <div key={i} className="ed-step card">
            <div className="row">
              <span className="ed-step-no">{i + 1}</span>
              <LazyInput className="grow" value={s.name} placeholder={`步骤 ${i + 1}`} onCommit={(v) => mutate((d) => setField(d, ['steps', i, 'name'], v))} />
              {!readOnly && (
                <>
                  <button type="button" className="icon-btn" title="上移" disabled={i === 0} onClick={() => mutate((d) => moveStep(d, i, i - 1))}>
                    ↑
                  </button>
                  <button type="button" className="icon-btn" title="删除" disabled={parsed.steps.length === 1} onClick={() => mutate((d) => d.deleteIn(['steps', i]))}>
                    ×
                  </button>
                </>
              )}
            </div>
            <ScriptBox value={s.run} onCommit={(v) => mutate((d) => setField(d, ['steps', i, 'run'], v))} />
          </div>
        ))}
        {!readOnly && (
          <button
            type="button"
            className="btn link small"
            onClick={() =>
              mutate((d) => {
                const seq = d.get('steps', true)
                const node = d.createNode({ name: `step-${parsed.steps.length + 1}`, run: 'echo hello' })
                if (isSeq(seq)) seq.items.push(node)
                else d.set('steps', d.createNode([{ name: 'step-1', run: 'echo hello' }]))
              })
            }
          >
            + 添加步骤
          </button>
        )}
      </section>

      <section className="usage">
        <h3>在流水线中使用</h3>
        <pre className="mono small">{`steps:
  - uses: ${name || '模板名'}${required.length || parsed.inputs.length ? `\n    with:\n${(required.length ? required : parsed.inputs.slice(0, 2)).map((i) => `      ${i.name}: ...`).join('\n')}` : ''}`}</pre>
        <p className="muted small">在画布的步骤中选择「引用模板」即可，必填参数会自动列出。</p>
      </section>
    </fieldset>
  )
}

function moveStep(doc: Document, from: number, to: number) {
  const seq = doc.get('steps', true)
  if (!isSeq(seq)) return
  const [item] = seq.items.splice(from, 1)
  seq.items.splice(to, 0, item)
}

/** 多行脚本，失焦时提交。 */
function ScriptBox({ value, onCommit }: { value: string; onCommit: (v: string) => void }) {
  return (
    <textarea
      key={value}
      className="mono ed-script"
      spellCheck={false}
      defaultValue={value}
      rows={Math.min(12, Math.max(2, value.split('\n').length))}
      onBlur={(e) => e.target.value !== value && onCommit(e.target.value)}
    />
  )
}
