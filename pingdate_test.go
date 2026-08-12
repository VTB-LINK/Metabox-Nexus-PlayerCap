package main

// 本文件测「今天报过没有」这个判据。
//
// 它决定 DAU 准不准，而它错了**只表现为曲线偏低**：没有报错、没有日志、没人会去查。
// 三种错法各有一条用例守着：
//
//	跨了日却不发   → 长期挂机的机器整段时间不计活跃（正是心跳要解决的那个问题本身）
//	同一日重复发   → 浪费请求（不影响去重后的 DAU，但会污染「启动次数」那个数）
//	启动失败不补发 → 开机时断网的机器当天彻底不计

import (
	"testing"
	"time"
)

// withPingDate 把 lastPingDate 设成指定值并在用例结束后还原。
// lastPingDate 是包级状态，用例之间必须互不污染。
func withPingDate(t *testing.T, date string) {
	t.Helper()
	orig := lastPingDate
	lastPingDate = date
	t.Cleanup(func() { lastPingDate = orig })
}

func at(t *testing.T, layout string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", layout, time.Local)
	if err != nil {
		t.Fatalf("解析时间 %q 失败: %v", layout, err)
	}
	return parsed
}

// TestShouldPingOnFreshStart 本次运行还一次都没发出去时必须发。
//
// 覆盖的真实场景：开机自启，网卡还没拿到地址，启动时那次版本检查连不上 —— 那台机器
// 当天还没被计过。lastPingDate 保持空串正是为了让下一个唤醒点补上。
func TestShouldPingOnFreshStart(t *testing.T) {
	withPingDate(t, "")
	if !shouldPing(at(t, "2026-08-10 09:00")) {
		t.Fatal("本次运行还没成功发过，却判为「今天已报过」—— 启动时断网的机器会整天不计")
	}
}

func TestShouldNotPingTwiceSameDay(t *testing.T) {
	now := at(t, "2026-08-10 09:00")
	withPingDate(t, "")

	markPinged(now)

	for _, later := range []string{"2026-08-10 09:01", "2026-08-10 18:30", "2026-08-10 23:59"} {
		if shouldPing(at(t, later)) {
			t.Errorf("%s 判为需要发送，但今天 09:00 已经发过了", later)
		}
	}
}

// TestShouldPingAfterDateChange 钉死跨日就得发。
//
// 变异自证：把 shouldPing 的比较改成恒 false，本用例当场红。
func TestShouldPingAfterDateChange(t *testing.T) {
	withPingDate(t, "")
	markPinged(at(t, "2026-08-10 23:59"))

	if !shouldPing(at(t, "2026-08-11 00:00")) {
		t.Fatal("跨过零点后仍判为不需要发送 —— 长期挂机的机器只有第一天算活跃，" +
			"心跳等于白加")
	}
}

// TestShouldPingAcrossLongUptime 连开多天，每天都得有一次。
//
// 这条是心跳存在的全部理由：主播连播一周，DAU 必须是 7 天各一次，不是第一天一次。
func TestShouldPingAcrossLongUptime(t *testing.T) {
	withPingDate(t, "")
	start := at(t, "2026-08-10 20:00")

	for day := 0; day < 7; day++ {
		now := start.AddDate(0, 0, day)
		if !shouldPing(now) {
			t.Fatalf("第 %d 天（%s）判为不需要发送", day+1, now.Format(pingDateLayout))
		}
		markPinged(now)
		// 同一天内再问一次不该发 —— 每天恰好一次，不多不少。
		if shouldPing(now.Add(2 * time.Hour)) {
			t.Fatalf("第 %d 天发过之后又判为需要发送", day+1)
		}
	}
}

// TestPingDateUsesLocalCalendarDay 钉死判据走的是本地日历日，不是 UTC。
//
// 主播在 UTC+8。若用 UTC 计日，本地 08:00 之前都还算「昨天」—— 早上开播的机器
// 会被算进前一天，晚上 20:00 到零点这段又跨了 UTC 日、当天被计两次。
// 两种错都只体现为曲线形状怪，不会报错。
func TestPingDateUsesLocalCalendarDay(t *testing.T) {
	// 固定一个 UTC+8 的时刻：本地 2026-08-10 07:00，对应 UTC 2026-08-09 23:00。
	loc := time.FixedZone("CST", 8*3600)
	local := time.Date(2026, 8, 10, 7, 0, 0, 0, loc)

	if got, want := local.Format(pingDateLayout), "2026-08-10"; got != want {
		t.Fatalf("本地日历日算成了 %q，期望 %q（UTC 那天是 08-09）", got, want)
	}

	withPingDate(t, "")
	markPinged(local)
	if shouldPing(local.Add(3 * time.Hour)) {
		t.Error("同一本地日内判为需要重发")
	}
	if !shouldPing(local.AddDate(0, 0, 1)) {
		t.Error("次日判为不需要发送")
	}
}
