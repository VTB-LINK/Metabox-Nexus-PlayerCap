package launch

// 本文件只测一件事：经 Detached 拉起的进程看到的参数、原始命令行、工作目录与 STARTUPINFO，与修复前的直接启动
// （exec.Command(...).Start()）相比是否一致；退回直接启动的输入照常起得来。
//
// 做法：测试进程扮演 PlayerCap，对同一份假播放器（GUI 子系统的测试二进制副本，放在各种刁钻路径下）先直接启动、
// 再经 Detached 启动，比较两次的报告。cloudmusic 与 spotify 的 watchdog 读播放器的原始命令行判断有没有带保活
// 参数（前者找子串、后者按空白切开找完整参数项），所以原始命令行要求按空白切开后逐项相同。

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// launchBoth 对 exe 先直接启动、再经 Detached 启动，返回两份报告。两次拉起的进程写完报告即退出。
func launchBoth(t *testing.T, exe string, args []string, dir string) (direct, viaCmd report) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv(envRole, "player")

	t.Setenv(envReport, filepath.Join(tmp, "direct.json"))
	c := exec.Command(exe, args...)
	c.Dir = dir
	if err := c.Start(); err != nil {
		t.Fatalf("直接启动: %v", err)
	}
	direct = waitReport(t, filepath.Join(tmp, "direct.json"), 15*time.Second)
	c.Wait()

	t.Setenv(envReport, filepath.Join(tmp, "detached.json"))
	start := time.Now()
	detached, err := Detached(exe, args, dir)
	if err != nil || !detached {
		t.Fatalf("Detached = (%v, %v)，期望经 cmd.exe 中转成功", detached, err)
	}
	t.Logf("cmd.exe 中转耗时 %v", time.Since(start))
	viaCmd = waitReport(t, filepath.Join(tmp, "detached.json"), 15*time.Second)
	awaitExit(t, viaCmd.PID, exe, 10*time.Second)
	t.Logf("直接启动的原始命令行   %q", direct.CmdLine)
	t.Logf("经 Detached 的原始命令行 %q", viaCmd.CmdLine)
	return direct, viaCmd
}

// checkSame 断言经 Detached 拉起的进程与直接启动的看到同样的东西，且确实不是本进程的子进程。
func checkSame(t *testing.T, direct, viaCmd report, wantCwd string) {
	t.Helper()
	if !slices.Equal(viaCmd.Args, direct.Args) {
		t.Errorf("参数不同\n  直接启动   %q\n  经 Detached %q", direct.Args, viaCmd.Args)
	}
	if !slices.Equal(strings.Fields(viaCmd.CmdLine), strings.Fields(direct.CmdLine)) {
		t.Errorf("原始命令行按空白切开后不同\n  直接启动   %q\n  经 Detached %q", direct.CmdLine, viaCmd.CmdLine)
	}
	if !samePath(viaCmd.Cwd, wantCwd) || !samePath(direct.Cwd, wantCwd) {
		t.Errorf("工作目录：直接启动 %q，经 Detached %q，期望 %q", direct.Cwd, viaCmd.Cwd, wantCwd)
	}
	if direct.PPID != os.Getpid() {
		t.Errorf("直接启动的父进程是 %d，不是本进程 %d：夹具有误", direct.PPID, os.Getpid())
	}
	if viaCmd.PPID == os.Getpid() {
		t.Errorf("经 Detached 拉起的进程的父进程是本进程，仍在本进程的进程树里")
	}
	// 不得被要求以隐藏或最小化的状态启动：dwFlags 不带 STARTF_USESHOWWINDOW 时 wShowWindow 不生效。
	if viaCmd.Flags&windows.STARTF_USESHOWWINDOW != 0 && viaCmd.ShowWindow != windows.SW_SHOWNORMAL {
		t.Errorf("经 Detached 拉起的进程被要求以 wShowWindow=%d 启动（dwFlags=%#x）", viaCmd.ShowWindow, viaCmd.Flags)
	}
	t.Logf("STARTUPINFO：直接启动 dwFlags=%#x wShowWindow=%d；经 Detached dwFlags=%#x wShowWindow=%d",
		direct.Flags, direct.ShowWindow, viaCmd.Flags, viaCmd.ShowWindow)
}

