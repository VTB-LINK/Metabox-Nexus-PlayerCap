// Package restartmgr 经 Restart Manager 只读地查询打开给定文件的进程。
package restartmgr

import (
	"errors"
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

// Holders 在一个 Restart Manager 会话里注册 paths 的全部文件，返回打开其中任一文件的进程的 PID（可能为空）。
// paths 为空时返回 nil, nil，不开会话。只查询：不调用 RmShutdown、RmRestart，不影响任何进程；会话在返回前
// 结束。错误包装 RM 返回的 syscall.Errno，可用 IsFatal 分类。
func Holders(paths ...string) ([]uint32, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	files := make([]*uint16, len(paths))
	for i, path := range paths {
		p, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, err
		}
		files[i] = p
	}

	var session uint32
	var key [rmSessionKeyLen + 1]uint16
	if r, _, _ := procRmStartSession.Call(uintptr(unsafe.Pointer(&session)), 0, uintptr(unsafe.Pointer(&key[0]))); r != 0 {
		return nil, i18n.Errorf("RmStartSession failed: %w", syscall.Errno(r))
	}
	defer procRmEndSession.Call(uintptr(session))

	if r, _, _ := procRmRegisterResources.Call(uintptr(session), uintptr(len(files)), uintptr(unsafe.Pointer(&files[0])), 0, 0, 0, 0); r != 0 {
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

// HoldersOf 即 Holders(path)，供只查一个文件的调用方（如酷狗确认闸门的 holders 字段）。
func HoldersOf(path string) ([]uint32, error) { return Holders(path) }

// IsFatal 报告 err 是否为重试无益的 Restart Manager 错误：ERROR_SEM_TIMEOUT（拿不到注册表互斥体）、
// ERROR_WRITE_FAULT（RM 读写注册表失败）、ERROR_MAX_SESSIONS_REACHED（本用户会话的 RM 会话已满）。
// 按 errors.Is 判断，经 %w 包装的同样认得。
func IsFatal(err error) bool {
	return errors.Is(err, windows.ERROR_SEM_TIMEOUT) ||
		errors.Is(err, windows.ERROR_WRITE_FAULT) ||
		errors.Is(err, windows.ERROR_MAX_SESSIONS_REACHED)
}
