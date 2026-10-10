//go:build windows

package manager

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/audiocap/frame"
	"Metabox-Nexus-PlayerCap/audiocap/procscan"
)

// bindToTest 把测试起的子进程赋入 KILL_ON_JOB_CLOSE 的 Job，Job 句柄在 Cleanup 时关闭：测试进程
// 异常终止时，内核关闭该句柄并回收长睡的子进程。Job 到 Cleanup 才关，不影响用例内对 Kill 的断言。
func bindToTest(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	job, err := createKillOnCloseJob()
	if err != nil {
		t.Fatalf("createKillOnCloseJob: %v", err)
	}
	t.Cleanup(func() { windows.CloseHandle(job) })
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatalf("取子进程句柄: %v", err)
	}
	defer windows.CloseHandle(h)
	if err := assignToJob(job, h); err != nil {
		t.Fatalf("assignToJob: %v", err)
	}
}

// closedChan 返回一个已关闭的 struct{} 通道，用于构造「已退出」的 helperProc（不起真进程）。
func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// TestBumpBackoff 断言指数退避翻倍 + 封顶 backoffCap（§6「退避封顶」必备最小检查）。
func TestBumpBackoff(t *testing.T) {
	m := &Manager{}
	want := []time.Duration{
		1 * time.Second,  // base
		2 * time.Second,  // ×2
		4 * time.Second,  // ×2
		8 * time.Second,  // ×2
		16 * time.Second, // ×2
		30 * time.Second, // ×2=32 → 封顶 30
		30 * time.Second, // 保持封顶
	}
	for i, w := range want {
		m.bumpBackoff()
		if m.backoff != w {
			t.Fatalf("bumpBackoff 第 %d 次 = %v, 期望 %v", i+1, m.backoff, w)
		}
	}
}

// TestReapHelperQuickFail 构造 start 在 quickFailWindow 内的 helperProc，连续 reap：
// quickFails 递增，连满 maxQuickFails 次后 givenUp==true（此后不再退避重启）；每次 reap 都关闭 stdin。
func TestReapHelperQuickFail(t *testing.T) {
	m := &Manager{}
	for i := 1; i <= maxQuickFails; i++ {
		w := &recWriter{}
		m.helper = &helperProc{stdin: w, start: time.Now(), exited: closedChan(), job: 0}
		m.reapHelper()
		if m.quickFails != i {
			t.Fatalf("第 %d 次 reap 后 quickFails = %d, 期望 %d", i, m.quickFails, i)
		}
		if !w.closed {
			t.Fatalf("第 %d 次 reap 后 helper stdin 未关闭", i)
		}
	}
	if !m.givenUp {
		t.Fatalf("连续 %d 次快速失败后 givenUp 应为 true", maxQuickFails)
	}
}

// TestReapHelperStableResetsBackoff 断言 ①的修复：稳定运行（start 早于 quickFailWindow）分支
// 除清零 quickFails 外，还复位 backoff——下次孤立崩溃从 base 重新退避，不被历史棘轮钉死。
// 观测点：预置一个被抬高的 backoff，稳定 reap 后应回到 base（若不复位则会是 30s 封顶值）。
func TestReapHelperStableResetsBackoff(t *testing.T) {
	m := &Manager{
		backoff:    backoffCap, // 预置历史抬高到封顶
		quickFails: 3,          // 预置累计快速失败
	}
	m.helper = &helperProc{start: time.Now().Add(-2 * quickFailWindow), exited: closedChan(), job: 0}
	m.reapHelper()

	if m.quickFails != 0 {
		t.Fatalf("稳定运行后 quickFails = %d, 期望 0", m.quickFails)
	}
	// reapHelper 在复位 backoff=0 后立即 bumpBackoff → 回到 base；若未复位则会是 backoffCap。
	if m.backoff != backoffBase {
		t.Fatalf("稳定运行后 backoff = %v, 期望 %v（证明已从 0 复位重退避，而非沿历史棘轮）", m.backoff, backoffBase)
	}
	if m.givenUp {
		t.Fatalf("稳定运行后不应放弃")
	}
}

