package api

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"flow/internal/auth"
	"flow/internal/model"
	"flow/internal/protocol"
)

// ---- 密钥：/api/secrets 为租户级，/api/pipelines/{id}/secrets 为流水线级 ----

// secretScope 返回请求对应的流水线 ID（租户级为空），并校验流水线属于当前租户。
func (s *server) secretScope(w http.ResponseWriter, r *http.Request, p *auth.Principal) (string, bool) {
	id := r.PathValue("id")
	if id == "" {
		return "", true
	}
	if _, err := s.pipelineOf(r.Context(), id, p, secretPerm(r)); err != nil {
		respond(w, 0, nil, err)
		return "", false
	}
	return id, true
}

func (s *server) listSecrets(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	scope, ok := s.secretScope(w, r, p)
	if !ok {
		return
	}
	list, err := s.eng.Secrets(r.Context(), p.TenantID, scope)
	respond(w, http.StatusOK, nonNil(list), err)
}

func (s *server) putSecret(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req protocol.SecretRequest
	if !decode(w, r, &req) {
		return
	}
	scope, ok := s.secretScope(w, r, p)
	if !ok {
		return
	}
	sec, err := s.eng.PutSecret(r.Context(), p.TenantID, scope, r.PathValue("name"), req.Value)
	respond(w, http.StatusOK, sec, err)
}

func (s *server) deleteSecret(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	scope, ok := s.secretScope(w, r, p)
	if !ok {
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.DeleteSecret(r.Context(), p.TenantID, scope, r.PathValue("name")))
}

// ---- Runner 管理 ----

// runnerCreated 携带只展示一次的 Runner Token。
type runnerCreated struct {
	*model.Runner
	Token string `json:"token"`
}

func (s *server) listRunners(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	list, err := s.eng.Runners(r.Context(), p.TenantID)
	respond(w, http.StatusOK, nonNil(list), err)
}

func (s *server) deleteRunner(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	rn, err := s.eng.RunnerByID(r.Context(), r.PathValue("id"))
	if err = inTenant(err, tenantOf(rn), p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.DeleteRunner(r.Context(), rn.ID))
}

func tenantOf(r *model.Runner) string {
	if r == nil {
		return ""
	}
	return r.TenantID
}

// ---- 产物 ----

func (s *server) listArtifacts(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if _, err := s.runOf(r.Context(), r.PathValue("id"), p, model.PermView); err != nil {
		respond(w, 0, nil, err)
		return
	}
	list, err := s.eng.Artifacts(r.Context(), r.PathValue("id"))
	respond(w, http.StatusOK, nonNil(list), err)
}

func (s *server) downloadArtifact(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	a, err := s.eng.Artifact(r.Context(), r.PathValue("id"))
	if err == nil {
		err = inTenant(nil, a.TenantID, p)
	}
	if err == nil {
		_, err = s.runOf(r.Context(), a.RunID, p, model.PermView)
	}
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	s.streamArtifact(w, r, a)
}

func (s *server) streamArtifact(w http.ResponseWriter, r *http.Request, a *model.Artifact) {
	rc, err := s.eng.OpenArtifact(r.Context(), a)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	defer rc.Close()
	name, ctype := a.JobName+".tar.gz", "application/gzip"
	if a.Name != "" {
		name = a.Name + "." + a.Format
		if a.Format == "zip" {
			ctype = "application/zip"
		}
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.FormatInt(a.Size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", url.PathEscape(name)))
	io.Copy(w, rc)
}

// secretPerm 是流水线级密钥操作所需的权限：查看列表需要查看权限，增删需要编辑权限。
func secretPerm(r *http.Request) string {
	if r.Method == http.MethodGet {
		return model.PermView
	}
	return model.PermEdit
}
