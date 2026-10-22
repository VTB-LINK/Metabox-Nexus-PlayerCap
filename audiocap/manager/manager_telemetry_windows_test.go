//go:build windows

package manager

// issue #120 的接线用例（Windows-only）：stderr 读泵不卡监管路径、存活时长记账不漂移、退出分类的时序、
// 关停时丢弃、上报的 payload 与次数，以及 Ctrl+Break / Ctrl+C 下没有 signal.Notify 的子进程的退出码。
//
// 子进程一律是本测试二进制重入自身：TestMain 见到 fakeHelperEnv 即转入 fakeHelperMain，不跑任何用例。
// spawnHelper 以 m.exe = os.Args[0] 拉起的「helper」也是它，行为由继承下去的环境变量决定。

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// fakeHelperEnv 非空时，测试二进制作为假 helper 运行，取值即角色，见 fakeHelperMain。
const fakeHelperEnv = "AUDIOCAP_FAKE_HELPER"

// fakeHelperSelfTimeout 是假 helper 等外部事件的上限：用例失败时自行退出，不留残留进程。
const fakeHelperSelfTimeout = 30 * time.Second

// testReportDelay 替换 3 秒的上报延后，使关停窗口用例跑得快。
const testReportDelay = 150 * time.Millisecond

func TestMain(m *testing.M) {
	if role := os.Getenv(fakeHelperEnv); role != "" {
		fakeHelperMain(role)
	}
	os.Exit(m.Run())
}

var procSetConsoleCtrlHandler = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")

// fakeHelperMain 是假 helper 的入口，不返回。
//
//	exit   往 stderr 写 AUDIOCAP_FAKE_STDERR1，停 AUDIOCAP_FAKE_PAUSE_MS 毫秒，再写 AUDIOCAP_FAKE_STDERR2
//	       （AUDIOCAP_FAKE_STDERR_BYTES 非零时改写这么多字节），以 AUDIOCAP_FAKE_CODE 退出
//	sleep  不读 stdin、长睡，模拟要靠强杀才会结束的 helper
//	ready  往 stdout 打印 ready 后长睡；没有 signal.Notify，控制台事件按系统默认处理
//	ctrl   在自己的（隐藏的）控制台里启动一个 ready 子进程，向它发 AUDIOCAP_FAKE_EVENT（c 或 break），
//	       打印它的退出码；自己接住中断，不被同一事件结束
func fakeHelperMain(role string) {
	switch role {
	case "exit":
		os.Stderr.WriteString(os.Getenv("AUDIOCAP_FAKE_STDERR1"))
		if ms, _ := strconv.Atoi(os.Getenv("AUDIOCAP_FAKE_PAUSE_MS")); ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		if n, _ := strconv.Atoi(os.Getenv("AUDIOCAP_FAKE_STDERR_BYTES")); n > 0 {
			os.Stderr.Write(bytes.Repeat([]byte("0123456789abcde\n"), n/16))
		} else {
			os.Stderr.WriteString(os.Getenv("AUDIOCAP_FAKE_STDERR2"))
		}
		code, _ := strconv.ParseUint(os.Getenv("AUDIOCAP_FAKE_CODE"), 0, 32)
		os.Exit(int(code))
	case "sleep":
		time.Sleep(fakeHelperSelfTimeout)
		os.Exit(97)
	case "ready":
		fmt.Println("ready")
		time.Sleep(fakeHelperSelfTimeout)
		os.Exit(97)
	case "ctrl":
		fakeCtrlRelay(os.Getenv("AUDIOCAP_FAKE_EVENT"))
	}
	os.Exit(96)
}

