package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

/* ------------------------------------------------------------------ */
/* 路由                                                                */
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

	// Trae 授权回调：容器自己就在这个路径上，扫码后无需人工复制
	case p == "/authorize", p == "/oauth/callback":
		if method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.handleTraeCallback(w, r)
		return

	case p == "/oauth/probe":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok"))
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

	case p == "/trae" || p == "/wb":
		http.Redirect(w, r, "/", http.StatusFound)
		return

	case strings.HasPrefix(p, "/trae/api/"):
		a.routeTraeAPI(w, r, strings.TrimPrefix(p, "/trae/api"))
		return

	case strings.HasPrefix(p, "/wb/api/"):
		a.routeWBAPI(w, r, strings.TrimPrefix(p, "/wb/api"))
		return
	}

	writeErr(w, http.StatusNotFound, "not found")
}

/* ------------------------------------------------------------------ */
/* 批量签到汇总                                                        */
/* ------------------------------------------------------------------ */

type runSummary struct {
	At       string         `json:"at"`
	Accounts int            `json:"accounts"`
	Checked  int            `json:"checked"`
	Already  int            `json:"already"`
	Failed   int            `json:"failed"`
	Cooling  int            `json:"cooling,omitempty"`
	Results  []signinResult `json:"results"`
}

func newSummary(results []signinResult) runSummary {
	s := runSummary{
		At:       bjStamp(time.Now().UnixMilli(), time.FixedZone("CST", 8*3600)),
		Accounts: len(results),
		Results:  results,
	}
	for _, r := range results {
		switch r.State {
		case "checked":
			s.Checked++
		case "already", "skipped":
			s.Already++
		case "error", "token_dead":
			s.Failed++
		default:
			s.Cooling++
		}
	}
	return s
}

func (a *App) runTrae(ctx context.Context, force bool) runSummary {
	list, err := a.traeListMeta()
	if err != nil {
		return newSummary([]signinResult{{State: "error", Msg: err.Error()}})
	}
	results := make([]signinResult, 0, len(list))
	for _, meta := range list {
		acct, err := a.traeGet(meta.ID)
		if err != nil || acct == nil {
			continue
		}
		res := a.traeSigninOne(ctx, acct, force)
		results = append(results, res)
	}
	out := newSummary(results)
	// Trae 的汇总口径里把冷却也单列
	out.Cooling = 0
	for _, r := range results {
		if r.State == "rate" || r.State == "cooldown" || r.State == "pending" {
			out.Cooling++
		}
	}
	return out
}

func (a *App) runWB(ctx context.Context, force bool) runSummary {
	list, err := a.wbListMeta()
	if err != nil {
		return newSummary([]signinResult{{State: "error", Msg: err.Error()}})
	}
	results := make([]signinResult, 0, len(list))
	for _, meta := range list {
		acct, err := a.wbGet(meta.ID)
		if err != nil || acct == nil {
			continue
		}
		results = append(results, a.wbSigninOne(ctx, acct, force))
	}
	return newSummary(results)
}

func (a *App) runAll(ctx context.Context, force bool) map[string]any {
	at := bjStamp(time.Now().UnixMilli(), time.FixedZone("CST", 8*3600))
	return map[string]any{
		"at":   at,
		"trae": a.runTrae(ctx, force),
		"wb":   a.runWB(ctx, force),
	}
}

/* ------------------------------------------------------------------ */
/* Trae API                                                            */
/* ------------------------------------------------------------------ */

