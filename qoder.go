package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

/* ================================================================== */
/* Qoder（中国版）—— 设备登录 / 令牌续期 / 每日领取 Credits              */
/*                                                                    */
/* 协议相关的一切都在这个文件里：端点、请求头、活动状态码。               */
/* 官方接口变了只改这里，别的地方不用动。                                */
/* 下面的变量名 qoderAPIBase / qoderWebBase 供测试指向假服务。            */
/* ================================================================== */

const (
	qoderAPI = "https://openapi.qoder.com.cn"
	qoderWeb = "https://qoder.com.cn"

	epQoderRefresh   = "/api/v1/deviceToken/refresh"
	epQoderPoll      = "/api/v1/deviceToken/poll"
	epQoderCampaigns = "/sash/api/v1/me/campaigns"
	epQoderClaim     = "/sash/api/v1/me/campaigns/%s/claim"

	// 登录页与轮询用的客户端标识不一样，官方两套接口各认各的
	qoderUAAPI    = "Qoder/claim"
	qoderUAPoll   = "qoder-local-proxy"
	qoderVer      = "1.0.1"
	qoderTypeAPI  = "10"
	qoderTypePoll = "5"

	// 活动筛选口径
	qoderActionBenefit   = "CLAIM_BENEFIT"
	qoderStatusClaimable = "CLAIMABLE"
	qoderStatusClaimed   = "CLAIMED"

	qoderLoginTTL    = 10 * time.Minute
	qoderPollEveryMS = 2000
	qoderTimeout     = 20 * time.Second

	// 签到前令牌剩余有效期不足这么多，就先刷新
	qoderRefreshBeforeMS = 2 * 60 * 60 * 1000
)

var (
	qoderAPIBase = qoderAPI
	qoderWebBase = qoderWeb
)

type qoderLoginSession struct {
	Nonce     string `json:"nonce"`
	Verifier  string `json:"verifier"`
	Challenge string `json:"challenge"`
	MachineID string `json:"machineId"`
	Status    string `json:"status"`
	Label     string `json:"label"`
	AccountID string `json:"accountId"`
	Error     string `json:"error"`
	CreatedAt int64  `json:"createdAt"`
}

type qoderCampaign struct {
	ID      string
	Status  string
	Amount  float64
	StartAt int64
	EndAt   int64
}

/* ------------------------------------------------------------------ */
/* HTTP                                                               */
/* ------------------------------------------------------------------ */

func qoderDo(ctx context.Context, method, path, token string, body any, poll bool) (httpResult, error) {
	target := strings.TrimSuffix(qoderAPIBase, "/") + path
	h := map[string]string{"Accept": "application/json"}
	if poll {
		h["User-Agent"] = qoderUAPoll
		h["Cosy-Version"] = qoderVer
		h["Cosy-ClientType"] = qoderTypePoll
	} else {
		h["User-Agent"] = qoderUAAPI
		h["Cosy-ClientType"] = qoderTypeAPI
	}
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	return doJSON(ctx, method, target, h, body, qoderTimeout)
}

func qoderErrText(res httpResult, prefix string) string {
	msg := firstNonEmpty(
		asString(jget(res.JSON, "msg")),
		asString(jget(res.JSON, "message")),
		asString(jget(res.JSON, "errorMessage")),
	)
	if msg == "" {
		msg = truncate(res.Text, 120)
	}
	return fmt.Sprintf("%s HTTP %d %s", prefix, res.Status, msg)
}

/* ------------------------------------------------------------------ */
/* 令牌                                                               */
/* ------------------------------------------------------------------ */

