package kugou

// 本文件只测 EnsurePatched 返回错误之后 Start 的处置：停止修补（受阻、定时重查）、等待重试还是照常等 CDP，以及等待重试时的提示
// 只打一次。

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
)

// TestClassifyEnsureErr 钉死分流：找不到安装时，酷狗不在运行则等它启动后按进程定位（任意路径安装）；在运行时
// 连续 maxRunningInstallMisses 轮（含本轮）仍找不到才停止修补，之前同样等待重试（找安装与查运行之间的竞态、短暂
// 两个主进程）；运行态未就绪（ErrRunningNotReady）与启动另一份安装（ErrInstallChanged）均等待重试、不计入
// runningMisses；版本指纹不认识无论是否在运行都停止修补（立即重试只会反复弹 UAC）；其余照常。按错误的身份判定
// （errors.Is），不按文案。nil 不在本函数的定义域内（EnsurePatched 成功时 Start 不调用它）。
//
// 变异自证（均实测变红）：ErrInstallNotFound 在运行时不看次数、一律停止修补（即 #205 之前的行为）；一律等待重试；
// 次数比较写成 >（满 5 次不停止修补）；停止修补不看是否在运行；ErrVersionUnsupported 只认未包装的；按 err.Error()
// 的文案判定；classifyEnsureErr 不处理 ErrRunningNotReady（落到 ensureContinue）。
func TestClassifyEnsureErr(t *testing.T) {
	if maxRunningInstallMisses != 5 {
		t.Fatalf("maxRunningInstallMisses = %d，裁定为 5", maxRunningInstallMisses)
	}
	wrappedMissing := fmt.Errorf("locate: %w", watchdog.ErrInstallNotFound)
	cases := []struct {
		name    string
		err     error
		running bool
		misses  int
		want    ensureAction
	}{
		{"找不到安装、未运行 → 等待重试", watchdog.ErrInstallNotFound, false, 0, ensureRetryLater},
		{"找不到安装、未运行、次数已满 → 次数不影响，等待重试", watchdog.ErrInstallNotFound, false, 5, ensureRetryLater},
		{"找不到安装、未运行、次数远超 → 次数不影响，等待重试", watchdog.ErrInstallNotFound, false, 100, ensureRetryLater},
		{"找不到安装、在运行、0 次 → 等待重试", watchdog.ErrInstallNotFound, true, 0, ensureRetryLater},
		{"找不到安装、在运行、第 1 次 → 等待重试", watchdog.ErrInstallNotFound, true, 1, ensureRetryLater},
		{"找不到安装、在运行、4 次 → 等待重试", watchdog.ErrInstallNotFound, true, 4, ensureRetryLater},
		{"找不到安装、在运行、满 5 次 → 停止修补", watchdog.ErrInstallNotFound, true, 5, ensureStop},
		{"找不到安装、在运行、6 次 → 停止修补", watchdog.ErrInstallNotFound, true, 6, ensureStop},
		{"被 %w 包装的找不到安装、未运行 → 等待重试", wrappedMissing, false, 0, ensureRetryLater},
		{"被 %w 包装的找不到安装、在运行、4 次 → 等待重试", wrappedMissing, true, 4, ensureRetryLater},
		{"被 %w 包装的找不到安装、在运行、满 5 次 → 停止修补", wrappedMissing, true, 5, ensureStop},
		{"启动的是另一份安装、在运行 → 等待重试", watchdog.ErrInstallChanged, true, 0, ensureRetryLater},
		{"启动的是另一份安装、在运行、次数已满 → 次数只对找不到安装起作用，等待重试", watchdog.ErrInstallChanged, true, 5, ensureRetryLater},
		{"被 %w 包装的启动的是另一份安装、未运行 → 等待重试", fmt.Errorf("%w: started D:\\KuGou.exe", watchdog.ErrInstallChanged), false, 0, ensureRetryLater},
		{"运行态未就绪、在运行 → 等待重试", watchdog.ErrRunningNotReady, true, 0, ensureRetryLater},
		{"运行态未就绪、在运行、次数已满 → 次数只对找不到安装起作用，等待重试", watchdog.ErrRunningNotReady, true, 5, ensureRetryLater},
		{"运行态未就绪、未运行 → 等待重试", watchdog.ErrRunningNotReady, false, 0, ensureRetryLater},
		{"被 %w 包装的运行态未就绪、在运行 → 等待重试", fmt.Errorf("%w: KuGou (PID 25940) has no CEF child process yet", watchdog.ErrRunningNotReady), true, 5, ensureRetryLater},
		{"版本不支持、未运行 → 停止修补", watchdog.ErrVersionUnsupported, false, 0, ensureStop},
		{"版本不支持、在运行 → 停止修补", watchdog.ErrVersionUnsupported, true, 0, ensureStop},
		{"被 %w 包装的版本不支持 → 停止修补", fmt.Errorf("%w: KuGou 10.1.94 is in the 10.x series", watchdog.ErrVersionUnsupported), false, 0, ensureStop},
		{"反例：其他错误、未运行 → 照常", errors.New("x"), false, 0, ensureContinue},
		{"反例：其他错误、在运行、次数已满 → 次数只对找不到安装起作用，照常", errors.New("x"), true, 5, ensureContinue},
		{"反例：文案相同而身份不同的错误、在运行、次数已满 → 照常", errors.New(watchdog.ErrInstallNotFound.Error()), true, 5, ensureContinue},
		{"反例：文案相同而身份不同的运行态未就绪 → 照常", errors.New(watchdog.ErrRunningNotReady.Error()), true, 0, ensureContinue},
		{"反例：确认未就绪不是找不到安装 → 照常", watchdog.ErrConfirmNotReady, false, 0, ensureContinue},
		{"反例：提权失败由 Start 先行处理，落到本函数时按其他错误", watchdog.ErrElevationFailed, false, 0, ensureContinue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyEnsureErr(c.err, c.running, c.misses); got != c.want {
				t.Fatalf("classifyEnsureErr(%v, running=%v, misses=%d) = %d，应为 %d", c.err, c.running, c.misses, got, c.want)
			}
		})
	}
}

