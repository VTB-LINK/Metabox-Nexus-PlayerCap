// Package launch 拉起播放器进程，使其不在 PlayerCap 的进程树里（issue #127）。
//
// OBS 的 Autostarter 插件在 OBS 退出时，按父进程 ID 递归结束它所启动的进程（PlayerCap）的全部子进程，
// 再结束 PlayerCap。PlayerCap 为补调试参数而重启的播放器若由 exec.Command(...).Start() 直接启动，父进程
// 就是 PlayerCap，会被一并结束。经 cmd.exe 的 start 中转后，播放器的父进程是随即退出的 cmd.exe，不在
// PlayerCap 的子树里。
//
// 不用 SysProcAttr.ParentProcess 改挂到别的父进程下：以 explorer.exe 为父进程在开发机上报拒绝访问。
//
// 局限：只脱离按父进程 ID 维护的进程树。PlayerCap 若在 Job Object 里，经 start 启动的进程同样属于该
// Job（开发机实测），Job 带 KILL_ON_JOB_CLOSE 时照样被一并结束。被启动的须是 GUI 程序：控制台程序经
// start 会得到一个新的可见控制台窗口。
package launch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"Metabox-Nexus-PlayerCap/i18n"

	"golang.org/x/sys/windows"
)

// shell 是中转用的 cmd.exe，取系统目录下的绝对路径，不经 PATH 查找。测试替换它以模拟卡住或出错的 cmd。
var shell = systemCmd()

// waitLimit 是等 cmd.exe 退出的上限，超过即结束 cmd 并报错。start 拉起 GUI 程序后立即返回；会卡住的
// 情形是 start 弹出了错误框（例如程序不存在）：cmd 带 HideWindow，错误框不显示，cmd 一直等它被关掉。
var waitLimit = 10 * time.Second

// getwd 取本进程的当前目录；测试替换它以模拟 UNC 当前目录。
var getwd = os.Getwd

// held 按播放器（小写的 exe 路径）存中转进程句柄：同一播放器再次拉起时，只关闭那些已没有存活子进程（它们的
// 播放器已退出）的旧句柄，上一个实例若还活着（例如结束它失败了）就保留它的句柄；别的播放器的不动：播放器 A 反复
// 重启不能撤掉仍在运行的播放器 B 的保护。活着的旧实例各占一个句柄，正常情况下每个播放器只有一个。
var (
	heldMu sync.Mutex
	held   = map[string][]windows.Handle{}
)

// holdRelay 持有中转 cmd.exe 的进程句柄。播放器的父进程 ID 就是中转进程的 PID，进程对象一释放 PID 即可被复用
// （开发机连续起停 300 个短命进程，PID 出现了 5 次复用）：若被 PlayerCap 之后启动的子进程（音频 helper 等）复用
// 并存活到 OBS 退出，Autostarter 会沿 PlayerCap → 该子进程 → 播放器递归结束播放器。持有句柄，PID 就不会被复用。
// 须在 cmd.Wait 之前调用：此时 os/exec 仍持有它自己的句柄，PID 一定还没被复用。
// shortcut: OpenProcess 失败时静默跳过，只是少这一层防护，不影响拉起；要查失败原因时再加日志。
func holdRelay(exePath string, pid int) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return
	}
	key := strings.ToLower(exePath) // Windows 路径不分大小写
	heldMu.Lock()
	defer heldMu.Unlock()
	var kept []windows.Handle
	for _, old := range held[key] {
		if hasLiveChild(old) {
			kept = append(kept, old)
		} else {
			windows.CloseHandle(old)
		}
	}
	held[key] = append(kept, h)
}

// hasLiveChild 报告以 h 所指进程为父进程的进程是否还有存活的。h 持有期间该 PID 不会被复用，快照里
// ParentProcessID 等于它的就是它启动的进程（中转 cmd 经 start 起的播放器）。
// shortcut: 只按父进程 ID 判断，之前用过同一 PID 的已退出进程留下的、仍在运行的子进程会被误算进来，多留一个
// 句柄；快照失败也按存活处理。两者都只是多留，不会少保护。
func hasLiveChild(h windows.Handle) bool {
	id, err := windows.GetProcessId(h)
	if err != nil {
		return true
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return true
	}
	defer windows.CloseHandle(snap)
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err := windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ParentProcessID == id {
			return true
		}
	}
	return false
}

func systemCmd() string {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		dir = filepath.Join(os.Getenv("SystemRoot"), "System32")
	}
	return filepath.Join(dir, "cmd.exe")
}

