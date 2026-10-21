package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

/* ================================================================== */
/* 通用平台层                                                          */
/*                                                                    */
/* 想加一个新签到平台，只需要两步：                                      */
/*   1. 新建一个 qoder.go，实现本文件里的 Platform 接口                  */
/*   2. 在 NewApp 里 registerPlatform(&qoderPlatform{})                 */
/*                                                                    */
/* 路由、控制台菜单、定时汇总、列表渲染、账号存取全部由注册表驱动，        */
/* 不需要再动 routes.go / console.html / cron.go。                     */
/* 某个平台接口变了，只改它自己那个文件顶部的常量块与状态码映射表。        */
/* ================================================================== */

/* ------------------------------------------------------------------ */
/* 统一账户信封                                                        */
/* ------------------------------------------------------------------ */

// Account 是所有平台共用的账户信封。
//
// 字段命名刻意沿用 trae.go / wb.go 最初的 JSON tag：磁盘上已有的密文记录
// 可以直接解开，不需要迁移。新平台的私有字段一律放进 Ext，这样加平台不用
// 动这个结构。
type Account struct {
	ID               string   `json:"id"`
	Label            string   `json:"label"`
	RefreshToken     string   `json:"refreshToken"`
	AccessToken      string   `json:"accessToken"`
	AccessExp        int64    `json:"accessExp"`
	TokenRefreshedAt int64    `json:"tokenRefreshedAt"`
	CreatedAt        int64    `json:"createdAt"`
	LastState        string   `json:"lastState"`
	LastMsg          string   `json:"lastMsg"`
	LastRun          int64    `json:"lastRun"`
	LastOkDate       string   `json:"lastOkDate"`
	LastCredit       *float64 `json:"lastCredit"`
	TotalCredit      float64  `json:"totalCredit"`
	TotalSigned      int      `json:"totalSigned"`
	Credits          *float64 `json:"credits"`
	CheckedIn        bool     `json:"checkedIn"`
	CooldownUntil    int64    `json:"cooldownUntil"`
	Streak           *float64 `json:"streak"`
	Total            *float64 `json:"total"`
	Verified         bool     `json:"verified"`

	// Trae 设备身份，其余平台留空
	DeviceID      string `json:"deviceId"`
	MachineID     string `json:"machineId"`
	PrivateKeyPEM string `json:"privateKeyPem"`
	PublicKeyPEM  string `json:"publicKeyPem"`
	DeviceName    string `json:"deviceName"`
	DeviceModel   string `json:"deviceModel"`
	DeviceBrand   string `json:"deviceBrand"`
	DeviceType    string `json:"deviceType"`
	OSInfo        string `json:"osInfo"`
	OSVersion     string `json:"osVersion"`
	ClientID      string `json:"clientId"`
	PlatformCode  string `json:"platformCode"`
	Source        string `json:"source"`

	// 新平台的私有字段放这里，加平台不用改结构体
	Ext map[string]any `json:"ext,omitempty"`
}

// 类型别名：让两个平台文件里的 *TraeAccount / *WBAccount 直接等价于 *Account，
// 协议函数体一行都不用改。
type (
	TraeAccount = Account
	WBAccount   = Account
)

// accountMeta 是索引里的账户摘要，列表页先读它再逐个解密本体。
type accountMeta struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	CreatedAt int64  `json:"createdAt"`
}

/* ------------------------------------------------------------------ */
/* 平台接口                                                            */
/* ------------------------------------------------------------------ */

// daysBetween 算两个 YYYY-MM-DD 相差多少天；日期为空或非法时返回一个大数，
// 这样「从没签到过」的账号也会被保活照顾到。
func daysBetween(from, to string, loc *time.Location) int {
	a, err1 := time.ParseInLocation("2006-01-02", from, loc)
	b, err2 := time.ParseInLocation("2006-01-02", to, loc)
	if err1 != nil || err2 != nil {
		return 1 << 20
	}
	return int(b.Sub(a).Hours() / 24)
}

// 令牌剩余有效期还大于这个值时，保活不去做无谓的刷新
const keepaliveMarginMS = 6 * 60 * 60 * 1000

type LoginFlow string

// 登录形态决定控制台渲染哪张卡片，目前只有两种：
//   - callback：官方授权页把 code 回送到本机的 127.0.0.1（Trae），远程访问要粘贴兜底
//   - link：打开官方页面完成授权，本服务在后台轮询结果（WorkBuddy / Qoder）
const (
	FlowCallback LoginFlow = "callback"
	FlowLink     LoginFlow = "link"
)

