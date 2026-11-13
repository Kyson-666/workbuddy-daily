# 百度搭子（DuMate）签到能力设计（v3.1 · 已确认，可交付开发）

> 设计日期：2026-11-13　状态：**设计阶段，全部决策已确认，未改动任何代码**
> 目标项目：`workbuddy-daily`（Go 签到服务，部署到 NAS）
> 登录方案：**方案 B（浏览器扩展）**
> 用途：**本文件已确认，交由开发模型按此实现，开发前无需再确认方向**
> 关键既有事实：签到接口已实际拆解本地 DuMate 客户端确认；`dumate-proxy` 已验证 cookie 抓取与 token 换取链路。
> 参考：`deliverables/dumate-channel-recon-2026-11-12.md`、`dumate-proxy/lib/{browser-login,dumate,store}.js`、`workbuddy-daily/{platform,trae,qoder,wb,crypto,httpc,config}.go`、`workbuddy-daily/docker-compose.yml`

---

## 〇、已确认决策（开发依据）

用户已明确确认以下三点，开发按此执行，**不得偏离**：

1. **扩展安装流程**：接受「**下载扩展包 → 指引安装**」两步（浏览器安全策略不允许网页一键静默安装非商店扩展，此为硬约束）。
2. **扩展托管与部署**：扩展包由 **workbuddy-daily 自身托管在 `/dumate/extension/`**，**文件持久化到目录**，**更新 docker compose 部署文件**（见第三节 3.6）。
3. **签到接口实测**：**必须在本机实测** `loginBonus` / `loginBonusInfo`，锁定字段映射后再编码（见第八节阶段 0，为前置关口）。

---

## 一、可行性结论

**可行。** 架构上复用 `workbuddy-daily` 平台注册表，加平台 = 实现 `Platform` 接口 + 一行注册，路由/菜单/定时汇总/列表渲染全部自动驱动。签到接口已拆解确认（第二节），登录方案用浏览器扩展，满足「用户只登录、不找 cookie、换任意机器可从页面安装扩展」。

### 方案 B 核心原理

百度登录态是 **httpOnly cookie**，网页 JS 读不到——但**浏览器扩展通过 `chrome.cookies` API 在获得 `cookies` 权限后可读取 httpOnly cookie**。这是扩展方案成立的根本。

```
用户在 NAS 控制台点「添加 DuMate 账号」
  → 控制台「安装扩展」按钮 → 下载扩展包 → 按指引在 chrome://extensions 安装
  → 点「登录」→ 扩展拉起百度登录页
  → 用户在扩展打开的窗口登录（唯一动作）
  → 扩展读百度 cookie + 开 console 完成 CAS 握手补种 bce-session/bce-sessionid
  → 扩展把 cookie POST 回 NAS
  → NAS 用 cookie 换 app token、加密落库
```

---

## 二、签到接口拆解结论（已实际拆解本地客户端，开发依据）

从本地 DuMate 客户端（`D:\DuMate\resources\app.asar` 前端 + `dumate-main-server.exe` 网关）提取，签到功能与鉴权明确：

### 2.1 签到接口清单

| 接口 | 方法 | 认证 | 用途 |
|---|---|---|---|
| `console.bce.baidu.com/api/dumate/points/loginBonus` | **POST** | **Cookie** | **签到（领取每日奖励）** |
| `console.bce.baidu.com/api/dumate/points/loginBonusInfo` | **GET** | **Cookie** | 查签到状态：`signInDays[]`/`hasIssued`/`totalPoints`/`totalTimes` |
| `console.bce.baidu.com/api/dumate/points/quota_overview` | GET | Cookie/Bearer | 查积分额度明细 |
| `console.bce.baidu.com/api/dumate/app/token/get` | GET | Cookie | 换 app token（签到不需要，对话用） |

