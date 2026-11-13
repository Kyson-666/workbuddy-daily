package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* ------------------------------------------------------------------ */
/* 测试辅助                                                             */
/* ------------------------------------------------------------------ */

func dumateTestCookies() []dumateCookie {
	return []dumateCookie{
		{Name: cookieSession, Value: "sess-v", Domain: ".bce.baidu.com", Path: "/"},
		{Name: cookieSessionID, Value: "sid-v", Domain: ".bce.baidu.com", Path: "/"},
		{Name: cookieAccountID, Value: "acct-1", Domain: ".bce.baidu.com", Path: "/"},
		{Name: cookieDisplayName, Value: "%E6%B5%8B%E8%AF%95%E5%8F%B7", Domain: ".bce.baidu.com", Path: "/"},
		{Name: "BDUSS", Value: "bduss-v", Domain: ".baidu.com", Path: "/"},
	}
}

func dumateTestAccount() *Account {
	a := &Account{ID: "d1", Label: "DuMate 测试号"}
	arr := cookiesToAny(dumateTestCookies())
	a.Ext = map[string]any{"cookies": arr, "accountId": "acct-1"}
	return a
}

// dumateUpstream 假上游：可配置签到状态与 POST 行为
type dumateUpstream struct {
	issued       bool // loginBonusInfo.result.hasIssued
	totalTimes   int
	totalPoints  float64
	claimCalls   int
	claimStatus  int            // POST 的 HTTP 状态码
	claimBody    map[string]any // POST 的返回体
	tokenCalls   int
	tokenOK      bool
	tokenExpired string
}

func (u *dumateUpstream) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epDumateLoginBonusInfo:
			if r.Header.Get("Cookie") == "" {
				t.Errorf("loginBonusInfo 缺 Cookie 头")
			}
			if r.Method != http.MethodGet {
				t.Errorf("loginBonusInfo 应为 GET，收到 %s", r.Method)
			}
			// signInDays 动态：issued 时含今天，否则只含昨天——避免复读误判
			days := []string{"2026-11-12"}
			if u.issued {
				days = append(days, "2026-11-13")
			}
			writeTestJSON(w, map[string]any{
				"code": 0, "success": true,
				"result": map[string]any{
					"totalTimes":  u.totalTimes,
					"totalPoints": u.totalPoints,
					"hasIssued":   u.issued,
					"signInDays":  days,
				},
			})
		case epDumateLoginBonus:
			if r.Method != http.MethodPost {
				t.Errorf("loginBonus 应为 POST，收到 %s", r.Method)
			}
			u.claimCalls++
			if u.claimStatus != 0 && u.claimStatus != 200 {
				body, _ := json.Marshal(map[string]any{"code": "RateLimit", "message": "too many"})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(u.claimStatus)
				_, _ = w.Write(body)
				return
			}
			if u.claimBody != nil {
				writeTestJSON(w, u.claimBody)
				return
			}
			// 默认模拟成功：hasIssued 翻转
			u.issued = true
			u.totalTimes++
			u.totalPoints += 500
			writeTestJSON(w, map[string]any{"code": 0, "success": true, "result": true})
		case epDumateTokenGet:
			u.tokenCalls++
			if !u.tokenOK {
				w.WriteHeader(http.StatusUnauthorized)
				writeTestJSON(w, map[string]any{"code": 401, "message": "unauthorized"})
				return
			}
			writeTestJSON(w, map[string]any{
				"code": 0, "success": true,
				"result": map[string]any{"token": "APP-TOKEN-1", "expired_at": u.tokenExpired},
			})
		default:
			t.Errorf("未知路径 %s", r.URL.Path)
			writeTestJSON(w, map[string]any{"code": 404})
		}
	}
}

func withDumateUpstream(t *testing.T, u *dumateUpstream) func() {
	t.Helper()
	srv := httptest.NewServer(u.handler(t))
	old := dumateConsole
	dumateConsole = srv.URL
	return func() { dumateConsole = old; srv.Close() }
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
}

/* ------------------------------------------------------------------ */
/* cookie 解析                                                          */
/* ------------------------------------------------------------------ */

func TestDumateParseCookiesArray(t *testing.T) {
	got := parseDumateCookies(`[{"name":"bce-session","value":"v1"},{"name":"bce-sessionid","value":"v2"}]`)
	if len(got) != 2 || got[0].Name != "bce-session" || got[0].Value != "v1" {
		t.Fatalf("数组解析失败: %+v", got)
	}
}

