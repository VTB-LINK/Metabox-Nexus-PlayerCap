//go:build windows

package procscan

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Snapshot 用 CreateToolhelp32Snapshot 一次枚举所有进程，返回 PID/Name/PPID。
func Snapshot() ([]Proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		return nil, err
	}

	var procs []Proc
	for {
		procs = append(procs, Proc{
			PID:  pe.ProcessID,
			Name: windows.UTF16ToString(pe.ExeFile[:]),
			PPID: pe.ParentProcessID,
		})
		if err := windows.Process32Next(snap, &pe); err != nil {
			if err == windows.ERROR_NO_MORE_FILES {
				break
			}
			return procs, err
		}
	}
	return procs, nil
}

// AliveAny 复核 pid 在当前快照里仍存在且映像名精确匹配 names 中任一非空项（大小写不敏感），防 PID 复用 TOCTOU。
// 忽略空串名称，无有效名称时返回 false。
func AliveAny(pid uint32, names ...string) bool {
	hasName := false
	for _, n := range names {
		if n != "" {
			hasName = true
			break
		}
	}
	if !hasName {
		return false
	}
	procs, err := Snapshot()
	if err != nil {
		return false
	}
	for _, p := range procs {
		if p.PID == pid {
			return matchesAny(p.Name, names)
		}
	}
	return false
}

// Alive 复核 pid 在当前快照里仍存在且名字匹配（大小写不敏感），防 PID 复用 TOCTOU。
// 空串返回 false。
func Alive(pid uint32, name string) bool {
	return AliveAny(pid, name)
}
