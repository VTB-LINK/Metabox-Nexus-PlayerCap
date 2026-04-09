package proc

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// ============================================================================
// Windows API 常量和类型
// ============================================================================

const (
	PROCESS_VM_READ           = 0x0010
	PROCESS_QUERY_INFORMATION = 0x0400
	MAX_MODULE_NAME32         = 255
	MAX_PATH                  = 260
	TH32CS_SNAPPROCESS        = 0x00000002
	MEM_COMMIT                = 0x1000
	PAGE_READWRITE            = 0x04
	PAGE_WRITECOPY            = 0x08
	PAGE_EXECUTE_READWRITE    = 0x40
	PAGE_EXECUTE_WRITECOPY    = 0x80
)

type PROCESSENTRY32 struct {
	Size              uint32
	CntUsage          uint32
	ProcessID         uint32
	DefaultHeapID     uintptr
	ModuleID          uint32
	CntThreads        uint32
	ParentProcessID   uint32
	PriorityClassBase int32
	Flags             uint32
	ExeFile           [MAX_PATH]uint16
}

type MODULEENTRY32 struct {
	Size         uint32
	ModuleID     uint32
	ProcessID    uint32
	GlblcntUsage uint32
	ProccntUsage uint32
	ModBaseAddr  uintptr
	ModBaseSize  uint32
	Module       uintptr
	ModuleName   [MAX_MODULE_NAME32 + 1]uint16
	ExePath      [MAX_PATH]uint16
}

type MEMORY_BASIC_INFORMATION struct {
	BaseAddress       uintptr
	AllocationBase    uintptr
	AllocationProtect uint32
	RegionSize        uintptr
	State             uint32
	Protect           uint32
	Type              uint32
}

// Module 表示一个已加载的模块
type Module struct {
	Name string
	Base uint32
	Size uint32
}

// ============================================================================
// Windows API 加载
// ============================================================================

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	psapi    = syscall.NewLazyDLL("psapi.dll")
	user32   = syscall.NewLazyDLL("user32.dll")

	procOpenProcess            = kernel32.NewProc("OpenProcess")
	procCloseHandle            = kernel32.NewProc("CloseHandle")
	procReadProcessMemory      = kernel32.NewProc("ReadProcessMemory")
	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW        = kernel32.NewProc("Process32FirstW")
	procProcess32NextW         = kernel32.NewProc("Process32NextW")
	procModule32FirstW         = kernel32.NewProc("Module32FirstW")
	procModule32NextW          = kernel32.NewProc("Module32NextW")
	procVirtualQueryEx         = kernel32.NewProc("VirtualQueryEx")

	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procIsIconic                 = user32.NewProc("IsIconic")
	procGetGUIThreadInfo         = user32.NewProc("GetGUIThreadInfo")

	TH32CS_SNAPMODULE  uint32 = 0x00000008
	TH32CS_SNAPMODULE32 uint32 = 0x00000010
)

// ============================================================================
// 窗口检测
// ============================================================================

// WindowInfo 窗口信息
type WindowInfo struct {
	Handle uintptr
	Title  string
}

// --- 包级回调：EnumProcessWindows（避免反复 NewCallback 导致 "too many callback functions"）---
var (
	enumWinMu      sync.Mutex
	enumWinPID     uint32
	enumWinResults []WindowInfo
)

var enumWindowsCallback = syscall.NewCallback(func(hwnd uintptr, lParam uintptr) uintptr {
	var windowPID uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&windowPID)))
	if windowPID != enumWinPID {
		return 1
	}
	visible, _, _ := procIsWindowVisible.Call(hwnd)
	if visible == 0 {
		return 1
	}
	buf := make([]uint16, 256)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
	title := syscall.UTF16ToString(buf)
	if title != "" {
		enumWinResults = append(enumWinResults, WindowInfo{Handle: hwnd, Title: title})
	}
	return 1
})

// EnumProcessWindows 枚举属于指定 PID 的所有可见顶层窗口
func EnumProcessWindows(pid uint32) []WindowInfo {
	enumWinMu.Lock()
	defer enumWinMu.Unlock()

	enumWinPID = pid
	enumWinResults = nil

	procEnumWindows.Call(enumWindowsCallback, 0)

	result := enumWinResults
	enumWinResults = nil
	return result
}