// TestInstallWaitNotifiesOnce 钉死等待重试时的提示：连续多轮找不到安装只打一次，找到安装后重置，下一次
// 找不到时再打一次。Start 每 2 秒重试一轮，不去重就每 2 秒一条。
//
// 变异自证（均实测变红）：notify 恒返回 true；reset 不清除。
func TestInstallWaitNotifiesOnce(t *testing.T) {
	var w installWait
	if !w.notify() {
		t.Fatal("第一次找不到安装应打提示")
	}
	for i := 2; i <= 3; i++ {
		if w.notify() {
			t.Fatalf("连续第 %d 次找不到安装不应再打提示", i)
		}
	}
	w.reset()
	if !w.notify() {
		t.Fatal("找到安装（reset）后再次找不到，应重新打提示")
	}
	if w.notify() {
		t.Fatal("重置后的连续第二次不应再打提示")
	}

	var fresh installWait
	fresh.reset() // 还没打过就重置：不影响下一次
	if !fresh.notify() {
		t.Fatal("未打过提示时 reset 不应吞掉第一次提示")
	}
}

// TestStartRunningNotReadyNoSpamOrInstallMiss 钉死 Start 对 ErrRunningNotReady 与 ensureRetryLater 告警去重的处置：
//  1. 酷狗在运行、连续 7 轮返回相同 ErrRunningNotReady 时，始终保持 standby / cdp_disconnected（不计入 runningMisses，
//     不在第 5 轮转为 install_not_found，不进入 waitForCDP），按 loopStep（2s）重试且只打 1 次 Warn；第 8 轮错误文本变化
//     再打 1 次 Warn，第 9 轮维持新文本不重复告警；
//  2. 进程未运行（!running）或进入其他错误分支后 lastRetryWarn 清零，相同错误再次出现时重新告警；
//  3. ErrInstallChanged 同样走 lastRetryWarn 去重，不每 2s 刷屏。
//
// 变异自证（实测变红）：kugou.go 去掉 lastRetryWarn 去重、每轮直接打 log.Warn → 4 个子用例日志条数断言全红
// （连续 7+2 轮打 9 次 vs 期望 2 次；进程退出清零子例打 3 次 vs 期望 2 次；其他分支清零子例打 3 次 vs 期望 2 次；
// ErrInstallChanged 子例打 3 次 vs 期望 1 次）；classifyEnsureErr 未归入 ErrRunningNotReady → 落入 waitForCDP 变红。
func TestStartRunningNotReadyNoSpamOrInstallMiss(t *testing.T) {
	d := kugouDetails()
	errNoChild := fmt.Errorf("%w: KuGou (PID 25940) has no CEF child process yet", watchdog.ErrRunningNotReady)
	errHandoff := fmt.Errorf("%w: KuGou launcher is handing off to HD_KuGou.exe", watchdog.ErrRunningNotReady)

	t.Run("连续7轮相同错误只打1次Warn且不误报找不到安装_第8轮换原因再打1次", func(t *testing.T) {
		sc := startScript{
			until:   17,
			running: always(true),
			ensure: func(_ time.Duration, n int) error {
				if n <= 7 {
					return errNoChild
				}
				return errHandoff
			},
		}
		r := runStartScript(t, sc)
		r.check(t, sc, []timedStatus{
			at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
		}, secs(0, 2, 4, 6, 8, 10, 12, 14, 16), []time.Duration{})
		if len(r.connectAt) != 0 {
			t.Errorf("ErrRunningNotReady 期间不应连接 CDP，实于 %v 调用", r.connectAt)
		}
		if n := strings.Count(r.logs, "[!]"); n != 2 {
			t.Errorf("Warn 打了 %d 次，期望 2 次（前 7 轮 1 次 + 第 8 轮换原因 1 次）；日志：\n%s", n, r.logs)
		}
		if n := strings.Count(r.logs, errNoChild.Error()); n != 1 {
			t.Errorf("第 1 种原因出现 %d 次，期望 1 次；日志：\n%s", n, r.logs)
		}
		if n := strings.Count(r.logs, errHandoff.Error()); n != 1 {
			t.Errorf("第 2 种原因出现 %d 次，期望 1 次；日志：\n%s", n, r.logs)
		}
	})

	t.Run("进程退出清零后相同错误重新告警", func(t *testing.T) {
		// 0s(running=true) 进 ensure 耗时 1s 后返回 errNoChild，打第 1 次 Warn 并设 2s 后(3s)重查；
		// 1s 回到循环顶部时 running=false，仅走顶部 !running 清零并睡 2s（不调 ensure、不走其他分支）；
		// 3s(running=true) 再次返回 errNoChild 应重新打第 2 次 Warn；5s(running=true) 再次返回 errNoChild 去重。
		sc := startScript{
			until: 6,
			running: func(now time.Duration) bool {
				return now < 500*time.Millisecond || now >= 2500*time.Millisecond
			},
			ensure: func(_ time.Duration, n int) error {
				if n == 1 {
					time.Sleep(time.Second)
				}
				return errNoChild
			},
		}
		r := runStartScript(t, sc)
		r.check(t, sc, []timedStatus{
			at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
			at(1, waitingProcess, player.ReasonProcessExited, d.Exited),
			at(3, standby, player.ReasonCDPDisconnected, d.Disconnected),
		}, secs(0, 3, 5), []time.Duration{})
		if len(r.connectAt) != 0 {
			t.Errorf("ErrRunningNotReady 期间不应连接 CDP，实于 %v 调用", r.connectAt)
		}
		if n := strings.Count(r.logs, "[!]"); n != 2 {
			t.Errorf("!running 清零后 Warn 打了 %d 次，期望 2 次；日志：\n%s", n, r.logs)
		}
	})

	t.Run("经过其他分支清零后相同错误重新告警", func(t *testing.T) {
		sc := startScript{
			until:   7,
			running: always(true),
			ensure: func(_ time.Duration, n int) error {
				if n == 2 {
					return watchdog.ErrInstallNotFound
				}
				return errNoChild
			},
		}
		r := runStartScript(t, sc)
		r.check(t, sc, []timedStatus{
			at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
		}, secs(0, 2, 4, 6), []time.Duration{})
		if len(r.connectAt) != 0 {
			t.Errorf("ensureRetryLater 期间不应连接 CDP，实于 %v 调用", r.connectAt)
		}
		if n := strings.Count(r.logs, "[!]"); n != 2 {
			t.Errorf("经 ErrInstallNotFound 清零后 Warn 打了 %d 次，期望 2 次；日志：\n%s", n, r.logs)
		}
	})

	t.Run("ErrInstallChanged连续多轮同样去重", func(t *testing.T) {
		errChanged := fmt.Errorf("%w: started D:\\KuGou\\KuGou.exe", watchdog.ErrInstallChanged)
		sc := startScript{
			until:   5,
			running: always(true),
			ensure:  failWith(errChanged),
		}
		r := runStartScript(t, sc)
		r.check(t, sc, []timedStatus{
			at(0, standby, player.ReasonCDPDisconnected, d.Disconnected),
		}, secs(0, 2, 4), []time.Duration{})
		if n := strings.Count(r.logs, "[!]"); n != 1 {
			t.Errorf("ErrInstallChanged 连续 3 轮 Warn 打了 %d 次，期望 1 次；日志：\n%s", n, r.logs)
		}
	})
}