**关键结论**：
1. **签到走 `console.bce.baidu.com` 域，Cookie 认证，不需要 app token。**
2. **签到 = POST `loginBonus`（空 body）**，成功返回 `{code:0, result}`。
3. **每日奖励 500 积分**，前端逻辑 `totalPoints += 500, totalTimes += 1`，`signInDays` 记录已签日期。
4. **签到状态判定**：`loginBonusInfo` 返回 `signInDays`、`hasIssued`、`totalPoints`、`totalTimes`，用 `hasIssuedToday` 判已签。

### 2.2 签到鉴权

- **Cookie 认证**：请求带百度登录 cookie，最小集 `bce-session` + `bce-sessionid`（与换 token 的最小集一致）。
- **不需要 Bearer / app token**。

### 2.3 签到流程（对齐前端逻辑）

```
1. 查 loginBonusInfo → signInDays / hasIssuedToday / totalPoints / totalTimes
2. hasIssuedToday == true → already，记 LastOkDate
3. 否则 POST loginBonus（空 body，Cookie）
   - code==0 → 成功，+500 积分
   - 已签类错误 → already
   - 429 → rate 冷却；其他 → 重试 2~3 次（429 以外也重试）
4. 回查 loginBonusInfo 确认 signInDays 含今日、totalPoints 增加（复读验证）
5. 记 LastCredit=500、TotalCredit+=500、TotalSigned++、LastOkDate=今天
```

---

## 三、登录方案设计：浏览器扩展（方案 B）

### 3.1 扩展安装入口（已确认：下载扩展包 → 指引安装）

**入口设计**：

1. **控制台登录卡片内置「安装扩展」按钮**。用户点「添加 DuMate 账号」时，控制台检测浏览器是否已装 DuMate 登录扩展；未安装则显示安装引导卡，含：
   - **「下载扩展」按钮**：触发下载扩展包（`.crx` 主、zip 兜底），下载地址 `/dumate/extension/<包名>.crx`。
   - **安装指引**：分 Chrome / Edge 的文字步骤 + 示意图（`chrome://extensions` → 开启「开发者模式」→ 「加载已解压的扩展程序」选解压目录 / 或将 `.crx` 拖入）。
   - 安装完成后点「刷新检测」，控制台判定已安装，按钮变为「登录」。

2. **扩展包由 workbuddy-daily 静态托管在 `/dumate/extension/`**：用户**换任何机器打开 NAS 控制台，同一入口即可获取**，无需其他渠道。

**安装流程（两步，已确认接受）**：
- **第一步**：控制台点「下载扩展」→ 浏览器下载扩展包（`.crx` 或 zip）。
- **第二步**：按指引在 `chrome://extensions` 安装（Edge 对 sideload 更宽松，体验更好；Chrome 需开启开发者模式）。

> 说明：因属自托管非商店扩展，浏览器会提示「非商店扩展」警告，属正常现象，安装指引中说明即可。

### 3.2 扩展分发形态

| 形态 | 适用 | 安装方式 |
|---|---|---|
| **`.crx` 包**（主） | Chrome / Edge | 下载后拖入扩展页或「加载已解压」 |
| **zip 解压包**（兜底） | 通用 | 解压到目录 → 「加载已解压的扩展程序」 |

扩展包随 workbuddy-daily 构建/发布，保证版本与后端登录接口对齐。

### 3.3 扩展职责与登录流程

**扩展需要做的事**（全部自动，用户仅登录）：

1. **接收指令**：扩展监听来自控制台页面的消息，获取本次登录任务与 NAS 地址。
2. **打开登录页**：`chrome.tabs.create` 打开 `login.bce.baidu.com`，用户在新开窗口扫码/输密码登录。
3. **触发 CAS 握手**：检测到登录后，扩展再开 `console.bce.baidu.com/dumate/` 标签，让 BCE 前端跑完 CAS 握手、补种 `bce-session`/`bce-sessionid`（复用 `dumate-proxy` 已验证的「必须开 console 才种 cookie」结论，约 30~40 秒）。
4. **读 cookie**：`chrome.cookies.getAll` 读取百度系域名 cookie（含 httpOnly）。
5. **回传 NAS**：把 cookie 数组 POST 到 NAS 的登录接口 `/dumate/api/login/claim`。
6. **回报结果**：扩展通知控制台页登录成功/失败，控制台刷新账号列表。

