package watchdog

// 本文件只测等酷狗启动的判定（awaitKuGou）与结束酷狗之前的闸门（readyToKill、ElevatedHelper.running），
// 即 helper 先于酷狗启动退出时不结束酷狗（issue #141）。不起真酷狗：酷狗是否在跑、helper 是否还在都由脚本给出，
// running 用本测试二进制长睡的子进程充当 helper。

import (
	"testing"
	"time"
)

// kugouScript 按调用次数给出 awaitKuGou 的输入，并记下各被调了几次。
type kugouScript struct {
	noHelper     bool // 没有 helper（helperRunning 传 nil）
	helperExitAt int  // helperRunning 第几次调用（从 0 计）起返回 false；负数表示一直在
	kugouAt      int  // kugouRunning 第几次调用起返回 true；负数表示一直不出现
	stopAt       int  // 第几次调用 kugouRunning 时（返回之前）关闭 stopCh；负数表示不关
	stopBefore   bool // 开始之前就关闭 stopCh

	stop            chan struct{}
	stopped         bool
	nHelper, nKuGou int
}

func (s *kugouScript) helperRunning() bool {
	n := s.nHelper
	s.nHelper++
	return s.helperExitAt < 0 || n < s.helperExitAt
}

func (s *kugouScript) kugouRunning() bool {
	n := s.nKuGou
	s.nKuGou++
	// 第 1000 次起关闭 stopCh：变异使循环不收敛时以 kugouWaitStopped 判红，而不是挂起。
	if !s.stopped && ((s.stopAt >= 0 && n >= s.stopAt) || n >= 1000) {
		close(s.stop)
		s.stopped = true
	}
	return s.kugouAt >= 0 && n >= s.kugouAt
}

// TestAwaitKuGou 断言等酷狗的结论：helper 一直在时酷狗出现即可结束它；helper 在等待中退出时不就此返回、
// 接着等，酷狗出现时报「helper 已不在」；同一轮里 helper 退出与酷狗出现同时发生时同样报「helper 已不在」；
// stopCh 关闭即停，开始前就关闭时即使酷狗已在也不看。
//
// 变异自证（隔离副本，实测）：helper 退出即返回（不再等酷狗）→ helper 退出的四例红（含「helper 在等待中退出、
// 酷狗一直不来」）；先看酷狗、后看 helper → 红（含「同一轮」两例）；helper 退出后仍每轮再问 → helper 在等待中
// 退出的两例红；轮间等待改为不理会 stopCh 的 time.Sleep → 「轮间关闭 stopCh」挂起，由 go test -timeout 判红；
// 轮首不看 stopCh → 「开始前 stopCh 已关」两例红。
func TestAwaitKuGou(t *testing.T) {
	cases := []struct {
		name       string
		s          kugouScript
		interval   time.Duration
		want       kugouWait
		wantHelper int // helperRunning 被调用的次数（负数不查）
		wantKuGou  int // kugouRunning 被调用的次数（负数不查）
	}{
		{"helper 一直在，第 3 轮酷狗出现", kugouScript{helperExitAt: -1, kugouAt: 2, stopAt: -1}, time.Millisecond, kugouStarted, 3, 3},
		{"开始时酷狗已在", kugouScript{helperExitAt: -1, kugouAt: 0, stopAt: -1}, time.Millisecond, kugouStarted, 1, 1},
		{"没有 helper，酷狗出现", kugouScript{noHelper: true, helperExitAt: -1, kugouAt: 2, stopAt: -1}, time.Millisecond, kugouStarted, 0, 3},
		{"helper 在等待中退出，之后酷狗出现", kugouScript{helperExitAt: 2, kugouAt: 5, stopAt: -1}, time.Millisecond, kugouStartedNoHelper, 3, 6},
		{"同一轮：helper 退出且酷狗出现", kugouScript{helperExitAt: 2, kugouAt: 2, stopAt: -1}, time.Millisecond, kugouStartedNoHelper, 3, 3},
		{"同一轮：开始时 helper 已退出、酷狗已在", kugouScript{helperExitAt: 0, kugouAt: 0, stopAt: -1}, time.Millisecond, kugouStartedNoHelper, 1, 1},
		{"helper 在等待中退出、酷狗一直不来", kugouScript{helperExitAt: 1, kugouAt: -1, stopAt: 6}, time.Millisecond, kugouWaitStopped, 2, 7},
		{"helper 一直在、酷狗一直不来", kugouScript{helperExitAt: -1, kugouAt: -1, stopAt: 4}, time.Millisecond, kugouWaitStopped, 5, 5},
		{"轮间关闭 stopCh（间隔 1 小时也立即返回）", kugouScript{helperExitAt: -1, kugouAt: -1, stopAt: 0}, time.Hour, kugouWaitStopped, 1, 1},
		{"开始前 stopCh 已关", kugouScript{helperExitAt: -1, kugouAt: -1, stopAt: -1, stopBefore: true}, time.Millisecond, kugouWaitStopped, 0, 0},
		{"开始前 stopCh 已关、酷狗已在", kugouScript{helperExitAt: -1, kugouAt: 0, stopAt: -1, stopBefore: true}, time.Millisecond, kugouWaitStopped, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.s
			s.stop = make(chan struct{})
			if s.stopBefore {
				close(s.stop)
				s.stopped = true
			}
			var helper func() bool
			if !s.noHelper {
				helper = s.helperRunning
			}
			start := time.Now()
			got := awaitKuGou(s.stop, c.interval, helper, s.kugouRunning)
			if got != c.want {
				t.Fatalf("结论 = %d，期望 %d", got, c.want)
			}
			if c.wantHelper >= 0 && s.nHelper != c.wantHelper {
				t.Errorf("helperRunning 被调用 %d 次，期望 %d", s.nHelper, c.wantHelper)
			}
			if c.wantKuGou >= 0 && s.nKuGou != c.wantKuGou {
				t.Errorf("kugouRunning 被调用 %d 次，期望 %d", s.nKuGou, c.wantKuGou)
			}
			if el := time.Since(start); el > 5*time.Second {
				t.Errorf("用时 %v，stopCh 关闭后应立即返回", el)
			}
		})
	}
}

