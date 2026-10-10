package main

// 本文件只测一件事：启动另一份自身之后，原进程等它结束并沿用它的退出码（runAndWait 与
// relaunchSelf，issue #71）。
//
// 子进程一律是本测试二进制重入自身：TestMain 见到 relaunchHelperEnv 即转入 relaunchHelperMain，
// 不跑任何用例，也不运行服务本体。
//
// # 离线测不到的（只能真机验）
//
//   - 真正的 Ctrl+C。CTRL_C_EVENT 不能只发给指定的进程组（GenerateConsoleCtrlEvent 的文档所述，
//     未实测），发给整个控制台会波及测试运行器。这里发的是可以限定进程组的 CTRL_BREAK_EVENT；
//     二者在 runtime.ctrlHandler 里是同一个分支，都映射为 SIGINT。
//   - 关闭终端窗口（CTRL_CLOSE_EVENT，映射为 SIGTERM）：GenerateConsoleCtrlEvent 产生不了它。
//     runAndWait 有意不接 SIGTERM，原进程此时按默认处理结束；这一点没有用例。
//   - restartSelf 这个调用点：取自身路径、启动失败后的日志与退出码。ensureCanonicalName 的各条分支
//     另见 canonicalname_test.go。
//
// 测试进程没有控制台时 GenerateConsoleCtrlEvent 应当失败，两条 CTRL_BREAK 用例随之跳过
// （推断，未遇到过）。

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// relaunchHelperEnv 非空时，测试二进制作为 helper 子进程运行，取值即角色，见 relaunchHelperMain。
const relaunchHelperEnv = "PLAYERCAP_RELAUNCH_TEST_HELPER"

// helper 自身出错时的退出码，取值避开各用例使用的退出码。
const (
	helperExitEnsureReturned = 95 // ensureCanonicalName 返回了（以当前文件继续运行）
	helperExitReturned       = 96 // relaunchSelf 返回了（启动失败）
	helperExitBadArgs        = 97 // 参数不合预期，包括参数没有传到
	helperExitTimeout        = 98 // 没有等到预期的事件
)

// helperSelfTimeout 是 helper 等待外部事件的上限：用例失败时 helper 自行退出，不留残留进程。
const helperSelfTimeout = 15 * time.Second

// statusControlCExit 是进程被系统默认的控制台事件处理结束时的退出码（STATUS_CONTROL_C_EXIT）。
const statusControlCExit = 0xC000013A

func TestMain(m *testing.M) {
	if role := os.Getenv(relaunchHelperEnv); role != "" {
		relaunchHelperMain(role, os.Args[1:])
	}
	if os.Getenv(loadHelperEnv) != "" {
		loadHelperMain() // 见 cliexit_test.go
	}
	os.Exit(m.Run())
}

// relaunchHelperMain 是 helper 子进程的入口，不返回。
//
//	relay      把角色改为 leaf 后走 relaunchSelf：子进程继承改过的环境变量，以同一份命令行参数
//	           作为 leaf 运行。覆盖出货路径上的参数、标准流、等待与 os.Exit。
//	startfail  runAndWait 启动失败后继续运行：打印 ready，等着被 CTRL_BREAK 结束。
//	ensure     把角色改为 leaf 后调 ensureCanonicalName（走哪条分支取决于本二进制被复制成的文件名）；
//	           它若返回，打印 ensure-returned 并以 helperExitEnsureReturned 退出。
//	ensure-longarg  同 ensure，但先往 os.Args 追加一个超出命令行长度上限的参数，使复制品启动失败。
//	leaf       按参数行事：
//	             exit <code>            立即以 code 退出
//	             sleep <ms> <code>      运行 ms 毫秒后以 code 退出
//	             echo <code> <args...>  把 args 逐个按 %q 写到标准输出，再写从标准输入读到的一行，
//	                                    往标准错误写一行，以 code 退出
//	             graceful <code>        接住中断后打印 ready；收到中断后停 300ms、打印 child-done，
//	                                    以 code 退出
func relaunchHelperMain(role string, args []string) {
	num := func(i int) int {
		if i >= len(args) {
			os.Exit(helperExitBadArgs)
		}
		n, err := strconv.Atoi(args[i])
		if err != nil {
			os.Exit(helperExitBadArgs)
		}
		return n
	}

	switch role {
	case "relay":
		os.Setenv(relaunchHelperEnv, "leaf")
		err := relaunchSelf(os.Args[0])
		fmt.Fprintln(os.Stderr, "relaunchSelf 返回:", err)
		os.Exit(helperExitReturned)

	case "startfail":
		if _, err := runAndWait(os.Args[0]+".missing", nil); err == nil {
			os.Exit(helperExitBadArgs)
		}
		fmt.Println("ready")
		time.Sleep(helperSelfTimeout)
		os.Exit(helperExitTimeout)

	case "ensure", "ensure-longarg":
		if role == "ensure-longarg" {
			os.Args = append(os.Args, strings.Repeat("x", 40000))
		}
		os.Setenv(relaunchHelperEnv, "leaf")
		ensureCanonicalName()
		fmt.Println("ensure-returned")
		os.Exit(helperExitEnsureReturned)

	case "leaf":
		if len(args) == 0 {
			os.Exit(helperExitBadArgs)
		}
		switch args[0] {
		case "exit":
			os.Exit(num(1))
		case "sleep":
			time.Sleep(time.Duration(num(1)) * time.Millisecond)
			os.Exit(num(2))
		case "echo":
			code := num(1)
			for _, a := range args[2:] {
				fmt.Printf("%q\n", a)
			}
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			fmt.Printf("stdin=%q\n", strings.TrimRight(line, "\r\n"))
			fmt.Fprintln(os.Stderr, "leaf-stderr")
			os.Exit(code)
		case "graceful":
			code := num(1)
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt)
			fmt.Println("ready")
			select {
			case <-sigCh:
				time.Sleep(300 * time.Millisecond)
				fmt.Println("child-done")
				os.Exit(code)
			case <-time.After(helperSelfTimeout):
				os.Exit(helperExitTimeout)
			}
		}
	}
	os.Exit(helperExitBadArgs)
}

