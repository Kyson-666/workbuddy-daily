package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

//go:embed console.html
var consoleHTML []byte

type App struct {
	cfg *Config
	kv  *KV
}

func NewApp(cfg *Config) (*App, error) {
	kv, err := NewKV(cfg.DataDir + "/kv")
	if err != nil {
		return nil, err
	}
	return &App{cfg: cfg, kv: kv}, nil
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
