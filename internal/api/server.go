// Package api 提供 HTTP API：/api/* 面向用户与前端（会话 Cookie 或个人访问令牌鉴权，按租户隔离、按角色授权），
// /api/runner/* 面向 Runner（Runner 独立 Token 或共享 Token），/api/hooks/* 面向 Git 平台（触发器密钥）。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"flow/internal/auth"
	"flow/internal/engine"
	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/store"
)

const (
	maxPipelineBytes = 1 << 20
	maxLogChunkBytes = 4 << 20
	maxWebhookBytes  = 25 << 20 // 与 GitHub 的载荷上限一致
	maxArtifactBytes = 2 << 30

	defaultRunLimit = 50
	maxRevisionList = 100
	maxRunLimit     = 200

	sessionCookie = "flow_session"
	// csrfHeader 是使用 Cookie 鉴权的写请求必须携带的请求头；跨站页面无法在不经 CORS 预检的情况下设置它。
	csrfHeader = "X-Flow-Request"
)

// Config 是 API 层的可选配置。
type Config struct {
	RunnerToken  string // 共享 Runner Token（可领取所有租户的 Job），空为仅允许注册 Runner
	RunnerDist   string // flow-runner 各平台二进制所在目录（flow-runner-<os>-<arch>），供安装脚本下载
	SecureCookie bool   // 通过 HTTPS 访问时为会话 Cookie 加 Secure
	// Done 关闭时结束所有 SSE 长连接，避免拖慢优雅退出。
	Done <-chan struct{}
}

type server struct {
	eng  *engine.Engine
	auth *auth.Service
	cfg  Config
}

// handler 是已认证用户请求的处理函数。
type handler func(w http.ResponseWriter, r *http.Request, p *auth.Principal)