// TestRunAndWaitReturnsChildExitCode 断言返回值逐一等于子进程的退出码，含 0、大于 255 的值与
// 被 Ctrl+C 默认处理结束时的 STATUS_CONTROL_C_EXIT。
//
// 变异（均实测为红）：启动后不等待、直接返回 0（issue #71 的缺陷）；等待后返回值写死为 0；
// 返回值截成低 8 位（256 与 0xC000013A 两例红）。
func TestRunAndWaitReturnsChildExitCode(t *testing.T) {
	t.Setenv(relaunchHelperEnv, "leaf")
	for _, want := range []int{0, 1, 3, 256, statusControlCExit} {
		got, err := runAndWait(os.Args[0], []string{"exit", strconv.Itoa(want)})
		if err != nil {
			t.Fatalf("退出码 %d：runAndWait 返回错误 %v", want, err)
		}
		if got != want {
			t.Errorf("子进程以 %d（%#x）退出，runAndWait 返回 %d（%#x）", want, want, got, got)
		}
	}
}

// TestRunAndWaitBlocksUntilChildExits 断言子进程运行期间调用方一直阻塞：子进程至少运行 400ms，
// runAndWait 不得早于此返回。启动即返回正是 issue #71 的缺陷。
//
// 变异（实测为红）：启动后不等待、直接返回 0。
func TestRunAndWaitBlocksUntilChildExits(t *testing.T) {
	t.Setenv(relaunchHelperEnv, "leaf")
	const childRuns = 400 * time.Millisecond

	start := time.Now()
	code, err := runAndWait(os.Args[0], []string{"sleep", strconv.FormatInt(childRuns.Milliseconds(), 10), "5"})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("runAndWait 返回错误 %v", err)
	}
	if elapsed < childRuns {
		t.Errorf("子进程至少运行 %v，runAndWait 在 %v 后即返回", childRuns, elapsed)
	}
	if code != 5 {
		t.Errorf("退出码 %d，期望 5", code)
	}
}

// TestRunAndWaitStartFailure 断言启动不了时返回错误、不 panic：调用方据此判断「没有子进程在跑」。
//
// 变异（实测为红）：不检查 cmd.Start 的错误、照常等待并返回 nil。
func TestRunAndWaitStartFailure(t *testing.T) {
	dir := t.TempDir()
	notExe := filepath.Join(dir, "not-an-image.exe")
	if err := os.WriteFile(notExe, []byte("not a PE image"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ name, path string }{
		{"文件不存在", filepath.Join(dir, "missing.exe")},
		{"路径是目录", dir},
		{"内容不是可执行映像", notExe},
		{"空路径", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := runAndWait(c.path, []string{"exit", "0"}); err == nil {
				t.Errorf("runAndWait(%q) 未返回错误", c.path)
			}
		})
	}
}

// runRelay 以 relay 角色运行 helper，喂给它一行标准输入，返回它的退出码与两个输出流的内容。
func runRelay(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), relaunchHelperEnv+"=relay")
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut

	var exitErr *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("运行 relay: %v", err)
	}
	return cmd.ProcessState.ExitCode(), out.String(), errOut.String()
}