// TestReapHelperWatchdogKillQuick 断言看门狗所杀且存活不足窗口时不计 helper 快速失败、不动退避、
// 不推迟重启：预置 quickFails = maxQuickFails-1（若仍计数则本次即放弃）与一个非零 backoff。
func TestReapHelperWatchdogKillQuick(t *testing.T) {
	m := &Manager{quickFails: maxQuickFails - 1, backoff: 4 * time.Second}
	m.helper = &helperProc{start: time.Now(), exited: closedChan(), watchdogKill: true}
	m.reapHelper()

	if m.quickFails != maxQuickFails-1 {
		t.Fatalf("看门狗所杀后 quickFails = %d, 期望不变 %d", m.quickFails, maxQuickFails-1)
	}
	if m.givenUp {
		t.Fatalf("看门狗所杀不应导致 helper 放弃")
	}
	if m.backoff != 4*time.Second {
		t.Fatalf("看门狗所杀后 backoff = %v, 期望不变 %v", m.backoff, 4*time.Second)
	}
	if now := time.Now(); m.respawnAfter.After(now) {
		t.Fatalf("看门狗所杀后 respawnAfter = %v 晚于当前 %v，下一轮无法立即重启", m.respawnAfter, now)
	}
}

// TestReapHelperWatchdogKillStable 断言看门狗所杀但已存活满窗口时，清零 quickFails 与退避，且不再抬退避。
func TestReapHelperWatchdogKillStable(t *testing.T) {
	m := &Manager{backoff: backoffCap, quickFails: 3}
	m.helper = &helperProc{start: time.Now().Add(-2 * quickFailWindow), exited: closedChan(), watchdogKill: true}
	m.reapHelper()

	if m.quickFails != 0 {
		t.Fatalf("存活满窗口后 quickFails = %d, 期望 0", m.quickFails)
	}
	if m.backoff != 0 {
		t.Fatalf("存活满窗口后 backoff = %v, 期望 0", m.backoff)
	}
	if now := time.Now(); m.respawnAfter.After(now) {
		t.Fatalf("看门狗所杀后 respawnAfter = %v 晚于当前 %v，下一轮无法立即重启", m.respawnAfter, now)
	}
}

// TestTeardownHelperStableResets 断言按需拆除存活满窗口的 helper 时，与 reapHelper 稳定分支一致地
// 清零 quickFails 与 backoff；存活不足窗口的则原样保留。job 用真实 Job 句柄，不起子进程。
func TestTeardownHelperStableResets(t *testing.T) {
	for _, c := range []struct {
		name      string
		age       time.Duration
		wantFails int
		wantBack  time.Duration
	}{
		{"存活满窗口", 2 * quickFailWindow, 0, 0},
		{"存活不足窗口", 0, maxQuickFails - 1, backoffCap},
	} {
		t.Run(c.name, func(t *testing.T) {
			job, err := createKillOnCloseJob()
			if err != nil {
				t.Fatalf("createKillOnCloseJob: %v", err)
			}
			m := &Manager{quickFails: maxQuickFails - 1, backoff: backoffCap}
			m.helper = &helperProc{stdin: &recWriter{}, job: job, exited: closedChan(), start: time.Now().Add(-c.age)}
			m.teardownHelper()

			if m.quickFails != c.wantFails || m.backoff != c.wantBack {
				t.Fatalf("teardown 后 quickFails=%d backoff=%v, 期望 %d、%v", m.quickFails, m.backoff, c.wantFails, c.wantBack)
			}
		})
	}
}

// TestHelperProcess 不是独立用例：仅在 AUDIOCAP_TEST_HELPER=1 时作为子进程运行，不读 stdin、长睡，
// 模拟不响应 stdin EOF 的卡死 helper。未设该变量时直接返回。
func TestHelperProcess(t *testing.T) {
	if os.Getenv("AUDIOCAP_TEST_HELPER") != "1" {
		return
	}
	time.Sleep(time.Hour)
}

// TestReapHelperKillsLiveProcess 断言 exited 已关但进程仍活（demux panic 恢复后的情形）且 job == 0 时，
// reapHelper 以 Process.Kill 终止进程：关 stdin 不足以让卡死 helper 退出，缺 Kill 则 5s 内不退出。
// 另断言 cmd.Wait 返回错误（Kill 的退出码为 1）：子进程若因 -test.run 失配而自行以 0 退出，缺 Kill
// 时用例照样变红，不会悄悄失效。
func TestReapHelperKillsLiveProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "AUDIOCAP_TEST_HELPER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	bindToTest(t, cmd)

	m := &Manager{}
	m.helper = &helperProc{cmd: cmd, stdin: stdin, job: 0, start: time.Now(), exited: closedChan()}
	m.reapHelper()

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("子进程以 0 退出，不是被 Kill 终止（-test.run 失配？）")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("reapHelper 后 5s 内 helper 进程未退出")
	}
}

