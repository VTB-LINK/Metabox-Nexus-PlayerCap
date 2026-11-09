package watchdog

// 本文件只测提权 helper 何时退出、何时写 DLL（issue #140）：等指令的判定（awaitTrigger）、读触发文件
// （takeTrigger）、主进程句柄（openParent、parentExitedWithin）、主进程一侧的取消（Close），以及把它们接起来的
// runPatchHelper。命令行与令牌在 helperargs_test.go。
//
// 用到真实进程的用例拿本测试二进制长睡的子进程充当主进程（TestFakeParentProcess）；runPatchHelper 在本进程内
// 跑，信号文件放在临时目录，libcef.dll 是临时目录里的假文件，不碰 %TEMP% 下的信号文件与真实的酷狗安装。

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestFakeParentProcess 不是独立用例：仅在 KUGOU_WATCHDOG_TEST_SLEEPER=1 时作为子进程运行，长睡，充当主进程。
// 未设该变量时直接返回。
func TestFakeParentProcess(t *testing.T) {
	if os.Getenv("KUGOU_WATCHDOG_TEST_SLEEPER") != "1" {
		return
	}
	time.Sleep(time.Hour)
}

// startSleeper 起一个长睡的子进程，返回其 PID 与一个可等待、可结束的句柄；用例结束时结束它、关句柄。
func startSleeper(t *testing.T) (uint32, windows.Handle) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFakeParentProcess$")
	cmd.Env = append(os.Environ(), "KUGOU_WATCHDOG_TEST_SLEEPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程: %v", err)
	}
	pid := uint32(cmd.Process.Pid)
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("打开子进程 %d: %v", pid, err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		windows.CloseHandle(h)
	})
	return pid, h
}

// killAndWait 结束 h 所指的进程并等它退出。句柄仍由调用方持有，进程对象因此不释放、PID 不会被复用。
func killAndWait(t *testing.T, h windows.Handle) {
	t.Helper()
	if err := windows.TerminateProcess(h, 1); err != nil {
		t.Fatalf("TerminateProcess: %v", err)
	}
	if ev, _ := windows.WaitForSingleObject(h, 5000); ev != windows.WAIT_OBJECT_0 {
		t.Fatalf("子进程 5s 内未退出")
	}
}

// script 按调用次数给出 awaitTrigger 的三个输入，并记下各被调了几次。parentExited 另核对传入的等待时长：
// 紧跟在读到 GO 之后的那一次应为 0（不等），其余应为一个轮询间隔。
type script struct {
	expireAt int      // expired 第几次调用（从 0 计）起返回 true；负数表示第 1000 次（见 expired）
	exitAt   int      // parentExited 第几次调用起返回 true；负数表示永不
	triggers []string // 第 i 次 takeTrigger 的返回值，超出部分为空串

	nExpired, nParent, nTake int
	tookGo                   bool     // 上一次 takeTrigger 返回了 GO
	badWaits                 []string // 等待时长不符的记录
}

func (s *script) expired() bool {
	n := s.nExpired
	s.nExpired++
	limit := s.expireAt
	if limit < 0 {
		limit = 1000 // 不设超时的用例也给上限：变异使循环不收敛时以「超时」结论判红，而不是挂起
	}
	return n >= limit
}

func (s *script) parentExited(wait time.Duration) bool {
	want := helperPollInterval
	if s.tookGo {
		want = 0
	}
	if wait != want {
		s.badWaits = append(s.badWaits, fmt.Sprintf("第 %d 次 parentExited 等 %v，期望 %v", s.nParent, wait, want))
	}
	s.tookGo = false
	n := s.nParent
	s.nParent++
	return s.exitAt >= 0 && n >= s.exitAt
}

func (s *script) takeTrigger() string {
	n := s.nTake
	s.nTake++
	r := ""
	if n < len(s.triggers) {
		r = s.triggers[n]
	}
	s.tookGo = r == triggerGo
	return r
}

