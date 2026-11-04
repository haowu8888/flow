package runner

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"flow/internal/model"
)

// 步骤之间的上下文：步骤可以写入
//
//	$FLOW_ENV     KEY=VALUE（每行一个）→ 后续步骤的环境变量
//	$FLOW_PATH    目录（每行一个）       → 后续步骤 PATH 的前缀
//	$FLOW_OUTPUT  KEY=VALUE              → 任务输出：后续步骤与下游任务的环境变量，运行条件也可引用
//
// $FLOW_TOOLS 是 Runner 上跨任务复用的工具目录（安装 Node / Go / Java 等的下载缓存），
// $FLOW_WORKSPACE 是代码目录。容器中运行时这些路径映射为容器内的路径。

const (
	ctxDir           = ".flow"
	containerWS      = "/workspace"
	containerTools   = "/flow-tools"
	maxOutputs       = 100
	maxOutputBytes   = 64 << 10
	defaultUnixPaths = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

var envLine = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// stepContext 记录一个任务内步骤间传递的状态。
type stepContext struct {
	dir     string // 工作目录（宿主机路径）
	tools   string // 工具目录（宿主机路径）
	env     map[string]string
	paths   []string
	outputs map[string]string
	mounts  []string // docker -v 参数：缓存目录等
}

func (r *Runner) newStepContext(job *model.Job, dir string, out io.Writer) *stepContext {
	sc := &stepContext{dir: dir, tools: filepath.Join(r.cfg.CacheDir, "tools"), env: map[string]string{}, outputs: map[string]string{}}
	_ = os.MkdirAll(filepath.Join(dir, ctxDir), 0o755)
	_ = os.MkdirAll(sc.tools, 0o755)
	for _, p := range job.CacheDirs {
		if !filepath.IsAbs(p) || strings.Contains(p, "..") {
			fmt.Fprintf(out, "==> warning: cache dir %q must be an absolute path, ignored\n", p)
			continue
		}
		sum := sha256.Sum256([]byte(p))
		host := filepath.Join(r.cfg.CacheDir, "pipelines", job.PipelineID, hex.EncodeToString(sum[:8]))
		if err := os.MkdirAll(host, 0o755); err != nil {
			fmt.Fprintf(out, "==> warning: cache dir %s: %v\n", p, err)
			continue
		}
		sc.mounts = append(sc.mounts, host+":"+p)
	}
	if len(sc.mounts) > 0 && job.Image == "" {
		fmt.Fprintln(out, "==> note: cache dirs are mounted only when the job runs in a container (image)")
	}
	return sc
}

// vars 返回本步骤额外的环境变量（容器中运行时为容器内路径），并清空上下文文件。
func (sc *stepContext) vars(inContainer bool) map[string]string {
	for _, f := range []string{"env", "path", "output"} {
		_ = os.WriteFile(filepath.Join(sc.dir, ctxDir, f), nil, 0o644)
	}
	ws, tools := sc.dir, sc.tools
	if inContainer {
		ws, tools = containerWS, containerTools
	}
	v := map[string]string{
		"FLOW_WORKSPACE": ws,
		"FLOW_TOOLS":     tools,
		"FLOW_ENV":       ws + "/" + ctxDir + "/env",
		"FLOW_PATH":      ws + "/" + ctxDir + "/path",
		"FLOW_OUTPUT":    ws + "/" + ctxDir + "/output",
	}
	for k, x := range sc.env {
		v[k] = x
	}
	if len(sc.paths) > 0 {
		base := defaultUnixPaths
		if !inContainer {
			base = os.Getenv("PATH")
		}
		mapped := make([]string, len(sc.paths))
		for i, p := range sc.paths {
			mapped[i] = sc.mapPath(p, inContainer)
		}
		v["PATH"] = strings.Join(mapped, ":") + ":" + base
	}
	return v
}

// mapPath 在宿主机与容器路径之间转换工具目录与工作目录下的路径（步骤写入的是自己看到的路径）。
func (sc *stepContext) mapPath(p string, inContainer bool) string {
	pairs := [][2]string{{sc.dir, containerWS}, {sc.tools, containerTools}}
	for _, pr := range pairs {
		from, to := pr[0], pr[1]
		if !inContainer {
			from, to = to, from
		}
		if p == from || strings.HasPrefix(p, from+"/") {
			return to + strings.TrimPrefix(p, from)
		}
	}
	return p
}

// collect 读取步骤写入的上下文文件，更新后续步骤的环境与任务输出。
func (sc *stepContext) collect(out io.Writer) {
	readLines(filepath.Join(sc.dir, ctxDir, "env"), func(k, v string) { sc.env[k] = v })
	readLines(filepath.Join(sc.dir, ctxDir, "output"), func(k, v string) {
		if len(sc.outputs) >= maxOutputs && sc.outputs[k] == "" {
			fmt.Fprintf(out, "==> warning: too many outputs, %s ignored\n", k)
			return
		}
		sc.outputs[k], sc.env[k] = v, v
	})
	if f, err := os.Open(filepath.Join(sc.dir, ctxDir, "path")); err == nil {
		s := bufio.NewScanner(f)
		for s.Scan() {
			if p := strings.TrimSpace(s.Text()); p != "" {
				// 步骤写的是它所在环境（宿主机或容器）中的路径，统一记为宿主机路径
				sc.paths = append([]string{sc.mapPath(p, false)}, sc.paths...)
			}
		}
		f.Close()
	}
	size := 0
	for k, v := range sc.outputs {
		size += len(k) + len(v)
	}
	if size > maxOutputBytes {
		fmt.Fprintf(out, "==> warning: outputs exceed %d KB and are not reported\n", maxOutputBytes>>10)
		sc.outputs = map[string]string{}
	}
}

func readLines(path string, fn func(k, v string)) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if m := envLine.FindStringSubmatch(line); m != nil && !strings.HasPrefix(m[1], "FLOW_RUNNER") {
			fn(m[1], m[2])
		}
	}
}

// mountArgs 返回 docker run 需要的额外挂载。
func (sc *stepContext) mountArgs() []string {
	args := []string{"-v", sc.tools + ":" + containerTools}
	for _, m := range sc.mounts {
		args = append(args, "-v", m)
	}
	return args
}
