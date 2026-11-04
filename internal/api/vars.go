package api

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"strings"

	"flow/internal/auth"
	"flow/internal/engine"
	"flow/internal/model"
	"flow/internal/pipeline"
	"flow/internal/steps"
)

// 通用变量组、服务连接、运行备注与流水线 Webhook。

func (s *server) routeVars(mux *http.ServeMux, viewer, dev, admin func(handler) http.HandlerFunc) {
	mux.HandleFunc("GET /api/var-groups", viewer(s.listVarGroups))
	mux.HandleFunc("POST /api/var-groups", admin(s.saveVarGroup))
	mux.HandleFunc("PUT /api/var-groups/{id}", admin(s.saveVarGroup))
	mux.HandleFunc("DELETE /api/var-groups/{id}", admin(s.deleteVarGroup))

	mux.HandleFunc("GET /api/connection-types", viewer(s.connectionTypes))
	mux.HandleFunc("GET /api/connections", viewer(s.listConnections))
	mux.HandleFunc("POST /api/connections", dev(s.saveConnection))
	mux.HandleFunc("PUT /api/connections/{id}", dev(s.saveConnection))
	mux.HandleFunc("DELETE /api/connections/{id}", dev(s.deleteConnection))
	mux.HandleFunc("POST /api/connections/test", dev(s.testConnection))

	mux.HandleFunc("PUT /api/runs/{id}/remark", dev(s.setRunRemark))
	// 流水线 Webhook：以 URL 中的令牌鉴权，请求体为 {"变量名": "值"}。
	mux.HandleFunc("POST /api/open/flow/webhook/{id}", s.genericWebhook)
	mux.HandleFunc("GET /api/step-kinds", viewer(s.stepKinds))
	mux.HandleFunc("DELETE /api/runs/{id}", dev(s.deleteRun))
	mux.HandleFunc("DELETE /api/pipelines/{id}/runs", dev(s.deleteFinishedRuns))
	mux.HandleFunc("POST /api/pipelines/{id}/copy", dev(s.copyPipeline))
	mux.HandleFunc("GET /api/pipeline-groups", viewer(s.listGroups))
	mux.HandleFunc("POST /api/pipeline-groups", dev(s.createGroup))
	mux.HandleFunc("PUT /api/pipeline-groups/{name}", dev(s.renameGroup))
	mux.HandleFunc("DELETE /api/pipeline-groups/{name}", dev(s.deleteGroup))
	mux.HandleFunc("GET /api/open/approve/{id}", s.approveLink)
	mux.HandleFunc("POST /api/open/approve/{id}", s.approveLink)
}

func (s *server) listVarGroups(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	list, err := s.eng.VarGroups(r.Context(), p.TenantID)
	respond(w, http.StatusOK, nonNil(list), err)
}

func (s *server) saveVarGroup(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req model.VarGroup
	if !decode(w, r, &req) {
		return
	}
	g, err := s.eng.SaveVarGroup(r.Context(), p.TenantID, r.PathValue("id"), p.User.Username, req)
	status := http.StatusOK
	if r.PathValue("id") == "" {
		status = http.StatusCreated
	}
	respond(w, status, g, err)
}

func (s *server) deleteVarGroup(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	respond(w, http.StatusNoContent, nil, s.eng.DeleteVarGroup(r.Context(), p.TenantID, r.PathValue("id")))
}

func (s *server) connectionTypes(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	respond(w, http.StatusOK, engine.ConnTypes, nil)
}

func (s *server) listConnections(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	list, err := s.eng.Connections(r.Context(), p.TenantID, p.User.Username, p.Can(model.RoleAdmin))
	respond(w, http.StatusOK, nonNil(list), err)
}

func (s *server) saveConnection(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req engine.ConnInput
	if !decode(w, r, &req) {
		return
	}
	c, err := s.eng.SaveConnection(r.Context(), p.TenantID, r.PathValue("id"), p.User.Username, p.Can(model.RoleAdmin), req)
	status := http.StatusOK
	if r.PathValue("id") == "" {
		status = http.StatusCreated
	}
	respond(w, status, c, err)
}

func (s *server) deleteConnection(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	respond(w, http.StatusNoContent, nil, s.eng.DeleteConnection(r.Context(), p.TenantID, r.PathValue("id"), p.User.Username, p.Can(model.RoleAdmin)))
}

