package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTarRoundTrip(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "dist/sub"), 0o755)
	os.WriteFile(filepath.Join(src, "dist/sub/a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(src, "dist/run.sh"), []byte("#!/bin/sh"), 0o755)
	os.Symlink("sub/a.txt", filepath.Join(src, "dist/link"))
	os.WriteFile(filepath.Join(src, "skip.txt"), []byte("x"), 0o644)

	paths, err := collectPaths(src, []string{"dist", "none-*"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths, ","); got != "dist,dist/link,dist/run.sh,dist/sub,dist/sub/a.txt" {
		t.Fatalf("paths = %s", got)
	}
	if _, err := collectPaths(src, []string{"../*"}); err == nil {
		t.Error("pattern outside workspace accepted")
	}

	var buf bytes.Buffer
	if err := writeTarGz(&buf, src, paths); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if n, err := extractTarGz(dst, &buf); err != nil || n != 5 {
		t.Fatalf("extract = %d, %v", n, err)
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "dist/link")); string(data) != "a" {
		t.Errorf("symlink content = %q", data)
	}
	if st, _ := os.Stat(filepath.Join(dst, "dist/run.sh")); st.Mode().Perm()&0o100 == 0 {
		t.Errorf("exec bit lost: %v", st.Mode())
	}
}

func TestExtractRejectsEscapes(t *testing.T) {
	cases := map[string][]tar.Header{
		"dotdot":         {{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644}},
		"absolute link":  {{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/etc"}},
		"escaping link":  {{Name: "a/l", Typeflag: tar.TypeSymlink, Linkname: "../../x"}},
		"write via link": {{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "."}, {Name: "l/../../evil", Typeflag: tar.TypeReg, Mode: 0o644}},
	}
	for name, hdrs := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			for _, h := range hdrs {
				tw.WriteHeader(&h)
			}
			tw.Close()
			gz.Close()
			root := filepath.Join(t.TempDir(), "root")
			os.Mkdir(root, 0o755)
			if _, err := extractTarGz(root, &buf); err == nil {
				t.Error("escape accepted")
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(root), "evil")); err == nil {
				t.Error("file written outside root")
			}
		})
	}
}

func TestStepEnv(t *testing.T) {
	job := map[string]string{"A": "1", "TOKEN": "secret", "SHA": "abc"}
	if got := stepEnv(job, nil, nil); got["A"] != "1" {
		t.Error("job env not passed through")
	}
	got := stepEnv(job, map[string]string{"IMAGE": "app:${SHA}-${MISSING}", "TOKEN": "override", "LIT": "$SHA"},
		map[string]string{"TOKEN": "secret"})
	if got["IMAGE"] != "app:abc-" || got["TOKEN"] != "secret" || got["LIT"] != "$SHA" || got["A"] != "1" {
		t.Errorf("step env = %v", got)
	}
	if _, ok := job["IMAGE"]; ok {
		t.Error("job env mutated")
	}
}
