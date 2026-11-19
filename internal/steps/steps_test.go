package steps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestCatalog(t *testing.T) {
	all := All()
	if len(all) < 40 {
		t.Fatalf("only %d kinds", len(all))
	}
	cats := map[string]bool{}
	for _, c := range Categories {
		cats[c.Value] = true
	}
	for _, k := range all {
		if !cats[k.Category] || k.Label == "" {
			t.Errorf("%s: bad category or label", k.Kind)
		}
		if !k.Native && k.script == "" {
			t.Errorf("%s: no script", k.Kind)
		}
	}
	if _, _, err := Get("run-command").Expand(map[string]string{}); err == nil {
		t.Error("missing required input accepted")
	}
	if _, _, err := Get("run-command").Expand(map[string]string{"command": "true", "bogus": "x"}); err == nil {
		t.Error("unknown input accepted")
	}
	if _, _, err := Get("run-command").Expand(map[string]string{"command": "true", "shell": "zsh"}); err == nil {
		t.Error("bad select value accepted")
	}
}

// 每个脚本都能被 sh 解析（不执行）。
func TestScriptsParse(t *testing.T) {
	dir := t.TempDir()
	for _, k := range All() {
		if k.Native {
			continue
		}
		script, _, _ := k.Expand(map[string]string{})
		p := filepath.Join(dir, k.Kind+".sh")
		os.WriteFile(p, []byte(script), 0o644)
		if out, err := exec.Command("sh", "-n", p).CombinedOutput(); err != nil {
			t.Errorf("%s: %v %s", k.Kind, err, out)
		}
	}
}

// 实际执行几个不依赖网络的脚本，验证输出与导出机制。
func TestScriptsRun(t *testing.T) {
	dir := t.TempDir()
	run := func(kind string, with map[string]string) (string, error) {
		script, env, err := Get(kind).Expand(with)
		if err != nil {
			return "", err
		}
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), "FLOW_WORKSPACE="+dir, "FLOW_TOOLS="+filepath.Join(dir, "tools"),
			"FLOW_OUTPUT="+filepath.Join(dir, "out"), "FLOW_ENV="+filepath.Join(dir, "env"))
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("set-variable", map[string]string{"vars": "A=1\nB=x y"}); err != nil {
		t.Fatalf("set-variable: %v %s", err, out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "out"))
	if string(data) != "A=1\nB=x y\n" {
		t.Errorf("outputs = %q", data)
	}
	if out, err := run("run-command", map[string]string{"command": "echo hi; false"}); err == nil || !strings.Contains(out, "hi") {
		t.Errorf("run-command failure = %v %s", err, out)
	}
}

// 网盘上传：多个路径与通配符、保留目录、上传前清空（目标目录已存在 / 还不存在）、分享链接与任务输出（对接模拟的 Cloudreve v4 接口）。
func TestCloudreveUpload(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	for _, existing := range []bool{true, false} {
		t.Run(fmt.Sprint("existing=", existing), func(t *testing.T) { testCloudreveUpload(t, existing) })
	}
}