// testConnection 测试连接：{"id": 已保存的连接（可选）, "type", "values"}，表单中未改动的私密字段沿用已保存的值。
func (s *server) testConnection(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req struct {
		ID string `json:"id"`
		engine.ConnInput
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.eng.TestConnection(r.Context(), p.TenantID, req.ID, p.User.Username, p.Can(model.RoleAdmin), req.ConnInput)
	respond(w, http.StatusNoContent, nil, err)
}

func (s *server) setRunRemark(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req struct {
		Remark string `json:"remark"`
	}
	if !decode(w, r, &req) {
		return
	}
	run, err := s.runOf(r.Context(), r.PathValue("id"), p, model.PermRun)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	if err := s.eng.SetRunRemark(r.Context(), run.ID, req.Remark); err != nil {
		respond(w, 0, nil, err)
		return
	}
	s.writeRunDetail(w, r, http.StatusOK, run.ID)
}

// genericWebhook 处理流水线 Webhook：?token=；请求体为扁平的 {"变量": "值"}（数字、布尔按字面量转成字符串），可为空。
func (s *server) genericWebhook(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	body := map[string]string{}
	if len(strings.TrimSpace(string(data))) > 0 {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		for k, v := range raw {
			var str string
			if json.Unmarshal(v, &str) != nil {
				str = string(v)
			}
			body[k] = str
		}
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		token = bearer(r)
	}
	res, err := s.eng.FireGeneric(r.Context(), r.PathValue("id"), token, body)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"ok": true, "runId": res.RunID, "buildNumber": res.Number, "vars": res.Vars, "ignored": res.Ignored}, nil)
}

// stepKinds 返回内置步骤库（「添加步骤」面板）。
func (s *server) stepKinds(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	respond(w, http.StatusOK, map[string]any{"categories": steps.Categories, "kinds": steps.All(), "plugins": pipeline.PluginTypes}, nil)
}

var approvePage = template.Must(template.New("approve").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}}</title>
<style>body{font:15px/1.6 -apple-system,"PingFang SC","Microsoft YaHei",sans-serif;background:#f7f7f5;color:#1c1917;display:grid;place-items:center;min-height:100vh;margin:0}
.c{background:#fff;border:1px solid #e7e5e4;border-radius:12px;padding:28px;max-width:420px;width:90%}h1{font-size:18px;margin:0 0 8px}p{color:#57534e}
button{border:0;border-radius:8px;padding:10px 18px;font-size:15px;color:#fff;cursor:pointer;background:{{.Color}}}</style></head>
<body><div class="c"><h1>{{.Title}}</h1><p>{{.Body}}</p>{{if .Form}}<form method="post"><button type="submit">{{.Button}}</button></form>{{end}}</div></body></html>`))

// approveLink 处理审批消息中的链接：GET 显示确认页，POST 执行（避免聊天软件预览链接时误触发）。
func (s *server) approveLink(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	action := q.Get("action")
	page := struct {
		Title, Body, Button, Color string
		Form                       bool
	}{Color: "#16a34a", Button: "确认通过"}
	if action == "reject" {
		page.Color, page.Button = "#dc2626", "确认驳回"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		j, err := s.eng.Job(r.Context(), r.PathValue("id"))
		if err != nil || j.Status != model.StatusWaiting {
			page.Title, page.Body = "无需审批", "这个任务已经不在待审批状态。"
		} else {
			page.Title, page.Body, page.Form = "审批「"+j.Name+"」", "确认后流水线将"+map[bool]string{true: "继续执行。", false: "以失败结束。"}[action != "reject"], true
		}
		approvePage.Execute(w, page)
		return
	}
	j, err := s.eng.ApproveByLink(r.Context(), r.PathValue("id"), action, q.Get("sig"))
	switch {
	case errors.Is(err, engine.ErrUnauthorized):
		w.WriteHeader(http.StatusForbidden)
		page.Title, page.Body = "链接无效", "审批链接无效或已过期（任务重试后旧链接失效）。"
	case err != nil:
		page.Title, page.Body = "无法审批", err.Error()
	case j.Status == model.StatusFailed:
		page.Title, page.Body = "已驳回", "「"+j.Name+"」已驳回。"
	default:
		page.Title, page.Body = "已通过", "「"+j.Name+"」已通过，流水线继续执行。"
	}
	approvePage.Execute(w, page)
}

func (s *server) deleteRun(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	run, err := s.runOf(r.Context(), r.PathValue("id"), p, model.PermDeleteRun)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.DeleteRun(r.Context(), run.ID))
}

// deleteFinishedRuns 删除流水线全部已结束的运行记录（运行中的保留），返回 {"deleted": N}。
func (s *server) deleteFinishedRuns(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	pl, err := s.pipelineOf(r.Context(), r.PathValue("id"), p, model.PermDeleteRun)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	n, err := s.eng.DeleteFinishedRuns(r.Context(), pl.ID)
	respond(w, http.StatusOK, map[string]int{"deleted": n}, err)
}

func (s *server) copyPipeline(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeOptional(w, r, &req) {
		return
	}
	src, err := s.pipelineOf(r.Context(), r.PathValue("id"), p, model.PermEdit)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	pl, err := s.eng.CopyPipeline(r.Context(), src, req.Name, p.User.Username)
	s.respondPipeline(w, r, p, http.StatusCreated, pl, err)
}

func (s *server) listGroups(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	gs, err := s.eng.Groups(r.Context(), p.TenantID)
	respond(w, http.StatusOK, nonNil(gs), err)
}

func (s *server) createGroup(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.CreateGroup(r.Context(), p.TenantID, req.Name))
}

func (s *server) renameGroup(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.RenameGroup(r.Context(), p.TenantID, r.PathValue("name"), req.Name))
}

func (s *server) deleteGroup(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	respond(w, http.StatusNoContent, nil, s.eng.DeleteGroup(r.Context(), p.TenantID, r.PathValue("name")))
}