func (a *App) routeTraeAPI(w http.ResponseWriter, r *http.Request, sub string) {
	// 扫码回调不走登录密码，其余都要
	if sub == "/oauth/callback" {
		a.handleTraeCallback(w, r)
		return
	}
	if ok, st, msg := a.adminOK(r); !ok {
		writeErr(w, st, msg)
		return
	}
	method := strings.ToUpper(r.Method)

	switch {
	case sub == "/list" && method == http.MethodGet:
		a.handleTraeList(w, r)
	case sub == "/run" && method == http.MethodPost:
		writeJSON(w, http.StatusOK, a.runTrae(r.Context(), r.URL.Query().Get("force") == "1"))
	case sub == "/login/start" && method == http.MethodPost:
		a.handleTraeLoginStart(w, r)
	case sub == "/login/poll" && method == http.MethodGet:
		a.handleTraeLoginPoll(w, r)
	case sub == "/login/claim" && method == http.MethodPost:
		a.handleTraeLoginClaim(w, r)
	case sub == "/accounts/remove" && method == http.MethodPost:
		a.handleTraeRemove(w, r)
	case sub == "/rename" && method == http.MethodPost:
		a.handleTraeRename(w, r)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

type traeListItem struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	LastState     string   `json:"lastState"`
	LastMsg       string   `json:"lastMsg"`
	LastRun       *string  `json:"lastRun"`
	LastOkDate    *string  `json:"lastOkDate"`
	LastCredit    *float64 `json:"lastCredit"`
	Credits       *float64 `json:"credits"`
	TotalCredit   float64  `json:"totalCredit"`
	TotalSigned   int      `json:"totalSigned"`
	CooldownUntil int64    `json:"cooldownUntil"`
	HasKeys       bool     `json:"hasKeys"`
	HasDevice     bool     `json:"hasDevice"`
	Today         string   `json:"today"`
	SignedToday   bool     `json:"signedToday"`
	RanToday      bool     `json:"ranToday"`
}

func (a *App) handleTraeList(w http.ResponseWriter, r *http.Request) {
	list, err := a.traeListMeta()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now().UnixMilli()
	today := bjDay(now, a.cfg.Loc)
	out := make([]traeListItem, 0, len(list))
	for _, meta := range list {
		acct, err := a.traeGet(meta.ID)
		if err != nil || acct == nil {
			continue
		}
		item := traeListItem{
			ID:          acct.ID,
			Label:       acct.Label,
			LastState:   acct.LastState,
			LastMsg:     acct.LastMsg,
			LastCredit:  acct.LastCredit,
			TotalCredit: acct.TotalCredit,
			TotalSigned: acct.TotalSigned,
			HasKeys:     acct.PrivateKeyPEM != "" && acct.PublicKeyPEM != "",
			HasDevice:   strings.TrimSpace(acct.DeviceID) != "",
			Today:       today,
			SignedToday: acct.LastOkDate == today,
			RanToday:    acct.LastRun > 0 && bjDay(acct.LastRun, a.cfg.Loc) == today,
		}
		if acct.LastRun > 0 {
			s := bjStamp(acct.LastRun, a.cfg.Loc)
			item.LastRun = &s
		}
		if acct.LastOkDate != "" {
			d := acct.LastOkDate
			item.LastOkDate = &d
		}
		if acct.Credits != nil {
			v := round2(*acct.Credits)
			item.Credits = &v
		}
		if acct.CooldownUntil > now {
			item.CooldownUntil = acct.CooldownUntil
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "today": today, "accounts": out})
}

func (a *App) handleTraeLoginStart(w http.ResponseWriter, r *http.Request) {
	privPEM, pubPEM, err := genDeviceIdentity()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成设备密钥失败: "+err.Error())
		return
	}
	deviceID := genDeviceID()
	machineID := randomHex(16)
	verifier, challenge := pkcePair()
	trace := randomHex(8)

	authURL := buildAuthorizeURL("trae", clientIDIDE, trace, a.cfg.TraeCallback, deviceID, machineID, challenge)

	sess := &traeLoginSession{
		Trace:         trace,
		Line:          "trae",
		Verifier:      verifier,
		Callback:      a.cfg.TraeCallback,
		DeviceID:      deviceID,
		MachineID:     machineID,
		PrivateKeyPEM: privPEM,
		PublicKeyPEM:  pubPEM,
		ClientID:      clientIDIDE,
		Platform:      "IDE_PC",
		Status:        "pending",
		CreatedAt:     time.Now().UnixMilli(),
	}
	if err := a.putTraeLoginSession(sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"trace":    trace,
		"authUrl":  authURL,
		"callback": a.cfg.TraeCallback,
		"deviceId": deviceID,
		"line":     "trae",
		"label":    "Trae",
	})
}

func (a *App) handleTraeLoginPoll(w http.ResponseWriter, r *http.Request) {
	trace := strings.TrimSpace(r.URL.Query().Get("trace"))
	if trace == "" {
		writeErr(w, http.StatusBadRequest, "缺少 trace")
		return
	}
	sess := a.getTraeLoginSession(trace)
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    orDefault(sess.Status, "pending"),
		"label":     sess.Label,
		"error":     sess.Error,
		"accountId": sess.AccountID,
		"via":       sess.Via,
	})
}

