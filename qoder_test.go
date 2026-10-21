package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// tokenExpIn 造一个 exp 在指定时长之后的 JWT，用来控制「令牌是否快到期」
func tokenExpIn(d time.Duration) string {
	return "eyJhbGciOiJub25lIn0." + b64url([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(d).Unix()))) + ".sig"
}

func qoderTestAccount() *Account {
	return &Account{
		ID:           "q1",
		Label:        "测试号",
		RefreshToken: "RT-OLD",
		AccessToken:  tokenExpIn(24 * time.Hour),
		AccessExp:    time.Now().Add(24 * time.Hour).Unix(),
	}
}

func qoderCampaignJSON(status string, amount float64) map[string]any {
	now := time.Now().Unix()
	return map[string]any{"campaigns": []any{
		map[string]any{
			"campaignId":  "c1",
			"actionType":  qoderActionBenefit,
			"claimStatus": status,
			"benefit":     map[string]any{"amount": amount},
			"startAt":     now - 3600,
			"endAt":       now + 86400,
		},
	}}
}

func TestQoderSigninChecked(t *testing.T) {
	app := newTestApp(t)
	acct := qoderTestAccount()
	if err := app.qoderAppend(acct); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}

	var claimed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == epQoderCampaigns:
			if r.Header.Get("Authorization") != "Bearer "+acct.AccessToken {
				t.Errorf("活动查询缺 Bearer 头: %q", r.Header.Get("Authorization"))
			}
			if r.Header.Get("Cosy-ClientType") != qoderTypeAPI {
				t.Errorf("活动查询缺 Cosy-ClientType")
			}
			writeTestJSON(w, qoderCampaignJSON(qoderStatusClaimable, 100))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			claimed = append(claimed, r.URL.Path)
			writeTestJSON(w, map[string]any{"data": map[string]any{
				"status":  qoderStatusClaimed,
				"benefit": map[string]any{"amount": 100},
			}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	old := qoderAPIBase
	qoderAPIBase = srv.URL
	defer func() { qoderAPIBase = old }()

	res := app.qoderSigninOne(context.Background(), acct, false)
	if res.State != "checked" {
		t.Fatalf("状态应为 checked，实际 %s（%s）", res.State, res.Msg)
	}
	if len(claimed) != 1 || !strings.Contains(claimed[0], "c1") {
		t.Fatalf("领取请求没打到正确的活动上: %v", claimed)
	}
	if res.Msg != "+100 Credits" {
		t.Fatalf("到账文案不对: %s", res.Msg)
	}
	if acct.TotalCredit != 100 || acct.TotalSigned != 1 {
		t.Fatalf("累计统计不对: %v / %d", acct.TotalCredit, acct.TotalSigned)
	}

	// 列表里应该展示累计值，且不返回令牌
	items, _, err := app.listAccounts(&qoderPlatform{})
	if err != nil {
		t.Fatalf("拉列表失败: %v", err)
	}
	if len(items) != 1 || len(items[0].Metrics) != 1 || items[0].Metrics[0].V != "100" {
		t.Fatalf("累计 Credits 没进列表: %+v", items)
	}
	if items[0].Tag != "已领" || items[0].Tone != "ok" {
		t.Fatalf("标签判定不对: %+v", items[0])
	}
}

func TestQoderSigninAlready(t *testing.T) {
	app := newTestApp(t)
	acct := qoderTestAccount()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == epQoderCampaigns {
			writeTestJSON(w, qoderCampaignJSON(qoderStatusClaimed, 100))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	old := qoderAPIBase
	qoderAPIBase = srv.URL
	defer func() { qoderAPIBase = old }()

	res := app.qoderSigninOne(context.Background(), acct, false)
	if res.State != "already" {
		t.Fatalf("状态应为 already，实际 %s（%s）", res.State, res.Msg)
	}
	if acct.LastOkDate == "" {
		t.Fatal("已领时也要记下今天，避免当天反复查")
	}
}

func TestQoderSigninPending(t *testing.T) {
	app := newTestApp(t)
	acct := qoderTestAccount()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == epQoderCampaigns {
			writeTestJSON(w, map[string]any{"campaigns": []any{}})
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	old := qoderAPIBase
	qoderAPIBase = srv.URL
	defer func() { qoderAPIBase = old }()

	res := app.qoderSigninOne(context.Background(), acct, false)
	if res.State != "pending" {
		t.Fatalf("活动未下发时应为 pending，实际 %s", res.State)
	}
	if acct.LastOkDate != "" {
		t.Fatal("活动没下发不能记成已签，否则当天就不再重试了")
	}
}

func TestQoderSigninTokenDead(t *testing.T) {
	app := newTestApp(t)
	acct := qoderTestAccount()
	acct.AccessToken = "" // 手上没有可用令牌，只能靠刷新
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"code": 401, "msg": "refresh token invalid"})
		w.WriteHeader(401)
	}))
	defer srv.Close()
	old := qoderAPIBase
	qoderAPIBase = srv.URL
	defer func() { qoderAPIBase = old }()

	res := app.qoderSigninOne(context.Background(), acct, false)
	if res.State != "token_dead" {
		t.Fatalf("应为 token_dead，实际 %s（%s）", res.State, res.Msg)
	}
}

