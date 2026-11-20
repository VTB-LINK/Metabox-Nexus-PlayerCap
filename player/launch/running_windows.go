package launch

import (
	"errors"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Proc 是按映像名找到的一个运行中进程。
type Proc struct {
	PID     uint32
	ExePath string // 映像的完整路径；读不到（无权打开、已退出）为空串，交 Gate 按路径未知拒绝
}

// Running 以 Toolhelp 快照按映像名（不分大小写）找出全部运行中的进程，逐个读映像路径（issue #212）。
//
// 按名字枚举不打开进程：读不到映像路径的同名进程同样在结果里，而 taskkill /IM 按名字结束的正是全部同名进程。
// 不用 gopsutil 枚举：它的 Name() 取自映像路径，读不到映像路径的进程连名字也取不到，会被漏掉。快照取不到或中途
// 出错时返回错误，不返回半份列表。
func Running(name string) ([]Proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	var out []Proc
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if strings.EqualFold(windows.UTF16ToString(e.ExeFile[:]), name) {
			out = append(out, Proc{PID: e.ProcessID, ExePath: exePathOf(e.ProcessID)})
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return out, nil
}

// exePathOf 以 PROCESS_QUERY_LIMITED_INFORMATION 打开 pid 读映像的完整路径，读不到返回空串。
func exePathOf(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}