func TestDumateParseCookiesWrapper(t *testing.T) {
	got := parseDumateCookies(`{"cookies":[{"name":"BDUSS","value":"x"}],"accountId":"a1"}`)
	if len(got) != 1 || got[0].Name != "BDUSS" {
		t.Fatalf("wrapper 解析失败: %+v", got)
	}
}

func TestDumateParseCookiesHeader(t *testing.T) {
	got := parseDumateCookies(`bce-session=v1; bce-sessionid=v2`)
	if len(got) != 2 || got[0].Value != "v1" {
		t.Fatalf("头格式解析失败: %+v", got)
	}
}

func TestDumateParseCookiesEmpty(t *testing.T) {
	if got := parseDumateCookies(``); got != nil {
		t.Fatalf("空输入应返回 nil: %+v", got)
	}
	if got := parseDumateCookies(`not json at all`); got != nil {
		t.Fatalf("非法输入应返回 nil: %+v", got)
	}
}

/* ------------------------------------------------------------------ */
/* 签到状态机矩阵                                                       */
/* ------------------------------------------------------------------ */

// A 首次签到成功（+500）
func TestDumateSigninChecked(t *testing.T) {
	app := newTestApp(t)
	acct := dumateTestAccount()
	mustNoErr(t, app.dumateAppend(acct))

	u := &dumateUpstream{issued: false, totalTimes: 1, totalPoints: 500}
	defer withDumateUpstream(t, u)()

	res := app.dumateSigninOne(context.Background(), acct, false)
	if res.State != "checked" {
		t.Fatalf("期望 checked，得到 %s（%s）", res.State, res.Msg)
	}
	if res.Points == nil || *res.Points != 500 {
		t.Fatalf("期望 +500，得到 %v", res.Points)
	}
	if acct.LastOkDate == "" || acct.TotalSigned != 1 || acct.TotalCredit != 500 {
		t.Fatalf("状态落盘不对: okDate=%q signed=%d credit=%v", acct.LastOkDate, acct.TotalSigned, acct.TotalCredit)
	}
	if u.claimCalls != 1 {
		t.Fatalf("POST 应只调一次，实际 %d", u.claimCalls)
	}
}

// B 当日重复签到 → skipped，不打上游
func TestDumateSigninSkippedToday(t *testing.T) {
	app := newTestApp(t)
	acct := dumateTestAccount()
	acct.LastOkDate = bjDay(time.Now().UnixMilli(), app.cfg.Loc)
	mustNoErr(t, app.dumateAppend(acct))

	u := &dumateUpstream{}
	defer withDumateUpstream(t, u)()

	res := app.dumateSigninOne(context.Background(), acct, false)
	if res.State != "skipped" {
		t.Fatalf("期望 skipped，得到 %s", res.State)
	}
	if u.claimCalls != 0 {
		t.Fatalf("skip 时不应调 POST")
	}
}

// C hasIssued=true → already（硬规则：不 POST）
func TestDumateSigninAlreadyIssued(t *testing.T) {
	app := newTestApp(t)
	acct := dumateTestAccount()
	mustNoErr(t, app.dumateAppend(acct))

	u := &dumateUpstream{issued: true, totalTimes: 2, totalPoints: 1000}
	defer withDumateUpstream(t, u)()

	res := app.dumateSigninOne(context.Background(), acct, false)
	if res.State != "already" {
		t.Fatalf("期望 already，得到 %s", res.State)
	}
	if u.claimCalls != 0 {
		t.Fatalf("hasIssued=true 不应调 POST（硬规则），实际 %d 次", u.claimCalls)
	}
}

