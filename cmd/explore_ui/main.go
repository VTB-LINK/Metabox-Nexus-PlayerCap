package main

import (
	"fmt"
	"syscall"
	"unicode/utf16"
	"unsafe"
	"Metabox-Nexus-WesingCap/proc"
)

var (
	user32DLL           = syscall.NewLazyDLL("user32.dll")
	pEnumChildWindows   = user32DLL.NewProc("EnumChildWindows")
	pGetClassName       = user32DLL.NewProc("GetClassNameW")
	pGetWindowTextW     = user32DLL.NewProc("GetWindowTextW")
	pGetWindowTextLen   = user32DLL.NewProc("GetWindowTextLengthW")
	pIsWindowVisible    = user32DLL.NewProc("IsWindowVisible")
	pGetWindowRect      = user32DLL.NewProc("GetWindowRect")
	pGetWindowLong      = user32DLL.NewProc("GetWindowLongW")
)

const GWL_STYLE = -16

type RECT struct {
	Left, Top, Right, Bottom int32
}

var allChildren []uintptr

func enumChildProc(hwnd uintptr, lParam uintptr) uintptr {
	allChildren = append(allChildren, hwnd)
	return 1
}

func getClassNameStr(hwnd uintptr) string {
	buf := make([]uint16, 256)
	pGetClassName.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
	return syscall.UTF16ToString(buf)
}

func getWindowTitle(hwnd uintptr) string {
	length, _, _ := pGetWindowTextLen.Call(hwnd)
	if length == 0 {
		return ""
	}
	buf := make([]uint16, length+1)
	pGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), length+1)
	return string(utf16.Decode(buf[:length]))
}

func isVisible(hwnd uintptr) bool {
	ret, _, _ := pIsWindowVisible.Call(hwnd)
	return ret != 0
}

func getRect(hwnd uintptr) RECT {
	var r RECT
	pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}

func getStyle(hwnd uintptr) uint32 {
	ret, _, _ := pGetWindowLong.Call(hwnd, uintptr(0xFFFFFFF0)) // GWL_STYLE = -16
	return uint32(ret)
}

func main() {
	pid, err := proc.FindProcess("WeSing.exe")
	if err != nil {
		fmt.Println("WeSing not found:", err)
		return
	}
	fmt.Printf("WeSing PID: %d\n\n", pid)

	windows := proc.EnumProcessWindows(pid)
	fmt.Printf("=== Top-Level Windows (%d) ===\n", len(windows))
	for _, w := range windows {
		vis := "hidden"
		if isVisible(w.Handle) {
			vis = "VISIBLE"
		}
		r := getRect(w.Handle)
		cls := getClassNameStr(w.Handle)
		style := getStyle(w.Handle)
		fmt.Printf("  HWND=0x%X [%s] class=%q title=%q style=0x%08X rect=(%d,%d)-(%d,%d)\n",
			w.Handle, vis, cls, w.Title, style, r.Left, r.Top, r.Right, r.Bottom)

		// Enumerate child windows
		allChildren = nil
		cb := syscall.NewCallback(enumChildProc)
		pEnumChildWindows.Call(w.Handle, cb, 0)

		if len(allChildren) > 0 {
			fmt.Printf("    Children: %d\n", len(allChildren))
			limit := 50
			for j, ch := range allChildren {
				if j >= limit {
					fmt.Printf("    ... and %d more\n", len(allChildren)-limit)
					break
				}
				chCls := getClassNameStr(ch)
				chTitle := getWindowTitle(ch)
				chVis := "hidden"
				if isVisible(ch) {
					chVis = "VIS"
				}
				chR := getRect(ch)
				chStyle := getStyle(ch)
				fmt.Printf("    [%d] 0x%X [%s] cls=%q text=%q style=0x%08X (%d,%d %dx%d)\n",
					j, ch, chVis, chCls, chTitle, chStyle,
					chR.Left, chR.Top, chR.Right-chR.Left, chR.Bottom-chR.Top)
			}
		}
		fmt.Println()
	}
}
