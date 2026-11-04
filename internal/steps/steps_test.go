package steps

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
