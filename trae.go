package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

/* ================================================================== */
/* 模块一：Trae —— 设备身份 / 扫码授权 / 签到                            */
/* 与 upstream/worker.js 模块一逐函数对齐，改动请同步对照                */
/* ================================================================== */

const (
	traeAPI = "https://api.trae.cn"

	epTraeClaim    = "/trae/api/v2/ug/checkin_credits/claim"
	epTraeStatus   = "/trae/api/v2/ug/checkin_credits/status"
	epTraeEntitle  = "/trae/api/v2/pay/user_current_entitlement_list"
	epTraeExchange = "/trae/api/v3/oauth/ExchangeToken"

	epTraeExchangeLegacy = "https://api.trae.com.cn/cloudide/api/v3/trae/oauth/ExchangeToken"

	clientIDIDE = "ono9krqynydwx5"
	clientIDOld = "en1oxy7wnw8j9n"
	ideVersion  = "1.107.1"
	uaTrae      = "Trae/" + ideVersion

	authorizeBase      = "https://www.trae.cn"
	oauthAppID         = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	oauthAppVersion    = "3.3.100"
	oauthPluginVersion = "2.3.83560"

	traeLoginTTL = 15 * time.Minute
)

const (
	traeCodeOK      = 0
	traeCodeAlready = 9095
	traeCodeRate    = 9074

	traeCooldownMin     = 55
	traePeakCooldownMin = 12
)

var traeAuthFailCodes = map[float64]bool{1001: true, 1002: true, 401: true, 403: true}

// 便于测试指向本地假服务
var (
	traeAPIBase          = traeAPI
	traeExchangeLegacyEP = epTraeExchangeLegacy
)

type TraeAccount struct {
	ID               string   `json:"id"`
	Label            string   `json:"label"`
	RefreshToken     string   `json:"refreshToken"`
	DeviceID         string   `json:"deviceId"`
	MachineID        string   `json:"machineId"`
	PrivateKeyPEM    string   `json:"privateKeyPem"`
	PublicKeyPEM     string   `json:"publicKeyPem"`
	DeviceName       string   `json:"deviceName"`
	DeviceModel      string   `json:"deviceModel"`
	DeviceBrand      string   `json:"deviceBrand"`
	DeviceType       string   `json:"deviceType"`
	OSInfo           string   `json:"osInfo"`
	OSVersion        string   `json:"osVersion"`
	ClientID         string   `json:"clientId"`
	PlatformCode     string   `json:"platformCode"`
	Source           string   `json:"source"`
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
	AccessToken      string   `json:"accessToken"`
	AccessExp        int64    `json:"accessExp"`
	TokenRefreshedAt int64    `json:"tokenRefreshedAt"`
}

type accountMeta struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	CreatedAt int64  `json:"createdAt"`
}

type traeLoginSession struct {
	Trace         string `json:"trace"`
	Line          string `json:"line"`
	Verifier      string `json:"verifier"`
	Callback      string `json:"callback"`
	DeviceID      string `json:"deviceId"`
	MachineID     string `json:"machineId"`
	PrivateKeyPEM string `json:"privateKeyPem"`
	PublicKeyPEM  string `json:"publicKeyPem"`
	ClientID      string `json:"clientId"`
	Platform      string `json:"platform"`
	Status        string `json:"status"`
	Label         string `json:"label"`
	AccountID     string `json:"accountId"`
	Via           string `json:"via"`
	Error         string `json:"error"`
	CreatedAt     int64  `json:"createdAt"`
}

const (
	traeIndexKey   = "index:accounts"
	traeLatestKey  = "login:latest"
	traeAuditLabel = "trae"
)

func (a *App) traeKV() scopedKV { return scopedKV{kv: a.kv, prefix: "trae:"} }

/* ------------------------------------------------------------------ */
/* 账号存取（密文，令牌永不出现在响应里）                                */
/* ------------------------------------------------------------------ */

