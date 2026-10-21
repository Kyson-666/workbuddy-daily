package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

/* ---------------- 注册表驱动的路由与汇总 ---------------- */

func TestPlatformRegistry(t *testing.T) {
	app := newTestApp(t)
	mux := httptest.NewServer(app)
	defer mux.Close()

	req, _ := http.NewRequest("GET", mux.URL+"/api/platforms", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("拉平台列表失败: %v", err)
	}
	defer resp.Body.Close()
	var got struct {
		Platforms []struct {
			Key     string `json:"key"`
			Label   string `json:"label"`
			Flow    string `json:"flow"`
			Caps    Caps   `json:"caps"`
			Metrics []struct {
				K     string `json:"k"`
				Label string `json:"label"`
			} `json:"metrics"`
		} `json:"platforms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got.Platforms) != 3 {
		t.Fatalf("应注册三个平台，实际 %d", len(got.Platforms))
	}
	want := []struct{ key, label, flow string }{
		{"trae", "Trae", "callback"},
		{"wb", "WorkBuddy", "link"},
		{"qoder", "Qoder", "link"},
	}
	for i, w := range want {
		if got.Platforms[i].Key != w.key || got.Platforms[i].Label != w.label || got.Platforms[i].Flow != w.flow {
			t.Fatalf("第 %d 个平台不对：%+v", i, got.Platforms[i])
		}
	}
	if !got.Platforms[0].Caps.Local || !got.Platforms[0].Caps.Cooldown {
		t.Fatalf("Trae 的能力位不对（需本机会回调 + 有限流）: %+v", got.Platforms[0].Caps)
	}
	if got.Platforms[2].Caps.Local {
		t.Fatal("Qoder 不依赖 127.0.0.1 回调，不该带 local")
	}
	if len(got.Platforms[2].Metrics) != 1 || got.Platforms[2].Metrics[0].K != "totalCredit" {
		t.Fatalf("Qoder 的指标列不对: %+v", got.Platforms[2].Metrics)
	}
}

func TestRouteDispatch(t *testing.T) {
	app := newTestApp(t)
	mux := httptest.NewServer(app)
	defer mux.Close()

	// 老路径必须保持可用：/trae/api/*、/wb/api/*
	for _, path := range []string{"/trae/api/list", "/wb/api/list", "/qoder/api/list"} {
		req, _ := http.NewRequest("GET", mux.URL+path, nil)
		req.Header.Set("X-Admin-Token", "test-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s 请求失败: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s 应返回 200，实际 %d", path, resp.StatusCode)
		}
		if !strings.Contains(string(body), `"accounts"`) {
			t.Fatalf("%s 响应结构不对: %s", path, string(body))
		}
	}

	// 没注册的平台要 404，不能落到某个平台上去
	req, _ := http.NewRequest("GET", mux.URL+"/nope/api/list", nil)
	req.Header.Set("X-Admin-Token", "test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("未注册平台应 404，实际 %d", resp.StatusCode)
	}

	// 单独访问 /<key> 回控制台
	req2, _ := http.NewRequest("GET", mux.URL+"/qoder", nil)
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp2, err := cl.Do(req2)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 302 || resp2.Header.Get("Location") != "/" {
		t.Fatalf("/qoder 应重定向到首页，实际 %d %s", resp2.StatusCode, resp2.Header.Get("Location"))
	}
}

func TestRunAllCoversEveryPlatform(t *testing.T) {
	app := newTestApp(t)

	// 三个平台各放一个账号，签到端点都答 404（够用来验证「都被跑到了」）
	if err := app.storeOf("trae").Append(&Account{ID: "t1", Label: "T", AccessToken: tokenExpIn(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := app.storeOf("wb").Append(&Account{ID: "w1", Label: "W", RefreshToken: "RT"}); err != nil {
		t.Fatal(err)
	}
	if err := app.storeOf("qoder").Append(qoderTestAccount()); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()
	oldT, oldW, oldQ := traeAPIBase, wbAPIBase, qoderAPIBase
	traeAPIBase, wbAPIBase, qoderAPIBase = srv.URL, srv.URL, srv.URL
	defer func() { traeAPIBase, wbAPIBase, qoderAPIBase = oldT, oldW, oldQ }()

	// 让每个账号都真的走到签到，不然会被「今日已完成」跳过
	summary := app.runAll(context.Background(), true)
	for _, key := range []string{"trae", "wb", "qoder"} {
		s, ok := summary[key].(runSummary)
		if !ok {
			t.Fatalf("汇总里缺 %s：%+v", key, summary)
		}
		if s.Accounts != 1 {
			t.Fatalf("%s 应跑 1 个账号，实际 %d", key, s.Accounts)
		}
	}
	if _, ok := summary["at"]; !ok {
		t.Fatal("汇总缺 at 字段")
	}
}

/* ---------------- 旧数据兼容：磁盘上的密文记录零迁移就能读 ---------------- */

// TestLegacySealedRecords 用改造前的字段布局各封存一条记录，验证新代码能直接解开。
// 这里刻意写成旧 trae.go / wb.go 里那两个结构体的样子。
func TestLegacySealedRecords(t *testing.T) {
	app := newTestApp(t)

	legacyTrae := `{"id":"t-old","label":"老 Trae 号","refreshToken":"RT-LEGACY",` +
		`"deviceId":"123456789012345","machineId":"machine-old","privateKeyPEM":"PRIV","publicKeyPEM":"PUB",` +
		`"deviceName":"PC","deviceModel":"","deviceBrand":"","deviceType":"Windows","osInfo":"Windows","osVersion":"10.0.19045",` +
		`"clientId":"ono9krqynydwx5","platformCode":"IDE_PC","source":"scan","createdAt":1700000000000,` +
		`"lastState":"checked","lastMsg":"+100 积分","lastRun":1700000000000,"lastOkDate":"2026-01-02",` +
		`"lastCredit":5,"totalCredit":500,"totalSigned":5,"credits":900,"checkedIn":true,"cooldownUntil":0,` +
		`"accessToken":"","accessExp":0,"tokenRefreshedAt":1700000000000}`

	legacyWB := `{"id":"w-old","label":"老 WB 号","refreshToken":"RT-WB","createdAt":1700000000000,` +
		`"lastState":"checked","lastMsg":"+100 积分","lastRun":1700000000000,"lastOkDate":"2026-01-02",` +
		`"streak":3,"total":300,"lastCredit":100,"totalSigned":7,"tokenRefreshedAt":1700000000000,"verified":true}`

	sealedTrae, err := sealRecord(app.cfg.MasterKey, "trae-signin/v1/account", json.RawMessage(legacyTrae))
	if err != nil {
		t.Fatal(err)
	}
	sealedWB, err := sealRecord(app.cfg.MasterKey, "wb-signin/v1/account", json.RawMessage(legacyWB))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.kv.Put("trae:acct:t-old", sealedTrae, 0); err != nil {
		t.Fatal(err)
	}
	if err := app.kv.Put("wb:acct:w-old", sealedWB, 0); err != nil {
		t.Fatal(err)
	}
	if err := app.kv.Put("trae:index:accounts", `[{"id":"t-old","label":"老 Trae 号","createdAt":1700000000000}]`, 0); err != nil {
		t.Fatal(err)
	}
	if err := app.kv.Put("wb:index:accounts", `[{"id":"w-old","label":"老 WB 号","createdAt":1700000000000}]`, 0); err != nil {
		t.Fatal(err)
	}

	traeAcct, err := app.storeOf("trae").Get("t-old")
	if err != nil || traeAcct == nil {
		t.Fatalf("旧 Trae 记录读不出来: %v", err)
	}
	if traeAcct.ID != "t-old" || traeAcct.RefreshToken != "RT-LEGACY" ||
		traeAcct.DeviceID != "123456789012345" || traeAcct.PrivateKeyPEM != "PRIV" ||
		traeAcct.ClientID != clientIDIDE || traeAcct.PlatformCode != "IDE_PC" {
		t.Fatalf("旧 Trae 字段丢了: %+v", traeAcct)
	}
	if traeAcct.Credits == nil || *traeAcct.Credits != 900 || traeAcct.TotalCredit != 500 {
		t.Fatalf("旧 Trae 统计值丢了: %+v", traeAcct)
	}
	// 旧记录里 lastCredit 是数字 5，现在统一成 *float64 也要能读出来
	if traeAcct.LastCredit == nil || *traeAcct.LastCredit != 5 {
		t.Fatalf("旧 Trae lastCredit 没读出: %v", traeAcct.LastCredit)
	}

	wbAcct, err := app.storeOf("wb").Get("w-old")
	if err != nil || wbAcct == nil {
		t.Fatalf("旧 WB 记录读不出来: %v", err)
	}
	if wbAcct.ID != "w-old" || wbAcct.RefreshToken != "RT-WB" || !wbAcct.Verified {
		t.Fatalf("旧 WB 字段丢了: %+v", wbAcct)
	}
	if wbAcct.Streak == nil || *wbAcct.Streak != 3 || wbAcct.Total == nil || *wbAcct.Total != 300 {
		t.Fatalf("旧 WB 指标丢了: %+v", wbAcct)
	}
	if wbAcct.LastCredit == nil || *wbAcct.LastCredit != 100 {
		t.Fatalf("旧 WB lastCredit 没读出: %v", wbAcct.LastCredit)
	}

	// 列表能正常渲染（说明 tag/tone 判定在新结构上成立）
	for _, p := range []Platform{&traePlatform{}, &wbPlatform{}} {
		items, _, err := app.listAccounts(p)
		if err != nil {
			t.Fatalf("%s 拉列表失败: %v", p.Key(), err)
		}
		if len(items) != 1 || items[0].Tag == "" || items[0].Tone == "" {
			t.Fatalf("%s 旧账号没进列表: %+v", p.Key(), items)
		}
		if items[0].Label != "老 Trae 号" && items[0].Label != "老 WB 号" {
			t.Fatalf("备注名不对: %s", items[0].Label)
		}
	}
}

/* ---------------- 列表不泄露凭据 ---------------- */

func TestListNeverLeaksSecrets(t *testing.T) {
	app := newTestApp(t)
	acct := qoderTestAccount()
	if err := app.qoderAppend(acct); err != nil {
		t.Fatal(err)
	}
	items, _, err := app.listAccounts(&qoderPlatform{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(items)
	for _, secret := range []string{acct.RefreshToken, acct.AccessToken} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("列表里泄露了令牌: %s", string(raw))
		}
	}
	if items[0].RefreshedAt != nil {
		if strings.Contains(*items[0].RefreshedAt, "RT-") {
			t.Fatal("续期时间串里带了令牌")
		}
	}
}
