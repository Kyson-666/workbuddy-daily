package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

/* ------------------------------------------------------------------ */
/* 路由：只做分发，平台相关的处理都在各自的平台文件里                     */
/*                                                                    */
/* /<key>/api/... 的 key 来自注册表，加新平台不用改这里。                 */
/* ------------------------------------------------------------------ */

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = strings.TrimSuffix(p, "/")
	}
	if p == "" {
		p = "/"
	}
	method := strings.ToUpper(r.Method)

	switch {
	case p == "/" || p == "/index.html":
		writeHTML(w, http.StatusOK, string(consoleHTML))
		return

	case p == "/favicon.ico":
		w.WriteHeader(http.StatusNoContent)
		return

	case p == "/oauth/probe":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok"))
		return

	// 官方授权页把 code 回送到这里，浏览器跳进来带不了登录密码
	case p == "/authorize", p == "/oauth/callback":
		if method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.handleAuthorize(w, r)
		return

	case p == "/cron":
		if ok, st, msg := a.cronOK(r); !ok {
			writeErr(w, st, msg)
			return
		}
		writeJSON(w, http.StatusOK, a.runAll(r.Context(), false))
		return

	case p == "/api/ping":
		if ok, st, msg := a.adminOK(r); !ok {
			writeErr(w, st, msg)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return

	case p == "/api/platforms":
		if ok, st, msg := a.adminOK(r); !ok {
			writeErr(w, st, msg)
			return
		}
		a.handlePlatforms(w, r)
		return

	case strings.HasPrefix(p, "/dumate/extension/"):
		a.serveDumateExtension(w, r, strings.TrimPrefix(p, "/dumate/extension/"))
		return

	case p == "/api/run-all":
		if method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if ok, st, msg := a.adminOK(r); !ok {
			writeErr(w, st, msg)
			return
		}
		writeJSON(w, http.StatusOK, a.runAll(r.Context(), r.URL.Query().Get("force") == "1"))
		return
	}

	// /<key>/api/<sub>
	if i := strings.Index(p, "/api/"); i > 0 {
		key := strings.TrimPrefix(p[:i], "/")
		if plat := a.byKey(key); plat != nil {
			a.routePlatformAPI(w, r, plat, strings.TrimPrefix(p[i:], "/api"))
			return
		}
	}
	// 单独访问 /<key> 时回控制台
	if plat := a.byKey(strings.TrimPrefix(p, "/")); plat != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	writeErr(w, http.StatusNotFound, "not found")
}

/* ------------------------------------------------------------------ */
/* 回调落地页                                                          */
/* ------------------------------------------------------------------ */

// oauthPage 通知开窗的控制台，然后自动关窗
func oauthPage(title, detail string, ok bool, label ...string) string {
	lab := ""
	if len(label) > 0 {
		lab = label[0]
	}
	payloadBytes, _ := json.Marshal(map[string]any{"type": "login-done", "ok": ok, "label": lab})
	payload := strings.ReplaceAll(string(payloadBytes), "<", "\\u003c")
	color, bg := "#0d8a61", "rgba(13,138,97,.10)"
	icon := `<path d="M4.5 12.6l4.6 4.6L19.5 6.8"/>`
	if !ok {
		color, bg = "#bf3529", "rgba(191,53,41,.10)"
		icon = `<path d="M6.5 6.5l11 11M17.5 6.5l-11 11"/>`
	}
	closeJS := ""
	if ok {
		closeJS = `setTimeout(function(){try{window.close()}catch(e){}},1400);`
	}
	return `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1"><title>` + escHTML(title) + `</title>` +
		`<style>*{box-sizing:border-box}body{margin:0;min-height:100dvh;display:grid;place-items:center;` +
		`font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;` +
		`background:#eef1f5;color:#0c141d;padding:24px}` +
		`.c{width:min(420px,92vw);background:#fff;border:1px solid rgba(15,23,42,.09);border-radius:18px;` +
		`padding:28px;text-align:center;box-shadow:0 18px 44px -24px rgba(15,23,42,.35)}` +
		`.m{width:52px;height:52px;margin:0 auto 16px;border-radius:16px;display:grid;place-items:center;` +
		`background:` + bg + `}.m svg{width:26px;height:26px;fill:none;stroke:` + color + `;stroke-width:2.6;` +
		`stroke-linecap:round;stroke-linejoin:round}h1{font-size:19px;margin:0 0 8px;letter-spacing:-.01em}` +
		`p{margin:0;color:#5e6a78;font-size:13.5px;word-break:break-word}` +
		`.tip{margin-top:18px;font-size:12px;color:#8b98a7}` +
		`a.btn{display:inline-block;margin-top:16px;padding:9px 16px;border-radius:10px;background:#0d8a61;` +
		`color:#fff;text-decoration:none;font-weight:600;font-size:13px}</style></head><body>` +
		`<div class="c"><div class="m"><svg viewBox="0 0 24 24">` + icon + `</svg></div>` +
		`<h1>` + escHTML(title) + `</h1><p>` + escHTML(detail) + `</p>` +
		`<div class="tip">` + map[bool]string{true: "窗口将自动关闭，账号已出现在列表里", false: "可以关闭此窗口，回到控制台重试"}[ok] + `</div>` +
		`<a class="btn" href="/">返回控制台</a></div>` +
		`<script>try{if(window.opener)window.opener.postMessage(` + payload + `,'*')}catch(e){}` + closeJS + `</script>` +
		`</body></html>`
}

func escHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

/* ------------------------------------------------------------------ */
/* DuMate 扩展包静态托管                                                */
/*                                                                    */
/* 目录约定（见 deliverables/dumate-signin-design-2026-11-13.md 3.6）：   */
/*   - 运行目录 cfg.ExtDir（默认 /data/extension），宿主 volume 持久化     */
/*   - 镜像内置初始版本 /opt/extension-builtin/，宿主目录为空时播种        */
/*   - GET /dumate/extension/list     → 文件清单（供控制台安装按钮）        */
/*   - GET /dumate/extension/<file>   → 下载，仅白名单扩展名，防路径穿越    */
/* ------------------------------------------------------------------ */

// 扩展包白名单扩展名：crx 为主、zip 兜底
var dumateExtWhitelist = map[string]string{
	".crx": "application/x-chrome-extension",
	".zip": "application/zip",
}

// 内置初始扩展包目录（Dockerfile COPY 的镜像内路径），var 供单测替换
var dumateBuiltinDir = "/opt/extension-builtin"

// seedDumateExtDir 宿主扩展目录为空时，从镜像内置目录复制初始扩展包。
// 目录不存在或内置目录为空都静默跳过（本地裸跑没有内置目录是常态）。
func seedDumateExtDir(extDir string) {
	entries, err := os.ReadDir(extDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				return // 已有文件，不覆盖
			}
		}
	}
	builtin, err := os.ReadDir(dumateBuiltinDir)
	if err != nil {
		return
	}
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		return
	}
	for _, e := range builtin {
		if e.IsDir() || !dumateExtAllowed(e.Name()) {
			continue
		}
		src := filepath.Join(dumateBuiltinDir, e.Name())
		dst := filepath.Join(extDir, e.Name())
		if data, rerr := os.ReadFile(src); rerr == nil {
			_ = os.WriteFile(dst, data, 0o644)
		}
	}
}

// dumateExtAllowed 文件名白名单校验：仅 .crx/.zip 后缀，拒绝路径穿越与隐藏文件。
func dumateExtAllowed(name string) bool {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") ||
		strings.HasPrefix(name, ".") {
		return false
	}
	_, ok := dumateExtWhitelist[strings.ToLower(filepath.Ext(name))]
	return ok
}

// serveDumateExtension 处理 /dumate/extension/{list,<file>}。
func (a *App) serveDumateExtension(w http.ResponseWriter, r *http.Request, rest string) {
	rest = strings.Trim(rest, "/")
	switch {
	case rest == "list":
		a.handleExtList(w, r)
	case dumateExtAllowed(rest):
		a.handleExtDownload(w, r, rest)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func (a *App) handleExtList(w http.ResponseWriter, r *http.Request) {
	if ok, st, msg := a.adminOK(r); !ok {
		writeErr(w, st, msg)
		return
	}
	seedDumateExtDir(a.cfg.ExtDir)
	entries, err := os.ReadDir(a.cfg.ExtDir)
	files := []map[string]any{}
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !dumateExtAllowed(name) {
				continue
			}
			info, ierr := e.Info()
			size := int64(0)
			if ierr == nil {
				size = info.Size()
			}
			files = append(files, map[string]any{
				"name": name,
				"size": size,
				"url":  "/dumate/extension/" + url.PathEscape(name),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (a *App) handleExtDownload(w http.ResponseWriter, r *http.Request, name string) {
	// 与 list 一致要求鉴权：浏览器 <a download> 带不了 header，用 ?token=（adminOK 支持）
	if ok, st, msg := a.adminOK(r); !ok {
		writeErr(w, st, msg)
		return
	}
	seedDumateExtDir(a.cfg.ExtDir)
	path := filepath.Join(a.cfg.ExtDir, filepath.Base(name))
	data, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "extension file not found")
		return
	}
	ct := dumateExtWhitelist[strings.ToLower(filepath.Ext(name))]
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