// TestAwaitTrigger 断言等指令的结论，以及结论之后不再读触发文件（读即消费）。
//
// 主进程退出与指令同一轮出现时按主进程退出处理、不写 DLL，指令也不读（取舍见 awaitTrigger 的注释）；读到 GO
// 之后不等待地再查一次主进程，这时已退出同样不写；超时与指令同一轮出现时按超时处理，与改动前「到点即停」一致。
//
// 变异自证（隔离副本，实测）：循环里先读触发文件、后看主进程 → 红（含「同一轮：」开头的三例）；丢弃轮首
// parentExited 的结果 → 轮首即判出主进程退出的五例红；删掉读到 GO 之后的复查 → 「读到 GO 时主进程刚退出」
// 两例红；复查改为等一个轮询间隔 → 读到 GO 的各例红（等待时长不符）；删掉 CANCEL 分支 → 「收到 CANCEL」
// 「CANCEL 之后的 GO 不再读」两例红；GO 与 CANCEL 两个分支的返回值对调 → 读到指令的各例红；循环条件改成
// for {}（不看超时）→ 死循环，由 go test -timeout 判红。
func TestAwaitTrigger(t *testing.T) {
	cases := []struct {
		name          string
		s             script
		want          helperVerdict
		wantTake      int // 结论得出时 takeTrigger 被调用的次数
		wantParentMax int // parentExited 至多被调用的次数（负数不查）
	}{
		{"第 3 轮收到 GO", script{expireAt: -1, exitAt: -1, triggers: []string{"", "", triggerGo}}, verdictPatch, 3, -1},
		{"第 1 轮就收到 GO", script{expireAt: -1, exitAt: -1, triggers: []string{triggerGo}}, verdictPatch, 1, -1},
		{"等待中主进程退出", script{expireAt: -1, exitAt: 2}, verdictParentExited, 2, 3},
		{"第 1 轮主进程就已退出", script{expireAt: -1, exitAt: 0}, verdictParentExited, 0, 1},
		{"同一轮：主进程退出且有 GO", script{expireAt: -1, exitAt: 2, triggers: []string{"", "", triggerGo}}, verdictParentExited, 2, 3},
		{"同一轮：第 1 轮主进程已退出且 GO 早已写好", script{expireAt: -1, exitAt: 0, triggers: []string{triggerGo}}, verdictParentExited, 0, 1},
		{"同一轮：主进程退出且有 CANCEL", script{expireAt: -1, exitAt: 1, triggers: []string{"", triggerCancel}}, verdictParentExited, 1, 2},
		{"读到 GO 时主进程刚退出（第 3 轮）", script{expireAt: -1, exitAt: 3, triggers: []string{"", "", triggerGo}}, verdictParentExited, 3, 4},
		{"读到 GO 时主进程刚退出（第 1 轮）", script{expireAt: -1, exitAt: 1, triggers: []string{triggerGo}}, verdictParentExited, 1, 2},
		{"收到 CANCEL", script{expireAt: -1, exitAt: -1, triggers: []string{"", triggerCancel}}, verdictCancelled, 2, -1},
		{"CANCEL 之后的 GO 不再读", script{expireAt: -1, exitAt: -1, triggers: []string{triggerCancel, triggerGo}}, verdictCancelled, 1, -1},
		{"GO 之后的 CANCEL 不再读", script{expireAt: -1, exitAt: -1, triggers: []string{triggerGo, triggerCancel}}, verdictPatch, 1, -1},
		{"不认识的内容不算指令", script{expireAt: -1, exitAt: -1, triggers: []string{"G", "CANC", "go", "GO\n", " GO", triggerGo}}, verdictPatch, 6, -1},
		{"超时：主进程在、一直没有指令", script{expireAt: 5, exitAt: -1}, verdictTimedOut, 5, 5},
		{"开始即超时：什么都不看", script{expireAt: 0, exitAt: -1, triggers: []string{triggerGo}}, verdictTimedOut, 0, 0},
		{"超时前一轮收到 GO", script{expireAt: 3, exitAt: -1, triggers: []string{"", "", triggerGo}}, verdictPatch, 3, -1},
		{"同一轮：超时且有 GO", script{expireAt: 2, exitAt: -1, triggers: []string{"", "", triggerGo}}, verdictTimedOut, 2, 2},
		{"超时之后主进程才退出", script{expireAt: 2, exitAt: 2}, verdictTimedOut, 2, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.s
			got := awaitTrigger(s.expired, s.parentExited, s.takeTrigger)
			if got != c.want {
				t.Fatalf("结论 = %d，期望 %d", got, c.want)
			}
			if s.nTake != c.wantTake {
				t.Errorf("takeTrigger 被调用 %d 次，期望 %d（多读即多消费一条指令）", s.nTake, c.wantTake)
			}
			if c.wantParentMax >= 0 && s.nParent > c.wantParentMax {
				t.Errorf("parentExited 被调用 %d 次，期望至多 %d", s.nParent, c.wantParentMax)
			}
			for _, b := range s.badWaits {
				t.Error(b)
			}
		})
	}
}

