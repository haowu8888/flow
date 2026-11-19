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

// runStep 执行单个 Step：Job 未指定 image 时在本机 sh 中运行，否则在 Job 的容器（jobContainer）里用 docker exec 运行，
// 工作目录挂载为 /workspace，同一 Job 的各 Step 共享该目录和容器。env 为 Job 变量与密钥。
func runStep(ctx context.Context, job *model.Job, idx int, dir string, env map[string]string, sc *stepContext, out io.Writer) (int, error) {
	step := job.Steps[idx]
	var cmd *exec.Cmd
	if job.Image == "" {
		cmd = exec.CommandContext(ctx, "sh", "-ec", step.Run)
		cmd.Dir = dir
		setProcessGroup(cmd)
	} else {
		if sc == nil || sc.container == nil {
			return -1, fmt.Errorf("job container not started")
		}
		name := sc.container.name
		args := []string{"exec", "-w", containerWS}
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "-e", k) // 值经 docker CLI 的环境变量传入，不出现在进程参数里
		}
		args = append(args, name, "sh", "-ec", step.Run)
		cmd = exec.CommandContext(ctx, "docker", args...)
		// 被取消时 Job 也就结束了：删掉容器才能停下容器里的进程（只结束 docker exec 客户端停不了）
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

// jobContainer 是 Job 指定 image 时所有 Step 共用的容器：前面步骤装的系统软件包、写在工作目录以外的文件
// 后面的步骤都能用到（同 GitLab / GitHub Actions）。以前每个 Step 各起一个 docker run --rm，装完就没了。
type jobContainer struct {
	name string
	path string // 镜像自己的 PATH：$FLOW_PATH 的目录加在它前面
	// 以下用于结束时交还文件属主
	image, dir, ctx string
	out             io.Writer
}

// startContainer 启动 Job 的容器，常驻到 Job 结束（stop）。
func startContainer(ctx context.Context, job *model.Job, dir string, sc *stepContext, out io.Writer) (*jobContainer, error) {
	name := "flow-" + job.ID
	_ = exec.Command("docker", "rm", "-f", name).Run() // Runner 异常退出时留下的同名容器
	args := []string{"run", "-d", "--name", name, "-v", dir + ":" + containerWS, "-w", containerWS}
	args = append(args, sc.mountArgs()...)
	// 绕过镜像的入口脚本只让容器常驻；Step 都经 docker exec 执行，本来就不走入口脚本
	args = append(args, "--entrypoint", "tail", job.Image, "-f", "/dev/null")
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = baseEnv()
	cmd.Stderr = out // 拉取镜像的进度
	if err := cmd.Run(); err != nil {
		_ = exec.Command("docker", "rm", "-f", name).Run()
		return nil, fmt.Errorf("start container %s: %w", job.Image, err)
	}
	c := &jobContainer{name: name, path: defaultUnixPaths, image: job.Image, dir: dir, ctx: sc.ctx, out: out}
	inspect := exec.CommandContext(ctx, "docker", "container", "inspect", "-f", "{{range .Config.Env}}{{println .}}{{end}}", name)
	inspect.Env = baseEnv()
	if b, err := inspect.Output(); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if p, ok := strings.CutPrefix(strings.TrimSpace(line), "PATH="); ok && p != "" {
				c.path = p
			}
		}
	}
	return c, nil
}

// stop 删除容器。容器里的进程多以 root 写文件，Runner 不是 root 时再把工作目录和上下文目录交还给 Runner 用户，
// 否则保存缓存、上传产物会读不到文件，Job 结束后工作目录也删不掉、越积越多。
func (c *jobContainer) stop() {
	cmd := exec.Command("docker", "rm", "-f", c.name)
	cmd.Env = baseEnv()
	_ = cmd.Run()
	uid := os.Getuid()
	if uid <= 0 { // root，或 Windows（-1）
		return
	}
	// 另起一个临时容器来改：Job 被取消时它的容器已经删掉了（runStep 的 Cancel）
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	chown := exec.CommandContext(ctx, "docker", "run", "--rm", "-u", "0", "--entrypoint", "chown",
		"-v", c.dir+":"+containerWS, "-v", c.ctx+":"+containerCtx, c.image,
		"-Rh", fmt.Sprintf("%d:%d", uid, os.Getgid()), containerWS, containerCtx)
	chown.Env = baseEnv()
	if b, err := chown.CombinedOutput(); err != nil {
		fmt.Fprintf(c.out, "==> warning: chown workspace to runner user: %v %s\n", err, strings.TrimSpace(string(b)))
	}
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
