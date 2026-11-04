package blob

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func exercise(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	if err := s.Put(ctx, "runs/r1/a.tar.gz", strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "runs/r1/b.tar.gz", strings.NewReader("unknown size"), -1); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"runs/r1/a.tar.gz": "hello", "runs/r1/b.tar.gz": "unknown size"} {
		r, err := s.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(r)
		r.Close()
		if string(got) != want {
			t.Errorf("%s = %q", key, got)
		}
	}
	if err := s.Delete(ctx, "runs/r1/a.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, "runs/r1/a.tar.gz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted = %v", err)
	}
	if err := s.Delete(ctx, "runs/r1/a.tar.gz"); err != nil {
		t.Errorf("double delete = %v", err)
	}
}

func TestLocal(t *testing.T) {
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, l)
	if err := l.Put(context.Background(), "../escape", strings.NewReader("x"), 1); err == nil {
		t.Error("path traversal accepted")
	}
}

func TestMemory(t *testing.T) { exercise(t, NewMemory()) }

// AWS 文档中的 SigV4 示例（GET Object，带 Range 头）。
func TestSignV4Vector(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	signV4(req, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", emptySHA256,
		time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
	}
}

// 用假 S3 服务验证请求路径、签名头和对象读写。
func TestS3AgainstFake(t *testing.T) {
	var mu sync.Mutex
	objs := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=ak/") {
			http.Error(w, "unsigned", http.StatusForbidden)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			if r.ContentLength < 0 {
				http.Error(w, "length required", http.StatusLengthRequired)
				return
			}
			b, _ := io.ReadAll(r.Body)
			objs[r.URL.Path] = string(b)
		case http.MethodGet:
			v, ok := objs[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, v)
		case http.MethodDelete:
			delete(objs, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	s, err := NewS3(S3Config{Endpoint: srv.URL, Bucket: "flow", AccessKey: "ak", SecretKey: "sk", PathStyle: true, Prefix: "ci/"})
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, s)
	if _, ok := objs["/flow/ci/runs/r1/b.tar.gz"]; !ok {
		t.Errorf("objects = %v", objs)
	}
}
