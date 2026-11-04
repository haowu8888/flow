package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"flow/internal/auth"
	"flow/internal/engine"
	"flow/internal/model"
	"flow/internal/protocol"
)

// 构建节点（自建 / GitHub）、主机 Agent 的管理接口，安装脚本与 flow-runner 下载，以及 Agent 与部署 Runner 的任务接口。
func (s *server) routeNodes(mux *http.ServeMux, viewer, admin func(handler) http.HandlerFunc) {
	mux.HandleFunc("PUT /api/runners/{id}", admin(s.updateNode))
	mux.HandleFunc("POST /api/runners/{id}/token", admin(s.resetNodeToken))
	mux.HandleFunc("GET /api/runners/{id}/workflow", viewer(s.nodeWorkflow))
	mux.HandleFunc("POST /api/hosts/{id}/agent-token", admin(s.hostAgentToken))

	// 无需登录：安装脚本只包含 Server 地址，Token 由用户在命令中传入。
	mux.HandleFunc("GET /api/runner/install.sh", s.installScript)
	mux.HandleFunc("GET /api/runner/download/{os}/{arch}", s.downloadRunner)

	mux.HandleFunc("POST /api/runner/jobs/{id}/host-tasks", s.runnerAuth(s.createHostTask))
	mux.HandleFunc("GET /api/runner/host-tasks/{id}", s.runnerAuth(s.hostTaskProgress))
	mux.HandleFunc("POST /api/runner/agent/claim", s.runnerAuth(s.agentClaim))
	mux.HandleFunc("POST /api/runner/agent/tasks/{id}/log", s.runnerAuth(s.agentLog))
	mux.HandleFunc("POST /api/runner/agent/tasks/{id}/finish", s.runnerAuth(s.agentFinish))
	mux.HandleFunc("GET /api/runner/agent/tasks/{id}/upload", s.runnerAuth(s.agentUpload))
}

// registerRunner 创建构建节点：kind 为空或 self 为自建节点，github 为 GitHub 节点。
func (s *server) registerRunner(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req engine.NodeInput
	if !decode(w, r, &req) {
		return
	}
	rn, token, err := s.eng.CreateNode(r.Context(), p.TenantID, req)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	respond(w, http.StatusCreated, runnerCreated{Runner: rn, Token: token}, nil)
}

func (s *server) nodeOf(r *http.Request, p *auth.Principal) (*model.Runner, error) {
	rn, err := s.eng.RunnerByID(r.Context(), r.PathValue("id"))
	return rn, inTenant(err, tenantOf(rn), p)
}