func NewHandler(eng *engine.Engine, authSvc *auth.Service, cfg Config) http.Handler {
	s := &server{eng: eng, auth: authSvc, cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })

	viewer := func(h handler) http.HandlerFunc { return s.authed(model.RoleViewer, h) }
	dev := func(h handler) http.HandlerFunc { return s.authed(model.RoleDeveloper, h) }
	admin := func(h handler) http.HandlerFunc { return s.authed(model.RoleAdmin, h) }

	s.routeHosts(mux, viewer, dev, admin)
	mux.HandleFunc("GET /api/audit", admin(s.listAudit))
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/logout", viewer(s.logout))
	mux.HandleFunc("GET /api/me", viewer(s.me))
	mux.HandleFunc("POST /api/me/tenant", viewer(s.switchTenant))
	mux.HandleFunc("POST /api/me/password", viewer(s.changePassword))
	mux.HandleFunc("PUT /api/me/profile", viewer(s.updateProfile))
	mux.HandleFunc("GET /api/me/sessions", viewer(s.listSessions))
	mux.HandleFunc("DELETE /api/me/sessions/{id}", viewer(s.revokeSession))
	mux.HandleFunc("DELETE /api/me", viewer(s.deleteAccount))
	mux.HandleFunc("GET /api/me/tokens", viewer(s.listAPITokens))
	mux.HandleFunc("POST /api/me/tokens", viewer(s.createAPIToken))
	mux.HandleFunc("DELETE /api/me/tokens/{id}", viewer(s.deleteAPIToken))
	mux.HandleFunc("POST /api/tenants", viewer(s.createTenant)) // 仅系统管理员，由 auth.Service 校验
	mux.HandleFunc("GET /api/members", viewer(s.listMembers))
	mux.HandleFunc("POST /api/members", admin(s.addMember))
	mux.HandleFunc("PUT /api/members/{id}", admin(s.setMemberRole))
	mux.HandleFunc("DELETE /api/members/{id}", admin(s.removeMember))
	mux.HandleFunc("POST /api/members/{id}/password", admin(s.resetPassword))

	mux.HandleFunc("POST /api/pipelines", dev(s.createPipeline))
	mux.HandleFunc("POST /api/pipelines/validate", dev(s.validatePipeline))
	mux.HandleFunc("GET /api/pipelines", viewer(s.listPipelines))
	mux.HandleFunc("GET /api/pipelines/{id}", viewer(s.getPipeline))
	mux.HandleFunc("PUT /api/pipelines/{id}", dev(s.updatePipeline))
	mux.HandleFunc("DELETE /api/pipelines/{id}", dev(s.deletePipeline))
	mux.HandleFunc("GET /api/pipelines/{id}/revisions", viewer(s.listRevisions))
	mux.HandleFunc("GET /api/pipelines/{id}/stats", viewer(s.pipelineStats))
	mux.HandleFunc("GET /api/pipelines/{id}/deliveries", viewer(s.pipelineDeliveries))
	mux.HandleFunc("GET /api/pipelines/{id}/revisions/{version}", viewer(s.getRevision))
	mux.HandleFunc("POST /api/pipelines/{id}/revisions/{version}/restore", dev(s.restoreRevision))
	mux.HandleFunc("GET /api/pipelines/summary", viewer(s.pipelineSummary))
	mux.HandleFunc("PUT /api/pipelines/{id}/group", dev(s.setPipelineGroup))
	mux.HandleFunc("PUT /api/pipelines/{id}/info", dev(s.setPipelineInfo))
	mux.HandleFunc("GET /api/pipelines/trash", viewer(s.listTrash))
	mux.HandleFunc("POST /api/pipelines/{id}/recover", dev(s.recoverPipeline))
	mux.HandleFunc("GET /api/dashboard", viewer(s.dashboard))
	mux.HandleFunc("GET /api/pipelines/{id}/members", viewer(s.pipelineMembers))
	mux.HandleFunc("PUT /api/pipelines/{id}/members/{user}", dev(s.putPipelineMember))
	mux.HandleFunc("DELETE /api/pipelines/{id}/members/{user}", dev(s.deletePipelineMember))
	mux.HandleFunc("PUT /api/pipelines/{id}/public", dev(s.setPipelinePublic))
	s.routeVars(mux, viewer, dev, admin)
	mux.HandleFunc("GET /api/pipeline-roles", viewer(s.listPipelineRoles))
	mux.HandleFunc("POST /api/pipeline-roles", admin(s.savePipelineRole))
	mux.HandleFunc("PUT /api/pipeline-roles/{id}", admin(s.savePipelineRole))
	mux.HandleFunc("DELETE /api/pipeline-roles/{id}", admin(s.deletePipelineRole))
	mux.HandleFunc("PUT /api/pipelines/{id}/favorite", viewer(s.setFavorite(true)))
	mux.HandleFunc("DELETE /api/pipelines/{id}/favorite", viewer(s.setFavorite(false)))
	mux.HandleFunc("POST /api/pipelines/{id}/runs", dev(s.triggerRun))
	mux.HandleFunc("GET /api/pipelines/{id}/runs", viewer(s.listRuns))
	mux.HandleFunc("GET /api/runs/{id}", viewer(s.getRun))
	mux.HandleFunc("POST /api/runs/{id}/cancel", dev(s.cancelRun))
	mux.HandleFunc("POST /api/runs/{id}/retry", dev(s.retryRun))
	mux.HandleFunc("POST /api/runs/{id}/rollback", dev(s.rollbackRun))
	mux.HandleFunc("POST /api/runs/{id}/redeploy", dev(s.redeployRun))
	mux.HandleFunc("GET /api/runs/{id}/tests", viewer(s.listTestReports))
	mux.HandleFunc("GET /api/runs/{id}/queue", viewer(s.runQueueWait))
	mux.HandleFunc("GET /api/runs/{id}/artifacts", viewer(s.listArtifacts))
	mux.HandleFunc("GET /api/artifacts/{id}/download", viewer(s.downloadArtifact))
	mux.HandleFunc("GET /api/runs/{id}/events", viewer(s.streamRun))
	mux.HandleFunc("GET /api/jobs/{id}/logs", viewer(s.readLog))
	mux.HandleFunc("GET /api/jobs/{id}/logs/stream", viewer(s.streamLog))
	mux.HandleFunc("POST /api/jobs/{id}/approve", dev(s.approve))
	mux.HandleFunc("GET /api/approvals", viewer(s.listApprovals))

	mux.HandleFunc("GET /api/pipelines/{id}/triggers", viewer(s.listTriggers))
	mux.HandleFunc("POST /api/pipelines/{id}/triggers", dev(s.createTrigger))
	mux.HandleFunc("GET /api/triggers/{id}", viewer(s.getTrigger))
	mux.HandleFunc("PUT /api/triggers/{id}", dev(s.updateTrigger))
	mux.HandleFunc("DELETE /api/triggers/{id}", dev(s.deleteTrigger))
	mux.HandleFunc("POST /api/triggers/{id}/rotate-secret", dev(s.rotateTriggerSecret))
	// Git 平台回调，以触发器自身的密钥鉴权。
	mux.HandleFunc("POST /api/hooks/{id}", s.webhook)

	mux.HandleFunc("GET /api/templates", viewer(s.listTemplates))
	mux.HandleFunc("POST /api/templates", dev(s.createTemplate))
	mux.HandleFunc("GET /api/templates/{id}", viewer(s.getTemplate))
	mux.HandleFunc("GET /api/templates/{id}/users", viewer(s.templateUsers))
	mux.HandleFunc("PUT /api/templates/{id}", dev(s.updateTemplate))
	mux.HandleFunc("DELETE /api/templates/{id}", dev(s.deleteTemplate))
	mux.HandleFunc("GET /api/templates/{id}/revisions", viewer(s.listTemplateRevisions))
	mux.HandleFunc("GET /api/templates/{id}/revisions/{version}", viewer(s.getTemplateRevision))
	mux.HandleFunc("POST /api/templates/{id}/revisions/{version}/restore", dev(s.restoreTemplate))

	mux.HandleFunc("GET /api/secrets", viewer(s.listSecrets))
	mux.HandleFunc("PUT /api/secrets/{name}", admin(s.putSecret))
	mux.HandleFunc("DELETE /api/secrets/{name}", admin(s.deleteSecret))
	mux.HandleFunc("GET /api/pipelines/{id}/secrets", viewer(s.listSecrets))
	mux.HandleFunc("PUT /api/pipelines/{id}/secrets/{name}", dev(s.putSecret))
	mux.HandleFunc("DELETE /api/pipelines/{id}/secrets/{name}", dev(s.deleteSecret))

	mux.HandleFunc("GET /api/runners", viewer(s.listRunners))
	mux.HandleFunc("POST /api/runners", admin(s.registerRunner))
	mux.HandleFunc("DELETE /api/runners/{id}", admin(s.deleteRunner))
	s.routeNodes(mux, viewer, admin)

	mux.HandleFunc("POST /api/runner/claim", s.runnerAuth(s.claim))
	mux.HandleFunc("POST /api/runner/jobs/{id}/heartbeat", s.runnerAuth(s.heartbeat))
	mux.HandleFunc("POST /api/runner/jobs/{id}/logs", s.runnerAuth(s.appendLog))
	mux.HandleFunc("POST /api/runner/jobs/{id}/commit", s.runnerAuth(s.reportCommit))
	mux.HandleFunc("POST /api/runner/jobs/{id}/steps", s.runnerAuth(s.reportStep))
	mux.HandleFunc("POST /api/runner/jobs/{id}/hosts", s.runnerAuth(s.reportHosts))
	mux.HandleFunc("POST /api/runner/jobs/{id}/tests", s.runnerAuth(s.uploadTestReport))
	mux.HandleFunc("POST /api/runner/jobs/{id}/complete", s.runnerAuth(s.complete))
	mux.HandleFunc("POST /api/runner/jobs/{id}/artifacts", s.runnerAuth(s.uploadArtifact))
	mux.HandleFunc("GET /api/runner/artifacts/{id}", s.runnerAuth(s.runnerDownloadArtifact))
	return mux
}

