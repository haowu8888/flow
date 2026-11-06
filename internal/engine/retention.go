package engine

import (
	"context"

	"flow/internal/pipeline"
)

// retentionBatch 是每批清理的条数，避免长事务与大 IN 列表。
const retentionBatch = 200

// ApplyRetention 执行保留策略：删除过期产物（保留各流水线最近一次成功运行的），
// 以及超出流水线 retention.runs 的已结束旧运行。先删记录再删对象存储，删除对象失败只记日志。
func (e *Engine) ApplyRetention(ctx context.Context) (artifacts, runs int, err error) {
	for {
		arts, err := e.store.ExpiredArtifacts(ctx, e.now(), retentionBatch)
		if err != nil {
			return artifacts, runs, err
		}
		if len(arts) == 0 {
			break
		}
		ids := make([]string, len(arts))
		for i, a := range arts {
			ids[i] = a.ID
		}
		if err := e.store.DeleteArtifacts(ctx, ids); err != nil {
			return artifacts, runs, err
		}
		for _, a := range arts {
			e.deleteBlob(ctx, a.BlobKey)
			e.runChanged(a.RunID)
		}
		artifacts += len(arts)
		if len(arts) < retentionBatch {
			break
		}
	}

	tenants, err := e.store.ListTenants(ctx)
	if err != nil {
		return artifacts, runs, err
	}
	for _, t := range tenants {
		pipes, err := e.store.ListPipelines(ctx, t.ID)
		if err != nil {
			return artifacts, runs, err
		}
		for _, p := range pipes {
			spec, err := pipeline.Parse([]byte(p.YAML))
			if err != nil || spec.Retention == nil || spec.Retention.Runs <= 0 {
				continue
			}
			for {
				n, keys, err := e.store.PruneRuns(ctx, p.ID, spec.Retention.Runs, retentionBatch)
				if err != nil {
					return artifacts, runs, err
				}
				for _, k := range keys {
					e.deleteBlob(ctx, k)
				}
				runs += n
				if n < retentionBatch {
					break
				}
			}
		}
	}
	return artifacts, runs, nil
}

func (e *Engine) deleteBlob(ctx context.Context, key string) {
	if err := e.removeBlob(ctx, key); err != nil {
		e.logf("delete artifact blob %s: %v", key, err)
	}
}