// panicSink 的 AnySubscribers 会 panic，其余方法报告无订阅：reconcile 首次查订阅即 panic，
// idleKeepalive 只调 HasRootSubscriber 与 HasSubscribers，不受影响。
type panicSink struct{}

const sinkPanicMsg = "测试注入的 reconcile panic"

func (panicSink) BroadcastAudioFrame(string, []byte) {}
func (panicSink) HasSubscribers(string) bool         { return false }
func (panicSink) HasRootSubscriber() bool            { return false }
func (panicSink) AnySubscribers() bool               { panic(sinkPanicMsg) }
func (panicSink) ActivePlayer() string               { return "" }

// TestRunPanicTearsDownHelper 断言 Run panic 展开时拆除 helper：Run 返回前子进程已被回收、m.helper
// 为 nil，且 stopCh 未关闭（不调 Stop，idleKeepalive 继续运行）。子进程不响应 stdin EOF、job 为 0，
// 只能由 teardownHelper 的 Process.Kill 终止；exited 由等待子进程退出的 goroutine 关闭，与 demux
// 的 cmd.Wait 后 close 同序。子进程命令行不含 helperFlag，sweepOrphans 不清杀它。cmd.Wait 须返回
// 错误，证明子进程是被杀而非自行退出。
func TestRunPanicTearsDownHelper(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "AUDIOCAP_TEST_HELPER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	bindToTest(t, cmd)
	exited := make(chan struct{})
	var waitErr error // close(exited) 之前写入，关闭后读取安全
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()

	m := New(panicSink{}, nil, nil, 25, os.Args[0])
	t.Cleanup(m.Stop) // 结束 idleKeepalive
	m.helper = &helperProc{cmd: cmd, stdin: stdin, exited: exited, start: time.Now()}
	m.Wake() // 第一轮 reconcile 立即执行

	recovered := make(chan any, 1)
	go func() {
		defer func() { recovered <- recover() }()
		m.Run()
	}()
	select {
	case r := <-recovered:
		if r != sinkPanicMsg {
			t.Fatalf("Run 应因 sink 注入的 panic 返回, recover 得到 %v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run 10s 内未 panic 返回")
	}

	if !isClosed(exited) {
		t.Fatal("Run panic 返回时 helper 子进程仍未被回收")
	}
	if waitErr == nil {
		t.Fatal("子进程以 0 退出，不是被 Kill 终止（-test.run 失配？）")
	}
	if m.helper != nil {
		t.Fatal("Run panic 返回时 m.helper 应为 nil")
	}
	if isClosed(m.stopCh) {
		t.Fatal("Run panic 展开不应关闭 stopCh")
	}
}

// TestCheckWatchdogSingleCapture 断言单路捕获失败（另一路本次下发后仍出真帧）只停该路：下发
// stop、清除 dispatched / dispatchedAt / lastFrame、记账 1 次，helper 不动；另一路已跑稳则清账。
//
// kugou 预置 3 次旧失败，本次捕获下发 12s 后失败（跑过窗口）→ 次数应重置为 1。若先删 dispatchedAt
// 再记账，下发时刻缺失按不足窗口加 1，次数为 4。
func TestCheckWatchdogSingleCapture(t *testing.T) {
	w := &recWriter{}
	m := newApplyManager(w, NormParams{})
	now := time.Now()
	m.dispatched = map[string]uint32{"kugou": 100, "qqmusic": 200}
	m.dispatchedAt = map[string]time.Time{"kugou": now.Add(-12 * time.Second), "qqmusic": now.Add(-quickFailWindow - time.Second)}
	m.lastFrame = map[string]time.Time{"kugou": now.Add(-3 * time.Second), "qqmusic": now.Add(-100 * time.Millisecond)}
	m.captureFails = map[string]captureFailure{"kugou": {count: 3, pid: 100}, "qqmusic": {count: 2, pid: 200}}

	m.checkWatchdog(map[string]uint32{"kugou": 100, "qqmusic": 200})

	if got, want := w.lines, []string{"stop kugou"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("下发 = %v, 期望 %v", got, want)
	}
	if _, ok := m.dispatched["kugou"]; ok {
		t.Fatalf("dispatched 仍含 kugou")
	}
	if _, ok := m.dispatchedAt["kugou"]; ok {
		t.Fatalf("dispatchedAt 仍含 kugou")
	}
	if _, ok := m.lastFrame["kugou"]; ok {
		t.Fatalf("lastFrame 仍含 kugou")
	}
	if _, ok := m.dispatched["qqmusic"]; !ok {
		t.Fatalf("qqmusic 不应受影响")
	}
	if f := m.captureFails["kugou"]; f.count != 1 || f.pid != 100 {
		t.Fatalf("kugou 记账 = %+v, 期望 count=1 pid=100", f)
	}
	if _, ok := m.captureFails["qqmusic"]; ok {
		t.Fatalf("qqmusic 已跑稳，记账应清除")
	}
	if w.closed || m.helper == nil || m.helper.watchdogKill {
		t.Fatalf("helper 不应被杀: stdin closed=%v helper=%v", w.closed, m.helper)
	}
}

// TestCheckWatchdogKeepsUnstableFailures 断言未跑稳（最后收帧距本次下发不足 quickFailWindow）的 player
// 即便正在出帧、未超阈值，其失败记账也原样保留，不下发任何控制。
func TestCheckWatchdogKeepsUnstableFailures(t *testing.T) {
	w := &recWriter{}
	m := newApplyManager(w, NormParams{})
	now := time.Now()
	f := captureFailure{count: 2, retryAt: now.Add(-time.Second), pid: 100}
	m.dispatched = map[string]uint32{"kugou": 100}
	m.dispatchedAt = map[string]time.Time{"kugou": now.Add(-3 * time.Second)}
	m.lastFrame = map[string]time.Time{"kugou": now.Add(-100 * time.Millisecond)}
	m.captureFails = map[string]captureFailure{"kugou": f}

	m.checkWatchdog(map[string]uint32{"kugou": 100})

	if got, ok := m.captureFails["kugou"]; !ok || !reflect.DeepEqual(got, f) {
		t.Fatalf("kugou 记账 = %+v (存在=%v), 期望原样保留 %+v", got, ok, f)
	}
	if len(w.lines) != 0 {
		t.Fatalf("不应下发控制, 实为 %v", w.lines)
	}
}

// stubSink 只对 player 报有 per-player 订阅：AnySubscribers 为真，无 root 订阅，无活跃播放器。
type stubSink struct{ player string }

func (stubSink) BroadcastAudioFrame(string, []byte) {}
func (s stubSink) HasSubscribers(n string) bool     { return n == s.player }
func (stubSink) HasRootSubscriber() bool            { return false }
func (stubSink) AnySubscribers() bool               { return true }
func (stubSink) ActivePlayer() string               { return "" }

// TestReconcileFilterWant 断言 reconcile 下发前经 filterWant 过滤 want：已放弃、退避期内的不下发
// capture；退避期满且 PID 未变时下发一次并保留记账；PID 与记账不同时清账并下发一次。用例把 sodamusic
// 的进程名临时改为本测试进程独有的名字，把测试二进制复制为该名字并以 TestHelperProcess 长睡启动，
// 使其进入真实进程快照，又不与真实播放器进程同名。
func TestReconcileFilterWant(t *testing.T) {
	const player = "sodamusic"
	orig := procscan.PlayerProcess[player]
	proc := fmt.Sprintf("audiocap-test-%d.exe", os.Getpid())
	procscan.PlayerProcess[player] = proc
	// 先于 TempDir 与子进程的清理注册，故最后执行。
	t.Cleanup(func() { procscan.PlayerProcess[player] = orig })

	exe := filepath.Join(t.TempDir(), proc)
	bin, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatalf("读测试二进制: %v", err)
	}
	if err := os.WriteFile(exe, bin, 0o755); err != nil {
		t.Fatalf("复制测试二进制: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "AUDIOCAP_TEST_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程: %v", err)
	}
	// 晚于 t.TempDir 注册，故先于其清理执行：进程未退出时 exe 删不掉。
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	bindToTest(t, cmd)
	pid := uint32(cmd.Process.Pid)

	w := &recWriter{}
	m := newApplyManager(w, NormParams{})
	m.sink = stubSink{player}
	m.status = func(string) string { return "playing" }
	m.helper = &helperProc{stdin: w, exited: make(chan struct{}), start: time.Now()}

	for _, c := range []struct {
		name      string
		fail      captureFailure
		wantLines []string
		wantKept  bool
	}{
		{"已放弃", captureFailure{count: maxQuickFails, givenUp: true, pid: pid}, nil, true},
		{"退避期内", captureFailure{count: 1, retryAt: time.Now().Add(time.Hour), pid: pid}, nil, true},
		{"退避期满", captureFailure{count: 1, retryAt: time.Now().Add(-time.Second), pid: pid}, []string{fmt.Sprintf("capture %d %s 0 0 0 50", pid, player)}, true},
		{"PID 变化", captureFailure{count: maxQuickFails, givenUp: true, pid: pid + 1}, []string{fmt.Sprintf("capture %d %s 0 0 0 50", pid, player)}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.lines = nil
			m.dispatched = map[string]uint32{}
			m.dispatchedAt = map[string]time.Time{}
			m.captureFails = map[string]captureFailure{player: c.fail}

			m.reconcile()

			if !reflect.DeepEqual(w.lines, c.wantLines) {
				t.Fatalf("下发 = %v, 期望 %v", w.lines, c.wantLines)
			}
			if _, ok := m.captureFails[player]; ok != c.wantKept {
				t.Fatalf("记账存在 = %v, 期望 %v", ok, c.wantKept)
			}
		})
	}
}

