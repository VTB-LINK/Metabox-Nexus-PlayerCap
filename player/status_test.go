package player

// 本文件只测 status.go 的非歌名状态合成（ProcessStatus、NoSongStatus）与重查等待（RecheckWait），#231。

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestProcessStatus 钉住设计 §3.2 的取值表：进程是否在运行先于一切，standby 只在进程在运行时出现。
//
// 变异自证（均实测变红）：
//   - running 为假时按 Blocked 出 standby（`if !running && b.Reason == ""`）：「未运行」而带 Reason 的五例红；
//   - seen 的判断取反（`if !seen`）：「未运行」的八例红；
//   - running 为真时不判 Blocked、一律 cdp_disconnected：「运行」而带 Reason 的六例红；
//   - 没有受阻改按零值判（`b == (Blocked{})`）：「运行·只填了 NotRunningDetail」一例的 reason 为空，红。
func TestProcessStatus(t *testing.T) {
	d := ProcessDetails{NotRunning: "未启动", Exited: "已退出", Disconnected: "调试通道断开"}
	ver := Blocked{Reason: ReasonVersionUnsupported, Detail: "版本不支持", NotRunningDetail: "未启动；版本不支持"}
	admin := Blocked{Reason: ReasonNoAdmin, Detail: "无权读取"} // NotRunningDetail 为空
	meta := Blocked{Reason: ReasonMetadataUnavailable, Detail: "读不到曲目信息"}
	onlyNotRunning := Blocked{NotRunningDetail: "未启动；已放弃提权"} // 没有 Reason

	cases := []struct {
		name          string
		running, seen bool
		b             Blocked
		want          StatusInfo
	}{
		// 进程不在运行、从未见过：process_not_running。
		{"未运行·未见过·未受阻", false, false, Blocked{}, StatusInfo{"waiting_process", "未启动", ReasonProcessNotRunning}},
		{"未运行·未见过·受阻带 NotRunningDetail（不得为 standby）", false, false, ver, StatusInfo{"waiting_process", "未启动；版本不支持", ReasonProcessNotRunning}},
		{"未运行·未见过·受阻无 NotRunningDetail（退回 NotRunning）", false, false, admin, StatusInfo{"waiting_process", "未启动", ReasonProcessNotRunning}},
		{"未运行·未见过·metadata_unavailable 不带出", false, false, meta, StatusInfo{"waiting_process", "未启动", ReasonProcessNotRunning}},
		{"未运行·未见过·只有 NotRunningDetail", false, false, onlyNotRunning, StatusInfo{"waiting_process", "未启动；已放弃提权", ReasonProcessNotRunning}},
		// 进程不在运行、见过：process_exited。
		{"未运行·见过·未受阻", false, true, Blocked{}, StatusInfo{"waiting_process", "已退出", ReasonProcessExited}},
		{"未运行·见过·受阻带 NotRunningDetail（优先于 Exited）", false, true, ver, StatusInfo{"waiting_process", "未启动；版本不支持", ReasonProcessExited}},
		{"未运行·见过·受阻无 NotRunningDetail（退回 Exited）", false, true, admin, StatusInfo{"waiting_process", "已退出", ReasonProcessExited}},
		// 进程在运行、未受阻：cdp_disconnected，seen 不影响。
		{"运行·未见过·未受阻", true, false, Blocked{}, StatusInfo{"standby", "调试通道断开", ReasonCDPDisconnected}},
		{"运行·见过·未受阻", true, true, Blocked{}, StatusInfo{"standby", "调试通道断开", ReasonCDPDisconnected}},
		{"运行·只填了 NotRunningDetail（按未受阻）", true, true, onlyNotRunning, StatusInfo{"standby", "调试通道断开", ReasonCDPDisconnected}},
		// 进程在运行、受阻：standby 加 Blocked 的原因与 Detail，NotRunningDetail 不用。
		{"运行·未见过·版本不支持", true, false, ver, StatusInfo{"standby", "版本不支持", ReasonVersionUnsupported}},
		{"运行·见过·版本不支持", true, true, ver, StatusInfo{"standby", "版本不支持", ReasonVersionUnsupported}},
		{"运行·no_admin", true, true, admin, StatusInfo{"standby", "无权读取", ReasonNoAdmin}},
		{"运行·metadata_unavailable 原样带出", true, true, meta, StatusInfo{"standby", "读不到曲目信息", ReasonMetadataUnavailable}},
		{"运行·install_not_found", true, false, Blocked{Reason: ReasonInstallNotFound, Detail: "未找到安装"}, StatusInfo{"standby", "未找到安装", ReasonInstallNotFound}},
		{"运行·显式 cdp_disconnected 用 Blocked 的 Detail", true, true, Blocked{Reason: ReasonCDPDisconnected, Detail: "需以调试端口启动"}, StatusInfo{"standby", "需以调试端口启动", ReasonCDPDisconnected}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ProcessStatus(c.running, c.seen, d, c.b)
			if got == nil {
				t.Fatal("返回 nil")
			}
			if *got != c.want {
				t.Errorf("ProcessStatus(%v, %v, …, %+v) = %+v，期望 %+v", c.running, c.seen, c.b, *got, c.want)
			}
			if got.Status == "" || got.Reason == "" {
				t.Errorf("Status 与 Reason 都应非空：%+v", *got)
			}
			if !c.running && got.Status == "standby" {
				t.Errorf("进程不在运行却报 standby：%+v", *got)
			}
			if !c.seen && got.Reason == ReasonProcessExited {
				t.Errorf("没见过进程却报 process_exited：%+v", *got)
			}
		})
	}

	// 每次返回新的指针：调用方可能在发出后持有它，共用一个实例会被下一次合成改写。
	if ProcessStatus(false, false, d, Blocked{}) == ProcessStatus(false, false, d, Blocked{}) {
		t.Error("两次调用返回了同一个指针")
	}
}

