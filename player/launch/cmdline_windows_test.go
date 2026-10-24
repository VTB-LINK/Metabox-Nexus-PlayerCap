package launch

// 本文件只测 command 这个纯函数：经 cmd.exe 中转时的命令行逐字内容与 cmd 的启动标志，以及哪些输入退回直接启动。
// 不启动任何进程；拉起之后的实际效果见 passthrough_windows_test.go 与 processtree_windows_test.go。

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

const shellPrefix = `cmd.exe /d /v:off /s /c "start "" `

// TestCommandLine 钉住中转命令行的逐字内容。
//
// 防的缺陷与变异（均实测变红）：
//   - 去掉 /v:off：注册表把延迟展开设为默认开启时，路径与参数里的 !变量! 被展开；
//   - 去掉 /d：注册表里的 AutoRun 命令随 cmd 一起执行；
//   - 去掉空标题 ""：start 把带引号的程序路径当作窗口标题，程序本身不被启动；
//   - cmdToken 不给不在引号里的元字符加 ^：& 把命令截成两条、^ 被吃掉；
//   - cmdToken 对引号里的元字符也加 ^：^ 在引号里是字面量，程序路径变成另一个不存在的路径；
//   - 程序路径一律加引号（与 exec.Command 的规则不同）：被启动进程的原始命令行与直接启动时不一致；
//   - 参数改用别的引号规则（例如一律加引号）：spotify 按完整参数项判断的保活标记带上引号后认不出。
func TestCommandLine(t *testing.T) {
	flags := []string{
		"--remote-debugging-port=9222",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-background-timer-throttling",
		"--disable-features=CalculateNativeWinOcclusion",
	}
	cases := []struct {
		name string
		exe  string
		args []string
		want string // shellPrefix 之后的部分
	}{
		{"路径含空格与括号：整项加引号，括号在引号里不转义",
			`C:\Program Files (x86)\NetEase\CloudMusic\cloudmusic.exe`, flags,
			`"C:\Program Files (x86)\NetEase\CloudMusic\cloudmusic.exe" --remote-debugging-port=9222 --disable-backgrounding-occluded-windows --disable-renderer-backgrounding --disable-background-timer-throttling --disable-features=CalculateNativeWinOcclusion"`},
		{"路径不含空格：与 exec.Command 一样不加引号",
			`C:\Users\a\AppData\Roaming\Spotify\Spotify.exe`, []string{"--remote-debugging-port=9223"},
			`C:\Users\a\AppData\Roaming\Spotify\Spotify.exe --remote-debugging-port=9223"`},
		{"路径不含空格而含元字符：逐个加 ^",
			`C:\a&b\x^y(z)\KuGou.exe`, nil,
			`C:\a^&b\x^^y^(z^)\KuGou.exe"`},
		{"路径含中文与空格", `D:\网易云 音乐\cloudmusic.exe`, []string{"中文 参数", "--x=中"},
			`"D:\网易云 音乐\cloudmusic.exe" "中文 参数" --x=中"`},
		{"参数里的元字符：不在引号里的加 ^，在引号里的不动",
			`C:\p\x.exe`, []string{"a^b", "p&q", "(r)", "s|t", "u<v>w", "a b&c"},
			`C:\p\x.exe a^^b p^&q ^(r^) s^|t u^<v^>w "a b&c""`},
		{"空参数、末尾反斜杠、制表符：与 syscall.EscapeArg 相同",
			`C:\p\x.exe`, []string{"", `trail\`, `sp ace\`, "\t", "a,b;c=d"},
			`C:\p\x.exe "" trail\ "sp ace\\" "` + "\t" + `" a,b;c=d"`},
		{"! 原样写出，靠 /v:off 保持字面量", `C:\wow!hi!\x.exe`, []string{"!PATH!"},
			`C:\wow!hi!\x.exe !PATH!"`},
		{"没有参数", `C:\p\x.exe`, nil, `C:\p\x.exe"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, viaShell := command(context.Background(), c.exe, c.args, "")
			if !viaShell {
				t.Fatalf("应经 cmd.exe 中转，却退回了直接启动")
			}
			if got := cmd.SysProcAttr.CmdLine; got != shellPrefix+c.want {
				t.Errorf("命令行\n  得到 %s\n  期望 %s", got, shellPrefix+c.want)
			}
		})
	}
}

// TestCommandHidesShell 钉住中转用的 cmd.exe 的启动方式：系统目录下的绝对路径、CREATE_NO_WINDOW 与
// HideWindow 都带上、工作目录即 dir。
//
// 防的缺陷与变异（均实测变红）：去掉 CREATE_NO_WINDOW、去掉 HideWindow（本进程没有控制台时任缺其一都可能
// 闪出 cmd 的黑窗，两个都带时开发机实测不出现任何控制台窗口）；cmd.exe 改为按 PATH 查找（"cmd.exe"）；
// 不设 cmd.Dir（被启动的进程拿不到 dir 作工作目录，酷狗依赖它）。
func TestCommandHidesShell(t *testing.T) {
	cmd, viaShell := command(context.Background(), `C:\p\x.exe`, nil, `C:\work dir`)
	if !viaShell {
		t.Fatal("应经 cmd.exe 中转")
	}
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	// 只看 cmd.Path 不够：exec.Command("cmd.exe") 经 PATH 查找后 Path 同样是系统目录下的那一个。
	if want := filepath.Join(sys, "cmd.exe"); !strings.EqualFold(shell, want) || !strings.EqualFold(cmd.Path, want) {
		t.Errorf("shell = %q、cmd.Path = %q，期望都是系统目录下的 %q", shell, cmd.Path, want)
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Error("cmd.exe 没有带 CREATE_NO_WINDOW")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("cmd.exe 没有带 HideWindow")
	}
	if cmd.Dir != `C:\work dir` {
		t.Errorf("cmd.Dir = %q，期望 dir", cmd.Dir)
	}
}

// TestCommandFallback 钉住退回直接启动的判定：这些输入经 cmd.exe 会被改写或改变含义，必须退回
// exec.Command（参数原样、Dir 即 dir、不带 SysProcAttr）。
//
// 防的缺陷与变异（均实测变红）：去掉 % 的检查（cmd 在引号里照样展开 %变量%，开发机实测）；去掉 " 的检查（打乱 cmd 的
// 引号配对）；去掉控制字符的检查（换行截断命令行）；控制字符的判定把制表符也算进去（见下一个用例）；去掉
// 绝对路径或 .exe 扩展名的检查（start 在 PATH 里查找、按文件关联打开）；去掉 UNC 工作目录的检查（cmd 改用
// Windows 目录作当前目录，实测）。
func TestCommandFallback(t *testing.T) {
	const exe = `C:\p\x.exe`
	cases := []struct {
		name string
		exe  string
		args []string
		dir  string
	}{
		{"参数含 %", exe, []string{"%PATH%"}, ""},
		{"参数含孤立的 %", exe, []string{"100%"}, ""},
		{"路径含 %", `C:\50%off\x.exe`, nil, ""},
		{"参数含引号", exe, []string{`a"b`}, ""},
		{"路径含引号", `C:\a"b\x.exe`, nil, ""},
		{"参数含换行", exe, []string{"a\nb"}, ""},
		{"参数含回车", exe, []string{"a\rb"}, ""},
		{"参数含 NUL", exe, []string{"a\x00b"}, ""},
		{"参数含其他控制字符", exe, []string{"a\x1bb"}, ""},
		{"路径含换行", "C:\\a\nb\\x.exe", nil, ""},
		{"相对路径", `x.exe`, nil, ""},
		{"以反斜杠开头、不带盘符", `\p\x.exe`, nil, ""},
		{"不是 .exe", `C:\p\x.bat`, nil, ""},
		{"没有扩展名", `C:\p\x`, nil, ""},
		{"工作目录是 UNC", exe, nil, `\\server\share\dir`},
		{"工作目录是正斜杠写的 UNC", exe, nil, `//server/share/dir`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, viaShell := command(context.Background(), c.exe, c.args, c.dir)
			if viaShell {
				t.Fatalf("应退回直接启动，却经 cmd.exe 中转：%s", cmd.SysProcAttr.CmdLine)
			}
			if want := append([]string{c.exe}, c.args...); !slices.Equal(cmd.Args, want) {
				t.Errorf("cmd.Args = %q，期望 %q", cmd.Args, want)
			}
			if cmd.Dir != c.dir {
				t.Errorf("cmd.Dir = %q，期望 %q", cmd.Dir, c.dir)
			}
			if cmd.SysProcAttr != nil {
				t.Errorf("直接启动不应带 SysProcAttr：%+v", cmd.SysProcAttr)
			}
		})
	}
}

// TestCommandPassableCounterexamples 是上一个用例的反例：这些输入 cmd.exe 能原样转交，不得退回直接启动
// （退回就保不住播放器）。
//
// 防的缺陷与变异（均实测变红）：控制字符的判定把制表符算进去；把 !、^ & ( )、, ; = 三组之一列为退路字符；
// 扩展名比较区分大小写（.EXE）；UNC 的判定放宽到含空格的目录。
func TestCommandPassableCounterexamples(t *testing.T) {
	cases := []struct {
		name string
		exe  string
		args []string
		dir  string
	}{
		{"制表符", `C:\p\x.exe`, []string{"a\tb"}, ""},
		{"感叹号", `C:\wow!\x.exe`, []string{"!x!"}, ""},
		{"元字符", `C:\a&b\x^y(z)\x.exe`, []string{"p&q", "a^b", "(r)", "s|t", "u<v>w"}, ""},
		{"逗号分号等号", `C:\a=b,c;d\x.exe`, []string{"a,b;c=d"}, ""},
		{"中文与空格", `C:\网易云 音乐\x.exe`, []string{"中文 参数"}, `C:\网易云 音乐`},
		{"大写扩展名", `C:\p\X.EXE`, nil, ""},
		{"空参数", `C:\p\x.exe`, []string{""}, ""},
		{"工作目录在盘符根下", `C:\p\x.exe`, nil, `C:\`},
		{"工作目录含空格与括号", `C:\p\x.exe`, nil, `C:\Program Files (x86)\KuGou`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if cmd, viaShell := command(context.Background(), c.exe, c.args, c.dir); !viaShell {
				t.Errorf("应经 cmd.exe 中转，却退回了直接启动（Args=%q）", cmd.Args)
			}
		})
	}
}