// HasSingingWindow 检查 WeSing 进程是否有 K歌窗口打开
func HasSingingWindow(pid uint32) bool {
	state := GetPlayState(pid)
	return state.Phase != PhaseStandby
}

// GetSongTitle 从 K歌窗口标题提取歌曲信息
func GetSongTitle(pid uint32) string {
	state := GetPlayState(pid)
	return state.SongTitle
}

// PlayPhase 播放阶段
type PlayPhase int

const (
	PhaseStandby PlayPhase = iota // 待机：无K歌窗口
	PhaseLoading                  // 加载中：有K歌窗口+CScoreWnd，无CLyricRenderWnd
	PhasePlaying                  // 播放中：有K歌窗口+CScoreWnd+CLyricRenderWnd
)

// GUITHREADINFO 用于 GetGUIThreadInfo
type GUITHREADINFO struct {
	CbSize        uint32
	Flags         uint32
	HwndActive    uintptr
	HwndFocus     uintptr
	HwndCapture   uintptr
	HwndMenuOwner uintptr
	HwndMoveSize  uintptr
	HwndCaret     uintptr
	RcCaret       [4]int32 // RECT
}

const GUI_INMOVESIZE = 0x0002

// PlayState 播放状态（单次 EnumWindows 获取全部信息）
type PlayState struct {
	Phase       PlayPhase // 播放阶段
	SongTitle   string    // 歌曲标题（从窗口标题提取，空=无歌）
	IsMinimized bool      // 播放窗口是否最小化
	IsMoving    bool      // 播放窗口是否正在被拖动/调整大小
}

// --- 包级回调：GetPlayState（避免反复 NewCallback 导致 "too many callback functions"）---
var (
	getPlayStateMu          sync.Mutex
	getPlayStatePID         uint32
	getPlayStateHasSong     bool
	getPlayStateHasLyric    bool
	getPlayStateSongTitle   string
	getPlayStateIsMinimized bool
	getPlayStateSongHwnd    uintptr // 播放窗口句柄（用于后续 GUI 状态检查）
)

var getPlayStateCallback = syscall.NewCallback(func(hwnd uintptr, lParam uintptr) uintptr {
	var windowPID uint32
	// GetWindowThreadProcessId 返回值是线程 ID
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&windowPID)))
	if windowPID != getPlayStatePID {
		return 1
	}
	visible, _, _ := procIsWindowVisible.Call(hwnd)
	if visible == 0 {
		return 1
	}
	buf := make([]uint16, 256)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
	title := syscall.UTF16ToString(buf)

	switch {
	case title == "CLyricRenderWnd":
		getPlayStateHasLyric = true
	case strings.Contains(title, "全民K歌") && strings.Contains(title, " - "):
		getPlayStateHasSong = true
		idx := strings.Index(title, " - ")
		getPlayStateSongTitle = strings.TrimSpace(title[idx+3:])
		getPlayStateSongHwnd = hwnd
		// 检测窗口是否最小化
		iconic, _, _ := procIsIconic.Call(hwnd)
		if iconic != 0 {
			getPlayStateIsMinimized = true
		}
	}
	return 1
})

// GetPlayState 一次枚举获取完整播放状态（快速）
func GetPlayState(pid uint32) PlayState {
	getPlayStateMu.Lock()
	defer getPlayStateMu.Unlock()

	getPlayStatePID = pid
	getPlayStateHasSong = false
	getPlayStateHasLyric = false
	getPlayStateSongTitle = ""
	getPlayStateIsMinimized = false
	getPlayStateSongHwnd = 0

	procEnumWindows.Call(getPlayStateCallback, 0)

	var state PlayState
	state.SongTitle = getPlayStateSongTitle
	state.IsMinimized = getPlayStateIsMinimized

	// 检测窗口是否正在被拖动/调整大小
	if getPlayStateSongHwnd != 0 {
		threadID, _, _ := procGetWindowThreadProcessId.Call(getPlayStateSongHwnd, 0)
		if threadID != 0 {
			var gti GUITHREADINFO
			gti.CbSize = uint32(unsafe.Sizeof(gti))
			ret, _, _ := procGetGUIThreadInfo.Call(uintptr(threadID), uintptr(unsafe.Pointer(&gti)))
			if ret != 0 && gti.Flags&GUI_INMOVESIZE != 0 {
				state.IsMoving = true
			}
		}
	}

	if getPlayStateHasSong && getPlayStateHasLyric {
		state.Phase = PhasePlaying
	} else if getPlayStateHasSong {
		state.Phase = PhaseLoading
	} else {
		state.Phase = PhaseStandby
	}
	return state
}

