// Package procscan 发现在跑的播放器进程：Toolhelp 快照枚举、同名集合里的主进程定位、
// PID 身份复核（防复用 TOCTOU）。MainPID/Proc 无 windows 依赖，纯 Go 可测；
// 快照枚举与身份复核在 procscan_windows.go，进程名清单在 names_windows.go。
package procscan

import "strings"

// Proc 是进程快照里的一条记录。
type Proc struct {
	PID  uint32
	Name string
	PPID uint32
	// CreateTime 为进程创建时间（Windows 文件时间纳秒）。
	// Phase 1 未逐进程采集（避免每 tick 全表 OpenProcess）；如需按创建时间强化 PID 复用防护，由调用方对候选 PID 单独取。
	CreateTime int64
}

// MainPID 在 procs 里挑出名为 name（大小写不敏感）的同名进程集合的「主进程」：
// 其父进程不在同名集合中的那个——Chromium/Electron 主进程无 --type=，其父不是同名浏览器；
// 而子进程的父是主进程、落在同名集合内。多个候选取 PID 最小（稳定）。
// 找不到任何同名进程返回 (0, false)。
func MainPID(procs []Proc, name string) (uint32, bool) {
	same := make(map[uint32]bool, len(procs))
	for _, p := range procs {
		if strings.EqualFold(p.Name, name) {
			same[p.PID] = true
		}
	}
	if len(same) == 0 {
		return 0, false
	}
	var main, any uint32
	mainFound, anyFound := false, false
	for _, p := range procs {
		if !strings.EqualFold(p.Name, name) {
			continue
		}
		if !anyFound || p.PID < any {
			any, anyFound = p.PID, true
		}
		if same[p.PPID] {
			continue // 父也是同名 → 是子进程
		}
		if !mainFound || p.PID < main {
			main, mainFound = p.PID, true
		}
	}
	if mainFound {
		return main, true
	}
	// 同名进程互为父子成环（真实快照不会发生）——退化取 PID 最小者，避免误判无进程。
	return any, anyFound
}
