package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	kv, err := NewKV(dir)
	if err != nil {
		t.Fatalf("KV 初始化失败: %v", err)
	}
	a := &App{
		cfg: &Config{
			MasterKey:    randomBytes(32),
			Loc:          time.FixedZone("CST", 8*3600),
			DataDir:      dir,
			AdminToken:   "test-token",
			TraeCallback: "http://127.0.0.1:17388/authorize",
		},
		kv:    kv,
		store: map[string]*platStore{},
	}
	a.registerPlatform(&traePlatform{})
	a.registerPlatform(&wbPlatform{})
	a.registerPlatform(&qoderPlatform{})
	return a
}

// 造一个本地看来「还没过期」的 accessToken，避免触发续期
func futureToken() string {
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
	return "eyJhbGciOiJub25lIn0." + payload + ".sig"
}

func newTraeAccount(t *testing.T) *TraeAccount {
	priv, pub, err := genDeviceIdentity()
	if err != nil {
		t.Fatalf("生成设备密钥失败: %v", err)
	}
	return &TraeAccount{
		ID:            "t1",
		Label:         "测试号",
		RefreshToken:  "RT-OLD",
		DeviceID:      "123456789012345",
		MachineID:     "machine-1",
		PrivateKeyPEM: priv,
		PublicKeyPEM:  pub,
		ClientID:      clientIDIDE,
		PlatformCode:  "IDE_PC",
		AccessToken:   futureToken(),
	}
}

func TestTraeSigninChecked(t *testing.T) {
	app := newTestApp(t)
	acct := newTraeAccount(t)

	var mu sync.Mutex
	var claimAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epTraeClaim:
			mu.Lock()
			claimAuth = r.Header.Get("Authorization")
			mu.Unlock()
			if r.Header.Get("x-device-id") != acct.DeviceID {
				t.Errorf("缺少 x-device-id 头")
			}
			writeTestJSON(w, map[string]any{"code": 0, "message": "ok", "data": map[string]any{"points": 100}})
		case epTraeStatus:
			writeTestJSON(w, map[string]any{"code": 0, "checked_in": true, "credits": 900, "enable": true})
		case epTraeEntitle:
			writeTestJSON(w, map[string]any{"data": map[string]any{
				"usage_summary": map[string]any{"total_amount": 1000, "consumed_amount": 100},
			}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	old := traeAPIBase
	traeAPIBase = srv.URL
	defer func() { traeAPIBase = old }()

	res := app.traeSigninOne(context.Background(), acct, false)
	if res.State != "checked" {
		t.Fatalf("状态应为 checked，实际 %s（%s）", res.State, res.Msg)
	}
	if claimAuth != "Cloud-IDE-JWT "+acct.AccessToken {
		t.Fatalf("签到鉴权头不对: %s", claimAuth)
	}
	if res.Msg != "+100 积分" {
		t.Fatalf("到账文案不对: %s", res.Msg)
	}
	if acct.Credits == nil || *acct.Credits != 900 {
		t.Fatalf("余额应为 900，实际 %v", acct.Credits)
	}
	if acct.LastOkDate != bjDay(time.Now().UnixMilli(), app.cfg.Loc) {
		t.Fatalf("lastOkDate 未写入: %s", acct.LastOkDate)
	}
	if acct.TotalSigned != 1 || acct.TotalCredit != 100 {
		t.Fatalf("累计统计不对: %+v", acct)
	}

	// 同一天再跑一次：非 force 应直接跳过
	res2 := app.traeSigninOne(context.Background(), acct, false)
	if res2.State != "skipped" {
		t.Fatalf("第二次应为 skipped，实际 %s", res2.State)
	}
}

func TestTraeSigninAlreadyAndRate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim map[string]any
		want  string
	}{
		{"already", map[string]any{"code": 9095, "message": "今日已签"}, "already"},
		{"rate", map[string]any{"code": 9074, "message": "too many"}, "rate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp(t)
			acct := newTraeAccount(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case epTraeClaim:
					writeTestJSON(w, tc.claim)
				default:
					writeTestJSON(w, map[string]any{"code": 0})
				}
			}))
			defer srv.Close()
			old := traeAPIBase
			traeAPIBase = srv.URL
			defer func() { traeAPIBase = old }()

			res := app.traeSigninOne(context.Background(), acct, true)
			if res.State != tc.want {
				t.Fatalf("状态应为 %s，实际 %s（%s）", tc.want, res.State, res.Msg)
			}
			if tc.want == "rate" && acct.CooldownUntil <= time.Now().UnixMilli() {
				t.Fatal("限流后应写入冷却时间")
			}
		})
	}
}

