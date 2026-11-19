package runner

import (
	"fmt"
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

// 结束 Job 容器时先删容器，Runner 不是 root 再用临时容器把工作目录和上下文目录的属主改回 Runner 用户。
func TestJobContainerStopChownsWorkspace(t *testing.T) {
	if os.Getuid() <= 0 {
		t.Skip("runner is root: no chown needed")
	}
	bin := t.TempDir()
	logf := filepath.Join(bin, "calls")
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho \"$*\" >> "+logf+"\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	c := &jobContainer{name: "flow-j1", image: "golang:1.23", dir: "/w/job-1", ctx: "/w/job-1.flow", out: os.Stderr}
	c.stop()
	b, _ := os.ReadFile(logf)
	want := "rm -f flow-j1\n" + fmt.Sprintf("run --rm -u 0 --entrypoint chown -v /w/job-1:/workspace -v /w/job-1.flow:/flow-ctx golang:1.23 -Rh %d:%d /workspace /flow-ctx\n", os.Getuid(), os.Getgid())
	if string(b) != want {
		t.Errorf("docker calls =\n%s\nwant\n%s", b, want)
	}
}
