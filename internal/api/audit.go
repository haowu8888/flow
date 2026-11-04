package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"flow/internal/auth"
	"flow/internal/model"
	"flow/internal/store"
)

// auditActions 把需要审计的写接口（按路由模式）映射为动作名；动作名前缀即对象类型。
var auditActions = map[string]string{
	"POST /api/members":                                    "member.add",
	"PUT /api/members/{id}":                                "member.role",
	"DELETE /api/members/{id}":                             "member.remove",
	"POST /api/members/{id}/password":                      "member.reset_password",
	"POST /api/me/tokens":                                  "token.create",
	"DELETE /api/me/tokens/{id}":                           "token.delete",
	"POST /api/me/password":                                "account.change_password",
	"DELETE /api/me/sessions/{id}":                         "account.revoke_session",
	"DELETE /api/me":                                       "account.delete",
	"POST /api/pipelines":                                  "pipeline.create",
	"PUT /api/pipelines/{id}":                              "pipeline.update",
	"DELETE /api/pipelines/{id}":                           "pipeline.delete",
	"PUT /api/pipelines/{id}/group":                        "pipeline.group",
	"PUT /api/pipelines/{id}/info":                         "pipeline.info",
	"PUT /api/pipelines/{id}/members/{user}":               "pipeline.member_set",
	"DELETE /api/pipelines/{id}/members/{user}":            "pipeline.member_remove",
	"PUT /api/pipelines/{id}/public":                       "pipeline.public",
	"POST /api/pipeline-roles":                             "role.create",
	"POST /api/var-groups":                                 "vargroup.create",
	"PUT /api/var-groups/{id}":                             "vargroup.update",
	"DELETE /api/var-groups/{id}":                          "vargroup.delete",
	"POST /api/connections":                                "connection.create",
	"PUT /api/connections/{id}":                            "connection.update",
	"DELETE /api/connections/{id}":                         "connection.delete",
	"PUT /api/runs/{id}/remark":                            "run.remark",
	"DELETE /api/runs/{id}":                                "run.delete",
	"DELETE /api/pipelines/{id}/runs":                      "run.delete_all",
	"POST /api/pipelines/{id}/copy":                        "pipeline.copy",
	"POST /api/pipeline-groups":                            "group.create",
	"PUT /api/pipeline-groups/{name}":                      "group.rename",
	"DELETE /api/pipeline-groups/{name}":                   "group.delete",
	"PUT /api/pipeline-roles/{id}":                         "role.update",
	"DELETE /api/pipeline-roles/{id}":                      "role.delete",
	"POST /api/pipelines/{id}/recover":                     "pipeline.recover",
	"POST /api/pipelines/{id}/runs":                        "run.trigger",
	"POST /api/runs/{id}/cancel":                           "run.cancel",
	"POST /api/runs/{id}/retry":                            "run.retry",
	"POST /api/runs/{id}/rollback":                         "run.rollback",
	"POST /api/runs/{id}/redeploy":                         "run.redeploy",
	"POST /api/jobs/{id}/approve":                          "job.approve",
	"POST /api/pipelines/{id}/triggers":                    "trigger.create",
	"PUT /api/triggers/{id}":                               "trigger.update",
	"DELETE /api/triggers/{id}":                            "trigger.delete",
	"POST /api/triggers/{id}/rotate-secret":                "trigger.rotate_secret",
	"POST /api/pipelines/{id}/revisions/{version}/restore": "pipeline.restore",
	"POST /api/templates":                                  "template.create",
	"PUT /api/templates/{id}":                              "template.update",
	"DELETE /api/templates/{id}":                           "template.delete",
	"POST /api/templates/{id}/revisions/{version}/restore": "template.restore",
	"PUT /api/secrets/{name}":                              "secret.put",
	"DELETE /api/secrets/{name}":                           "secret.delete",
	"PUT /api/pipelines/{id}/secrets/{name}":               "secret.put",
	"DELETE /api/pipelines/{id}/secrets/{name}":            "secret.delete",
	"POST /api/runners":                                    "runner.register",
	"DELETE /api/runners/{id}":                             "runner.delete",
	"PUT /api/runners/{id}":                                "runner.update",
	"POST /api/runners/{id}/token":                         "runner.token",
	"POST /api/hosts/{id}/agent-token":                     "host.agent_token",
	"POST /api/environments":                               "environment.create",
	"PUT /api/environments/{id}":                           "environment.update",
	"DELETE /api/environments/{id}":                        "environment.delete",
	"POST /api/host-groups":                                "host_group.create",
	"PUT /api/host-groups/{id}":                            "host_group.update",
	"DELETE /api/host-groups/{id}":                         "host_group.delete",
	"POST /api/hosts":                                      "host.create",
	"PUT /api/hosts/{id}":                                  "host.update",
	"DELETE /api/hosts/{id}":                               "host.delete",
	"POST /api/hosts/{id}/test":                            "host.test",
	"POST /api/hosts/{id}/reset-key":                       "host.reset_key",
	"POST /api/tenants":                                    "tenant.create",
}

// auditRecorder 记录响应状态与响应体开头（用于取对象 ID / 名称），同时照常写给客户端。
type auditRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

const maxAuditBody = 64 << 10

func (a *auditRecorder) WriteHeader(code int) {
	a.status = code
	a.ResponseWriter.WriteHeader(code)
}

func (a *auditRecorder) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	if room := maxAuditBody - a.body.Len(); room > 0 {
		a.body.Write(b[:min(len(b), room)])
	}
	return a.ResponseWriter.Write(b)
}