**登录成功判据**：以「cookie 能否换到 app token」为准（复用 `dumate-proxy`），不猜 cookie 名。扩展回传 cookie 后，NAS 用 cookie 调 `token/get` 验证，成功才落库。

**登录态判定的 cookie 痕迹**（复用 `dumate-proxy` 的 `hasLoginTrace`）：`bce-session` / `bce-sessionid` / `bce-long-term-sessionid` / `bce-user-info` / `BDUSS` / `bce-login-accountid` 任一存在即视为有登录痕迹；但**真正成功判据是换到 token**。

### 3.4 扩展权限最小化

`manifest.json`（MV3）权限设计：

```json
{
  "permissions": ["cookies", "tabs"],
  "host_permissions": [
    "*://*.bce.baidu.com/*",
    "*://*.baidu.com/*"
  ]
}
```

- **不加** `<all_urls>`、不做后台静默抓取，仅在用户主动点登录时工作。
- NAS 地址由控制台页传入，不硬编码，适配任意 NAS。

### 3.5 NAS 地址传递

- 控制台页发起登录时，把当前 NAS 的 origin（如 `http://nas-ip:17388`）通过消息发给扩展。
- 扩展回传 cookie 时 POST 到该地址的 `/dumate/api/login/claim`。
- 同一扩展适配所有 NAS，换机器/换 NAS 地址无需改扩展。

### 3.6 扩展托管与 compose 部署（已确认：持久化到目录 + 更新 compose）

**持久化与托管方案**：

1. **扩展包存放目录**：服务从固定目录读取扩展包提供静态下载。容器内路径 `/data/extension/`（复用 `/data` 数据卷，与账号密文同卷，便于统一备份）。
2. **构建时内置**：镜像构建时将扩展打包产物（`.crx` + zip）`COPY` 到镜像内 `/data/extension/` 作为初始版本；容器首次启动若目录为空，从镜像内置复制（保证开箱可用）。
3. **持久化挂载**：docker compose 新增 volume，把宿主目录挂载到容器 `/data/extension/`，用户更新扩展时只需替换宿主目录文件，**无需重建镜像**。

**docker compose 改动**（在现有 `docker-compose.yml` 基础上新增 volume，保留原有 `./data:/data`）：

```yaml
services:
  checkin:
    image: ghcr.io/kyson-666/workbuddy-daily:latest
    build: .
    container_name: checkin
    restart: unless-stopped
    ports:
      # 17388 不是随便挑的：Trae 的授权回调固定回送到 http://127.0.0.1:17388/authorize
      - "17388:17388"
    environment:
      TZ: Asia/Shanghai
      # 留空则首次启动自动生成，写入 data/admin_token 并在日志里打印一次
      ADMIN_TOKEN: ${ADMIN_TOKEN:-}
      # 留空则首次启动自动生成到 data/master.key（丢了只能重新扫码）
      MASTER_KEY: ${MASTER_KEY:-}
      # 北京时间，逗号分隔；留空即关闭自动签到
      CRON_SCHEDULE: ${CRON_SCHEDULE:-08:10,00:20}
      CRON_SECRET: ${CRON_SECRET:-}
      # 扩展包托管目录（容器内），默认 /data/extension
      DUMATE_EXTENSION_DIR: ${DUMATE_EXTENSION_DIR:-/data/extension}
    volumes:
      - ./data:/data
      # 新增：扩展包持久化目录，宿主维护，容器读
      - ./dumate-extension:/data/extension
```