// TestQoderRefreshesBeforeSignin 令牌快到期时先换一对，且轮换后的 refreshToken 要落盘
func TestQoderRefreshesBeforeSignin(t *testing.T) {
	app := newTestApp(t)
	acct := qoderTestAccount()
	acct.AccessToken = tokenExpIn(30 * time.Minute) // 不到 2 小时，会先刷新
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == epQoderRefresh:
			if r.Header.Get("Cosy-ClientType") != qoderTypeAPI {
				t.Errorf("刷新缺 Cosy-ClientType")
			}
			writeTestJSON(w, map[string]any{"token": tokenExpIn(24 * time.Hour), "refreshToken": "RT-NEW"})
		case r.URL.Path == epQoderCampaigns:
			if r.Header.Get("Authorization") != "Bearer "+tokenExpIn(24*time.Hour) {
				t.Errorf("没用刷新后的令牌去签到")
			}
			writeTestJSON(w, qoderCampaignJSON(qoderStatusClaimable, 100))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			writeTestJSON(w, map[string]any{"data": map[string]any{
				"status": qoderStatusClaimed, "benefit": map[string]any{"amount": 100}}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	old := qoderAPIBase
	qoderAPIBase = srv.URL
	defer func() { qoderAPIBase = old }()

	res := app.qoderSigninOne(context.Background(), acct, false)
	if res.State != "checked" {
		t.Fatalf("状态应为 checked，实际 %s（%s）", res.State, res.Msg)
	}
	if acct.RefreshToken != "RT-NEW" {
		t.Fatalf("轮换后的 refreshToken 没落盘: %s", acct.RefreshToken)
	}
	if acct.TokenRefreshedAt == 0 {
		t.Fatal("没记录续期时间")
	}
}

/* ---------------- 设备登录 → 轮询 → 入库 ---------------- */

func TestQoderDeviceLogin(t *testing.T) {
	app := newTestApp(t)

	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != epQoderPoll {
			w.WriteHeader(404)
			return
		}
		q := r.URL.Query()
		if q.Get("challenge_method") != "S256" || q.Get("nonce") == "" || q.Get("verifier") == "" {
			t.Errorf("轮询参数不全: %v", q)
		}
		if r.Header.Get("Cosy-ClientType") != qoderTypePoll {
			t.Errorf("轮询用了错误的 ClientType: %s", r.Header.Get("Cosy-ClientType"))
		}
		polls++
		if polls < 2 {
			w.WriteHeader(202) // 用户还没在页面上确认
			return
		}
		writeTestJSON(w, map[string]any{"data": map[string]any{
			"token":         tokenExpIn(24 * time.Hour),
			"refresh_token": "RT-SCAN",
			"user_id":       "u-1",
		}})
	}))
	defer srv.Close()
	oldAPI, oldWeb := qoderAPIBase, qoderWebBase
	qoderAPIBase, qoderWebBase = srv.URL, "https://qoder.example"
	defer func() { qoderAPIBase, qoderWebBase = oldAPI, oldWeb }()

	p := &qoderPlatform{}
	ctx := context.Background()

	h, err := p.StartLogin(ctx, app)
	if err != nil {
		t.Fatalf("发起登录失败: %v", err)
	}
	if h.ID == "" || !strings.Contains(h.AuthURL, "/device/selectAccounts?") {
		t.Fatalf("登录页地址不对: %s", h.AuthURL)
	}
	for _, want := range []string{"challenge=", "challenge_method=S256", "machine_id=", "nonce="} {
		if !strings.Contains(h.AuthURL, want) {
			t.Fatalf("登录页缺少 %s: %s", want, h.AuthURL)
		}
	}

	first, err := p.PollLogin(ctx, app, h.ID)
	if err != nil {
		t.Fatalf("第一次轮询失败: %v", err)
	}
	if first.Status != "pending" {
		t.Fatalf("用户还没确认时应为 pending，实际 %s", first.Status)
	}

	second, err := p.PollLogin(ctx, app, h.ID)
	if err != nil {
		t.Fatalf("第二次轮询失败: %v", err)
	}
	if second.Status != "ok" || second.ID == "" {
		t.Fatalf("轮询状态不对: %+v", second)
	}

	// 同一个 nonce 再轮询一次不能重复建号
	third, _ := p.PollLogin(ctx, app, h.ID)
	if third.Status != "ok" {
		t.Fatalf("第三次轮询应为 ok，实际 %s", third.Status)
	}
	items, _, err := app.listAccounts(p)
	if err != nil {
		t.Fatalf("拉列表失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("重复轮询导致账号数不对: %d", len(items))
	}
	if items[0].Tag != "今日未领" {
		t.Fatalf("新账号标签不对: %s", items[0].Tag)
	}

	// 过期的会话要报 expired，而不是当成新登录
	expired, err := p.PollLogin(ctx, app, "no-such-nonce")
	if err != nil {
		t.Fatalf("未知会话不该抛错: %v", err)
	}
	if expired.Status != "expired" {
		t.Fatalf("未知会话应为 expired，实际 %s", expired.Status)
	}
}

