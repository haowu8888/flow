package api

import (
	"context"
	"net/http"
	"slices"

	"flow/internal/auth"
	"flow/internal/model"
	"flow/internal/store"
)

// 流水线权限：租户管理员与拥有者拥有全部权限；成员按角色授权；公开的流水线其他人只能查看；
// 租户内的只读成员无论角色如何都只能查看。没有查看权限时按不存在处理（404）。

// accessor 缓存一个请求主体在当前租户的成员角色，用于批量判断（列表、仪表盘）。
type accessor struct {
	p       *auth.Principal
	members map[string]string              // 流水线 ID → 角色 ID
	custom  map[string]*model.PipelineRole // 自定义角色
}

func (s *server) accessFor(ctx context.Context, p *auth.Principal) (*accessor, error) {
	a := &accessor{p: p, custom: map[string]*model.PipelineRole{}}
	if p.Role == model.RoleAdmin {
		return a, nil
	}
	var err error
	if a.members, err = s.eng.UserPipelineRoles(ctx, p.User.ID); err != nil {
		return nil, err
	}
	roles, err := s.eng.PipelineRoles(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	for _, r := range roles {
		a.custom[r.ID] = r
	}
	return a, nil
}

// perms 返回主体对流水线的权限（按 model.AllPermissions 的顺序）。
func (a *accessor) perms(pl *model.Pipeline) []string {
	if pl.TenantID != a.p.TenantID {
		return nil
	}
	if a.p.Role == model.RoleAdmin || (pl.CreatedBy != "" && pl.CreatedBy == a.p.User.Username) {
		return a.cap(model.AllPermissions)
	}
	if roleID, ok := a.members[pl.ID]; ok {
		return a.cap(rolePerms(roleID, a.custom))
	}
	if pl.IsPublic {
		return []string{model.PermView}
	}
	return nil
}

// cap 按租户角色收紧：只读成员只能查看。
func (a *accessor) cap(perms []string) []string {
	if a.p.Role.Rank() < model.RoleDeveloper.Rank() {
		if slices.Contains(perms, model.PermView) {
			return []string{model.PermView}
		}
		return nil
	}
	return perms
}

func (a *accessor) can(pl *model.Pipeline, perm string) bool {
	return slices.Contains(a.perms(pl), perm)
}

// rolePerms 返回角色的权限；查看总是包含在内，未知角色只能查看。
func rolePerms(id string, custom map[string]*model.PipelineRole) []string {
	var perms []string
	for _, r := range model.BuiltinRoles() {
		if r.ID == id {
			perms = r.Permissions
		}
	}
	if r := custom[id]; r != nil {
		perms = r.Permissions
	}
	out := []string{}
	for _, p := range model.AllPermissions {
		if p == model.PermView || slices.Contains(perms, p) {
			out = append(out, p)
		}
	}
	return out
}

// pipelineAccess 取出流水线并校验权限：不可见为 404，可见但缺少 perm 为 403。
func (s *server) pipelineAccess(ctx context.Context, pl *model.Pipeline, p *auth.Principal, perm string) (*model.Pipeline, error) {
	if pl.TenantID != p.TenantID {
		return nil, store.ErrNotFound
	}
	a, err := s.accessFor(ctx, p)
	if err != nil {
		return nil, err
	}
	perms := a.perms(pl)
	if !slices.Contains(perms, model.PermView) {
		return nil, store.ErrNotFound
	}
	if !slices.Contains(perms, perm) {
		return nil, auth.ErrForbidden
	}
	return pl, nil
}

// pipelineView 是带当前用户权限的流水线。
type pipelineView struct {
	*model.Pipeline
	Permissions []string `json:"permissions"`
}

func (s *server) withPerms(ctx context.Context, pl *model.Pipeline, p *auth.Principal) (*pipelineView, error) {
	a, err := s.accessFor(ctx, p)
	if err != nil {
		return nil, err
	}
	return &pipelineView{Pipeline: pl, Permissions: a.perms(pl)}, nil
}

// ---- 成员与角色 ----

type memberView struct {
	*model.PipelineMember
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
}

// pipelineMembers 返回拥有者在前的成员列表（拥有者不入库，按 created_by 补上）。
func (s *server) pipelineMembers(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	pl, err := s.pipelineOf(r.Context(), r.PathValue("id"), p, model.PermView)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	ms, err := s.eng.PipelineMembers(r.Context(), pl.ID)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	out := []memberView{}
	if pl.CreatedBy != "" {
		if u, err := s.eng.UserByName(r.Context(), pl.CreatedBy); err == nil {
			out = append(out, memberView{PipelineMember: &model.PipelineMember{PipelineID: pl.ID, UserID: u.ID, RoleID: model.RoleOwnerID, CreatedAt: pl.CreatedAt},
				Username: u.Username, DisplayName: u.DisplayName, Email: u.Email})
		}
	}
	for _, m := range ms {
		u, err := s.eng.User(r.Context(), m.UserID)
		if err != nil {
			continue
		}
		out = append(out, memberView{PipelineMember: m, Username: u.Username, DisplayName: u.DisplayName, Email: u.Email})
	}
	respond(w, http.StatusOK, out, nil)
}

func (s *server) putPipelineMember(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req struct {
		RoleID string `json:"role_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	pl, err := s.pipelineOf(r.Context(), r.PathValue("id"), p, model.PermManageMembers)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.SetPipelineMember(r.Context(), pl, r.PathValue("user"), req.RoleID))
}

func (s *server) deletePipelineMember(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	pl, err := s.pipelineOf(r.Context(), r.PathValue("id"), p, model.PermManageMembers)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.RemovePipelineMember(r.Context(), pl.ID, r.PathValue("user")))
}

func (s *server) setPipelinePublic(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req struct {
		Public bool `json:"public"`
	}
	if !decode(w, r, &req) {
		return
	}
	pl, err := s.pipelineOf(r.Context(), r.PathValue("id"), p, model.PermManageMembers)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	if err := s.eng.SetPipelinePublic(r.Context(), pl.ID, req.Public); err != nil {
		respond(w, 0, nil, err)
		return
	}
	pl.IsPublic = req.Public
	v, err := s.withPerms(r.Context(), pl, p)
	respond(w, http.StatusOK, v, err)
}

type roleView struct {
	*model.PipelineRole
	Pipelines int `json:"pipelines"` // 使用该角色的流水线数
}

func (s *server) listPipelineRoles(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	custom, err := s.eng.PipelineRoles(r.Context(), p.TenantID)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	usage, err := s.eng.RoleUsage(r.Context(), p.TenantID)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	out := []roleView{}
	for _, role := range append(model.BuiltinRoles(), custom...) {
		out = append(out, roleView{PipelineRole: role, Pipelines: usage[role.ID]})
	}
	respond(w, http.StatusOK, out, nil)
}

func (s *server) savePipelineRole(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req model.PipelineRole
	if !decode(w, r, &req) {
		return
	}
	req.TenantID, req.ID = p.TenantID, r.PathValue("id")
	role, err := s.eng.SavePipelineRole(r.Context(), &req)
	status := http.StatusOK
	if r.PathValue("id") == "" {
		status = http.StatusCreated
	}
	respond(w, status, role, err)
}

func (s *server) deletePipelineRole(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	respond(w, http.StatusNoContent, nil, s.eng.DeletePipelineRole(r.Context(), p.TenantID, r.PathValue("id")))
}
