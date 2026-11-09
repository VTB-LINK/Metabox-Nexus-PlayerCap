package watchdog

// 本文件只测主进程等 helper READY 的判定（awaitReady、ElevatedHelper.exited、waitReady）：helper 启动即失败时
// 立即返回，并带上它写下的原因（issue #142）。集成用例拿本测试二进制充当 helper 子进程（TestFakeHelperProcess，
// 跑的是真实的 runPatchHelper，非提权），信号文件与假 libcef.dll 都在临时目录，不碰 %TEMP% 与真实的酷狗安装。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// readyScript 按调用次数给出 awaitReady 的输入。readies、results 超出部分取最后一项（文件写下后一直在），
// 为空则一直读到空串。
type readyScript struct {
	expireAt int // expired 第几次调用（从 0 计）起返回 true；负数表示第 1000 次
	exitAt   int // helperExited 第几次调用起返回 true；负数表示一直无从确认已退出
	readies  []string
	results  []string

	nExpired, nReady, nExit, nResult int
}

func pick(list []string, n int) string {
	if len(list) == 0 {
		return ""
	}
	return list[min(n, len(list)-1)]
}

func (s *readyScript) expired() bool {
	n := s.nExpired
	s.nExpired++
	limit := s.expireAt
	if limit < 0 {
		limit = 1000 // 变异使循环不收敛时以超时判红，而不是挂起
	}
	return n >= limit
}

func (s *readyScript) helperExited() bool {
	n := s.nExit
	s.nExit++
	return s.exitAt >= 0 && n >= s.exitAt
}

func (s *readyScript) readReady() string {
	n := s.nReady
	s.nReady++
	return pick(s.readies, n)
}

func (s *readyScript) readResult() string {
	n := s.nResult
	s.nResult++
	return pick(s.results, n)
}

// TestAwaitReady 断言等 READY 的结论：READY 先到即就绪；helper 写下 ERROR 后退出即失败并带原因；helper 仍在
// 却写下 ERROR 时连续两轮相同才采信；结果文件写了一半不报半截原因；进程已退出又没有原因即报已退出；都没有就
// 等到超时，进程状态无从确认时同样。READY 与 ERROR 同一轮出现时按就绪处理。
//
// 变异自证（隔离副本，实测）：不看进程是否已退出 → 「进程先退出」两例红（等到超时）；不读结果文件 → 读到 ERROR 的
// 各例红；先看结果文件、后看 READY → 「READY 与 ERROR 同一轮」红；去掉「连续两轮相同」（读到 ERROR 即返回）→
// 「结果文件写了一半」红（报出半截原因）；去掉 READY 的 TrimSpace → 「READY 带换行」红；前缀改为只认 "ERROR"
// （不带冒号与空格）→ 「只写到 ERROR」红。
func TestAwaitReady(t *testing.T) {
	cases := []struct {
		name       string
		s          readyScript
		want       readyWait
		wantReason string
		wantResult int // readResult 被调用的次数（负数不查）
	}{
		{"READY 先到（第 3 轮）", readyScript{expireAt: -1, exitAt: -1, readies: []string{"", "", "READY"}}, readyOK, "", 2},
		{"READY 带换行", readyScript{expireAt: -1, exitAt: -1, readies: []string{"READY\r\n"}}, readyOK, "", 0},
		{"ERROR 先到、helper 已退出", readyScript{expireAt: -1, exitAt: 0, results: []string{"ERROR: 原因甲"}}, readyFailed, "原因甲", 1},
		{"ERROR 先到、helper 仍在且一直不退", readyScript{expireAt: -1, exitAt: -1, results: []string{"ERROR: 原因乙"}}, readyFailed, "原因乙", 2},
		{"结果文件写了一半、helper 随后退出", readyScript{expireAt: -1, exitAt: 4, results: []string{"", "ERR", "ERROR: 打开主", "ERROR: 打开主进程失败"}}, readyFailed, "打开主进程失败", 5},
		{"只写到 ERROR、helper 仍在", readyScript{expireAt: -1, exitAt: -1, results: []string{"ERROR", "ERROR", "ERROR: 原因丙"}}, readyFailed, "原因丙", 4},
		{"进程先退出且无结果文件", readyScript{expireAt: -1, exitAt: 2}, readyExited, "", 3},
		{"进程先退出、结果只写了一半", readyScript{expireAt: -1, exitAt: 1, results: []string{"ERR"}}, readyExited, "", 2},
		{"READY 与 ERROR 同一轮", readyScript{expireAt: -1, exitAt: 0, readies: []string{"READY"}, results: []string{"ERROR: 原因丁"}}, readyOK, "", 0},
		{"结果不是 ERROR、随后 READY", readyScript{expireAt: -1, exitAt: -1, readies: []string{"", "", "READY"}, results: []string{"OK"}}, readyOK, "", 2},
		{"超时：进程状态无从确认、一直没有 READY", readyScript{expireAt: 5, exitAt: -1}, readyTimedOut, "", 5},
		{"开始即超时", readyScript{expireAt: 0, exitAt: 0, readies: []string{"READY"}}, readyTimedOut, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.s
			got, reason := awaitReady(s.expired, s.helperExited, s.readReady, s.readResult, 0)
			if got != c.want || reason != c.wantReason {
				t.Fatalf("结论 = (%d, %q)，期望 (%d, %q)", got, reason, c.want, c.wantReason)
			}
			if c.wantResult >= 0 && s.nResult != c.wantResult {
				t.Errorf("readResult 被调用 %d 次，期望 %d", s.nResult, c.wantResult)
			}
		})
	}
}