// TestQoderKeepalive 保活只刷令牌，不触发领取
func TestQoderKeepalive(t *testing.T) {
	app := newTestApp(t)
	acct := qoderTestAccount()
	acct.AccessToken = tokenExpIn(1 * time.Hour) // 6 小时内到期，会被刷新

	var hitRefresh bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == epQoderRefresh {
			hitRefresh = true
			writeTestJSON(w, map[string]any{"token": tokenExpIn(24 * time.Hour), "refreshToken": "RT-KEEP"})
			return
		}
		t.Errorf("保活不该碰 %s", r.URL.Path)
		w.WriteHeader(404)
	}))
	defer srv.Close()
	old := qoderAPIBase
	qoderAPIBase = srv.URL
	defer func() { qoderAPIBase = old }()

	p := &qoderPlatform{}
	if err := p.Keepalive(context.Background(), app, acct); err != nil {
		t.Fatalf("保活失败: %v", err)
	}
	if !hitRefresh {
		t.Fatal("令牌快到期时应该刷新一次")
	}
	if acct.RefreshToken != "RT-KEEP" {
		t.Fatalf("新令牌没落盘: %s", acct.RefreshToken)
	}

	// 令牌还很久才到期时，不应该去打扰服务端
	acct.AccessToken = tokenExpIn(48 * time.Hour)
	hitRefresh = false
	if err := p.Keepalive(context.Background(), app, acct); err != nil {
		t.Fatalf("保活失败: %v", err)
	}
	if hitRefresh {
		t.Fatal("令牌还早，不该刷新")
	}
}
