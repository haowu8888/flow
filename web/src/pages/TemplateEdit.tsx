import { lazy, Suspense, useEffect, useRef, useState } from 'react'
import { api, ApiError } from '../api'
import { useSession } from '../auth'
import { RevisionHistory, templateSource, type RevisionContent } from '../editor/History'
import { PipelineEditor, type EditorHandle } from '../editor/PipelineEditor'
import { StepTemplateForm } from '../editor/StepTemplateForm'
import { fmtTime } from '../format'
import { href, navigate, setLeaveGuard } from '../router'
import type { Template, TemplateKind } from '../types'

const YamlEditor = lazy(() => import('../editor/YamlEditor').then((m) => ({ default: m.YamlEditor })))

export const STEPS_EXAMPLE = `description: 构建并推送 Docker 镜像
inputs:
  IMAGE: {required: true, description: 镜像名，不含标签}
  DOCKERFILE: {default: Dockerfile, description: Dockerfile 路径}
steps:
  - name: build
    run: docker build -f "$DOCKERFILE" -t "$IMAGE:$FLOW_RUN_ID" .
  - name: push
    run: docker push "$IMAGE:$FLOW_RUN_ID"
`

export const PIPELINE_EXAMPLE = `name: my-pipeline
stages:
  - name: build
    jobs:
      - name: build
        steps:
          - name: build
            run: echo build
  - name: deploy
    jobs:
      - name: deploy
        steps:
          - name: deploy
            run: echo deploy
`

const kindLabel: Record<TemplateKind, string> = { steps: '步骤模板', pipeline: '流水线模板' }

/** 编辑模板：流水线模板用画布编排，步骤模板用表单编辑（都可切换到 YAML）。id 为 new-steps / new-pipeline 时新建。 */
export function TemplateEdit({ id }: { id: string }) {
  const creating = id === 'new-steps' || id === 'new-pipeline'
  const [template, setTemplate] = useState<Template>()
  const [loadError, setLoadError] = useState<string>()
  useEffect(() => {
    if (creating) {
      const kind: TemplateKind = id === 'new-steps' ? 'steps' : 'pipeline'
      setTemplate({ id: '', kind, name: '', description: '', yaml: kind === 'steps' ? STEPS_EXAMPLE : PIPELINE_EXAMPLE, version: 0, created_at: '', updated_at: '' })
    } else {
      api.getTemplate(id).then(setTemplate, (e: Error) => setLoadError(e.message))
    }
  }, [id, creating])
  if (loadError) return <div className="alert">{loadError}</div>
  if (!template) return <p className="muted">加载中…</p>
  return <Editor key={id} template={template} creating={creating} />
}

const usesLabel = (versions: number[]) => versions.map((v) => (v ? `v${v}` : '最新')).join('、')

