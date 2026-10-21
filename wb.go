package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

/* ================================================================== */
/* 模块二：WorkBuddy —— 扫码授权 / 令牌续期 / 签到                       */
/* 与 upstream/worker.js 模块二逐函数对齐，改动请同步对照                */
/* ================================================================== */

const (
	wbAPI   = "https://copilot.tencent.com"
	wbUA    = "workbuddy-signin-worker/1.0"
	wbLogin = 15 * time.Minute
)

var wbAPIBase = wbAPI

// 账户结构已上提到 platform.go 的 Account（WBAccount 是它的别名）。

type wbLoginSession struct {
	State     string `json:"state"`
	CreatedAt int64  `json:"createdAt"`
}

// 以下转发到通用存取：前缀与加密标签由 key 派生，旧的 wb: 记录零迁移就能读。

func (a *App) wbKV() scopedKV { return a.storeOf("wb").sc() }

func (a *App) wbListMeta() ([]accountMeta, error) { return a.storeOf("wb").ListMeta() }

func (a *App) wbSaveIndex(list []accountMeta) error { return a.storeOf("wb").SaveMeta(list) }

func (a *App) wbGet(id string) (*WBAccount, error) { return a.storeOf("wb").Get(id) }

func (a *App) wbPut(acct *WBAccount) error { return a.storeOf("wb").Put(acct) }

func (a *App) wbAppend(acct *WBAccount) error { return a.storeOf("wb").Append(acct) }

/* ------------------------------------------------------------------ */
/* 与 WorkBuddy 服务端交互                                              */
/* ------------------------------------------------------------------ */

func wbHeaders(base map[string]string) map[string]string {
	h := map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   wbUA,
		"X-Domain":     "copilot.tencent.com",
	}
	for k, v := range base {
		h[k] = v
	}
	return h
}

var wbAnonymous = map[string]string{
	"X-No-Authorization":   "true",
	"X-No-User-Id":         "true",
	"X-No-Enterprise-Id":   "true",
	"X-No-Department-Info": "true",
}

func (a *App) wbFetch(ctx context.Context, path, method string, headers map[string]string, body any) (httpResult, error) {
	// 上游对 POST 一律带 {} 作为 body，这里保持一致
	if method == "POST" && body == nil {
		body = map[string]any{}
	}
	res, err := doJSON(ctx, method, wbAPIBase+path, wbHeaders(headers), body, 20*time.Second)
	if err != nil {
		return httpResult{}, err
	}
	if res.JSON == nil {
		return res, fmt.Errorf("响应不是 JSON (HTTP %d): %s", res.Status, truncate(res.Text, 120))
	}
	return res, nil
}

type wbTokenError struct {
	msg string
}

func (e *wbTokenError) Error() string { return e.msg }

// wbRefreshTokens 用 refresh_token 换新的双令牌；refreshToken 会轮换，必须立刻保存新的。
func (a *App) wbRefreshTokens(ctx context.Context, refreshToken string) (tokenPair, error) {
	res, err := a.wbFetch(ctx, "/v2/plugin/auth/token/refresh", "POST", map[string]string{
		"X-Refresh-Token":       refreshToken,
		"X-Auth-Refresh-Source": "plugin",
	}, nil)
	if err != nil {
		return tokenPair{}, err
	}
	if code := asFloat0(jget(res.JSON, "code")); code != 0 || jget(res.JSON, "data") == nil {
		return tokenPair{}, &wbTokenError{msg: fmt.Sprintf("刷新令牌失败 code=%s %s (HTTP %d)",
			fmtCode(code), asString(jget(res.JSON, "msg")), res.Status)}
	}
	d := jget(res.JSON, "data")
	at := asString(jget(d, "accessToken"))
	rt := asString(jget(d, "refreshToken"))
	if rt == "" {
		rt = refreshToken
	}
	if at == "" {
		return tokenPair{}, &wbTokenError{msg: "刷新令牌失败：响应里没有 accessToken"}
	}
	return tokenPair{AccessToken: at, RefreshToken: rt}, nil
}

// wbRequestLoginState 扫码登录第一步：向服务端申请一个登录态（官方登录页 + state）
func (a *App) wbRequestLoginState(ctx context.Context) (state, authURL string, err error) {
	res, err := a.wbFetch(ctx, "/v2/plugin/auth/state?platform=workbuddy", "POST", wbAnonymous, nil)
	if err != nil {
		return "", "", err
	}
	code := asFloat0(jget(res.JSON, "code"))
	state = asString(jget(jget(res.JSON, "data"), "state"))
	authURL = asString(jget(jget(res.JSON, "data"), "authUrl"))
	if code != 0 || state == "" {
		return "", "", fmt.Errorf("申请登录态失败 code=%s %s (HTTP %d)",
			fmtCode(code), asString(jget(res.JSON, "msg")), res.Status)
	}
	return state, authURL, nil
}

