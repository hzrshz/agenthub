// Package httpx 的 Web 静态资源：go:embed 构建产物，单二进制交付。
package httpx

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web_dist
var webDist embed.FS

// webHandler 服务构建产物；非资源路径回退到 index.html（前端路由）。
func webHandler() http.Handler {
	sub, err := fs.Sub(webDist, "web_dist")
	if err != nil {
		panic("web_dist missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(sub, path); err != nil {
			// SPA 回退。
			r2 := new(http.Request)
			*r2 = *r
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
