package api

import (
	"context"
	"net/http"
	"strconv"

	"flow/internal/auth"
	"flow/internal/engine"
	"flow/internal/model"
	"flow/internal/store"
)

// 环境、主机组、主机：查看对所有成员开放；修改需要管理员（主机凭据可登录服务器）。
// 连接测试开发者即可，部署记录所有成员可查看。
func (s *server) routeHosts(mux *http.ServeMux, viewer, dev, admin func(handler) http.HandlerFunc) {
	mux.HandleFunc("GET /api/environments", viewer(s.listEnvironments))
	mux.HandleFunc("POST /api/environments", admin(s.createEnvironment))
	mux.HandleFunc("PUT /api/environments/{id}", admin(s.updateEnvironment))
	mux.HandleFunc("DELETE /api/environments/{id}", admin(s.deleteEnvironment))

	mux.HandleFunc("GET /api/host-groups", viewer(s.listHostGroups))
	mux.HandleFunc("POST /api/host-groups", admin(s.createHostGroup))
	mux.HandleFunc("PUT /api/host-groups/{id}", admin(s.updateHostGroup))
	mux.HandleFunc("DELETE /api/host-groups/{id}", admin(s.deleteHostGroup))
	mux.HandleFunc("GET /api/host-groups/{id}/users", viewer(s.hostGroupUsers))

	mux.HandleFunc("GET /api/hosts", viewer(s.listHosts))
	mux.HandleFunc("POST /api/hosts", admin(s.createHost))
	mux.HandleFunc("PUT /api/hosts/{id}", admin(s.updateHost))
	mux.HandleFunc("DELETE /api/hosts/{id}", admin(s.deleteHost))
	mux.HandleFunc("POST /api/hosts/{id}/test", dev(s.testHost))
	mux.HandleFunc("POST /api/hosts/{id}/reset-key", admin(s.resetHostKey))

	mux.HandleFunc("GET /api/deployments", viewer(s.listDeployments))
}

func (s *server) envOf(ctx context.Context, id string, p *auth.Principal) error {
	e, err := s.eng.Environment(ctx, id)
	if err != nil {
		return err
	}
	return inTenant(nil, e.TenantID, p)
}

func (s *server) groupOf(ctx context.Context, id string, p *auth.Principal) (*model.HostGroup, error) {
	g, err := s.eng.HostGroup(ctx, id)
	if err != nil {
		return nil, err
	}
	return g, inTenant(nil, g.TenantID, p)
}

func (s *server) hostOf(ctx context.Context, id string, p *auth.Principal) error {
	h, err := s.eng.Host(ctx, id)
	if err != nil {
		return err
	}
	return inTenant(nil, h.TenantID, p)
}

func (s *server) listEnvironments(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	list, err := s.eng.Environments(r.Context(), p.TenantID)
	respond(w, http.StatusOK, list, err)
}

func (s *server) createEnvironment(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var in engine.EnvironmentInput
	if !decode(w, r, &in) {
		return
	}
	env, err := s.eng.CreateEnvironment(r.Context(), p.TenantID, in)
	respond(w, http.StatusCreated, env, err)
}

func (s *server) updateEnvironment(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var in engine.EnvironmentInput
	if !decode(w, r, &in) {
		return
	}
	if err := s.envOf(r.Context(), r.PathValue("id"), p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	env, err := s.eng.UpdateEnvironment(r.Context(), r.PathValue("id"), in)
	respond(w, http.StatusOK, env, err)
}

func (s *server) deleteEnvironment(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if err := s.envOf(r.Context(), r.PathValue("id"), p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.DeleteEnvironment(r.Context(), r.PathValue("id")))
}

func (s *server) listHostGroups(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	list, err := s.eng.HostGroups(r.Context(), p.TenantID)
	respond(w, http.StatusOK, list, err)
}

func (s *server) createHostGroup(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var in engine.HostGroupInput
	if !decode(w, r, &in) {
		return
	}
	g, err := s.eng.CreateHostGroup(r.Context(), p.TenantID, in)
	respond(w, http.StatusCreated, g, err)
}

func (s *server) updateHostGroup(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var in engine.HostGroupInput
	if !decode(w, r, &in) {
		return
	}
	if _, err := s.groupOf(r.Context(), r.PathValue("id"), p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	g, err := s.eng.UpdateHostGroup(r.Context(), r.PathValue("id"), in)
	respond(w, http.StatusOK, g, err)
}

func (s *server) deleteHostGroup(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if _, err := s.groupOf(r.Context(), r.PathValue("id"), p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.DeleteHostGroup(r.Context(), r.PathValue("id")))
}

func (s *server) hostGroupUsers(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	g, err := s.groupOf(r.Context(), r.PathValue("id"), p)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	users, err := s.eng.HostGroupUsers(r.Context(), p.TenantID, g.Name)
	out := make([]map[string]string, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]string{"id": u.ID, "name": u.Name})
	}
	respond(w, http.StatusOK, out, err)
}

func (s *server) listHosts(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	list, err := s.eng.Hosts(r.Context(), p.TenantID)
	respond(w, http.StatusOK, list, err)
}

func (s *server) createHost(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var in engine.HostInput
	if !decode(w, r, &in) {
		return
	}
	h, err := s.eng.CreateHost(r.Context(), p.TenantID, in)
	respond(w, http.StatusCreated, h, err)
}

func (s *server) updateHost(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var in engine.HostInput
	if !decode(w, r, &in) {
		return
	}
	if err := s.hostOf(r.Context(), r.PathValue("id"), p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	h, err := s.eng.UpdateHost(r.Context(), r.PathValue("id"), in)
	respond(w, http.StatusOK, h, err)
}

func (s *server) deleteHost(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if err := s.hostOf(r.Context(), r.PathValue("id"), p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.DeleteHost(r.Context(), r.PathValue("id")))
}

func (s *server) testHost(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if err := s.hostOf(r.Context(), r.PathValue("id"), p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	res, err := s.eng.TestHost(r.Context(), r.PathValue("id"))
	respond(w, http.StatusOK, res, err)
}

func (s *server) resetHostKey(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if err := s.hostOf(r.Context(), r.PathValue("id"), p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	h, err := s.eng.ResetHostKey(r.Context(), r.PathValue("id"))
	respond(w, http.StatusOK, h, err)
}

func (s *server) listDeployments(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	f := store.DeploymentFilter{EnvironmentID: q.Get("environment"), PipelineID: q.Get("pipeline"), Limit: limit}
	list, err := s.eng.Deployments(r.Context(), p.TenantID, f)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	a, err := s.accessFor(r.Context(), p)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	visible := map[string]bool{}
	out := list[:0]
	for _, d := range list {
		ok, seen := visible[d.PipelineID]
		if !seen {
			pl, err := s.eng.Pipeline(r.Context(), d.PipelineID)
			ok = err == nil && a.can(pl, model.PermView)
			visible[d.PipelineID] = ok
		}
		if ok {
			out = append(out, d)
		}
	}
	respond(w, http.StatusOK, out, nil)
}