**改动要点**：
- 新增 `DUMATE_EXTENSION_DIR` 环境变量（默认 `/data/extension`），`config.go` 读取，服务静态托管该目录下的扩展包。
- 新增 volume `./dumate-extension:/data/extension`，扩展包持久化到宿主 `dumate-extension/` 目录。
- 服务静态路由：`GET /dumate/extension/<文件名>` → 从 `DUMATE_EXTENSION_DIR` 读取并返回（`Content-Type` 按扩展名，`Cache-Control` 适度缓存）。
- **首次部署**：宿主 `dumate-extension/` 为空时，容器从镜像内置复制初始扩展包到该目录（启动逻辑处理），保证开箱可用；之后用户替换该目录文件即可更新扩展。

**扩展静态托管路由设计**：
- `GET /dumate/extension/list` → 返回目录内可用扩展文件清单（文件名、类型、版本），供控制台安装按钮展示。
- `GET /dumate/extension/<file>` → 下载对应扩展包。
- 目录遍历需防越权（只允许下载该目录下、白名单扩展名 `.crx`/`.zip` 的文件，禁止路径穿越）。

### 3.7 登录流命名：FlowExt

新增登录流 `FlowExt`，接口实现：

- `StartLogin`：创建登录会话，返回 `LoginHandle{Flow: FlowExt}`。控制台据此渲染「安装扩展 + 登录」引导卡。
- `PollLogin`：返回 pending，等待扩展回传 cookie。
- `CompleteLogin`：**扩展回传落点**。接收扩展 POST 的 cookie，用 cookie 调 `token/get` 换 app token + accountId，验证成功后生成 `DumateAccount` 加密入库。

**复用现有通道**：扩展回传复用 `handleLoginClaim` 的「提交内容 → CompleteLogin」通道，扩展把 cookie 当结构化内容提交。前端为 `FlowExt` 增加安装引导 + 登录触发 + 已安装检测的交互分支。

### 3.8 兜底

保留粘贴 cookie JSON 的兜底：若用户环境无法装扩展，可从 DevTools/扩展导出 cookie 粘贴。主路径是扩展自动抓取。

---

## 四、账号模型与多账号

### 4.1 复用 Account 信封

| Account 字段 | DuMate 含义 |
|---|---|
| `AccessToken` | app token（签到不用，留作扩展） |
| `RefreshToken` | 留空（cookie 走 Ext） |
| `Label` | 百度登录名（`bce-login-display-name` cookie 或登录返回） |
| `AccessExp` | app token 过期时间 |
| `Ext` | `{ cookies[], accountId, displayName, tokenExpiresAt, signedDate }` |

**不改 `Account` 结构体**（项目红线：改 JSON tag 破坏磁盘旧密文记录），cookie 数组存 `Ext`，由现有 `sealRecord`（AES-256-GCM+HKDF，标签 `dumate-signin/v1/account`）加密落库。新增 `type DumateAccount = Account`。

### 4.2 多账号：天然支持

每账号独立 cookie，签到各自用 Cookie 调 `loginBonus`，N 账号 = N 条记录。扩展每次登录一个账号，回传后 NAS 各存一份。换机器装扩展后，登录其他账号即可追加，不影响既有账号。

---

## 五、签到流程设计（接口已确认）

`Signin(ctx, a, acct, force)` 单账号流程，对齐 `traeSigninOne`：

```
1. 防重复：!force 且 acct.LastOkDate == 今天 → skipped
2. Cookie 校验：Ext.cookies 含 bce-session/bce-sessionid，否则 token_dead「缺登录态」
3. 查签到状态：GET loginBonusInfo（Cookie）→ signInDays / hasIssuedToday / totalPoints
   - 401/403 → token_dead（cookie 失效，提示重新登录）
4. hasIssuedToday == true → already，写 LastOkDate
5. 否则 POST loginBonus（Cookie，空 body）
   - code==0 → 成功；已签类 → already；429 → rate 冷却；其他 → 重试 2~3 次
6. 回查 loginBonusInfo 确认 signInDays 含今日、totalPoints 增加（复读验证）
7. 写状态：成功 → checked, LastCredit=500, TotalCredit+=500, TotalSigned++, signedDate=今天
   失败 → error/token_dead + LastMsg
8. 返回 signinResult{State, Msg, Points:500}
```