// TestSpawnHelperResetsDispatchState 断言 spawnHelper 重建 dispatched 与 dispatchedAt（reap / teardown
// 将二者置 nil）。m.exe 为测试二进制，子进程不认 helperFlag 即退出，不影响本断言。
func TestSpawnHelperResetsDispatchState(t *testing.T) {
	m := &Manager{exe: os.Args[0]}
	if err := m.spawnHelper(); err != nil {
		t.Fatalf("spawnHelper: %v", err)
	}
	defer m.teardownHelper()

	if m.helper == nil || m.dispatched == nil || m.dispatchedAt == nil {
		t.Fatalf("spawn 后 helper=%v dispatched=%v dispatchedAt=%v, 均应非 nil", m.helper, m.dispatched, m.dispatchedAt)
	}
}

// TestCheckWatchdogKillHelper 端到端断言 helper 假死路径：唯一一路持续无帧 → 记账 1 次、不下发 stop、
// 关 stdin、关 job 并置 0；随后 reap 不计 helper 快速失败。
func TestCheckWatchdogKillHelper(t *testing.T) {
	job, err := createKillOnCloseJob()
	if err != nil {
		t.Fatalf("createKillOnCloseJob: %v", err)
	}
	w := &recWriter{}
	m := newApplyManager(w, NormParams{})
	m.helper = &helperProc{stdin: w, job: job, exited: make(chan struct{}), start: time.Now()}
	stuckAt := time.Now().Add(-hangThreshold - time.Second)
	m.dispatched = map[string]uint32{"kugou": 100}
	m.dispatchedAt = map[string]time.Time{"kugou": stuckAt}
	m.lastFrame = map[string]time.Time{"kugou": stuckAt}

	m.checkWatchdog(map[string]uint32{"kugou": 100})

	if len(w.lines) != 0 {
		t.Fatalf("kill 路径不应下发任何控制（含 stop）, 实为 %v", w.lines)
	}
	if !w.closed {
		t.Fatalf("helper stdin 应已关闭")
	}
	if m.helper.job != 0 {
		t.Fatalf("job 应已关闭并置 0, 实为 %v", m.helper.job)
	}
	if f := m.captureFails["kugou"]; f.count != 1 {
		t.Fatalf("kugou 记账次数 = %d, 期望 1", f.count)
	}

	m.helper.exited = closedChan()
	m.reapHelper()
	if m.quickFails != 0 {
		t.Fatalf("看门狗所杀的 helper 被 reap 后 quickFails = %d, 期望 0", m.quickFails)
	}
}