// ---- 鉴权 ----

// authed 识别请求主体并校验其在当前租户的角色不低于 min。
// 优先使用 Authorization: Bearer（API 令牌），否则使用会话 Cookie；Cookie 鉴权的写请求须带 csrfHeader。
func (s *server) authed(min model.Role, next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, fromCookie := bearer(r), false
		if tok == "" {
			if c, err := r.Cookie(sessionCookie); err == nil {
				tok, fromCookie = c.Value, true
			}
		}
		if fromCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) == "" {
			writeError(w, http.StatusForbidden, errors.New("missing "+csrfHeader+" header"))
			return
		}
		p, err := s.auth.Authenticate(r.Context(), tok)
		if err != nil {
			respond(w, 0, nil, err)
			return
		}
		if !p.Can(min) {
			writeError(w, http.StatusForbidden, errors.New("requires "+string(min)+" role"))
			return
		}
		if action, ok := auditActions[r.Pattern]; ok {
			s.audited(action, next)(w, r, p)
			return
		}
		next(w, r, p)
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// inTenant 将其他租户的资源视为不存在，避免泄露其 ID 是否有效。
func inTenant(err error, tenantID string, p *auth.Principal) error {
	if err == nil && tenantID != p.TenantID {
		return store.ErrNotFound
	}
	return err
}