// Detached 启动 exePath，参数为 args，dir 非空时为工作目录；被启动的进程不在本进程的进程树里。
//
// detached=false 且 err=nil 表示路径、参数或工作目录不能经 cmd.exe 原样转交（见 passable），已退回
// 直接启动：进程照常运行，但它是本进程的子进程，调用方据此打一条 Warn。出错时 detached 为 false。
//
// 中转 cmd.exe 的进程句柄由 holdRelay 按播放器持有，使播放器记着的父进程 ID 不被复用；同一 exePath 再次调用时，
// 已没有存活子进程的旧句柄被关闭，旧实例还活着的保留。
//
// 经 cmd.exe 中转之前先确认 exePath 是存在的普通文件：程序不存在时 start 弹出错误框并阻塞（开发机实测），
// 是目录时 start 按关联打开它（推断，未实测）。
func Detached(exePath string, args []string, dir string) (detached bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	cmd, viaShell := command(ctx, exePath, args, dir)
	if !viaShell {
		return false, cmd.Start()
	}
	st, err := os.Stat(exePath)
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() {
		return false, i18n.Errorf("%s is not a regular file", exePath)
	}
	if err := cmd.Start(); err != nil {
		return false, i18n.Errorf("start cmd.exe: %w", err)
	}
	holdRelay(exePath, cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return false, i18n.Errorf("cmd.exe did not exit within %s while starting %s; stopped it", waitLimit, exePath)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false, i18n.Errorf("cmd.exe exited with code %d while starting %s", ee.ExitCode(), exePath)
		}
		return false, i18n.Errorf("start cmd.exe: %w", err)
	}
	return true, nil
}

// command 构造拉起 exePath 的命令，不启动它；viaShell 报告是否经 cmd.exe 中转。
//
// 中转的命令行是 cmd.exe /d /v:off /s /c "start "" <exe> <args...>"：/d 不执行注册表里的 AutoRun，
// /v:off 不做 !变量! 的延迟展开（注册表可把它设为默认开启），/s 只剥去 /c 之后最外层的一对引号，
// start 的第一个带引号的项是窗口标题，故先给一个空标题。cmd 带 CREATE_NO_WINDOW 与 HideWindow：
// 本进程没有控制台时，不带这两个标志会闪出 cmd 的控制台窗口，两个都带时不出现任何控制台窗口。
// 被启动的进程拿到的 STARTUPINFO 由 start 另行填写，不继承 cmd 的 HideWindow；它的原始命令行与直接
// 启动时相比，程序路径之后多一个空格（没有参数时是末尾多一个空格），其余逐字相同（以上均为开发机实测）。
func command(ctx context.Context, exePath string, args []string, dir string) (cmd *exec.Cmd, viaShell bool) {
	// dir 为空时被启动的进程继承本进程的当前目录，cmd 同样不接受 UNC 的当前目录，故按实际的目录判定。
	// shortcut: 取不到当前目录时按可转交处理，直接启动在这种情形下同样没有确定的工作目录。
	eff := dir
	if eff == "" {
		eff, _ = getwd()
	}
	if !passable(exePath, args, eff) {
		// 不带 ctx：带了的话，Detached 返回时 ctx 取消，刚起来的播放器会被结束。
		cmd = exec.Command(exePath, args...)
		cmd.Dir = dir
		return cmd, false
	}
	var b strings.Builder
	b.WriteString(`cmd.exe /d /v:off /s /c "start "" `)
	b.WriteString(cmdToken(exePath))
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(cmdToken(a))
	}
	b.WriteByte('"')
	cmd = exec.CommandContext(ctx, shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: b.String(), HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	cmd.Dir = dir // start 启动的进程继承 cmd 的当前目录
	return cmd, true
}

// passable 报告能否经 cmd.exe 原样转交：
//   - exePath 是绝对路径、扩展名为 .exe：其余情形 start 会在 PATH 里查找或按文件关联打开，与 CreateProcess 不同；
//   - 路径与参数里没有 %（cmd 在引号里照样展开 %变量%）、没有 "（打乱 cmd 的引号配对，元字符随之失去保护）、
//     没有制表符以外的控制字符（换行截断命令行，NUL 放不进命令行）；
//   - dir 不是 UNC 路径：cmd 不接受 UNC 作当前目录，改用 Windows 目录，被启动的进程随之拿到别的工作目录。
func passable(exePath string, args []string, dir string) bool {
	if !filepath.IsAbs(exePath) || !strings.EqualFold(filepath.Ext(exePath), ".exe") {
		return false
	}
	if strings.HasPrefix(dir, `\\`) || strings.HasPrefix(dir, "//") {
		return false
	}
	bad := func(r rune) bool { return r == '%' || r == '"' || (r < 0x20 && r != '\t') }
	if strings.ContainsFunc(exePath, bad) {
		return false
	}
	for _, a := range args {
		if strings.ContainsFunc(a, bad) {
			return false
		}
	}
	return true
}

// cmdToken 先按 syscall.EscapeArg 写一项（与 exec.Command 拼命令行的规则相同，被启动的进程解析出的参数
// 不变），不在引号里的 cmd 元字符再各加一个 ^：cmd 解析时去掉 ^，start 拿到的仍是 EscapeArg 的结果。
// 经 passable 过滤后没有 "，EscapeArg 的结果要么整项在一对引号里（含空格或制表符时），要么不含引号。
func cmdToken(s string) string {
	e := syscall.EscapeArg(s)
	if strings.HasPrefix(e, `"`) {
		return e // 引号里的元字符对 cmd 是字面量
	}
	var b strings.Builder
	for _, r := range e {
		if strings.ContainsRune(`^&|<>()`, r) {
			b.WriteByte('^')
		}
		b.WriteRune(r)
	}
	return b.String()
}