// C2 POST 报错但复读确认已签（客户端抢先）→ already
func TestDumateSigninPreempted(t *testing.T) {
	app := newTestApp(t)
	acct := dumateTestAccount()
	mustNoErr(t, app.dumateAppend(acct))

	// POST 返回上游「重复签到」形态：HTTP 200 + 字符串 code
	u := &dumateUpstream{issued: false, totalTimes: 1, totalPoints: 500,
		claimBody: map[string]any{"success": false, "code": "InternalServerError", "message": map[string]any{"global": "服务器内部错误"}}}
	defer withDumateUpstream(t, u)()

	res := app.dumateSigninOne(context.Background(), acct, false)
	// 我们的假上游没有翻转 hasIssued，复读确认未签 → error（重试耗尽）
	if res.State != "error" {
		t.Fatalf("复读确认未签时期望 error，得到 %s（%s）", res.State, res.Msg)
	}
	// 换成 POST 后翻转（抢先完成）：复读确认已签 → already
	u2 := &dumateUpstream{issued: false, totalTimes: 1, totalPoints: 500,
		claimBody: map[string]any{"success": false, "code": "InternalServerError", "message": map[string]any{"global": "服务器内部错误"}}}
	defer withDumateUpstream(t, u2)()
	// 让假上游在 POST 时翻转状态
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epDumateLoginBonus:
			u2.claimCalls++
			u2.issued = true // 模拟客户端其实签上了
			writeTestJSON(w, u2.claimBody)
		case epDumateLoginBonusInfo:
			writeTestJSON(w, map[string]any{"code": 0, "success": true, "result": map[string]any{
				"totalTimes": 2, "totalPoints": 1000, "hasIssued": u2.issued, "signInDays": []string{"2026-11-13"}}})
		}
	}))
	old := dumateConsole
	dumateConsole = srv.URL
	defer func() { dumateConsole = old; srv.Close() }()

	res2 := app.dumateSigninOne(context.Background(), acct, false)
	if res2.State != "already" {
		t.Fatalf("复读确认已签时期望 already，得到 %s（%s）", res2.State, res2.Msg)
	}
}

// D cookie 缺失 → token_dead
func TestDumateSigninNoCookie(t *testing.T) {
	app := newTestApp(t)
	acct := &Account{ID: "d2", Label: "无 cookie"} // 无 Ext
	mustNoErr(t, app.dumateAppend(acct))

	u := &dumateUpstream{}
	defer withDumateUpstream(t, u)()

	res := app.dumateSigninOne(context.Background(), acct, false)
	if res.State != "token_dead" {
		t.Fatalf("期望 token_dead，得到 %s", res.State)
	}
}

// E 429 限流 → rate + 冷却
func TestDumateSigninRateLimited(t *testing.T) {
	app := newTestApp(t)
	acct := dumateTestAccount()
	mustNoErr(t, app.dumateAppend(acct))

	u := &dumateUpstream{claimStatus: http.StatusTooManyRequests}
	defer withDumateUpstream(t, u)()

	res := app.dumateSigninOne(context.Background(), acct, false)
	if res.State != "rate" {
		t.Fatalf("期望 rate，得到 %s（%s）", res.State, res.Msg)
	}
	if acct.CooldownUntil == 0 {
		t.Fatalf("限流应设置冷却时间")
	}
	if u.claimCalls != 1 {
		t.Fatalf("429 不应重试，实际 POST %d 次", u.claimCalls)
	}
}

// F 签到端点 500 → 重试后 error
func TestDumateSigninServer500(t *testing.T) {
	app := newTestApp(t)
	acct := dumateTestAccount()
	mustNoErr(t, app.dumateAppend(acct))

	u := &dumateUpstream{claimStatus: http.StatusInternalServerError}
	defer withDumateUpstream(t, u)()

	res := app.dumateSigninOne(context.Background(), acct, false)
	if res.State != "error" {
		t.Fatalf("期望 error，得到 %s", res.State)
	}
	if u.claimCalls < 2 {
		t.Fatalf("非 429 错误应重试，实际 POST %d 次", u.claimCalls)
	}
}

// G 上游 401 → token_dead
func TestDumateSigninUnauthorized(t *testing.T) {
	app := newTestApp(t)
	acct := dumateTestAccount()
	mustNoErr(t, app.dumateAppend(acct))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == epDumateLoginBonusInfo {
			w.WriteHeader(http.StatusUnauthorized)
			writeTestJSON(w, map[string]any{"code": 401})
			return
		}
		t.Errorf("401 后不应继续调 %s", r.URL.Path)
	}))
	old := dumateConsole
	dumateConsole = srv.URL
	defer func() { dumateConsole = old; srv.Close() }()

	res := app.dumateSigninOne(context.Background(), acct, false)
	if res.State != "token_dead" {
		t.Fatalf("期望 token_dead，得到 %s", res.State)
	}
}

/* ------------------------------------------------------------------ */
/* 多账号隔离                                                           */
/* ------------------------------------------------------------------ */