func (a *App) traeListMeta() ([]accountMeta, error) {
	raw, ok := a.traeKV().Get(traeIndexKey)
	if !ok {
		return []accountMeta{}, nil
	}
	var list []accountMeta
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (a *App) traeSaveIndex(list []accountMeta) error {
	return a.traeKV().Put(traeIndexKey, string(mustJSON(list)), 0)
}

func (a *App) traeGet(id string) (*TraeAccount, error) {
	raw, ok := a.traeKV().Get("acct:" + id)
	if !ok {
		return nil, nil
	}
	var acct TraeAccount
	if err := openRecord(a.cfg.MasterKey, infoTrae, raw, &acct); err != nil {
		return nil, err
	}
	return &acct, nil
}

func (a *App) traePut(acct *TraeAccount) error {
	sealed, err := sealRecord(a.cfg.MasterKey, infoTrae, acct)
	if err != nil {
		return err
	}
	return a.traeKV().Put("acct:"+acct.ID, sealed, 0)
}

/* ------------------------------------------------------------------ */
/* 时间辅助                                                            */
/* ------------------------------------------------------------------ */

func bjDay(ms int64, loc *time.Location) string {
	return time.UnixMilli(ms).In(loc).Format("2006-01-02")
}

func bjHour(ms int64, loc *time.Location) int {
	return time.UnixMilli(ms).In(loc).Hour()
}

func bjStamp(ms int64, loc *time.Location) string {
	return time.UnixMilli(ms).In(loc).Format("2006-01-02T15:04:05.000") + " +08:00"
}

// 早窗（北京时间 0 点）冷却更短，好让下一轮还能在窗口里补签
func (a *App) traeCooldownMinutes() int {
	if bjHour(time.Now().UnixMilli(), a.cfg.Loc) == 0 {
		return traePeakCooldownMin
	}
	return traeCooldownMin
}

/* ------------------------------------------------------------------ */
/* ECDSA P-256：签名输出 base64(DER)，低 s 归一化                        */
/* ------------------------------------------------------------------ */

func parseECPrivateKeyPEM(pemText string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemText)))
	if block == nil {
		return nil, errors.New("设备私钥不是 PEM 格式")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if ec, ok := k.(*ecdsa.PrivateKey); ok {
			return ec, nil
		}
		return nil, errors.New("设备私钥不是 EC 类型")
	}
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	return nil, errors.New("设备私钥结构异常（既不是 PKCS#8 也不是 SEC1）")
}

// ecSign 与 worker.js 的 ecSign 等价：SHA-256 + DER(r,s) + 低 s 归一化。
func ecSign(privateKeyPEM string, data []byte) (string, error) {
	key, err := parseECPrivateKeyPEM(privateKeyPEM)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	n := key.Curve.Params().N
	half := new(big.Int).Rsh(n, 1)
	if s.Cmp(half) > 0 {
		s = new(big.Int).Sub(n, s)
	}
	der, err := asn1.Marshal(struct {
		R, S *big.Int
	}{r, s})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

func genDeviceIdentity() (privPEM, pubPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})), nil
}

// 15 位数字设备号，首位非 0，与客户端 device_id 形态一致
func genDeviceID() string {
	b := randomBytes(15)
	var sb strings.Builder
	sb.WriteByte(byte('1' + b[0]%9))
	for i := 1; i < 15; i++ {
		sb.WriteByte(byte('0' + b[i]%10))
	}
	return sb.String()
}

/* ------------------------------------------------------------------ */
/* 与 Trae 服务端交互                                                   */
/* ------------------------------------------------------------------ */

func traeRequest(ctx context.Context, method, target string, headers map[string]string, body any) (httpResult, error) {
	h := map[string]string{}
	for k, v := range headers {
		h[k] = v
	}
	return doJSON(ctx, method, target, h, body, 20*time.Second)
}

// 签到/积分接口通用头：缺设备头会被服务端拒成 9074
func traeUGHeaders(token string, acct *TraeAccount) map[string]string {
	h := map[string]string{
		"Authorization": "Cloud-IDE-JWT " + token,
		"X-User-Region": "CN",
		"User-Agent":    uaTrae,
		"Content-Type":  "application/json",
	}
	if did := strings.TrimSpace(acct.DeviceID); did != "" {
		h["x-device-id"] = did
	}
	dt := acct.DeviceType
	if dt == "" {
		dt = "Windows"
	}
	osv := acct.OSVersion
	if osv == "" {
		osv = "10.0.19045"
	}
	h["x-device-type"] = dt
	h["x-os-version"] = osv
	h["x-app-version"] = ideVersion
	if brand := strings.TrimSpace(acct.DeviceBrand); brand != "" {
		h["x-device-brand"] = brand
	}
	return h
}

// jwtExp 从 JWT 取过期时间（秒），兼容 TokenExpireAt 的毫秒值
func jwtExp(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return 0
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return 0
	}
	v := asFloat0(jget(m, "exp"))
	if v == 0 {
		v = asFloat0(jget(m, "TokenExpireAt"))
	}
	if v > 1e12 {
		v = v / 1000
	}
	return int64(v)
}

func asFloat0(v any) float64 {
	f, _ := asFloat(v)
	return f
}

