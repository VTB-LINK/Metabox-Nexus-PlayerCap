//go:build windows

package taskbar

import (
	"runtime"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
)

// 传统 conhost 的任务栏进度后端：ITaskbarList3 COM + FlashWindowEx。见 taskbar.go 顶部说明。

var (
	clsidTaskbarList = ole.NewGUID("{56FDF344-FD6D-11D0-958A-006097C9A090}")
	iidITaskbarList3 = ole.NewGUID("{EA1AFB91-9E28-4B86-90E9-9E9F8A5EEFAF}")
)

// ITaskbarList3 进度状态（TBPFLAG）。
const (
	tbpfNoProgress = 0x0
	tbpfNormal     = 0x2 // 绿
	tbpfError      = 0x4 // 红
	tbpfPaused     = 0x8 // 黄
)

// FlashWindowEx 的 dwFlags：只闪任务栏按钮。
const flashwTray = 0x2

type flashwInfo struct {
	cbSize    uint32
	hwnd      windows.HWND
	dwFlags   uint32
	uCount    uint32
	dwTimeout uint32
}

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procFlashWindowEx    = user32.NewProc("FlashWindowEx")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
)

// 由 consoleInit 填好，后续 consoleSet 复用。
var (
	tbUnk  *ole.IUnknown
	tbHwnd windows.HWND
)

func init() {
	consoleBackendInit = consoleInit
	consoleBackendSet = consoleSet
	consoleBackendFlash = consoleFlash
	consoleBackendDetach = consoleDetach
}

func consoleWindow() windows.HWND {
	h, _, _ := procGetConsoleWindow.Call()
	return windows.HWND(h)
}

// consoleInit 建立 ITaskbarList3。COM 套间有线程亲和，故锁定当前 OS 线程 —— 本函数在
// checkAndUpdate 主 goroutine 内调用，该 goroutine 后续必然 os.Exit（成功 restart / 失败
// exit），不必再 Unlock。任一步失败都干净降级，绝不影响更新本身。
func consoleInit() bool {
	hwnd := consoleWindow()
	if hwnd == 0 {
		return false
	}
	runtime.LockOSThread()
	// S_FALSE / 已初始化都无妨；真失败时下面 CreateInstance 会失败并降级。
	ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED)
	unk, err := ole.CreateInstance(clsidTaskbarList, iidITaskbarList3)
	if err != nil || unk == nil {
		ole.CoUninitialize()
		runtime.UnlockOSThread()
		return false
	}
	// HrInit（vtable[3]）
	syscall.SyscallN(vtable(unk)[3], uintptr(unsafe.Pointer(unk)))
	tbUnk = unk
	tbHwnd = hwnd
	return true
}

func consoleSet(state, pct int) {
	if tbUnk == nil {
		return
	}
	var flag uintptr
	switch state {
	case tbStateNormal:
		flag = tbpfNormal
	case tbStateError:
		flag = tbpfError
	case tbStateWarning:
		flag = tbpfPaused
	default:
		flag = tbpfNoProgress
	}
	// SetProgressState（vtable[10]）
	syscall.SyscallN(vtable(tbUnk)[10], uintptr(unsafe.Pointer(tbUnk)), uintptr(tbHwnd), flag)
	// 除清除外都写进度值：normal 用实际 pct，error/warning 传满格让红/黄铺满整条、醒目可见
	// （长度为 0 几乎看不出颜色）。SetProgressValue（vtable[9]）：completed / total 为
	// ULONGLONG，amd64 下各占一个 uintptr。
	if state != tbStateClear {
		if pct < 0 {
			pct = 0
		} else if pct > 100 {
			pct = 100
		}
		syscall.SyscallN(vtable(tbUnk)[9], uintptr(unsafe.Pointer(tbUnk)), uintptr(tbHwnd), uintptr(pct), uintptr(100))
	}
}

func consoleFlash() {
	hwnd := consoleWindow()
	if hwnd == 0 {
		return
	}
	fi := flashwInfo{
		hwnd:    hwnd,
		dwFlags: flashwTray,
		uCount:  3,
	}
	fi.cbSize = uint32(unsafe.Sizeof(fi))
	procFlashWindowEx.Call(uintptr(unsafe.Pointer(&fi)))
}

// consoleDetach 释放 ITaskbarList3 与被锁定的 OS 线程，配对 consoleInit。任务栏当前进度色
// 由 shell 按窗口句柄维持，释放后仍显示。
func consoleDetach() {
	if tbUnk == nil {
		return
	}
	tbUnk.Release()
	tbUnk = nil
	ole.CoUninitialize()
	runtime.UnlockOSThread()
}

// vtable 取 COM 对象的方法指针数组。COM 对象首字段即 vtable 指针。
func vtable(unk *ole.IUnknown) *[16]uintptr {
	return (*[16]uintptr)(unsafe.Pointer(unk.RawVTable))
}
