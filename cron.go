package main

import (
	"context"
	"strings"
	"time"
)

// StartScheduler 常驻一个 20 秒的心跳，命中配置的 HH:MM 就整体跑一轮。
// 相比真正的 cron 解析器，这里只支持「每天某时刻」，够用且不会因为时区/夏令时出错。
func (a *App) StartScheduler(ctx context.Context) {
	if len(a.cfg.Schedules) == 0 {
		logf("定时签到未启用（CRON_SCHEDULE 为空）")
		return
	}
	logf("定时签到已启用：每天 %s（%s）", strings.Join(a.cfg.Schedules, "、"), a.cfg.Loc)

	go func() {
		fired := map[string]string{} // 时间点 -> 已执行日期，防同一分钟内重复触发
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()

		check := func() {
			now := time.Now().In(a.cfg.Loc)
			slot := now.Format("15:04")
			day := now.Format("2006-01-02")

			// 先清掉昨天的记录：既保证「同一天同一时刻只跑一次」，又不会让 map 无限增长
			for k, v := range fired {
				if v != day {
					delete(fired, k)
				}
			}

			for _, want := range a.cfg.Schedules {
				if want != slot || fired[want] == day {
					continue
				}
				fired[want] = day
				a.runScheduled(want)
			}
		}

		check()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
}

func (a *App) runScheduled(slot string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	logf("⏰ 定时签到开始（%s）", slot)
	started := time.Now()

	trae := a.runTrae(ctx, false)
	logf("   Trae     账号 %d · 签到成功 %d · 今日已签 %d · 失败 %d",
		trae.Accounts, trae.Checked, trae.Already, trae.Failed)

	wb := a.runWB(ctx, false)
	logf("   WorkBuddy 账号 %d · 签到成功 %d · 今日已签 %d · 失败 %d",
		wb.Accounts, wb.Checked, wb.Already, wb.Failed)

	for _, r := range append(trae.Results, wb.Results...) {
		if r.State == "error" || r.State == "token_dead" {
			logf("   ! %s: %s", r.Label, r.Msg)
		}
	}
	logf("⏰ 定时签到结束，用时 %.1fs", time.Since(started).Seconds())
}
