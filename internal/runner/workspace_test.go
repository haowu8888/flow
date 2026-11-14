package runner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flow/internal/model"
)

// gitRepo 在 dir 建一个只有一个提交的仓库并克隆成裸仓库 bare，返回提交号。
func gitRepo(t *testing.T, dir, bare, content string) string {
	t.Helper()
	g := func(d string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	os.MkdirAll(dir, 0o755)
	g(dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "README"), []byte(content), 0o644)
	g(dir, "add", ".")
	g(dir, "commit", "-qm", content)
	os.MkdirAll(filepath.Dir(bare), 0o755)
	g(dir, "clone", "-q", "--bare", dir, bare)
	g(bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	return g(dir, "rev-parse", "HEAD")
}

// 加速地址是「前缀 + 仓库地址」：返回目标提交就用它，返回别的提交（被篡改或缓存落后）就换下一个，最后直连。
func TestCheckoutMirrors(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	want := gitRepo(t, filepath.Join(root, "src"), origin, "real")
	good, bad := filepath.Join(root, "good"), filepath.Join(root, "bad")
	os.MkdirAll(filepath.Dir(good+origin), 0o755)
	if b, err := exec.Command("git", "clone", "-q", "--bare", origin, good+origin).CombinedOutput(); err != nil {
		t.Fatalf("clone mirror: %v\n%s", err, b)
	}
	gitRepo(t, filepath.Join(root, "src-bad"), bad+origin, "tampered")

	for _, tc := range []struct {
		name    string
		mirrors []string
		via     bool // 是否应从加速地址取得代码（不再直连下载）
	}{
		{"good mirror", []string{good}, true},
		{"tampered mirror falls back", []string{bad}, false},
		{"tampered then good", []string{bad, good, filepath.Join(root, "missing")}, true},
		{"unreachable mirror falls back", []string{filepath.Join(root, "missing")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var out bytes.Buffer
			co := &model.Checkout{URL: origin, Ref: "main", Depth: 1, Mirrors: tc.mirrors}
			head, err := checkout(context.Background(), dir, co, nil, &out)
			if err != nil {
				t.Fatalf("checkout: %v\n%s", err, out.String())
			}
			if head != want {
				t.Fatalf("HEAD = %s, want %s\n%s", head, want, out.String())
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "README")); string(b) != "real" {
				t.Fatalf("README = %q", b)
			}
			if got := !strings.Contains(out.String(), "From "+strings.TrimSuffix(origin, ".git")+"\n"); got != tc.via {
				t.Errorf("fetched via mirror = %v, want %v\n%s", got, tc.via, out.String())
			}
		})
	}

	// 配置了访问令牌时不经过加速地址，免得令牌发给代理
	var out bytes.Buffer
	co := &model.Checkout{URL: origin, Ref: "main", Depth: 1, Mirrors: []string{good}, TokenSecret: "T"}
	if _, err := checkout(context.Background(), t.TempDir(), co, map[string]string{"T": "x"}, &out); err != nil {
		t.Fatalf("checkout with token: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "fetch via") || !strings.Contains(out.String(), "mirrors are not used") {
		t.Errorf("token checkout used mirrors:\n%s", out.String())
	}
}

func TestCheckoutRetries(t *testing.T) {
	old := fetchBackoff
	fetchBackoff = []time.Duration{time.Millisecond}
	defer func() { fetchBackoff = old }()

	var out bytes.Buffer
	co := &model.Checkout{URL: filepath.Join(t.TempDir(), "none.git"), Ref: "main", Depth: 1}
	_, err := checkout(context.Background(), t.TempDir(), co, nil, &out)
	if err == nil {
		t.Fatal("checkout of a missing repo succeeded")
	}
	if !strings.Contains(err.Error(), "git fetch:") {
		t.Errorf("error = %v, want it to name the git command", err)
	}
	if n := strings.Count(out.String(), "fetch failed, retrying"); n != fetchAttempts-1 {
		t.Errorf("retries = %d, want %d\n%s", n, fetchAttempts-1, out.String())
	}
}

// 步骤清空工作目录（git clean -fdx）后，后续步骤仍能写 $FLOW_OUTPUT。
func TestContextSurvivesWorkspaceClean(t *testing.T) {
	dir := t.TempDir()
	sc := &stepContext{dir: dir, ctx: dir + ctxSuffix, tools: t.TempDir(), env: map[string]string{}, outputs: map[string]string{}}
	defer sc.remove()
	v := sc.vars(false)
	if strings.HasPrefix(v["FLOW_OUTPUT"], dir+string(filepath.Separator)) {
		t.Fatalf("FLOW_OUTPUT %s is inside the workspace", v["FLOW_OUTPUT"])
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
	if err := os.WriteFile(v["FLOW_OUTPUT"], []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sc.collect(os.Stderr)
	if sc.outputs["A"] != "1" {
		t.Errorf("outputs = %v", sc.outputs)
	}
}
