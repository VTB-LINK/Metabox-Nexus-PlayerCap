package watchdog

import (
	"syscall"
	"unsafe"

	"Metabox-Nexus-PlayerCap/i18n"
	"golang.org/x/sys/windows"
)

var (
	modRstrtmgr             = windows.NewLazySystemDLL("rstrtmgr.dll")
	procRmStartSession      = modRstrtmgr.NewProc("RmStartSession")
	procRmRegisterResources = modRstrtmgr.NewProc("RmRegisterResources")
	procRmGetList           = modRstrtmgr.NewProc("RmGetList")
	procRmEndSession        = modRstrtmgr.NewProc("RmEndSession")
)

const (
	rmSessionKeyLen = 32 // CCH_RM_SESSION_KEY
	rmMaxAppName    = 255
	rmMaxSvcName    = 63
	// rmGetListRetries 是 RmGetList 返回 ERROR_MORE_DATA（两次调用之间占用者变多）后至多重取的次数。
	rmGetListRetries = 3
)

// rmProcessInfo 对应 RM_PROCESS_INFO（x64 下 668 字节，各字段 4 字节对齐）；开头两个字段即 RM_UNIQUE_PROCESS。
// https://learn.microsoft.com/en-us/windows/win32/api/restartmanager/ns-restartmanager-rm_process_info
type rmProcessInfo struct {
	ProcessID        uint32
	ProcessStartTime windows.Filetime
	AppName          [rmMaxAppName + 1]uint16
	ServiceShortName [rmMaxSvcName + 1]uint16
	ApplicationType  uint32
	AppStatus        uint32
	TSSessionID      uint32
	Restartable      int32
}

// rmHolders 经 Restart Manager 查询打开 path 的进程，返回其 PID（可能为空）。只查询：不调用 RmShutdown、
// RmRestart，不影响任何进程；会话在返回前结束。
func rmHolders(path string) ([]uint32, error) {
	var session uint32
	var key [rmSessionKeyLen + 1]uint16
	if r, _, _ := procRmStartSession.Call(uintptr(unsafe.Pointer(&session)), 0, uintptr(unsafe.Pointer(&key[0]))); r != 0 {
		return nil, i18n.Errorf("RmStartSession failed: %w", syscall.Errno(r))
	}
	defer procRmEndSession.Call(uintptr(session))

	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	files := []*uint16{p}
	if r, _, _ := procRmRegisterResources.Call(uintptr(session), 1, uintptr(unsafe.Pointer(&files[0])), 0, 0, 0, 0); r != 0 {
		return nil, i18n.Errorf("RmRegisterResources failed: %w", syscall.Errno(r))
	}

	var infos []rmProcessInfo
	for retry := 0; ; retry++ {
		var needed, reasons uint32
		n := uint32(len(infos))
		var first *rmProcessInfo
		if len(infos) > 0 {
			first = &infos[0]
		}
		r, _, _ := procRmGetList.Call(uintptr(session), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&n)),
			uintptr(unsafe.Pointer(first)), uintptr(unsafe.Pointer(&reasons)))
		if r == 0 {
			pids := make([]uint32, 0, n)
			for _, info := range infos[:min(int(n), len(infos))] {
				pids = append(pids, info.ProcessID)
			}
			return pids, nil
		}
		if syscall.Errno(r) != windows.ERROR_MORE_DATA || retry == rmGetListRetries {
			return nil, i18n.Errorf("RmGetList failed: %w", syscall.Errno(r))
		}
		infos = make([]rmProcessInfo, needed)
	}
}
