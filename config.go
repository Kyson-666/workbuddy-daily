package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // 内嵌时区库：scratch 镜像里没有 zoneinfo，也要能 LoadLocation("Asia/Shanghai")
)

// Config 汇总所有部署参数，全部来自环境变量（docker compose 的 .env 即可）。
type Config struct {
	Port         string   // 控制台端口，默认 17388（与 Trae 回调端口一致，扫码天然全自动）
	CallbackPort string   // 只承接 /authorize 的端口，默认同 Port
	DataDir      string   // 数据目录，默认 /data（compose 挂卷）
	AdminToken   string   // 控制台登录密码
	AdminGen     bool     // 密码是首启自动生成的
	CronSecret   string   // /cron 独立口令，缺省回落到 AdminToken
	MasterKey    []byte   // 32 字节，AES/HKDF 主密钥
	Schedules    []string // 北京时间 "HH:MM" 列表，空 = 不自动签到
	Loc          *time.Location
	TraeCallback string // 交给 Trae 的回调地址，必须是 127.0.0.1

	KeepaliveDays int    // 连续这么久没签到成功的账号，定时只刷新令牌（0 = 关闭保活）
	KeepaliveSlot string // 保活时间点，非空时一并进 Schedules
	LogRequests   bool   // LOG_REQUESTS=1 时打印每个请求，用于排查反代
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		Port:    envOr("PORT", "17388"),
		DataDir: envOr("DATA_DIR", "/data"),
	}

	if !filepath.IsAbs(cfg.DataDir) {
		if abs, err := filepath.Abs(cfg.DataDir); err == nil {
			cfg.DataDir = abs
		}
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("数据目录不可用（%s）: %w", cfg.DataDir, err)
	}

	cfg.CallbackPort = envOr("CALLBACK_PORT", cfg.Port)

	tz := envOr("TZ", "Asia/Shanghai")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("时区 %q 无法加载: %w", tz, err)
	}
	cfg.Loc = loc

	key, err := loadMasterKey(cfg.DataDir, os.Getenv("MASTER_KEY"))
	if err != nil {
		return nil, err
	}
	cfg.MasterKey = key

	token, gen, err := loadAdminToken(cfg.DataDir, os.Getenv("ADMIN_TOKEN"))
	if err != nil {
		return nil, err
	}
	cfg.AdminToken, cfg.AdminGen = token, gen
	cfg.CronSecret = strings.TrimSpace(os.Getenv("CRON_SECRET"))

	// 定时任务时间（北京时间）。上游推荐 08:10 + 次日 00:20，两次机会互为兜底。
	raw := envOr("CRON_SCHEDULE", "08:10,00:20")
	if strings.EqualFold(strings.TrimSpace(os.Getenv("CRON_SCHEDULE")), "off") {
		raw = ""
	}
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if _, err := time.Parse("15:04", p); err != nil {
			return nil, fmt.Errorf("CRON_SCHEDULE 里的 %q 不是 HH:MM 格式", p)
		}
		cfg.Schedules = append(cfg.Schedules, p)
	}

	// 保活：默认关闭。开启后每天固定时间把「很久没签到」的账号只刷新令牌。
	if raw := strings.TrimSpace(os.Getenv("KEEPALIVE_DAYS")); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n <= 0 {
			return nil, fmt.Errorf("KEEPALIVE_DAYS 需要是正整数，当前 %q", raw)
		} else {
			cfg.KeepaliveDays = n
			slot := envOr("KEEPALIVE_TIME", "12:30")
			if _, err := time.Parse("15:04", slot); err != nil {
				return nil, fmt.Errorf("KEEPALIVE_TIME 里的 %q 不是 HH:MM 格式", slot)
			}
			cfg.KeepaliveSlot = slot
			cfg.Schedules = append(cfg.Schedules, slot)
		}
	}

	cfg.LogRequests = os.Getenv("LOG_REQUESTS") == "1"

	// Trae 只认 http://127.0.0.1:<端口> 作为回调，换域名会直接「网络错误」。
	cbHost := envOr("TRAE_CALLBACK_HOST", "127.0.0.1")
	cbPort := envOr("TRAE_CALLBACK_PORT", cfg.CallbackPort)
	cfg.TraeCallback = fmt.Sprintf("http://%s:%s/authorize", cbHost, cbPort)

	return cfg, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func addrOf(port string) string {
	if strings.Contains(port, ":") {
		return port
	}
	return ":" + port
}
