package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

/* ------------------------------------------------------------------ */
/* 绑定 DuMate 时的诊断质量：粘贴容错 + 自检 + 错误信息可定位              */
/* ------------------------------------------------------------------ */

// 用户直接贴「Cookie: …」一整行的常见形态
func TestParseDumateCookiesToleratesCookieHeaderPrefix(t *testing.T) {
	got := parseDumateCookies("Cookie: bce-session=SESS; bce-sessionid=SID; bce-login-accountid=ACCT")
	if len(got) != 3 {
		t.Fatalf("应解析出 3 条，实际 %d：%+v", len(got), got)
	}
	if got[0].Name != "bce-session" {
		t.Fatalf("表头前缀没被剥掉，首条 name=%q", got[0].Name)
	}
	if !cookieHas(got, cookieSessionID) || !cookieHas(got, cookieAccountID) {
		t.Fatalf("关键 cookie 解析不全：%+v", got)
	}
}

// DevTools「Copy as cURL (bash)」：Cookie 以 -H 'Cookie: …' 出现
func TestParseDumateCookiesFromCurlHeaderForm(t *testing.T) {
	cmd := `curl 'https://console.bce.baidu.com/api/dumate/points/loginBonusInfo' \
  -H 'accept: application/json, text/plain, */*' \
  -H 'cookie: bce-session=SESS; bce-sessionid=SID; bce-login-accountid=ACCT' \
  -H 'referer: https://console.bce.baidu.com/dumate/'`
	got := parseDumateCookies(cmd)
	if len(got) != 3 {
		t.Fatalf("cURL 形态应解析出 3 条，实际 %d：%+v", len(got), got)
	}
	if !cookieHas(got, cookieSession) || !cookieHas(got, cookieSessionID) {
		t.Fatalf("cURL 形态缺关键 cookie：%+v", got)
	}
	// 不得把 -H 之类参数混进 cookie 名：只允许出现预期的三个名字
	want := map[string]bool{cookieSession: true, cookieSessionID: true, cookieAccountID: true}
	for _, c := range got {
		if !want[c.Name] {
			t.Fatalf("cookie 名被污染：%q", c.Name)
		}
	}
}

// DevTools 另一形态：Cookie 以 -b '…' 出现
func TestParseDumateCookiesFromCurlFlagForm(t *testing.T) {
	cmd := `curl 'https://console.bce.baidu.com/api/dumate/points/loginBonusInfo' -b 'bce-session=SESS; bce-sessionid=SID' -H 'accept: */*'`
	got := parseDumateCookies(cmd)
	if len(got) != 2 || !cookieHas(got, cookieSession) || !cookieHas(got, cookieSessionID) {
		t.Fatalf("-b 形态解析失败：%+v", got)
	}
}

// JSON 数组形态不能被新增的容错逻辑破坏
func TestParseDumateCookiesJsonArrayStillWorks(t *testing.T) {
	got := parseDumateCookies(`[{"name":"bce-session","value":"v1"},{"name":"bce-sessionid","value":"v2"}]`)
	if len(got) != 2 || got[0].Name != "bce-session" {
		t.Fatalf("JSON 数组解析回归：%+v", got)
	}
}

// 自检：完全没解析出 cookie
func TestDumateCheckCookieSetEmptyInput(t *testing.T) {
	err := dumateCheckCookieSet(nil)
	if err == nil {
		t.Fatal("空输入应当报错")
	}
	if !strings.Contains(err.Error(), "解析出任何 cookie") {
		t.Fatalf("应给出可操作提示，实际：%s", err.Error())
	}
}

// 自检：缺 bce-session/bce-sessionid 时，要说清缺哪条 + 实际拿到了哪几条
func TestDumateCheckCookieSetNamesWhatIsMissing(t *testing.T) {
	cookies := []dumateCookie{{Name: "BDUSS", Value: "x"}, {Name: "HMACCOUNT", Value: "y"}}
	err := dumateCheckCookieSet(cookies)
	if err == nil {
		t.Fatal("缺关键 cookie 应当报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, cookieSession) || !strings.Contains(msg, cookieSessionID) {
		t.Fatalf("应点名缺哪两条，实际：%s", msg)
	}
	if !strings.Contains(msg, "BDUSS") {
		t.Fatalf("应列出实际解析到的 cookie 名，实际：%s", msg)
	}
}

// 自检不通过时直接失败，不打上游（省一次无意义的请求）
func TestDumatePersistRejectsIncompleteCookieWithoutUpstream(t *testing.T) {
	app := newTestApp(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		writeTestJSON(w, map[string]any{"code": 0})
	}))
	defer srv.Close()
	old := dumateConsole
	dumateConsole = srv.URL
	defer func() { dumateConsole = old }()

	cookies := []dumateCookie{{Name: "BDUSS", Value: "x"}}
	if _, err := app.dumatePersist(context.Background(), cookies); err == nil {
		t.Fatal("缺 bce-session 应当报错")
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("自检失败时不应请求上游，实际 %d 次", n)
	}
}