func (s *server) updateNode(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req engine.NodeInput
	if !decode(w, r, &req) {
		return
	}
	if _, err := s.nodeOf(r, p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	rn, err := s.eng.UpdateNode(r.Context(), r.PathValue("id"), req)
	respond(w, http.StatusOK, rn, err)
}

func (s *server) resetNodeToken(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if _, err := s.nodeOf(r, p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	rn, token, err := s.eng.ResetNodeToken(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	respond(w, http.StatusOK, runnerCreated{Runner: rn, Token: token}, nil)
}

func (s *server) nodeWorkflow(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if _, err := s.nodeOf(r, p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	io.WriteString(w, engine.GitHubWorkflow(s.serverURL(r)))
}

func (s *server) hostAgentToken(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if err := s.hostOf(r.Context(), r.PathValue("id"), p); err != nil {
		respond(w, 0, nil, err)
		return
	}
	token, err := s.eng.HostAgentToken(r.Context(), r.PathValue("id"))
	respond(w, http.StatusOK, map[string]string{"token": token}, err)
}

// serverURL 是 Runner / Agent 连接 Server 使用的地址：优先 FLOW_PUBLIC_URL，否则取本次请求的地址。
func (s *server) serverURL(r *http.Request) string {
	if u := s.eng.PublicURL(); u != "" {
		return strings.TrimRight(u, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}

var platformPart = regexp.MustCompile(`^[a-z0-9]+$`)

// runnerBinary 查找 flow-runner 二进制：FLOW_RUNNER_DIST/flow-runner-<os>-<arch>；
// 与 Server 同平台时也接受 FLOW_RUNNER_DIST/flow-runner 或 Server 同目录下的 flow-runner。
func (s *server) runnerBinary(goos, arch string) string {
	var dirs []string
	if s.cfg.RunnerDist != "" {
		dirs = append(dirs, s.cfg.RunnerDist)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, d := range dirs {
		names := []string{"flow-runner-" + goos + "-" + arch}
		if goos == runtime.GOOS && arch == runtime.GOARCH {
			names = append(names, "flow-runner")
		}
		for _, n := range names {
			p := filepath.Join(d, n)
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				return p
			}
		}
	}
	return ""
}

func (s *server) downloadRunner(w http.ResponseWriter, r *http.Request) {
	goos, arch := r.PathValue("os"), r.PathValue("arch")
	if !platformPart.MatchString(goos) || !platformPart.MatchString(arch) {
		writeError(w, http.StatusBadRequest, errors.New("invalid platform"))
		return
	}
	p := s.runnerBinary(goos, arch)
	if p == "" {
		writeError(w, http.StatusNotFound, errors.New("flow-runner for "+goos+"/"+arch+" is not available on this server (set FLOW_RUNNER_DIST)"))
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="flow-runner"`)
	http.ServeFile(w, r, p)
}

// installScript 返回安装脚本：下载 flow-runner 并注册为 systemd 服务（无 systemd 时后台运行）。
// 用法：curl -fsSL <server>/api/runner/install.sh | FLOW_RUNNER_TOKEN=<token> sh [-s -- agent]
func (s *server) installScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	io.WriteString(w, strings.ReplaceAll(installSh, "@SERVER@", s.serverURL(r)))
}

const installSh = `#!/bin/sh
# Flow 构建节点 / 主机 Agent 安装脚本。
#   构建节点：curl -fsSL @SERVER@/api/runner/install.sh | FLOW_RUNNER_TOKEN=<token> sh
#   主机 Agent：curl -fsSL @SERVER@/api/runner/install.sh | FLOW_RUNNER_TOKEN=<token> sh -s -- agent
# 可选环境变量：FLOW_RUNNER_LABELS、FLOW_CONCURRENCY、FLOW_INSTALL_DIR（默认 /usr/local/bin）、FLOW_WORKDIR。
set -eu
SERVER="${FLOW_SERVER:-@SERVER@}"
MODE="${1:-runner}"
: "${FLOW_RUNNER_TOKEN:?请设置 FLOW_RUNNER_TOKEN}"
case "$(uname -s)" in Linux) OS=linux ;; Darwin) OS=darwin ;; *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64|amd64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;; esac
DIR="${FLOW_INSTALL_DIR:-/usr/local/bin}"
SUDO=""
if [ "$(id -u)" != 0 ] && command -v sudo >/dev/null 2>&1; then SUDO=sudo; fi
if [ ! -w "$DIR" ] && [ -z "$SUDO" ]; then DIR="$HOME/.local/bin"; mkdir -p "$DIR"; fi
echo "==> downloading flow-runner ($OS/$ARCH) to $DIR"
TMP="$(mktemp)"
curl -fsSL "$SERVER/api/runner/download/$OS/$ARCH" -o "$TMP"
chmod +x "$TMP"
$SUDO mv "$TMP" "$DIR/flow-runner"
NAME=flow-runner
ARGS="-server $SERVER"
if [ "$MODE" = agent ]; then NAME=flow-agent; ARGS="$ARGS -agent"; fi
WORKDIR="${FLOW_WORKDIR:-/var/lib/$NAME}"
ENVFILE="/etc/$NAME.env"
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  $SUDO mkdir -p "$WORKDIR"
  printf 'FLOW_RUNNER_TOKEN=%s\nFLOW_RUNNER_LABELS=%s\nFLOW_CONCURRENCY=%s\nFLOW_WORKDIR=%s\n' \
    "$FLOW_RUNNER_TOKEN" "${FLOW_RUNNER_LABELS:-}" "${FLOW_CONCURRENCY:-1}" "$WORKDIR" | $SUDO tee "$ENVFILE" >/dev/null
  $SUDO chmod 600 "$ENVFILE"
  $SUDO tee "/etc/systemd/system/$NAME.service" >/dev/null <<EOF
[Unit]
Description=Flow $NAME
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=$ENVFILE
ExecStart=$DIR/flow-runner $ARGS
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
  $SUDO systemctl daemon-reload
  $SUDO systemctl enable --now "$NAME"
  $SUDO systemctl restart "$NAME"
  echo "==> $NAME installed as a systemd service (journalctl -u $NAME -f)"
else
  WORKDIR="${FLOW_WORKDIR:-$HOME/.$NAME}"
  mkdir -p "$WORKDIR"
  FLOW_RUNNER_TOKEN="$FLOW_RUNNER_TOKEN" FLOW_WORKDIR="$WORKDIR" nohup "$DIR/flow-runner" $ARGS >"$WORKDIR/$NAME.log" 2>&1 &
  echo "==> $NAME started in background (log: $WORKDIR/$NAME.log); no systemd, it will not start on boot"
fi
`

// ---- 部署 Runner ↔ Agent 主机任务 ----

func (s *server) createHostTask(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	id, ok := resolveRunnerID(w, ri, r.URL.Query().Get("runner_id"))
	if !ok {
		return
	}
	var in protocol.HostTaskRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxArtifactBytes)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t, err := s.eng.CreateHostTask(r.Context(), r.PathValue("id"), id, in)
	respond(w, http.StatusCreated, t, err)
}

func (s *server) hostTaskProgress(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	q := r.URL.Query()
	id, ok := resolveRunnerID(w, ri, q.Get("runner_id"))
	if !ok {
		return
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	p, err := s.eng.HostTaskProgressOf(r.Context(), r.PathValue("id"), id, offset, q.Get("cancel") == "1")
	respond(w, http.StatusOK, p, err)
}

func (s *server) agentClaim(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	var req protocol.ClaimRequest
	if !decode(w, r, &req) {
		return
	}
	ri.Info = req.Info
	s.eng.RunnerSeen(r.Context(), ri)
	t, err := s.eng.AgentClaim(r.Context(), ri)
	if err == nil && t == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	respond(w, http.StatusOK, t, err)
}

func (s *server) agentLog(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLogChunkBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.eng.RunnerSeen(r.Context(), ri)
	cancel, err := s.eng.AgentLog(r.Context(), ri, r.PathValue("id"), data)
	respond(w, http.StatusOK, protocol.HeartbeatResponse{Cancel: cancel}, err)
}

func (s *server) agentFinish(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	var req protocol.CompleteRequest
	if !decode(w, r, &req) {
		return
	}
	respond(w, http.StatusNoContent, nil, s.eng.AgentFinish(r.Context(), ri, r.PathValue("id"), req.ExitCode, req.Error))
}

func (s *server) agentUpload(w http.ResponseWriter, r *http.Request, ri engine.RunnerInfo) {
	rc, err := s.eng.AgentUpload(r.Context(), ri, r.PathValue("id"))
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	io.Copy(w, rc)
}