// wbPollLoginToken 扫码登录第二步：轮询 state 是否已授权（11217 = 还在等）
func (a *App) wbPollLoginToken(ctx context.Context, state string) (pending bool, tokens tokenPair, err error) {
	res, err := a.wbFetch(ctx, "/v2/plugin/auth/token?state="+url.QueryEscape(state), "GET", wbAnonymous, nil)
	if err != nil {
		return false, tokenPair{}, err
	}
	code := asFloat0(jget(res.JSON, "code"))
	if code == 11217 {
		return true, tokenPair{}, nil
	}
	if code == 0 {
		d := jget(res.JSON, "data")
		at := asString(jget(d, "accessToken"))
		if at != "" {
			return false, tokenPair{AccessToken: at, RefreshToken: asString(jget(d, "refreshToken"))}, nil
		}
	}
	return false, tokenPair{}, fmt.Errorf("轮询登录结果失败 code=%s %s (HTTP %d)",
		fmtCode(code), asString(jget(res.JSON, "msg")), res.Status)
}

// wbFetchNickname 尽量取一个昵称当备注名，取不到就算了
func (a *App) wbFetchNickname(ctx context.Context, accessToken string) string {
	res, err := a.wbFetch(ctx, "/v2/accounts", "GET", map[string]string{
		"Authorization": "Bearer " + accessToken,
	}, nil)
	if err != nil {
		return ""
	}
	list := asSlice(jget(jget(res.JSON, "data"), "accounts"))
	if list == nil {
		list = asSlice(jget(res.JSON, "data"))
	}
	if len(list) == 0 {
		return ""
	}
	first := list[0]
	return strings.TrimSpace(firstNonEmpty(asString(jget(first, "nickname")), asString(jget(first, "nickName"))))
}

func (a *App) wbPersistAccount(ctx context.Context, refreshToken, label, accessToken string) (*WBAccount, error) {
	id := randToken(9)
	name := strings.TrimSpace(truncate(label, 40))
	if name == "" && accessToken != "" {
		name = a.wbFetchNickname(ctx, accessToken)
	}
	if name == "" {
		name = "账号-" + truncate(id, 4)
	}
	now := time.Now().UnixMilli()
	acct := &WBAccount{
		ID:               id,
		Label:            name,
		RefreshToken:     refreshToken,
		CreatedAt:        now,
		LastState:        "new",
		LastMsg:          "尚未运行",
		Verified:         true,
		TokenRefreshedAt: now,
	}
	if err := a.wbAppend(acct); err != nil {
		return nil, err
	}
	return acct, nil
}

type wbStatus struct {
	OK   bool
	Data any
	Code float64
	Msg  string
}

func (a *App) wbCheckinStatus(ctx context.Context, accessToken string) wbStatus {
	res, err := a.wbFetch(ctx, "/v2/billing/meter/checkin-activity-status", "POST", map[string]string{
		"Authorization": "Bearer " + accessToken,
	}, nil)
	if err != nil {
		return wbStatus{Msg: err.Error()}
	}
	if asFloat0(jget(res.JSON, "code")) != 0 || jget(res.JSON, "data") == nil {
		return wbStatus{Code: asFloat0(jget(res.JSON, "code")), Msg: asString(jget(res.JSON, "msg"))}
	}
	return wbStatus{OK: true, Data: jget(res.JSON, "data")}
}

type wbCheckinResult struct {
	State  string
	Credit float64
	Code   float64
	Msg    string
}

func (a *App) wbDoCheckin(ctx context.Context, accessToken string) wbCheckinResult {
	res, err := a.wbFetch(ctx, "/v2/billing/meter/daily-checkin", "POST", map[string]string{
		"Authorization": "Bearer " + accessToken,
	}, nil)
	if err != nil {
		return wbCheckinResult{State: "error", Code: -1, Msg: err.Error()}
	}
	code := asFloat0(jget(res.JSON, "code"))
	switch code {
	case 0:
		credit := 100.0
		if c, ok := asFloat(jget(jget(res.JSON, "data"), "credit")); ok {
			credit = c
		}
		return wbCheckinResult{State: "checked", Credit: credit}
	case 10001:
		return wbCheckinResult{State: "already"}
	default:
		return wbCheckinResult{State: "error", Code: code, Msg: asString(jget(res.JSON, "msg"))}
	}
}

/* ------------------------------------------------------------------ */
/* 单个账号的完整签到流程                                               */
/* ------------------------------------------------------------------ */