// pipelineOf 取出流水线并校验主体在其上的权限 perm（见 access.go）。
func (s *server) pipelineOf(ctx context.Context, id string, p *auth.Principal, perm string) (*model.Pipeline, error) {
	pl, err := s.eng.Pipeline(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.pipelineAccess(ctx, pl, p, perm)
}

func (s *server) runOf(ctx context.Context, id string, p *auth.Principal, perm string) (*model.Run, error) {
	run, err := s.eng.Run(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := inTenant(nil, run.TenantID, p); err != nil {
		return nil, err
	}
	if _, err := s.pipelineOf(ctx, run.PipelineID, p, perm); err != nil {
		return nil, err
	}
	return run, nil
}

func (s *server) jobOf(ctx context.Context, id string, p *auth.Principal, perm string) (*model.Job, error) {
	j, err := s.eng.Job(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := inTenant(nil, j.TenantID, p); err != nil {
		return nil, err
	}
	if _, err := s.runOf(ctx, j.RunID, p, perm); err != nil {
		return nil, err
	}
	return j, nil
}

func (s *server) triggerOf(ctx context.Context, id string, p *auth.Principal, perm string) (*model.Trigger, error) {
	t, err := s.eng.Trigger(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := inTenant(nil, t.TenantID, p); err != nil {
		return nil, err
	}
	if _, err := s.pipelineOf(ctx, t.PipelineID, p, perm); err != nil {
		return nil, err
	}
	return t, nil
}

// ---- 流水线与运行 ----

func (s *server) createPipeline(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPipelineBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	pl, err := s.eng.CreatePipeline(r.Context(), p.TenantID, string(body), p.User.Username)
	s.respondPipeline(w, r, p, http.StatusCreated, pl, err)
}

// validatePipeline 供编辑器实时校验：合法返回 204，否则与创建时相同的 400 错误。
func (s *server) validatePipeline(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPipelineBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.ValidatePipeline(r.Context(), p.TenantID, string(body)))
}

func (s *server) listPipelines(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ps, err := s.visiblePipelines(r.Context(), p, s.eng.Pipelines)
	respond(w, http.StatusOK, ps, err)
}

// visiblePipelines 过滤出主体能看到的流水线，并附上权限。
func (s *server) visiblePipelines(ctx context.Context, p *auth.Principal, list func(context.Context, string) ([]*model.Pipeline, error)) ([]*pipelineView, error) {
	ps, err := list(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	a, err := s.accessFor(ctx, p)
	if err != nil {
		return nil, err
	}
	out := []*pipelineView{}
	for _, pl := range ps {
		if perms := a.perms(pl); slices.Contains(perms, model.PermView) {
			out = append(out, &pipelineView{Pipeline: pl, Permissions: perms})
		}
	}
	return out, nil
}

func (s *server) getPipeline(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	pl, err := s.pipelineOf(r.Context(), r.PathValue("id"), p, model.PermView)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	v, err := s.withPerms(r.Context(), pl, p)
	respond(w, http.StatusOK, v, err)
}

func (s *server) updatePipeline(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPipelineBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	base, ok := baseVersion(w, r)
	if !ok {
		return
	}
	if _, err := s.pipelineOf(r.Context(), r.PathValue("id"), p, model.PermEdit); err != nil {
		respond(w, 0, nil, err)
		return
	}
	pl, err := s.eng.UpdatePipeline(r.Context(), r.PathValue("id"), string(body), p.User.Username, base)
	s.respondPipeline(w, r, p, http.StatusOK, pl, err)
}

// baseVersion 解析可选的 ?base_version=N（编辑起点的版本号）：期间被他人保存过则返回 409，不覆盖。
func baseVersion(w http.ResponseWriter, r *http.Request) (int, bool) {
	v := r.URL.Query().Get("base_version")
	if v == "" {
		return 0, true
	}
	base, err := strconv.Atoi(v)
	if err != nil || base <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid base_version %q", v))
		return 0, false
	}
	return base, true
}

// restoreRevision 回退到历史版本：以其定义保存为新版本并立即生效。
func (s *server) restoreRevision(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
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
	if _, err := s.pipelineOf(r.Context(), id, p, model.PermEdit); err != nil {
		respond(w, 0, nil, err)
		return
	}
	pl, err := s.eng.RestoreRevision(r.Context(), id, version, p.User.Username, base)
	s.respondPipeline(w, r, p, http.StatusOK, pl, err)
}

// PipelineSummary 是列表页的一行：流水线、是否已收藏、最近几次运行。
// 含 YAML：列表中的运行按钮据此判断是否需要填写运行参数。
type PipelineSummary struct {
	*model.Pipeline
	Permissions []string     `json:"permissions"`
	Favorite    bool         `json:"favorite"`
	RecentRuns  []*model.Run `json:"recent_runs"`
}

const maxSummaryRuns = 20

// pipelineSummary 一次返回列表页所需的数据（?runs=N 为每条流水线的最近运行数，默认 10）。
func (s *server) pipelineSummary(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	n, _ := strconv.Atoi(r.URL.Query().Get("runs"))
	if n <= 0 {
		n = 10
	}
	ctx := r.Context()
	ps, err := s.visiblePipelines(ctx, p, s.eng.Pipelines)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	runs, err := s.eng.RecentRuns(ctx, p.TenantID, min(n, maxSummaryRuns))
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	favs, err := s.eng.Favorites(ctx, p.User.ID)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	out := make([]PipelineSummary, len(ps))
	for i, pl := range ps {
		rs := runs[pl.ID]
		if rs == nil {
			rs = []*model.Run{}
		}
		out[i] = PipelineSummary{Pipeline: pl.Pipeline, Permissions: pl.Permissions, Favorite: slices.Contains(favs, pl.ID), RecentRuns: rs}
	}
	respond(w, http.StatusOK, out, nil)
}

func (s *server) setPipelineGroup(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req struct {
		Group string `json:"group"`
	}
	if !decode(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if _, err := s.pipelineOf(r.Context(), id, p, model.PermEdit); err != nil {
		respond(w, 0, nil, err)
		return
	}
	if err := s.eng.SetPipelineGroup(r.Context(), id, req.Group); err != nil {
		respond(w, 0, nil, err)
		return
	}
	pl, err := s.eng.Pipeline(r.Context(), id)
	s.respondPipeline(w, r, p, http.StatusOK, pl, err)
}

// setFavorite 收藏是个人设置，只读成员也可以收藏。
func (s *server) setFavorite(on bool) func(http.ResponseWriter, *http.Request, *auth.Principal) {
	return func(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
		id := r.PathValue("id")
		if _, err := s.pipelineOf(r.Context(), id, p, model.PermView); err != nil {
			respond(w, 0, nil, err)
			return
		}
		respond(w, http.StatusNoContent, nil, s.eng.SetFavorite(r.Context(), p.User.ID, id, on))
	}
}

// pipelineStats 返回最近 ?days=N 天（默认 30，最多 180）的运行统计。
func (s *server) pipelineStats(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	pl, err := s.pipelineOf(r.Context(), r.PathValue("id"), p, model.PermView)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 30
	}
	st, err := s.eng.Stats(r.Context(), pl, min(days, 180))
	respond(w, http.StatusOK, st, err)
}

// pipelineDeliveries 返回流水线的部署历史：每次运行的构建物、网盘 / 对象存储上传与主机部署。
func (s *server) pipelineDeliveries(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	pl, err := s.pipelineOf(r.Context(), r.PathValue("id"), p, model.PermView)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	items, total, err := s.eng.Deliveries(r.Context(), pl, max(offset, 0), limit)
	for _, d := range items {
		s.fillArtifactURLs(r, d.Artifacts)
	}
	respond(w, http.StatusOK, map[string]any{"items": items, "total": total}, err)
}

func (s *server) listRevisions(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	id := r.PathValue("id")
	if _, err := s.pipelineOf(r.Context(), id, p, model.PermView); err != nil {
		respond(w, 0, nil, err)
		return
	}
	revs, err := s.eng.Revisions(r.Context(), id, maxRevisionList)
	respond(w, http.StatusOK, revs, err)
}

func (s *server) getRevision(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	id := r.PathValue("id")
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeError(w, http.StatusNotFound, store.ErrNotFound)
		return
	}
	if _, err := s.pipelineOf(r.Context(), id, p, model.PermView); err != nil {
		respond(w, 0, nil, err)
		return
	}
	rev, err := s.eng.Revision(r.Context(), id, version)
	respond(w, http.StatusOK, rev, err)
}

// deletePipeline 移入回收站；?purge=true 彻底删除回收站中的流水线。
func (s *server) deletePipeline(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	id := r.PathValue("id")
	if _, err := s.pipelineOf(r.Context(), id, p, model.PermDelete); err != nil {
		respond(w, 0, nil, err)
		return
	}
	if r.URL.Query().Get("purge") == "true" {
		respond(w, http.StatusNoContent, nil, s.eng.PurgePipeline(r.Context(), id))
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.TrashPipeline(r.Context(), id))
}

func (s *server) recoverPipeline(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	id := r.PathValue("id")
	if _, err := s.pipelineOf(r.Context(), id, p, model.PermDelete); err != nil {
		respond(w, 0, nil, err)
		return
	}
	if err := s.eng.RestorePipeline(r.Context(), id); err != nil {
		respond(w, 0, nil, err)
		return
	}
	pl, err := s.eng.Pipeline(r.Context(), id)
	s.respondPipeline(w, r, p, http.StatusOK, pl, err)
}

func (s *server) listTrash(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ps, err := s.visiblePipelines(r.Context(), p, s.eng.TrashedPipelines)
	respond(w, http.StatusOK, ps, err)
}

func (s *server) setPipelineInfo(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req struct {
		Description string `json:"description"`
		Archived    bool   `json:"archived"`
	}
	if !decode(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if _, err := s.pipelineOf(r.Context(), id, p, model.PermEdit); err != nil {
		respond(w, 0, nil, err)
		return
	}
	if err := s.eng.SetPipelineInfo(r.Context(), id, req.Description, req.Archived); err != nil {
		respond(w, 0, nil, err)
		return
	}
	pl, err := s.eng.Pipeline(r.Context(), id)
	s.respondPipeline(w, r, p, http.StatusOK, pl, err)
}

// dashboard 返回租户最近 ?days=N 天（7 / 30 / 90，默认 30）的概览。
func (s *server) dashboard(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 30
	}
	a, err := s.accessFor(r.Context(), p)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	d, err := s.eng.Dashboard(r.Context(), p.TenantID, p.User.Username, min(days, 90), func(pl *model.Pipeline) bool { return a.can(pl, model.PermView) })
	respond(w, http.StatusOK, d, err)
}

func (s *server) triggerRun(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req protocol.TriggerRequest
	if !decodeOptional(w, r, &req) {
		return
	}
	if _, err := s.pipelineOf(r.Context(), r.PathValue("id"), p, model.PermRun); err != nil {
		respond(w, 0, nil, err)
		return
	}
	trigger := "manual"
	if p.Token.Kind == auth.KindAPI {
		trigger = "api"
	}
	run, err := s.eng.TriggerRunWithRemark(r.Context(), r.PathValue("id"), trigger, p.User.Username, req.Env, req.Remark)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	s.writeRunDetail(w, r, http.StatusCreated, run.ID)
}

func (s *server) listRuns(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	id := r.PathValue("id")
	if _, err := s.pipelineOf(r.Context(), id, p, model.PermView); err != nil {
		respond(w, 0, nil, err)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = defaultRunLimit
	}
	// 可选筛选：?status=failed&trigger=webhook&before=<编号>（翻页）
	before, _ := strconv.Atoi(q.Get("before"))
	if q.Get("status") != "" || q.Get("trigger") != "" || before > 0 {
		runs, err := s.eng.QueryRuns(r.Context(), id, store.RunFilter{Status: model.Status(q.Get("status")), Trigger: q.Get("trigger"),
			BeforeNumber: before, Limit: min(limit, maxRunLimit)})
		respond(w, http.StatusOK, runs, err)
		return
	}
	runs, err := s.eng.Runs(r.Context(), id, min(limit, maxRunLimit))
	respond(w, http.StatusOK, runs, err)
}

// listApprovals 返回当前用户可以审批的等待中任务（只读成员没有）。
func (s *server) listApprovals(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if !p.Can(model.RoleDeveloper) {
		respond(w, http.StatusOK, []engine.Approval{}, nil)
		return
	}
	list, err := s.eng.PendingApprovals(r.Context(), p.TenantID, p.User.Username, p.Can(model.RoleAdmin))
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	a, err := s.accessFor(r.Context(), p)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	out := []engine.Approval{}
	for _, ap := range list {
		if pl, err := s.eng.Pipeline(r.Context(), ap.Run.PipelineID); err == nil && a.can(pl, model.PermView) {
			out = append(out, ap)
		}
	}
	respond(w, http.StatusOK, out, nil)
}

func (s *server) getRun(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if _, err := s.runOf(r.Context(), r.PathValue("id"), p, model.PermView); err != nil {
		respond(w, 0, nil, err)
		return
	}
	s.writeRunDetail(w, r, http.StatusOK, r.PathValue("id"))
}

func (s *server) cancelRun(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if _, err := s.runOf(r.Context(), r.PathValue("id"), p, model.PermRun); err != nil {
		respond(w, 0, nil, err)
		return
	}
	var run *model.Run
	var err error
	if r.URL.Query().Get("force") == "true" {
		run, err = s.eng.ForceCancelRun(r.Context(), r.PathValue("id"), p.User.Username)
	} else {
		run, err = s.eng.CancelRun(r.Context(), r.PathValue("id"))
	}
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	s.writeRunDetail(w, r, http.StatusOK, run.ID)
}

// runQueueWait 返回运行的排队原因（并发组名额 / 等待构建节点），不在等待时为 null。
func (s *server) runQueueWait(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if _, err := s.runOf(r.Context(), r.PathValue("id"), p, model.PermView); err != nil {
		respond(w, 0, nil, err)
		return
	}
	qw, err := s.eng.QueueWait(r.Context(), r.PathValue("id"))
	respond(w, http.StatusOK, qw, err)
}

func (s *server) retryRun(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if _, err := s.runOf(r.Context(), r.PathValue("id"), p, model.PermRun); err != nil {
		respond(w, 0, nil, err)
		return
	}
	run, err := s.eng.RetryRun(r.Context(), r.PathValue("id"), p.User.Username)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	s.writeRunDetail(w, r, http.StatusOK, run.ID)
}

func (s *server) listTestReports(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if _, err := s.runOf(r.Context(), r.PathValue("id"), p, model.PermView); err != nil {
		respond(w, 0, nil, err)
		return
	}
	reports, err := s.eng.TestReports(r.Context(), r.PathValue("id"))
	respond(w, http.StatusOK, reports, err)
}

// rollbackRun 以一次成功运行当时的配置版本、参数与代码提交重新运行（回滚部署）。
func (s *server) rollbackRun(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if _, err := s.runOf(r.Context(), r.PathValue("id"), p, model.PermRun); err != nil {
		respond(w, 0, nil, err)
		return
	}
	run, err := s.eng.RollbackRun(r.Context(), r.PathValue("id"), p.User.Username)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	s.writeRunDetail(w, r, http.StatusCreated, run.ID)
}

// redeployRun 只重新执行运行中的一个部署任务 {"job": "deploy-prod"}，沿用当时的配置、参数与产物。
func (s *server) redeployRun(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req struct {
		Job string `json:"job"`
	}
	if !decode(w, r, &req) {
		return
	}
	if _, err := s.runOf(r.Context(), r.PathValue("id"), p, model.PermRun); err != nil {
		respond(w, 0, nil, err)
		return
	}
	run, err := s.eng.Redeploy(r.Context(), r.PathValue("id"), req.Job, p.User.Username)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	s.writeRunDetail(w, r, http.StatusCreated, run.ID)
}

func (s *server) writeRunDetail(w http.ResponseWriter, r *http.Request, code int, runID string) {
	run, err := s.eng.Run(r.Context(), runID)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	jobs, err := s.eng.Jobs(r.Context(), runID)
	respond(w, code, protocol.RunDetail{Run: run, Jobs: jobs}, err)
}

// readLog 返回 offset 之后的增量日志，X-Next-Offset 供前端轮询续读。
func (s *server) readLog(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	j, err := s.jobOf(r.Context(), r.PathValue("id"), p, model.PermView)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	data, err := s.eng.ReadLog(r.Context(), r.PathValue("id"), offset)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", url.PathEscape(j.Name+".log")))
	}
	w.Header().Set("X-Next-Offset", strconv.Itoa(max(offset, 0)+len(data)))
	w.Write(data)
}

func (s *server) approve(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req protocol.ApprovalRequest
	if !decode(w, r, &req) {
		return
	}
	if _, err := s.jobOf(r.Context(), r.PathValue("id"), p, model.PermView); err != nil {
		respond(w, 0, nil, err)
		return
	}
	j, err := s.eng.Approve(r.Context(), r.PathValue("id"), p.User.Username, p.Can(model.RoleAdmin), req.Approve, req.Comment)
	respond(w, http.StatusOK, j, err)
}

// ---- 触发器 ----

func (s *server) listTriggers(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if _, err := s.pipelineOf(r.Context(), r.PathValue("id"), p, model.PermView); err != nil {
		respond(w, 0, nil, err)
		return
	}
	ts, err := s.eng.Triggers(r.Context(), r.PathValue("id"))
	for _, t := range ts {
		hideTriggerSecret(t, p)
	}
	respond(w, http.StatusOK, ts, err)
}

// hideTriggerSecret 对只读成员隐藏 Webhook 密钥：持有密钥即可伪造推送事件触发运行。
func hideTriggerSecret(t *model.Trigger, p *auth.Principal) {
	if t != nil && !p.Can(model.RoleDeveloper) {
		t.Secret = ""
	}
}

func (s *server) createTrigger(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req protocol.TriggerSpec
	if !decode(w, r, &req) {
		return
	}
	if _, err := s.pipelineOf(r.Context(), r.PathValue("id"), p, model.PermEdit); err != nil {
		respond(w, 0, nil, err)
		return
	}
	t, err := s.eng.CreateTrigger(r.Context(), r.PathValue("id"), req.Kind, enabledOrDefault(req.Enabled), req.TriggerConfig)
	respond(w, http.StatusCreated, t, err)
}

func (s *server) getTrigger(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	t, err := s.triggerOf(r.Context(), r.PathValue("id"), p, model.PermView)
	hideTriggerSecret(t, p)
	respond(w, http.StatusOK, t, err)
}

func (s *server) updateTrigger(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req protocol.TriggerSpec
	if !decode(w, r, &req) {
		return
	}
	if _, err := s.triggerOf(r.Context(), r.PathValue("id"), p, model.PermEdit); err != nil {
		respond(w, 0, nil, err)
		return
	}
	t, err := s.eng.UpdateTrigger(r.Context(), r.PathValue("id"), enabledOrDefault(req.Enabled), req.TriggerConfig)
	respond(w, http.StatusOK, t, err)
}

func (s *server) deleteTrigger(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if _, err := s.triggerOf(r.Context(), r.PathValue("id"), p, model.PermEdit); err != nil {
		respond(w, 0, nil, err)
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.DeleteTrigger(r.Context(), r.PathValue("id")))
}

func (s *server) rotateTriggerSecret(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if _, err := s.triggerOf(r.Context(), r.PathValue("id"), p, model.PermEdit); err != nil {
		respond(w, 0, nil, err)
		return
	}
	t, err := s.eng.RotateTriggerSecret(r.Context(), r.PathValue("id"))
	respond(w, http.StatusOK, t, err)
}

// webhook 创建 Run 时返回 202；事件被过滤时返回 200 和原因，避免 Git 平台把正常过滤显示为投递失败。
func (s *server) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.eng.HandleWebhook(r.Context(), r.PathValue("id"), r.Header, body)
	code := http.StatusOK // 被忽略或重复投递
	if res != nil && res.Run != nil && !res.Duplicate {
		code = http.StatusAccepted
	}
	respond(w, code, res, err)
}

func enabledOrDefault(b *bool) bool {
	return b == nil || *b
}

// ---- 通用 ----

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

// decodeOptional 允许空请求体。
func decodeOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v)
	if err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func respond(w http.ResponseWriter, code int, v any, err error) {
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, store.ErrConflict), errors.Is(err, engine.ErrLeaseLost):
		code = http.StatusConflict
	case errors.Is(err, engine.ErrInvalid), errors.Is(err, auth.ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, engine.ErrUnauthorized), errors.Is(err, auth.ErrUnauthenticated), errors.Is(err, auth.ErrInvalidCredentials):
		code = http.StatusUnauthorized
	case errors.Is(err, engine.ErrForbidden), errors.Is(err, auth.ErrForbidden):
		code = http.StatusForbidden
	default:
		code = http.StatusInternalServerError
	}
	if err != nil {
		writeError(w, code, err)
		return
	}
	if code == http.StatusNoContent {
		w.WriteHeader(code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// respondPipeline 返回带当前用户权限的流水线。
func (s *server) respondPipeline(w http.ResponseWriter, r *http.Request, p *auth.Principal, status int, pl *model.Pipeline, err error) {
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	v, err := s.withPerms(r.Context(), pl, p)
	respond(w, status, v, err)
}
