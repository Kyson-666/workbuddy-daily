# DuMate 签到接口实测记录（阶段 0，2026-11-13）

> 实测机：本机 Windows，登录态来自 dumate-proxy 浏览器登录（60 条 cookie， accountId `e53e…9501`）
> 状态：**已实测，字段映射锁定**。今日（2026-11-13）该账号已签（`hasIssued:true`），未签成功样例待明日自然验证。

## 一、接口结构（实测确认）

### GET /api/dumate/points/loginBonusInfo（Cookie 认证）

```json
{
  "logId": "d11c792d-…",
  "code": 0,
  "message": "",
  "success": true,
  "result": {
    "totalTimes": 2,
    "totalPoints": 1000,
    "hasIssued": true,
    "signInDays": ["2026-11-12", "2026-11-13"]
  }
}
```

- `code==0 && success==true` → 成功；`result.hasIssued` 判今日已签；`result.signInDays` 是 `YYYY-MM-DD` 字符串数组。
- **每日 +500 积分**：totalTimes=2 对应 totalPoints=1000（500×2），quota_overview 的 `incremental` 里两条 `source:"login_bonus"` 各 500 积分，口径吻合。

### POST /api/dumate/points/loginBonus（Cookie 认证，空 body）

**已签时实测返回**（今日该账号已签）：
```json
{
  "success": false,
  "requestId": "bdc9431b-…",
  "code": "InternalServerError",
  "message": { "global": "服务器内部错误" }
}
```

- **上游对「重复签到」返回字符串 code `InternalServerError` + `success:false`**，HTTP 仍是 200。
- **开发规则**：`hasIssued==true` 时**不调 POST**（直接 already）。若 POST 返回非 0 code，**必须复读 `loginBonusInfo`**：
  - `hasIssued` 变 true / `signInDays` 含今日 → 按 `already` 处理（POST 与查询间被客户端抢先签了）；
  - 仍 `hasIssued:false` → 才判 error（重试 2~3 次）。
- **成功返回样例**（未签状态 POST）尚未实测（今日已签），按 `{code:0, success:true, result:...}` 预期处理，**成功积分以签到前后 `loginBonusInfo.result.totalPoints` 差值为准**（+500），不依赖 POST 返回体里的积分数。

### GET /api/dumate/points/quota_overview（口径对照）

- `usedPoints`/`totalPoints` 是**字符串小数**（`"705.48"`/`"6000.00"`）。
- `incremental[]` 的 `source:"login_bonus"` 条目即签到奖励包，`packageType:"grant_point"`。

## 二、单测字段依据

| 字段 | 类型 | 说明 |
|---|---|---|
| `result.hasIssued` | bool | 今日已签判定（硬规则：true 不 POST） |
| `result.signInDays` | string[] | YYYY-MM-DD 数组，复读验证判据 |
| `result.totalPoints` | number | 累计签到积分（500/次） |
| `result.totalTimes` | number | 累计签到次数 |
| `code` | number(0)/string | 成功 0；失败为字符串 code（如 InternalServerError） |
| `success` | bool | 与 code 配套 |

## 三、待明日验证项

- [ ] 未签状态 POST `loginBonus` 成功返回样例（预期 `{code:0,success:true}`）。
- [ ] cookie 失效（过期后）`loginBonusInfo` 的返回形态（预期 401 或 code!=0）。