function Editor({ template: initialTemplate, creating }: { template: Template; creating: boolean }) {
  const canEdit = useSession().can('developer')
  const [template, setTemplate] = useState(initialTemplate) // 服务端当前版本
  const readOnly = !canEdit || !!template.builtin
  const [name, setName] = useState(template.name)
  const [description, setDescription] = useState(template.description)
  const [yaml, setYaml] = useState(template.yaml)
  const [mode, setMode] = useState<'visual' | 'yaml'>('visual')
  const [users, setUsers] = useState<{ id: string; name: string; versions: number[] }[]>()
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<{ ok: boolean; text: string }>()
  const [conflict, setConflict] = useState<Template>() // 编辑期间他人保存的最新版本
  const [history, setHistory] = useState<{ version: number; compare: 'prev' | 'draft' }>()
  const [editorKey, setEditorKey] = useState(0)
  const editor = useRef<EditorHandle>(null)
  const dirty = !readOnly && (name !== template.name || description !== template.description || yaml !== template.yaml)
  const dirtyRef = useRef(dirty)
  dirtyRef.current = dirty

  useEffect(() => setLeaveGuard(() => dirtyRef.current), [])
  useEffect(() => {
    if (template.kind === 'steps' && template.id) api.templateUsers(template.id).then(setUsers, () => {})
  }, [template.id, template.kind])

  /** 把历史版本载入编辑器（流水线模板经画布编辑器写入，可撤销）。 */
  function load(c: RevisionContent) {
    if (c.meta?.名称 !== undefined) setName(c.meta.名称)
    if (c.meta?.描述 !== undefined) setDescription(c.meta.描述)
    if (template.kind === 'pipeline' && editor.current) editor.current.replace(c.yaml)
    else setYaml(c.yaml)
    setMsg(undefined)
  }

  /** 服务端已保存 t：以它为新的基准；reset 时编辑器内容也换成它。 */
  function accept(t: Template, reset: boolean) {
    setTemplate(t)
    setConflict(undefined)
    setName(t.name) // 服务端会去掉首尾空白、补全描述
    setDescription(t.description)
    if (reset) {
      setYaml(t.yaml)
      setEditorKey((k) => k + 1)
    }
  }

  async function save(base = template.version) {
    setBusy(true)
    setMsg(undefined)
    try {
      if (!template.id) {
        const t = await api.createTemplate(template.kind, name, description, yaml)
        dirtyRef.current = false
        navigate(href.template(t.id))
        return
      }
      const t = await api.updateTemplate(template.id, name, description, yaml, base)
      accept(t, false)
      setMsg({ ok: true, text: t.version === template.version ? '内容没有变化' : `已保存为 v${t.version}` })
    } catch (e) {
      if (e instanceof ApiError && e.status === 409 && template.id) {
        try {
          const latest = await api.getTemplate(template.id)
          if (latest.version !== template.version) {
            setConflict(latest)
            return
          }
        } catch {
          // 取不到最新版本时按普通错误提示
        }
      }
      setMsg({ ok: false, text: (e as Error).message })
    } finally {
      setBusy(false)
    }
  }
  async function remove() {
    if (!confirm(`删除模板「${template.name}」？所有历史版本会一并删除。`)) return
    try {
      await api.deleteTemplate(template.id)
      dirtyRef.current = false
      navigate(href.templates)
    } catch (e) {
      setMsg({ ok: false, text: (e as Error).message })
    }
  }

  const versionInfo = template.version > 0 && (
    <button className="btn link small" title="查看历史版本与差异" onClick={() => setHistory({ version: template.version, compare: dirty ? 'draft' : 'prev' })}>
      v{template.version}
      {template.updated_by ? ` · ${template.updated_by}` : ''} · {fmtTime(template.updated_at)}
    </button>
  )
  const actions = (
    <>
      {msg && <span className={`ed-save-msg ${msg.ok ? 'ok-text' : 'error-text'}`} title={msg.text}>{msg.text}</span>}
      {dirty && !msg && <span className="muted small">有未保存的修改</span>}
      {versionInfo}
      {template.kind === 'pipeline' && template.id && (
        <button className="btn" onClick={() => navigate(href.newFromTemplate(template.id))}>
          用此模板新建
        </button>
      )}
      {!readOnly && (
        <button className="btn primary" disabled={busy || !name.trim() || !dirty} onClick={() => save()}>
          {busy ? '保存中…' : creating ? '创建模板' : '保存'}
        </button>
      )}
    </>
  )
  const draft: RevisionContent = { yaml, meta: { 名称: name, 描述: description } }

  return (
    <section>
      <nav className="crumbs">
        <a href={href.templates}>模板</a> / {creating ? `新建${kindLabel[template.kind]}` : template.name}
      </nav>
      <div className="page-head">
        <h1>{creating ? `新建${kindLabel[template.kind]}` : template.name}</h1>
        {template.builtin && <span className="tag">内置 · 只读</span>}
        <div className="spacer" />
        {template.id && !readOnly && (
          <button className="btn danger" disabled={(users?.length ?? 0) > 0} title={users?.length ? '仍被流水线引用' : undefined} onClick={remove}>
            删除模板
          </button>
        )}
      </div>
      {conflict && (
        <div className="alert conflict">
          <span>
            <strong>{conflict.updated_by || '其他成员'}</strong> 在你编辑期间保存了 v{conflict.version}，直接保存会覆盖对方的修改。
          </span>
          <div className="row">
            <button className="btn" onClick={() => setHistory({ version: conflict.version, compare: 'draft' })}>
              查看差异
            </button>
            <button className="btn" onClick={() => accept(conflict, true)}>
              载入最新版本
            </button>
            <button className="btn danger" onClick={() => confirm(`用你的内容覆盖 v${conflict.version}？对方的修改仍保留在历史版本中。`) && save(conflict.version)}>
              仍然保存我的版本
            </button>
          </div>
        </div>
      )}
      {!readOnly && (
        <div className="form-grid template-meta">
          <label className="field">
            <span>名称</span>
            <input
              className={template.kind === 'steps' ? 'mono' : ''}
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={template.kind === 'steps' ? 'docker-build（小写字母、数字、. _ -，流水线中以此引用）' : '如：后端服务标准流程'}
            />
          </label>
          <label className="field">
            <span>描述</span>
            <input value={description} onChange={(e) => setDescription(e.target.value)} placeholder={template.kind === 'steps' ? '留空则取模板中的说明' : '可选'} />
          </label>
        </div>
      )}
      {template.kind === 'pipeline' ? (
        <PipelineEditor key={editorKey} handle={editor} value={yaml} onChange={setYaml} readOnly={readOnly} actions={actions} />
      ) : (
        <>
          <div className="ed-toolbar">
            <div className="segmented">
              <button className={mode === 'visual' ? 'active' : ''} onClick={() => setMode('visual')}>
                可视化
              </button>
              <button className={mode === 'yaml' ? 'active' : ''} onClick={() => setMode('yaml')}>
                YAML
              </button>
            </div>
            <div className="spacer" />
            {actions}
          </div>
          {mode === 'visual' ? (
            <StepTemplateForm key={editorKey} value={yaml} onChange={setYaml} name={name} readOnly={readOnly} />
          ) : (
            <Suspense fallback={<p className="muted">加载编辑器…</p>}>
              <div className="step-yaml">
                <YamlEditor value={yaml} onChange={setYaml} readOnly={readOnly} problems={[]} templates={[]} assist={false} />
              </div>
            </Suspense>
          )}
          {users && (
            <p className="muted small">
              {users.length === 0 ? '没有流水线引用此模板。' : '被以下流水线引用（括号内为所用版本；引用「最新」的从下次运行起使用新版本）：'}
              {users.map((u, i) => (
                <span key={u.id}>
                  {i > 0 && '、'}
                  <a href={href.pipeline(u.id)}>{u.name}</a>（{usesLabel(u.versions)}）
                </span>
              ))}
            </p>
          )}
          {template.version > 0 && (
            <p className="muted small">
              在流水线中写 <code>uses: {template.name}@{template.version}</code> 可锁定当前版本，之后的修改不影响它；也可在画布的步骤中选择版本。
            </p>
          )}
        </>
      )}
      {history && (
        <RevisionHistory
          title="版本历史"
          source={templateSource(conflict ?? template)}
          draft={draft}
          dirty={dirty}
          initial={history}
          onRestore={readOnly ? undefined : load}
          onRolledBack={
            readOnly
              ? undefined
              : (t, from) => {
                  accept(t, true)
                  setMsg({ ok: true, text: t.version === template.version ? `内容与 v${from} 相同，未生成新版本` : `已回退到 v${from}（生成 v${t.version}）` })
                }
          }
          onClose={() => setHistory(undefined)}
        />
      )}
    </section>
  )
}
