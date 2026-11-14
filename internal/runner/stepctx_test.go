package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// $FLOW_PATH 写的目录加在 PATH 前面：容器里接在镜像自己的 PATH 后面（不能丢掉 golang 镜像的 /usr/local/go/bin），本机接在 Runner 的 PATH 后面。
func TestFlowPathKeepsImagePath(t *testing.T) {
	dir, tools := t.TempDir(), t.TempDir()
	sc := &stepContext{dir: dir, ctx: dir + ctxSuffix, tools: tools, env: map[string]string{}, outputs: map[string]string{}}
	defer sc.remove()
	sc.vars(true)
	os.WriteFile(filepath.Join(sc.ctx, "path"), []byte("/flow-tools/bun\n/workspace/bin\n"), 0o644)
	sc.collect(os.Stderr)

	if got := sc.vars(true)["PATH"]; got != "/workspace/bin:/flow-tools/bun:"+defaultUnixPaths {
		t.Errorf("no container: PATH = %q", got)
	}
	sc.container = &jobContainer{path: "/go/bin:/usr/local/go/bin:/usr/bin:/bin"}
	if got := sc.vars(true)["PATH"]; got != "/workspace/bin:/flow-tools/bun:/go/bin:/usr/local/go/bin:/usr/bin:/bin" {
		t.Errorf("container: PATH = %q", got)
	}
	want := filepath.Join(dir, "bin") + ":" + filepath.Join(tools, "bun") + ":" + os.Getenv("PATH")
	if got := sc.vars(false)["PATH"]; got != want {
		t.Errorf("host: PATH = %q, want %q", got, want)
	}
}
