import { useMemo } from 'react'
import { isApproval, parse, type PipelineSpec } from '../editor/model'

/** 流水线的缩略流程图：阶段为列，任务为其中的小卡片（部署 / 卡点带标识）。 */
export function FlowPreview({ yaml, spec: given, maxJobs = 3 }: { yaml?: string; spec?: PipelineSpec; maxJobs?: number }) {
  const spec = useMemo(() => given ?? (yaml ? parse(yaml).spec : undefined), [given, yaml])
  if (!spec) return <div className="flow-preview muted small">无法预览</div>
  return (
    <div className="flow-preview">
      {spec.stages.map((st, si) => (
        <div key={si} className="fp-stage">
          <div className="fp-stage-name" title={st.name}>
            {st.name}
          </div>
          {st.jobs.slice(0, maxJobs).map((j, ji) => {
            const kind = isApproval(j) ? 'approval' : j.deploy ? 'deploy' : 'run'
            return (
              <div key={ji} className={`fp-job ${kind}`} title={j.deploy ? `部署到 ${j.deploy.hosts}` : j.name}>
                <span aria-hidden>{kind === 'approval' ? '✋' : kind === 'deploy' ? '⇪' : '▸'}</span>
                <span className="fp-job-name">{j.name}</span>
              </div>
            )
          })}
          {st.jobs.length > maxJobs && <div className="fp-more">+{st.jobs.length - maxJobs}</div>}
        </div>
      ))}
    </div>
  )
}

/** 从定义推断的特征标签，用于模板卡片。 */
export function specTags(spec?: PipelineSpec): string[] {
  if (!spec) return []
  const jobs = spec.stages.flatMap((s) => s.jobs)
  const tags: string[] = []
  if (jobs.some((j) => j.deploy)) tags.push('部署到主机')
  if (jobs.some((j) => isApproval(j))) tags.push('人工审批')
  if (jobs.some((j) => j.matrix)) tags.push('矩阵构建')
  if (jobs.some((j) => j.cache)) tags.push('依赖缓存')
  if (jobs.some((j) => /kubectl/.test(j.steps.map((s) => s.run).join('\n')))) tags.push('Kubernetes')
  if (jobs.some((j) => /docker (build|push)/.test(j.steps.map((s) => s.run).join('\n')))) tags.push('Docker 镜像')
  return tags
}
