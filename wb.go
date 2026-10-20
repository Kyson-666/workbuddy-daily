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

const (
	wbIndexKey = "index:accounts"
)

type WBAccount struct {
	ID               string   `json:"id"`
	Label            string   `json:"label"`
	RefreshToken     string   `json:"refreshToken"`
	CreatedAt        int64    `json:"createdAt"`
	LastState        string   `json:"lastState"`
	LastMsg          string   `json:"lastMsg"`
	LastRun          int64    `json:"lastRun"`
	LastOkDate       string   `json:"lastOkDate"`
	Streak           *float64 `json:"streak"`
	Total            *float64 `json:"total"`
	LastCredit       float64  `json:"lastCredit"`
	TotalSigned      int      `json:"totalSigned"`
	TokenRefreshedAt int64    `json:"tokenRefreshedAt"`
	Verified         bool     `json:"verified"`
}

func (a *App) wbKV() scopedKV { return scopedKV{kv: a.kv, prefix: "wb:"} }

func (a *App) wbListMeta() ([]accountMeta, error) {
	raw, ok := a.wbKV().Get(wbIndexKey)
	if !ok {
		return []accountMeta{}, nil
	}
	var list []accountMeta
	if err := jsonUnmarshal([]byte(raw), &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (a *App) wbSaveIndex(list []accountMeta) error {
	return a.wbKV().Put(wbIndexKey, string(mustJSON(list)), 0)
}

func (a *App) wbGet(id string) (*WBAccount, error) {
	raw, ok := a.wbKV().Get("acct:" + id)
	if !ok {
		return nil, nil
	}
	var acct WBAccount
	if err := openRecord(a.cfg.MasterKey, infoWB, raw, &acct); err != nil {
		return nil, err
	}
	return &acct, nil
}

func (a *App) wbPut(acct *WBAccount) error {
	sealed, err := sealRecord(a.cfg.MasterKey, infoWB, acct)
	if err != nil {
		return err
	}
	return a.wbKV().Put("acct:"+acct.ID, sealed, 0)
}

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
	if err := a.wbPut(acct); err != nil {
		return nil, err
	}
	list, err := a.wbListMeta()
	if err != nil {
		return nil, err
	}
	list = append(list, accountMeta{ID: id, Label: name, CreatedAt: now})
	if err := a.wbSaveIndex(list); err != nil {
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
			acct.LastCredit = result.Credit
			acct.TotalSigned++
		}
	}

	_ = a.wbPut(acct)
	return signinResult{ID: acct.ID, Label: acct.Label, State: acct.LastState, Msg: acct.LastMsg,
		Streak: streak, Total: total}
}