// recWriter 是记录写入行的假 helper stdin（不起真进程）。sendControl 每条控制一次 WriteString
// （含结尾 "\n"），故一次 Write = 一行。
type recWriter struct {
	lines  []string
	closed bool
}

func (w *recWriter) Write(p []byte) (int, error) {
	line := string(p)
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	w.lines = append(w.lines, line)
	return len(p), nil
}

func (w *recWriter) Close() error {
	w.closed = true
	return nil
}

// newApplyManager 构造一个带固定 normResolver 的 Manager（接 recWriter 假 stdin），
// resolver 对所有 player 返回同一 NormParams，便于断言 capture 命令的各字段。帧率取 50 而非默认的 25：
// applyControls 把帧率字段写死为 25 时，TestApplyControls 等用例的期望行即不符（隔离副本，实测）。
func newApplyManager(w *recWriter, np NormParams) *Manager {
	m := &Manager{
		normResolver: func(string) NormParams { return np },
		frameRate:    50,
		dispatched:   map[string]uint32{},
		dispatchedAt: map[string]time.Time{},
		captureFails: map[string]captureFailure{},
		lastFrame:    map[string]time.Time{},
		lastForward:  map[string]time.Time{},
	}
	m.helper = &helperProc{stdin: w}
	return m
}

// TestApplyControls 验证差异控制：新增→capture；PID 变更→先 stop 旧再 capture 新；
// 移除→stop 且 dispatched 与 lastFrame 均被清。capture 命令为 7 字段（归一化参数与帧率），
// 由 normResolver 的 Enabled 决定 bit 位（1/0）。
func TestApplyControls(t *testing.T) {
	w := &recWriter{}
	m := newApplyManager(w, NormParams{Enabled: true, Target: -14, MaxGain: 30})

	// 1. 新增 kugou→100：只下发 capture，含归一化参数（enabled→bit=1）与帧率。
	m.applyControls(map[string]uint32{"kugou": 100})
	if got, want := w.lines, []string{"capture 100 kugou 1 -14 30 50"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("新增后下发 = %v, 期望 %v", got, want)
	}
	if m.dispatched["kugou"] != 100 {
		t.Fatalf("新增后 dispatched[kugou] = %d, 期望 100", m.dispatched["kugou"])
	}
	if _, ok := m.lastFrame["kugou"]; !ok {
		t.Fatalf("新增后应置 lastFrame 基线")
	}
	// 基线须与下发时刻相等，看门狗才能区分基线与真帧。
	if !m.dispatchedAt["kugou"].Equal(m.lastFrame["kugou"]) {
		t.Fatalf("新增后 dispatchedAt[kugou] = %v, 期望等于 lastFrame 基线 %v", m.dispatchedAt["kugou"], m.lastFrame["kugou"])
	}
	// 基线不是转发：下发不写 lastForward，捕获启动中照常推空闲帧。
	if _, ok := m.lastForward["kugou"]; ok {
		t.Fatalf("新增后不应写 lastForward[kugou]")
	}

	// 2. PID 变更 kugou→200：先 stop 旧再 capture 新（仍带全部参数）。
	w.lines = nil
	m.applyControls(map[string]uint32{"kugou": 200})
	if got, want := w.lines, []string{"stop kugou", "capture 200 kugou 1 -14 30 50"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("PID 变更后下发 = %v, 期望 %v", got, want)
	}
	if m.dispatched["kugou"] != 200 {
		t.Fatalf("PID 变更后 dispatched[kugou] = %d, 期望 200", m.dispatched["kugou"])
	}

	// 3. 移除（want 空）：下发 stop，dispatched 与 lastFrame 均清空。
	w.lines = nil
	m.applyControls(map[string]uint32{})
	if got, want := w.lines, []string{"stop kugou"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("移除后下发 = %v, 期望 %v", got, want)
	}
	if len(m.dispatched) != 0 {
		t.Fatalf("移除后 dispatched 应清空, 实为 %v", m.dispatched)
	}
	if len(m.dispatchedAt) != 0 {
		t.Fatalf("移除后 dispatchedAt 应清空, 实为 %v", m.dispatchedAt)
	}
	if len(m.lastFrame) != 0 {
		t.Fatalf("移除后 lastFrame 应清空, 实为 %v", m.lastFrame)
	}
}