func TestDumateMultiAccountIsolation(t *testing.T) {
	app := newTestApp(t)
	acct1 := dumateTestAccount()
	acct2 := dumateTestAccount()
	acct2.ID = "d3"
	// 改第二账号的 accountId，验证各自独立
	arr := cookiesToAny(dumateTestCookies())
	for _, m := range arr {
		mm := m.(map[string]any)
		if mm["name"] == cookieAccountID {
			mm["value"] = "acct-2"
		}
	}
	acct2.Ext = map[string]any{"cookies": arr, "accountId": "acct-2"}
	mustNoErr(t, app.dumateAppend(acct1))
	mustNoErr(t, app.dumateAppend(acct2))

	if acct1.Ext["accountId"] == acct2.Ext["accountId"] {
		t.Fatalf("两账号 accountId 不应相同")
	}
	list, err := app.storeOf("dumate").ListMeta()
	mustNoErr(t, err)
	if len(list) != 2 {
		t.Fatalf("期望 2 个账号，得到 %d", len(list))
	}
	got1, _ := app.dumateGet(acct1.ID)
	got2, _ := app.dumateGet(acct2.ID)
	if got1 == nil || got2 == nil {
		t.Fatalf("账号读取失败")
	}
	c1 := cookieHeaderOf(app.dumateCookiesOf(got1))
	c2 := cookieHeaderOf(app.dumateCookiesOf(got2))
	if !strings.Contains(c1, "acct-1") || !strings.Contains(c2, "acct-2") {
		t.Fatalf("cookie 未隔离: %q vs %q", c1, c2)
	}
}

/* ------------------------------------------------------------------ */
/* token 换取与 Keepalive                                               */
/* ------------------------------------------------------------------ */

func TestDumateFetchToken(t *testing.T) {
	app := newTestApp(t)
	cookies := dumateTestCookies()
	u := &dumateUpstream{tokenOK: true, tokenExpired: "2026-11-14T00:00:00Z"}
	defer withDumateUpstream(t, u)()

	token, exp, acctID, err := app.dumateFetchAppToken(context.Background(), cookies)
	mustNoErr(t, err)
	if token != "APP-TOKEN-1" {
		t.Fatalf("token 错误: %q", token)
	}
	if exp == 0 {
		t.Fatalf("expired_at 应被解析")
	}
	if acctID != "acct-1" {
		t.Fatalf("accountId 应来自 cookie，得到 %q", acctID)
	}
}

func TestDumateKeepaliveRefresh(t *testing.T) {
	app := newTestApp(t)
	acct := dumateTestAccount()
	acct.AccessToken = "old-token"
	acct.AccessExp = time.Now().Add(1 * time.Hour).Unix() // 快到期，触发刷新
	mustNoErr(t, app.dumateAppend(acct))

	u := &dumateUpstream{tokenOK: true, tokenExpired: "2026-11-14T00:00:00Z"}
	defer withDumateUpstream(t, u)()

	mustNoErr(t, app.dumatePlatformKeepalive(acct))
	if acct.AccessToken != "APP-TOKEN-1" {
		t.Fatalf("Keepalive 应更新 token，得到 %q", acct.AccessToken)
	}
	// 存回盘验证
	got, err := app.dumateGet(acct.ID)
	mustNoErr(t, err)
	if got.AccessToken != "APP-TOKEN-1" {
		t.Fatalf("token 应已落盘")
	}
}

func TestDumateKeepaliveSkipFresh(t *testing.T) {
	app := newTestApp(t)
	acct := dumateTestAccount()
	acct.AccessToken = "fresh"
	acct.AccessExp = time.Now().Add(24 * time.Hour).Unix()
	mustNoErr(t, app.dumateAppend(acct))

	u := &dumateUpstream{tokenOK: true}
	defer withDumateUpstream(t, u)()

	mustNoErr(t, app.dumatePlatformKeepalive(acct))
	if u.tokenCalls != 0 {
		t.Fatalf("令牌未到期不应刷新")
	}
}

/* ------------------------------------------------------------------ */
/* 扩展回传入库（幂等）                                                  */
/* ------------------------------------------------------------------ */

func TestDumatePersistIdempotent(t *testing.T) {
	app := newTestApp(t)
	u := &dumateUpstream{tokenOK: true, tokenExpired: "2026-11-14T00:00:00Z"}
	defer withDumateUpstream(t, u)()

	cookies := dumateTestCookies()
	first, err := app.dumatePersist(context.Background(), cookies)
	mustNoErr(t, err)
	second, err := app.dumatePersist(context.Background(), cookies)
	mustNoErr(t, err)
	if first.ID != second.ID {
		t.Fatalf("同账号重复登录应复用既有账号: %s vs %s", first.ID, second.ID)
	}
	list, _ := app.storeOf("dumate").ListMeta()
	if len(list) != 1 {
		t.Fatalf("同账号应只存一条，实际 %d", len(list))
	}
}

