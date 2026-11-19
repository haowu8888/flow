// Package webui 内嵌前端构建产物（web/ 下 npm run build 输出到 dist/），由 Server 在 / 下托管。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Handler 托管静态文件；前端未构建时返回提示而非空白页。
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "Web UI is not built. Run `npm ci && npm run build` in web/, then rebuild the server.", http.StatusNotFound)
		})
	}
	return serve(sub)
}

func serve(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 内嵌文件没有修改时间，浏览器无从校验缓存，不加缓存头就每次都重新下载。
		// assets/ 下文件名带内容哈希，可以长期缓存（不存在的不缓存，免得 404 被记住）；index.html 每次都取，发版后才能拿到新的文件名。
		cache := "no-cache"
		if name, ok := strings.CutPrefix(r.URL.Path, "/assets/"); ok {
			if _, err := fs.Stat(fsys, "assets/"+name); err == nil {
				cache = "public, max-age=31536000, immutable"
			}
		}
		w.Header().Set("Cache-Control", cache)
		files.ServeHTTP(w, r)
	})
}