// qoderRefreshTokens 换一对新令牌；响应里没有新 refreshToken 时沿用旧的。
func (a *App) qoderRefreshTokens(ctx context.Context, refreshToken string) (tokenPair, error) {
	res, err := qoderDo(ctx, http.MethodPost, epQoderRefresh, "", map[string]any{"refresh_token": refreshToken}, false)
	if err != nil {
		return tokenPair{}, err
	}
	if res.Status == 401 || res.Status == 403 {
		return tokenPair{}, errors.New("refreshToken 已失效，请重新登录")
	}
	if res.Status != 200 {
		return tokenPair{}, errors.New(qoderErrText(res, "刷新失败"))
	}
	data := jget(res.JSON, "data")
	if data == nil {
		data = res.JSON
	}
	at := firstNonEmpty(
		asString(jget(data, "token")),
		asString(jget(data, "accessToken")),
		asString(jget(data, "access_token")),
	)
	if at == "" {
		return tokenPair{}, errors.New("刷新响应里没有 accessToken")
	}
	rt := firstNonEmpty(
		asString(jget(data, "refreshToken")),
		asString(jget(data, "refresh_token")),
	)
	if rt == "" {
		rt = refreshToken
	}
	return tokenPair{AccessToken: at, RefreshToken: rt}, nil
}

/* ------------------------------------------------------------------ */
/* 设备登录：打开官方选账号页 → 本服务后台轮询                            */
/* ------------------------------------------------------------------ */