// TestHelperExited 断言 exited 只在确知 helper 已退出时返回 true：活着、句柄无效（等待失败）都为 false。
//
// 变异自证（隔离副本，实测）：判定改成 `ev != WAIT_TIMEOUT`（等待失败当作已退出）→ 红。
func TestHelperExited(t *testing.T) {
	_, h := startSleeper(t)
	if (&ElevatedHelper{hProcess: h}).exited() {
		t.Fatal("子进程活着，exited 应为 false")
	}
	killAndWait(t, h)
	if !(&ElevatedHelper{hProcess: h}).exited() {
		t.Error("子进程已退出，exited 应为 true")
	}
	if (&ElevatedHelper{hProcess: 0}).exited() {
		t.Error("句柄无效（等待失败），exited 应为 false")
	}
}

// TestFakeHelperProcess 不是独立用例：仅在 KUGOU_WATCHDOG_TEST_HELPER=1 时作为子进程运行，以环境变量给出的
// 参数与信号目录跑真实的 runPatchHelper 并以其返回值退出。未设该变量时直接返回。
func TestFakeHelperProcess(t *testing.T) {
	if os.Getenv("KUGOU_WATCHDOG_TEST_HELPER") != "1" {
		return
	}
	args := strings.Split(os.Getenv("KUGOU_WATCHDOG_TEST_HELPER_ARGS"), "\n")
	self, _ := processCreated(windows.CurrentProcess())
	os.Exit(runPatchHelper(args, windows.GetCurrentProcessId(), self, os.Getenv("KUGOU_WATCHDOG_TEST_HELPER_DIR")))
}

// startFakeHelper 以 args 起一个 helper 子进程，信号文件放在 dir，返回对应的 ElevatedHelper（句柄另开，交给
// 它的 Close 关）与一个等它退出用的句柄；用例结束时结束它。
func startFakeHelper(t *testing.T, dir, token string, args []string) (*ElevatedHelper, windows.Handle) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFakeHelperProcess$")
	cmd.Env = append(os.Environ(),
		"KUGOU_WATCHDOG_TEST_HELPER=1",
		"KUGOU_WATCHDOG_TEST_HELPER_ARGS="+strings.Join(args, "\n"),
		"KUGOU_WATCHDOG_TEST_HELPER_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 helper 子进程: %v", err)
	}
	pid := uint32(cmd.Process.Pid)
	access := uint32(windows.SYNCHRONIZE | windows.PROCESS_QUERY_LIMITED_INFORMATION)
	mine, err1 := windows.OpenProcess(access, false, pid)
	theirs, err2 := windows.OpenProcess(access, false, pid)
	if err1 != nil || err2 != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("打开 helper 子进程 %d: %v / %v", pid, err1, err2)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		windows.CloseHandle(mine)
	})
	ready, trig, res := signalFiles(dir, token)
	return &ElevatedHelper{hProcess: theirs, trigFile: trig, resFile: res, readyFile: ready}, mine
}

// TestWaitReadyRealHelper 端到端（非提权）断言主进程一侧的 waitReady：helper 启动即失败时立即返回（远早于 30 s），
// 有原因时错误里带上原因；helper 正常启动时返回 nil，随后 Close 让它退出、信号文件清空。
//
// 变异自证（隔离副本，实测）：waitReady 不看 helper 是否已退出（传恒为 false 的函数）→ 「令牌不合格」红（等满 30 s
// 才以超时返回）；readyFailed 分支不带原因 → 「令牌合格、PID 不合格」红。
func TestWaitReadyRealHelper(t *testing.T) {
	dir := t.TempDir()
	libcef := filepath.Join(dir, "libcef.dll")
	if err := os.WriteFile(libcef, []byte("FAKE"), 0o644); err != nil {
		t.Fatal(err)
	}
	self := strconv.Itoa(os.Getpid())

	t.Run("令牌合格、PID 不合格：带原因", func(t *testing.T) {
		token := newHelperToken()
		args := []string{libcef, "0", token}
		_, _, _, perr := parseHelperArgs(args, 1)
		h, _ := startFakeHelper(t, dir, token, args)
		start := time.Now()
		err := h.waitReady()
		if el := time.Since(start); el > 5*time.Second {
			t.Errorf("用时 %v，应立即返回", el)
		}
		if err == nil || perr == nil || !strings.Contains(err.Error(), perr.Error()) {
			t.Fatalf("waitReady = %v，应带上 helper 写下的原因 %q", err, perr)
		}
		h.Close()
	})

	t.Run("令牌不合格：未留下原因", func(t *testing.T) {
		token := newHelperToken()
		h, _ := startFakeHelper(t, dir, token, []string{libcef, self, "NOT-A-TOKEN"})
		start := time.Now()
		err := h.waitReady()
		if el := time.Since(start); el > 5*time.Second {
			t.Errorf("用时 %v，应立即返回（helper 已退出）", el)
		}
		if err == nil {
			t.Fatal("waitReady 应报错")
		}
		h.Close()
	})

	t.Run("正常启动，随后 Close 让它退出", func(t *testing.T) {
		token := newHelperToken()
		h, mine := startFakeHelper(t, dir, token, []string{libcef, self, token})
		if err := h.waitReady(); err != nil {
			t.Fatalf("waitReady = %v，期望 nil", err)
		}
		h.Close()
		if ev, _ := windows.WaitForSingleObject(mine, 5000); ev != windows.WAIT_OBJECT_0 {
			t.Fatal("Close 之后 helper 5 s 内未退出")
		}
		ready, trig, res := signalFiles(dir, token)
		for _, f := range []string{ready, trig, res} {
			if _, err := os.Stat(f); !os.IsNotExist(err) {
				t.Errorf("%s 应已清掉：%v", filepath.Base(f), err)
			}
		}
	})
}