// 上游 200 + code=0 但不给 token：错误里必须带上「上游原文」，否则用户无从下手
func TestDumateFetchAppTokenNoTokenShowsUpstreamBody(t *testing.T) {
	app := newTestApp(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != epDumateTokenGet {
			t.Errorf("只应请求 token 端点，收到 %s", r.URL.Path)
		}
		writeTestJSON(w, map[string]any{"code": 0, "message": "", "result": map[string]any{}})
	}))
	defer srv.Close()
	old := dumateConsole
	dumateConsole = srv.URL
	defer func() { dumateConsole = old }()

	_, _, _, err := app.dumateFetchAppToken(context.Background(), dumateTestCookies())
	if err == nil {
		t.Fatal("上游没给 token 应当报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "上游没有下发 token") {
		t.Fatalf("应说明上游没下发 token，实际：%s", msg)
	}
	if !strings.Contains(msg, "result") {
		t.Fatalf("错误里应带上游原文（便于定位），实际：%s", msg)
	}
	if !strings.Contains(msg, "bce-session") {
		t.Fatalf("应给出可操作的排查方向，实际：%s", msg)
	}
}

// 复刻真实行为（2026-11-17 实测）：未认证时上游 302 跳 login.bce.baidu.com，
// Go 客户端跟随跳转后拿到 200 + 登录页 HTML。三个上游调用都必须判成 token_dead，
// 而不是抛「缺少 token 字段」这类误导错误。
func TestDumateUpstreamLoginRedirectIsTokenDead(t *testing.T) {
	app := newTestApp(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login-page" {
			w.Header().Set("Content-Type", "text/html;charset=UTF-8")
			_, _ = w.Write([]byte("<!DOCTYPE html>\n<html><head><title>百度登录</title></head>" +
				"<body>login.bce.baidu.com</body></html>"))
			return
		}
		// 未认证 → 302 跳登录页（与真上游一致）
		http.Redirect(w, r, "/login-page", http.StatusFound)
	}))
	defer srv.Close()
	old := dumateConsole
	dumateConsole = srv.URL
	defer func() { dumateConsole = old }()

	cookies := dumateTestCookies()

	_, _, _, err := app.dumateFetchAppToken(context.Background(), cookies)
	if err == nil || !isDumateTokenDead(err) {
		t.Fatalf("token 端点被跳登录页时应判 token_dead，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "登录页") {
		t.Fatalf("错误应点明「被重定向到登录页」，实际：%s", err.Error())
	}

	if _, err := app.dumateGetBonusInfo(context.Background(), cookies); err == nil || !isDumateTokenDead(err) {
		t.Fatalf("状态端点被跳登录页时应判 token_dead，实际：%v", err)
	}
	if _, err := app.dumateClaimBonus(context.Background(), cookies); err == nil || !isDumateTokenDead(err) {
		t.Fatalf("签到端点被跳登录页时应判 token_dead，实际：%v", err)
	}

	// 绑定路径最终要给用户看到「登录态已失效」，而不是含糊的字段缺失
	_, perr := app.dumatePersist(context.Background(), cookies)
	if perr == nil || !strings.Contains(perr.Error(), "登录态已失效") {
		t.Fatalf("绑定应报「登录态已失效」，实际：%v", perr)
	}
}

// JSON 响应但没给 token（非跳转场景）不能被误判成 token_dead，仍走「没有下发 token」分支
func TestDumateJsonWithoutTokenIsNotTokenDead(t *testing.T) {
	app := newTestApp(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"code": 0, "result": map[string]any{}})
	}))
	defer srv.Close()
	old := dumateConsole
	dumateConsole = srv.URL
	defer func() { dumateConsole = old }()

	_, _, _, err := app.dumateFetchAppToken(context.Background(), dumateTestCookies())
	if err == nil {
		t.Fatal("应当报错")
	}
	if isDumateTokenDead(err) {
		t.Fatalf("JSON 响应不应被判为 token_dead，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "上游没有下发 token") {
		t.Fatalf("应走「没有下发 token」分支，实际：%s", err.Error())
	}
}

// 正向回归：cookie 齐全时绑定照常成功（粘贴路径端到端）
func TestDumatePersistWorksFromPastedCookieHeader(t *testing.T) {
	app := newTestApp(t)

	u := &dumateUpstream{tokenOK: true, tokenExpired: "2026-12-31T00:00:00Z"}
	defer withDumateUpstream(t, u)()

	pasted := "Cookie: bce-session=sess-v; bce-sessionid=sid-v; bce-login-accountid=acct-1"
	acct, err := app.dumatePersist(context.Background(), parseDumateCookies(pasted))
	mustNoErr(t, err)
	if acct.ID == "" || acct.AccessToken == "" {
		t.Fatalf("绑定结果不完整：%+v", acct)
	}
	if asString(acct.Ext["accountId"]) != "acct-1" {
		t.Fatalf("accountId 未入库：%+v", acct.Ext)
	}
}