// TestReadyToKill 断言结束酷狗之前的闸门：只有酷狗已出现且此刻 helper 仍在才放行；helper 在看到酷狗之后才退出、
// 等待时就已退出、stopCh 关闭，都不放行。
//
// 变异自证（隔离副本，实测）：去掉放行前的再查一次 → 「酷狗出现后 helper 才退出」红；helper 已不在也放行 →
// 「等待时 helper 已退出」红；stopCh 关闭也放行 → 「stopCh 关闭」红。
func TestReadyToKill(t *testing.T) {
	for _, c := range []struct {
		name      string
		w         kugouWait
		running   bool
		wantKill  bool
		wantAsked bool // 是否应再查一次 helper
	}{
		{"酷狗出现、helper 仍在", kugouStarted, true, true, true},
		{"酷狗出现后 helper 才退出", kugouStarted, false, false, true},
		{"等待时 helper 已退出", kugouStartedNoHelper, true, false, false},
		{"stopCh 关闭", kugouWaitStopped, true, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := false
			err := readyToKill(c.w, func() bool { asked = true; return c.running })
			if (err == nil) != c.wantKill {
				t.Fatalf("readyToKill = %v，期望放行 = %v", err, c.wantKill)
			}
			if asked != c.wantAsked {
				t.Errorf("是否再查 helper = %v，期望 %v", asked, c.wantAsked)
			}
		})
	}
}

// TestHelperRunning 断言 running 只在确知 helper 已退出时返回 false：进程活着为 true，已退出为 false，
// 句柄无效（等待失败、无从确认）为 true——沿用 #141 之前「结束酷狗、由 TriggerAndWait 判断」的行为，
// 免得句柄不可等待时修补流程整体失效。
//
// 变异自证（实测）：判定改成 `ev == WAIT_TIMEOUT`（等待失败当作已退出）→「句柄无效」一例红。
func TestHelperRunning(t *testing.T) {
	_, h := startSleeper(t)
	if !(&ElevatedHelper{hProcess: h}).running() {
		t.Fatal("子进程活着，running 应为 true")
	}
	killAndWait(t, h)
	if (&ElevatedHelper{hProcess: h}).running() {
		t.Error("子进程已退出，running 应为 false")
	}
	if !(&ElevatedHelper{hProcess: 0}).running() {
		t.Error("句柄无效（等待失败、无从确认），running 应为 true")
	}
}
