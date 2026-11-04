package engine

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"flow/internal/model"
	"flow/internal/pipeline"
	"flow/internal/store"
)

//go:embed builtin/*.yml
var builtinFS embed.FS

// builtinPipelines 是内置流水线模板（只读，所有租户可见），顺序即展示顺序。
var builtinPipelines = []struct{ file, name, category, desc string }{
	{"java-deploy.yml", "Java 服务 · 构建并部署到主机", "Java", "Maven 打包，依次部署测试、生产环境的服务器（生产可要求审批）"},
	{"go-deploy.yml", "Go 服务 · 构建并部署到主机", "Go", "测试、编译 Linux 二进制，上传到服务器并重启 systemd 服务"},
	{"web-deploy.yml", "前端站点 · 构建并发布到 Nginx", "Node.js", "npm 构建，把 dist 同时发布到所有 Web 服务器并重载 Nginx"},
	{"docker-deploy.yml", "容器服务 · 镜像构建并部署到主机", "容器", "构建推送 Docker 镜像，在服务器上拉取新版本并重建容器"},
	{"k8s.yml", "Kubernetes 滚动发布", "容器", "构建推送镜像，kubectl 更新 Deployment 并等待滚动完成"},
	{"maven.yml", "Java Maven 构建", "Java", "缓存本地仓库，测试并打包 jar"},
	{"go.yml", "Go 多平台构建", "Go", "测试 + 多平台交叉编译，缓存 Go 模块，上传二进制产物"},
	{"node.yml", "Node.js 构建", "Node.js", "npm ci（缓存 node_modules）、lint、测试、构建，上传 dist"},
	{"python.yml", "Python 测试与打包", "Python", "pip 缓存、pytest 测试、构建 wheel"},
	{"docker.yml", "Docker 镜像构建", "容器", "构建并推送镜像，以提交号为标签；需要带 docker 标签的 Runner"},
	{"deploy.yml", "审批发布流程（示例）", "通用", "构建、测试、人工审批后发布，失败时回滚"},
}

const builtinPrefix = "builtin:"

const maxTemplateBytes = 256 << 10

func builtinTemplates() []*model.Template {
	out := make([]*model.Template, 0, len(builtinPipelines))
	for _, b := range builtinPipelines {
		data, err := builtinFS.ReadFile("builtin/" + b.file)
		if err != nil {
			panic(err)
		}
		out = append(out, &model.Template{
			ID: builtinPrefix + strings.TrimSuffix(b.file, ".yml"), Kind: model.TemplatePipeline,
			Name: b.name, Category: b.category, Description: b.desc, YAML: string(data), Builtin: true,
		})
	}
	return out
}

// Templates 返回租户的模板；流水线模板包含内置模板（排在前面）。kind 为空时返回全部。
func (e *Engine) Templates(ctx context.Context, tenantID string, kind model.TemplateKind) ([]*model.Template, error) {
	own, err := e.store.ListTemplates(ctx, tenantID, kind)
	if err != nil {
		return nil, err
	}
	if kind == "" || kind == model.TemplatePipeline {
		return append(builtinTemplates(), own...), nil
	}
	return own, nil
}

// Template 按 ID 查找模板，包括内置模板。
func (e *Engine) Template(ctx context.Context, id string) (*model.Template, error) {
	if strings.HasPrefix(id, builtinPrefix) {
		for _, t := range builtinTemplates() {
			if t.ID == id {
				return t, nil
			}
		}
		return nil, store.ErrNotFound
	}
	return e.store.GetTemplate(ctx, id)
}

// TemplateInput 是创建 / 更新模板的内容。
type TemplateInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	YAML        string `json:"yaml"`
}

func (e *Engine) CreateTemplate(ctx context.Context, tenantID string, kind model.TemplateKind, in TemplateInput, user string) (*model.Template, error) {
	if err := checkTemplate(kind, &in); err != nil {
		return nil, err
	}
	now := e.now()
	t := &model.Template{ID: newID(), TenantID: tenantID, Kind: kind, Name: in.Name, Description: in.Description,
		YAML: in.YAML, UpdatedBy: user, CreatedAt: now, UpdatedAt: now}
	if err := e.store.CreateTemplate(ctx, t); err != nil {
		return nil, templateConflict(err, in.Name)
	}
	return t, nil
}

// UpdateTemplate 保存新版本。baseVersion 为编辑起点的版本号（0 表示不检查），期间已被他人保存时返回 ErrConflict。
// 步骤模板被流水线引用时不能改名，且修改后引用最新版本的流水线须仍能展开（如不能新增必填输入）；锁定了版本的引用不受影响。
func (e *Engine) UpdateTemplate(ctx context.Context, id string, in TemplateInput, user string, baseVersion int) (*model.Template, error) {
	return e.saveTemplate(ctx, id, in, user, baseVersion, 0)
}

