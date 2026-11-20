package runner

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func runHTTPStep(t *testing.T, env map[string]string) (*stepContext, int, error, string) {
	t.Helper()
	sc := &stepContext{env: map[string]string{}, outputs: map[string]string{}}
	var out bytes.Buffer
	code, err := httpRequest(context.Background(), nil, nil, sc, env, &out)
	return sc, code, err, out.String()
}

func TestHTTPRequestStep(t *testing.T) {
	var got struct{ method, ctype, auth, host, body string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method, got.ctype, got.auth, got.host, got.body = r.Method, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), r.Host, string(b)
		w.Write([]byte(`{"code":"200","data":{"accessToken":"at.x","expireTime":1795746304500},"list":[{"id":"a"}],"ok":true}` + "\n"))
	}))
	defer srv.Close()

	sc, code, err, out := runHTTPStep(t, map[string]string{
		"INPUT_METHOD": "POST", "INPUT_URL": srv.URL + "/token?k=v", "INPUT_HEADERS": "Authorization: Bearer t\nHost: api.example\n",
		"INPUT_BODY": `{"a":1}`, "INPUT_CONTENT_TYPE": "application/json", "INPUT_EXPECT": "200", "INPUT_JSON_PATH": "data",
	})
	if code != 0 || err != nil {
		t.Fatalf("code=%d err=%v\n%s", code, err, out)
	}
	if got.method != "POST" || got.ctype != "application/json" || got.auth != "Bearer t" || got.host != "api.example" || got.body != `{"a":1}` {
		t.Fatalf("request = %+v", got)
	}
	if v := sc.outputs["HTTP_VALUE"]; v != `{"accessToken":"at.x","expireTime":1795746304500}` || sc.env["HTTP_VALUE"] != v {
		t.Fatalf("HTTP_VALUE = %q", v)
	}
	if sc.outputs["HTTP_STATUS"] != "200" || strings.Contains(sc.outputs["HTTP_BODY"], "\n") || !strings.Contains(out, "[SUCCESS]") {
		t.Fatalf("outputs = %v\n%s", sc.outputs, out)
	}

	for path, want := range map[string]string{"data.accessToken": "at.x", "data.expireTime": "1795746304500", "list.0.id": "a", "ok": "true"} {
		sc, _, err, _ := runHTTPStep(t, map[string]string{"INPUT_URL": srv.URL, "INPUT_JSON_PATH": path})
		if err != nil || sc.outputs["HTTP_VALUE"] != want {
			t.Errorf("%s = %q, %v; want %q", path, sc.outputs["HTTP_VALUE"], err, want)
		}
	}
	for _, path := range []string{"nope", "list.1", "data.accessToken.x"} {
		if _, code, err, _ := runHTTPStep(t, map[string]string{"INPUT_URL": srv.URL, "INPUT_JSON_PATH": path}); code != 1 || err == nil || !strings.Contains(err.Error(), "响应里没有") {
			t.Errorf("%s: code=%d err=%v", path, code, err)
		}
	}
	if _, code, err, _ := runHTTPStep(t, map[string]string{"INPUT_URL": srv.URL, "INPUT_EXPECT": "201, 4xx"}); code != 1 || err == nil {
		t.Fatalf("expect mismatch: code=%d err=%v", code, err)
	}
	if _, code, err, _ := runHTTPStep(t, map[string]string{"INPUT_URL": "ftp://x"}); code != 1 || err == nil {
		t.Fatalf("bad url: code=%d err=%v", code, err)
	}
}

func TestHTTPRequestRetries(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	sc, code, err, out := runHTTPStep(t, map[string]string{"INPUT_URL": srv.URL, "INPUT_RETRIES": "1"})
	if code != 0 || err != nil || n.Load() != 2 || sc.outputs["HTTP_BODY"] != "ok" || !strings.Contains(out, "[WARN]") {
		t.Fatalf("code=%d err=%v calls=%d\n%s", code, err, n.Load(), out)
	}

	n.Store(0)
	if _, code, err, _ := runHTTPStep(t, map[string]string{"INPUT_URL": srv.URL}); code != 1 || err == nil || n.Load() != 1 {
		t.Fatalf("no retry: code=%d err=%v calls=%d", code, err, n.Load())
	}
}

func TestTruncUTF8(t *testing.T) {
	if got := string(truncUTF8([]byte("ab中文"), 4)); got != "ab" {
		t.Fatalf("got %q", got)
	}
	if got := string(truncUTF8([]byte("ab中文"), 5)); got != "ab中" {
		t.Fatalf("got %q", got)
	}
}