func (a *App) handleTraeLoginClaim(w http.ResponseWriter, r *http.Request) {
	body := readJSONBody(r)
	text := strings.TrimSpace(firstNonEmpty(asString(body["url"]), asString(body["text"])))
	if text == "" {
		writeErr(w, http.StatusBadRequest, "请粘贴回调地址")
		return
	}

	u, err := url.Parse(text)
	if err != nil || u.Scheme == "" {
		tail := text
		if strings.HasPrefix(tail, "?") {
			// 只贴了 ? 后面的部分
		} else {
			tail = strings.TrimLeft(tail, "?/")
		}
		if u2, err2 := url.Parse("https://placeholder.local/" + tail); err2 == nil {
			u = u2
		} else {
			writeErr(w, http.StatusBadRequest, "这不像是回调地址")
			return
		}
	}

	p := parseCallbackParams(u)
	if p.AuthCode == "" && p.RefreshToken == "" {
		writeErr(w, http.StatusBadRequest, "地址里没找到 authCodeInfo / code，请复制完整地址（含 ? 之后的部分）")
		return
	}

	trace := p.Trace
	if trace == "" {
		trace = a.latestTraeTrace()
	}
	if trace == "" {
		writeErr(w, http.StatusBadRequest, "没有待完成的登录会话，请先点「添加账号」")
		return
	}
	sess := a.getTraeLoginSession(trace)
	if sess == nil {
		writeErr(w, http.StatusBadRequest, "登录会话已过期，请重新发起")
		return
	}

	out, err := a.completeTraeLogin(r.Context(), sess, p)
	if err != nil {
		sess.Status = "error"
		sess.Error = err.Error()
		_ = a.putTraeLoginSession(sess)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "label": out.Label, "id": out.ID, "via": out.Via})
}

func (a *App) handleTraeRemove(w http.ResponseWriter, r *http.Request) {
	body := readJSONBody(r)
	id := strings.TrimSpace(asString(body["id"]))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 id")
		return
	}
	_ = a.traeKV().Delete("acct:" + id)
	list, _ := a.traeListMeta()
	keep := make([]accountMeta, 0, len(list))
	for _, x := range list {
		if x.ID != id {
			keep = append(keep, x)
		}
	}
	_ = a.traeSaveIndex(keep)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": id})
}

func (a *App) handleTraeRename(w http.ResponseWriter, r *http.Request) {
	body := readJSONBody(r)
	id := strings.TrimSpace(asString(body["id"]))
	label := strings.TrimSpace(truncate(asString(body["label"]), 40))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 id")
		return
	}
	if label == "" {
		writeErr(w, http.StatusBadRequest, "备注不能为空")
		return
	}
	acct, err := a.traeGet(id)
	if err != nil || acct == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	acct.Label = label
	if err := a.traePut(acct); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	list, _ := a.traeListMeta()
	for i := range list {
		if list[i].ID == id {
			list[i].Label = label
		}
	}
	_ = a.traeSaveIndex(list)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "label": label})
}

/* ------------------------------------------------------------------ */
/* Trae 授权回调（浏览器直接跳进来，带不了登录密码）                      */
/* ------------------------------------------------------------------ */

func (a *App) handleTraeCallback(w http.ResponseWriter, r *http.Request) {
	p := parseCallbackParams(r.URL)

	// 无凭据也无 trace：多半是官网在探测「客户端是否在线」，直接 200
	if p.AuthCode == "" && p.RefreshToken == "" && p.Error == "" && p.Trace == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok"))
		return
	}

	if p.Error != "" {
		writeHTML(w, http.StatusBadRequest, oauthPage("授权未完成", p.Error, false))
		return
	}

	trace := p.Trace
	if trace == "" {
		trace = a.latestTraeTrace()
	}
	if trace == "" {
		writeHTML(w, http.StatusBadRequest, oauthPage("登录会话丢失", "找不到对应的登录会话，请回到控制台重新发起", false))
		return
	}
	sess := a.getTraeLoginSession(trace)
	if sess == nil {
		writeHTML(w, http.StatusBadRequest, oauthPage("登录会话已过期", "二维码有效期 15 分钟，请回到控制台重新获取", false))
		return
	}

	out, err := a.completeTraeLogin(r.Context(), sess, p)
	if err != nil {
		sess.Status = "error"
		sess.Error = err.Error()
		_ = a.putTraeLoginSession(sess)
		writeHTML(w, http.StatusBadRequest, oauthPage("换取令牌失败", err.Error(), false))
		return
	}
	title := "登录成功"
	if out.Repeat {
		title = "这个二维码已经用过了"
	}
	writeHTML(w, http.StatusOK, oauthPage(title, "已保存为「"+out.Label+"」", true, out.Label))
}

