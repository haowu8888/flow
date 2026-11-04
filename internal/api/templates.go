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

// templateRequest 是创建 / 更新模板的请求体；更新时 kind 被忽略。
type templateRequest struct {
	Kind model.TemplateKind `json:"kind"`
	engine.TemplateInput
}

// templateUser 是引用某个步骤模板的流水线。
type templateUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Versions 为所用版本，0 表示最新版本。
	Versions []int `json:"versions"`
}

// templateOf 返回当前租户可见的模板：本租户的或内置的。
func (s *server) templateOf(ctx context.Context, id string, p *auth.Principal) (*model.Template, error) {
	t, err := s.eng.Template(ctx, id)
	if err != nil || t.Builtin {
		return t, err
	}
	return t, inTenant(nil, t.TenantID, p)
}

func (s *server) listTemplates(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ts, err := s.eng.Templates(r.Context(), p.TenantID, model.TemplateKind(r.URL.Query().Get("kind")))
	respond(w, http.StatusOK, ts, err)
}

func (s *server) getTemplate(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	t, err := s.templateOf(r.Context(), r.PathValue("id"), p)
	respond(w, http.StatusOK, t, err)
}

func (s *server) templateUsers(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	t, err := s.templateOf(r.Context(), r.PathValue("id"), p)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	out := []templateUser{}
	if t.Kind == model.TemplateSteps {
		refs, err := s.eng.TemplateRefs(r.Context(), p.TenantID, t.Name)
		if err != nil {
			respond(w, 0, nil, err)
			return
		}
		for _, u := range refs {
			out = append(out, templateUser{ID: u.Pipeline.ID, Name: u.Pipeline.Name, Versions: u.Versions})
		}
	}
	respond(w, http.StatusOK, out, nil)
}

func (s *server) createTemplate(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req templateRequest
	if !decode(w, r, &req) {
		return
	}
	t, err := s.eng.CreateTemplate(r.Context(), p.TenantID, req.Kind, req.TemplateInput, p.User.Username)
	respond(w, http.StatusCreated, t, err)
}

func (s *server) updateTemplate(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req templateRequest
	if !decode(w, r, &req) {
		return
	}
	base, ok := baseVersion(w, r)
	if !ok {
		return
	}
	if _, err := s.templateOf(r.Context(), r.PathValue("id"), p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	t, err := s.eng.UpdateTemplate(r.Context(), r.PathValue("id"), req.TemplateInput, p.User.Username, base)
	respond(w, http.StatusOK, t, err)
}

func (s *server) listTemplateRevisions(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	id := r.PathValue("id")
	if _, err := s.templateOf(r.Context(), id, p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	revs, err := s.eng.TemplateRevisions(r.Context(), id, maxRevisionList)
	respond(w, http.StatusOK, revs, err)
}

func (s *server) getTemplateRevision(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	id := r.PathValue("id")
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeError(w, http.StatusNotFound, store.ErrNotFound)
		return
	}
	if _, err := s.templateOf(r.Context(), id, p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	rev, err := s.eng.TemplateRevision(r.Context(), id, version)
	respond(w, http.StatusOK, rev, err)
}

// restoreTemplate 回退到历史版本：以其内容保存为新版本。
func (s *server) restoreTemplate(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	id := r.PathValue("id")
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeError(w, http.StatusNotFound, store.ErrNotFound)
		return
	}
	base, ok := baseVersion(w, r)
	if !ok {
		return
	}
	if _, err := s.templateOf(r.Context(), id, p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	t, err := s.eng.RestoreTemplate(r.Context(), id, version, p.User.Username, base)
	respond(w, http.StatusOK, t, err)
}

func (s *server) deleteTemplate(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if _, err := s.templateOf(r.Context(), r.PathValue("id"), p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.DeleteTemplate(r.Context(), r.PathValue("id")))
}