// TestAwaitTriggerRechecksParentAfterGo 用真实进程复现「本轮等待之后、读触发文件之前，主进程写下 GO 并退出」：
// takeTrigger 回调里先结束充当主进程的子进程、再返回 GO，awaitTrigger 必须判为主进程已退出、不写。对照组
// 不结束子进程，同样返回 GO，判为写。
//
// 变异自证（隔离副本，实测）：删掉读到 GO 之后的复查 → 「GO 之前主进程已退出」红，对照组仍绿。
func TestAwaitTriggerRechecksParentAfterGo(t *testing.T) {
	for _, c := range []struct {
		name string
		kill bool
		want helperVerdict
	}{
		{"GO 之前主进程已退出", true, verdictParentExited},
		{"对照：主进程一直在", false, verdictPatch},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, h := startSleeper(t)
			rounds := 0
			got := awaitTrigger(
				func() bool { rounds++; return rounds > 10 },
				func(wait time.Duration) bool { return parentExitedWithin(h, wait) },
				func() string {
					if c.kill {
						killAndWait(t, h)
					}
					return triggerGo
				},
			)
			if got != c.want {
				t.Fatalf("结论 = %d，期望 %d", got, c.want)
			}
		})
	}
}

// TestTakeTrigger 断言只有内容恰为 GO 或 CANCEL 才算指令并删掉文件；文件刚建好还没写完（空、半截）时
// 不算、不删，写完之后的下一轮读到。
//
// 变异自证（隔离副本，实测）：改回只看文件在不在（存在即返回 GO）、认出指令后不删文件、比较前先 TrimSpace，均红。
func TestTakeTrigger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trigger.txt")

	if got := takeTrigger(path); got != "" {
		t.Fatalf("文件不存在时应返回空串，实得 %q", got)
	}

	for _, content := range []string{"", "G", "C", "CAN", "CANCE", "go", "cancel", "GO\n", "GO\r\n", " GO", "GOGO", "CANCELGO", "READY"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := takeTrigger(path); got != "" {
			t.Errorf("内容 %q 不是指令，应返回空串，实得 %q", content, got)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("内容 %q 不是指令，文件应保留（等写完再读），实得 %v", content, err)
		}
	}

	for _, cmd := range []string{triggerGo, triggerCancel} {
		if err := os.WriteFile(path, []byte(""), 0o600); err != nil { // 主进程刚建好文件
			t.Fatal(err)
		}
		if got := takeTrigger(path); got != "" {
			t.Fatalf("空文件应返回空串，实得 %q", got)
		}
		if err := os.WriteFile(path, []byte(cmd), 0o600); err != nil { // 写完
			t.Fatal(err)
		}
		if got := takeTrigger(path); got != cmd {
			t.Errorf("内容 %q 应返回该指令，实得 %q", cmd, got)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("读到指令 %q 后应删掉触发文件，实得 %v", cmd, err)
		}
	}
}