// 续期路径：验证设备证明的 DER 签名、请求字段，以及新令牌落盘
func TestTraeRefreshWithDeviceProof(t *testing.T) {
	app := newTestApp(t)
	acct := newTraeAccount(t)
	acct.AccessToken = "" // 强制走续期

	var proofOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epTraeExchange:
			var body struct {
				ClientID     string
				RefreshToken string
				DeviceInfo   struct{ DevicePublicKey, DeviceID string }
				DeviceProof  struct {
					Timestamp int64
					Nonce     string
					Signature string
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("续期请求体解析失败: %v", err)
			}
			if r.Header.Get("x-cloudide-token") == "" && r.Header.Values("x-cloudide-token") == nil {
				t.Errorf("缺少 x-cloudide-token 头")
			}
			canonical := strings.Join([]string{
				"POST", epTraeExchange, body.ClientID, body.RefreshToken,
				strconv.FormatInt(body.DeviceProof.Timestamp, 10), body.DeviceProof.Nonce,
			}, "\n")
			block, _ := pem.Decode([]byte(body.DeviceInfo.DevicePublicKey))
			if block == nil {
				t.Errorf("DevicePublicKey 不是 PEM")
			} else if pub, err := x509.ParsePKIXPublicKey(block.Bytes); err != nil {
				t.Errorf("DevicePublicKey 解析失败: %v", err)
			} else {
				der, _ := base64.StdEncoding.DecodeString(body.DeviceProof.Signature)
				digest := sha256.Sum256([]byte(canonical))
				proofOK = ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), digest[:], der)
			}
			writeTestJSON(w, map[string]any{"Result": map[string]any{
				"Token": "AT-NEW", "RefreshToken": "RT-NEW",
			}})
		case epTraeClaim:
			if r.Header.Get("Authorization") != "Cloud-IDE-JWT AT-NEW" {
				t.Errorf("续期后的 accessToken 没被使用: %s", r.Header.Get("Authorization"))
			}
			writeTestJSON(w, map[string]any{"code": 9095, "message": "今日已签"})
		default:
			writeTestJSON(w, map[string]any{"code": 0})
		}
	}))
	defer srv.Close()
	oldAPI, oldLegacy := traeAPIBase, traeExchangeLegacyEP
	traeAPIBase, traeExchangeLegacyEP = srv.URL, srv.URL+"/legacy"
	defer func() { traeAPIBase, traeExchangeLegacyEP = oldAPI, oldLegacy }()

	res := app.traeSigninOne(context.Background(), acct, false)
	if !proofOK {
		t.Fatal("设备证明签名验证失败（DER/canonical 串与上游不一致）")
	}
	if res.State != "already" {
		t.Fatalf("应为 already，实际 %s（%s）", res.State, res.Msg)
	}
	if acct.RefreshToken != "RT-NEW" || acct.AccessToken != "AT-NEW" {
		t.Fatalf("新令牌没有落盘: rt=%s at=%s", acct.RefreshToken, acct.AccessToken)
	}
	stored, err := app.traeGet("t1")
	if err != nil || stored == nil {
		t.Fatalf("账号应已加密存盘: %v", err)
	}
	if stored.RefreshToken != "RT-NEW" {
		t.Fatal("存盘的 refreshToken 不是轮换后的值")
	}
}

