package runner

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"flow/internal/model"
	"flow/internal/protocol"
)

// 由 Runner 直接执行的内置步骤（需要以 Runner 身份访问 Server，不能交给用户脚本）。
type nativeStep func(ctx context.Context, r *Runner, a *protocol.ClaimResponse, sc *stepContext, env map[string]string, out io.Writer) (int, error)

var nativeSteps = map[string]nativeStep{
	"artifact-upload":   uploadNamedArtifact,
	"artifact-download": downloadNamedArtifact,
}

// outputKey 把构建物名称转成环境变量名的一部分：ARTIFACT_<名称>。
func outputKey(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// uploadNamedArtifact 把匹配的文件打成 zip，作为命名构建物上传（版本为空时由 Server 递增为 vN）。
// 输入：INPUT_NAME 构建物名称，INPUT_PATHS 打包路径（每行一个，相对代码目录，支持通配符），
// INPUT_VERSION 版本，INPUT_INCLUDE_DIR 为 true 时保留打包路径的目录层级。
func uploadNamedArtifact(ctx context.Context, r *Runner, a *protocol.ClaimResponse, sc *stepContext, env map[string]string, out io.Writer) (int, error) {
	name := strings.TrimSpace(env["INPUT_NAME"])
	if name == "" {
		return 1, fmt.Errorf("构建物名称不能为空")
	}
	var patterns []string
	for _, p := range strings.Split(env["INPUT_PATHS"], "\n") {
		if p = strings.TrimSpace(p); p != "" {
			patterns = append(patterns, p)
		}
	}
	if len(patterns) == 0 {
		return 1, fmt.Errorf("打包路径不能为空")
	}
	keepDir := env["INPUT_INCLUDE_DIR"] == "true"
	tmp, err := os.CreateTemp(r.cfg.WorkDir, "artifact-*.zip")
	if err != nil {
		return -1, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	zw := zip.NewWriter(tmp)
	files := 0
	for _, pat := range patterns {
		if err := checkRel(pat); err != nil {
			return 1, err
		}
		matches, err := filepath.Glob(filepath.Join(sc.dir, pat))
		if err != nil {
			return 1, fmt.Errorf("打包路径 %q: %w", pat, err)
		}
		if len(matches) == 0 {
			return 1, fmt.Errorf("打包路径 %q 没有匹配的文件", pat)
		}
		for _, m := range matches {
			base := filepath.Dir(m) // 默认不含打包路径本身的目录：dist/ → 包内直接是 dist 里的文件
			if info, err := os.Stat(m); err == nil && info.IsDir() && !keepDir {
				base = m
			}
			if keepDir {
				base = sc.dir
			}
			err := filepath.WalkDir(m, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
					return err
				}
				rel, err := filepath.Rel(base, p)
				if err != nil {
					return err
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				h, err := zip.FileInfoHeader(info)
				if err != nil {
					return err
				}
				h.Name, h.Method = filepath.ToSlash(rel), zip.Deflate
				w, err := zw.CreateHeader(h)
				if err != nil {
					return err
				}
				f, err := os.Open(p)
				if err != nil {
					return err
				}
				defer f.Close()
				_, err = io.Copy(w, f)
				files++
				return err
			})
			if err != nil {
				return -1, err
			}
		}
	}
	if err := zw.Close(); err != nil {
		return -1, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return -1, err
	}
	q := url.Values{"runner_id": {a.Job.RunnerID}, "name": {name}, "version": {strings.TrimSpace(env["INPUT_VERSION"])}, "format": {"zip"},
		"connection": {env[stepConnectionEnv]}, "bucket": {strings.TrimSpace(env["INPUT_BUCKET"])}, "expire_days": {strings.TrimSpace(env["INPUT_EXPIRE_DAYS"])}}
	var art model.Artifact
	path := jobPath(a.Job.ID, "artifacts") + "?" + q.Encode()
	if _, err := r.client.doWith(ctx, r.client.upload, "POST", path, tmp, "application/zip", &art); err != nil {
		return -1, fmt.Errorf("上传构建物：%w", err)
	}
	fmt.Fprintf(out, "[SUCCESS] 已上传构建物 %s %s（%d 个文件，%d 字节）\n", art.Name, art.Version, files, art.Size)
	key := "ARTIFACT_" + outputKey(name)
	sc.outputs[key], sc.env[key] = name+"@"+art.Version, name+"@"+art.Version
	sc.outputs[key+"_VERSION"], sc.env[key+"_VERSION"] = art.Version, art.Version
	sc.outputs[key+"_ID"], sc.env[key+"_ID"] = art.ID, art.ID
	if art.URL != "" {
		sc.outputs[key+"_URL"], sc.env[key+"_URL"] = art.URL, art.URL
	}
	return 0, nil
}

// stepConnectionEnv 向内置步骤传递所选服务连接的名称。
const stepConnectionEnv = "FLOW_STEP_CONNECTION"

// downloadNamedArtifact 下载上游任务上传的命名构建物并解压。输入：INPUT_ARTIFACT 名称，INPUT_TARGET_DIR 解压目录。
func downloadNamedArtifact(ctx context.Context, r *Runner, a *protocol.ClaimResponse, sc *stepContext, env map[string]string, out io.Writer) (int, error) {
	name := strings.TrimSpace(env["INPUT_ARTIFACT"])
	var ref *protocol.ArtifactRef
	for i := range a.Named {
		if a.Named[i].Name == name {
			ref = &a.Named[i] // 多个上游同名时取最后一个
		}
	}
	if ref == nil {
		var names []string
		for _, n := range a.Named {
			names = append(names, n.Name)
		}
		return 1, fmt.Errorf("上游任务没有名为 %q 的构建物（可用：%s）", name, strings.Join(names, "、"))
	}
	target := strings.TrimSpace(env["INPUT_TARGET_DIR"])
	if target == "" {
		target = "."
	}
	if err := checkRel(target); err != nil && target != "." {
		return 1, err
	}
	dest := filepath.Join(sc.dir, target)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return -1, err
	}
	rc, err := r.client.DownloadArtifact(ctx, ref.ID)
	if err != nil {
		return -1, err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(r.cfg.WorkDir, "artifact-*")
	if err != nil {
		return -1, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := io.Copy(tmp, rc)
	if err != nil {
		return -1, err
	}
	if ref.Format == "zip" {
		zr, err := zip.NewReader(tmp, n)
		if err != nil {
			return -1, err
		}
		for _, f := range zr.File {
			if err := extractZipFile(f, dest); err != nil {
				return -1, err
			}
		}
	} else {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return -1, err
		}
		if _, err := extractTarGz(dest, tmp); err != nil {
			return -1, err
		}
	}
	fmt.Fprintf(out, "[SUCCESS] 已下载构建物 %s %s 到 %s\n", ref.Name, ref.Version, target)
	return 0, nil
}

func extractZipFile(f *zip.File, dest string) error {
	p, err := safeJoin(dest, f.Name)
	if err != nil {
		return fmt.Errorf("构建物中的路径不安全：%s", f.Name)
	}
	if f.FileInfo().IsDir() {
		return os.MkdirAll(p, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	w, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode().Perm()|0o600)
	if err != nil {
		return err
	}
	defer w.Close()
	_, err = io.Copy(w, rc)
	return err
}

func checkRel(p string) error {
	p = filepath.ToSlash(strings.TrimSpace(p))
	if p == "" || strings.HasPrefix(p, "/") || slices.Contains(strings.Split(p, "/"), "..") {
		return fmt.Errorf("路径 %q 必须是代码目录内的相对路径", p)
	}
	return nil
}