// fakeCtrlRelay 见 fakeHelperMain 的 ctrl。CTRL_C_EVENT 只能发给整个控制台（进程组参数为 0），所以本进程须在
// 自己独占的控制台里（测试以 CREATE_NEW_CONSOLE 启动它），不波及测试运行器。
func fakeCtrlRelay(event string) {
	// 进程是否处理 Ctrl+C 随父进程继承；显式恢复处理，使子进程与真实 helper 一样按系统默认处理 Ctrl+C。
	procSetConsoleCtrlHandler.Call(0, 0)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), fakeHelperEnv+"=ready")
	if event == "break" {
		child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	}
	out, err := child.StdoutPipe()
	if err == nil {
		err = child.Start()
	}
	if err != nil {
		fmt.Println("start-failed:", err)
		os.Exit(95)
	}
	if line, _ := bufio.NewReader(out).ReadString('\n'); strings.TrimSpace(line) != "ready" {
		fmt.Printf("not-ready: %q\n", line)
		child.Process.Kill()
		os.Exit(94)
	}
	if event == "break" {
		err = windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(child.Process.Pid))
	} else {
		err = windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0)
	}
	if err != nil {
		fmt.Println("event-failed:", err)
		child.Process.Kill()
		os.Exit(93)
	}
	done := make(chan struct{})
	go func() { child.Wait(); close(done) }()
	select {
	case <-done:
		fmt.Printf("code=0x%X\n", uint32(child.ProcessState.ExitCode()))
		os.Exit(0)
	case <-time.After(10 * time.Second):
		fmt.Println("child-survived")
		child.Process.Kill()
		os.Exit(92)
	}
}

// TestNoNotifyChildExitCodeOnCtrlEvents 用真子进程实测设计 §3.2.2 的论断：没有 signal.Notify 的进程收到
// Ctrl+C 或 Ctrl+Break 时被系统默认处理结束，退出码为 STATUS_CONTROL_C_EXIT（statusControlCExit）。
// manager 据此把该退出码一律判为随主服务关停、不报。中转进程在隐藏的新控制台里发事件，不波及测试运行器。
func TestNoNotifyChildExitCodeOnCtrlEvents(t *testing.T) {
	for _, event := range []string{"c", "break"} {
		t.Run(event, func(t *testing.T) {
			relay := exec.Command(os.Args[0])
			relay.Env = append(os.Environ(), fakeHelperEnv+"=ctrl", "AUDIOCAP_FAKE_EVENT="+event)
			relay.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE, HideWindow: true}
			var out bytes.Buffer
			relay.Stdout = &out
			if err := relay.Start(); err != nil {
				t.Fatalf("启动中转进程: %v", err)
			}
			bindToTest(t, relay)
			waited := make(chan error, 1)
			go func() { waited <- relay.Wait() }()
			select {
			case err := <-waited:
				if err != nil {
					t.Fatalf("中转进程失败: %v，输出 %q", err, out.String())
				}
			case <-time.After(20 * time.Second):
				relay.Process.Kill()
				t.Fatalf("中转进程 20s 内未结束，输出 %q", out.String())
			}
			if got, want := strings.TrimSpace(out.String()), fmt.Sprintf("code=0x%X", statusControlCExit); got != want {
				t.Fatalf("子进程收到事件后的结果 %q，期望 %q", got, want)
			}
		})
	}
}

// reportSink 是假的 Manager.report，记下收到的事件（定时器 goroutine 调用，带锁）。
type reportSink struct {
	mu  sync.Mutex
	evs []TelemetryEvent
}

func (s *reportSink) add(ev TelemetryEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
}

func (s *reportSink) byKey(key string) []TelemetryEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []TelemetryEvent
	for _, ev := range s.evs {
		if ev.Key == key {
			out = append(out, ev)
		}
	}
	return out
}

func (s *reportSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.evs)
}

// settleReports 等过上报延后与余量，此后 reportSink 里就是全部会到的事件。
func settleReports() { time.Sleep(testReportDelay + 400*time.Millisecond) }

// newTelemetryManager 经 New 构造（与生产同一组默认值），换上假 report、缩短的延后与给定的 stderr 转发目标。
// 结束时拆掉仍在的 helper。
func newTelemetryManager(t *testing.T, out io.Writer) (*Manager, *reportSink) {
	t.Helper()
	sink := &reportSink{}
	m := New(stubSink{"kugou"}, nil, nil, 25, os.Args[0])
	m.report = sink.add
	m.reportDelay = testReportDelay
	m.stderrOut = out
	t.Cleanup(m.teardownHelper)
	return m, sink
}