// TestDetachedPassesThrough 逐个刁钻的路径与参数，比对经 Detached 与直接启动所见。
//
// 防的缺陷与变异（均实测变红）：
//   - cmdToken 不给元字符加 ^：路径含 & 时 start 只拿到 & 之前的半截路径，参数里的 ^ 被吃掉；
//   - cmdToken 对引号里的元字符也加 ^：「Program Files (x86)」变成另一个不存在的路径，cmd 卡在隐藏的错误框上直到超时；
//   - 程序路径一律加引号：原始命令行按空白切开后首项带了引号；
//   - 参数一律加引号：原始命令行里的参数项带了引号（spotify 按完整参数项找保活标记，找不到）；
//   - command 不设 cmd.Dir：工作目录变成本进程的当前目录；
//   - start 加 /min：被启动的进程被要求最小化启动；
//   - Detached 内部改为直接 exec.Command(...).Start()（仍返回 detached=true）：被启动的进程的父进程是本进程。
func TestDetachedPassesThrough(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	flags := []string{
		"--remote-debugging-port=9222",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-background-timer-throttling",
		"--disable-features=CalculateNativeWinOcclusion",
	}
	cases := []struct {
		name    string
		exeDir  string // 相对用例临时目录
		args    []string
		workDir string // 相对用例临时目录；空为不给
	}{
		{"路径含空格与括号，带等号的参数", `Program Files (x86)\Net Ease`, flags, ""},
		{"路径含与号而不含空格", `a&b`, []string{"--remote-debugging-port=9223"}, ""},
		{"路径不含空格而含脱字符括号与号", `x^y(z)&w`, nil, ""},
		{"路径含中文与空格", `网易云 音乐`, []string{"中文 参数", "--x=中"}, ""},
		{"路径含感叹号且参数含感叹号变量", `wow!hi!`, []string{"!PATH!", "!x"}, ""},
		// 元字符失去保护时：& 与 | 后面是另一条命令，< > 是重定向；工作目录放在临时目录，误建的文件落在那里。
		{"参数含cmd元字符而不含空格", `plain`, []string{"a^b", "x^^y", "p&zzlaunchq", "(r)", "s|zzlaunchu", "u<zzlaunchv", "w>zzlaunchw"}, "work"},
		{"含空格的参数、末尾反斜杠、空参数、制表符", `plain`, []string{"a b", `trail\`, `sp ace\`, "", "\t", "a,b;c=d"}, ""},
		{"空参数列表", `plain`, nil, ""},
		{"工作目录生效", `plain`, nil, `work dir (x86) & 中文`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			exe := fakePlayer(t, filepath.Join(tmp, c.exeDir, "player.exe"))
			dir, wantCwd := "", cwd
			if c.workDir != "" {
				dir = filepath.Join(tmp, c.workDir)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				wantCwd = dir
			}
			direct, viaCmd := launchBoth(t, exe, c.args, dir)
			checkSame(t, direct, viaCmd, wantCwd)
		})
	}
}

// TestDetachedFallbackStillStarts：路径或参数含 % 或 " 时退回直接启动，返回 detached=false、err=nil，进程照常
// 起来、参数原样，Detached 返回之后仍在运行。
//
// 防的缺陷与变异（均实测变红）：去掉 % 的检查（返回 detached=true，%PATH% 被 cmd 展开成本机的 PATH）；去掉 " 的
// 检查（返回 detached=true，参数被改写）；退路改用 exec.CommandContext(ctx, …)（Detached 返回时 ctx 取消，刚起来
// 的播放器被结束）。
func TestDetachedFallbackStillStarts(t *testing.T) {
	cases := []struct {
		name   string
		exeDir string
		args   []string
	}{
		{"参数含百分号变量", `plain`, []string{"%PATH%", "100%"}},
		{"参数含引号", `plain`, []string{`a"b`, `"`}},
		{"路径含百分号", `50%off`, []string{"--x=1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			exe := fakePlayer(t, filepath.Join(tmp, c.exeDir, "player.exe"))
			rep := filepath.Join(tmp, "player.json")
			t.Setenv(envRole, "player")
			t.Setenv(envReport, rep)
			t.Setenv(envLinger, "5s")
			detached, err := Detached(exe, c.args, "")
			if err != nil || detached {
				t.Fatalf("Detached = (%v, %v)，期望 (false, nil)", detached, err)
			}
			r := waitReport(t, rep, 15*time.Second)
			h := own(t, r.PID, exe)
			if !slices.Equal(r.Args, c.args) {
				t.Errorf("参数 %q，期望原样 %q", r.Args, c.args)
			}
			if r.PPID != os.Getpid() {
				t.Errorf("退路是直接启动，父进程应是本进程，实为 %d", r.PPID)
			}
			if exited(h, time.Second) {
				t.Errorf("Detached 返回之后，退路拉起的进程不到 1 秒就退出了（它应运行 5 秒）")
			}
		})
	}
}