// uuidLike 造一个 uuid 形状的机器标识，官方只要求它是个稳定的随机串。
func uuidLike() string {
	h := randomHex(16)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func (a *App) qoderStartLogin() (*qoderLoginSession, string, error) {
	verifier := randToken(32)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	nonce := randomHex(16)
	machineID := uuidLike()

	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("machine_id", machineID)
	q.Set("nonce", nonce)
	authURL := strings.TrimSuffix(qoderWebBase, "/") + "/device/selectAccounts?" + q.Encode()

	sess := &qoderLoginSession{
		Nonce:     nonce,
		Verifier:  verifier,
		Challenge: challenge,
		MachineID: machineID,
		Status:    "pending",
		CreatedAt: time.Now().UnixMilli(),
	}
	return sess, authURL, nil
}

// qoderPollOnce 返回 pending 表示还没确认，其余情况给出令牌或错误。
func (a *App) qoderPollOnce(ctx context.Context, sess *qoderLoginSession) (bool, tokenPair, error) {
	q := url.Values{}
	q.Set("nonce", sess.Nonce)
	q.Set("verifier", sess.Verifier)
	q.Set("challenge_method", "S256")

	res, err := qoderDo(ctx, http.MethodGet, epQoderPoll+"?"+q.Encode(), "", nil, true)
	if err != nil {
		return false, tokenPair{}, err
	}
	if res.Status == 202 || res.Status == 404 {
		return true, tokenPair{}, nil // 用户还没在页面上确认
	}
	if res.Status >= 400 {
		return false, tokenPair{}, errors.New(qoderErrText(res, "授权轮询失败"))
	}

	data := jget(res.JSON, "data")
	if data == nil {
		data = res.JSON
	}
	at := firstNonEmpty(
		asString(jget(data, "token")),
		asString(jget(data, "access_token")),
		asString(jget(data, "device_token")),
	)
	if at == "" {
		if code := asFloat0(jget(res.JSON, "code")); code != 0 {
			return false, tokenPair{}, fmt.Errorf("授权失败 code=%s %s",
				fmtCode(code), firstNonEmpty(asString(jget(res.JSON, "msg")), asString(jget(res.JSON, "message"))))
		}
		return true, tokenPair{}, nil
	}
	rt := firstNonEmpty(
		asString(jget(data, "refresh_token")),
		asString(jget(data, "refreshToken")),
	)
	return false, tokenPair{AccessToken: at, RefreshToken: rt}, nil
}

/* ------------------------------------------------------------------ */
/* 活动与领取                                                          */
/* ------------------------------------------------------------------ */

func (a *App) qoderCampaigns(ctx context.Context, at string) ([]qoderCampaign, error) {
	res, err := qoderDo(ctx, http.MethodGet, epQoderCampaigns+"?forceRefresh=true", at, nil, false)
	if err != nil {
		return nil, err
	}
	if res.Status != 200 {
		return nil, errors.New(qoderErrText(res, "查询活动失败"))
	}
	items := asSlice(jget(res.JSON, "campaigns"))
	if items == nil {
		items = asSlice(jget(jget(res.JSON, "data"), "campaigns"))
	}
	out := make([]qoderCampaign, 0, len(items))
	for _, it := range items {
		m := asMap(it)
		if m == nil {
			continue
		}
		if asString(jget(m, "actionType")) != qoderActionBenefit {
			continue
		}
		amount, _ := asFloat(jget(jget(m, "benefit"), "amount"))
		out = append(out, qoderCampaign{
			ID:      firstNonEmpty(asString(jget(m, "campaignId")), asString(jget(m, "id"))),
			Status:  asString(jget(m, "claimStatus")),
			Amount:  amount,
			StartAt: qoderTimeSec(jget(m, "startAt")),
			EndAt:   qoderTimeSec(jget(m, "endAt")),
		})
	}
	return out, nil
}

// qoderTimeSec 有的版本给秒、有的给毫秒，统一成秒。
func qoderTimeSec(v any) int64 {
	f, ok := asFloat(v)
	if !ok {
		return 0
	}
	if f > 1e12 {
		return int64(f / 1000)
	}
	return int64(f)
}

func (a *App) qoderClaim(ctx context.Context, at, campaignID string) (float64, error) {
	path := fmt.Sprintf(epQoderClaim, url.PathEscape(campaignID))
	res, err := qoderDo(ctx, http.MethodPost, path, at, map[string]any{}, false)
	if err != nil {
		return 0, err
	}
	if res.Status != 200 {
		return 0, errors.New(qoderErrText(res, "领取失败"))
	}
	data := jget(res.JSON, "data")
	if data == nil {
		data = res.JSON
	}
	status := asString(jget(data, "status"))
	if status != "" && status != qoderStatusClaimed {
		return 0, fmt.Errorf("领取未成功：%s", status)
	}
	amount, _ := asFloat(jget(jget(data, "benefit"), "amount"))
	return amount, nil
}

/* ------------------------------------------------------------------ */
/* 单个账号的领取流程                                                   */
/* ------------------------------------------------------------------ */

func (a *App) qoderSigninOne(ctx context.Context, acct *Account, force bool) signinResult {
	now := time.Now().UnixMilli()
	today := bjDay(now, a.cfg.Loc)
	base := signinResult{ID: acct.ID, Label: acct.Label}

	if !force && acct.LastOkDate == today {
		return signinResult{ID: base.ID, Label: base.Label, State: "skipped", Msg: "今日已完成"}
	}

	// 1) 令牌：快到期就先换一对
	at := acct.AccessToken
	exp := jwtExp(at)
	needRefresh := at == "" || exp == 0 || exp*1000-now < qoderRefreshBeforeMS
	if needRefresh || force {
		pair, err := a.qoderRefreshTokens(ctx, acct.RefreshToken)
		if err != nil {
			if !(at != "" && (exp == 0 || exp*1000 > now)) {
				acct.LastState = "token_dead"
				acct.LastMsg = err.Error()
				acct.LastRun = now
				_ = a.qoderPut(acct)
				return signinResult{ID: base.ID, Label: base.Label, State: "token_dead", Msg: err.Error()}
			}
			// 旧的还没过期，先凑合用
		} else {
			at = pair.AccessToken
			acct.RefreshToken = pair.RefreshToken
			acct.AccessToken = at
			acct.AccessExp = jwtExp(at)
			acct.TokenRefreshedAt = now
			exp = acct.AccessExp
			_ = a.qoderPut(acct) // 令牌链不能断
		}
	}

	// 2) 活动列表：401 就刷新一次再试
	camps, err := a.qoderCampaigns(ctx, at)
	if err != nil && strings.Contains(err.Error(), "HTTP 401") {
		if pair, rerr := a.qoderRefreshTokens(ctx, acct.RefreshToken); rerr == nil {
			at = pair.AccessToken
			acct.RefreshToken = pair.RefreshToken
			acct.AccessToken = at
			acct.AccessExp = jwtExp(at)
			acct.TokenRefreshedAt = now
			_ = a.qoderPut(acct)
			camps, err = a.qoderCampaigns(ctx, at)
		}
	}
	if err != nil {
		state := "error"
		if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "refreshToken 已失效") {
			state = "token_dead"
		}
		acct.LastState = state
		acct.LastMsg = err.Error()
		acct.LastRun = now
		_ = a.qoderPut(acct)
		return signinResult{ID: base.ID, Label: base.Label, State: state, Msg: err.Error()}
	}

	acct.LastRun = now
	benefits := make([]qoderCampaign, 0, len(camps))
	claimable := make([]qoderCampaign, 0, len(camps))
	for _, c := range camps {
		benefits = append(benefits, c)
		if c.Status == qoderStatusClaimable && c.ID != "" {
			claimable = append(claimable, c)
		}
	}

	// 3) 有可领的，逐个领
	if len(claimable) > 0 {
		got, failed := 0.0, 0
		var lastErr string
		for _, c := range claimable {
			amount, cerr := a.qoderClaim(ctx, at, c.ID)
			if cerr != nil {
				failed++
				lastErr = cerr.Error()
				continue
			}
			if amount <= 0 {
				amount = c.Amount
			}
			got += amount
		}
		if failed > 0 {
			acct.LastState = "error"
			acct.LastMsg = fmt.Sprintf("%d/%d 个活动领取失败：%s", failed, len(claimable), lastErr)
			_ = a.qoderPut(acct)
			return signinResult{ID: base.ID, Label: base.Label, State: "error", Msg: acct.LastMsg}
		}
		acct.LastState = "checked"
		acct.LastMsg = fmt.Sprintf("+%s Credits", fmtNum(got))
		acct.LastOkDate = today
		credit := got
		acct.LastCredit = &credit
		acct.TotalCredit += got
		acct.TotalSigned++
		_ = a.qoderPut(acct)
		return signinResult{ID: base.ID, Label: base.Label, State: "checked", Msg: acct.LastMsg,
			Points: &credit, Total: &acct.TotalCredit}
	}

	// 4) 今天的活动已经领过
	nowSec := now / 1000
	for _, c := range benefits {
		if c.Status == qoderStatusClaimed && (c.StartAt == 0 || c.StartAt <= nowSec) && (c.EndAt == 0 || nowSec < c.EndAt) {
			acct.LastState = "already"
			acct.LastMsg = "今日已领取"
			acct.LastOkDate = today
			_ = a.qoderPut(acct)
			return signinResult{ID: base.ID, Label: base.Label, State: "already", Msg: "今日已领取",
				Total: &acct.TotalCredit}
		}
	}

	// 5) 活动还没下发，等下轮再试（不写 LastOkDate，避免误判成已签）
	acct.LastState = "pending"
	acct.LastMsg = "活动未下发，等下轮再试"
	_ = a.qoderPut(acct)
	return signinResult{ID: base.ID, Label: base.Label, State: "pending", Msg: acct.LastMsg}
}