// TestApplyControlsNormDisabled 断言 normResolver 返回 Enabled=false 时 capture 的 bit 位为 0。
func TestApplyControlsNormDisabled(t *testing.T) {
	w := &recWriter{}
	m := newApplyManager(w, NormParams{Enabled: false, Target: -14, MaxGain: 30})

	m.applyControls(map[string]uint32{"kugou": 100})
	if got, want := w.lines, []string{"capture 100 kugou 0 -14 30 50"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("禁用归一化后下发 = %v, 期望 %v", got, want)
	}
}

// idleSink 无活跃播放器；root 为真时报告有 root 订阅，HasSubscribers 只对 subs 中的播放器为真。
// BroadcastAudioFrame 非阻塞写入带缓冲的 ch：Stop 之后多余的调用既不阻塞，也不与测试读端竞争。
type idleSink struct {
	ch   chan []byte
	root bool
	subs map[string]bool
}

func (s idleSink) BroadcastAudioFrame(_ string, b []byte) {
	select {
	case s.ch <- b:
	default:
	}
}
func (s idleSink) HasSubscribers(n string) bool { return s.subs[n] }
func (s idleSink) HasRootSubscriber() bool      { return s.root }
func (idleSink) AnySubscribers() bool           { return false }
func (idleSink) ActivePlayer() string           { return "" }