func TestDumateCompleteLogin(t *testing.T) {
	app := newTestApp(t)
	u := &dumateUpstream{tokenOK: true, tokenExpired: "2026-11-14T00:00:00Z"}
	defer withDumateUpstream(t, u)()

	p := &dumatePlatform{}
	// 数组形态
	out, err := p.CompleteLogin(context.Background(), app, "", LoginInput{Text: `[{"name":"bce-session","value":"s"},{"name":"bce-sessionid","value":"i"},{"name":"bce-login-accountid","value":"acct-1"},{"name":"bce-login-display-name","value":"%E5%AF%92%E6%9C%88"}]`})
	mustNoErr(t, err)
	if out.ID == "" || out.Label == "" {
		t.Fatalf("登录结果缺失: %+v", out)
	}
	// 头格式兜底
	out2, err := p.CompleteLogin(context.Background(), app, "", LoginInput{Text: `bce-session=s2; bce-sessionid=i2; bce-login-accountid=acct-1`})
	mustNoErr(t, err)
	if out2.ID != out.ID {
		t.Fatalf("头格式登录应复用同账号: %s vs %s", out2.ID, out.ID)
	}
	// 无 cookie 报错
	if _, err := p.CompleteLogin(context.Background(), app, "", LoginInput{Text: "garbage"}); err == nil {
		t.Fatalf("垃圾输入应报错")
	}
}

func TestDumateErrTextStringCode(t *testing.T) {
	res := httpResult{
		Status: 200,
		JSON: map[string]any{
			"success": false,
			"code":    "InternalServerError",
			"message": map[string]any{"global": "服务器内部错误"},
		},
	}
	msg := dumateErrText(res, "签到未成功")
	if !strings.Contains(msg, "InternalServerError") || !strings.Contains(msg, "服务器内部错误") {
		t.Fatalf("错误文本应含字符串 code 与 global 消息: %q", msg)
	}
}

/* ------------------------------------------------------------------ */
/* 平台注册表接入                                                       */
/* ------------------------------------------------------------------ */

func TestDumatePlatformRegistry(t *testing.T) {
	app := newTestApp(t)
	p := app.byKey("dumate")
	if p == nil {
		t.Fatalf("dumate 平台未注册")
	}
	if p.Key() != "dumate" || p.Label() != "DuMate" {
		t.Fatalf("平台元数据不对: %s/%s", p.Key(), p.Label())
	}
	if p.Flow() != FlowExt {
		t.Fatalf("期望 FlowExt，得到 %s", p.Flow())
	}
	if !p.Caps().Credits {
		t.Fatalf("应声明 Credits 能力")
	}
	h, err := p.StartLogin(context.Background(), app)
	mustNoErr(t, err)
	if h.ID == "" {
		t.Fatalf("登录会话 ID 为空")
	}
	var sess dumateLoginSession
	if !app.storeOf("dumate").GetSession(h.ID, &sess) {
		t.Fatalf("登录会话未落库")
	}
	if _, err := p.PollLogin(context.Background(), app, h.ID); err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	// 签到接口在注册表路由上可达（401 拦截即证明路由已挂）
	srv := httptest.NewServer(app)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/dumate/api/list")
	mustNoErr(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 请求 /dumate/api/list 期望 401，得到 %d", resp.StatusCode)
	}
}

func TestDumateTokenDeadError(t *testing.T) {
	if !isDumateTokenDead(errDumateTokenDead) {
		t.Fatalf("errors.Is 判断失败")
	}
	if isDumateTokenDead(errors.New("other")) {
		t.Fatalf("普通错误不应判为 token_dead")
	}
}

/* ------------------------------------------------------------------ */
/* 扩展托管：白名单 / 播种 / 鉴权一致性（回补自验证报告建议）              */
/* ------------------------------------------------------------------ */

func TestDumateExtAllowed(t *testing.T) {
	ok := []string{"dumate-extension.zip", "pkg.CRX", "a-b.c.zip"}
	for _, n := range ok {
		if !dumateExtAllowed(n) {
			t.Fatalf("%q 应通过白名单", n)
		}
	}
	bad := []string{"", ".hidden", "..", "a/../master.key", `a\master.key`, "master.key", "x.pem", "dir/x.zip"}
	for _, n := range bad {
		if dumateExtAllowed(n) {
			t.Fatalf("%q 应被拒绝", n)
		}
	}
}