// TestRelaunchSelfPassesArgsStreamsAndExitCode 经 relay 走 relaunchSelf 的出货路径：leaf 把收到的
// 参数与标准输入写回标准输出，本测试读回核对；relay 的退出码须等于 leaf 的。
//
// 变异（均实测为红）：relaunchSelf 以写死的 0 退出；relaunchSelf 不传 os.Args[1:]（leaf 以 97 退出）；
// selfCommand 不接 Stdout、不接 Stderr、不接 Stdin（各一次）。
func TestRelaunchSelfPassesArgsStreamsAndExitCode(t *testing.T) {
	cases := []struct {
		name string
		code int
		args []string
	}{
		{"带空格、空串、引号、反斜杠与非 ASCII 的参数", 3,
			[]string{"-addr", "127.0.0.1:8765", "a b", "", `q"uote`, `C:\dir with space\`, "中文参数"}},
		{"没有额外参数且退出码为 0", 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"echo", strconv.Itoa(c.code)}, c.args...)
			code, stdout, stderr := runRelay(t, "line from stdin\n", args...)

			want := ""
			for _, a := range c.args {
				want += fmt.Sprintf("%q\n", a)
			}
			want += fmt.Sprintf("stdin=%q\n", "line from stdin")

			if code != c.code {
				t.Errorf("relay 退出码 %d，期望沿用 leaf 的 %d；stderr: %s", code, c.code, stderr)
			}
			if stdout != want {
				t.Errorf("leaf 的标准输出\n%s期望\n%s", stdout, want)
			}
			if !strings.Contains(stderr, "leaf-stderr") {
				t.Errorf("leaf 的标准错误没有传回: %q", stderr)
			}
		})
	}
}

// startInNewGroup 在新的进程组里以 role 启动 helper，返回命令与它的标准输出。进程组号即 helper 的
// 进程号；helper 的子进程同属该组，CTRL_BREAK 只送达它们。
func startInNewGroup(t *testing.T, role string, args ...string) (*exec.Cmd, *bufio.Reader) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), relaunchHelperEnv+"="+role)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 helper: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	return cmd, bufio.NewReader(stdout)
}

// breakWhenReady 等 helper 一侧打印 ready 后向它的进程组发 CTRL_BREAK，返回此后的标准输出与
// helper 的退出码。
func breakWhenReady(t *testing.T, cmd *exec.Cmd, out *bufio.Reader) (rest string, code int) {
	t.Helper()
	line, err := out.ReadString('\n')
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("没有等到 ready：读到 %q，错误 %v", line, err)
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid)); err != nil {
		t.Skipf("发不出 CTRL_BREAK（测试进程没有控制台？）: %v", err)
	}
	b, _ := io.ReadAll(out)
	cmd.Wait()
	return string(b), cmd.ProcessState.ExitCode()
}

// TestRelaunchSelfOutlivesCtrlBreak 断言等待期间原进程不被中断先行结束：relay 与 leaf 同时收到
// CTRL_BREAK，leaf 收尾 300ms 后以 7 退出，relay 须等到那时并以 7 退出。child-done 同时证明事件
// 确实送达了该进程组。
//
// 变异（均实测为红，relay 以 0xC000013A 退出）：runAndWait 去掉 signal.Notify；
// 换成 signal.Ignore(os.Interrupt)。
func TestRelaunchSelfOutlivesCtrlBreak(t *testing.T) {
	cmd, out := startInNewGroup(t, "relay", "graceful", "7")
	rest, code := breakWhenReady(t, cmd, out)

	if !strings.Contains(rest, "child-done") {
		t.Errorf("leaf 没有走到收尾，输出 %q", rest)
	}
	if code != 7 {
		t.Errorf("relay 退出码 %#x，期望沿用 leaf 的 7（%#x 表示它被 CTRL_BREAK 先行结束）", code, statusControlCExit)
	}
}

// TestRunAndWaitStartFailureRestoresCtrlHandling 断言启动失败返回后中断恢复默认处理：调用方此后以
// 当前进程继续运行，在 main 注册自己的信号处理之前，Ctrl+C 仍须能结束它。
//
// 变异（实测为红，helper 不被结束、15s 后以 98 退出）：runAndWait 去掉 signal.Stop。
func TestRunAndWaitStartFailureRestoresCtrlHandling(t *testing.T) {
	cmd, out := startInNewGroup(t, "startfail")
	_, code := breakWhenReady(t, cmd, out)

	if code != statusControlCExit {
		t.Errorf("helper 退出码 %#x，期望被默认处理结束（%#x）", code, statusControlCExit)
	}
}
