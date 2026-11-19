package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"flow/internal/model"
	"flow/internal/protocol"
)

const defaultGitUser = "oauth2"

// 拉取代码的重试：直连 GitHub 这类境外仓库时连接常被中途断开（SSL unexpected eof），重试大多能成功。
var (
	fetchAttempts = 3
	fetchBackoff  = []time.Duration{3 * time.Second, 10 * time.Second}
)

// checkout 在空工作目录中拉取代码：浅克隆指定分支 / 标签（空为远端默认分支），再检出指定提交，
// 返回检出的提交（无法确定时为空）。
// 访问令牌经 GIT_CONFIG_* 环境变量以 HTTP 头传给 git，不出现在命令行参数和 .git/config 中。
// 配置了加速地址（co.Mirrors）时，先直连仓库取得目标提交，再经加速地址下载，只检出该提交：
// 提交号由内容算出，代理返回的代码对不上就不会被使用。
func checkout(ctx context.Context, dir string, co *model.Checkout, secrets map[string]string, out io.Writer) (string, error) {
	env := append(baseEnv(), "GIT_TERMINAL_PROMPT=0")
	if co.TokenSecret != "" {
		token, ok := secrets[co.TokenSecret]
		if !ok {
			return "", fmt.Errorf("checkout: secret %s not found", co.TokenSecret)
		}
		user := co.Username
		if user == "" {
			user = defaultGitUser
		}
		cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+cred)
	}
	run := func(w io.Writer, args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, w, out
		if err := cmd.Run(); err != nil {
			sub := args[0]
			for i := 0; i+1 < len(args) && args[i] == "-c"; i += 2 {
				sub = args[i+2]
			}
			return fmt.Errorf("git %s: %w", sub, err)
		}
		return nil
	}
	git := func(args ...string) error { return run(out, args...) }
	// 传输速度低于 1 KB/s 持续 60 秒（只查提交号时 20 秒）就放弃这次连接，交给重试，而不是一直挂到任务超时。
	slow := []string{"-c", "http.lowSpeedLimit=1000", "-c", "http.lowSpeedTime=60"}
	quick := []string{"-c", "http.lowSpeedLimit=1000", "-c", "http.lowSpeedTime=20"}
	fetchFrom := func(remote, ref string) error {
		args := append(slices.Clone(slow), "fetch", "--no-tags", "--progress")
		if co.Depth > 0 {
			args = append(args, fmt.Sprintf("--depth=%d", co.Depth))
		}
		return git(append(args, remote, ref)...)
	}
	fetch := func(ref string) error {
		var err error
		for i := 0; i < fetchAttempts; i++ {
			if i > 0 {
				d := fetchBackoff[min(i-1, len(fetchBackoff)-1)]
				fmt.Fprintf(out, "==> fetch failed, retrying in %s (%d/%d)\n", d, i+1, fetchAttempts)
				sleep(ctx, d)
			}
			if err = fetchFrom("origin", ref); err == nil || ctx.Err() != nil {
				return err
			}
		}
		return err
	}
	has := func(commit string) bool { return git("cat-file", "-e", commit+"^{commit}") == nil }

	ref := co.Ref
	if ref == "" {
		ref = "HEAD"
	}
	fmt.Fprintf(out, "==> checkout %s (%s)\n", co.URL, ref)
	if err := git("init", "-q"); err != nil {
		return "", err
	}
	if err := git("remote", "add", "origin", co.URL); err != nil {
		return "", err
	}

	target := ""
	if len(co.Mirrors) > 0 && co.TokenSecret != "" {
		fmt.Fprintln(out, "==> note: mirrors are not used with token_secret (the token would be sent to the mirror)")
	} else if len(co.Mirrors) > 0 {
		want := co.Commit
		if want == "" {
			var err error
			if want, err = lsRemote(ctx, run, quick, ref); err != nil {
				fmt.Fprintf(out, "==> warning: cannot resolve %s on origin, mirrors skipped: %v\n", ref, err)
			}
		}
		if want != "" {
			fmt.Fprintf(out, "==> target commit %s\n", want)
			for _, m := range co.Mirrors {
				url := m + co.URL
				fmt.Fprintf(out, "==> fetch via %s\n", m)
				// 先按分支拉（镜像有缓存时可能落后），没有目标提交再按提交号拉
				_ = fetchFrom(url, ref)
				if !has(want) {
					_ = fetchFrom(url, want)
				}
				if has(want) {
					target = want
					break
				}
				fmt.Fprintf(out, "==> %s did not return commit %s, trying next\n", m, want)
			}
		}
	}
	if target == "" {
		if err := fetch(ref); err != nil {
			return "", err
		}
		target = "FETCH_HEAD"
		if co.Commit != "" {
			target = co.Commit
			// 指定提交不在浅克隆历史中时（分支已前进），尝试直接按 SHA 拉取。
			if !has(co.Commit) {
				if err := fetch(co.Commit); err != nil {
					return "", fmt.Errorf("commit %s not found on %s: %w", co.Commit, ref, err)
				}
			}
		}
	}
	if err := git("checkout", "-q", "--detach", target); err != nil {
		return "", err
	}
	var sha bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir, cmd.Env, cmd.Stdout = dir, env, &sha
	if cmd.Run() != nil {
		return "", nil
	}
	head := strings.TrimSpace(sha.String())
	fmt.Fprintf(out, "==> HEAD is %s\n", head)
	return head, nil
}