func (a *App) wbSigninOne(ctx context.Context, acct *WBAccount, force bool) signinResult {
	now := time.Now().UnixMilli()
	today := bjDay(now, a.cfg.Loc)

	// 今天已成功过、且没要求强制，直接跳过（省一次请求，也避免无谓刷新）
	if !force && acct.LastOkDate == today && acct.LastState != "error" {
		return signinResult{ID: acct.ID, Label: acct.Label, State: "skipped", Msg: "今日已完成"}
	}

	tokens, err := a.wbRefreshTokens(ctx, acct.RefreshToken)
	if err != nil {
		state := "error"
		var te *wbTokenError
		if errors.As(err, &te) {
			state = "token_dead"
		}
		acct.LastState = state
		acct.LastMsg = err.Error()
		acct.LastRun = now
		_ = a.wbPut(acct)
		return signinResult{ID: acct.ID, Label: acct.Label, State: state, Msg: err.Error()}
	}

	// 先落盘新令牌。哪怕后面签到失败，令牌链也不能断。
	acct.RefreshToken = tokens.RefreshToken
	acct.TokenRefreshedAt = now

	result := a.wbDoCheckin(ctx, tokens.AccessToken)

	var streak, total *float64
	if st := a.wbCheckinStatus(ctx, tokens.AccessToken); st.OK {
		if v, ok := asFloat(jget(st.Data, "streak_days")); ok {
			streak = &v
		}
		if v, ok := asFloat(jget(st.Data, "total_credits")); ok {
			total = &v
		}
		if asBool(jget(st.Data, "checked_in_today")) && result.State == "error" {
			result = wbCheckinResult{State: "already"}
		}
	}

	acct.LastRun = now
	acct.Streak = streak
	acct.Total = total

	if result.State == "error" {
		acct.LastState = "error"
		acct.LastMsg = strings.TrimSpace(fmt.Sprintf("签到失败 code=%s %s", fmtCode(result.Code), result.Msg))
	} else {
		acct.LastState = result.State
		if result.State == "checked" {
			acct.LastMsg = "+" + fmtNum(result.Credit) + " 积分"
		} else {
			acct.LastMsg = "今日已签"
		}
		acct.LastOkDate = today
		if result.State == "checked" {
			credit := result.Credit
			acct.LastCredit = &credit
			acct.TotalSigned++
		}
	}

	_ = a.wbPut(acct)
	return signinResult{ID: acct.ID, Label: acct.Label, State: acct.LastState, Msg: acct.LastMsg,
		Streak: streak, Total: total}
}

/* ================================================================== */
/* WorkBuddy 适配器                                                    */
/* ================================================================== */

type wbPlatform struct{}

func (p *wbPlatform) Key() string     { return "wb" }
func (p *wbPlatform) Label() string   { return "WorkBuddy" }
func (p *wbPlatform) Flow() LoginFlow { return FlowLink }

func (p *wbPlatform) Caps() Caps {
	return Caps{Streak: true, Credits: true}
}

func (p *wbPlatform) Metrics() []Metric {
	return []Metric{{K: "streak", Label: "连签"}, {K: "total", Label: "Credits"}}
}

func (p *wbPlatform) Describe(acct *Account, v viewCtx) (string, string, string) {
	switch {
	case v.SignedToday:
		if acct.LastState == "checked" {
			return "已签", "ok", acct.LastMsg
		}
		return "已签", "ok", ""
	case acct.LastState == "token_dead":
		return "令牌失效", "bad", acct.LastMsg
	case acct.LastState == "error" && v.RanToday:
		return "签到失败", "bad", acct.LastMsg
	}
	return "今日未签", "idle", ""
}

func (p *wbPlatform) StartLogin(ctx context.Context, a *App) (LoginHandle, error) {
	state, authURL, err := a.wbRequestLoginState(ctx)
	if err != nil {
		return LoginHandle{}, err
	}
	// 记下 state 是我们发出去的，避免有人拿别人的 state 来轮询
	if err := a.storeOf("wb").PutSession(state, &wbLoginSession{
		State:     state,
		CreatedAt: time.Now().UnixMilli(),
	}, wbLogin); err != nil {
		return LoginHandle{}, err
	}
	return LoginHandle{ID: state, AuthURL: authURL}, nil
}

func (p *wbPlatform) PollLogin(ctx context.Context, a *App, id string) (LoginPoll, error) {
	st := a.storeOf("wb")
	var sess wbLoginSession
	if !st.GetSession(id, &sess) {
		return LoginPoll{Status: "expired"}, nil
	}
	pending, tokens, err := a.wbPollLoginToken(ctx, id)
	if err != nil {
		return LoginPoll{}, err
	}
	if pending {
		return LoginPoll{Status: "pending"}, nil
	}
	if strings.TrimSpace(tokens.RefreshToken) == "" {
		return LoginPoll{}, errors.New("登录成功但服务端没返回 refreshToken")
	}
	acct, err := a.wbPersistAccount(ctx, tokens.RefreshToken, "", tokens.AccessToken)
	if err != nil {
		return LoginPoll{}, err
	}
	st.DeleteSession(id)
	return LoginPoll{Status: "ok", ID: acct.ID, Label: acct.Label}, nil
}

func (p *wbPlatform) CompleteLogin(ctx context.Context, a *App, id string, in LoginInput) (*LoginOutcome, error) {
	return nil, errors.New("WorkBuddy 在页面里扫码即可，不需要粘贴地址")
}

func (p *wbPlatform) Signin(ctx context.Context, a *App, acct *Account, force bool) signinResult {
	return a.wbSigninOne(ctx, acct, force)
}

// Keepalive 只续令牌，不触发签到。
func (p *wbPlatform) Keepalive(ctx context.Context, a *App, acct *Account) error {
	tokens, err := a.wbRefreshTokens(ctx, acct.RefreshToken)
	if err != nil {
		return err
	}
	acct.RefreshToken = tokens.RefreshToken
	acct.TokenRefreshedAt = time.Now().UnixMilli()
	return a.wbPut(acct)
}