func TestDumateSeedExtDir(t *testing.T) {
	dir := t.TempDir()
	builtin := t.TempDir()
	src := filepath.Join(builtin, "seed.zip")
	if err := os.WriteFile(src, []byte("PK-seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := dumateBuiltinDir
	dumateBuiltinDir = builtin
	defer func() { dumateBuiltinDir = old }()

	// 空目录 → 播种
	seedDumateExtDir(dir)
	seeded, err := os.ReadFile(filepath.Join(dir, "seed.zip"))
	mustNoErr(t, err)
	if string(seeded) != "PK-seed" {
		t.Fatalf("播种内容不对: %q", seeded)
	}

	// 已有文件 → 不覆盖
	if err := os.WriteFile(filepath.Join(dir, "keep.zip"), []byte("PK-keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedDumateExtDir(dir)
	keep, err := os.ReadFile(filepath.Join(dir, "keep.zip"))
	mustNoErr(t, err)
	if string(keep) != "PK-keep" {
		t.Fatalf("已有文件被覆盖: %q", keep)
	}
}

func TestDumateExtRoutesAuth(t *testing.T) {
	app := newTestApp(t)
	// 准备扩展目录与文件
	dir := t.TempDir()
	app.cfg.ExtDir = dir
	if err := os.WriteFile(filepath.Join(dir, "dumate-extension.zip"), []byte("PK-zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(app)
	defer srv.Close()
	client := &http.Client{Timeout: 5 * time.Second}

	get := func(path string) *http.Response {
		resp, err := client.Get(srv.URL + path)
		mustNoErr(t, err)
		return resp
	}

	// list 与 download 鉴权一致：无 token 均 401
	for _, path := range []string{"/dumate/extension/list", "/dumate/extension/dumate-extension.zip"} {
		resp := get(path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s 无 token 期望 401，得到 %d", path, resp.StatusCode)
		}
	}
	// 带 token 下载 200 且内容正确
	resp, err := client.Get(srv.URL + "/dumate/extension/dumate-extension.zip?token=" + app.cfg.AdminToken)
	mustNoErr(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "PK-zip" {
		t.Fatalf("带 token 下载失败: %d %q", resp.StatusCode, body)
	}
	// 带错的 token → 401
	resp, err = client.Get(srv.URL + "/dumate/extension/dumate-extension.zip?token=wrong")
	mustNoErr(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错 token 期望 401，得到 %d", resp.StatusCode)
	}
	// 穿越/白名单外 → 404（鉴权通过后按文件存在性与白名单判定）
	for _, path := range []string{"/dumate/extension/..%2fmaster.key", "/dumate/extension/master.key", "/dumate/extension/nope.zip"} {
		resp, gerr := client.Get(srv.URL + path + "?token=" + app.cfg.AdminToken)
		mustNoErr(t, gerr)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s 期望 404，得到 %d", path, resp.StatusCode)
		}
	}
}

// handleLoginClaim 路由级：FlowExt 的 {cookies:[...]} body 可绑定账号
func TestDumateClaimRouteWithCookiesBody(t *testing.T) {
	app := newTestApp(t)
	u := &dumateUpstream{tokenOK: true, tokenExpired: "2026-11-14T00:00:00Z"}
	defer withDumateUpstream(t, u)()

	srv := httptest.NewServer(app)
	defer srv.Close()

	body := fmt.Sprintf(`{"cookies":[
		{"name":"bce-session","value":"s1"},
		{"name":"bce-sessionid","value":"s2"},
		{"name":"bce-login-accountid","value":"acct-rt"},
		{"name":"bce-login-display-name","value":"%%E5%%AF%%92%%E6%%9C%%88"}]}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/dumate/api/login/claim",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", app.cfg.AdminToken)
	resp, err := http.DefaultClient.Do(req)
	mustNoErr(t, err)
	defer resp.Body.Close()
	var got struct {
		OK    bool   `json:"ok"`
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if !got.OK || got.ID == "" {
		t.Fatalf("claim 应成功: %d %+v", resp.StatusCode, got)
	}
	if got.Label != "寒月" {
		t.Fatalf("display-name 应解码: %q", got.Label)
	}
	// 落库验证（写操作复读）
	acct, err := app.dumateGet(got.ID)
	mustNoErr(t, err)
	if acct == nil || acct.Ext["accountId"] != "acct-rt" {
		t.Fatalf("账号未正确落库")
	}
}
