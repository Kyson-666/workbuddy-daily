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

	if slot == a.cfg.KeepaliveSlot {
		a.runKeepalive(ctx)
		return
	}

	logf("⏰ 定时签到开始（%s）", slot)
	started := time.Now()

	summaries := a.runAll(ctx, false)
	for _, e := range a.regs {
		s, _ := summaries[e.p.Key()].(runSummary)
		logf("   %s 账号 %d · 签到成功 %d · 今日已签 %d · 冷却 %d · 失败 %d",
			e.p.Label(), s.Accounts, s.Checked, s.Already, s.Cooling, s.Failed)
		for _, r := range s.Results {
			if r.State == "error" || r.State == "token_dead" {
				logf("   ! %s: %s", r.Label, r.Msg)
			}
		}
	}
	logf("⏰ 定时签到结束，用时 %.1fs", time.Since(started).Seconds())
}

// runKeepalive 只刷新令牌，不签到：长期不跑的账号不会因为令牌过期而变成 token_dead。
//
// 注意：Trae / WorkBuddy 的 refreshToken 都是轮换制，刷新会让官方客户端里的旧令牌
// 失效。所以这个功能默认关闭，且只处理「很久没签到」的账号。
func (a *App) runKeepalive(ctx context.Context) {
	logf("🔁 保活刷新开始（连续 %d 天未签到）", a.cfg.KeepaliveDays)
	started := time.Now()
	today := time.Now().In(a.cfg.Loc).Format("2006-01-02")

	done, skipped, failed := 0, 0, 0
	for _, e := range a.regs {
		kp, ok := e.p.(keepaliver)
		if !ok {
			continue
		}
		st := a.storeOf(e.p.Key())
		list, err := st.ListMeta()
		if err != nil {
			logf("   ! %s 读取账号列表失败: %s", e.p.Label(), err.Error())
			continue
		}
		for _, meta := range list {
			acct, err := st.Get(meta.ID)
			if err != nil || acct == nil {
				continue
			}
			if daysBetween(acct.LastOkDate, today, a.cfg.Loc) < a.cfg.KeepaliveDays {
				skipped++
				continue
			}
			if err := kp.Keepalive(ctx, a, acct); err != nil {
				failed++
				logf("   ! %s %s: %s", e.p.Label(), acct.Label, err.Error())
				continue
			}
			done++
		}
	}
	logf("🔁 保活刷新结束：刷新 %d · 跳过 %d · 失败 %d，用时 %.1fs",
		done, skipped, failed, time.Since(started).Seconds())
}