/* ------------------------------------------------------------------ */
/* 存取（走通用层，前缀 qoder:）                                        */
/* ------------------------------------------------------------------ */

func (a *App) qoderGet(id string) (*Account, error) { return a.storeOf("qoder").Get(id) }

func (a *App) qoderPut(acct *Account) error { return a.storeOf("qoder").Put(acct) }

func (a *App) qoderAppend(acct *Account) error { return a.storeOf("qoder").Append(acct) }

/* ================================================================== */
/* 适配器                                                              */
/* ================================================================== */

type qoderPlatform struct{}

func (p *qoderPlatform) Key() string     { return "qoder" }
func (p *qoderPlatform) Label() string   { return "Qoder" }
func (p *qoderPlatform) Flow() LoginFlow { return FlowLink }

func (p *qoderPlatform) Caps() Caps { return Caps{} }

func (p *qoderPlatform) Metrics() []Metric {
	return []Metric{{K: "totalCredit", Label: "累计 Credits"}}
}

func (p *qoderPlatform) Describe(acct *Account, v viewCtx) (string, string, string) {
	switch {
	case v.SignedToday:
		if acct.LastState == "checked" {
			return "已领", "ok", acct.LastMsg
		}
		return "已领", "ok", ""
	case acct.LastState == "token_dead":
		return "令牌失效", "bad", acct.LastMsg
	case acct.LastState == "pending" && v.RanToday:
		return "活动未下发", "warn", acct.LastMsg
	case acct.LastState == "error" && v.RanToday:
		return "领取失败", "bad", acct.LastMsg
	}
	return "今日未领", "idle", ""
}

