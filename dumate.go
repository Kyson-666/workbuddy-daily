package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

/* ================================================================== */
/* DuMate（百度搭子）—— 浏览器扩展回传 cookie / 签到 / 积分               */
/*                                                                    */
/* 签到接口已实际拆解本地客户端确认（见 testdata/dumate-stage0-findings.md）： */
/*   - 签到与积分都走 console.bce.baidu.com，Cookie 认证，不需要 app token */
/*   - 签到 = POST /api/dumate/points/loginBonus（每日 +500）            */
/*   - 状态 = GET  /api/dumate/points/loginBonusInfo                    */
/* 上游接口变了只改这个文件顶部的常量块与解析函数。                        */
/* ================================================================== */

const (
	dumateConsoleBase = "https://console.bce.baidu.com"

	epDumateLoginBonus     = "/api/dumate/points/loginBonus"
	epDumateLoginBonusInfo = "/api/dumate/points/loginBonusInfo"
	epDumateTokenGet       = "/api/dumate/app/token/get"
	epDumateQuotaOverview  = "/api/dumate/points/quotaOverview"

	dumateUA       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"
	dumateReferer  = "https://console.bce.baidu.com/dumate/"
	dumateTimeout  = 20 * time.Second
	dumateLoginTTL = 30 * time.Minute

	// 签到奖励固定 500 积分/次（前端 totalPoints += 500 实测口径）
	dumateBonusPoints = 500.0

	// cookie 最小集：缺失时视为登录态不完整
	cookieSession     = "bce-session"
	cookieSessionID   = "bce-sessionid"
	cookieAccountID   = "bce-login-accountid"
	cookieDisplayName = "bce-login-display-name"
)

// dumateConsole 供单测指向假服务
var dumateConsole = dumateConsoleBase

