package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

/* ------------------------------------------------------------------ */
/* DuMate 独立会话保活（只读 loginBonusInfo，每 4 小时一次）               */
/* ------------------------------------------------------------------ */

// keepaliveProbe 是只读保活的假上游：分别统计 GET loginBonusInfo、POST loginBonus
// 与任何预期外请求的次数。越界调用一律判失败，防止「保活偷偷签到」或「顺手动别的平台」。
type keepaliveProbe struct {
	infoGET   int32 // GET  /api/dumate/points/loginBonusInfo
	bonusPOST int32 // POST /api/dumate/points/loginBonus
	other     int32 // 其它任何路径
	status    int   // loginBonusInfo 的 HTTP 状态码，0 = 200
}

func (p *keepaliveProbe) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epDumateLoginBonusInfo:
			if r.Method != http.MethodGet {
				t.Errorf("loginBonusInfo 只应发 GET，收到 %s", r.Method)
			}
			if r.Header.Get("Cookie") == "" {
				t.Errorf("loginBonusInfo 缺 Cookie 头")
			}
			atomic.AddInt32(&p.infoGET, 1)
			if p.status != 0 {
				w.WriteHeader(p.status)
				writeTestJSON(w, map[string]any{"code": p.status, "message": "dead"})
				return
			}
			writeTestJSON(w, map[string]any{
				"code": 0, "success": true,
				"result": map[string]any{
					"totalTimes": 1, "totalPoints": 500.0,
					"hasIssued":  true, "signInDays": []string{},
				},
			})
		case epDumateLoginBonus:
			atomic.AddInt32(&p.bonusPOST, 1)
			t.Errorf("DuMate 保活绝不能发 POST /loginBonus（那是签到）")
			writeTestJSON(w, map[string]any{"code": 0, "success": true})
		default:
			atomic.AddInt32(&p.other, 1)
			t.Errorf("DuMate 保活出现预期外请求：%s %s", r.Method, r.URL.Path)
			writeTestJSON(w, map[string]any{"code": 404})
		}
	}
}

// withProbe 把 dumateConsole 指到假上游，返回恢复函数。
func withProbe(t *testing.T, p *keepaliveProbe) func() {
	t.Helper()
	srv := httptest.NewServer(p.handler(t))
	old := dumateConsole
	dumateConsole = srv.URL
	return func() { dumateConsole = old; srv.Close() }
}

// 1) 纯只读：只发一次 GET loginBonusInfo，不发 POST，不写任何账号字段。
func TestDuMateSessionKeepaliveIsReadOnly(t *testing.T) {
	app := newTestApp(t)
	mustNoErr(t, app.dumateAppend(dumateTestAccount()))

	p := &keepaliveProbe{}
	defer withProbe(t, p)()

	app.runDuMateSessionKeepalive(context.Background())

	if got := atomic.LoadInt32(&p.infoGET); got != 1 {
		t.Fatalf("期望 1 次只读查询，实际 %d 次", got)
	}
	if got := atomic.LoadInt32(&p.bonusPOST); got != 0 {
		t.Fatalf("保活不得签到（POST loginBonus），实际 %d 次", got)
	}
	if got := atomic.LoadInt32(&p.other); got != 0 {
		t.Fatalf("出现预期外请求 %d 次", got)
	}

	after, err := app.dumateGet("d1")
	mustNoErr(t, err)
	if after.LastState != "" || after.LastMsg != "" || after.LastRun != 0 || after.LastOkDate != "" {
		t.Fatalf("只读保活不得写状态字段：state=%q msg=%q run=%d okDate=%q",
			after.LastState, after.LastMsg, after.LastRun, after.LastOkDate)
	}
	if after.TotalSigned != 0 || after.TotalCredit != 0 {
		t.Fatalf("只读保活不得改积分/次数：signed=%d credit=%v", after.TotalSigned, after.TotalCredit)
	}
}