type Metric struct {
	K     string `json:"k"`
	Label string `json:"label"`
}

// Caps 描述平台能力，控制台据此决定显示哪些列、哪些提示。
type Caps struct {
	Cooldown bool `json:"cooldown"` // 有限流冷却
	Device   bool `json:"device"`   // 依赖设备密钥
	Local    bool `json:"local"`    // 扫码回调只认 127.0.0.1，远程访问需粘贴兜底
	Credits  bool `json:"credits"`  // 能查余额
	Streak   bool `json:"streak"`   // 能查连签天数
}

// LoginHandle 是发起登录后交给前端的引导信息，字段按 flow 取用。
type LoginHandle struct {
	ID         string    `json:"id"`
	Flow       LoginFlow `json:"flow"`
	AuthURL    string    `json:"authUrl,omitempty"`
	VerifyURL  string    `json:"verifyUrl,omitempty"`
	UserCode   string    `json:"userCode,omitempty"`
	IntervalMS int       `json:"intervalMs,omitempty"`
	ExpiresAt  int64     `json:"expiresAt,omitempty"`
	Callback   string    `json:"callback,omitempty"`
}

type LoginPoll struct {
	Status string `json:"status"` // pending / ok / expired / error
	ID     string `json:"id,omitempty"`
	Label  string `json:"label,omitempty"`
	Error  string `json:"error,omitempty"`
	Via    string `json:"via,omitempty"`
}

type LoginInput struct {
	URL  string
	Text string
}

// viewCtx 是列表行判定的全部依据，加判定维度时不用改接口签名。
type viewCtx struct {
	Today       string
	Now         int64
	Loc         *time.Location
	SignedToday bool
	RanToday    bool
}

type LoginOutcome struct {
	ID     string
	Label  string
	Via    string
	Repeat bool
}

// Platform 是各签到平台需要实现的全部契约。
//
// Signin 之外都是「通用层替你做的事」的钩子；协议相关的实现留在各自的
// 平台文件里，接口变了只改那一个文件。
type Platform interface {
	Key() string
	Label() string
	Flow() LoginFlow
	Caps() Caps
	// 列表要展示的指标列
	Metrics() []Metric
	// 列表行的标签、色调与备注，由平台自己判定（前端不做分支）
	Describe(acct *Account, v viewCtx) (tag, tone, msg string)

	StartLogin(ctx context.Context, a *App) (LoginHandle, error)
	PollLogin(ctx context.Context, a *App, id string) (LoginPoll, error)
	CompleteLogin(ctx context.Context, a *App, id string, in LoginInput) (*LoginOutcome, error)
	Signin(ctx context.Context, a *App, acct *Account, force bool) signinResult
}

// callbackHandler 由「扫码后浏览器回送 code」的平台实现（目前只有 Trae）。
type callbackHandler interface {
	HandleCallback(ctx context.Context, a *App, w http.ResponseWriter, r *http.Request)
}

// keepaliver 由支持「只刷新令牌不签到」的平台实现。
type keepaliver interface {
	Keepalive(ctx context.Context, a *App, acct *Account) error
}

/* ------------------------------------------------------------------ */
/* 注册表                                                              */
/* ------------------------------------------------------------------ */

type platformEntry struct {
	p  Platform
	st *platStore
}

func (a *App) registerPlatform(p Platform) {
	st := newPlatStore(a.kv, p.Key(), a.cfg.MasterKey)
	a.regs = append(a.regs, &platformEntry{p: p, st: st})
	a.store[p.Key()] = st
}

func (a *App) byKey(key string) Platform {
	for _, e := range a.regs {
		if e.p.Key() == key {
			return e.p
		}
	}
	return nil
}

func (a *App) storeOf(key string) *platStore { return a.store[key] }

// callbackOwner 找出这次回调属于哪个平台：先用 trace 定位会话，再退回最近一次。
func (a *App) callbackOwner(trace string) Platform {
	var fallback Platform
	for _, e := range a.regs {
		if _, ok := e.p.(callbackHandler); !ok {
			continue
		}
		if fallback == nil {
			fallback = e.p
		}
		if trace != "" {
			if e.st.HasSession(trace) {
				return e.p
			}
			continue
		}
		if e.st.Latest() != "" {
			return e.p
		}
	}
	return fallback
}