// qpcMillisNow 读一次系统 QPC 并换算为毫秒，与 idleKeepalive 的 TSMillis 同源。
func qpcMillisNow() float64 {
	var freq int64
	procQPFreq.Call(uintptr(unsafe.Pointer(&freq)))
	runtime.KeepAlive(&freq)
	return frame.QPCMillis(readQPC(), freq)
}

// decodeFrame 解出 b 中的一帧，失败即判测试失败。
func decodeFrame(t *testing.T, b []byte) frame.Frame {
	t.Helper()
	var dec frame.Decoder
	dec.Push(b)
	f, ok, err := dec.Next()
	if err != nil || !ok {
		t.Fatalf("解空闲帧失败: ok=%v err=%v", ok, err)
	}
	return f
}

// recvIdleFrame 起 idleKeepalive，取它推出的第一帧后 Stop，并断言该帧为 v1 布局、player 为 player、
// flags 为 0、无 bins、rms / peak / epoch 为 0，且 TSMillis 与真帧同为 QPC 毫秒：夹在推帧前后两次
// QPC 读数之间。
func recvIdleFrame(t *testing.T, sink idleSink, player string) {
	t.Helper()
	m := New(sink, nil, nil, 25, "")
	t.Cleanup(m.Stop)
	before := qpcMillisNow()
	go m.idleKeepalive()

	var b []byte
	select {
	case b = <-sink.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("2s 内未收到空闲帧")
	}
	after := qpcMillisNow()

	f := decodeFrame(t, b)
	if f.Player != player || f.Flags != 0 || len(f.Bins) != 0 {
		t.Fatalf("空闲帧 player=%q flags=%d bins=%d, 期望 %q、0、0", f.Player, f.Flags, len(f.Bins), player)
	}
	if f.RMS != 0 || f.PeakL != 0 || f.PeakR != 0 || f.Epoch != 0 {
		t.Fatalf("空闲帧 rms=%v peakL=%v peakR=%v epoch=%d, 期望全为 0", f.RMS, f.PeakL, f.PeakR, f.Epoch)
	}
	if !(before <= f.TSMillis && f.TSMillis <= after) {
		t.Fatalf("空闲帧 TSMillis=%v 不在 QPC 毫秒区间 [%v, %v]", f.TSMillis, before, after)
	}
	if v, h := binary.LittleEndian.Uint32(b[8:]), binary.LittleEndian.Uint32(b[12:]); v != 1 || h != 48 {
		t.Fatalf("空闲帧 version=%d headerLen=%d, 期望 1、48", v, h)
	}
}

// TestIdleKeepaliveFrameV1 断言 root 有订阅、无活跃播放器时推 player="" 的空闲帧（内容见 recvIdleFrame）。
func TestIdleKeepaliveFrameV1(t *testing.T) {
	recvIdleFrame(t, idleSink{ch: make(chan []byte, 1), root: true}, "")
}

// TestIdleKeepalivePlayerFrame 断言某播放器有订阅、无 root 订阅、从未转发过真帧时，推 player 为该
// 播放器名的空闲帧，内容与 root 空闲帧相同（见 recvIdleFrame）。
func TestIdleKeepalivePlayerFrame(t *testing.T) {
	recvIdleFrame(t, idleSink{ch: make(chan []byte, 1), subs: map[string]bool{"kugou": true}}, "kugou")
}