// 2) 只动 DuMate：其它平台的账号与上游一律不被触碰。
func TestDuMateSessionKeepaliveTouchesOnlyDuMate(t *testing.T) {
	app := newTestApp(t)

	mustNoErr(t, app.dumateAppend(dumateTestAccount()))
	mustNoErr(t, app.storeOf("wb").Append(&WBAccount{ID: "w1", Label: "WB 号", RefreshToken: "rt"}))
	mustNoErr(t, app.storeOf("trae").Append(newTraeAccount(t)))
	mustNoErr(t, app.storeOf("qoder").Append(&Account{ID: "q1", Label: "Qoder 号"}))

	p := &keepaliveProbe{}
	defer withProbe(t, p)()

	// WB 上游也指到假服务器：保活一旦误碰 WB 就会打进来
	var wbHits int32
	wbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&wbHits, 1)
		t.Errorf("保活不得请求其它平台：%s %s", r.Method, r.URL.Path)
		writeTestJSON(w, map[string]any{"code": 0})
	}))
	defer wbSrv.Close()
	oldWB := wbAPIBase
	wbAPIBase = wbSrv.URL
	defer func() { wbAPIBase = oldWB }()

	app.runDuMateSessionKeepalive(context.Background())

	if got := atomic.LoadInt32(&p.infoGET); got != 1 {
		t.Fatalf("DuMate 只读查询应恰好 1 次，实际 %d", got)
	}
	if got := atomic.LoadInt32(&wbHits); got != 0 {
		t.Fatalf("WB 上游不应被请求，实际 %d 次", got)
	}

	for _, key := range []string{"wb", "trae", "qoder"} {
		list, err := app.storeOf(key).ListMeta()
		mustNoErr(t, err)
		for _, m := range list {
			acct, err := app.storeOf(key).Get(m.ID)
			mustNoErr(t, err)
			if acct.LastRun != 0 || acct.LastState != "" || acct.LastOkDate != "" {
				t.Fatalf("%s 账号被保活写入：state=%q run=%d okDate=%q",
					key, acct.LastState, acct.LastRun, acct.LastOkDate)
			}
		}
	}
}

// 3) cookie 失效（401）→ 按现状标 token_dead，且只写三个状态字段。
func TestDuMateSessionKeepaliveMarksTokenDead(t *testing.T) {
	app := newTestApp(t)
	mustNoErr(t, app.dumateAppend(dumateTestAccount()))

	p := &keepaliveProbe{status: http.StatusUnauthorized}
	defer withProbe(t, p)()

	app.runDuMateSessionKeepalive(context.Background())

	after, err := app.dumateGet("d1")
	mustNoErr(t, err)
	if after.LastState != "token_dead" {
		t.Fatalf("期望 token_dead，得到 %q", after.LastState)
	}
	if after.LastMsg == "" {
		t.Fatal("token_dead 应带「请重新登录」提示")
	}
	if after.LastRun == 0 {
		t.Fatal("token_dead 应记录 LastRun")
	}
	if after.LastOkDate != "" || after.TotalSigned != 0 || after.TotalCredit != 0 {
		t.Fatalf("除三个状态字段外不得写：okDate=%q signed=%d credit=%v",
			after.LastOkDate, after.TotalSigned, after.TotalCredit)
	}
	if atomic.LoadInt32(&p.bonusPOST) != 0 {
		t.Fatal("标记 token_dead 也不得发 POST")
	}
}

// 4) cookie 缺失 → 直接跳过：零请求、零写盘。
func TestDuMateSessionKeepaliveSkipsAccountWithoutCookie(t *testing.T) {
	app := newTestApp(t)
	mustNoErr(t, app.dumateAppend(&Account{ID: "d9", Label: "无 cookie"}))

	p := &keepaliveProbe{}
	defer withProbe(t, p)()

	app.runDuMateSessionKeepalive(context.Background())

	if atomic.LoadInt32(&p.infoGET) != 0 {
		t.Fatal("登录态不完整时不应发请求")
	}
	after, err := app.dumateGet("d9")
	mustNoErr(t, err)
	if after.LastState != "" || after.LastRun != 0 {
		t.Fatalf("跳过时不得写盘：state=%q run=%d", after.LastState, after.LastRun)
	}
}

// 5) 分派：DuMate 保活时间点只跑只读保活，绝不走签到。
func TestRunScheduledRoutesDuMateSlotToReadOnlyKeepalive(t *testing.T) {
	app := newTestApp(t)
	app.cfg.SigninSlots = []string{"08:10"}
	app.cfg.DuMateKeepaliveSlots = []string{"04:05"}

	mustNoErr(t, app.dumateAppend(dumateTestAccount()))

	p := &keepaliveProbe{}
	defer withProbe(t, p)()

	app.runScheduled("04:05")

	if got := atomic.LoadInt32(&p.infoGET); got != 1 {
		t.Fatalf("DuMate 保活时间点应恰好 1 次只读查询，实际 %d", got)
	}
	if got := atomic.LoadInt32(&p.bonusPOST); got != 0 {
		t.Fatalf("DuMate 保活时间点不得触发签到，实际 %d 次 POST", got)
	}
	after, err := app.dumateGet("d1")
	mustNoErr(t, err)
	if after.LastOkDate != "" {
		t.Fatalf("不应走签到流程（LastOkDate 被写为 %q）", after.LastOkDate)
	}
}