// buildProof 构造设备证明 (DeviceProof, DeviceInfo)
func buildProof(acct *TraeAccount, path, clientID, platformCode, refreshToken string) (map[string]any, error) {
	ts := time.Now().Unix()
	nonce := randomHex(16)
	canonical := strings.Join([]string{"POST", path, clientID, refreshToken, strconv.FormatInt(ts, 10), nonce}, "\n")
	sig, err := ecSign(acct.PrivateKeyPEM, []byte(canonical))
	if err != nil {
		return nil, fmt.Errorf("设备签名失败（检查设备私钥）: %w", err)
	}
	info := map[string]any{
		"DeviceID":        acct.DeviceID,
		"MachineID":       acct.MachineID,
		"PlatformCode":    platformCode,
		"DeviceType":      "PC",
		"DeviceName":      orDefault(acct.DeviceName, "PC"),
		"DeviceModel":     acct.DeviceModel,
		"ClientVersion":   ideVersion,
		"DevicePublicKey": acct.PublicKeyPEM,
		"DeviceBrand":     acct.DeviceBrand,
		"DeviceCPU":       "",
		"OSInfo":          orDefault(acct.OSInfo, "Windows"),
		"OSVersion":       orDefault(acct.OSVersion, "10.0.19045"),
	}
	return map[string]any{
		"proof": map[string]any{"Timestamp": ts, "Nonce": nonce, "Signature": sig},
		"info":  info,
	}, nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func explainRefreshErr(res httpResult) string {
	errObj := jget(jget(res.JSON, "ResponseMetadata"), "Error")
	code := asString(jget(errObj, "Code"))
	msg := ""
	if data := jget(errObj, "Data"); data != nil {
		msg = asString(jget(data, "__Message.error"))
	}
	if msg == "" {
		msg = asString(jget(errObj, "Message"))
	}
	if msg == "" {
		msg = truncate(res.Text, 200)
	}
	hints := map[string]string{
		"20101": "refreshToken 已失效或被别处轮换，请删掉该账号重新扫码登录",
		"10101": "refreshToken 与当前产品线不匹配，请用对应按钮重新扫码登录",
		"20405": "服务端要求设备证明，但该账号缺少设备密钥，请删掉后重新扫码登录",
		"20403": "设备密钥与令牌不匹配，请删掉该账号重新扫码登录",
	}
	if code == "" || code == "0" {
		code = "?"
	}
	hint := hints[code]
	out := fmt.Sprintf("code=%s %s", code, msg)
	if hint != "" {
		out += "（" + hint + "）"
	}
	return out
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

type tokenPair struct {
	AccessToken  string
	RefreshToken string
}

// refreshTraeTokens 用 refreshToken 换新的双令牌
func (a *App) refreshTraeTokens(ctx context.Context, acct *TraeAccount) (tokenPair, error) {
	rt := strings.TrimSpace(acct.RefreshToken)
	if rt == "" {
		return tokenPair{}, errors.New("账号没有 refreshToken")
	}
	hasKeys := acct.PrivateKeyPEM != "" && acct.PublicKeyPEM != ""

	if !hasKeys {
		body := map[string]any{
			"ClientID":     clientIDOld,
			"RefreshToken": rt,
			"ClientSecret": "-",
			"UserID":       "",
		}
		res, err := traeRequest(ctx, "POST", traeExchangeLegacyEP, map[string]string{
			"Accept":     "application/json",
			"User-Agent": uaTrae,
		}, body)
		if err != nil {
			return tokenPair{}, err
		}
		token := asString(jget(jget(res.JSON, "Result"), "Token"))
		if res.Status >= 400 || token == "" {
			return tokenPair{}, errors.New("刷新令牌失败: " + explainRefreshErr(res))
		}
		newRT := asString(jget(jget(res.JSON, "Result"), "RefreshToken"))
		if newRT == "" {
			newRT = rt
		}
		return tokenPair{AccessToken: token, RefreshToken: newRT}, nil
	}

	combos := [][2]string{
		{orDefault(acct.ClientID, clientIDIDE), orDefault(acct.PlatformCode, "IDE_PC")},
		{clientIDOld, "IDE_PC"},
		{clientIDOld, "SOLO_PC"},
	}
	seen := map[string]bool{}
	var uniq [][2]string
	for _, c := range combos {
		k := c[0] + "|" + c[1]
		if seen[k] {
			continue
		}
		seen[k] = true
		uniq = append(uniq, c)
	}

	lastErr := ""
	for _, c := range uniq {
		clientID, platformCode := c[0], c[1]
		built, err := buildProof(acct, epTraeExchange, clientID, platformCode, rt)
		if err != nil {
			return tokenPair{}, err
		}
		body := map[string]any{
			"ClientID":     clientID,
			"ClientSecret": "",
			"RefreshToken": rt,
			"DeviceInfo":   built["info"],
			"DeviceProof":  built["proof"],
			"IDEVersion":   ideVersion,
		}
		res, err := traeRequest(ctx, "POST", traeAPIBase+epTraeExchange, map[string]string{
			"x-cloudide-token": "",
		}, body)
		if err != nil {
			return tokenPair{}, err
		}
		token := asString(jget(jget(res.JSON, "Result"), "Token"))
		if res.Status < 400 && token != "" {
			newRT := asString(jget(jget(res.JSON, "Result"), "RefreshToken"))
			if newRT == "" {
				newRT = rt
			}
			return tokenPair{AccessToken: token, RefreshToken: newRT}, nil
		}
		lastErr = explainRefreshErr(res)
		// 只有「不匹配」类错误才值得换一组钥匙，其它错误直接抛
		if !strings.Contains(res.Text, "10101") && !strings.Contains(res.Text, "20403") {
			break
		}
	}
	if lastErr == "" {
		lastErr = "未知错误"
	}
	return tokenPair{}, errors.New("刷新令牌失败: " + lastErr)
}

type claimResult struct {
	Status int
	Code   float64
	Msg    string
	Points *float64
}

// claimTrae 提交签到：0 成功 / 9095 今日已签 / 9074 限流
func (a *App) claimTrae(ctx context.Context, token string, acct *TraeAccount) claimResult {
	res, err := traeRequest(ctx, "POST", traeAPIBase+epTraeClaim, traeUGHeaders(token, acct), map[string]any{"req_source": 1})
	if err != nil {
		return claimResult{Status: 0, Code: -1, Msg: err.Error()}
	}
	if res.JSON == nil {
		return claimResult{Status: res.Status, Code: -1, Msg: truncate(res.Text, 160)}
	}
	code := asFloat0(jget(res.JSON, "code"))
	msg := asString(jget(res.JSON, "message"))
	if msg == "" {
		msg = truncate(res.Text, 160)
	}
	var points *float64
	if data := jget(res.JSON, "data"); data != nil {
		if p, ok := asFloat(jget(data, "points")); ok {
			points = &p
		}
	}
	return claimResult{Status: res.Status, Code: code, Msg: msg, Points: points}
}

type statusResult struct {
	OK      bool
	Checked bool
	Credits *float64
	Enable  any
	Code    float64
	Raw     string
}

func (a *App) traeStatusOf(ctx context.Context, token string, acct *TraeAccount) statusResult {
	res, err := traeRequest(ctx, "POST", traeAPIBase+epTraeStatus, traeUGHeaders(token, acct), map[string]any{"req_source": 1})
	if err != nil {
		return statusResult{Raw: err.Error()}
	}
	if res.JSON == nil {
		return statusResult{Raw: truncate(res.Text, 160)}
	}
	if res.Status >= 400 {
		return statusResult{Code: asFloat0(jget(res.JSON, "code")), Raw: truncate(res.Text, 160)}
	}
	out := statusResult{
		OK:      true,
		Checked: asBool(jget(res.JSON, "checked_in")),
		Enable:  jget(res.JSON, "enable"),
		Code:    asFloat0(jget(res.JSON, "code")),
	}
	if c, ok := asFloat(jget(res.JSON, "credits")); ok {
		out.Credits = &c
	}
	return out
}

// creditsOf 查积分余额：优先 usage_summary，再按 entitlement 包求和兜底
func (a *App) traeCreditsOf(ctx context.Context, token string, acct *TraeAccount) *float64 {
	res, err := traeRequest(ctx, "POST", traeAPIBase+epTraeEntitle, traeUGHeaders(token, acct),
		map[string]any{"require_usage": true})
	if err != nil || res.JSON == nil {
		return nil
	}
	root := jget(res.JSON, "data")
	if root == nil {
		root = res.JSON
	}
	us := jget(root, "usage_summary")
	total, _ := asFloat(jget(us, "total_amount"))
	used, _ := asFloat(jget(us, "consumed_amount"))
	if total > 0 {
		v := total - used
		return &v
	}
	packs := asSlice(jget(root, "user_entitlement_pack_list"))
	sum := 0.0
	found := false
	for _, p := range packs {
		limit, _ := asFloat(jget(jget(jget(p, "entitlement_base_info"), "quota"), "credits_limit"))
		consumed, _ := asFloat(jget(jget(p, "usage"), "credits_amount"))
		if limit > 0 {
			found = true
			if limit-consumed > 0 {
				sum += limit - consumed
			}
		}
	}
	if !found {
		return nil
	}
	return &sum
}

/* ------------------------------------------------------------------ */
/* 单个账号的完整签到流程                                               */
/* ------------------------------------------------------------------ */

type signinResult struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	State   string   `json:"state"`
	Msg     string   `json:"msg"`
	Credits *float64 `json:"credits,omitempty"`
	Points  *float64 `json:"points,omitempty"`
	Streak  *float64 `json:"streak,omitempty"`
	Total   *float64 `json:"total,omitempty"`
}

func (a *App) traeSigninOne(ctx context.Context, acct *TraeAccount, force bool) signinResult {
	now := time.Now().UnixMilli()
	today := bjDay(now, a.cfg.Loc)
	base := signinResult{ID: acct.ID, Label: acct.Label}

	if !force && acct.CooldownUntil > now {
		left := (acct.CooldownUntil - now + 59999) / 60000
		return signinResult{ID: base.ID, Label: base.Label, State: "cooldown",
			Msg: fmt.Sprintf("限流冷却中，约 %d 分钟", left)}
	}
	if !force && acct.LastOkDate == today {
		return signinResult{ID: base.ID, Label: base.Label, State: "skipped", Msg: "今日已完成"}
	}

	// 1) 拿一个可用的 accessToken：还没过期就直接用，避免无谓轮换
	token := acct.AccessToken
	exp := jwtExp(token)
	usable := token != "" && (exp == 0 || exp*1000-now > 120000)

	if !usable {
		pair, err := a.refreshTraeTokens(ctx, acct)
		if err != nil {
			acct.LastState = "token_dead"
			acct.LastMsg = err.Error()
			acct.LastRun = now
			_ = a.traePut(acct)
			return signinResult{ID: acct.ID, Label: acct.Label, State: "token_dead", Msg: err.Error()}
		}
		token = pair.AccessToken
		acct.RefreshToken = pair.RefreshToken
		acct.AccessToken = token
		acct.AccessExp = jwtExp(token)
		acct.TokenRefreshedAt = now
		_ = a.traePut(acct) // 先落盘：令牌链不能断
	}

	// 2) 提交签到（鉴权类错误刷新一次再试）
	res := a.claimTrae(ctx, token, acct)
	if traeAuthFailCodes[res.Code] {
		pair, err := a.refreshTraeTokens(ctx, acct)
		if err != nil {
			acct.LastState = "token_dead"
			acct.LastMsg = err.Error()
			acct.LastRun = now
			_ = a.traePut(acct)
			return signinResult{ID: acct.ID, Label: acct.Label, State: "token_dead", Msg: err.Error()}
		}
		token = pair.AccessToken
		acct.RefreshToken = pair.RefreshToken
		acct.AccessToken = token
		acct.AccessExp = jwtExp(token)
		_ = a.traePut(acct)
		res = a.claimTrae(ctx, token, acct)
	}

	acct.LastRun = now

	// 3) 限流：按账号冷却等下轮
	if res.Code == traeCodeRate || res.Status == 429 || res.Status >= 500 {
		mins := a.traeCooldownMinutes()
		acct.CooldownUntil = now + int64(mins)*60000
		acct.LastState = "rate"
		acct.LastMsg = fmt.Sprintf("限流 9074，冷却 %d 分钟", mins)
		_ = a.traePut(acct)
		return signinResult{ID: acct.ID, Label: acct.Label, State: "rate", Msg: acct.LastMsg}
	}

	// 4) 今日已签
	if res.Code == traeCodeAlready {
		acct.LastState = "already"
		acct.LastMsg = "今日已签"
		acct.LastOkDate = today
		acct.CooldownUntil = 0
		if st := a.traeStatusOf(ctx, token, acct); st.OK {
			if st.Credits != nil {
				acct.Credits = st.Credits
			}
			acct.CheckedIn = true
		}
		_ = a.traePut(acct)
		return signinResult{ID: acct.ID, Label: acct.Label, State: "already", Msg: "今日已签", Credits: acct.Credits}
	}

	// 5) 其它失败
	if res.Code != traeCodeOK {
		acct.LastState = "error"
		acct.LastMsg = strings.TrimSpace(fmt.Sprintf("签到失败 code=%s %s", fmtCode(res.Code), res.Msg))
		_ = a.traePut(acct)
		return signinResult{ID: acct.ID, Label: acct.Label, State: "error", Msg: acct.LastMsg}
	}

	// 6) code=0 未必真到账：用同一 deviceId 回查 checked_in 是否翻转
	confirmed := false
	var credits *float64
	if st := a.traeStatusOf(ctx, token, acct); st.OK {
		confirmed = st.Checked
		credits = st.Credits
	}

	if !confirmed {
		bal := a.traeCreditsOf(ctx, token, acct)
		acct.LastState = "pending"
		acct.LastMsg = "已提交，但回查未确认，等下轮再试"
		acct.Credits = firstNonNil(bal, credits, acct.Credits)
		acct.CooldownUntil = now + 12*60000
		_ = a.traePut(acct)
		return signinResult{ID: acct.ID, Label: acct.Label, State: "pending", Msg: acct.LastMsg, Credits: acct.Credits}
	}

	bal := a.traeCreditsOf(ctx, token, acct)
	acct.LastState = "checked"
	if res.Points != nil {
		acct.LastMsg = "+" + fmtNum(*res.Points) + " 积分"
	} else {
		acct.LastMsg = "签到成功"
	}
	acct.LastOkDate = today
	acct.LastCredit = res.Points
	if res.Points != nil {
		acct.TotalCredit += *res.Points
	}
	acct.TotalSigned++
	acct.Credits = firstNonNil(bal, credits, acct.Credits)
	acct.CheckedIn = true
	acct.CooldownUntil = 0
	_ = a.traePut(acct)

	return signinResult{ID: acct.ID, Label: acct.Label, State: "checked", Msg: acct.LastMsg,
		Credits: acct.Credits, Points: res.Points}
}

func firstNonNil(vals ...*float64) *float64 {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func fmtCode(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func fmtNum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

/* ------------------------------------------------------------------ */
/* 扫码登录：授权链接 → 回调 → 换令牌 → 落库                             */
/* ------------------------------------------------------------------ */

func pkcePair() (verifier, challenge string) {
	verifier = randomHex(32)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, b64url(sum[:])
}

func qsQuote(v string) string {
	return strings.ReplaceAll(encodeURIComponent(v), "%2F", "/")
}

func buildAuthorizeURL(authFrom, clientID, trace, callback, deviceID, machineID, challenge string) string {
	params := [][2]string{
		{"login_version", "1"},
		{"auth_from", authFrom},
		{"login_channel", "native_ide"},
		{"plugin_version", oauthPluginVersion},
		{"auth_type", "local"},
		{"client_id", clientID},
		{"redirect", "0"},
		{"login_trace_id", trace},
		{"auth_callback_url", callback},
		{"machine_id", machineID},
		{"device_id", deviceID},
		{"x_device_id", deviceID},
		{"x_machine_id", machineID},
		{"x_device_brand", "PC"},
		{"x_device_type", "Windows"},
		{"x_os_version", "10.0.19045"},
		{"x_env", ""},
		{"x_app_version", oauthAppVersion},
		{"x_app_type", "stable"},
		{"code_challenge", challenge},
		{"code_challenge_method", "S256"},
		{"channel_name", "common"},
	}
	parts := make([]string, 0, len(params))
	for _, p := range params {
		parts = append(parts, p[0]+"="+qsQuote(p[1]))
	}
	return authorizeBase + "/authorization?" + strings.Join(parts, "&")
}

func (a *App) putTraeLoginSession(sess *traeLoginSession) error {
	sealed, err := sealRecord(a.cfg.MasterKey, infoTrae, sess)
	if err != nil {
		return err
	}
	if err := a.traeKV().Put("login:"+sess.Trace, sealed, traeLoginTTL); err != nil {
		return err
	}
	return a.traeKV().Put(traeLatestKey, sess.Trace, traeLoginTTL)
}

func (a *App) getTraeLoginSession(trace string) *traeLoginSession {
	raw, ok := a.traeKV().Get("login:" + trace)
	if !ok {
		return nil
	}
	var sess traeLoginSession
	if err := openRecord(a.cfg.MasterKey, infoTrae, raw, &sess); err != nil {
		return nil
	}
	return &sess
}

func (a *App) latestTraeTrace() string {
	v, _ := a.traeKV().Get(traeLatestKey)
	return v
}

type callbackParams struct {
	AuthCode     string
	RefreshToken string
	Trace        string
	Host         string
	Name         string
	UID          string
	Error        string
}

// parseCallbackParams 回调参数解析：AuthCode 可能在 authCodeInfo JSON 里，也可能直接是 code
func parseCallbackParams(u *url.URL) callbackParams {
	q := u.Query()
	var p callbackParams
	if raw := q.Get("authCodeInfo"); raw != "" {
		var info map[string]any
		if err := json.Unmarshal([]byte(raw), &info); err == nil {
			p.AuthCode = strings.TrimSpace(asString(jget(info, "AuthCode")))
		}
	}
	if p.AuthCode == "" {
		p.AuthCode = strings.TrimSpace(firstNonEmpty(q.Get("code"), q.Get("AuthCode")))
	}
	for _, k := range []string{"refreshToken", "refresh_token", "RefreshToken"} {
		if v := strings.Trim(strings.TrimSpace(q.Get(k)), `"`); v != "" {
			p.RefreshToken = v
			break
		}
	}
	p.Trace = strings.TrimSpace(firstNonEmpty(q.Get("loginTraceID"), q.Get("login_trace_id"), q.Get("state")))
	p.Host = strings.TrimSpace(q.Get("host"))
	p.Name = strings.TrimSpace(firstNonEmpty(q.Get("name"), q.Get("nickname"), q.Get("userName")))
	p.UID = strings.TrimSpace(q.Get("uid"))
	p.Error = strings.TrimSpace(q.Get("error"))
	return p
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// withCloudIDEToken 复制一份头再加空值的 x-cloudide-token（上游要求该头存在且为空）
func withCloudIDEToken(base map[string]string) map[string]string {
	out := make(map[string]string, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out["x-cloudide-token"] = ""
	return out
}

func hostBase(h string) string {
	s := strings.TrimSpace(h)
	if s == "" {
		return traeAPIBase
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return strings.TrimRight(s, "/")
	}
	return "https://" + strings.TrimRight(s, "/")
}

var (
	accessKeys  = []string{"AccessToken", "access_token", "token", "Jwt", "JWT"}
	refreshKeys = []string{"RefreshToken", "refresh_token"}
)

var nickKeys = []string{
	"Nickname", "nickname", "NickName", "nick_name",
	"UserName", "userName", "user_name", "username",
	"ScreenName", "screen_name",
	"DisplayName", "displayName", "display_name",
	"Name", "name",
}

var genericNames = regexp.MustCompile(`^(?i)(pc|windows|web|ide|trae|unknown)$`)

// pickNickname 递归找昵称：只认白名单字段，避免误抓 DeviceName 之类的值
func pickNickname(v any, depth int) string {
	if v == nil || depth > 5 {
		return ""
	}
	if m := asMap(v); m != nil {
		for _, k := range nickKeys {
			if s, ok := m[k]; ok {
				if str, isStr := s.(string); isStr {
					str = strings.TrimSpace(str)
					if str != "" && len([]rune(str)) <= 32 && !genericNames.MatchString(str) {
						return str
					}
				}
			}
		}
		for _, mv := range m {
			if r := pickNickname(mv, depth+1); r != "" {
				return r
			}
		}
		return ""
	}
	for _, mv := range asSlice(v) {
		if r := pickNickname(mv, depth+1); r != "" {
			return r
		}
	}
	return ""
}

// nicknameFromJwt 令牌是 JWT 时，载荷里常常带昵称；只取来当备注，不校验签名
func nicknameFromJWT(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) < 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return ""
	}
	return pickNickname(m, 0)
}

func describeExchangeErr(res httpResult) string {
	code := ""
	msg := ""
	if errObj := jget(jget(res.JSON, "ResponseMetadata"), "Error"); errObj != nil {
		code = asString(jget(errObj, "Code"))
		if data := jget(errObj, "Data"); data != nil {
			msg = asString(jget(data, "__Message.error"))
		}
		if msg == "" {
			msg = asString(jget(errObj, "Message"))
		}
	}
	if errObj := jget(res.JSON, "error"); errObj != nil {
		if code == "" {
			code = asString(jget(errObj, "Code"))
		}
		if msg == "" {
			msg = asString(jget(errObj, "Message"))
		}
	}
	if code == "" {
		code = asString(jget(res.JSON, "code"))
	}
	if msg == "" {
		msg = firstNonEmpty(asString(jget(res.JSON, "message")), asString(jget(res.JSON, "msg")))
	}
	if msg == "" {
		msg = truncate(res.Text, 160)
	}
	if code == "" {
		code = "?"
	}
	return fmt.Sprintf("code=%s %s (HTTP %d)", code, msg, res.Status)
}

// exchangeAuthCode AuthCode → 双令牌。后端有几套不同版本的兑换端点，逐个尝试。
func (a *App) exchangeTraeAuthCode(ctx context.Context, authCode, verifier, host, clientID, platform string, sess *traeLoginSession) (tokenPair, string, string, error) {
	dev := map[string]any{
		"DeviceID":        sess.DeviceID,
		"MachineID":       sess.MachineID,
		"PlatformCode":    platform,
		"DeviceType":      "PC",
		"DeviceName":      "PC",
		"DeviceModel":     "",
		"ClientVersion":   oauthAppVersion,
		"DevicePublicKey": sess.PublicKeyPEM,
		"DeviceBrand":     "",
		"DeviceCPU":       "",
		"OSInfo":          "Windows",
		"OSVersion":       "10.0.19045",
	}
	baseHeaders := map[string]string{
		"Accept":          "*/*",
		"User-Agent":      "Trae/" + oauthAppVersion,
		"x-device-id":     sess.DeviceID,
		"x-app-id":        oauthAppID,
		"x-platform-code": platform,
	}

	variants := []struct {
		tag     string
		url     string
		headers map[string]string
		body    map[string]any
	}{
		{
			tag:     "DeviceInfo",
			url:     hostBase(host) + epTraeExchange,
			headers: withCloudIDEToken(baseHeaders),
			body: map[string]any{
				"ClientID":     clientID,
				"AuthCode":     authCode,
				"CodeVerifier": verifier,
				"DeviceInfo":   dev,
				"IDEVersion":   oauthAppVersion,
			},
		},
		{
			tag:     "AuthCode",
			url:     traeExchangeLegacyEP,
			headers: baseHeaders,
			body: map[string]any{
				"ClientID":     clientID,
				"AuthCode":     authCode,
				"CodeVerifier": verifier,
				"DeviceID":     sess.DeviceID,
				"PlatformCode": platform,
			},
		},
		{
			tag:     "Code",
			url:     traeExchangeLegacyEP,
			headers: baseHeaders,
			body: map[string]any{
				"ClientID":     clientID,
				"Code":         authCode,
				"CodeVerifier": verifier,
				"DeviceID":     sess.DeviceID,
				"PlatformCode": platform,
			},
		},
	}

	var errs []string
	for _, v := range variants {
		res, err := traeRequest(ctx, "POST", v.url, v.headers, v.body)
		if err != nil {
			errs = append(errs, v.tag+": "+err.Error())
			continue
		}
		rt := deepFindString(res.JSON, refreshKeys, 0)
		at := deepFindString(res.JSON, accessKeys, 0)
		if rt != "" {
			name := pickNickname(res.JSON, 0)
			if name == "" {
				name = nicknameFromJWT(at)
			}
			return tokenPair{AccessToken: at, RefreshToken: rt}, v.tag, name, nil
		}
		if at != "" {
			errs = append(errs, v.tag+": 响应只有 AccessToken，没有 RefreshToken（无法续期）"+describeExchangeErr(res))
		} else {
			errs = append(errs, v.tag+": "+describeExchangeErr(res))
		}
	}
	return tokenPair{}, "", "", errors.New("登录失败 → " + strings.Join(errs, " | "))
}

func (a *App) saveScannedTraeAccount(label, refreshToken string, sess *traeLoginSession) (*TraeAccount, error) {
	rt := strings.TrimSpace(refreshToken)
	if rt == "" {
		return nil, errors.New("登录响应里没有 refreshToken")
	}
	id := randToken(9)
	suffix := sess.DeviceID
	if len(suffix) > 4 {
		suffix = suffix[len(suffix)-4:]
	}
	acct := &TraeAccount{
		ID:            id,
		Label:         strings.TrimSpace(truncate(label, 40)),
		RefreshToken:  rt,
		DeviceID:      sess.DeviceID,
		MachineID:     sess.MachineID,
		PrivateKeyPEM: sess.PrivateKeyPEM,
		PublicKeyPEM:  sess.PublicKeyPEM,
		DeviceName:    "PC",
		DeviceType:    "Windows",
		OSInfo:        "Windows",
		OSVersion:     "10.0.19045",
		ClientID:      sess.ClientID,
		PlatformCode:  sess.Platform,
		Source:        "scan",
		CreatedAt:     time.Now().UnixMilli(),
		LastState:     "new",
		LastMsg:       "扫码登录，尚未运行",
	}
	if acct.Label == "" {
		acct.Label = "Trae-" + suffix
	}
	if err := a.traePut(acct); err != nil {
		return nil, err
	}
	list, err := a.traeListMeta()
	if err != nil {
		return nil, err
	}
	list = append(list, accountMeta{ID: acct.ID, Label: acct.Label, CreatedAt: acct.CreatedAt})
	if err := a.traeSaveIndex(list); err != nil {
		return nil, err
	}
	return acct, nil
}

type loginOutcome struct {
	Label  string
	ID     string
	Via    string
	Repeat bool
}

func (a *App) completeTraeLogin(ctx context.Context, sess *traeLoginSession, p callbackParams) (loginOutcome, error) {
	if sess.Status == "ok" {
		return loginOutcome{Label: sess.Label, ID: sess.AccountID, Repeat: true}, nil
	}

	refreshToken := p.RefreshToken
	via := "callback"
	nick := p.Name
	if nick == "" {
		nick = nicknameFromJWT(p.RefreshToken)
	}
	if refreshToken == "" {
		if p.AuthCode == "" {
			return loginOutcome{}, errors.New("回调里既没有 AuthCode 也没有 refreshToken")
		}
		pair, tag, name, err := a.exchangeTraeAuthCode(ctx, p.AuthCode, sess.Verifier, p.Host, sess.ClientID, sess.Platform, sess)
		if err != nil {
			return loginOutcome{}, err
		}
		refreshToken = pair.RefreshToken
		via = tag
		if nick == "" {
			nick = name
		}
	}

	label := strings.TrimSpace(nick)
	if label == "" {
		label = strings.TrimSpace(p.UID)
	}
	acct, err := a.saveScannedTraeAccount(label, refreshToken, sess)
	if err != nil {
		return loginOutcome{}, err
	}

	sess.Status = "ok"
	sess.Label = acct.Label
	sess.AccountID = acct.ID
	sess.Via = via
	sess.Error = ""
	_ = a.putTraeLoginSession(sess)
	return loginOutcome{Label: acct.Label, ID: acct.ID, Via: via}, nil
}
