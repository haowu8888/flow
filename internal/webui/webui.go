// Package webui 内嵌前端构建产物（web/ 下 npm run build 输出到 dist/），由 Server 在 / 下托管。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
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
	return http.FileServerFS(sub)
}
