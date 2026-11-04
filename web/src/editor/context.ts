import { createContext, useContext } from 'react'
import type { Document } from 'yaml'
import type { Environment, HostGroup, Template } from '../types'
import type { Dep, JobRef, PipelineSpec } from './model'

// 选中项同时记下名称：拖动、撤销后下标可能变化，编辑器据名称重新定位。
export type Selection = { kind: 'job'; ref: JobRef; name: string } | { kind: 'stage'; si: number; name: string } | { kind: 'pipeline' } | null

export interface EditorApi {
  spec: PipelineSpec
  deps: Dep[][]
  stageIssues: string[][]
  jobIssues: string[][][]
  selection: Selection
  select: (s: Selection) => void
  readOnly: boolean
  /** coalesce：同一键在短时间内的连续修改合并为一步撤销。 */
  apply: (fn: (doc: Document) => void, coalesce?: string) => void
  stepTemplates: Template[]
  /** 部署任务可选的主机组及其所属环境 */
  hostGroups: HostGroup[]
  environments: Environment[]
  /** 打开任务组件库，选中的任务插入到阶段 si 的 at 位置。 */
  pickTask: (si: number, at: number) => void
}

export const EditorContext = createContext<EditorApi | null>(null)

export function useEditor() {
  const ctx = useContext(EditorContext)
  if (!ctx) throw new Error('useEditor outside EditorContext')
  return ctx
}
