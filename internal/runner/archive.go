package runner

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// collectPaths 按通配符（* ? [...]，相对 root）匹配文件，目录递归展开，返回去重排序后的相对路径。
// 匹配到 root 之外的路径会报错。
func collectPaths(root string, patterns []string) ([]string, error) {
	seen := map[string]bool{}
	for _, pat := range patterns {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if clean := filepath.Clean(filepath.FromSlash(pat)); filepath.IsAbs(clean) || clean == ".." ||
			strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("path %q must be inside the workspace", pat)
		}
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pat)))
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", pat, err)
		}
		for _, m := range matches {
			rel, err := filepath.Rel(root, m)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("path %q is outside the workspace", pat)
			}
			err = filepath.WalkDir(m, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				r, _ := filepath.Rel(root, p)
				if r != "." {
					seen[filepath.ToSlash(r)] = true
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	slices.Sort(out) // 父目录排在子项之前
	return out, nil
}

// writeTarGz 把 root 下的相对路径打包为 tar.gz（保留目录、普通文件与符号链接）。
func writeTarGz(w io.Writer, root string, paths []string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, rel := range paths {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(full); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			continue // 跳过设备文件、socket 等
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if info.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(full)
			if err != nil {
				return err
			}
			_, err = io.CopyN(tw, f, info.Size())
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// extractTarGz 解压到 root，拒绝逃逸出 root 的路径与符号链接，返回解压的条目数。
func extractTarGz(root string, r io.Reader) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	defer gz.Close()
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return n, err
		}
		if err := checkParent(root, target); err != nil {
			return n, err
		}
		mode := hdr.FileInfo().Mode().Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode|0o700); err != nil {
				return n, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return n, err
			}
			_ = os.Remove(target) // 覆盖已存在的文件或符号链接，而不是写穿符号链接
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode|0o600)
			if err != nil {
				return n, err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return n, err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) {
				return n, fmt.Errorf("archive entry %q: absolute symlink target", hdr.Name)
			}
			if _, err := safeJoin(root, filepath.Join(filepath.Dir(filepath.Clean(hdr.Name)), hdr.Linkname)); err != nil {
				return n, fmt.Errorf("archive entry %q: symlink escapes workspace", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return n, err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return n, err
			}
		default:
			continue
		}
		n++
	}
}

func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes workspace", name)
	}
	return filepath.Join(root, clean), nil
}

// checkParent 确认 target 的父目录（解析符号链接后）仍在 root 内，防止经由已解压的符号链接写到外部。
func checkParent(root, target string) error {
	dir := filepath.Dir(target)
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if errors.Is(err, fs.ErrNotExist) {
			if dir == root || dir == filepath.Dir(dir) {
				return nil
			}
			dir = filepath.Dir(dir) // 尚未创建，检查更上层
			continue
		}
		if err != nil {
			return err
		}
		if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
			return fmt.Errorf("archive entry %q escapes workspace via symlink", target)
		}
		return nil
	}
}
