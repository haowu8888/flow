package api

import (
	"crypto/subtle"
	"errors"
	"io"
	"net/http"

	"flow/internal/engine"
	"flow/internal/protocol"
	"flow/internal/store"
)

// runnerHandler 处理已认证 Runner 的请求；ri.ID 为空表示使用共享 Token 的 Runner。
type runnerHandler func(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo)

// runnerAuth 先匹配共享 Token，再按 Runner 独立 Token 查找注册 Runner。
func (s *server) runnerAuth(next runnerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if s.cfg.RunnerToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.RunnerToken)) == 1 {
			next(w, r, engine.RunnerInfo{})
			return
		}
		ri, err := s.eng.AuthenticateRunner(r.Context(), tok)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusUnauthorized, errors.New("invalid runner token"))
		case err != nil:
			respond(w, 0, nil, err)
		default:
			next(w, r, *ri)
		}
	}
}

// resolveRunnerID 返回 Runner 的标识：注册 Runner 固定为注册 ID，共享 Runner 使用自报的 runner_id。
func resolveRunnerID(w http.ResponseWriter, ri engine.RunnerInfo, claimed string) (string, bool) {
	if ri.ID != "" {
		return ri.ID, true
	}
	if claimed == "" {
		writeError(w, http.StatusBadRequest, errors.New("runner_id is required"))
		return "", false
	}
	return claimed, true
}

func (s *server) claim(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	var req protocol.ClaimRequest
	if !decode(w, r, &req) {
		return
	}
	id, ok := resolveRunnerID(w, ri, req.RunnerID)
	if !ok {
		return
	}
	ri.ID, ri.Labels, ri.Info = id, req.Labels, req.Info
	s.eng.RunnerSeen(r.Context(), ri)
	a, err := s.eng.Claim(r.Context(), ri)
	if err == nil && a == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	resp := protocol.ClaimResponse{Job: a.Job, Secrets: a.Secrets, Hosts: a.Hosts, Env: a.Env, DeployError: a.DeployError,
		Connections: a.Connections, Masks: a.Masks}
	for _, art := range a.Artifacts {
		resp.Artifacts = append(resp.Artifacts, protocol.ArtifactRef{ID: art.ID, JobName: art.JobName, Size: art.Size})
	}
	for _, art := range a.Named {
		resp.Named = append(resp.Named, protocol.ArtifactRef{ID: art.ID, JobName: art.JobName, Size: art.Size, Name: art.Name, Version: art.Version, Format: art.Format})
	}
	respond(w, http.StatusOK, resp, nil)
}

func (s *server) heartbeat(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	var req protocol.HeartbeatRequest
	if !decode(w, r, &req) {
		return
	}
	id, ok := resolveRunnerID(w, ri, req.RunnerID)
	if !ok {
		return
	}
	cancel, err := s.eng.Heartbeat(r.Context(), r.PathValue("id"), id)
	respond(w, http.StatusOK, protocol.HeartbeatResponse{Cancel: cancel}, err)
}

func (s *server) appendLog(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	id, ok := resolveRunnerID(w, ri, r.URL.Query().Get("runner_id"))
	if !ok {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLogChunkBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.AppendLog(r.Context(), r.PathValue("id"), id, data))
}

func (s *server) complete(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	var req protocol.CompleteRequest
	if !decode(w, r, &req) {
		return
	}
	id, ok := resolveRunnerID(w, ri, req.RunnerID)
	if !ok {
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.CompleteWithOutputs(r.Context(), r.PathValue("id"), id, req.Status, req.ExitCode, req.Error, req.Outputs))
}

// reportStep 记录步骤开始 / 结束。
func (s *server) reportStep(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	var req protocol.StepRequest
	if !decode(w, r, &req) {
		return
	}
	id, ok := resolveRunnerID(w, ri, req.RunnerID)
	if !ok {
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.ReportStep(r.Context(), r.PathValue("id"), id, req.Index, req.Status, req.ExitCode, req.Outputs))
}

// uploadTestReport 保存 Job 的测试报告。
func (s *server) uploadTestReport(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	var req protocol.TestReportRequest
	if !decode(w, r, &req) {
		return
	}
	id, ok := resolveRunnerID(w, ri, req.RunnerID)
	if !ok {
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.SaveTestReport(r.Context(), r.PathValue("id"), id, req.TestSummary, req.Cases))
}

// reportHosts 记录部署任务各主机的进度。
func (s *server) reportHosts(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	var req protocol.HostsRequest
	if !decode(w, r, &req) {
		return
	}
	id, ok := resolveRunnerID(w, ri, req.RunnerID)
	if !ok {
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.ReportHosts(r.Context(), r.PathValue("id"), id, req.Hosts))
}

// reportCommit 记录 Job 检出的代码提交。
func (s *server) reportCommit(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	var req protocol.CommitRequest
	if !decode(w, r, &req) {
		return
	}
	id, ok := resolveRunnerID(w, ri, req.RunnerID)
	if !ok {
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.RecordCommit(r.Context(), r.PathValue("id"), id, req.Commit))
}

func (s *server) uploadArtifact(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	id, ok := resolveRunnerID(w, ri, r.URL.Query().Get("runner_id"))
	if !ok {
		return
	}
	q := r.URL.Query()
	meta := engine.ArtifactMeta{Name: q.Get("name"), Version: q.Get("version"), Format: q.Get("format"),
		Connection: q.Get("connection"), Bucket: q.Get("bucket"), ExpireDays: q.Get("expire_days")}
	a, err := s.eng.UploadNamedArtifact(r.Context(), r.PathValue("id"), id, meta, http.MaxBytesReader(w, r.Body, maxArtifactBytes))
	if err == nil {
		a.URL = s.serverURL(r) + s.eng.ArtifactLinkPath(a)
	}
	respond(w, http.StatusCreated, a, err)
}

// runnerDownloadArtifact 供下游 Job 拉取上游产物；注册 Runner 只能访问所属租户的产物。
func (s *server) runnerDownloadArtifact(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	a, err := s.eng.Artifact(r.Context(), r.PathValue("id"))
	if err == nil && ri.TenantID != "" && a.TenantID != ri.TenantID {
		err = store.ErrNotFound
	}
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	s.streamArtifact(w, r, a)
}