// ============================================================================
// 进程操作
// ============================================================================

// FindProcess 通过进程名查找 PID
func FindProcess(name string) (uint32, error) {
	snapshot, _, err := procCreateToolhelp32Snapshot.Call(
		uintptr(TH32CS_SNAPPROCESS), 0)
	if snapshot == uintptr(^uintptr(0)) {
		return 0, fmt.Errorf("CreateToolhelp32Snapshot 失败: %v", err)
	}
	defer procCloseHandle.Call(snapshot)

	var entry PROCESSENTRY32
	entry.Size = uint32(unsafe.Sizeof(entry))

	ret, _, _ := procProcess32FirstW.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
	if ret == 0 {
		return 0, fmt.Errorf("Process32FirstW 失败")
	}

	nameLower := strings.ToLower(name)
	for {
		exeName := syscall.UTF16ToString(entry.ExeFile[:])
		if strings.ToLower(exeName) == nameLower {
			return entry.ProcessID, nil
		}
		ret, _, _ = procProcess32NextW.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
		if ret == 0 {
			break
		}
	}
	return 0, fmt.Errorf("未找到进程: %s", name)
}

// OpenProc 打开进程并返回句柄
func OpenProc(pid uint32) (syscall.Handle, error) {
	handle, _, err := procOpenProcess.Call(
		uintptr(PROCESS_VM_READ|PROCESS_QUERY_INFORMATION),
		0, uintptr(pid))
	if handle == 0 {
		return 0, fmt.Errorf("OpenProcess 失败: %v", err)
	}
	return syscall.Handle(handle), nil
}

// CloseProc 关闭进程句柄
func CloseProc(handle syscall.Handle) {
	procCloseHandle.Call(uintptr(handle))
}

// EnumModules 枚举进程的所有已加载模块（32位快照）
func EnumModules(pid uint32) ([]Module, error) {
	snapshot, _, err := procCreateToolhelp32Snapshot.Call(
		uintptr(TH32CS_SNAPMODULE|TH32CS_SNAPMODULE32), uintptr(pid))
	if snapshot == uintptr(^uintptr(0)) {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot(MODULE) 失败: %v", err)
	}
	defer procCloseHandle.Call(snapshot)

	var entry MODULEENTRY32
	entry.Size = uint32(unsafe.Sizeof(entry))

	ret, _, _ := procModule32FirstW.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
	if ret == 0 {
		return nil, fmt.Errorf("Module32FirstW 失败")
	}

	var modules []Module
	for {
		name := syscall.UTF16ToString(entry.ModuleName[:])
		modules = append(modules, Module{
			Name: name,
			Base: uint32(entry.ModBaseAddr),
			Size: entry.ModBaseSize,
		})
		ret, _, _ = procModule32NextW.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
		if ret == 0 {
			break
		}
	}
	return modules, nil
}

// ============================================================================
// 内存读取
// ============================================================================

// ReadBytes 从进程内存读取指定字节
func ReadBytes(handle syscall.Handle, addr uint32, size uint32) ([]byte, error) {
	buf := make([]byte, size)
	var bytesRead uintptr
	ret, _, err := procReadProcessMemory.Call(
		uintptr(handle),
		uintptr(addr),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(size),
		uintptr(unsafe.Pointer(&bytesRead)))
	if ret == 0 {
		return nil, fmt.Errorf("ReadProcessMemory(0x%X, %d) 失败: %v", addr, size, err)
	}
	return buf[:bytesRead], nil
}

// ReadUint32 读取一个 32 位无符号整数
func ReadUint32(handle syscall.Handle, addr uint32) (uint32, error) {
	buf, err := ReadBytes(handle, addr, 4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buf), nil
}

// ReadInt16 读取一个 16 位有符号整数
func ReadInt16(handle syscall.Handle, addr uint32) (int16, error) {
	buf, err := ReadBytes(handle, addr, 2)
	if err != nil {
		return 0, err
	}
	return int16(binary.LittleEndian.Uint16(buf)), nil
}

