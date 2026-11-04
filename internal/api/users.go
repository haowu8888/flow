package api

import (
	"net/http"
	"time"

	"flow/internal/auth"
	"flow/internal/model"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// meResponse 描述当前用户、当前租户与角色，以及可切换的租户。
type meResponse struct {
	User     *model.User       `json:"user"`
	TenantID string            `json:"tenant_id"`
	Role     model.Role        `json:"role"`
	Tenants  []auth.TenantView `json:"tenants"`
}

type tenantRequest struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type passwordRequest struct {
	OldPassword string `json:"old_password"`
	Password    string `json:"password"`
}

type memberRequest struct {
	Username    string     `json:"username"`
	DisplayName string     `json:"display_name"`
	Password    string     `json:"password"` // 用户不存在时用于创建
	Role        model.Role `json:"role"`
}

type tokenRequest struct {
	Name string `json:"name"`
	// ExpiresInDays 为有效天数，0 或缺省为永不过期。
	ExpiresInDays int `json:"expires_in_days,omitempty"`
}

// tokenCreated 携带只展示一次的令牌明文。
type tokenCreated struct {
	*model.Token
	Plain string `json:"token"`
}

// login 校验用户名密码，写入会话 Cookie（HttpOnly），同时在响应体返回令牌供脚本使用。
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decode(w, r, &req) {
		return
	}
	plain, p, err := s.auth.LoginFrom(r.Context(), req.Username, req.Password, auth.SessionMeta{UserAgent: r.UserAgent(), IP: clientIP(r)})
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: plain, Path: "/", MaxAge: int(auth.SessionTTL / time.Second),
		HttpOnly: true, Secure: s.cfg.SecureCookie, SameSite: http.SameSiteLaxMode,
	})
	me, err := s.meView(r, p)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	respond(w, http.StatusOK, struct {
		*meResponse
		Token string `json:"token"`
	}{me, plain}, nil)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: s.cfg.SecureCookie, SameSite: http.SameSiteLaxMode})
	if p.Token.Kind != auth.KindSession {
		w.WriteHeader(http.StatusNoContent) // 不通过登出接口删除 API 令牌
		return
	}
	respond(w, http.StatusNoContent, nil, s.auth.Logout(r.Context(), p))
}

func (s *server) meView(r *http.Request, p *auth.Principal) (*meResponse, error) {
	ts, err := s.auth.Tenants(r.Context(), p.User)
	if err != nil {
		return nil, err
	}
	return &meResponse{User: p.User, TenantID: p.TenantID, Role: p.Role, Tenants: ts}, nil
}

func (s *server) me(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	me, err := s.meView(r, p)
	respond(w, http.StatusOK, me, err)
}

func (s *server) switchTenant(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req tenantRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.auth.SwitchTenant(r.Context(), p, req.ID); err != nil {
		respond(w, 0, nil, err)
		return
	}
	np, err := s.auth.Authenticate(r.Context(), currentToken(r))
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	me, err := s.meView(r, np)
	respond(w, http.StatusOK, me, err)
}

// currentToken 取出本次请求使用的令牌明文（鉴权已在中间件完成）。
func currentToken(r *http.Request) string {
	if t := bearer(r); t != "" {
		return t
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

func (s *server) changePassword(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req passwordRequest
	if !decode(w, r, &req) {
		return
	}
	respond(w, http.StatusNoContent, nil, s.auth.ChangePassword(r.Context(), p, req.OldPassword, req.Password))
}

func (s *server) listAPITokens(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ts, err := s.auth.APITokens(r.Context(), p)
	respond(w, http.StatusOK, nonNil(ts), err)
}

func (s *server) createAPIToken(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req tokenRequest
	if !decode(w, r, &req) {
		return
	}
	plain, t, err := s.auth.CreateAPIToken(r.Context(), p, req.Name, time.Duration(req.ExpiresInDays)*24*time.Hour)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	respond(w, http.StatusCreated, tokenCreated{Token: t, Plain: plain}, nil)
}

func (s *server) deleteAPIToken(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	respond(w, http.StatusNoContent, nil, s.auth.DeleteAPIToken(r.Context(), p, r.PathValue("id")))
}

// sessionView 标出当前请求所用的会话。
type sessionView struct {
	*model.Token
	Current bool `json:"current"`
}

func (s *server) listSessions(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ts, err := s.auth.Sessions(r.Context(), p)
	out := make([]sessionView, len(ts))
	for i, t := range ts {
		out[i] = sessionView{Token: t, Current: t.ID == p.Token.ID}
	}
	respond(w, http.StatusOK, out, err)
}

func (s *server) revokeSession(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	respond(w, http.StatusNoContent, nil, s.auth.RevokeSession(r.Context(), p, r.PathValue("id")))
}

func (s *server) updateProfile(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req auth.Profile
	if !decode(w, r, &req) {
		return
	}
	u, err := s.auth.UpdateProfile(r.Context(), p, req)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	p.User = u
	me, err := s.meView(r, p)
	respond(w, http.StatusOK, me, err)
}

// deleteAccount 删除自己的账号，请求体 {"password": "..."}；成功后清除会话 Cookie。
func (s *server) deleteAccount(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.auth.DeleteAccount(r.Context(), p, req.Password); err != nil {
		respond(w, 0, nil, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cfg.SecureCookie})
	respond(w, http.StatusNoContent, nil, nil)
}

func (s *server) createTenant(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req tenantRequest
	if !decode(w, r, &req) {
		return
	}
	t, err := s.auth.CreateTenant(r.Context(), p, req.ID, req.Name)
	respond(w, http.StatusCreated, t, err)
}

func (s *server) listMembers(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ms, err := s.auth.Members(r.Context(), p)
	respond(w, http.StatusOK, ms, err)
}

func (s *server) addMember(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req memberRequest
	if !decode(w, r, &req) {
		return
	}
	m, err := s.auth.AddMember(r.Context(), p, req.Username, req.DisplayName, req.Password, req.Role)
	respond(w, http.StatusCreated, m, err)
}

func (s *server) setMemberRole(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req memberRequest
	if !decode(w, r, &req) {
		return
	}
	respond(w, http.StatusNoContent, nil, s.auth.SetMemberRole(r.Context(), p, r.PathValue("id"), req.Role))
}

func (s *server) removeMember(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	respond(w, http.StatusNoContent, nil, s.auth.RemoveMember(r.Context(), p, r.PathValue("id")))
}

func (s *server) resetPassword(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req passwordRequest
	if !decode(w, r, &req) {
		return
	}
	respond(w, http.StatusNoContent, nil, s.auth.ResetPassword(r.Context(), p, r.PathValue("id"), req.Password))
}

// nonNil 让空列表编码为 [] 而不是 null。
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