/* ------------------------------------------------------------------ */
/* 通用存取：按平台 key 隔离 KV 前缀与加密标签                           */
/* ------------------------------------------------------------------ */

type platStore struct {
	kv       *KV
	prefix   string
	info     string
	sessInfo string
	master   []byte
}

const platIndexKey = "index:accounts"

func newPlatStore(kv *KV, key string, master []byte) *platStore {
	return &platStore{
		kv:       kv,
		prefix:   key + ":",
		info:     key + "-signin/v1/account",
		sessInfo: key + "-signin/v1/session",
		master:   master,
	}
}

func (s *platStore) sc() scopedKV { return scopedKV{kv: s.kv, prefix: s.prefix} }

func (s *platStore) ListMeta() ([]accountMeta, error) {
	raw, ok := s.sc().Get(platIndexKey)
	if !ok {
		return []accountMeta{}, nil
	}
	var list []accountMeta
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *platStore) SaveMeta(list []accountMeta) error {
	return s.sc().Put(platIndexKey, string(mustJSON(list)), 0)
}

func (s *platStore) Get(id string) (*Account, error) {
	raw, ok := s.sc().Get("acct:" + id)
	if !ok {
		return nil, nil
	}
	var acct Account
	if err := openRecord(s.master, s.info, raw, &acct); err != nil {
		return nil, err
	}
	return &acct, nil
}

// Put 只写账号本体；索引由 Append/Remove/Rename 维护，避免这里重复落盘。
func (s *platStore) Put(acct *Account) error {
	sealed, err := sealRecord(s.master, s.info, acct)
	if err != nil {
		return err
	}
	return s.sc().Put("acct:"+acct.ID, sealed, 0)
}

// Append 新账号：写本体 + 追加索引。
func (s *platStore) Append(acct *Account) error {
	if err := s.Put(acct); err != nil {
		return err
	}
	list, _ := s.ListMeta()
	list = append(list, accountMeta{ID: acct.ID, Label: acct.Label, CreatedAt: acct.CreatedAt})
	return s.SaveMeta(list)
}

func (s *platStore) Remove(id string) error {
	_ = s.sc().Delete("acct:" + id)
	list, _ := s.ListMeta()
	keep := make([]accountMeta, 0, len(list))
	for _, x := range list {
		if x.ID != id {
			keep = append(keep, x)
		}
	}
	return s.SaveMeta(keep)
}

func (s *platStore) Rename(id, label string) error {
	acct, err := s.Get(id)
	if err != nil {
		return err
	}
	if acct == nil {
		return fmt.Errorf("账号不存在: %s", id)
	}
	acct.Label = label
	if err := s.Put(acct); err != nil {
		return err
	}
	list, _ := s.ListMeta()
	for i := range list {
		if list[i].ID == id {
			list[i].Label = label
		}
	}
	return s.SaveMeta(list)
}

/* 登录会话（密文，15 分钟 TTL） */

func (s *platStore) PutSession(id string, v any, ttl time.Duration) error {
	sealed, err := sealRecord(s.master, s.sessInfo, v)
	if err != nil {
		return err
	}
	return s.sc().Put("sess:"+id, sealed, ttl)
}

func (s *platStore) GetSession(id string, v any) bool {
	raw, ok := s.sc().Get("sess:" + id)
	if !ok {
		return false
	}
	return openRecord(s.master, s.sessInfo, raw, v) == nil
}

func (s *platStore) HasSession(id string) bool {
	_, ok := s.sc().Get("sess:" + id)
	return ok
}

func (s *platStore) DeleteSession(id string) { _ = s.sc().Delete("sess:" + id) }

func (s *platStore) SetLatest(id string, ttl time.Duration) {
	_ = s.sc().Put("sess:latest", id, ttl)
}

func (s *platStore) Latest() string {
	v, _ := s.sc().Get("sess:latest")
	return v
}

/* ------------------------------------------------------------------ */
/* 列表：tag / tone 由平台判定，前端只做渲染                             */
/* ------------------------------------------------------------------ */

type metricVal struct {
	K     string `json:"k"`
	Label string `json:"label"`
	V     string `json:"v"`
}