// ReadUint16 读取一个 16 位无符号整数
func ReadUint16(handle syscall.Handle, addr uint32) (uint16, error) {
	buf, err := ReadBytes(handle, addr, 2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(buf), nil
}

// ReadFloat32 读取一个 32 位浮点数
func ReadFloat32(handle syscall.Handle, addr uint32) (float32, error) {
	buf, err := ReadBytes(handle, addr, 4)
	if err != nil {
		return 0, err
	}
	bits := binary.LittleEndian.Uint32(buf)
	return math.Float32frombits(bits), nil
}

// ReadString 读取以 null 结尾的 ASCII 字符串
func ReadString(handle syscall.Handle, addr uint32, maxLen int) (string, error) {
	buf, err := ReadBytes(handle, addr, uint32(maxLen))
	if err != nil {
		return "", err
	}
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i]), nil
		}
	}
	return string(buf), nil
}

// ============================================================================
// 内存区域枚举与 AOB 扫描
// ============================================================================

// MemoryRegion 表示一个可读的内存区域
type MemoryRegion struct {
	Base uint32
	Size uint32
}

// EnumWritableRegions 枚举所有可写已提交的内存区域
func EnumWritableRegions(handle syscall.Handle) []MemoryRegion {
	var regions []MemoryRegion
	var addr uintptr
	var mbi MEMORY_BASIC_INFORMATION
	mbiSize := unsafe.Sizeof(mbi)

	for addr < 0x7FFF0000 { // 32 位用户空间上限
		ret, _, _ := procVirtualQueryEx.Call(
			uintptr(handle), addr,
			uintptr(unsafe.Pointer(&mbi)), mbiSize)
		if ret == 0 {
			break
		}
		if mbi.State == MEM_COMMIT && isWritable(mbi.Protect) {
			regions = append(regions, MemoryRegion{
				Base: uint32(mbi.BaseAddress),
				Size: uint32(mbi.RegionSize),
			})
		}
		addr = mbi.BaseAddress + mbi.RegionSize
	}
	return regions
}

func isWritable(protect uint32) bool {
	return protect == PAGE_READWRITE || protect == PAGE_EXECUTE_READWRITE
	// 排除 PAGE_WRITECOPY 和 PAGE_EXECUTE_WRITECOPY
}

// AOBScan 在可写内存区域搜索字节模式（支持 0xFF 作为通配符）
// pattern: 字节切片，mask: true=匹配, false=通配
// 返回所有匹配的地址
func AOBScan(handle syscall.Handle, pattern []byte, mask []bool, regions []MemoryRegion) []uint32 {
	var results []uint32
	patLen := len(pattern)

	for _, region := range regions {
		// 限制单次读取大小，避免内存爆炸
		size := region.Size
		if size > 64*1024*1024 {
			continue // 跳过超大区域
		}
		buf, err := ReadBytes(handle, region.Base, size)
		if err != nil {
			continue
		}

		// Boyer-Moore 简化：逐字节扫描
		for i := 0; i <= len(buf)-patLen; i++ {
			match := true
			for j := 0; j < patLen; j++ {
				if mask[j] && buf[i+j] != pattern[j] {
					match = false
					break
				}
			}
			if match {
				results = append(results, region.Base+uint32(i))
			}
		}
	}
	return results
}

// ParseAOBPattern 将 "E8 ?? AB 70" 格式的字符串解析为 pattern 和 mask
func ParseAOBPattern(s string) (pattern []byte, mask []bool) {
	parts := strings.Fields(s)
	for _, p := range parts {
		if p == "??" {
			pattern = append(pattern, 0)
			mask = append(mask, false)
		} else {
			var b byte
			fmt.Sscanf(p, "%02X", &b)
			pattern = append(pattern, b)
			mask = append(mask, true)
		}
	}
	return
}

// Uint32ToAOB 将一个 uint32 值转为小端 AOB 字节模式
func Uint32ToAOB(val uint32) ([]byte, []bool) {
	pattern := make([]byte, 4)
	binary.LittleEndian.PutUint32(pattern, val)
	mask := []bool{true, true, true, true}
	return pattern, mask
}