// TestIdleKeepaliveSkipsForwarded 断言最近转发过真帧的播放器不推空闲帧：kugou 与 sodamusic 均有订阅，
// kugou 在起跑前刚转发过（直接写 lastForward），sodamusic 从未转发。收到 sodamusic 的空闲帧即证明至少
// 跑过一拍；随后 Stop 并等 idleKeepalive 返回，已推出的帧中不得有 kugou。
func TestIdleKeepaliveSkipsForwarded(t *testing.T) {
	sink := idleSink{ch: make(chan []byte, 64), subs: map[string]bool{"kugou": true, "sodamusic": true}}
	m := New(sink, nil, nil, 25, "")
	t.Cleanup(m.Stop)
	m.lastForward["kugou"] = time.Now().Add(time.Hour) // 取未来时刻：首拍被调度延迟超过 noCaptureAfter 也不会误推
	done := make(chan struct{})
	go func() {
		m.idleKeepalive()
		close(done)
	}()

	var players []string
	timeout := time.After(2 * time.Second)
	for len(players) == 0 || players[len(players)-1] != "sodamusic" {
		select {
		case b := <-sink.ch:
			players = append(players, decodeFrame(t, b).Player)
		case <-timeout:
			t.Fatalf("2s 内未收到 sodamusic 的空闲帧, 已收 %v", players)
		}
	}
	m.Stop()
	<-done
	for len(sink.ch) > 0 {
		players = append(players, decodeFrame(t, <-sink.ch).Player)
	}
	for _, p := range players {
		if p == "kugou" {
			t.Fatalf("kugou 刚转发过真帧却收到其空闲帧, 已收 %v", players)
		}
	}
}

// TestDemuxRecordsForward 断言 demux 解出并转发一帧时写入该 player 的 lastForward（idleKeepalive 据此
// 判无捕获），并把帧交给 sink。cmd 未启动，结尾的 cmd.Wait 只返回错误。
func TestDemuxRecordsForward(t *testing.T) {
	sink := idleSink{ch: make(chan []byte, 1)}
	m := New(sink, nil, nil, 25, "")
	hp := &helperProc{cmd: &exec.Cmd{}, exited: make(chan struct{})}

	before := time.Now()
	m.demux(hp, bytes.NewReader(frame.Encode(nil, frame.Frame{Player: "kugou", Epoch: 1})))
	after := time.Now()

	if got, ok := m.lastForward["kugou"]; !ok || got.Before(before) || got.After(after) {
		t.Fatalf("lastForward[kugou] = %v (存在=%v), 期望在 [%v, %v] 内", got, ok, before, after)
	}
	if len(sink.ch) != 1 {
		t.Fatalf("sink 收到 %d 帧, 期望 1", len(sink.ch))
	}
}

// slowSink 的 BroadcastAudioFrame 先拖延 delay，记下即将返回的时刻，再交给 idleSink，模拟广播本身拖延。
type slowSink struct {
	idleSink
	delay    time.Duration
	returned *time.Time
}

func (s slowSink) BroadcastAudioFrame(p string, b []byte) {
	time.Sleep(s.delay)
	*s.returned = time.Now()
	s.idleSink.BroadcastAudioFrame(p, b)
}

// TestDemuxForwardTimeAfterBroadcast 断言 lastForward 取广播返回之后的时刻：广播拖延时若取广播前的时刻，
// 释放 mu 后 idleKeepalive 可能按旧时刻判超时，在刚送达的真帧之后补一帧空闲帧。
func TestDemuxForwardTimeAfterBroadcast(t *testing.T) {
	var returned time.Time
	sink := slowSink{idleSink: idleSink{ch: make(chan []byte, 1)}, delay: 50 * time.Millisecond, returned: &returned}
	m := New(sink, nil, nil, 25, "")
	hp := &helperProc{cmd: &exec.Cmd{}, exited: make(chan struct{})}

	m.demux(hp, bytes.NewReader(frame.Encode(nil, frame.Frame{Player: "kugou", Epoch: 1})))

	if got, ok := m.lastForward["kugou"]; !ok || got.Before(returned) {
		t.Fatalf("lastForward[kugou] = %v (存在=%v), 早于广播返回时刻 %v", got, ok, returned)
	}
}