// 6) 分派优先级：同一时间点既在 SigninSlots 又在 DuMateKeepaliveSlots 时，签到优先。
func TestRunScheduledSigninWinsOverDuMateSlot(t *testing.T) {
	app := newTestApp(t)
	app.cfg.SigninSlots = []string{"04:05"}
	app.cfg.DuMateKeepaliveSlots = []string{"04:05"}

	mustNoErr(t, app.dumateAppend(dumateTestAccount()))

	u := &dumateUpstream{issued: true, totalTimes: 1, totalPoints: 500}
	defer withDumateUpstream(t, u)()

	app.runScheduled("04:05")

	after, err := app.dumateGet("d1")
	mustNoErr(t, err)
	if after.LastState != "already" {
		t.Fatalf("时间点配重时应以签到为准，得到 state=%q", after.LastState)
	}
}

/* ------------------------------------------------------------------ */
/* 配置解析                                                             */
/* ------------------------------------------------------------------ */

func TestDuMateKeepaliveSlotsDefaultSixPerDay(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("CRON_SCHEDULE", "")
	t.Setenv("DUMATE_KEEPALIVE_SLOTS", "") // 空 → 回落默认，即「默认生效」

	cfg, err := loadConfig()
	mustNoErr(t, err)

	want := []string{"00:05", "04:05", "08:05", "12:05", "16:05", "20:05"}
	if strings.Join(cfg.DuMateKeepaliveSlots, ",") != strings.Join(want, ",") {
		t.Fatalf("默认时间点不符：%v", cfg.DuMateKeepaliveSlots)
	}
	if gap := maxGapMinutes(cfg.DuMateKeepaliveSlots); gap != 240 {
		t.Fatalf("默认最大间隔应为 240 分钟（每 4 小时），实际 %d", gap)
	}
	if !slotIn(cfg.Schedules, "04:05") {
		t.Fatalf("默认就该进定时器：%v", cfg.Schedules)
	}
	if !slotIn(cfg.SigninSlots, "08:10") || slotIn(cfg.SigninSlots, "04:05") {
		t.Fatalf("SigninSlots 解析异常：%v", cfg.SigninSlots)
	}
}

func TestDuMateKeepaliveSlotsOffDisables(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("CRON_SCHEDULE", "")
	t.Setenv("DUMATE_KEEPALIVE_SLOTS", "off")

	cfg, err := loadConfig()
	mustNoErr(t, err)

	if len(cfg.DuMateKeepaliveSlots) != 0 {
		t.Fatalf("off 应关闭，得到 %v", cfg.DuMateKeepaliveSlots)
	}
	if slotIn(cfg.Schedules, "04:05") {
		t.Fatalf("off 后 Schedules 不该含 DuMate 时间点：%v", cfg.Schedules)
	}
	if !slotIn(cfg.Schedules, "08:10") {
		t.Fatalf("CRON_SCHEDULE 应不受影响：%v", cfg.Schedules)
	}
}

func TestDuMateKeepaliveSlotsRejectsBadFormat(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("DUMATE_KEEPALIVE_SLOTS", "04:05,25:00")

	if _, err := loadConfig(); err == nil {
		t.Fatal("非法 HH:MM 应当报错")
	}
}

// maxGapMinutes 算「含跨天那一段」的最大间隔（分钟），用于验证 24h 内最大间隔。
func maxGapMinutes(slots []string) int {
	mins := make([]int, 0, len(slots))
	for _, s := range slots {
		var h, m int
		if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
			panic("非法时间点 " + s)
		}
		mins = append(mins, h*60+m)
	}
	sort.Ints(mins)
	best := 0
	prev := mins[len(mins)-1] - 24*60
	for _, v := range mins {
		if d := v - prev; d > best {
			best = d
		}
		prev = v
	}
	return best
}