// dumateCookie 是扩展回传的 cookie 数组元素，格式与 dumate-proxy 导出兼容。
type dumateCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path,omitempty"`
	Expires  float64 `json:"expires,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
}

// dumateBonusInfo 对应 loginBonusInfo.result
type dumateBonusInfo struct {
	TotalTimes  int      `json:"totalTimes"`
	TotalPoints float64  `json:"totalPoints"`
	HasIssued   bool     `json:"hasIssued"`
	SignInDays  []string `json:"signInDays"`
}

/* ------------------------------------------------------------------ */
/* cookie 与请求辅助                                                    */
/* ------------------------------------------------------------------ */

// parseDumateCookies 宽松解析扩展回传的 cookie JSON（数组或 {cookies:[...]}）。
func parseDumateCookies(text string) []dumateCookie {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	// 直接是数组
	var arr []dumateCookie
	if err := json.Unmarshal([]byte(text), &arr); err == nil && len(arr) > 0 {
		return arr
	}
	// dumate-proxy 风格：{"cookies": [...]}
	var wrap struct {
		Cookies []dumateCookie `json:"cookies"`
	}
	if err := json.Unmarshal([]byte(text), &wrap); err == nil && len(wrap.Cookies) > 0 {
		return wrap.Cookies
	}
	// 逗号分隔的 "k=v; k2=v2" 头格式兜底
	if strings.Contains(text, "=") && !strings.HasPrefix(text, "[") {
		var out []dumateCookie
		for _, part := range strings.Split(text, ";") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) == 2 && kv[0] != "" && kv[1] != "" {
				out = append(out, dumateCookie{Name: strings.TrimSpace(kv[0]), Value: strings.TrimSpace(kv[1])})
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func cookieHeaderOf(cookies []dumateCookie) string {
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		if c.Name == "" {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func cookieHas(cookies []dumateCookie, name string) bool {
	for _, c := range cookies {
		if c.Name == name {
			return true
		}
	}
	return false
}

func cookieValueOf(cookies []dumateCookie, name string) string {
	for _, c := range cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func (a *App) dumateHeaders(cookies []dumateCookie) map[string]string {
	return map[string]string{
		"Cookie":     cookieHeaderOf(cookies),
		"Accept":     "application/json, text/plain, */*",
		"User-Agent": dumateUA,
		"Referer":    dumateReferer,
	}
}

func dumateErrText(res httpResult, prefix string) string {
	// code 可能是数字（0）或字符串（InternalServerError），宽松处理
	code := asString(jget(res.JSON, "code"))
	msg := ""
	if m := jget(res.JSON, "message"); m != nil {
		if s := asString(m); s != "" {
			msg = s
		} else if g := asString(jget(m, "global")); g != "" {
			msg = g
		}
	}
	if msg == "" {
		msg = firstNonEmpty(asString(jget(res.JSON, "msg")), asString(jget(res.JSON, "error")))
	}
	if msg == "" {
		msg = truncate(res.Text, 120)
	}
	return fmt.Sprintf("%s HTTP %d code=%s %s", prefix, res.Status, code, msg)
}

/* ------------------------------------------------------------------ */
/* 上游调用                                                             */
/* ------------------------------------------------------------------ */

// dumateGetBonusInfo 查签到状态；code==0 视为成功。
func (a *App) dumateGetBonusInfo(ctx context.Context, cookies []dumateCookie) (*dumateBonusInfo, error) {
	res, err := doJSON(ctx, http.MethodGet, dumateConsole+epDumateLoginBonusInfo, a.dumateHeaders(cookies), nil, dumateTimeout)
	if err != nil {
		return nil, err
	}
	if res.Status == 401 || res.Status == 403 {
		return nil, errDumateTokenDead
	}
	if res.Status != 200 || asFloat0(jget(res.JSON, "code")) != 0 {
		return nil, errors.New(dumateErrText(res, "查询签到状态失败"))
	}
	root := jget(res.JSON, "result")
	if root == nil {
		return nil, errors.New("签到状态响应缺少 result")
	}
	info := &dumateBonusInfo{}
	if v, ok := asFloat(jget(root, "totalTimes")); ok {
		info.TotalTimes = int(v)
	}
	if v, ok := asFloat(jget(root, "totalPoints")); ok {
		info.TotalPoints = v
	}
	info.HasIssued = asBool(jget(root, "hasIssued"))
	for _, d := range asSlice(jget(root, "signInDays")) {
		if s := asString(d); s != "" {
			info.SignInDays = append(info.SignInDays, s)
		}
	}
	return info, nil
}

// dumateClaimBonus 提交签到。返回 code 与错误；上游对重复签到给字符串 code，
// 由调用方复读 loginBonusInfo 兜底判定。
func (a *App) dumateClaimBonus(ctx context.Context, cookies []dumateCookie) (int, error) {
	res, err := doJSON(ctx, http.MethodPost, dumateConsole+epDumateLoginBonus, a.dumateHeaders(cookies), map[string]any{}, dumateTimeout)
	if err != nil {
		return -1, err
	}
	if res.Status == 401 || res.Status == 403 {
		return -1, errDumateTokenDead
	}
	if res.Status != 200 {
		return -1, errors.New(dumateErrText(res, "签到请求失败"))
	}
	codeF, hasCode := asFloat(jget(res.JSON, "code"))
	success := asBool(jget(res.JSON, "success"))
	switch {
	case hasCode && codeF == 0 && success:
		return 0, nil
	case hasCode && codeF == 0 && res.JSON != nil && !hasKey(res.JSON, "success"):
		// 有的版本没有 success 字段，code==0 即成功
		return 0, nil
	default:
		return -1, errors.New(dumateErrText(res, "签到未成功"))
	}
}

// dumateFetchAppToken 用 cookie 换 app token（登录验证 + Keepalive 用）。
func (a *App) dumateFetchAppToken(ctx context.Context, cookies []dumateCookie) (token string, expiredAt int64, accountID string, err error) {
	res, err := doJSON(ctx, http.MethodGet, dumateConsole+epDumateTokenGet, a.dumateHeaders(cookies), nil, dumateTimeout)
	if err != nil {
		return "", 0, "", err
	}
	if res.Status == 401 || res.Status == 403 {
		return "", 0, "", errDumateTokenDead
	}
	if res.Status != 200 || asFloat0(jget(res.JSON, "code")) != 0 {
		return "", 0, "", errors.New(dumateErrText(res, "换取 token 失败"))
	}
	result := jget(res.JSON, "result")
	token = asString(jget(result, "token"))
	if token == "" {
		return "", 0, "", errors.New("token 响应缺少 token 字段")
	}
	// expired_at 是 ISO 字符串，解析失败则默认 8h
	if s := asString(jget(result, "expired_at")); s != "" {
		if t, perr := time.Parse(time.RFC3339, s); perr == nil {
			expiredAt = t.Unix()
		}
	}
	if expiredAt == 0 {
		expiredAt = time.Now().Unix() + 8*3600
	}
	accountID = cookieValueOf(cookies, cookieAccountID)
	return token, expiredAt, accountID, nil
}

// hasKey 判断 JSON 对象是否含有某键（code==0 且无 success 字段时的成功判定兜底）。
func hasKey(v any, key string) bool {
	m := asMap(v)
	if m == nil {
		return false
	}
	if _, ok := m[key]; ok {
		return true
	}
	for k := range m {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// errDumateTokenDead 标记登录态失效。
var errDumateTokenDead = errors.New("dumate: 登录态已失效，请重新登录")

func isDumateTokenDead(err error) bool { return errors.Is(err, errDumateTokenDead) }

/* ------------------------------------------------------------------ */
/* 单账号签到流程                                                       */
/* ------------------------------------------------------------------ */

func (a *App) dumateCookiesOf(acct *Account) []dumateCookie {
	raw, _ := acct.Ext["cookies"].([]any)
	out := make([]dumateCookie, 0, len(raw))
	for _, item := range raw {
		m := asMap(item)
		if m == nil {
			continue
		}
		out = append(out, dumateCookie{
			Name:   asString(jget(m, "name")),
			Value:  asString(jget(m, "value")),
			Domain: asString(jget(m, "domain")),
			Path:   asString(jget(m, "path")),
		})
	}
	return out
}

func (a *App) dumateSaveCookies(acct *Account, cookies []dumateCookie) {
	// 存回 Ext 时用通用 map 形态，保持 JSON 往返稳定
	arr := make([]any, 0, len(cookies))
	for _, c := range cookies {
		m := map[string]any{"name": c.Name, "value": c.Value}
		if c.Domain != "" {
			m["domain"] = c.Domain
		}
		if c.Path != "" {
			m["path"] = c.Path
		}
		arr = append(arr, m)
	}
	if acct.Ext == nil {
		acct.Ext = map[string]any{}
	}
	acct.Ext["cookies"] = arr
}

func (a *App) dumateRefreshToken(ctx context.Context, acct *Account) error {
	cookies := a.dumateCookiesOf(acct)
	if !cookieHas(cookies, cookieSession) && !cookieHas(cookies, cookieSessionID) {
		return errDumateTokenDead
	}
	token, expiredAt, accountID, err := a.dumateFetchAppToken(ctx, cookies)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	acct.AccessToken = token
	acct.AccessExp = expiredAt
	acct.TokenRefreshedAt = now
	if acct.Ext == nil {
		acct.Ext = map[string]any{}
	}
	acct.Ext["tokenExpiresAt"] = expiredAt * 1000
	if accountID != "" {
		acct.Ext["accountId"] = accountID
	}
	// 令牌链不能断：更新立即落盘
	return a.dumatePut(acct)
}

func (a *App) dumateSigninOne(ctx context.Context, acct *Account, force bool) signinResult {
	now := time.Now().UnixMilli()
	today := bjDay(now, a.cfg.Loc)
	base := signinResult{ID: acct.ID, Label: acct.Label}

	// 1) 防重复
	if !force && acct.LastOkDate == today {
		return signinResult{ID: base.ID, Label: base.Label, State: "skipped", Msg: "今日已完成"}
	}

	cookies := a.dumateCookiesOf(acct)
	if !cookieHas(cookies, cookieSession) && !cookieHas(cookies, cookieSessionID) {
		acct.LastState = "token_dead"
		acct.LastMsg = "缺少登录 cookie（bce-session / bce-sessionid），请重新登录"
		acct.LastRun = now
		_ = a.dumatePut(acct)
		return signinResult{ID: base.ID, Label: base.Label, State: "token_dead", Msg: acct.LastMsg}
	}

	// 2) 查签到状态
	info, err := a.dumateGetBonusInfo(ctx, cookies)
	if err != nil {
		state := "error"
		if isDumateTokenDead(err) {
			state = "token_dead"
		}
		acct.LastState = state
		acct.LastMsg = err.Error()
		acct.LastRun = now
		_ = a.dumatePut(acct)
		return signinResult{ID: base.ID, Label: base.Label, State: state, Msg: err.Error()}
	}
	acct.LastRun = now

	// 3) 今日已签（硬规则：hasIssued==true 不调 POST）
	if info.HasIssued {
		acct.LastState = "already"
		acct.LastMsg = "今日已签"
		acct.LastOkDate = today
		if acct.Ext == nil {
			acct.Ext = map[string]any{}
		}
		acct.Ext["signedDate"] = today
		acct.Credits = &info.TotalPoints
		_ = a.dumatePut(acct)
		bonus := dumateBonusPoints
		return signinResult{ID: base.ID, Label: base.Label, State: "already", Msg: "今日已签",
			Points: &bonus, Total: &info.TotalPoints}
	}

	// 4) 提交签到（非 429 错误重试 2 次）
	var claimErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		_, claimErr = a.dumateClaimBonus(ctx, cookies)
		if claimErr == nil {
			break
		}
		// 429 不重试，直接冷却
		if strings.Contains(claimErr.Error(), "HTTP 429") {
			mins := 55
			acct.CooldownUntil = now + int64(mins)*60000
			acct.LastState = "rate"
			acct.LastMsg = "签到限流，冷却约 " + fmtNum(float64(mins)) + " 分钟"
			_ = a.dumatePut(acct)
			return signinResult{ID: base.ID, Label: base.Label, State: "rate", Msg: acct.LastMsg}
		}
	}

	// 5) 复读验证：POST 报错时先确认是否「已被抢先签到」（上游对重复签到报 InternalServerError）
	info2, verr := a.dumateGetBonusInfo(ctx, cookies)
	if verr != nil {
		// 复读失败但 claim 成功：按成功记，积分按 +500
		if claimErr == nil {
			a.dumateMarkChecked(acct, today, info, dumateBonusPoints)
			bonus := dumateBonusPoints
			return signinResult{ID: base.ID, Label: base.Label, State: "checked",
				Msg: "+" + fmtNum(dumateBonusPoints) + " 积分（回查失败，按提交成功计）", Points: &bonus}
		}
		state := "error"
		if isDumateTokenDead(verr) {
			state = "token_dead"
		}
		acct.LastState = state
		acct.LastMsg = verr.Error()
		_ = a.dumatePut(acct)
		return signinResult{ID: base.ID, Label: base.Label, State: state, Msg: verr.Error()}
	}

	todayStr := today
	confirmed := info2.HasIssued || containsDay(info2.SignInDays, todayStr)
	if claimErr != nil && !confirmed {
		// POST 失败且复读确认未签 → error（重试耗尽）
		acct.LastState = "error"
		acct.LastMsg = claimErr.Error()
		_ = a.dumatePut(acct)
		return signinResult{ID: base.ID, Label: base.Label, State: "error", Msg: acct.LastMsg}
	}

	// 6) 已确认签上（POST 成功，或 POST 报错但复读确认已签=被客户端抢先）
	if claimErr != nil && confirmed {
		// 被抢先：不记 +500（不知是否本轮发放），按 already
		acct.LastState = "already"
		acct.LastMsg = "今日已签（客户端抢先完成，已确认）"
		acct.LastOkDate = today
		if acct.Ext == nil {
			acct.Ext = map[string]any{}
		}
		acct.Ext["signedDate"] = today
		acct.Credits = &info2.TotalPoints
		_ = a.dumatePut(acct)
		return signinResult{ID: base.ID, Label: base.Label, State: "already", Msg: acct.LastMsg, Total: &info2.TotalPoints}
	}

	gain := info2.TotalPoints - info.TotalPoints
	if gain <= 0 {
		gain = dumateBonusPoints
	}
	a.dumateMarkChecked(acct, today, info2, gain)
	msg := "+" + fmtNum(gain) + " 积分"
	return signinResult{ID: base.ID, Label: base.Label, State: "checked", Msg: msg, Points: &gain, Total: &info2.TotalPoints}
}

// dumateMarkChecked 写签到成功状态。
func (a *App) dumateMarkChecked(acct *Account, today string, info *dumateBonusInfo, gain float64) {
	acct.LastState = "checked"
	acct.LastMsg = "+" + fmtNum(gain) + " 积分"
	acct.LastOkDate = today
	credit := gain
	acct.LastCredit = &credit
	acct.TotalCredit += gain
	acct.TotalSigned++
	if acct.Ext == nil {
		acct.Ext = map[string]any{}
	}
	acct.Ext["signedDate"] = today
	acct.Credits = &info.TotalPoints
	_ = a.dumatePut(acct)
}

func containsDay(days []string, today string) bool {
	for _, d := range days {
		if d == today {
			return true
		}
	}
	return false
}

/* ------------------------------------------------------------------ */
/* 存取（走通用层，前缀 dumate:）                                        */
/* ------------------------------------------------------------------ */

func (a *App) dumateGet(id string) (*Account, error) { return a.storeOf("dumate").Get(id) }
func (a *App) dumatePut(acct *Account) error         { return a.storeOf("dumate").Put(acct) }
func (a *App) dumateAppend(acct *Account) error      { return a.storeOf("dumate").Append(acct) }

/* ------------------------------------------------------------------ */
/* 登录：扩展回传 cookie → 换 token 验证 → 入库                          */
/* ------------------------------------------------------------------ */

type dumateLoginSession struct {
	Status    string `json:"status"`
	Label     string `json:"label"`
	AccountID string `json:"accountId"`
	Error     string `json:"error"`
	CreatedAt int64  `json:"createdAt"`
}

// dumatePersist 用验证过的 cookie 建号入库。重复 accountId 时更新既有账号的 cookie。
func (a *App) dumatePersist(ctx context.Context, cookies []dumateCookie) (*Account, error) {
	token, expiredAt, accountID, err := a.dumateFetchAppToken(ctx, cookies)
	if err != nil {
		return nil, fmt.Errorf("cookie 验证失败: %w", err)
	}
	displayName := strings.TrimSpace(cookieValueOf(cookies, cookieDisplayName))
	// display-name cookie 的值是 URL 编码的中文（如 %E5%AF%92%E6%9C%88），解码为可读备注
	if displayName != "" {
		if dec, derr := url.QueryUnescape(displayName); derr == nil {
			displayName = strings.TrimSpace(dec)
		}
	}
	now := time.Now().UnixMilli()

	// 同账号幂等：accountId 相同则更新既有账号（扩展重复登录同一账号）
	if accountID != "" {
		st := a.storeOf("dumate")
		if list, lerr := st.ListMeta(); lerr == nil {
			for _, meta := range list {
				existing, gerr := st.Get(meta.ID)
				if gerr != nil || existing == nil {
					continue
				}
				if asString(jget(map[string]any{"v": existing.Ext["accountId"]}, "v")) == accountID {
					a.dumateSaveCookies(existing, cookies)
					existing.AccessToken = token
					existing.AccessExp = expiredAt
					existing.TokenRefreshedAt = now
					existing.LastState = "new"
					existing.LastMsg = "cookie 已更新"
					if displayName != "" {
						existing.Label = displayName
					}
					_ = st.Put(existing)
					return existing, nil
				}
			}
		}
	}

	id := randToken(9)
	label := displayName
	if label == "" {
		label = "DuMate-" + truncate(id, 4)
	}
	acct := &Account{
		ID:               id,
		Label:            label,
		AccessToken:      token,
		AccessExp:        expiredAt,
		TokenRefreshedAt: now,
		CreatedAt:        now,
		LastState:        "new",
		LastMsg:          "扩展登录，尚未运行",
		Ext: map[string]any{
			"cookies":        cookiesToAny(cookies),
			"accountId":      accountID,
			"displayName":    displayName,
			"tokenExpiresAt": expiredAt * 1000,
		},
	}
	if err := a.dumateAppend(acct); err != nil {
		return nil, err
	}
	return acct, nil
}

func cookiesToAny(cookies []dumateCookie) []any {
	arr := make([]any, 0, len(cookies))
	for _, c := range cookies {
		m := map[string]any{"name": c.Name, "value": c.Value}
		if c.Domain != "" {
			m["domain"] = c.Domain
		}
		if c.Path != "" {
			m["path"] = c.Path
		}
		arr = append(arr, m)
	}
	return arr
}

/* ================================================================== */
/* 适配器                                                              */
/* ================================================================== */

type dumatePlatform struct{}

func (p *dumatePlatform) Key() string     { return "dumate" }
func (p *dumatePlatform) Label() string   { return "DuMate" }
func (p *dumatePlatform) Flow() LoginFlow { return FlowExt }

func (p *dumatePlatform) Caps() Caps { return Caps{Credits: true} }

func (p *dumatePlatform) Metrics() []Metric {
	return []Metric{{K: "totalCredit", Label: "累计积分"}, {K: "credits", Label: "签到积分"}}
}

func (p *dumatePlatform) Describe(acct *Account, v viewCtx) (string, string, string) {
	switch {
	case v.SignedToday:
		if acct.LastState == "checked" {
			return "已签", "ok", acct.LastMsg
		}
		return "已签", "ok", ""
	case acct.LastState == "token_dead":
		return "登录态失效", "bad", acct.LastMsg
	case acct.LastState == "error" && v.RanToday:
		return "签到失败", "bad", acct.LastMsg
	case acct.LastState == "rate":
		return "限流冷却", "warn", ""
	}
	return "今日未签", "idle", ""
}

func (p *dumatePlatform) StartLogin(ctx context.Context, a *App) (LoginHandle, error) {
	id := randToken(12)
	sess := &dumateLoginSession{Status: "pending", CreatedAt: time.Now().UnixMilli()}
	if err := a.storeOf("dumate").PutSession(id, sess, dumateLoginTTL); err != nil {
		return LoginHandle{}, err
	}
	return LoginHandle{
		ID:        id,
		ExpiresAt: sess.CreatedAt + int64(dumateLoginTTL/time.Millisecond),
	}, nil
}

func (p *dumatePlatform) PollLogin(ctx context.Context, a *App, id string) (LoginPoll, error) {
	var sess dumateLoginSession
	if !a.storeOf("dumate").GetSession(id, &sess) {
		return LoginPoll{Status: "expired"}, nil
	}
	return LoginPoll{Status: orDefault(sess.Status, "pending"), ID: sess.AccountID, Label: sess.Label, Error: sess.Error}, nil
}

// CompleteLogin 扩展回传落点：LoginInput.Text 是 cookie JSON（数组或 {cookies:[...]}）。
func (p *dumatePlatform) CompleteLogin(ctx context.Context, a *App, id string, in LoginInput) (*LoginOutcome, error) {
	cookies := parseDumateCookies(firstNonEmpty(in.Text, in.URL))
	if len(cookies) == 0 {
		return nil, errors.New("没有解析到 cookie，请通过 DuMate 登录扩展登录，或粘贴 cookie JSON")
	}
	acct, err := a.dumatePersist(ctx, cookies)
	if err != nil {
		return nil, err
	}
	if id != "" {
		var sess dumateLoginSession
		if a.storeOf("dumate").GetSession(id, &sess) {
			sess.Status = "ok"
			sess.Label = acct.Label
			sess.AccountID = acct.ID
			_ = a.storeOf("dumate").PutSession(id, &sess, time.Minute)
		}
	}
	return &LoginOutcome{ID: acct.ID, Label: acct.Label, Via: "extension"}, nil
}

func (p *dumatePlatform) Signin(ctx context.Context, a *App, acct *Account, force bool) signinResult {
	return a.dumateSigninOne(ctx, acct, force)
}

// Keepalive 只刷 app token（cookie 不轮换，不会导致客户端掉线）。
func (p *dumatePlatform) Keepalive(ctx context.Context, a *App, acct *Account) error {
	return a.dumatePlatformKeepalive(acct)
}

// dumatePlatformKeepalive 便捷入口（单测与接口共用）。
func (a *App) dumatePlatformKeepalive(acct *Account) error {
	now := time.Now().UnixMilli()
	if acct.AccessToken != "" && acct.AccessExp > 0 && acct.AccessExp*1000-now > keepaliveMarginMS {
		return nil
	}
	return a.dumateRefreshToken(context.Background(), acct)
}
