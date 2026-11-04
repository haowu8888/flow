package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"

	"flow/internal/model"
)

// 只透传这些 Runner 进程变量给 Step，避免 FLOW_RUNNER_TOKEN 等敏感信息泄露到用户脚本。
var passthroughEnv = []string{
	"PATH", "HOME", "USER", "LANG", "TMPDIR",
	"DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY",
}

// runStep 执行单个 Step：Job 未指定 image 时在本机 sh 中运行，否则在 docker 容器中运行，
// 工作目录挂载为 /workspace，同一 Job 的各 Step 共享该目录。env 为 Job 变量与密钥。
func runStep(ctx context.Context, job *model.Job, idx int, dir string, env map[string]string, sc *stepContext, out io.Writer) (int, error) {
	step := job.Steps[idx]
	var cmd *exec.Cmd
	if job.Image == "" {
		cmd = exec.CommandContext(ctx, "sh", "-ec", step.Run)
		cmd.Dir = dir
		setProcessGroup(cmd)
	} else {
		name := fmt.Sprintf("flow-%s-%d", job.ID, idx)
		args := []string{"run", "--rm", "--name", name, "-v", dir + ":/workspace", "-w", "/workspace"}
		if sc != nil {
			args = append(args, sc.mountArgs()...)
		}
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "-e", k) // 值经 docker CLI 的环境变量传入，不出现在进程参数里
		}
		args = append(args, job.Image, "sh", "-ec", step.Run)
		cmd = exec.CommandContext(ctx, "docker", args...)
		cmd.Cancel = func() error {
			_ = exec.Command("docker", "rm", "-f", name).Run()
			return cmd.Process.Kill()
		}
	}
	cmd.Env = baseEnv()
	if _, ok := env["PATH"]; ok {
		cmd.Env = slices.DeleteFunc(cmd.Env, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") })
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), fmt.Errorf("exit code %d", exitErr.ExitCode())
	}
	return -1, err
}

// baseEnv 返回透传给 Step 与 git 的 Runner 进程变量。
func baseEnv() []string {
	var env []string
	for _, k := range passthroughEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}
