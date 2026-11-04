package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"flow/internal/model"
)

// 流水线成员、公开与自定义角色。

func (e *Engine) UserPipelineRoles(ctx context.Context, userID string) (map[string]string, error) {
	return e.store.UserPipelineRoles(ctx, userID)
}

func (e *Engine) PipelineRoles(ctx context.Context, tenantID string) ([]*model.PipelineRole, error) {
	return e.store.ListPipelineRoles(ctx, tenantID)
}

func (e *Engine) RoleUsage(ctx context.Context, tenantID string) (map[string]int, error) {
	return e.store.RoleUsage(ctx, tenantID)
}

func (e *Engine) PipelineMembers(ctx context.Context, pipelineID string) ([]*model.PipelineMember, error) {
	return e.store.ListPipelineMembers(ctx, pipelineID)
}

func (e *Engine) SetPipelinePublic(ctx context.Context, id string, public bool) error {
	return e.store.SetPipelinePublic(ctx, id, public)
}

func (e *Engine) User(ctx context.Context, id string) (*model.User, error) {
	return e.store.GetUser(ctx, id)
}

func (e *Engine) UserByName(ctx context.Context, name string) (*model.User, error) {
	return e.store.GetUserByName(ctx, name)
}

// SetPipelineMember 添加成员或修改其角色。成员须属于流水线所在租户；不能设为拥有者，拥有者本人也不能被设为成员。
func (e *Engine) SetPipelineMember(ctx context.Context, p *model.Pipeline, userID, roleID string) error {
	if roleID == model.RoleOwnerID {
		return fmt.Errorf("%w: a member cannot be made owner", ErrInvalid)
	}
	if err := e.checkRole(ctx, p.TenantID, roleID); err != nil {
		return err
	}
	u, err := e.store.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if u.Username == p.CreatedBy {
		return fmt.Errorf("%w: %s is the owner of this pipeline", ErrInvalid, u.Username)
	}
	ms, err := e.store.ListMembers(ctx, p.TenantID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(ms, func(m *model.Membership) bool { return m.UserID == userID }) {
		return fmt.Errorf("%w: %s is not a member of this tenant", ErrInvalid, u.Username)
	}
	return e.store.PutPipelineMember(ctx, &model.PipelineMember{PipelineID: p.ID, UserID: userID, RoleID: roleID, CreatedAt: e.now()})
}

func (e *Engine) RemovePipelineMember(ctx context.Context, pipelineID, userID string) error {
	return e.store.DeletePipelineMember(ctx, pipelineID, userID)
}

func (e *Engine) checkRole(ctx context.Context, tenantID, roleID string) error {
	for _, r := range model.BuiltinRoles() {
		if r.ID == roleID {
			return nil
		}
	}
	roles, err := e.store.ListPipelineRoles(ctx, tenantID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(roles, func(r *model.PipelineRole) bool { return r.ID == roleID }) {
		return fmt.Errorf("%w: unknown role %q", ErrInvalid, roleID)
	}
	return nil
}

// SavePipelineRole 新建（ID 为空）或修改自定义角色；查看权限总是包含在内。
func (e *Engine) SavePipelineRole(ctx context.Context, r *model.PipelineRole) (*model.PipelineRole, error) {
	r.Name, r.Description = strings.TrimSpace(r.Name), strings.TrimSpace(r.Description)
	if r.Name == "" || utf8.RuneCountInString(r.Name) > 32 || utf8.RuneCountInString(r.Description) > 200 {
		return nil, fmt.Errorf("%w: role name is required (at most 32 characters, description at most 200)", ErrInvalid)
	}
	for _, b := range model.BuiltinRoles() {
		if r.ID == b.ID || r.Name == b.Name {
			return nil, fmt.Errorf("%w: built-in role %q cannot be changed", ErrInvalid, b.Name)
		}
	}
	perms := []string{model.PermView}
	for _, p := range r.Permissions {
		if !slices.Contains(model.AllPermissions, p) {
			return nil, fmt.Errorf("%w: unknown permission %q", ErrInvalid, p)
		}
	}
	for _, p := range model.AllPermissions[1:] {
		if slices.Contains(r.Permissions, p) {
			perms = append(perms, p)
		}
	}
	r.Permissions = perms
	if r.ID == "" {
		r.ID, r.CreatedAt = "r-"+randHex(6), e.now()
	} else if err := e.checkRole(ctx, r.TenantID, r.ID); err != nil {
		return nil, err
	}
	if err := e.store.PutPipelineRole(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// DeletePipelineRole 删除自定义角色；仍有流水线在用时拒绝。
func (e *Engine) DeletePipelineRole(ctx context.Context, tenantID, id string) error {
	usage, err := e.store.RoleUsage(ctx, tenantID)
	if err != nil {
		return err
	}
	if n := usage[id]; n > 0 {
		return fmt.Errorf("%w: role is used by %d pipeline(s)", ErrInvalid, n)
	}
	return e.store.DeletePipelineRole(ctx, tenantID, id)
}
