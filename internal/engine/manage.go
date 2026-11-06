package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"flow/internal/model"
	"flow/internal/pipeline"
	"flow/internal/store"

	"gopkg.in/yaml.v3"
)

// 运行记录删除、流水线复制与分组管理。

// DeleteRun 删除一条已结束的运行（含日志与产物）。
func (e *Engine) DeleteRun(ctx context.Context, runID string) error {
	keys, err := e.store.DeleteRun(ctx, runID)
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("%w: the run is still in progress; cancel it first", ErrInvalid)
	}
	if err != nil {
		return err
	}
	e.deleteBlobs(ctx, keys)
	return nil
}

// DeleteFinishedRuns 删除流水线全部已结束的运行，运行中的保留；返回删除的数量。
func (e *Engine) DeleteFinishedRuns(ctx context.Context, pipelineID string) (int, error) {
	total := 0
	for {
		n, keys, err := e.store.PruneRuns(ctx, pipelineID, 0, 500)
		if err != nil {
			return total, err
		}
		e.deleteBlobs(ctx, keys)
		total += n
		if n < 500 {
			return total, nil
		}
	}
}

func (e *Engine) deleteBlobs(ctx context.Context, keys []string) {
	for _, k := range keys {
		if err := e.removeBlob(ctx, k); err != nil {
			e.logf("delete artifact blob %s: %v", k, err)
		}
	}
}

// CopyPipeline 复制流水线配置为新流水线（新名称写入 YAML 的 name；触发器、密钥、运行记录不复制）。
func (e *Engine) CopyPipeline(ctx context.Context, src *model.Pipeline, name, user string) (*model.Pipeline, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = src.Name + " 副本"
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src.YAML), &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: pipeline YAML cannot be copied", ErrInvalid)
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "name" {
			root.Content[i+1].Value, root.Content[i+1].Style = name, 0
		}
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, err
	}
	if _, err := pipeline.Parse(out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	p, err := e.CreatePipeline(ctx, src.TenantID, string(out), user)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, fmt.Errorf("%w: a pipeline named %q already exists", store.ErrConflict, name)
		}
		return nil, err
	}
	if src.Group != "" {
		if err := e.store.SetPipelineGroup(ctx, p.ID, src.Group); err == nil {
			p.Group = src.Group
		}
	}
	return p, nil
}

func checkGroupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxGroupLen || strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("%w: group name is required (at most %d characters)", ErrInvalid, maxGroupLen)
	}
	return name, nil
}

func (e *Engine) Groups(ctx context.Context, tenantID string) ([]string, error) {
	return e.store.ListGroups(ctx, tenantID)
}

func (e *Engine) CreateGroup(ctx context.Context, tenantID, name string) error {
	name, err := checkGroupName(name)
	if err != nil {
		return err
	}
	return e.store.CreateGroup(ctx, tenantID, name, e.now())
}

func (e *Engine) RenameGroup(ctx context.Context, tenantID, from, to string) error {
	to, err := checkGroupName(to)
	if err != nil {
		return err
	}
	return e.store.RenameGroup(ctx, tenantID, from, to, e.now())
}

func (e *Engine) DeleteGroup(ctx context.Context, tenantID, name string) error {
	return e.store.DeleteGroup(ctx, tenantID, name)
}
