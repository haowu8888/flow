package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// 带哈希的 assets/ 长期缓存，其余（index.html）每次校验，不存在的文件不缓存。
func TestCacheHeaders(t *testing.T) {
	h := serve(fstest.MapFS{
		"index.html":      {Data: []byte("<html></html>")},
		"assets/app-1.js": {Data: []byte("x")},
	})
	for path, want := range map[string]string{
		"/":                "no-cache",
		"/assets/app-1.js": "public, max-age=31536000, immutable",
		"/assets/gone.js":  "", // 404：FileServer 出错时去掉缓存头
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Header().Get("Cache-Control") != want {
			t.Errorf("%s: %d Cache-Control %q, want %q", path, rec.Code, rec.Header().Get("Cache-Control"), want)
		}
	}
}
