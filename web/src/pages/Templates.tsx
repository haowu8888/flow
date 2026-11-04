import { useState } from 'react'
import { api } from '../api'
import { useSession } from '../auth'
import { FlowPreview, specTags } from '../components/FlowPreview'
import { parse } from '../editor/model'
import { fmtTime } from '../format'
import { usePolling } from '../hooks'
import { href, navigate } from '../router'
import type { Template, TemplateKind } from '../types'

const kindLabel: Record<TemplateKind, string> = { steps: '步骤模板', pipeline: '流水线模板' }

// 记住上次查看的类型。
const KIND_KEY = 'flow:template-kind'
function loadKind(): TemplateKind {
  try {
    return localStorage.getItem(KIND_KEY) === 'steps' ? 'steps' : 'pipeline'
  } catch {
    return 'pipeline'
  }
}

export function Templates() {
  const [kind, setKindState] = useState<TemplateKind>(loadKind)
  const setKind = (k: TemplateKind) => {
    setKindState(k)
    try {
      localStorage.setItem(KIND_KEY, k)
    } catch {
      // 忽略
    }
  }
  const { data: templates, error } = usePolling(() => api.listTemplates(kind), [kind], () => null)
  const canEdit = useSession().can('developer')

  return (
    <section>
      <div className="page-head">
        <h1>模板</h1>
        {canEdit && (
          <button className="btn primary" onClick={() => navigate(href.template(kind === 'steps' ? 'new-steps' : 'new-pipeline'))}>
            新建{kindLabel[kind]}
          </button>
        )}
      </div>
      <div className="segmented">
        {(['pipeline', 'steps'] as const).map((k) => (
          <button key={k} className={kind === k ? 'active' : ''} onClick={() => setKind(k)}>
            {kindLabel[k]}
          </button>
        ))}
      </div>
      {kind === 'steps' ? (
        <p className="muted">
          可复用的一组步骤，带输入参数。在流水线的步骤中「引用模板」并填写参数（注入为环境变量）。每次保存生成一个版本：引用「最新版本」的流水线从下次运行起生效，也可锁定某个版本（uses: 名称@版本）；已有运行（含重试）不受影响，被引用的模板不能改名或删除。
        </p>
      ) : (
        <p className="muted">新建流水线时的起点：内置模板覆盖常见语言的构建与部署，团队可以把自己的流水线「另存为模板」或在这里用画布编排。</p>
      )}
      {error && <div className="alert">{error}</div>}
      {templates && templates.length === 0 && <p className="muted">还没有{kindLabel[kind]}。</p>}
      <div className={kind === 'pipeline' ? 'gallery-grid' : 'template-grid'}>
        {templates?.map((t) => (kind === 'pipeline' ? <PipelineTemplateCard key={t.id} t={t} canEdit={canEdit} /> : <StepTemplateCard key={t.id} t={t} canEdit={canEdit} />))}
      </div>
    </section>
  )
}

function PipelineTemplateCard({ t, canEdit }: { t: Template; canEdit: boolean }) {
  const spec = parse(t.yaml).spec
  return (
    <div className="gallery-card as-div">
      <div className="gallery-title">
        <strong>{t.name}</strong>
        {t.builtin ? <span className="tag">内置</span> : <span className="tag">团队</span>}
      </div>
      <span className="muted small">{t.description || '（无描述）'}</span>
      <FlowPreview spec={spec} />
      <div className="gallery-tags">
        {t.category && <span className="tag">{t.category}</span>}
        {specTags(spec).map((x) => (
          <span key={x} className={`tag${x === '部署到主机' ? ' accent' : ''}`}>
            {x}
          </span>
        ))}
      </div>
      {!t.builtin && (
        <span className="muted small">
          v{t.version} · {t.updated_by && `${t.updated_by} · `}
          {fmtTime(t.updated_at)}
        </span>
      )}
      <div className="row">
        {canEdit && (
          <button className="btn primary small" onClick={() => navigate(href.newFromTemplate(t.id))}>
            用此模板新建
          </button>
        )}
        <a className="btn small" href={href.template(t.id)}>
          {t.builtin || !canEdit ? '查看' : '编辑'}
        </a>
      </div>
    </div>
  )
}

function StepTemplateCard({ t, canEdit }: { t: Template; canEdit: boolean }) {
  let inputs = 0
  let steps = 0
  try {
    const js = (parse(t.yaml).doc?.toJS() ?? {}) as { inputs?: object; steps?: unknown[] }
    inputs = Object.keys(js.inputs ?? {}).length
    steps = js.steps?.length ?? 0
  } catch {
    // 展示用，解析失败忽略
  }
  return (
    <a className="card template-card" href={href.template(t.id)}>
      <div className="row">
        <span className="strong mono">{t.name}</span>
      </div>
      <p className="muted">{t.description || '（无描述）'}</p>
      <p className="muted small">
        v{t.version} · {steps} 个步骤 · {inputs} 个参数 · {t.updated_by && `${t.updated_by} · `}
        {fmtTime(t.updated_at)}
      </p>
      <span className="btn small">{canEdit ? '编辑' : '查看'}</span>
    </a>
  )
}