func testCloudreveUpload(t *testing.T, existing bool) {
	app := "cloudreve://my/builds/%E5%BA%94%E7%94%A8"
	var mu sync.Mutex
	var uploads, deleted []string
	created := 0
	sessions := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		reply := func(data any) { json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data}) }
		var body map[string]any
		if strings.Contains(r.Header.Get("Content-Type"), "json") {
			json.NewDecoder(r.Body).Decode(&body)
		}
		switch p := strings.TrimPrefix(r.URL.Path, "/api/v4"); {
		case p == "/session/token":
			reply(map[string]any{"token": map[string]any{"access_token": "tok"}})
		case p == "/file" && r.Method == http.MethodGet && r.URL.Query().Get("uri") == "cloudreve://my/builds" && existing:
			reply(map[string]any{"files": []any{map[string]any{"name": "应用", "type": 1}}, "storage_policy": map[string]any{"id": "p1"}})
		case p == "/file" && r.Method == http.MethodGet && r.URL.Query().Get("uri") == app && existing && len(uploads) == 0:
			reply(map[string]any{"files": []any{map[string]any{"name": "old.zip", "type": 0}, map[string]any{"name": "dist", "type": 1}}, "storage_policy": map[string]any{"id": "p1"}})
		case p == "/file/info":
			// 已存在的目录带着上次的分享（外加一个过期的），沿用不重建
			var shares []any
			if existing {
				shares = []any{map[string]any{"url": "https://pan.example/s/old", "expired": true}, map[string]any{"url": "https://pan.example/s/keep"}}
			}
			reply(map[string]any{"extended_info": map[string]any{"shares": shares}})
		case p == "/file/create" || p == "/file" && r.Method == http.MethodGet:
			reply(map[string]any{"files": []any{}, "storage_policy": map[string]any{"id": "p1"}})
		case p == "/file" && r.Method == http.MethodDelete:
			if !existing { // 同真实的 Cloudreve：删不存在的目录报错
				json.NewEncoder(w).Encode(map[string]any{"code": 40004, "msg": "Path not exist"})
				return
			}
			for _, u := range body["uris"].([]any) {
				deleted = append(deleted, u.(string))
			}
			reply(nil)
		case p == "/file/upload" && r.Method == http.MethodPut:
			sid := fmt.Sprint(len(sessions))
			sessions[sid] = body["uri"].(string)
			reply(map[string]any{"session_id": sid, "chunk_size": 1 << 20})
		case strings.HasPrefix(p, "/file/upload/"):
			uploads = append(uploads, sessions[strings.Split(p, "/")[3]])
			reply(nil)
		case p == "/share":
			created++
			reply("https://pan.example/s/abc")
		default:
			http.Error(w, p, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "dist", "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "dist", "a.zip"), []byte("aa"), 0o644)
	os.WriteFile(filepath.Join(dir, "dist", "sub", "b.txt"), []byte("bbb"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.md"), []byte("n"), 0o644)
	script, env, err := Get("cloudreve-upload").Expand(map[string]string{"source": "dist\n*.md", "target": "/builds/应用/", "include_dir": "true", "clean": "true", "share": "true"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "FLOW_WORKSPACE="+dir, "FLOW_TOOLS="+filepath.Join(dir, ".tools"), "FLOW_OUTPUT="+filepath.Join(dir, ".out"),
		"CONN_SERVER="+srv.URL, "CONN_EMAIL=ci@example.com", "CONN_PASSWORD=x")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("upload: %v\n%s", err, out)
	}
	sort.Strings(uploads)
	want := []string{app + "/dist/a.zip", app + "/dist/sub/b.txt", app + "/notes.md"}
	// 清空只删目录里的内容，目录本身（和它的分享）保留
	var wantDeleted []string
	shareURL, wantCreated := "https://pan.example/s/abc", 1
	if existing {
		wantDeleted = []string{app + "/old.zip", app + "/dist"}
		shareURL, wantCreated = "https://pan.example/s/keep", 0
	}
	if !slices.Equal(uploads, want) || !slices.Equal(deleted, wantDeleted) || created != wantCreated {
		t.Errorf("uploads = %v, deleted = %v, shares created = %d\n%s", uploads, deleted, created, out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, ".out"))
	for _, line := range []string{"CLOUDREVE_DIR=/builds/应用", "CLOUDREVE_FILES=3", "CLOUDREVE_SIZE=6", "CLOUDREVE_SHARE_URL=" + shareURL,
		"CLOUDREVE_URL=" + srv.URL + "/home?path=cloudreve%3A%2F%2Fmy%2Fbuilds%2F%E5%BA%94%E7%94%A8"} {
		if !strings.Contains(string(data), line+"\n") {
			t.Errorf("outputs missing %q:\n%s", line, data)
		}
	}
}