// audited 在写操作成功后记录审计事件；删除前先查出对象名称。
func (s *server) audited(action string, next handler) handler {
	return func(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
		ctx := r.Context()
		ev := &model.AuditEvent{TenantID: p.TenantID, Actor: p.User.Username, Action: action, IP: clientIP(r)}
		ev.TargetType, _, _ = strings.Cut(action, ".")
		ev.TargetID = r.PathValue("id")
		if r.Method == http.MethodDelete || strings.HasSuffix(action, ".restore") {
			ev.TargetName = s.auditName(ctx, ev.TargetType, ev.TargetID)
		}
		rec := &auditRecorder{ResponseWriter: w}
		next(rec, r, p)
		if rec.status == 0 || rec.status >= 400 {
			return
		}
		s.fillAudit(ctx, ev, r, rec.body.Bytes())
		s.eng.RecordAudit(context.WithoutCancel(ctx), ev)
	}
}

// fillAudit 从路径与响应体补充对象信息。
func (s *server) fillAudit(ctx context.Context, ev *model.AuditEvent, r *http.Request, body []byte) {
	var resp struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Username   string `json:"username"`
		Version    int    `json:"version"`
		Status     string `json:"status"`
		PipelineID string `json:"pipeline_id"`
		Run        *struct {
			ID         string `json:"id"`
			Number     int    `json:"number"`
			PipelineID string `json:"pipeline_id"`
		} `json:"run"`
	}
	_ = json.Unmarshal(body, &resp)
	// 新建的对象以响应中的 ID 为准（路径中的 id 可能是上级对象，如新建触发器时的流水线）。
	if resp.ID != "" && (ev.TargetID == "" || strings.HasSuffix(ev.Action, ".create")) {
		ev.TargetID = resp.ID
	}
	ev.TargetName = first(ev.TargetName, resp.Name, resp.Username)
	switch ev.TargetType {
	case "secret":
		ev.TargetType, ev.TargetID, ev.TargetName = "secret", r.PathValue("name"), r.PathValue("name")
		if pid := r.PathValue("id"); pid != "" {
			ev.Detail = "流水线 " + s.auditName(ctx, "pipeline", pid)
		} else {
			ev.Detail = "租户级"
		}
	case "run":
		if resp.Run != nil {
			ev.TargetID, ev.TargetName = resp.Run.ID, "#"+strconv.Itoa(resp.Run.Number)
			ev.Detail = "流水线 " + s.auditName(ctx, "pipeline", resp.Run.PipelineID)
		}
	case "job":
		if resp.Status == string(model.StatusFailed) {
			ev.Action = "job.reject"
		}
		if j, err := s.eng.Job(ctx, ev.TargetID); err == nil {
			if run, err := s.eng.Run(ctx, j.RunID); err == nil {
				ev.Detail = "流水线 " + s.auditName(ctx, "pipeline", run.PipelineID) + " #" + strconv.Itoa(run.Number)
			}
		}
	case "pipeline", "template":
		if resp.Version > 0 && !strings.HasSuffix(ev.Action, ".delete") {
			ev.Detail = "版本 v" + strconv.Itoa(resp.Version)
		}
		if ev.Action == "pipeline.recover" {
			ev.Detail = "从回收站恢复"
		}
		if ev.Action == "pipeline.delete" {
			ev.Detail = "移入回收站"
			if r.URL.Query().Get("purge") == "true" {
				ev.Detail = "彻底删除"
			}
		}
		if strings.HasSuffix(ev.Action, ".restore") {
			ev.Detail = "回退到 v" + r.PathValue("version") + "，生成 v" + strconv.Itoa(resp.Version)
		}
	case "trigger":
		if t, err := s.eng.Trigger(ctx, ev.TargetID); err == nil {
			ev.TargetName = string(t.Kind)
			ev.Detail = "流水线 " + s.auditName(ctx, "pipeline", t.PipelineID)
		} else if resp.PipelineID != "" {
			ev.Detail = "流水线 " + s.auditName(ctx, "pipeline", resp.PipelineID)
		}
	}
}

// auditName 查找对象名称（用于删除前、以及只有 ID 的对象），找不到返回空。
func (s *server) auditName(ctx context.Context, typ, id string) string {
	if id == "" {
		return ""
	}
	switch typ {
	case "pipeline":
		if p, err := s.eng.Pipeline(ctx, id); err == nil {
			return p.Name
		}
	case "host":
		if h, err := s.eng.Host(ctx, id); err == nil {
			return h.Name
		}
	case "host_group":
		if g, err := s.eng.HostGroup(ctx, id); err == nil {
			return g.Name
		}
	case "environment":
		if e, err := s.eng.Environment(ctx, id); err == nil {
			return e.Name
		}
	case "template":
		if t, err := s.eng.Template(ctx, id); err == nil {
			return t.Name
		}
	case "trigger":
		if t, err := s.eng.Trigger(ctx, id); err == nil {
			return string(t.Kind)
		}
	}
	return ""
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// clientIP 取连接地址；部署在反向代理后时这是代理地址。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// listAudit 查询审计日志（管理员）：?actor=&category=&target=&before=RFC3339&limit=
func (s *server) listAudit(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	q := r.URL.Query()
	f := store.AuditFilter{Actor: q.Get("actor"), Category: q.Get("category"), TargetID: q.Get("target")}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	if v := q.Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		f.Before = t
	}
	list, err := s.eng.AuditEvents(r.Context(), p.TenantID, f)
	respond(w, http.StatusOK, list, err)
}