// TestOpenParent 断言打开主进程：活着、且创建早于 helper 的才算；PID 打不开、或那个进程的创建不早于 helper
// （PID 已被复用给 helper 之后才创建的进程）都拒绝。
//
// 变异自证（隔离副本，实测）：去掉创建时间核对 → 「创建晚于 helper」「创建与 helper 同时」两例红；
// `>=` 改成 `>` → 「同时」一例红。
func TestOpenParent(t *testing.T) {
	pid, h := startSleeper(t)
	childCreated, err := processCreated(h)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("接受/创建早于 helper", func(t *testing.T) {
		ph, err := openParent(pid, childCreated+1)
		if err != nil {
			t.Fatalf("openParent: %v", err)
		}
		defer windows.CloseHandle(ph)
		if ev, _ := windows.WaitForSingleObject(ph, 0); ev != uint32(windows.WAIT_TIMEOUT) {
			t.Errorf("返回的句柄应指向仍在运行的子进程，WaitForSingleObject = %#x", ev)
		}
	})
	for _, c := range []struct {
		name        string
		pid         uint32
		selfCreated int64
	}{
		{"创建晚于 helper（PID 复用）", pid, time.Now().Add(-time.Hour).UnixNano()},
		{"创建与 helper 同时", pid, childCreated},
		{"PID 不存在", 0xFFFFFFFC, time.Now().Add(time.Hour).UnixNano()},
		{"PID 为 0", 0, time.Now().Add(time.Hour).UnixNano()},
	} {
		t.Run("拒绝/"+c.name, func(t *testing.T) {
			ph, err := openParent(c.pid, c.selfCreated)
			if err == nil {
				windows.CloseHandle(ph)
				t.Fatalf("openParent(%d) 应报错", c.pid)
			}
			if ph != 0 {
				t.Errorf("报错时句柄应为 0，实得 %#x", ph)
			}
		})
	}
}

// TestParentExitedWithin 断言等主进程：活着时等满给定时长、返回 false；给定 0 时不等；退出后立即返回 true；
// 等待本身失败（句柄无效）时返回 true。
//
// 变异自证（隔离副本，实测）：判定改成 `ev == WAIT_OBJECT_0`（等待失败当作还活着）、等待时长恒为 0（helper
// 会空转），均红。
func TestParentExitedWithin(t *testing.T) {
	_, h := startSleeper(t)

	start := time.Now()
	if parentExitedWithin(h, helperPollInterval) {
		t.Fatal("子进程活着，应返回 false")
	}
	if el := time.Since(start); el < helperPollInterval/2 {
		t.Errorf("活着时应等满约一个间隔（%v），实际只等了 %v", helperPollInterval, el)
	}
	start = time.Now()
	if parentExitedWithin(h, 0) {
		t.Fatal("子进程活着，给定 0 也应返回 false")
	}
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Errorf("给定 0 时不应等待，实际等了 %v", el)
	}

	killAndWait(t, h)
	start = time.Now()
	if !parentExitedWithin(h, 5*time.Second) {
		t.Fatal("子进程已退出，应返回 true")
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("已退出时应立即返回，实际等了 %v", el)
	}

	if !parentExitedWithin(0, helperPollInterval) {
		t.Error("句柄无效时等待失败，应按主进程已不在返回 true")
	}
}