type acctItem struct {
	ID            string      `json:"id"`
	Label         string      `json:"label"`
	Tag           string      `json:"tag"`
	Tone          string      `json:"tone"` // ok / warn / bad / idle
	Msg           string      `json:"msg"`
	LastRun       *string     `json:"lastRun"`
	LastOkDate    *string     `json:"lastOkDate"`
	RefreshedAt   *string     `json:"refreshedAt"`
	Metrics       []metricVal `json:"metrics"`
	SignedToday   bool        `json:"signedToday"`
	RanToday      bool        `json:"ranToday"`
	CooldownUntil int64       `json:"cooldownUntil,omitempty"`
	Today         string      `json:"today"`
}

var metricGetters = map[string]func(*Account) *float64{
	"credits":     func(a *Account) *float64 { return a.Credits },
	"streak":      func(a *Account) *float64 { return a.Streak },
	"total":       func(a *Account) *float64 { return a.Total },
	"lastCredit":  func(a *Account) *float64 { return a.LastCredit },
	"totalCredit": func(a *Account) *float64 { return &a.TotalCredit },
}

func metricsOf(p Platform, acct *Account) []metricVal {
	out := make([]metricVal, 0, 2)
	for _, m := range p.Metrics() {
		get, ok := metricGetters[m.K]
		if !ok {
			continue
		}
		v := get(acct)
		if v == nil || *v == 0 {
			continue
		}
		out = append(out, metricVal{K: m.K, Label: m.Label, V: fmtNum(round2(*v))})
	}
	return out
}

func (a *App) listAccounts(p Platform) ([]acctItem, string, error) {
	st := a.storeOf(p.Key())
	list, err := st.ListMeta()
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UnixMilli()
	today := bjDay(now, a.cfg.Loc)
	out := make([]acctItem, 0, len(list))
	for _, meta := range list {
		acct, err := st.Get(meta.ID)
		if err != nil || acct == nil {
			continue
		}
		signedToday := acct.LastOkDate == today
		ranToday := acct.LastRun > 0 && bjDay(acct.LastRun, a.cfg.Loc) == today
		tag, tone, msg := p.Describe(acct, viewCtx{
			Today: today, Now: now, Loc: a.cfg.Loc,
			SignedToday: signedToday, RanToday: ranToday,
		})
		item := acctItem{
			ID:          acct.ID,
			Label:       acct.Label,
			Tag:         tag,
			Tone:        tone,
			Msg:         msg,
			Metrics:     metricsOf(p, acct),
			Today:       today,
			SignedToday: signedToday,
			RanToday:    ranToday,
		}
		if acct.LastRun > 0 {
			s := bjStamp(acct.LastRun, a.cfg.Loc)
			item.LastRun = &s
		}
		if acct.LastOkDate != "" {
			d := acct.LastOkDate
			item.LastOkDate = &d
		}
		if acct.TokenRefreshedAt > 0 {
			s := bjStamp(acct.TokenRefreshedAt, a.cfg.Loc)
			item.RefreshedAt = &s
		}
		if acct.CooldownUntil > now {
			item.CooldownUntil = acct.CooldownUntil
		}
		out = append(out, item)
	}
	return out, today, nil
}

/* ------------------------------------------------------------------ */
/* 批量汇总                                                            */
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
		default: // rate / cooldown / pending
			s.Cooling++
		}
	}
	return s
}

func (a *App) runPlatform(ctx context.Context, p Platform, force bool) runSummary {
	st := a.storeOf(p.Key())
	list, err := st.ListMeta()
	if err != nil {
		return newSummary([]signinResult{{State: "error", Msg: err.Error()}})
	}
	results := make([]signinResult, 0, len(list))
	for _, meta := range list {
		acct, err := st.Get(meta.ID)
		if err != nil || acct == nil {
			continue
		}
		results = append(results, p.Signin(ctx, a, acct, force))
	}
	return newSummary(results)
}

// runAll 遍历注册表，键名就是平台 key（/trae、/wb… 的 cron 输出结构不变）。
func (a *App) runAll(ctx context.Context, force bool) map[string]any {
	at := bjStamp(time.Now().UnixMilli(), time.FixedZone("CST", 8*3600))
	out := map[string]any{"at": at}
	for _, e := range a.regs {
		out[e.p.Key()] = a.runPlatform(ctx, e.p, force)
	}
	return out
}

/* ------------------------------------------------------------------ */
/* 通用 API：/<key>/api/...                                            */
/* ------------------------------------------------------------------ */

