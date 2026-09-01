package taskbar

import (
	"fmt"
	"os"
)

// 任务栏更新进度。
//
// 本程序是控制台子系统应用（AGENTS.md §0.5），自身没有窗口 —— 任务栏上那个图标属于
// 承载它的终端。因此「在任务栏图标上画进度条」要按终端分两条机制：
//
//   - Windows Terminal / ConEmu：识别 ConEmu 定义的 OSC 9;4 转义序列，往 stdout 写字节
//     即可。这类终端下 GetConsoleWindow 返回 ConPTY 伪窗口，Win32 窗口 API 够不到任务栏，
//     只能走转义序列。
//   - 传统 conhost（双击 exe 的黑框）：不认转义序列，但 GetConsoleWindow 返回真顶层窗口
//     （就是任务栏上显示本程序图标那个），用 ITaskbarList3 COM 画进度、FlashWindowEx 闪烁。
//     见 taskbar_windows.go。
//
// 识别不了的终端一律降级为空操作，绝不往终端吐裸序列污染下载进度那行文本（AGENTS.md §0：
// 宁可降级）。全部调用发生在启动期 checkAndUpdate 的同一 goroutine，无并发。

// 进度状态。数值对齐 OSC 9;4 的 state 取值；conhost 后端另映射到 TBPFLAG。
const (
	tbStateClear   = 0 // 清除
	tbStateNormal  = 1 // 绿色，带百分比
	tbStateError   = 2 // 红色
	tbStateWarning = 4 // 黄色
)

type taskbarMode int

const (
	tbNop     taskbarMode = iota // 不支持的终端，空操作
	tbOSC                        // Windows Terminal / ConEmu
	tbConsole                    // 传统 conhost，经 COM
)

var tbMode = tbNop

// conhost 后端（ITaskbarList3 COM 实现）由 taskbar_windows.go 在 Windows 下注入；
// 其他平台保持空操作，使本文件可跨平台编译。
var (
	consoleBackendInit   = func() bool { return false } // 初始化 COM，成功返回 true
	consoleBackendSet    = func(state, pct int) {}      // 设进度状态/值
	consoleBackendFlash  = func() {}                    // 闪烁任务栏图标
	consoleBackendDetach = func() {}                    // 释放 COM/线程，保留当前颜色
)

// Init 探测终端并选定后端。多次调用只有第一次生效：下载路径在下载前调，
// 「连不上网关」路径也调，两条互斥执行，guard 只是防御性幂等。
var tbInited bool

func Init() {
	if tbInited {
		return
	}
	tbInited = true
	if supportsOSC() {
		tbMode = tbOSC
		return
	}
	if consoleBackendInit() {
		tbMode = tbConsole
	}
}

// supportsOSC 报告当前终端是否识别 OSC 9;4：Windows Terminal 注入 WT_SESSION，
// ConEmu 注入 ConEmuANSI=ON。
func supportsOSC() bool {
	return os.Getenv("WT_SESSION") != "" || os.Getenv("ConEmuANSI") == "ON"
}

func Progress(pct int) { taskbarSet(tbStateNormal, pct) }
func Error()           { taskbarSet(tbStateError, 100) }   // 满格红，否则长度 0 看不出颜色
func Warning()         { taskbarSet(tbStateWarning, 100) } // 满格黄，同上
func Clear()           { taskbarSet(tbStateClear, 0) }

func taskbarSet(state, pct int) {
	switch tbMode {
	case tbOSC:
		// 与下载进度同走 stdout，避免和 stderr 日志交错。
		fmt.Fprintf(os.Stdout, "\x1b]9;4;%d;%d\a", state, pct)
	case tbConsole:
		consoleBackendSet(state, pct)
	}
}

// Flash 闪烁任务栏图标提示更新完成。仅传统 conhost 有效（FlashWindowEx 作用于
// GetConsoleWindow 的真窗口）；Windows Terminal 下是伪窗口，调用无害但无视觉效果，
// 此时任务栏停在 Warning 的黄色。
func Flash() {
	if tbMode != tbNop {
		consoleBackendFlash()
	}
}

// Detach 释放 conhost 后端占用的 COM 与被锁定的 OS 线程，但保留任务栏当前颜色
// （进度状态绑定窗口句柄、由 shell 维持，释放 COM 对象不影响显示）。用于「连不上网关→标红」
// 这类设完终态还要继续跑服务的路径，避免 consoleInit 的 LockOSThread 泄漏到 server 阶段。
// 下载成功/失败路径不需要它——那两条都直接 os.Exit。
func Detach() {
	if tbMode == tbConsole {
		consoleBackendDetach()
	}
}