// lsRemote 直连 origin 查出 ref 指向的提交（请求很小，比下载代码更容易成功），按 fetchAttempts 重试。
// ref 可以是 HEAD、完整引用（refs/...）或分支 / 标签名；附注标签取它指向的提交。
func lsRemote(ctx context.Context, run func(io.Writer, ...string) error, slow []string, ref string) (string, error) {
	var buf bytes.Buffer
	var err error
	for i := 0; i < fetchAttempts; i++ {
		if i > 0 {
			sleep(ctx, fetchBackoff[min(i-1, len(fetchBackoff)-1)])
		}
		buf.Reset()
		if err = run(&buf, append(slices.Clone(slow), "ls-remote", "origin", ref)...); err == nil || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return "", err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(buf.String(), "\n") {
		if sha, name, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok {
			refs[name] = sha
		}
	}
	names := []string{ref}
	if ref != "HEAD" && !strings.HasPrefix(ref, "refs/") {
		names = []string{"refs/heads/" + ref, "refs/tags/" + ref}
	}
	for _, n := range names {
		if sha := refs[n+"^{}"]; sha != "" {
			return sha, nil
		}
		if sha := refs[n]; sha != "" {
			return sha, nil
		}
	}
	return "", fmt.Errorf("%s not found", ref)
}

// downloadArtifacts 把上游 Job 的产物解压到工作目录。
func (r *Runner) downloadArtifacts(ctx context.Context, dir string, refs []protocol.ArtifactRef, out io.Writer) error {
	for _, a := range refs {
		fmt.Fprintf(out, "==> download artifacts of %s (%s)\n", a.JobName, humanSize(a.Size))
		rc, err := r.client.DownloadArtifact(ctx, a.ID)
		if err != nil {
			return err
		}
		n, err := extractTarGz(dir, rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("extract artifacts of %s: %w", a.JobName, err)
		}
		fmt.Fprintf(out, "    %d entries\n", n)
	}
	return nil
}

// uploadArtifacts 打包匹配的路径并流式上传；没有匹配时只记录警告。
func (r *Runner) uploadArtifacts(ctx context.Context, job *model.Job, dir string, out io.Writer) error {
	paths, err := collectPaths(dir, job.Artifacts)
	if err != nil {
		return fmt.Errorf("artifacts: %w", err)
	}
	if len(paths) == 0 {
		fmt.Fprintf(out, "==> warning: no files match artifacts %v\n", job.Artifacts)
		return nil
	}
	fmt.Fprintf(out, "==> upload artifacts: %d entries\n", len(paths))
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTarGz(pw, dir, paths)) }()
	err = r.client.UploadArtifact(ctx, job.ID, job.RunnerID, pr)
	pr.CloseWithError(err) // 上传提前失败时让打包协程退出
	if err != nil {
		return fmt.Errorf("upload artifacts: %w", err)
	}
	return nil
}

// cachePath 返回缓存归档路径：按租户与展开后的 key 区分，key 中可引用 $VAR / ${VAR}。
func (r *Runner) cachePath(job *model.Job, env map[string]string) (key, path string) {
	key = os.Expand(job.Cache.Key, func(k string) string { return env[k] })
	sum := sha256.Sum256([]byte(job.TenantID + "\x00" + key))
	return key, filepath.Join(r.cfg.CacheDir, hex.EncodeToString(sum[:16])+".tar.gz")
}

// restoreCache 命中时把缓存解压到工作目录；缓存损坏只告警不失败。
func (r *Runner) restoreCache(job *model.Job, dir string, env map[string]string, out io.Writer) {
	key, path := r.cachePath(job, env)
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(out, "==> cache miss: %s\n", key)
		return
	}
	defer f.Close()
	if n, err := extractTarGz(dir, f); err != nil {
		fmt.Fprintf(out, "==> warning: restore cache %s: %v\n", key, err)
	} else {
		fmt.Fprintf(out, "==> cache restored: %s (%d entries)\n", key, n)
	}
}

// saveCache 在 Job 成功后保存缓存（先写临时文件再改名，并发 Job 不会读到半个归档）。
func (r *Runner) saveCache(job *model.Job, dir string, env map[string]string, out io.Writer) {
	key, path := r.cachePath(job, env)
	paths, err := collectPaths(dir, job.Cache.Paths)
	if err == nil && len(paths) == 0 {
		fmt.Fprintf(out, "==> warning: no files match cache paths %v\n", job.Cache.Paths)
		return
	}
	if err == nil {
		err = writeFileAtomic(path, func(w io.Writer) error { return writeTarGz(w, dir, paths) })
	}
	if err != nil {
		fmt.Fprintf(out, "==> warning: save cache %s: %v\n", key, err)
		return
	}
	fmt.Fprintf(out, "==> cache saved: %s (%d entries)\n", key, len(paths))
}

func writeFileAtomic(path string, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := write(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