// useFakeHelper 让此后 spawnHelper 拉起的子进程以 role 运行，kv 为成对的环境变量名与值。
func useFakeHelper(t *testing.T, role string, kv ...string) {
	t.Helper()
	t.Setenv(fakeHelperEnv, role)
	for i := 0; i+1 < len(kv); i += 2 {
		t.Setenv(kv[i], kv[i+1])
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, d time.Duration, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("%v 内未关闭：%s", d, msg)
	}
}

// waitProcessGone 等 hp 的进程真的结束（不经 cmd.Wait，不碰 demux）。
func waitProcessGone(t *testing.T, hp *helperProc, d time.Duration) {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(hp.cmd.Process.Pid))
	if err != nil {
		t.Fatalf("取子进程句柄: %v", err)
	}
	defer windows.CloseHandle(h)
	if ev, err := windows.WaitForSingleObject(h, uint32(d.Milliseconds())); ev != windows.WAIT_OBJECT_0 {
		t.Fatalf("子进程 %v 内未结束: %v %v", d, ev, err)
	}
}

// releaseOnce 关闭 barrierWriter 的 release，可重复调用。
func releaseOnce(w *barrierWriter) func() {
	var once sync.Once
	return func() { once.Do(func() { close(w.release) }) }
}

// TestSupervisionNotBlockedByStuckStderr 断言 stderr 转发卡住（控制台卡住）时监管路径不受影响：helper 退出后
// exited 照常关闭，reapHelper 立即返回，重新拉起的 helper 照常工作；放开之后读泵读完剩余字节（含 cmd.Wait 返回时
// 还留在管道里的）并退出，不泄漏 goroutine。
//
// 变异自证（实测）：demux 在 Wait 之后等读泵结束、或 cmd.Stderr 改回非 *os.File 的写入器（os/exec 起拷贝
// goroutine，Wait 等它）→ exited 不关闭而红；读泵不写转发目标 → 转发目标始终没有收到第一块而红；读泵等 exited
// 关闭才起 → 两块被一次读走、尾部不止第一块而红。
func TestSupervisionNotBlockedByStuckStderr(t *testing.T) {
	a, b := strings.Repeat("A", 1000)+"\n", strings.Repeat("B", 1000)+"\n"
	useFakeHelper(t, "exit", "AUDIOCAP_FAKE_STDERR1", a, "AUDIOCAP_FAKE_PAUSE_MS", "500", "AUDIOCAP_FAKE_STDERR2", b, "AUDIOCAP_FAKE_CODE", "3")
	out := newBarrierWriter()
	release := releaseOnce(out)
	m, _ := newTelemetryManager(t, out)
	t.Cleanup(release) // 晚于 newTelemetryManager 注册，故先于其中的 teardownHelper 执行
	m.report = nil     // 不起上报定时器，下面按 goroutine 数判泄漏
	base := runtime.NumGoroutine()

	if err := m.spawnHelper(); err != nil {
		t.Fatalf("spawnHelper: %v", err)
	}
	hp := m.helper
	waitClosed(t, out.entered, 5*time.Second, "读泵没有把第一块写往转发目标")
	waitClosed(t, hp.exited, 5*time.Second, "helper 退出后 exited 没有关闭（demux 在等 stderr 读泵？）")
	start := time.Now()
	m.reapHelper()
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("reapHelper 用了 %v，期望立即返回", d)
	}
	if got := hp.stderrTail.Snapshot(); got != a {
		t.Fatalf("转发卡住时尾部为 %d 字节，期望只有第一块（第二块还在管道里）", len(got))
	}

	// 重新订阅：新 helper 照常拉起、退出、被回收，不受卡住的旧读泵影响。
	if err := m.spawnHelper(); err != nil {
		t.Fatalf("第二次 spawnHelper: %v", err)
	}
	hp2 := m.helper
	waitClosed(t, hp2.exited, 5*time.Second, "第二个 helper 退出后 exited 没有关闭")
	m.reapHelper()

	release()
	deadline := time.Now().Add(5 * time.Second)
	for len(out.bytes()) < 2*len(a+b) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := out.bytes()
	if len(got) != 2*len(a+b) || bytes.Count(got, []byte(a)) != 2 || bytes.Count(got, []byte(b)) != 2 {
		t.Fatalf("放开后转发目标收到 %d 字节，期望两个 helper 各 %d 字节、一字不差", len(got), len(a+b))
	}
	for i, h := range []*helperProc{hp, hp2} {
		if s := h.stderrTail.Snapshot(); s != a+b {
			t.Fatalf("第 %d 个 helper 的尾部 %d 字节，期望 %d", i+1, len(s), len(a+b))
		}
	}
	for time.Now().Before(deadline) && runtime.NumGoroutine() > base {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Fatalf("放开后 goroutine %d 个，多于拉起前的 %d 个：读泵或 demux 没有退出", n, base)
	}
}