func (a *App) routePlatformAPI(w http.ResponseWriter, r *http.Request, p Platform, sub string) {
	// 官方授权页回送 code 时跳进来，带不了登录密码
	if sub == "/oauth/callback" {
		a.handleAuthorize(w, r)
		return
	}
	if ok, st, msg := a.adminOK(r); !ok {
		writeErr(w, st, msg)
		return
	}
	method := strings.ToUpper(r.Method)
	switch {
	case sub == "/list" && method == http.MethodGet:
		a.handleList(w, r, p)
	case sub == "/run" && method == http.MethodPost:
		writeJSON(w, http.StatusOK, a.runPlatform(r.Context(), p, r.URL.Query().Get("force") == "1"))
	case sub == "/login/start" && method == http.MethodPost:
		a.handleLoginStart(w, r, p)
	case sub == "/login/poll" && method == http.MethodGet:
		a.handleLoginPoll(w, r, p)
	case sub == "/login/claim" && method == http.MethodPost:
		a.handleLoginClaim(w, r, p)
	case sub == "/remove" && method == http.MethodPost:
		a.handleRemove(w, r, p)
	case sub == "/rename" && method == http.MethodPost:
		a.handleRename(w, r, p)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func (a *App) handleList(w http.ResponseWriter, r *http.Request, p Platform) {
	items, today, err := a.listAccounts(p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "today": today, "accounts": items})
}

func (a *App) handlePlatforms(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0, len(a.regs))
	for _, e := range a.regs {
		out = append(out, map[string]any{
			"key":     e.p.Key(),
			"label":   e.p.Label(),
			"flow":    e.p.Flow(),
			"caps":    e.p.Caps(),
			"metrics": e.p.Metrics(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"platforms": out})
}

func (a *App) handleLoginStart(w http.ResponseWriter, r *http.Request, p Platform) {
	h, err := p.StartLogin(r.Context(), a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	h.Flow = p.Flow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "handle": h})
}

func (a *App) handleLoginPoll(w http.ResponseWriter, r *http.Request, p Platform) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 id")
		return
	}
	res, err := p.PollLogin(r.Context(), a, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleLoginClaim 远程访问时，用户把授权后的地址粘回来。
func (a *App) handleLoginClaim(w http.ResponseWriter, r *http.Request, p Platform) {
	if p.Flow() != FlowCallback {
		writeErr(w, http.StatusBadRequest, p.Label()+"不需要粘贴地址，请在打开的页面里完成授权")
		return
	}
	body := readJSONBody(r)
	text := strings.TrimSpace(firstNonEmpty(asString(body["url"]), asString(body["text"])))
	if text == "" {
		writeErr(w, http.StatusBadRequest, "请粘贴回调地址")
		return
	}
	out, err := p.CompleteLogin(r.Context(), a, "", LoginInput{URL: text})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": out.ID, "label": out.Label, "via": out.Via})
}

func (a *App) handleRemove(w http.ResponseWriter, r *http.Request, p Platform) {
	body := readJSONBody(r)
	id := strings.TrimSpace(asString(body["id"]))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 id")
		return
	}
	if err := a.storeOf(p.Key()).Remove(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": id})
}

func (a *App) handleRename(w http.ResponseWriter, r *http.Request, p Platform) {
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
	if err := a.storeOf(p.Key()).Rename(id, label); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "label": label})
}

/* ------------------------------------------------------------------ */
/* 授权回调                                                            */
/* ------------------------------------------------------------------ */

func (a *App) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	params := parseCallbackParams(r.URL)

	// 无凭据也无 trace：官网在探测客户端是否在线，直接 200
	if params.AuthCode == "" && params.RefreshToken == "" && params.Error == "" && params.Trace == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok"))
		return
	}

	target := a.callbackOwner(params.Trace)
	if target == nil {
		writeHTML(w, http.StatusBadRequest, oauthPage("登录会话丢失", "找不到对应的登录会话，请回到控制台重新发起", false))
		return
	}
	if params.Error != "" {
		writeHTML(w, http.StatusBadRequest, oauthPage("授权未完成", params.Error, false))
		return
	}
	if h, ok := target.(callbackHandler); ok {
		h.HandleCallback(r.Context(), a, w, r)
		return
	}
	writeHTML(w, http.StatusBadRequest, oauthPage("登录会话丢失", "这个回调没有对应的登录方式", false))
}
