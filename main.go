package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags)
	log.SetOutput(os.Stdout)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}

	app, err := NewApp(cfg)
	if err != nil {
		log.Fatalf("初始化数据目录失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	handler := withRequestLog(cfg.LogRequests, app)

	mainAddr := addrOf(cfg.Port)
	srv := &http.Server{
		Addr:              mainAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("HTTP 服务异常: %v", err)
			stop()
		}
	}()

	// 控制台端口与 Trae 回调端口分开时，再起一个（两个端口都指向同一个 handler）
	var cbSrv *http.Server
	if cfg.CallbackPort != "" && addrOf(cfg.CallbackPort) != mainAddr {
		cbSrv = &http.Server{
			Addr:              addrOf(cfg.CallbackPort),
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			if err := cbSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logf("回调端口 %s 监听失败: %v", cfg.CallbackPort, err)
			}
		}()
	}

	app.StartScheduler(ctx)

	logf("──────────────────────────────────────────────")
	logf("签到控制台已启动")
	logf("  访问地址   http://<本机 IP>:%s", cfg.Port)
	if cbSrv != nil {
		logf("  回调端口   %s（Trae 授权跳转专用）", cfg.CallbackPort)
	}
	logf("  数据目录   %s", cfg.DataDir)
	logf("  回调地址   %s", cfg.TraeCallback)
	if cfg.AdminGen {
		logf("  登录密码   %s  （本次自动生成，已存到 %s/admin_token）", cfg.AdminToken, cfg.DataDir)
	} else {
		logf("  登录密码   已从 ADMIN_TOKEN / 数据目录读取")
	}
	if len(cfg.Schedules) > 0 {
		logf("  定时签到   %v", cfg.Schedules)
	}
	if cfg.KeepaliveSlot != "" {
		logf("  保活刷新   每天 %s（连续 %d 天未签到才刷）", cfg.KeepaliveSlot, cfg.KeepaliveDays)
	}
	if cfg.LogRequests {
		logf("  访问日志   已开启（LOG_REQUESTS=1）")
	}
	if cfg.TraeCallback != "http://127.0.0.1:"+cfg.CallbackPort+"/authorize" {
		logf("  提示       自定义回调地址后，扫码需要手动粘贴回调链接")
	}
	logf("──────────────────────────────────────────────")

	<-ctx.Done()
	logf("收到退出信号，正在关闭…")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if cbSrv != nil {
		_ = cbSrv.Shutdown(shutdownCtx)
	}
	logf("已退出")
}