func (p *qoderPlatform) StartLogin(ctx context.Context, a *App) (LoginHandle, error) {
	sess, authURL, err := a.qoderStartLogin()
	if err != nil {
		return LoginHandle{}, err
	}
	if err := a.storeOf("qoder").PutSession(sess.Nonce, sess, qoderLoginTTL); err != nil {
		return LoginHandle{}, err
	}
	return LoginHandle{
		ID:         sess.Nonce,
		AuthURL:    authURL,
		IntervalMS: qoderPollEveryMS,
		ExpiresAt:  sess.CreatedAt + int64(qoderLoginTTL/time.Millisecond),
	}, nil
}

func (p *qoderPlatform) PollLogin(ctx context.Context, a *App, id string) (LoginPoll, error) {
	st := a.storeOf("qoder")
	var sess qoderLoginSession
	if !st.GetSession(id, &sess) {
		return LoginPoll{Status: "expired"}, nil
	}
	if sess.Status == "ok" {
		return LoginPoll{Status: "ok", ID: sess.AccountID, Label: sess.Label}, nil
	}
	pending, tokens, err := a.qoderPollOnce(ctx, &sess)
	if err != nil {
		sess.Status = "error"
		sess.Error = err.Error()
		_ = st.PutSession(id, &sess, qoderLoginTTL)
		return LoginPoll{}, err
	}
	if pending {
		return LoginPoll{Status: "pending"}, nil
	}
	if tokens.RefreshToken == "" {
		return LoginPoll{}, errors.New("登录成功但服务端没返回 refreshToken")
	}
	acct, err := a.qoderPersist(tokens)
	if err != nil {
		return LoginPoll{}, err
	}
	sess.Status = "ok"
	sess.AccountID = acct.ID
	sess.Label = acct.Label
	_ = st.PutSession(id, &sess, time.Minute)
	return LoginPoll{Status: "ok", ID: acct.ID, Label: acct.Label}, nil
}

func (p *qoderPlatform) CompleteLogin(ctx context.Context, a *App, id string, in LoginInput) (*LoginOutcome, error) {
	return nil, errors.New("Qoder 在打开的页面里完成登录即可，不需要粘贴地址")
}

func (p *qoderPlatform) Signin(ctx context.Context, a *App, acct *Account, force bool) signinResult {
	return a.qoderSigninOne(ctx, acct, force)
}

func (p *qoderPlatform) Keepalive(ctx context.Context, a *App, acct *Account) error {
	now := time.Now().UnixMilli()
	if exp := jwtExp(acct.AccessToken); exp != 0 && exp*1000-now > keepaliveMarginMS {
		return nil
	}
	pair, err := a.qoderRefreshTokens(ctx, acct.RefreshToken)
	if err != nil {
		return err
	}
	acct.AccessToken = pair.AccessToken
	acct.RefreshToken = pair.RefreshToken
	acct.AccessExp = jwtExp(pair.AccessToken)
	acct.TokenRefreshedAt = now
	return a.qoderPut(acct)
}

func (a *App) qoderPersist(tokens tokenPair) (*Account, error) {
	now := time.Now().UnixMilli()
	id := randToken(9)
	label := strings.TrimSpace(nicknameFromJWT(tokens.AccessToken))
	if label == "" {
		label = "Qoder-" + truncate(id, 4)
	}
	acct := &Account{
		ID:               id,
		Label:            label,
		RefreshToken:     tokens.RefreshToken,
		AccessToken:      tokens.AccessToken,
		AccessExp:        jwtExp(tokens.AccessToken),
		TokenRefreshedAt: now,
		CreatedAt:        now,
		LastState:        "new",
		LastMsg:          "设备登录，尚未运行",
		Ext:              map[string]any{"source": "device"},
	}
	if err := a.qoderAppend(acct); err != nil {
		return nil, err
	}
	return acct, nil
}