### Keepalive

实现 `keepaliver`：用 Ext.cookie 调 `token/get` 刷新 app token（不签到）。cookie 过期时提示重新登录。

### Caps 与 Metrics

```
Caps: { Credits: true }
Metrics: [{K:"totalCredit", Label:"累计积分"}, {K:"total", Label:"累计签到次数"}]
```

### Describe 状态判定

| 条件 | tag | tone | msg |
|---|---|---|---|
| 今日已签 | 已签 | ok | +500 积分 |
| token_dead | 登录态失效 | bad | 需重新登录 |
| error 且今日已跑 | 签到失败 | bad | LastMsg |
| 缺 cookie | 缺登录态 | warn | 请先登录 |
| 默认 | 今日未签 | idle | — |

---

## 六、改动清单（交付开发）

### 6.1 workbuddy-daily 服务端

| 文件 | 动作 | 内容 |
|---|---|---|
| `dumate.go` | 新增 | 协议常量（签到/状态端点、Cookie 头、超时、重试）、`dumateSigninOne`、`dumateRefreshToken`、`dumatePlatform` 适配器、`CompleteLogin`（cookie 解析 + token 换取）、`dumatePersist` |
| `dumate_test.go` | 新增 | 单测：签到状态机、cookie 解析、积分解析、多账号隔离。假 HTTP 服务，对齐 `qoder_test.go` 风格 |
| `platform.go` | 微改 | `LoginFlow` 常量块加 `FlowExt` |
| `app.go` | 一行 | `NewApp` 加 `registerPlatform(&dumatePlatform{})` |
| `config.go` | 微改 | 新增 `DUMATE_EXTENSION_DIR` 配置项（默认 `/data/extension`） |
| `routes.go` | 小改 | 新增 `/dumate/extension/*` 静态托管路由（list + 下载，白名单 + 防路径穿越）；扩展回传复用 `/login/claim` 无需改 |
| `console.html` | 小改 | `FlowExt` 分支：安装扩展引导卡 + 下载按钮 + 安装指引 + 登录触发 + 已安装检测 |
| `docker-compose.yml` | **更新** | 新增 `DUMATE_EXTENSION_DIR` 环境变量 + `./dumate-extension:/data/extension` volume（见 3.6） |
| `.env.example` | 更新 | 补充 `DUMATE_EXTENSION_DIR` 说明 |
| `Dockerfile` | 小改 | 构建时将扩展打包产物 `COPY` 到镜像 `/data/extension/`（初始版本） |
| `README.md` | 更新 | DuMate 平台说明：扩展安装（下载 + 指引）、换机器安装、compose 部署、多账号、封号风险 |

**零改动**：`cron.go`、`store.go`、`crypto.go`、`httpc.go`、`Account` 结构体。

### 6.2 浏览器扩展（新增产物，随 workbuddy-daily 构建发布）

| 文件 | 内容 |
|---|---|
| `extension/manifest.json` | MV3，权限 `cookies`/`tabs` + baidu 域（见 3.4） |
| `extension/background.js`（service worker） | 监听登录指令、开登录页、触发 CAS 握手、读 cookie、回传 NAS（见 3.3） |
| `extension/popup.html` + `popup.js` | 扩展弹窗，展示登录状态、触发登录、回报结果 |
| `extension/icons/` | 扩展图标 |
| **扩展打包** | 构建产出 `.crx` + zip，放入服务端 `DUMATE_EXTENSION_DIR` 目录提供下载 |

**扩展构建产物随服务发布**：保证扩展版本与后端登录接口对齐，用户从控制台下载的永远是当前版本。

---

## 七、关键实现约定（开发注意）

