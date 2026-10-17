package httpx

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 嵌入产物自检：字体必须能被正确提供，且不能被 SPA 回退吞成 index.html。
//
// 这条测试守的是"前端改为自托管字体"这次改动的回归点：
// web_handler 对任意不存在的路径都回退 index.html（SPA 路由需要），
// 因此一个拼错的字体路径会静默返回 HTML 而不是 404，浏览器拿到
// 非法 woff2 后只能静默回退系统字体——界面看起来"字体没生效"，
// 却没有任何报错。把这个失败模式在这里钉死。
func TestEmbeddedFontAssetsServed(t *testing.T) {
	t.Run("fonts.css", func(t *testing.T) {
		rec := httptest.NewRecorder()
		webHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fonts/fonts.css", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("fonts.css must be served, got %d", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
			t.Fatalf("fonts.css fell back to index.html (Content-Type %q) — font path is wrong", ct)
		}
		body := rec.Body.Bytes()
		if !bytes.Contains(body, []byte("@font-face")) {
			t.Fatalf("fonts.css has no @font-face rules (%d bytes)", len(body))
		}
		// 不得有任何外网字体引用：内网交付要求运行时零外网依赖。
		// 只查真正会发起网络请求的位置（url(...) 与协议相对地址）——
		// 注释里说明"为什么改成自托管"时提到域名是允许的。
		for _, forbidden := range []string{"fonts.googleapis.com", "fonts.gstatic.com"} {
			for _, pattern := range []string{
				"url(https://" + forbidden,
				"url(http://" + forbidden,
				"//" + forbidden,
				"url('" + forbidden,
				"url(\"" + forbidden,
			} {
				if bytes.Contains(body, []byte(pattern)) {
					t.Fatalf("fonts.css loads %s over the network (runtime must stay offline-capable)", forbidden)
				}
			}
		}
	})

	t.Run("woff2 payload", func(t *testing.T) {
		name := firstEmbeddedFont(t)
		rec := httptest.NewRecorder()
		webHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fonts/"+name, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s must be served, got %d", name, rec.Code)
		}
		body := rec.Body.Bytes()
		if !bytes.HasPrefix(body, []byte("wOF2")) {
			t.Fatalf("%s must be a real woff2 (got %d bytes, prefix %q)", name, len(body), body[:min(4, len(body))])
		}
	})
}

func firstEmbeddedFont(t *testing.T) string {
	t.Helper()
	sub, err := fs.Sub(webDist, "web_dist")
	if err != nil {
		t.Fatalf("sub fs: %v", err)
	}
	entries, err := fs.ReadDir(sub, "fonts")
	if err != nil {
		t.Fatalf("read embedded fonts dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".woff2") {
			return entry.Name()
		}
	}
	t.Fatal("no .woff2 found in embedded web_dist/fonts — self-hosted fonts missing from the bundle")
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
