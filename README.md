# Trae + WorkBuddy 每日签到 · 自托管 Docker 版

把两个平台的每日签到搬到自己的机器上：一个容器、一个控制台、一条定时任务。
**扫码后账号自动进列表，不用再复制回调链接。**

- 运行时零依赖：`scratch` 镜像，只有一个静态二进制 + CA 证书，约 **7 MB**
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
| 镜像体积 | — | ~7 MB（scratch） |
| 首次配置 | 手动建 KV / 三个 Secret | 只映射端口即可，密码与主密钥自动生成 |

---

## 二、快速开始

```bash
git clone https://github.com/hanyuestar/workbuddy-daily.git
cd workbuddy-daily
cp .env.example .env        # 可全部留空
docker compose up -d --build
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
  ghcr.io/hanyuestar/workbuddy-daily:latest
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

> 备注名优先取账号昵称，取不到时是 `Trae-xxxx` / `账号-xxxx`，点名字或铅笔可以就地改。

---

## 四、签到与定时任务

- **立即签到**：只跑当前标签页；**全部签到**：两个平台一起跑
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

返回两个平台各自的汇总（`checked` / `already` / `failed`）。`CRON_SECRET` 没配时回落到 `ADMIN_TOKEN`。

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
| `TZ` | `Asia/Shanghai` | 影响「今天」的判定与定时时间 |

高级项（一般不用动）：`TRAE_CALLBACK_HOST` / `TRAE_CALLBACK_PORT`，改动后回调地址随之变化，自动回调可能失效。

---

## 六、数据与安全

```
data/
├── master.key      # 32 字节主密钥（base64），0600
├── admin_token     # 自动生成的登录密码，0600
└── kv/             # 每条账号记录一个文件，内容是 AES-256-GCM 密文
```

- 密钥派生：`HKDF-SHA256(MASTER_KEY, 随机 16B salt, "trae-signin/v1/account" | "wb-signin/v1/account")`
- 每条记录随机 salt + IV，密文格式与上游 Worker 一致（`{"v":1,"s":…,"i":…,"d":…}`），
  从 Cloudflare KV 迁移过来的记录可以直接读
- 列表接口只返回备注、状态、积分，**令牌与私钥永不下发**
- **备份 `data/` 就是备份一切**：里面既有密文也有密钥，别外传、别进 Git
- 换掉 `MASTER_KEY` = 已有账号全部解不开，只能重新扫码

---

## 七、镜像与构建

```bash
# 本机架构
docker build -t workbuddy-daily:latest .

# 多架构（需要 buildx）
docker buildx build --platform linux/amd64,linux/arm64 -t <tag> --push .
```

7 MB 的来源：Go 静态二进制（`-trimpath -ldflags="-s -w"`，约 7.0 MB）+ `ca-certificates`（约 0.2 MB），
运行镜像 `FROM scratch`，不含 shell、包管理器、Node、Python。

时区库用 `time/tzdata` 编进二进制，所以 `scratch` 里也能正确按 `Asia/Shanghai` 算日期。

本地开发：

```bash
go test ./...                                   # 含与上游 JS 的逐字节比对
go build -o checkin . && ADMIN_TOKEN=dev DATA_DIR=./data ./checkin
```

---

## 八、接口速查

所有管理接口认三种口令：`X-Admin-Token: <token>`、`Authorization: Bearer <token>`、`?token=<token>`。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` | 控制台 |
| GET | `/authorize` | Trae 授权回调（自动入库，无需口令） |
| GET | `/oauth/callback` | 同上，兼容上游路径 |
| GET | `/api/ping` | 校验密码 |
| POST | `/api/run-all` | 两个平台一起签（`?force=1` 强制） |
| POST | `/cron` | 定时入口，口令走 `X-Cron-Secret` 或 `?key=` |
| GET | `/trae/api/list` · POST `/trae/api/run` | Trae 列表 / 签到 |
| POST | `/trae/api/login/start` · GET `/trae/api/login/poll` · POST `/trae/api/login/claim` | Trae 扫码三步 |
| POST | `/trae/api/accounts/remove` · POST `/trae/api/rename` | 删除 / 改备注 |
| GET | `/wb/api/list` · POST `/wb/api/run` | WorkBuddy 列表 / 签到 |
| POST | `/wb/api/login/start` · GET `/wb/api/login/poll` | WorkBuddy 扫码两步 |
| POST | `/wb/api/remove` · POST `/wb/api/rename` | 删除 / 改备注 |

---

## 九、排错

| 现象 | 原因 / 处理 |
|---|---|
| 打不开控制台 | 端口没映射或被占用：`docker compose logs checkin`；换个 `PORT` 并同时映射 |
| 「登录密码不正确」 | `.env` 里的 `ADMIN_TOKEN` 有多余空格，或改过之后没重启容器 |
| 扫码后停在「无法访问此网站」 | 浏览器不在容器所在机器——这是正常的，复制地址栏整段链接粘到控制台即可（粘上即绑定） |
| 回调页「登录会话已过期」 | 授权链接 15 分钟有效，重新点「添加账号」 |
| Trae 显示「限流 N 分钟」 | 服务端 9074 限流，已按账号冷却，到点自动重试；0 点前后冷却只有 12 分钟 |
| 「令牌失效」 | refreshToken 被别处轮换或长期未跑，删掉账号重新扫码 |
| 「缺设备密钥」 | 账号记录异常（极少见），删掉重新扫码 |
| 定时没跑 | 看日志 `定时签到已启用`；确认 `CRON_SCHEDULE` 是北京时间 `HH:MM` |
| 想确认时区对不对 | 看 `docker logs` 行首的时间戳是否为北京时间 |

---

## 十、与上游同步

`upstream/worker.js` 是原项目单文件实现，本仓库的 `trae.go` / `wb.go` 与它**逐函数对齐**
（授权链接拼装、三套换令牌变体、设备证明签名、限流与待确认状态机、WorkBuddy 令牌轮换）。

两个平台接口都是逆向得到的，客户端升级可能改路径或参数。上游更新时：

```bash
git fetch upstream && git diff upstream/main -- upstream/worker.js
```

按 diff 同步到对应的 Go 函数即可；`go test ./...` 里的基准值（`testdata/gen_fixture.js`）也可一并重生成。

---

## 致谢与声明

- 原始实现：[ggzhzc/workbuddy-trae-checkin](https://github.com/ggzhzc/workbuddy-trae-checkin)
- 本项目为非官方工具，与 Trae、WorkBuddy 及其所属公司无任何隶属关系
- 签到接口由客户端逆向得到，平台升级可能导致失效；自动化签到可能涉及服务条款，**建议只给自己的账号使用**