// TestLargeStderrDoesNotStallHelper 断言 helper 写出远超管道缓冲的 stderr 后照常退出、被回收，字节全部转发：
// 读泵从拉起起就在读，背压只在转发目标真的卡住时才落到 helper 身上。
//
// 变异自证（实测）：读泵改成等 exited 关闭（即 cmd.Wait 返回）之后才起 → helper 写满管道后卡住、exited 不关闭
// 而红；读泵不写转发目标 → 字节数不符而红。
func TestLargeStderrDoesNotStallHelper(t *testing.T) {
	const n = 256 << 10
	useFakeHelper(t, "exit", "AUDIOCAP_FAKE_STDERR_BYTES", strconv.Itoa(n), "AUDIOCAP_FAKE_CODE", "0")
	var mu sync.Mutex
	var buf bytes.Buffer
	m, _ := newTelemetryManager(t, writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	if err := m.spawnHelper(); err != nil {
		t.Fatalf("spawnHelper: %v", err)
	}
	hp := m.helper
	waitClosed(t, hp.exited, 10*time.Second, "写出大量 stderr 的 helper 没有退出（stderr 没人读？）")
	m.reapHelper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		l := buf.Len()
		mu.Unlock()
		if l >= n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if buf.Len() != n || !bytes.Equal(buf.Bytes(), bytes.Repeat([]byte("0123456789abcde\n"), n/16)) {
		t.Fatalf("转发目标收到 %d 字节，期望 %d 字节、内容一致", buf.Len(), n)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestAliveAccountingIgnoresStuckStderr 断言 stderr 转发卡住不会计进存活时长：预置快速失败 4 次，helper 存活约
// 8.8 秒（start 前移 8.5 秒）后非零退出，转发卡住 2.5 秒。exited 须在进程结束后 1 秒内关闭（否则这段时间里
// 看门狗会对已退出的进程追加失败）；回收后快速失败为 5 并放弃，拆除时计数原样保留，都不得因多算了等待的时间
// 而判为跑稳、清零。
//
// 变异自证（实测）：demux 在 Wait 之后等读泵结束、或 cmd.Stderr 改回非 *os.File 的写入器 → 两例均红（回收一例
// exited 迟关，拆除一例等到放开之后才返回、计数清零）。
func TestAliveAccountingIgnoresStuckStderr(t *testing.T) {
	for _, path := range []string{"reap", "teardown"} {
		t.Run(path, func(t *testing.T) {
			useFakeHelper(t, "exit", "AUDIOCAP_FAKE_STDERR1", "fatal error: boom\n", "AUDIOCAP_FAKE_PAUSE_MS", "300", "AUDIOCAP_FAKE_CODE", "2")
			out := newBarrierWriter()
			release := releaseOnce(out)
			m, _ := newTelemetryManager(t, out)
			t.Cleanup(release) // 先于 teardownHelper 执行
			m.quickFails = maxQuickFails - 1
			m.backoff = 4 * time.Second

			if err := m.spawnHelper(); err != nil {
				t.Fatalf("spawnHelper: %v", err)
			}
			hp := m.helper
			hp.start = time.Now().Add(-8500 * time.Millisecond)
			time.AfterFunc(2500*time.Millisecond, release)

			waitProcessGone(t, hp, 5*time.Second)
			if path == "reap" {
				waitClosed(t, hp.exited, time.Second, "进程已结束，exited 却没关（demux 在等卡住的 stderr 读泵）")
				m.reapHelper()
				if m.quickFails != maxQuickFails || !m.givenUp {
					t.Fatalf("回收后 quickFails=%d givenUp=%v，期望 %d、true（存活不足 %v，不得算作跑稳）", m.quickFails, m.givenUp, maxQuickFails, quickFailWindow)
				}
				return
			}
			m.teardownHelper()
			if m.quickFails != maxQuickFails-1 || m.backoff != 4*time.Second {
				t.Fatalf("拆除后 quickFails=%d backoff=%v，期望原样 %d、4s（存活不足 %v，不得清零）", m.quickFails, m.backoff, maxQuickFails-1, quickFailWindow)
			}
		})
	}
}

// TestExitedReportTiming 断言「自行退出」按进程真实的退出时刻与 manager 下手的时刻判定：先退出、后被看门狗或拆除
// 下手的照报；先下手、进程随后才死的不报；进程已死而 demux 尚未返回时下手的，按真实退出时刻判为自行退出。
//
// 变异自证（实测）：exit.at 改回 demux 观察到退出的时刻 →「demux 未返回时下手」红；去掉 killMarkedAt 比较 →
// 三例「先下手」红；看门狗或拆除处不调 markKilled → 对应的「先下手」红；markKilled 覆盖已记的时刻 →
// 「看门狗之后又拆除」红；reapHelper 或 teardownHelper 不调 settleExit、demux 不调 recordExit → 照报的各例红。
func TestExitedReportTiming(t *testing.T) {
	selfExit := func(t *testing.T) (*Manager, *reportSink) {
		useFakeHelper(t, "exit", "AUDIOCAP_FAKE_STDERR1", "boom\n", "AUDIOCAP_FAKE_CODE", "3")
		m, sink := newTelemetryManager(t, io.Discard)
		if err := m.spawnHelper(); err != nil {
			t.Fatalf("spawnHelper: %v", err)
		}
		waitClosed(t, m.helper.exited, 5*time.Second, "helper 未退出")
		return m, sink
	}
	longLived := func(t *testing.T) (*Manager, *reportSink) {
		useFakeHelper(t, "sleep")
		m, sink := newTelemetryManager(t, io.Discard)
		if err := m.spawnHelper(); err != nil {
			t.Fatalf("spawnHelper: %v", err)
		}
		time.Sleep(100 * time.Millisecond) // 确保进程已跑起来，下手在它的生命期内
		return m, sink
	}
	expect := func(t *testing.T, sink *reportSink, want int) {
		t.Helper()
		settleReports()
		evs := sink.byKey(keyHelperExited)
		if len(evs) != want {
			t.Fatalf("helper_exited_abnormally 报了 %d 次，期望 %d", len(evs), want)
		}
		if want == 1 && evs[0].Tags["exit_code"] != "0x3" {
			t.Fatalf("exit_code = %q，期望 0x3", evs[0].Tags["exit_code"])
		}
	}

	t.Run("自行退出后看门狗下手：照报", func(t *testing.T) {
		m, sink := selfExit(t)
		m.helper.watchdogKill = true
		m.killHelperForRestart()
		m.reapHelper()
		expect(t, sink, 1)
	})
	t.Run("自行退出后拆除：照报", func(t *testing.T) {
		m, sink := selfExit(t)
		m.teardownHelper()
		expect(t, sink, 1)
	})
	t.Run("看门狗先下手、进程随后才死：不报", func(t *testing.T) {
		m, sink := longLived(t)
		m.helper.watchdogKill = true
		m.killHelperForRestart()
		waitClosed(t, m.helper.exited, 5*time.Second, "被看门狗杀的 helper 未退出")
		m.reapHelper()
		expect(t, sink, 0)
	})
	t.Run("拆除先下手：不报", func(t *testing.T) {
		m, sink := longLived(t)
		m.teardownHelper()
		expect(t, sink, 0)
	})
	t.Run("看门狗之后又拆除：按最早的下手判定，不报", func(t *testing.T) {
		m, sink := longLived(t)
		m.helper.watchdogKill = true
		m.killHelperForRestart()
		waitProcessGone(t, m.helper, 5*time.Second)
		time.Sleep(20 * time.Millisecond)
		m.teardownHelper()
		expect(t, sink, 0)
	})
	t.Run("进程已死、demux 未返回时下手：判为自行退出", func(t *testing.T) {
		m, sink := newTelemetryManager(t, io.Discard)
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), fakeHelperEnv+"=exit", "AUDIOCAP_FAKE_CODE=3")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		bindToTest(t, cmd)
		hp := &helperProc{cmd: cmd, stdin: &recWriter{}, exited: make(chan struct{}), start: time.Now()}
		waitProcessGone(t, hp, 5*time.Second)
		time.Sleep(20 * time.Millisecond) // 下手的时刻确定晚于进程真实的退出时刻
		m.helper = hp
		m.killHelperForRestart() // job 为 0：对已死的进程 Process.Kill，错误忽略
		m.demux(hp, stdout)      // demux 此刻才观察到退出
		m.reapHelper()
		expect(t, sink, 1)
	})
}

// TestExitInfoOfUsesRealExitTime 断言退出信息取自 ProcessState：退出码按 uint32 原样（含 0xC0000005 这类高位
// 置位的码），时刻取进程真实的退出时刻而非传入的观察时刻；ProcessState 为 nil 时 known 为假。
//
// 变异自证（实测）：不取 SysUsage 的 ExitTime、一律用观察时刻，即红。
func TestExitInfoOfUsesRealExitTime(t *testing.T) {
	if e := exitInfoOf(nil, time.Now()); e.known {
		t.Fatalf("ProcessState 为 nil 时 known 应为假，得到 %+v", e)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), fakeHelperEnv+"=exit", "AUDIOCAP_FAKE_CODE=0xC0000005")
	before := time.Now().Round(0)
	if err := cmd.Run(); err == nil {
		t.Fatal("子进程以 0 退出（环境变量没传到？）")
	}
	after := time.Now().Round(0)
	observed := after.Add(time.Hour)
	e := exitInfoOf(cmd.ProcessState, observed)
	if !e.known || e.code != 0xC0000005 {
		t.Fatalf("exitInfoOf = %+v，期望 known、code 0xC0000005", e)
	}
	if e.at.Equal(observed) || e.at.Before(before) || e.at.After(after) {
		t.Fatalf("退出时刻 %v 不在 [%v, %v] 内（观察时刻 %v）：没取进程真实的退出时刻", e.at, before, after, observed)
	}
}