// oauthPage 回调落地页：通知开窗的控制台，然后自动关窗
func oauthPage(title, detail string, ok bool, label ...string) string {
	lab := ""
	if len(label) > 0 {
		lab = label[0]
	}
	payloadBytes, _ := json.Marshal(map[string]any{"type": "trae-login", "ok": ok, "label": lab})
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
/* WorkBuddy API                                                       */
/* ------------------------------------------------------------------ */

func (a *App) routeWBAPI(w http.ResponseWriter, r *http.Request, sub string) {
	if ok, st, msg := a.adminOK(r); !ok {
		writeErr(w, st, msg)
		return
	}
	method := strings.ToUpper(r.Method)
	switch {
	case sub == "/list" && method == http.MethodGet:
		a.handleWBList(w, r)
	case sub == "/run" && method == http.MethodPost:
		writeJSON(w, http.StatusOK, a.runWB(r.Context(), r.URL.Query().Get("force") == "1"))
	case sub == "/login/start" && method == http.MethodPost:
		a.handleWBLoginStart(w, r)
	case sub == "/login/poll" && method == http.MethodGet:
		a.handleWBLoginPoll(w, r)
	case sub == "/remove" && method == http.MethodPost:
		a.handleWBRemove(w, r)
	case sub == "/rename" && method == http.MethodPost:
		a.handleWBRename(w, r)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

type wbListItem struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	LastState   string   `json:"lastState"`
	LastMsg     string   `json:"lastMsg"`
	LastRun     *string  `json:"lastRun"`
	LastOkDate  *string  `json:"lastOkDate"`
	Streak      *float64 `json:"streak"`
	Total       *float64 `json:"total"`
	TotalSigned int      `json:"totalSigned"`
}

func (a *App) handleWBList(w http.ResponseWriter, r *http.Request) {
	list, err := a.wbListMeta()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]wbListItem, 0, len(list))
	for _, meta := range list {
		acct, err := a.wbGet(meta.ID)
		if err != nil || acct == nil {
			continue
		}
		item := wbListItem{
			ID:          acct.ID,
			Label:       acct.Label,
			LastState:   acct.LastState,
			LastMsg:     acct.LastMsg,
			Streak:      acct.Streak,
			Total:       acct.Total,
			TotalSigned: acct.TotalSigned,
		}
		if acct.LastRun > 0 {
			s := bjStamp(acct.LastRun, a.cfg.Loc)
			item.LastRun = &s
		}
		if acct.LastOkDate != "" {
			d := acct.LastOkDate
			item.LastOkDate = &d
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "accounts": out})
}

func (a *App) handleWBLoginStart(w http.ResponseWriter, r *http.Request) {
	state, authURL, err := a.wbRequestLoginState(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// 记一下 state 是我们发出去的，避免有人拿别人的 state 来轮询
	_ = a.wbKV().Put("login:"+state, fmt.Sprintf("%d", time.Now().UnixMilli()), wbLogin)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": state, "authUrl": authURL})
}

func (a *App) handleWBLoginPoll(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state == "" {
		writeErr(w, http.StatusBadRequest, "缺少 state")
		return
	}
	if _, ok := a.wbKV().Get("login:" + state); !ok {
		writeErr(w, http.StatusBadRequest, "登录会话已过期，请重新获取二维码")
		return
	}
	pending, tokens, err := a.wbPollLoginToken(r.Context(), state)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if pending {
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	if strings.TrimSpace(tokens.RefreshToken) == "" {
		writeErr(w, http.StatusBadGateway, "登录成功但服务端没返回 refreshToken")
		return
	}
	acct, err := a.wbPersistAccount(r.Context(), tokens.RefreshToken, "", tokens.AccessToken)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = a.wbKV().Delete("login:" + state)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "id": acct.ID, "label": acct.Label})
}

func (a *App) handleWBRemove(w http.ResponseWriter, r *http.Request) {
	body := readJSONBody(r)
	id := strings.TrimSpace(asString(body["id"]))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 id")
		return
	}
	_ = a.wbKV().Delete("acct:" + id)
	list, _ := a.wbListMeta()
	keep := make([]accountMeta, 0, len(list))
	for _, x := range list {
		if x.ID != id {
			keep = append(keep, x)
		}
	}
	_ = a.wbSaveIndex(keep)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": id})
}

func (a *App) handleWBRename(w http.ResponseWriter, r *http.Request) {
	body := readJSONBody(r)
	id := strings.TrimSpace(asString(body["id"]))
	label := strings.TrimSpace(truncate(asString(body["label"]), 40))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 id")
		return
	}
	if label == "" {
		writeErr(w, http.StatusBadRequest, "备注不能为空")
		return
	}
	acct, err := a.wbGet(id)
	if err != nil || acct == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	acct.Label = label
	if err := a.wbPut(acct); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	list, _ := a.wbListMeta()
	for i := range list {
		if list[i].ID == id {
			list[i].Label = label
		}
	}
	_ = a.wbSaveIndex(list)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "label": label})
}