// TestNoSongStatus reason 与 detail 原样带出，状态恒为 waiting_song。
//
// 变异自证（均实测变红）：reason 写死为 ReasonNoSong；不带 Detail。
func TestNoSongStatus(t *testing.T) {
	for _, c := range []struct{ reason, detail string }{
		{ReasonNoSong, "当前没有歌曲"},
		{ReasonWindowNotOpen, "K歌窗口未打开"},
	} {
		got := NoSongStatus(c.reason, c.detail)
		want := StatusInfo{Status: "waiting_song", Detail: c.detail, Reason: c.reason}
		if got == nil || *got != want {
			t.Errorf("NoSongStatus(%q, %q) = %+v，期望 %+v", c.reason, c.detail, got, want)
		}
	}
}

// countRunning 返回一个计调用次数的 running 回调：第 falseAt 次（从 1 起）返回假，falseAt 为 0 时恒真。
func countRunning(falseAt int64) (func() bool, *atomic.Int64) {
	var n atomic.Int64
	return func() bool { return n.Add(1) != falseAt }, &n
}

// TestRecheckWait
//
// 变异自证（均实测变红）：
//   - 每段不截到 step、一次等满 total：「第 k 次为假」与「恒真时的调用次数」两例红；
//   - 不理会 running 的结果：「第 k 次为假」一例红（等满 total、调用次数超过 k）；
//   - 不 select stop、只等定时器：两个 stop 例红（等满 total）。
func TestRecheckWait(t *testing.T) {
	t.Run("等满返回 (false,false)", func(t *testing.T) {
		running, n := countRunning(0)
		start := time.Now()
		stopped, gone := RecheckWait(make(chan struct{}), 40*time.Millisecond, 10*time.Millisecond, running)
		if stopped || gone {
			t.Errorf("= (%v, %v)，期望 (false, false)", stopped, gone)
		}
		if el := time.Since(start); el < 40*time.Millisecond || el > 2*time.Second {
			t.Errorf("用时 %v，期望约 40ms", el)
		}
		if got := n.Load(); got < 1 || got > 4 {
			t.Errorf("running 调用 %d 次，期望 1～4 次", got)
		}
	})

	t.Run("第 k 次 running 为假即返回且不再调用", func(t *testing.T) {
		running, n := countRunning(3)
		start := time.Now()
		stopped, gone := RecheckWait(make(chan struct{}), 10*time.Second, 5*time.Millisecond, running)
		if stopped || !gone {
			t.Errorf("= (%v, %v)，期望 (false, true)", stopped, gone)
		}
		if el := time.Since(start); el > 5*time.Second {
			t.Errorf("用时 %v：进程退出没有在一个 step 内察觉", el)
		}
		if got := n.Load(); got != 3 {
			t.Errorf("running 调用 %d 次，期望恰为 3 次", got)
		}
	})

	t.Run("stop 已关闭即返回 (true,false)", func(t *testing.T) {
		stop := make(chan struct{})
		close(stop)
		running, n := countRunning(0)
		start := time.Now()
		stopped, gone := RecheckWait(stop, 10*time.Second, time.Second, running)
		if !stopped || gone {
			t.Errorf("= (%v, %v)，期望 (true, false)", stopped, gone)
		}
		if el := time.Since(start); el > 500*time.Millisecond {
			t.Errorf("用时 %v，stop 已关闭应立即返回", el)
		}
		if got := n.Load(); got != 0 {
			t.Errorf("running 调用 %d 次，期望 0 次", got)
		}
	})

	t.Run("等待中途 stop 即返回 (true,false)", func(t *testing.T) {
		stop := make(chan struct{})
		time.AfterFunc(20*time.Millisecond, func() { close(stop) })
		running, _ := countRunning(0)
		start := time.Now()
		stopped, gone := RecheckWait(stop, 10*time.Second, 5*time.Millisecond, running)
		if !stopped || gone {
			t.Errorf("= (%v, %v)，期望 (true, false)", stopped, gone)
		}
		if el := time.Since(start); el > 5*time.Second {
			t.Errorf("用时 %v，stop 之后没有及时返回", el)
		}
	})

	t.Run("step 大于 total 时至多调用一次、不等满 step", func(t *testing.T) {
		running, n := countRunning(0)
		start := time.Now()
		stopped, gone := RecheckWait(make(chan struct{}), 10*time.Millisecond, 5*time.Second, running)
		if stopped || gone {
			t.Errorf("= (%v, %v)，期望 (false, false)", stopped, gone)
		}
		if el := time.Since(start); el > 2*time.Second {
			t.Errorf("用时 %v：等的是 step 而不是 total", el)
		}
		if got := n.Load(); got > 1 {
			t.Errorf("running 调用 %d 次，期望至多 1 次", got)
		}
	})

	t.Run("running 恒真时调用次数约为 total/step", func(t *testing.T) {
		running, n := countRunning(0)
		RecheckWait(make(chan struct{}), 200*time.Millisecond, 10*time.Millisecond, running)
		// 上界 total/step 是确定的（除最后一段外每段都等满 step）；下界留四倍余量，容定时器在负载下延迟。
		if got := n.Load(); got < 5 || got > 20 {
			t.Errorf("running 调用 %d 次，期望约 20 次（5～20）", got)
		}
	})
}