// 续期失败（令牌失效）应落到 token_dead，并提示重新扫码
func TestTraeRefreshDead(t *testing.T) {
	app := newTestApp(t)
	acct := newTraeAccount(t)
	acct.AccessToken = ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"ResponseMetadata": map[string]any{"Error": map[string]any{
				"Code": 20101, "Message": "refresh token expired",
			}},
		})
	}))
	defer srv.Close()
	oldAPI, oldLegacy := traeAPIBase, traeExchangeLegacyEP
	traeAPIBase, traeExchangeLegacyEP = srv.URL, srv.URL+"/legacy"
	defer func() { traeAPIBase, traeExchangeLegacyEP = oldAPI, oldLegacy }()

	res := app.traeSigninOne(context.Background(), acct, true)
	if res.State != "token_dead" {
		t.Fatalf("应为 token_dead，实际 %s（%s）", res.State, res.Msg)
	}
	if !strings.Contains(res.Msg, "重新扫码") {
		t.Fatalf("错误提示缺少引导: %s", res.Msg)
	}
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

/* ---------------- WorkBuddy 状态机 ---------------- */

func wbTestAccount() *WBAccount {
	return &WBAccount{ID: "w1", Label: "主号", RefreshToken: "WRT-OLD"}
}

func TestWBSigninChecked(t *testing.T) {
	app := newTestApp(t)
	acct := wbTestAccount()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token/refresh":
			if r.Header.Get("X-Refresh-Token") != "WRT-OLD" {
				t.Errorf("刷新请求没带旧 refreshToken")
			}
			writeTestJSON(w, map[string]any{"code": 0, "data": map[string]any{
				"accessToken": "WAT", "refreshToken": "WRT-NEW",
			}})
		case "/v2/billing/meter/daily-checkin":
			if r.Header.Get("Authorization") != "Bearer WAT" {
				t.Errorf("签到鉴权头不对: %s", r.Header.Get("Authorization"))
			}
			writeTestJSON(w, map[string]any{"code": 0, "data": map[string]any{"credit": 100}})
		case "/v2/billing/meter/checkin-activity-status":
			writeTestJSON(w, map[string]any{"code": 0, "data": map[string]any{
				"streak_days": 7, "total_credits": 700, "checked_in_today": true,
			}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	old := wbAPIBase
	wbAPIBase = srv.URL
	defer func() { wbAPIBase = old }()

	res := app.wbSigninOne(context.Background(), acct, false)
	if res.State != "checked" {
		t.Fatalf("应为 checked，实际 %s（%s）", res.State, res.Msg)
	}
	if res.Msg != "+100 积分" {
		t.Fatalf("到账文案不对: %s", res.Msg)
	}
	if res.Streak == nil || *res.Streak != 7 || res.Total == nil || *res.Total != 700 {
		t.Fatalf("连签/本期解析不对: %+v", res)
	}
	if acct.RefreshToken != "WRT-NEW" {
		t.Fatal("轮换后的 refreshToken 没保存")
	}
}

func TestWBSigninTokenDead(t *testing.T) {
	app := newTestApp(t)
	acct := wbTestAccount()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"code": 40001, "msg": "refresh token invalid"})
	}))
	defer srv.Close()
	old := wbAPIBase
	wbAPIBase = srv.URL
	defer func() { wbAPIBase = old }()

	res := app.wbSigninOne(context.Background(), acct, false)
	if res.State != "token_dead" {
		t.Fatalf("应为 token_dead，实际 %s（%s）", res.State, res.Msg)
	}
}

/* ---------------- 扫码 → 自动回调 → 自动入库 ---------------- */

