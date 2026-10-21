package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

//go:embed console.html
var consoleHTML []byte

type App struct {
	cfg   *Config
	kv    *KV
	regs  []*platformEntry
	store map[string]*platStore
}

// NewApp 里注册平台，顺序就是控制台菜单顺序。
// 加一个新签到平台 = 加一个实现了 Platform 的文件 + 这里一行 registerPlatform。
func NewApp(cfg *Config) (*App, error) {
	kv, err := NewKV(cfg.DataDir + "/kv")
	if err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, kv: kv, store: map[string]*platStore{}}
	a.registerPlatform(&traePlatform{})
	a.registerPlatform(&wbPlatform{})
	a.registerPlatform(&qoderPlatform{})
	return a, nil
}

/* ---------------- 响应辅助 ---------------- */

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeHTML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func readJSONBody(r *http.Request) map[string]any {
	out := map[string]any{}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		return out
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return map[string]any{}
	}
	return out
}

/* ---------------- 访问日志（LOG_REQUESTS=1 时开启，用于排查反代） ---------------- */

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func withRequestLog(on bool, h http.Handler) http.Handler {
	if !on {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		logf("http %s %s -> %d %dms ua=%q",
			r.Method, r.URL.RequestURI(), rec.status, time.Since(started).Milliseconds(),
			truncate(r.UserAgent(), 48))
	})
}

/* ---------------- 鉴权 ---------------- */

func constEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (a *App) adminOK(r *http.Request) (bool, int, string) {
	given := r.Header.Get("X-Admin-Token")
	if given == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			given = strings.TrimSpace(auth[7:])
		}
	}
	if given == "" {
		given = r.URL.Query().Get("token")
	}
	switch {
	case a.cfg.AdminToken == "":
		return false, http.StatusInternalServerError, "未配置 ADMIN_TOKEN"
	case !constEq(given, a.cfg.AdminToken):
		return false, http.StatusUnauthorized, "登录密码不正确"
	}
	return true, 0, ""
}

func (a *App) cronOK(r *http.Request) (bool, int, string) {
	secret := a.cfg.CronSecret
	if secret == "" {
		secret = a.cfg.AdminToken
	}
	given := r.Header.Get("X-Cron-Secret")
	if given == "" {
		given = r.URL.Query().Get("key")
	}
	switch {
	case secret == "":
		return false, http.StatusInternalServerError, "未配置 CRON_SECRET / ADMIN_TOKEN"
	case !constEq(given, secret):
		return false, http.StatusUnauthorized, "口令不对"
	}
	return true, 0, ""
}
