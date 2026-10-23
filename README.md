# Trae + WorkBuddy + Qoder 每日签到 · 自托管 Docker 版

[![docker](https://github.com/Kyson-666/workbuddy-daily/actions/workflows/docker.yml/badge.svg)](https://github.com/Kyson-666/workbuddy-daily/actions/workflows/docker.yml)

把几个平台的每日签到搬到自己的机器上：一个容器、一个控制台、一条定时任务。
**登录完账号自动进列表，不用再复制回调链接。**

目前内置三个平台：**Trae**（抖音扫码）、**WorkBuddy**（微信扫码）、**Qoder**（官方页面登录）。
控制台菜单、定时任务、账号存储全部由平台注册表驱动 —— 想再加一个平台，
只需要写一个文件、注册一行，见[第七节：扩展一个新平台](#七扩展一个新平台)。

- 运行时零依赖：`scratch` 镜像，只有一个静态二进制 + CA 证书，拉取约 **3 MB**（解压约 7.2 MB）
- 数据落在本地卷，令牌用 AES-256-GCM 加密，接口任何响应都不返回令牌
- 支持 amd64 / arm64（群晖、树莓派、各种小主机）

> 算法与接口来自 [`ggzhzc/workbuddy-trae-checkin`](https://github.com/ggzhzc/workbuddy-trae-checkin)（Cloudflare Worker 版）。
> 原始实现原样保留在 [`upstream/`](upstream/)，本仓库是它的自托管移植。

---

## 一、和 Cloudflare Worker 版的差别

| | Worker 版 | 本版 |
|---|---|---|
| 部署位置 | Cloudflare 云端 | 自己的 Docker（NAS / 小主机 / 本机） |
| 账号存储 | Cloudflare KV | 本地卷 `data/`，同样是加密记录 |
| 定时任务 | CF Cron Triggers | 进程内定时器（`CRON_SCHEDULE`） |
| Trae 扫码 | 复制地址栏 → 粘贴 | **容器直接接住回调，自动入库**；粘贴仅作兜底 |
| WorkBuddy 扫码 | 自动 | 自动（不变） |
| Qoder 登录 | 无 | 打开官方页面 → 自动轮询入库 |
| 镜像体积 | — | 拉取 ~3 MB / 解压 7.2 MB |
| 首次配置 | 手动建 KV / 三个 Secret | 只映射端口即可，密码与主密钥自动生成 |

---

## 二、快速开始

```bash
git clone https://github.com/Kyson-666/workbuddy-daily.git
cd workbuddy-daily
cp .env.example .env        # 可全部留空
docker compose up -d        # 直接拉已发布镜像；想自己构建加 --build
```

已发布镜像（公开，amd64 + arm64，每次 push 到 main 自动构建）：

```
ghcr.io/kyson-666/workbuddy-daily:latest
```

打开 <http://127.0.0.1:17388>，输入登录密码。

密码从哪来：

- `.env` 里填了 `ADMIN_TOKEN` → 就是你填的那个
- 没填 → 首次启动自动生成，看日志一次：
  ```bash
  docker compose logs checkin | grep "登录密码"
  ```
  同时已保存到 `data/admin_token`，下次启动不再打印。

不用 compose 也可以：

```bash
docker run -d --name checkin --restart unless-stopped \
  -p 17388:17388 \
  -v "$PWD/data:/data" \
  -e TZ=Asia/Shanghai \
  ghcr.io/kyson-666/workbuddy-daily:latest
```

---

## 三、添加账号：扫码即完成

### Trae（抖音扫码，本机访问时全自动）

1. Trae 标签页点 **「添加账号」**，弹出官网授权页
2. 抖音扫码并确认
3. 浏览器跳到 `127.0.0.1:17388/authorize?…` —— **这个请求直接打到了容器**
4. 容器用 `AuthCode` + PKCE + 设备公钥换回 refreshToken，账号写入 `data/`，页面显示「登录成功」
5. 控制台每 2 秒轮询一次登录状态，拿到结果后自动刷新列表

全程零复制粘贴。

**为什么能自动**：Trae 的授权页只允许把 AuthCode 回送到 `http://127.0.0.1:<端口>`，不认任何域名。原版 Worker 跑在云端，够不到你本机的端口，所以只能人工粘贴；本版把控制台直接跑在回调端口 `17388` 上，回调天然落在自己身上。

**只有一种情况需要粘贴**：浏览器不在容器所在的那台机器上（例如容器跑在 NAS，你在电脑上开页面）。此时 `127.0.0.1` 指向你自己的电脑，回调落不到容器，控制台会把「手动粘贴」自动展开：

- 复制那个打不开页面的**整段地址栏链接**，粘到输入框 —— 粘上即自动绑定，不用点按钮
- 想在这种场景也全自动：本机做端口转发后改从 `127.0.0.1` 打开
  ```bash
  ssh -N -L 17388:127.0.0.1:17388 用户名@NAS地址
  # 然后浏览器打开 http://127.0.0.1:17388
  ```

### WorkBuddy（微信扫码，自动）

点 **「添加账号」** → 新窗口打开官方登录页 → 微信扫码确认 → 控制台轮询到结果后自动建号并关窗。二维码由官方页面渲染，本服务不接触你的微信信息。

### Qoder（官方页面登录，自动）

点 **「添加账号」** → 新窗口打开官方的账号选择页 → 在页面里完成登录 → 控制台轮询到令牌后自动建号并关窗。

流程上是一次标准的 PKCE 设备登录：本服务本地生成 `verifier` / `challenge`（S256）和一个
`nonce`，把它拼进官方页面的地址；你在页面上登录后，本服务拿 `nonce + verifier` 去官方
接口轮询拿令牌，拿到后加密入库。**全程不经过任何第三方**。

列表里显示「累计 Credits」这一列 —— 那是本服务自己累加的领取记录；
活动当天还没下发时状态是「活动未下发」，等下轮自动重试，不会误标成已签。

> 备注名优先取账号昵称，取不到时是 `Trae-xxxx` / `账号-xxxx` / `Qoder-xxxx`，点名字或铅笔可以就地改。

---

## 四、签到与定时任务

- **立即签到**：只跑当前标签页；**全部签到**：所有平台一起跑
- 签到接口本身幂等，重复点不会重复加分

容器内置定时器，默认北京时间 **08:10** 和次日 **00:20** 各跑一次（两条互为兜底：Trae 在 0 点前后限流冷却只有 12 分钟，白天是 55 分钟）。

```env
CRON_SCHEDULE=08:10,00:20   # 逗号分隔，留空 = 关闭
```

也可以用外部定时器触发（跑的逻辑与「全部签到」一致）：

```bash
curl -X POST "http://你的地址:17388/cron" -H "X-Cron-Secret: 你的CRON_SECRET"
# 或 GET /cron?key=...
```

返回每个平台各自的汇总（`checked` / `already` / `cooling` / `failed`）。
`CRON_SECRET` 没配时回落到 `ADMIN_TOKEN`。

### 保活刷新（默认关闭）

签到的那一次运行会顺带把快过期的令牌换掉，所以正常用不需要额外做什么。
但如果你有账号长期不跑（比如只在周末开一次机器），令牌可能早就过期了，
再跑就是「令牌失效」，只能重新登录。

打开保活后，每天 `KEEPALIVE_TIME`（默认 12:30）会把「连续 `KEEPALIVE_DAYS` 天没签到成功」的
账号**只刷新令牌、不签到**：

```env
KEEPALIVE_DAYS=3      # 连续 3 天没成功签到才刷；留空或 0 = 关闭
KEEPALIVE_TIME=12:30  # 每天跑保活的时间
```

> ⚠️ 三个平台的 refreshToken 都是**轮换制**：刷新一次，官方客户端里存的那条旧令牌会立刻失效
> （表现为客户端退出登录）。**平时也在用官方客户端的话，请不要打开这个开关。**
> 另外，同一个账号不要同时在两个地方跑自动签到（比如 CF 版 + 本版、或两台机器），两边互刷会互相踢。

账号列表的「上次续期」一列可以看到令牌最近一次刷新的时间。

---

## 五、配置项

| 变量 | 默认 | 说明 |
|---|---|---|
| `PORT` | `17388` | 控制台端口；同时也是 Trae 的回调端口 |
| `CALLBACK_PORT` | 同 `PORT` | 想换控制台端口时，可单独指定回调端口并额外映射 |
| `DATA_DIR` | `/data` | 数据目录（compose 挂 `./data`） |
| `ADMIN_TOKEN` | 自动生成 | 控制台登录密码 |
| `MASTER_KEY` | 自动生成 | 32 字节 base64，账号记录的加密主密钥 |
| `CRON_SCHEDULE` | `08:10,00:20` | 北京时间 `HH:MM`，逗号分隔；空 = 关闭 |
| `CRON_SECRET` | 空 | `/cron` 独立口令，空则用 `ADMIN_TOKEN` |
| `KEEPALIVE_DAYS` | 空（关闭） | 连续这么久没签到成功的账号，每天只刷新令牌不签到 |
| `KEEPALIVE_TIME` | `12:30` | 每天跑保活的时间，配了 `KEEPALIVE_DAYS` 才生效 |
| `LOG_REQUESTS` | 空 | 填 `1` 打印每个请求的方法/路径/状态码，排查反代用 |
| `TZ` | `Asia/Shanghai` | 影响「今天」的判定与定时时间 |

高级项（一般不用动）：`TRAE_CALLBACK_HOST` / `TRAE_CALLBACK_PORT`，改动后回调地址随之变化，自动回调可能失效。

> `CRON_SCHEDULE=off` 可以显式关掉定时；留空则等同于没配，回落到默认的 `08:10,00:20`。
> 保活刷新是独立开关，配了 `KEEPALIVE_DAYS` 就会把自己的时间点加进定时任务里。

---

## 六、数据与安全

```
data/
├── master.key      # 32 字节主密钥（base64），0600
├── admin_token     # 自动生成的登录密码，0600
└── kv/             # 每条账号记录一个文件，内容是 AES-256-GCM 密文
```

- 密钥派生：`HKDF-SHA256(MASTER_KEY, 随机 16B salt, "<平台 key>-signin/v1/account")`，
  即 `trae-signin/v1/account` / `wb-signin/v1/account` / `qoder-signin/v1/account`
- 每条记录随机 salt + IV，密文格式与上游 Worker 一致（`{"v":1,"s":…,"i":…,"d":…}`），
  从 Cloudflare KV 迁移过来的记录可以直接读
- 列表接口只返回备注、状态、积分，**令牌与私钥永不下发**
- **备份 `data/` 就是备份一切**：里面既有密文也有密钥，别外传、别进 Git
- 换掉 `MASTER_KEY` = 已有账号全部解不开，只能重新扫码

---

## 七、扩展一个新平台

控制台菜单、路由、定时任务、账号存取全都是按「平台注册表」跑的，
加一个新签到平台**不需要改路由、不需要改前端**。总共两步：

### 1. 写一个文件，实现 `Platform` 接口

```go
// zcode.go
type zcodePlatform struct{}

func (p *zcodePlatform) Key() string     { return "zcode" }        // 路由前缀 + 存储前缀
func (p *zcodePlatform) Label() string   { return "ZCode" }        // 菜单标题
func (p *zcodePlatform) Flow() LoginFlow { return FlowLink }       // callback 或 link
func (p *zcodePlatform) Caps() Caps      { return Caps{} }         // 控制台据此决定显示什么
func (p *zcodePlatform) Metrics() []Metric {                       // 列表里的指标列
	return []Metric{{K: "totalCredit", Label: "累计 Credits"}}
}
func (p *zcodePlatform) Describe(acct *Account, v viewCtx) (string, string, string) {
	return "今日未领", "idle", ""          // 标签文案 / 色调 / 备注，判定都在服务端
}
func (p *zcodePlatform) StartLogin(ctx context.Context, a *App) (LoginHandle, error) { … }
func (p *zcodePlatform) PollLogin(ctx context.Context, a *App, id string) (LoginPoll, error) { … }
func (p *zcodePlatform) CompleteLogin(ctx context.Context, a *App, id string, in LoginInput) (*LoginOutcome, error) { … }
func (p *zcodePlatform) Signin(ctx context.Context, a *App, acct *Account, force bool) signinResult { … }
```

### 2. 注册一行

```go
// app.go · NewApp
a.registerPlatform(&traePlatform{})
a.registerPlatform(&wbPlatform{})
a.registerPlatform(&qoderPlatform{})
a.registerPlatform(&zcodePlatform{})   // ← 加在这里，顺序就是菜单顺序
```

做完这两步，下面这些自动就有了，不用再动任何代码：

| 自动获得 | 说明 |
|---|---|
| 控制台菜单与面板 | 标题取 `Label()`，顺序取注册顺序 |
| 路由 `/zcode/api/{list,run,login/start,login/poll,login/claim,remove,rename}` | 通用分发 |
| 「全部签到」/ `POST /cron` 的汇总 | `runAll` 遍历注册表 |
| 账号加密存储 | KV 前缀 `zcode:`，密钥派生标签 `zcode-signin/v1/account` |
| 登录卡片 | 按 `Flow()` 选模板：`callback` 带粘贴兜底，`link` 是打开页面 + 轮询 |
| 保活刷新 | 再实现一个 `Keepalive(ctx, a, acct) error` 即可（可选） |

### 某个平台接口变了怎么办

协议相关的一切都在它自己那个文件里：端点、请求头、状态码。官方改版时
只改那个文件顶部的常量块与状态码映射，其余文件零改动。
`Signin` 返回的 `signinResult.State` 统一用
`checked / already / skipped / rate / cooldown / pending / error / token_dead`，
通用层据此汇总，平台之间互不牵连。

---

## 八、镜像与构建

```bash
# 本机架构
docker build -t workbuddy-daily:latest .

# 多架构（需要 buildx）
docker buildx build --platform linux/amd64,linux/arm64 -t <tag> --push .
```

体积实测（amd64）：Go 静态二进制 7.0 MB（gzip 后 2.9 MB）+ `ca-certificates` 0.2 MB（gzip 后 0.12 MB），
**拉取总量 3.01 MB**，解压后约 7.2 MB。运行镜像 `FROM scratch`，不含 shell、包管理器、Node、Python。

时区库用 `time/tzdata` 编进二进制，所以 `scratch` 里也能正确按 `Asia/Shanghai` 算日期。

本地开发：

```bash
go test ./...                                   # 含与上游 JS 的逐字节比对
go build -o checkin . && ADMIN_TOKEN=dev DATA_DIR=./data ./checkin
```

---

## 九、接口速查

所有管理接口认三种口令：`X-Admin-Token: <token>`、`Authorization: Bearer <token>`、`?token=<token>`。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` | 控制台 |
| GET | `/api/ping` | 校验密码 |
| GET | `/api/platforms` | 已注册平台列表（控制台据此生成菜单） |
| POST | `/api/run-all` | 所有平台一起签（`?force=1` 强制） |
| POST | `/cron` | 定时入口，口令走 `X-Cron-Secret` 或 `?key=` |
| GET | `/authorize` · `/oauth/callback` | Trae 授权回调（自动入库，无需口令） |

每个平台都有一套 `/<key>/api/...`，`<key>` 取 `trae` / `wb` / `qoder`：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/<key>/api/list` | 账号列表（含 tag / tone / 指标 / 上次运行 / 上次续期） |
| POST | `/<key>/api/run` | 单个平台签到（`?force=1` 强制） |
| POST | `/<key>/api/login/start` | 发起登录，返回 `handle`（含 id、授权页地址、轮询间隔） |
| GET | `/<key>/api/login/poll?id=` | 轮询登录结果：`pending` / `ok` / `expired` / `error` |
| POST | `/<key>/api/login/claim` | 粘贴回调地址兜底（仅 Trae 这类 `callback` 形态） |
| POST | `/<key>/api/remove` | 删除账号 |
| POST | `/<key>/api/rename` | 改备注 |

`login/start` 的返回统一形如：

```json
{ "ok": true, "handle": { "id": "…", "flow": "link", "authUrl": "https://…", "intervalMs": 2000 } }
```

---

## 十、排错

| 现象 | 原因 / 处理 |
|---|---|
| 打不开控制台 | 端口没映射或被占用：`docker compose logs checkin`；换个 `PORT` 并同时映射 |
| 「登录密码不正确」 | `.env` 里的 `ADMIN_TOKEN` 有多余空格，或改过之后没重启容器 |
| 扫码后停在「无法访问此网站」 | 浏览器不在容器所在机器——这是正常的，复制地址栏整段链接粘到控制台即可（粘上即绑定） |
| 回调页「登录会话已过期」 | 授权链接 15 分钟有效，重新点「添加账号」 |
| Trae 显示「限流 N 分钟」 | 服务端 9074 限流，已按账号冷却，到点自动重试；0 点前后冷却只有 12 分钟 |
| Qoder 显示「活动未下发」 | 当天活动还没下发到账号，等下一轮自动重试；不会误标成已签 |
| Qoder 领取失败 | 看账号行下面的错误原文，一般是活动已过期或令牌失效 |
| 「令牌失效」 | refreshToken 被别处轮换或长期未跑，删掉账号重新登录 |
| 「缺设备密钥」 | 账号记录异常（极少见），删掉重新扫码 |
| 定时没跑 | 看日志 `定时签到已启用`；确认 `CRON_SCHEDULE` 是北京时间 `HH:MM` |
| 反向代理访问 404 | 先 `curl -sI http://127.0.0.1:17388/` 确认容器本身是 200，再查反代的主机名/端口；打开 `LOG_REQUESTS=1` 看请求到底有没有进来 |
| 想确认时区对不对 | 看 `docker logs` 行首的时间戳是否为北京时间 |

---

## 十一、与上游同步

`upstream/worker.js` 是原项目单文件实现，本仓库的 `trae.go` / `wb.go` 与它**逐函数对齐**
（授权链接拼装、三套换令牌变体、设备证明签名、限流与待确认状态机、WorkBuddy 令牌轮换）。
Qoder 是本仓库新增的，上游没有对应实现。

这些接口都是逆向得到的，客户端升级可能改路径或参数。上游更新时：

```bash
git fetch upstream && git diff upstream/main -- upstream/worker.js
```

按 diff 同步到对应的 Go 函数即可；`go test ./...` 里的基准值（`testdata/gen_fixture.js`）也可一并重生成。

---

## 致谢与声明

- 原始实现：[ggzhzc/workbuddy-trae-checkin](https://github.com/ggzhzc/workbuddy-trae-checkin)
- 本项目为非官方工具，与 Trae、WorkBuddy、Qoder 及其所属公司无任何隶属关系
- 签到接口由客户端逆向得到，平台升级可能导致失效；自动化签到可能涉及服务条款，**建议只给自己的账号使用**

### Qoder 部分的说明

Qoder 的登录与领取流程参照了社区公开实现（设备登录 + `deviceToken` 轮询、
`/sash/api/v1/me/campaigns` 与 claim 端点），并已按假上游跑通全链路测试。
**但真实账号的实测尚未完成**：端点行为、活动下发时间、刷新轮换规则若有偏差，
以真实响应为准，并会在后续提交里修正（测试与代码都集中在 `qoder.go`，改一处即可）。