// triggerAllThree 经真实的触发点各触发一次三个事件（不起真进程）：一个自行退出的 helper 被回收、一个播放器捕获
// 放弃、spawn 失败累计放弃。
func triggerAllThree(m *Manager) {
	m.helper = &helperProc{exited: closedChan(), start: time.Now().Add(-time.Second), stderrTail: &tailBuffer{}}
	m.helper.exit = exitInfo{known: true, code: 3, at: time.Now()}
	m.reapHelper()

	m.dispatchedAt = map[string]time.Time{"kugou": time.Now()}
	m.captureFails["kugou"] = captureFailure{count: maxQuickFails - 1, pid: 1}
	m.noteCaptureFailure("kugou", 1, time.Now())

	m.quickFails, m.givenUp = maxQuickFails-1, false
	m.noteSpawnFailure(errors.New("spawn failed"))
}

// TestShutdownDropsReports 断言关停时三个事件都不报：BeginShutdown 或 Stop 发生在触发之前、或触发之后的延后窗口
// 之内，均丢弃；窗口之外已报出的不受影响。反例是不关停时三个都报。
//
// 变异自证（实测）：去掉 reportLater 里的关停检查、去掉延后（AfterFunc 改成 0）、BeginShutdown 不置位、Stop 不置位，
// 均有用例红。
func TestShutdownDropsReports(t *testing.T) {
	for _, c := range []struct {
		name string
		run  func(m *Manager)
		want int
	}{
		{"不关停：三个都报", func(m *Manager) { triggerAllThree(m); settleReports() }, 3},
		{"BeginShutdown 之后触发", func(m *Manager) { m.BeginShutdown(); triggerAllThree(m); settleReports() }, 0},
		{"触发后、延后窗口内 BeginShutdown", func(m *Manager) { triggerAllThree(m); m.BeginShutdown(); settleReports() }, 0},
		{"窗口之外 BeginShutdown：已报出的照旧", func(m *Manager) { triggerAllThree(m); settleReports(); m.BeginShutdown() }, 3},
		{"Stop 之后触发", func(m *Manager) { m.Stop(); triggerAllThree(m); settleReports() }, 0},
		{"触发后、延后窗口内 Stop", func(m *Manager) { triggerAllThree(m); m.Stop(); settleReports() }, 0},
		{"BeginShutdown 幂等", func(m *Manager) { m.BeginShutdown(); m.BeginShutdown(); triggerAllThree(m); settleReports() }, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, sink := newTelemetryManager(t, io.Discard)
			c.run(m)
			if got := sink.count(); got != c.want {
				t.Fatalf("报了 %d 个事件，期望 %d", got, c.want)
			}
		})
	}
}