// RestoreTemplate 把模板回退到历史版本 version：以该版本的内容保存为新版本（历史不删除）。
func (e *Engine) RestoreTemplate(ctx context.Context, id string, version int, user string, baseVersion int) (*model.Template, error) {
	if strings.HasPrefix(id, builtinPrefix) {
		return nil, store.ErrNotFound
	}
	rev, err := e.store.GetTemplateRevision(ctx, id, version)
	if err != nil {
		return nil, err
	}
	t, err := e.saveTemplate(ctx, id, TemplateInput{Name: rev.Name, Description: rev.Description, YAML: rev.YAML}, user, baseVersion, version)
	if errors.Is(err, ErrInvalid) {
		return nil, fmt.Errorf("version %d cannot be restored: %w", version, err)
	}
	return t, err
}

func (e *Engine) saveTemplate(ctx context.Context, id string, in TemplateInput, user string, baseVersion, restoredFrom int) (*model.Template, error) {
	if strings.HasPrefix(id, builtinPrefix) {
		return nil, fmt.Errorf("%w: built-in templates are read-only", ErrForbidden)
	}
	t, err := e.store.GetTemplate(ctx, id)
	if err != nil {
		return nil, err
	}
	if baseVersion != 0 && baseVersion != t.Version {
		return nil, templateVersionConflict(t)
	}
	if err := checkTemplate(t.Kind, &in); err != nil {
		return nil, err
	}
	if t.Kind == model.TemplateSteps {
		users, err := e.TemplateUsers(ctx, t.TenantID, t.Name)
		if err != nil {
			return nil, err
		}
		if in.Name != t.Name && len(users) > 0 {
			return nil, fmt.Errorf("%w: template %q is used by %s; it cannot be renamed", store.ErrConflict, t.Name, pipelineNames(users))
		}
		updated, _ := pipeline.ParseStepTemplate([]byte(in.YAML))
		for _, p := range users {
			spec, err := pipeline.Parse([]byte(p.YAML))
			if err != nil {
				continue // 流水线本身已无效，与此次修改无关
			}
			err = spec.ExpandTemplates(func(name string, version int) (*pipeline.StepTemplate, error) {
				if name == t.Name && version == 0 {
					return updated, nil
				}
				return e.lookupTemplate(ctx, e.store, t.TenantID)(name, version)
			})
			if err != nil {
				return nil, fmt.Errorf("%w: change would break pipeline %q: %v", ErrInvalid, p.Name, err)
			}
		}
	}
	if t.Name == in.Name && t.Description == in.Description && t.YAML == in.YAML {
		return t, nil // 无变化不产生新版本
	}
	t.Name, t.Description, t.YAML, t.UpdatedBy, t.UpdatedAt, t.RestoredFrom = in.Name, in.Description, in.YAML, user, e.now(), restoredFrom
	if err := e.store.UpdateTemplate(ctx, t); err != nil {
		if errors.Is(err, store.ErrConflict) {
			if cur, gerr := e.store.GetTemplate(ctx, id); gerr == nil && cur.Version != t.Version {
				return nil, templateVersionConflict(cur)
			}
		}
		return nil, templateConflict(err, in.Name)
	}
	return e.store.GetTemplate(ctx, id)
}

func templateVersionConflict(cur *model.Template) error {
	by := cur.UpdatedBy
	if by == "" {
		by = "someone else"
	}
	return fmt.Errorf("%w: template was saved by %s as version %d after you started editing; reload to see the changes",
		store.ErrConflict, by, cur.Version)
}

// TemplateRevisions 按版本倒序返回模板的保存记录（不含 YAML）；内置模板没有历史。
func (e *Engine) TemplateRevisions(ctx context.Context, id string, limit int) ([]*model.TemplateRevision, error) {
	if strings.HasPrefix(id, builtinPrefix) {
		return []*model.TemplateRevision{}, nil
	}
	return e.store.ListTemplateRevisions(ctx, id, limit)
}

func (e *Engine) TemplateRevision(ctx context.Context, id string, version int) (*model.TemplateRevision, error) {
	if strings.HasPrefix(id, builtinPrefix) {
		return nil, store.ErrNotFound
	}
	return e.store.GetTemplateRevision(ctx, id, version)
}

// DeleteTemplate 删除模板；仍被流水线引用的步骤模板不能删除。
func (e *Engine) DeleteTemplate(ctx context.Context, id string) error {
	if strings.HasPrefix(id, builtinPrefix) {
		return fmt.Errorf("%w: built-in templates are read-only", ErrForbidden)
	}
	t, err := e.store.GetTemplate(ctx, id)
	if err != nil {
		return err
	}
	if t.Kind == model.TemplateSteps {
		users, err := e.TemplateUsers(ctx, t.TenantID, t.Name)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			return fmt.Errorf("%w: template %q is still used by %s", store.ErrConflict, t.Name, pipelineNames(users))
		}
	}
	return e.store.DeleteTemplate(ctx, id)
}