1. **签到成功必须复读验证**：`code==0` 不算到账，回查 `loginBonusInfo` 确认 `signInDays` 含今日、`totalPoints` 增加（对齐交付自检第 8 条，防假阳性）。
2. **重试策略**：上游非 429 错误重试 2~3 次再判失败；429 进冷却不重试。
3. **Cookie 解析兼容**：扩展回传的 cookie 数组格式需与 `dumate-proxy` 导出兼容（`{name,value,domain,path,...}`），后端宽松解析。
4. **扩展静态托管安全**：`/dumate/extension/` 只允许白名单扩展名、防路径穿越，`Cache-Control` 适度。
5. **不改动 Account 结构体**：cookie 走 `Ext`，沿用现有密文体系。
6. **Keepalive 与轮换**：DuMate cookie 不轮换（非 refreshToken），Keepalive 仅刷 app token，不会导致官方客户端令牌失效（与 Trae/WB 不同，可放心开启）。

---

## 八、验证计划（动手后执行，阶段 0 为前置关口）

### 阶段 0：签到接口本机实测（**必须，前置**）

带百度 cookie 在本机实测，锁定字段映射，**此步不定不编码**：
1. `POST console.bce.baidu.com/api/dumate/points/loginBonus`（空 body，Cookie）→ 确认 `{code, result}` 结构、成功/已签/限流的 code 值。
2. `GET console.bce.baidu.com/api/dumate/points/loginBonusInfo`（Cookie）→ 确认 `signInDays`/`hasIssued`/`totalPoints`/`totalTimes` 字段名与类型。
3. 签到前后各查一次 `loginBonusInfo`，确认 `totalPoints` +500、`signInDays` 增加，锁定复读验证判据。
4. 记录实测返回样例，作为开发与单测的依据。

### 阶段 1：服务端 PoC

1. **cookie 换取单测**：构造假 `token/get` 响应，验证 cookie → token → accountId 解析。
2. **签到状态机矩阵**：
   - A 首次签到（成功，+500）
   - B 当日重复（already）
   - C cookie 缺失/失效（token_dead）
   - D 429 限流（rate/cooling）
   - E 签到端点 500（重试 2~3 次后 error）
   - F 多账号（两账号各自签到、积分独立）
   - G 回查验证（签到后 loginBonusInfo 的 signInDays/totalPoints 实际变化）
3. `go test ./...` 全绿（含 `parity_test.go` 逐字节比对，确认未破坏上游基准）。

### 阶段 2：浏览器扩展

1. **扩展功能实测**：Chrome 与 Edge 分别安装 → 打开控制台 → 点登录 → 扩展拉起百度登录页 → 登录 → 读 cookie 回传 → NAS 落库。验证 httpOnly cookie 读取与 CAS 握手。
2. **换机器安装实测**：在另一台机器打开 NAS 控制台 → 安装入口可见 → 下载扩展 → 安装 → 登录成功，验证「换任何机器可安装」。
3. **扩展静态托管实测**：`/dumate/extension/list` 返回清单、`/dumate/extension/<file>` 下载正常、路径穿越被拒。

### 阶段 3：真机 E2E

1. NAS 部署（含 compose 扩展 volume）→ 扩展登录 → 列表显示账号与积分。
2. 手动签到 + 定时签到触发，确认状态与积分正确。
3. 多账号（两个账号各自登录、签到、积分独立）。

---

## 九、分期路线

- **阶段 0（前置，必须）**：本机实测签到接口，锁定字段映射。**此步不定，不编码。**
- **阶段 1（服务端 PoC）**：实现 `dumate.go` 签到 + 登录接收 + 单测 + 注册 + 配置/路由/前端 `FlowExt` 分支 + compose 部署。登录先用粘贴 cookie 兜底，NAS 上单账号签到跑通。
- **阶段 2（扩展）**：开发浏览器扩展（manifest + background + popup），打包 `.crx`/zip 放入扩展目录，控制台安装引导卡 + 下载入口，Chrome/Edge 实测登录回传。多账号实测。
- **阶段 3（打磨）**：换机器安装验证、Keepalive、状态矩阵回归、README。