// TestReportPayloadsAndCounts 经真实的触发点断言每个触发点恰好报一次、其余路径一次不报，以及各事件的 payload：
// 五次快速自行退出各报一次 helper_exited_abnormally，第五次另报一次 helper_gave_up（cause=quick_exit，带最后一个
// helper 的退出码与脱敏的尾部）；看门狗所杀不报；捕获失败到放弃那次才报；spawn 失败到放弃那次才报（带脱敏的
// spawn_error）。上报时取的是触发时的尾部快照。
//
// 变异自证（实测）：settleExit 里安排两次上报、reapHelper 调两次 settleExit →「五次快速退出」红；放弃分支不上报
// → 对应用例红；stderr_tail 改成安排时取快照 →「取触发时的快照」红；tail 不脱敏 → 尾部断言红。
func TestReportPayloadsAndCounts(t *testing.T) {
	const tailText = "panic: boom\n\tC:\\Users\\张三\\src\\x.go:12\n"
	const redacted = "panic: boom\n\tC:\\Users\\<user>\\src\\x.go:12\n"

	t.Run("五次快速自行退出", func(t *testing.T) {
		m, sink := newTelemetryManager(t, io.Discard)
		for i := 1; i <= maxQuickFails; i++ {
			tail := &tailBuffer{}
			tail.Write([]byte(tailText))
			m.helper = &helperProc{exited: closedChan(), start: time.Now().Add(-time.Second), stderrTail: tail}
			m.helper.exit = exitInfo{known: true, code: 2, at: time.Now()}
			m.reapHelper()
		}
		settleReports()
		exited := sink.byKey(keyHelperExited)
		if len(exited) != maxQuickFails {
			t.Fatalf("helper_exited_abnormally 报了 %d 次，期望 %d（每次自行退出一次）", len(exited), maxQuickFails)
		}
		seen := map[any]bool{}
		for _, ev := range exited {
			if ev.Tags["exit_code"] != "0x2" || ev.Extra["stderr_tail"] != redacted {
				t.Fatalf("事件 tags=%v stderr_tail=%q，期望 exit_code=0x2、脱敏后的尾部", ev.Tags, ev.Extra["stderr_tail"])
			}
			if ms, _ := ev.Extra["alive_ms"].(int64); ms < 900 || ms > 5000 {
				t.Fatalf("alive_ms = %v，期望约 1000", ev.Extra["alive_ms"])
			}
			seen[ev.Extra["quick_fails"]] = true
		}
		for i := 1; i <= maxQuickFails; i++ {
			if !seen[i] {
				t.Fatalf("各次的 quick_fails 应为 1～%d，缺 %d：%v", maxQuickFails, i, seen)
			}
		}
		gave := sink.byKey(keyHelperGaveUp)
		if len(gave) != 1 {
			t.Fatalf("helper_gave_up 报了 %d 次，期望 1", len(gave))
		}
		want := TelemetryEvent{Tags: map[string]string{"cause": "quick_exit"},
			Extra: map[string]any{"quick_fails": maxQuickFails, "last_exit_code": "0x2", "stderr_tail": redacted}}
		if !mapsEqual(gave[0], want) {
			t.Fatalf("helper_gave_up tags=%v extra=%v，期望 %v %v", gave[0].Tags, gave[0].Extra, want.Tags, want.Extra)
		}
	})

	t.Run("看门狗所杀不报", func(t *testing.T) {
		m, sink := newTelemetryManager(t, io.Discard)
		now := time.Now()
		m.helper = &helperProc{exited: closedChan(), start: now.Add(-time.Second), watchdogKill: true}
		m.helper.killMarkedAt = now.Add(-time.Millisecond)
		m.helper.exit = exitInfo{known: true, code: 1, at: now}
		m.reapHelper()
		settleReports()
		if n := sink.count(); n != 0 {
			t.Fatalf("看门狗所杀报了 %d 个事件，期望 0", n)
		}
	})

	t.Run("捕获失败到放弃那次才报，取触发时的快照", func(t *testing.T) {
		m, sink := newTelemetryManager(t, io.Discard)
		tail := &tailBuffer{}
		m.helper = &helperProc{stdin: &recWriter{}, exited: make(chan struct{}), start: time.Now(), stderrTail: tail}
		t.Cleanup(func() { m.helper = nil }) // 没有真进程，不经 teardownHelper
		m.dispatchedAt = map[string]time.Time{"cloudmusicv3": time.Now()}
		for i := 1; i <= maxQuickFails; i++ {
			m.noteCaptureFailure("cloudmusicv3", 7, time.Now())
		}
		tail.Write([]byte("[helper] activate cloudmusicv3 failed hr=0x88890008\n")) // 安排上报之后、触发之前写入
		settleReports()
		evs := sink.byKey(keyCaptureGaveUp)
		if len(evs) != 1 || sink.count() != 1 {
			t.Fatalf("capture_gave_up 报了 %d 次（共 %d 个事件），期望恰 1 次", len(evs), sink.count())
		}
		want := TelemetryEvent{Tags: map[string]string{"player": "cloudmusicv3"},
			Extra: map[string]any{"failures": maxQuickFails, "stderr_tail": "[helper] activate cloudmusicv3 failed hr=0x88890008\n"}}
		if !mapsEqual(evs[0], want) {
			t.Fatalf("capture_gave_up tags=%v extra=%v，期望 %v %v", evs[0].Tags, evs[0].Extra, want.Tags, want.Extra)
		}
	})

	t.Run("spawn 失败到放弃那次才报", func(t *testing.T) {
		m, sink := newTelemetryManager(t, io.Discard)
		err := errors.New(`exec: "C:\\Users\\张三\\p.exe": Access is denied.`)
		for i := 1; i <= maxQuickFails; i++ {
			m.noteSpawnFailure(err)
		}
		settleReports()
		evs := sink.byKey(keyHelperGaveUp)
		if len(evs) != 1 || sink.count() != 1 {
			t.Fatalf("helper_gave_up 报了 %d 次（共 %d 个事件），期望恰 1 次", len(evs), sink.count())
		}
		want := TelemetryEvent{Tags: map[string]string{"cause": "spawn_failure"},
			Extra: map[string]any{"quick_fails": maxQuickFails, "spawn_error": `exec: "C:\\Users\\<user>\\p.exe": Access is denied.`, "stderr_tail": ""}}
		if !mapsEqual(evs[0], want) {
			t.Fatalf("helper_gave_up tags=%v extra=%v，期望 %v %v", evs[0].Tags, evs[0].Extra, want.Tags, want.Extra)
		}
	})
}

func mapsEqual(got, want TelemetryEvent) bool {
	return fmt.Sprint(got.Tags) == fmt.Sprint(want.Tags) && fmt.Sprint(got.Extra) == fmt.Sprint(want.Extra)
}

// TestNewWiresTelemetry 断言 New 接上默认的上报函数与 3 秒延后（测试里直接构造的 Manager 不上报）。
//
// 变异自证（实测）：New 里的延后改成 0 即红。
func TestNewWiresTelemetry(t *testing.T) {
	m := New(stubSink{}, nil, nil, 25, "")
	if m.report == nil {
		t.Fatal("New 没有接上上报函数")
	}
	if m.reportDelay != 3*time.Second || telemetryReportDelay != 3*time.Second {
		t.Fatalf("上报延后 %v（常量 %v），期望 3s", m.reportDelay, telemetryReportDelay)
	}
}
