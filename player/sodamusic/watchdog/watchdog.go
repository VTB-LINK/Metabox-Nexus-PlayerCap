// Package watchdog 负责给汽水音乐（SodaMusic.exe）主进程开出 Node inspector（9229）。
//
// 汽水音乐是 Electron，有原生反调试：启动 argv 里带 `--remote-debugging-port` 会被它在 ~2s 内
// 自杀。所以 cloudmusic 那套「杀掉重启 + 加 argv」在这里**不通**。
//
// 绕过手段 = 复刻 Node 的 `process._debugProcess(pid)`：目标 Node/Electron 主进程启动时会建一个
// 命名映射 `node-debug-handler-<pid>`，内含一个指针 = 目标地址空间里 StartIoThreadWrapper 的函数
// 地址。激活 = 读出该地址 → `CreateRemoteThread` 到目标 → 目标自己拉起 inspector I/O 线程（默认
// 9229）。这条路**不碰 argv**，反调试扫不到；实测 inspector 开着后长会话稳定，反调试不检测 9229。
//
// **非破坏性**（AGENTS §0.1 红线）：只读命名映射 + 建一个远程线程去跑目标**自带**的激活函数，
// 不改汽水音乐任何内存/状态。映射不存在（目标非 Node / 禁用了 debug handler）→ 返回错误让上层
// 降级，绝不盲写。
package watchdog

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unsafe"

	"Metabox-Nexus-PlayerCap/logger"

	"github.com/shirou/gopsutil/v3/process"
	"golang.org/x/sys/windows"
)

var log = logger.New("Soda] [Watchdog") // 渲染为 [Soda] [Watchdog]

// TargetProcessName 汽水音乐进程名。
const TargetProcessName = "SodaMusic.exe"

// InspectorPort Node inspector 的默认端口（_debugProcess 激活后固定开在这）。
const InspectorPort = 9229

// _debugProcess 复刻所需的 kernel32 过程。提为包级 var（syscall 过程句柄不应在轮询路径反复创建）。
var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procOpenFileMappingW   = kernel32.NewProc("OpenFileMappingW")
	procCreateRemoteThread = kernel32.NewProc("CreateRemoteThread")
	procRtlMoveMemory      = kernel32.NewProc("RtlMoveMemory")
)

const fileMapRead = 0x0004

// procAccess 是 CreateRemoteThread 所需的最小 OpenProcess 访问掩码。
const procAccess = windows.PROCESS_CREATE_THREAD |
	windows.PROCESS_QUERY_INFORMATION |
	windows.PROCESS_VM_OPERATION |
	windows.PROCESS_VM_WRITE |
	windows.PROCESS_VM_READ

// FindMainPID 找汽水音乐的**主进程**（Electron browser 进程）——即命令行里**没有** `--type=`
// 的那个 SodaMusic.exe。渲染器/GPU/utility 子进程都带 `--type=`，Node inspector 只在主进程上。
//
// 找不到（没运行）返回 err；调用方据此报「未启动」并降级。
func FindMainPID() (int, error) {
	procs, err := process.Processes()
	if err != nil {
		return 0, fmt.Errorf("枚举进程失败: %w", err)
	}
	for _, p := range procs {
		name, err := p.Name()
		if err != nil || !strings.EqualFold(name, TargetProcessName) {
			continue
		}
		// 主进程的命令行不含 --type=（子进程都带）。Cmdline 偶尔取不到（权限）→ 跳过该候选。
		cmd, err := p.Cmdline()
		if err != nil {
			continue
		}
		if !strings.Contains(cmd, "--type=") {
			return int(p.Pid), nil
		}
	}
	return 0, fmt.Errorf("未找到汽水音乐主进程（%s 未运行？）", TargetProcessName)
}

// InspectorUp 报告 9229 是否已可用（能取到 /json/list）。
func InspectorUp() bool {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", InspectorPort))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// EnsureInspector 确保 pid 对应主进程的 Node inspector（9229）已开。
//   - 已开（9229 可用）→ 直接返回 nil。
//   - 未开 → 复刻 _debugProcess 激活，再轮询等它起来。
//
// 返回 nil 表示 9229 已就绪可连。任何一步失败都返回错误（不 panic），让上层降级重试。
func EnsureInspector(pid int) error {
	if InspectorUp() {
		return nil
	}
	if err := activateInspector(pid); err != nil {
		return err
	}
	// 激活是异步的（远程线程去拉起 I/O 线程），轮询等 9229 就绪。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if InspectorUp() {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("已激活 inspector 但 9229 迟迟未就绪（pid=%d）", pid)
}

// activateInspector 复刻 process._debugProcess(pid) 的 Windows 机制。
func activateInspector(pid int) error {
	name := fmt.Sprintf("node-debug-handler-%d", pid)
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return fmt.Errorf("映射名编码失败: %w", err)
	}

	// OpenFileMappingW(FILE_MAP_READ, FALSE, name)：目标启动时建的命名映射。
	r, _, callErr := procOpenFileMappingW.Call(uintptr(fileMapRead), 0, uintptr(unsafe.Pointer(namePtr)))
	if r == 0 {
		return fmt.Errorf("打开命名映射 %q 失败（目标非 Node/Electron 或禁用了 inspector）: %v", name, callErr)
	}
	mapping := windows.Handle(r)
	defer windows.CloseHandle(mapping)

	// MapViewOfFile → 读出 handler 函数地址（目标地址空间内的 StartIoThreadWrapper）。
	addr, err := windows.MapViewOfFile(mapping, fileMapRead, 0, 0, 0)
	if err != nil || addr == 0 {
		return fmt.Errorf("映射视图失败: %v", err)
	}
	defer windows.UnmapViewOfFile(addr)
	// 用 RtlMoveMemory 把 8 字节指针拷进 Go 缓冲再读——避免 unsafe.Pointer(uintptr) 触发 vet
	// unsafeptr（addr 作 syscall 源参数是安全的；映射内存 OS 管理、GC 不会移动它）。
	var buf [8]byte
	procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&buf[0])), addr, unsafe.Sizeof(buf))
	handlerAddr := uintptr(binary.LittleEndian.Uint64(buf[:]))
	if handlerAddr == 0 {
		return fmt.Errorf("handler 地址为空（映射内容异常）")
	}

	// OpenProcess + CreateRemoteThread(handler)：让目标自己跑激活函数。
	proc, err := windows.OpenProcess(procAccess, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("打开目标进程失败: %w", err)
	}
	defer windows.CloseHandle(proc)

	var tid uint32
	th, _, callErr2 := procCreateRemoteThread.Call(
		uintptr(proc), 0, 0, handlerAddr, 0, 0, uintptr(unsafe.Pointer(&tid)))
	if th == 0 {
		return fmt.Errorf("CreateRemoteThread 失败: %v", callErr2)
	}
	windows.CloseHandle(windows.Handle(th))
	log.Info("已对汽水音乐主进程(pid=%d)激活 Node inspector", pid)
	return nil
}