func TestAutoCallbackFlow(t *testing.T) {
	app := newTestApp(t)

	// 假的兑换端点：真实环境是 api.trae.cn
	exchange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"Result": map[string]any{
			"Token": "AT-SCAN", "RefreshToken": "RT-SCAN",
		}})
	}))
	defer exchange.Close()
	oldAPI, oldLegacy := traeAPIBase, traeExchangeLegacyEP
	traeAPIBase, traeExchangeLegacyEP = exchange.URL, exchange.URL+"/legacy"
	defer func() { traeAPIBase, traeExchangeLegacyEP = oldAPI, oldLegacy }()

	mux := httptest.NewServer(app)
	defer mux.Close()

	// 1) 控制台点「添加账号」
	req, _ := http.NewRequest("POST", mux.URL+"/trae/api/login/start", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("发起登录失败: %v", err)
	}
	var start struct {
		Handle struct {
			ID      string `json:"id"`
			AuthURL string `json:"authUrl"`
		} `json:"handle"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()
	if start.Handle.ID == "" || !strings.Contains(start.Handle.AuthURL, "auth_callback_url=http%3A//127.0.0.1%3A17388/authorize") {
		t.Fatalf("登录会话或授权链接不对: %+v", start)
	}

	// 2) 扫码完成后浏览器被送到 127.0.0.1:17388/authorize
	cb := mux.URL + "/authorize?authCodeInfo=" +
		url.QueryEscape(`{"AuthCode":"AC-REAL"}`) + "&loginTraceID=" + start.Handle.ID
	resp2, err := http.Get(cb)
	if err != nil {
		t.Fatalf("回调请求失败: %v", err)
	}
	body := make([]byte, 4096)
	n, _ := resp2.Body.Read(body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 || !strings.Contains(string(body[:n]), "登录成功") {
		t.Fatalf("回调未成功（%d）：%s", resp2.StatusCode, string(body[:n]))
	}

	// 3) 控制台轮询到 ok
	req3, _ := http.NewRequest("GET", mux.URL+"/trae/api/login/poll?id="+start.Handle.ID, nil)
	req3.Header.Set("X-Admin-Token", "test-token")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	var poll struct {
		Status string `json:"status"`
		Label  string `json:"label"`
		Via    string `json:"via"`
	}
	_ = json.NewDecoder(resp3.Body).Decode(&poll)
	resp3.Body.Close()
	if poll.Status != "ok" || poll.Via != "DeviceInfo" || poll.Label == "" {
		t.Fatalf("轮询状态不对: %+v", poll)
	}

	// 4) 列表里出现这个账号，且不返回任何令牌
	req4, _ := http.NewRequest("GET", mux.URL+"/trae/api/list", nil)
	req4.Header.Set("X-Admin-Token", "test-token")
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatalf("拉列表失败: %v", err)
	}
	raw, _ := io.ReadAll(resp4.Body)
	resp4.Body.Close()
	if strings.Contains(string(raw), "RT-SCAN") || strings.Contains(string(raw), "PRIVATE") {
		t.Fatal("列表响应里泄露了令牌或私钥")
	}
	var list struct {
		Count    int `json:"count"`
		Accounts []struct {
			Label string `json:"label"`
			Tag   string `json:"tag"`
			Tone  string `json:"tone"`
		} `json:"accounts"`
	}
	_ = json.Unmarshal(raw, &list)
	if list.Count != 1 || list.Accounts[0].Tag == "" || list.Accounts[0].Tone == "" {
		t.Fatalf("账号没有自动入库: %s", string(raw))
	}

	// 5) 同一段回调再打一次：不能重复建号
	resp5, _ := http.Get(cb)
	resp5.Body.Close()
	req6, _ := http.NewRequest("GET", mux.URL+"/trae/api/list", nil)
	req6.Header.Set("X-Admin-Token", "test-token")
	resp6, _ := http.DefaultClient.Do(req6)
	var list2 struct {
		Count int `json:"count"`
	}
	_ = json.NewDecoder(resp6.Body).Decode(&list2)
	resp6.Body.Close()
	if list2.Count != 1 {
		t.Fatalf("回调重复触发导致重复建号: %d", list2.Count)
	}
}