// TestCloseCancelsLiveHelper 断言 Close：helper 还在运行时向本次的触发文件写 CANCEL（覆盖未被取走的 GO），
// 已退出时删掉它；两种情况都删掉本次的 READY 与结果文件，别的令牌的信号文件不动。
//
// 变异自证（隔离副本，实测）：改回无条件删除触发文件 → 「活着」两例红；判定写反（退出了才写 CANCEL）→ 三例全红。
func TestCloseCancelsLiveHelper(t *testing.T) {
	for _, c := range []struct {
		name     string
		alive    bool
		existing string // Close 之前触发文件的内容；空串表示不存在
	}{
		{"活着、没有触发文件", true, ""},
		{"活着、GO 未被取走", true, triggerGo},
		{"已退出、GO 未被取走", false, triggerGo},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ready, trig, res := signalFiles(dir, testToken)
			oReady, oTrig, oRes := signalFiles(dir, "fedcba9876543210") // 别的令牌
			for _, f := range []string{ready, res, oReady, oTrig, oRes} {
				if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if c.existing != "" {
				if err := os.WriteFile(trig, []byte(c.existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			pid, sh := startSleeper(t)
			if !c.alive {
				killAndWait(t, sh)
			}
			// Close 会关掉交给它的句柄，另开一个给它。
			hh, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
			if err != nil {
				t.Fatal(err)
			}
			(&ElevatedHelper{hProcess: hh, trigFile: trig, resFile: res, readyFile: ready}).Close()

			data, err := os.ReadFile(trig)
			if c.alive {
				if err != nil || string(data) != triggerCancel {
					t.Errorf("helper 活着：触发文件应为 %q，实得 %q（%v）", triggerCancel, data, err)
				}
			} else if !os.IsNotExist(err) {
				t.Errorf("helper 已退出：触发文件应删掉，实得 %q（%v）", data, err)
			}
			for _, f := range []string{ready, res} {
				if _, err := os.Stat(f); !os.IsNotExist(err) {
					t.Errorf("%s 应删掉，实得 %v", filepath.Base(f), err)
				}
			}
			for _, f := range []string{oReady, oTrig, oRes} {
				if data, err := os.ReadFile(f); err != nil || string(data) != "x" {
					t.Errorf("别的令牌的 %s 不应被动过，实得 %q（%v）", filepath.Base(f), data, err)
				}
			}
		})
	}
}

// helperRun 是在本进程内跑起来的 runPatchHelper。信号文件放在 sigDir（dir 的子目录），以便查出越出 sigDir 的写入。
type helperRun struct {
	dir, sigDir, libcef string
	token               string
	ready, trig, res    string
	done                chan int
	origLibcef          []byte
	selfCreated         int64
	args                []string
	parentPID           uint32
	parentHandle        windows.Handle
}

// newHelperRun 准备临时目录、假 libcef.dll 与充当主进程的子进程（创建早于「helper」）。parent 非 0 时用它
// 充当主进程，否则另起一个。
func newHelperRun(t *testing.T, dir string, parentPID uint32, parent windows.Handle) *helperRun {
	t.Helper()
	r := &helperRun{dir: dir, sigDir: filepath.Join(dir, "sig"), done: make(chan int, 1), token: newHelperToken()}
	if err := os.MkdirAll(r.sigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.ready, r.trig, r.res = signalFiles(r.sigDir, r.token)
	r.libcef = filepath.Join(r.dir, "libcef.dll")
	r.origLibcef = []byte("FAKE-LIBCEF-NOT-A-REAL-DLL\x00\x01\xFF")
	if err := os.WriteFile(r.libcef, r.origLibcef, 0o644); err != nil {
		t.Fatal(err)
	}
	if parent == 0 {
		parentPID, parent = startSleeper(t)
	}
	r.parentPID, r.parentHandle = parentPID, parent
	r.selfCreated = time.Now().Add(time.Hour).UnixNano() // 子进程算作早于 helper 创建
	r.args = []string{r.libcef, strconv.FormatUint(uint64(r.parentPID), 10), r.token}
	return r
}

func (r *helperRun) start() {
	go func() { r.done <- runPatchHelper(r.args, windows.GetCurrentProcessId(), r.selfCreated, r.sigDir) }()
}

func (r *helperRun) awaitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(r.ready); err == nil && string(data) == "READY" {
			return
		}
		select {
		case code := <-r.done:
			t.Fatalf("helper 未写 READY 就退出了，退出码 %d", code)
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("helper 5s 内未写 READY")
}

func (r *helperRun) awaitExit(t *testing.T, limit time.Duration) int {
	t.Helper()
	select {
	case code := <-r.done:
		return code
	case <-time.After(limit):
		t.Fatalf("helper %v 内未退出", limit)
		return -1
	}
}

// assertNotPatched 断言假 libcef.dll 原样、没有备份、没有结果文件（没有走到 patchWithBackup）。
func (r *helperRun) assertNotPatched(t *testing.T) {
	t.Helper()
	if got, _ := os.ReadFile(r.libcef); !bytes.Equal(got, r.origLibcef) {
		t.Errorf("libcef.dll 被改动：%q", got)
	}
	if _, err := os.Stat(r.libcef + ".playercap.bak"); !os.IsNotExist(err) {
		t.Errorf("不应产生 .bak：%v", err)
	}
	if data, err := os.ReadFile(r.res); !os.IsNotExist(err) {
		t.Errorf("不应写结果文件，实得 %q（%v）", data, err)
	}
}

// files 列出 dir 下（含子目录）的全部文件，路径相对 dir。
func files(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	slices.Sort(out)
	return out
}

// assertOnly 断言 dir 下的文件恰为 want（相对路径），即 helper 没有留下、也没有越界写出别的文件。
func assertOnly(t *testing.T, dir string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := files(t, dir); !slices.Equal(got, want) {
		t.Errorf("目录里的文件 = %q，期望 %q", got, want)
	}
}

// TestRunPatchHelperFollowsParent 端到端（本进程内）断言 helper 跟随主进程：等指令期间主进程退出，helper 在约
// 一个轮询间隔内退出、不写 DLL、本组信号文件一个不留；主进程在 helper 打开它之前就已退出（句柄还在、进程对象
// 未释放）时同样；退出与 GO 同时出现时不写。
//
// 变异自证（隔离副本，实测）：runPatchHelper 里等主进程的那一项换成 `time.Sleep(helperPollInterval); return false`
// （改动前的行为）→ 三例全红（前两例 helper 不退出，第三例取走 GO 去写）；退出时不删 READY → 三例全红；退出时
// 不删触发文件 → 「主进程已退出且 GO 已写好」红。
func TestRunPatchHelperFollowsParent(t *testing.T) {
	t.Run("等指令期间主进程退出", func(t *testing.T) {
		r := newHelperRun(t, t.TempDir(), 0, 0)
		r.start()
		r.awaitReady(t)
		killAndWait(t, r.parentHandle)
		killed := time.Now()
		if code := r.awaitExit(t, 5*time.Second); code != 0 {
			t.Errorf("退出码 %d，期望 0", code)
		}
		if el := time.Since(killed); el > time.Second {
			t.Errorf("主进程退出后 %v 才退出，期望约一个轮询间隔（%v）", el, helperPollInterval)
		}
		r.assertNotPatched(t)
		assertOnly(t, r.dir, "libcef.dll")
	})

	t.Run("主进程在 helper 打开它之前已退出", func(t *testing.T) {
		r := newHelperRun(t, t.TempDir(), 0, 0)
		killAndWait(t, r.parentHandle)
		r.start()
		if code := r.awaitExit(t, 5*time.Second); code != 0 {
			t.Errorf("退出码 %d，期望 0", code)
		}
		r.assertNotPatched(t)
		assertOnly(t, r.dir, "libcef.dll")
	})

	t.Run("主进程已退出且 GO 已写好", func(t *testing.T) {
		r := newHelperRun(t, t.TempDir(), 0, 0)
		if err := os.WriteFile(r.trig, []byte(triggerGo), 0o600); err != nil {
			t.Fatal(err)
		}
		killAndWait(t, r.parentHandle)
		r.start()
		if code := r.awaitExit(t, 5*time.Second); code != 0 {
			t.Errorf("退出码 %d，期望 0", code)
		}
		r.assertNotPatched(t)
		assertOnly(t, r.dir, "libcef.dll")
	})
}

// TestRunPatchHelperCancel 端到端（本进程内）断言主进程的 Close 能让等指令的 helper 退出、不写 DLL，退出后本组
// 信号文件全部清掉。Close 拿到的句柄是本进程（helper 就在本进程里跑），WaitForSingleObject 得到「仍在运行」。
// 另断言 Close 先于 READY 写下 CANCEL（helper 卡在启动里、主进程已不再等它）时，helper 醒来即退出、不留 READY。
//
// 变异自证（隔离副本，实测）：Close 改回无条件删除触发文件 → 两例红（helper 不退出）；runPatchHelper 的 CANCEL
// 分支改为接着 patch → 两例红（写了结果文件）；退出时不删 READY → 「READY 之前」红。
func TestRunPatchHelperCancel(t *testing.T) {
	t.Run("READY 之后 Close", func(t *testing.T) {
		r := newHelperRun(t, t.TempDir(), 0, 0)
		r.start()
		r.awaitReady(t)
		(&ElevatedHelper{hProcess: windows.CurrentProcess(), trigFile: r.trig, resFile: r.res, readyFile: r.ready}).Close()
		if code := r.awaitExit(t, 5*time.Second); code != 0 {
			t.Errorf("退出码 %d，期望 0", code)
		}
		r.assertNotPatched(t)
		assertOnly(t, r.dir, "libcef.dll")
	})

	t.Run("READY 之前 Close", func(t *testing.T) {
		r := newHelperRun(t, t.TempDir(), 0, 0)
		(&ElevatedHelper{hProcess: windows.CurrentProcess(), trigFile: r.trig, resFile: r.res, readyFile: r.ready}).Close()
		r.start()
		if code := r.awaitExit(t, 5*time.Second); code != 0 {
			t.Errorf("退出码 %d，期望 0", code)
		}
		r.assertNotPatched(t)
		assertOnly(t, r.dir, "libcef.dll")
	})
}

// TestRunPatchHelperGoReachesRecheck 端到端（本进程内）断言主进程在、收到 GO 时 helper 才去写，且写之前的复检
// 拒绝了不认识的文件：假 libcef.dll 原样、没有备份，只留下报 ERROR 的结果文件。
//
// 变异自证（隔离副本，实测）：GO 分支改成直接返回 0 → 红（没有结果文件）。
func TestRunPatchHelperGoReachesRecheck(t *testing.T) {
	r := newHelperRun(t, t.TempDir(), 0, 0)
	r.start()
	r.awaitReady(t)
	if err := os.WriteFile(r.trig, []byte(triggerGo), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := r.awaitExit(t, 5*time.Second); code != 1 {
		t.Errorf("退出码 %d，期望 1（复检拒绝）", code)
	}
	if got, _ := os.ReadFile(r.libcef); !bytes.Equal(got, r.origLibcef) {
		t.Errorf("libcef.dll 被改动：%q", got)
	}
	if data, err := os.ReadFile(r.res); err != nil || !strings.HasPrefix(string(data), "ERROR: ") {
		t.Errorf("结果文件应以 ERROR: 开头，实得 %q（%v）", data, err)
	}
	assertOnly(t, r.dir, "libcef.dll", filepath.Join("sig", filepath.Base(r.res)))
}

// TestRunPatchHelperTokensIsolated 端到端（本进程内）断言两个不同令牌的 helper 跟着同一个主进程、在同一目录里
// 同时等待时互不读对方的指令：向 A 写 GO 只有 A 去写（复检拒绝假文件、留下 A 的结果），B 照旧等；随后向 B 写
// CANCEL，B 退出且不写。
//
// 变异自证（隔离副本，实测）：signalFiles 不带令牌（退回共用一组文件）→ 红。
func TestRunPatchHelperTokensIsolated(t *testing.T) {
	dir := t.TempDir()
	a := newHelperRun(t, dir, 0, 0)
	b := newHelperRun(t, dir, a.parentPID, a.parentHandle)
	if a.token == b.token {
		t.Fatal("两次生成的令牌相同")
	}
	a.start()
	b.start()
	a.awaitReady(t)
	b.awaitReady(t)

	if err := os.WriteFile(a.trig, []byte(triggerGo), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := a.awaitExit(t, 5*time.Second); code != 1 {
		t.Errorf("A 退出码 %d，期望 1（复检拒绝）", code)
	}
	if data, err := os.ReadFile(a.res); err != nil || !strings.HasPrefix(string(data), "ERROR: ") {
		t.Errorf("A 的结果文件应以 ERROR: 开头，实得 %q（%v）", data, err)
	}
	select {
	case code := <-b.done:
		t.Fatalf("B 不应响应 A 的 GO，却以 %d 退出", code)
	case <-time.After(3 * helperPollInterval):
	}
	if data, err := os.ReadFile(b.ready); err != nil || string(data) != "READY" {
		t.Errorf("B 的 READY 应还在，实得 %q（%v）", data, err)
	}

	if err := os.WriteFile(b.trig, []byte(triggerCancel), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := b.awaitExit(t, 5*time.Second); code != 0 {
		t.Errorf("B 退出码 %d，期望 0", code)
	}
	b.assertNotPatched(t)
	if got, _ := os.ReadFile(a.libcef); !bytes.Equal(got, a.origLibcef) {
		t.Errorf("libcef.dll 被改动：%q", got)
	}
	assertOnly(t, dir, "libcef.dll", filepath.Join("sig", filepath.Base(a.res)))
}

// TestRunPatchHelperRefusesBadStart 端到端（本进程内）断言启动即拒绝的各种情形：helper 不写 READY、不等指令、
// 不写 DLL，立即以 1 退出。参数个数或令牌不合格时什么文件都不写（含越出信号目录的写入）；令牌合格、错在
// 路径、PID 或主进程时，只在本次的结果文件里写明原因。
//
// 变异自证（隔离副本，实测）：去掉 runPatchHelper 开头的令牌检查（令牌为空时照常往下走）→ 参数个数与令牌各例
// 红；出错写完结果文件后不返回、接着等指令 → 留结果文件的四例全红。
func TestRunPatchHelperRefusesBadStart(t *testing.T) {
	for _, c := range []struct {
		name       string
		mutate     func(r *helperRun)
		wantResult bool // 是否应留下本次的结果文件
	}{
		{"缺令牌（上一版格式）", func(r *helperRun) { r.args = r.args[:2] }, false},
		{"缺 PID 与令牌", func(r *helperRun) { r.args = r.args[:1] }, false},
		{"令牌含大写", func(r *helperRun) { r.args[2] = strings.ToUpper(r.token) }, false},
		{"令牌长度不对", func(r *helperRun) { r.args[2] = r.token[:helperTokenLen-1] }, false},
		{`令牌越出信号目录（\..\..\）`, func(r *helperRun) { r.args[2] = `\..\..\escape` }, false},
		{"令牌越出信号目录（/../../）", func(r *helperRun) { r.args[2] = "/../../escape" }, false},
		{"PID 是 helper 自己", func(r *helperRun) {
			r.args[1] = strconv.FormatUint(uint64(windows.GetCurrentProcessId()), 10)
		}, true},
		{"路径为空", func(r *helperRun) { r.args[0] = "" }, true},
		{"主进程创建晚于 helper（PID 复用）", func(r *helperRun) { r.selfCreated = time.Now().Add(-time.Hour).UnixNano() }, true},
		{"PID 打不开", func(r *helperRun) { r.args[1] = strconv.FormatUint(0xFFFFFFFC, 10) }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newHelperRun(t, t.TempDir(), 0, 0)
			c.mutate(r)
			r.start()
			if code := r.awaitExit(t, 2*time.Second); code != 1 {
				t.Errorf("退出码 %d，期望 1", code)
			}
			if got, _ := os.ReadFile(r.libcef); !bytes.Equal(got, r.origLibcef) {
				t.Errorf("libcef.dll 被改动：%q", got)
			}
			if !c.wantResult {
				assertOnly(t, r.dir, "libcef.dll")
				return
			}
			if data, err := os.ReadFile(r.res); err != nil || !strings.HasPrefix(string(data), "ERROR: ") {
				t.Errorf("结果文件应以 ERROR: 开头，实得 %q（%v）", data, err)
			}
			assertOnly(t, r.dir, "libcef.dll", filepath.Join("sig", filepath.Base(r.res)))
		})
	}
}