// TemplateUsers 返回引用了某个步骤模板的流水线。
func (e *Engine) TemplateUsers(ctx context.Context, tenantID, name string) ([]*model.Pipeline, error) {
	refs, err := e.TemplateRefs(ctx, tenantID, name)
	if err != nil {
		return nil, err
	}
	out := make([]*model.Pipeline, len(refs))
	for i, r := range refs {
		out[i] = r.Pipeline
	}
	return out, nil
}

// TemplateRef 是一条流水线对步骤模板的引用；Versions 为所用版本（0 表示最新版本）。
type TemplateRef struct {
	Pipeline *model.Pipeline
	Versions []int
}

func (e *Engine) TemplateRefs(ctx context.Context, tenantID, name string) ([]TemplateRef, error) {
	pipes, err := e.store.ListPipelines(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	var out []TemplateRef
	for _, p := range pipes {
		if spec, err := pipeline.Parse([]byte(p.YAML)); err == nil {
			if vs := spec.TemplateRefs()[name]; len(vs) > 0 {
				out = append(out, TemplateRef{p, vs})
			}
		}
	}
	return out, nil
}

func checkTemplate(kind model.TemplateKind, in *TemplateInput) error {
	in.Name, in.Description = strings.TrimSpace(in.Name), strings.TrimSpace(in.Description)
	if len(in.YAML) > maxTemplateBytes {
		return fmt.Errorf("%w: template is larger than %d KiB", ErrInvalid, maxTemplateBytes>>10)
	}
	switch kind {
	case model.TemplateSteps:
		if !pipeline.ValidTemplateName(in.Name) {
			return fmt.Errorf("%w: step template name must be lowercase letters, digits, '.', '_' or '-' (e.g. go-build)", ErrInvalid)
		}
		t, err := pipeline.ParseStepTemplate([]byte(in.YAML))
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if in.Description == "" {
			in.Description = t.Description
		}
	case model.TemplatePipeline:
		if in.Name == "" || utf8.RuneCountInString(in.Name) > 64 {
			return fmt.Errorf("%w: name is required (at most 64 characters)", ErrInvalid)
		}
		if _, err := pipeline.Parse([]byte(in.YAML)); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	default:
		return fmt.Errorf("%w: unknown template kind %q (want steps or pipeline)", ErrInvalid, kind)
	}
	if utf8.RuneCountInString(in.Description) > 500 {
		return fmt.Errorf("%w: description is longer than 500 characters", ErrInvalid)
	}
	return nil
}

func templateConflict(err error, name string) error {
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("%w: a template named %q already exists", store.ErrConflict, name)
	}
	return err
}

func pipelineNames(ps []*model.Pipeline) string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = fmt.Sprintf("%q", p.Name)
	}
	return strings.Join(names, ", ")
}

// templateSource 是 Store 与 Tx 共有的模板查询方法。
type templateSource interface {
	GetTemplateByName(ctx context.Context, tenantID string, kind model.TemplateKind, name string) (*model.Template, error)
	GetTemplateRevisionByName(ctx context.Context, tenantID string, kind model.TemplateKind, name string, version int) (*model.TemplateRevision, error)
}

// storeError 标记查询模板时的基础设施错误，以便与用户配置错误区分。
type storeError struct{ err error }

func (s storeError) Error() string { return s.err.Error() }
func (s storeError) Unwrap() error { return s.err }

func (e *Engine) lookupTemplate(ctx context.Context, src templateSource, tenantID string) pipeline.TemplateLookup {
	return func(name string, version int) (*pipeline.StepTemplate, error) {
		var text string
		if version == 0 {
			t, err := src.GetTemplateByName(ctx, tenantID, model.TemplateSteps, name)
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("step template %q not found", name)
			}
			if err != nil {
				return nil, storeError{err}
			}
			text = t.YAML
		} else {
			r, err := src.GetTemplateRevisionByName(ctx, tenantID, model.TemplateSteps, name, version)
			if errors.Is(err, store.ErrNotFound) {
				if _, err := src.GetTemplateByName(ctx, tenantID, model.TemplateSteps, name); errors.Is(err, store.ErrNotFound) {
					return nil, fmt.Errorf("step template %q not found", name)
				}
				return nil, fmt.Errorf("step template %q has no version %d", name, version)
			}
			if err != nil {
				return nil, storeError{err}
			}
			text = r.YAML
		}
		st, err := pipeline.ParseStepTemplate([]byte(text))
		if err != nil {
			return nil, fmt.Errorf("step template %q is invalid: %v", name, err)
		}
		return st, nil
	}
}

// expandTemplates 展开 spec 中的步骤模板；配置错误包装为 ErrInvalid，存储错误原样返回。
func (e *Engine) expandTemplates(ctx context.Context, src templateSource, tenantID string, spec *pipeline.Spec) error {
	err := spec.ExpandTemplates(e.lookupTemplate(ctx, src, tenantID))
	var se storeError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &se):
		return se.err
	default:
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
}
