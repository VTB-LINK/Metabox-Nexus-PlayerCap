package qqmusic

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"Metabox-Nexus-PlayerCap/telemetry"
)

var (
	modkernel32            = syscall.NewLazyDLL("kernel32.dll")
	procCreateToolhelp32   = modkernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32First     = modkernel32.NewProc("Process32FirstW")
	procProcess32Next      = modkernel32.NewProc("Process32NextW")
	procModule32First      = modkernel32.NewProc("Module32FirstW")
	procModule32Next       = modkernel32.NewProc("Module32NextW")
	procOpenProcess        = modkernel32.NewProc("OpenProcess")
	procCloseHandle        = modkernel32.NewProc("CloseHandle")
	procReadProcessMemory  = modkernel32.NewProc("ReadProcessMemory")
	procWriteProcessMemory = modkernel32.NewProc("WriteProcessMemory")
	procVirtualAllocEx     = modkernel32.NewProc("VirtualAllocEx")
	procVirtualProtectEx   = modkernel32.NewProc("VirtualProtectEx")

	modpsapi                 = syscall.NewLazyDLL("psapi.dll")
	procEnumProcessModulesEx = modpsapi.NewProc("EnumProcessModulesEx")
	procGetModuleBaseNameW   = modpsapi.NewProc("GetModuleBaseNameW")
	procGetModuleInformation = modpsapi.NewProc("GetModuleInformation")

	procVirtualQueryEx   = modkernel32.NewProc("VirtualQueryEx")
	procGetTickCount     = modkernel32.NewProc("GetTickCount")
	procGetProcAddress   = modkernel32.NewProc("GetProcAddress")
	procGetModuleHandleW = modkernel32.NewProc("GetModuleHandleW")

	modversion                  = syscall.NewLazyDLL("version.dll")
	procGetFileVersionInfoSizeW = modversion.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfoW     = modversion.NewProc("GetFileVersionInfoW")
	procVerQueryValueW          = modversion.NewProc("VerQueryValueW")
)

const (
	PROCESS_ALL_ACCESS     = 0x1F0FFF
	TH32CS_SNAPPROCESS     = 0x00000002
	TH32CS_SNAPMODULE      = 0x00000008
	TH32CS_SNAPMODULE32    = 0x00000010
	MEM_COMMIT             = 0x1000
	MEM_RESERVE            = 0x2000
	PAGE_EXECUTE_READWRITE = 0x40
	PAGE_READWRITE         = 0x04
	LIST_MODULES_ALL       = 0x03
)

type MEMORY_BASIC_INFORMATION struct {
	BaseAddress       uintptr
	AllocationBase    uintptr
	AllocationProtect uint32
	RegionSize        uintptr
	State             uint32
	Protect           uint32
	Type              uint32
}

type MODULEINFO struct {
	LpBaseOfDll unsafe.Pointer
	SizeOfImage uint32
	EntryPoint  unsafe.Pointer
}

type PROCESSENTRY32W struct {
	Size            uint32
	Usage           uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	Threads         uint32
	ParentProcessID uint32
	PriClassBase    int32
	Flags           uint32
	ExeFile         [260]uint16
}

type MODULEENTRY32W struct {
	Size         uint32
	ModuleID     uint32
	ProcessID    uint32
	GlblcntUsage uint32
	ProccntUsage uint32
	ModBaseAddr  *byte
	ModBaseSize  uint32
	Module       [256]uint16
	ExePath      [260]uint16
}

// dllOffsets 存储版本相关的 DLL 内部偏移量
type dllOffsets struct {
	// DLL 级偏移（版本敏感，每次更新可能变化）
	Struct1      uintptr // 元数据结构体 1 基址（QQMusic.dll 内偏移）
	Struct2Ptr   uintptr // 元数据结构体 2 指针（QQMusic.dll 内偏移，0=不可用）
	FastTimerPtr uintptr // 快速进度计时器指针（QQMusic.dll 内偏移，0=不可用）
	FastTimerOff uintptr // 快速计时器进度字段偏移（相对于指针解引用后的对象）

	// Struct1 内部偏移（结构体成员位置，版本间可能变化）
	NameOff     uintptr // 歌名字段偏移（SSO String 或 WCHAR* 取决于 UseWideStrings）
	SingerOff   uintptr // 歌手字段偏移
	AlbumOff    uintptr // 专辑名字段偏移
	SongIDOff   uintptr // SongID DWORD 偏移
	DurationOff uintptr // 总时长 DWORD 偏移
	ProgressOff uintptr // 慢速进度 DWORD 偏移

	// 版本特性标志
	UseWideStrings    bool    // true=歌名/歌手字段为 WCHAR* 指针（v20.05+）
	DurationInSeconds bool    // true=时长字段单位为秒需 *1000（v20.05+）
	SongMidParamsOff  uintptr // v20.05: struct1 内 URL 参数字符串指针偏移（0=不可用）
	// 格式: "0=<songMid>&2=<id>|<id>"，备选来源
	StreamURLOff uintptr // v20.05: struct1 内流媒体 URL 字符串指针偏移（0=不可用）
	// 格式: "http://stream*.qqmusic.qq.com/<songMid>.<ext>"
	// 文件名去扩展名即 songMid，比 URL params 更可靠
	ProgressDllOff uintptr // v20.05: DLL 基址直接偏移，DWORD 即播放毫秒数（0=不可用）
	// CE 手动确认: 0xB61E78 = 准确ms进度（备选 0xAF3600）

	SongMidFromHeap bool // true=songMid 不在结构体里，由 FindSongMid 从堆 JSON 按时长扫出（v22.41 兜底）

	// SongIDDurCheckOff：稳定 songID 所在「播放会话」结构自带的时长字段偏移（相对 Struct1，
	// 0=不校验）。与 DurationOff 的显示时长交叉核对，确认该 songID 已同步到当前歌（v22.41）。
	SongIDDurCheckOff uintptr
}

// knownVersions 版本偏移表
var knownVersions = map[string]dllOffsets{
	"20.05": {
		// CE 逆向 v20.05（便携版，32-bit x86）
		// QQMusic.dll base 0x5EC50000，BSS 0x5F73EE00-0x5F7B5302
		// 字符串类型：WCHAR* 指针（UTF-16 堆字符串），时长单位：秒
		// SongMidParamsOff: struct1+0xAC → ptr → UTF-16 "0=<songMid>&2=<v20id>|<id>"
		//   扫描验证：切歌后该堆地址内容就地更新（67→212 命中，7个交集地址之一）
		Struct1:           0xB63ED0,
		Struct2Ptr:        0,
		FastTimerPtr:      0,
		FastTimerOff:      0,
		SongIDOff:         0x40,
		DurationOff:       0x44,
		ProgressOff:       0x68,     // 静态值(文件大小KB)，仅作回退
		ProgressDllOff:    0xB61E78, // CE 手动确认，准确ms进度（备选 0xAF3600）
		NameOff:           0x70,
		SingerOff:         0x74,
		AlbumOff:          0x78,
		StreamURLOff:      0x80, // struct1+0x80 → WCHAR* → "http://.../00281PXu4DHKNp.wma"
		SongMidParamsOff:  0xAC, // 备选: "0=<songMid>&2=..."
		UseWideStrings:    true,
		DurationInSeconds: true,
	},
	"21.81": {
		// CE Lua 逆向 v21.81（32-bit x86）
		// SSO 字符串布局与 v22.16 相同，DLL base 0x72780000
		// 验证: 歌名"无地自容" SSO@DLL+0xB75840，歌手"黑豹乐队"，专辑"黑豹 同名专辑"
		// SongID=101832223 Duration=338333ms Progress=实时ms
		Struct1:      0xB75840,
		Struct2Ptr:   0,
		FastTimerPtr: 0,
		FastTimerOff: 0,
		NameOff:      0x00,
		SingerOff:    0x18,
		AlbumOff:     0x30,
		SongIDOff:    0x60,
		DurationOff:  0x68,
		ProgressOff:  0x6C,
	},
	"22.16": {
		Struct1:      0xC87C80,
		Struct2Ptr:   0xC86B00,
		FastTimerPtr: 0xC157D8,
		FastTimerOff: 0x618,
		NameOff:      0x00,
		SingerOff:    0x18,
		AlbumOff:     0x30,
		SongIDOff:    0x60,
		DurationOff:  0x68,
		ProgressOff:  0x6C,
	},
	"22.22": {
		Struct1:      0xC95EA0,
		Struct2Ptr:   0, // v22.22 中 Struct2 不再可用
		FastTimerPtr: 0xC23994,
		FastTimerOff: 0x798,
		NameOff:      0x80,
		SingerOff:    0x98,
		AlbumOff:     0xB0,
		SongIDOff:    0xE0,
		DurationOff:  0xE8,
		ProgressOff:  0xEC,
	},
	"22.41": {
		// CE 实测（32-bit x86）：结构体由窄 SSO 改为 UTF-16 WCHAR* 指针。
		// now-playing 显示对象在 QQMusic.dll 静态 .data（Struct1），跨歌验证字段原地更新：
		//   +0x00 Name*、+0x04 Singer*、+0x08 Album*、+0x0C 进度ms（亚秒高分辨率 →
		//   无需 FastTimer）、+0x10 时长ms。
		// 客户端上报 JSON 的 songid 恒 0，但真数字 songID 存在另一处固定「播放会话」结构里
		// （volume/speed 浮点 + songID + 时长），距 Struct1 0x722E8（绝对 QQMusic.dll+0xCB2830）。
		// 跨歌验证：上一首 2480434、当前 351669598 均落此偏移且与服务器一致。用它走现成的
		// songID 取词路径（musicu.fcg），无需堆扫描、无时序竞态。
		// 该结构与显示对象在换歌瞬间可能短暂不同步 → 用它自带时长（+0x722F0=DLL+0xCB2838）
		// 与显示时长交叉核对（SongIDDurCheckOff），不一致则弃用该 songID。
		// SongMidFromHeap 保留作兜底：songID 未就绪（读到 0）时回退 FindSongMid 堆扫描。
		Struct1:           0xC40548,
		NameOff:           0x00,
		SingerOff:         0x04,
		AlbumOff:          0x08,
		ProgressOff:       0x0C,
		DurationOff:       0x10,
		SongIDOff:         0x722E8, // → QQMusic.dll+0xCB2830，稳定 songID 字段
		SongIDDurCheckOff: 0x722F0, // → QQMusic.dll+0xCB2838，同结构自带时长，用于同步性核对
		UseWideStrings:    true,
		SongMidFromHeap:   true,
	},
}

type QQMusicMem struct {
	pid            uint32
	hProcess       uintptr
	qqmusicDllBase uintptr
	gfWrapperBase  uintptr
	gfWrapperSize  uint32
	kernel32Base   uintptr    // kernel32.dll base in the target 32-bit process
	sliderPointer  uintptr    // Dynamic memory cave address where EDI is stored
	progressPtr    uintptr    // Address where hooked progress (ms) is stored
	progressTsPtr  uintptr    // Address where GetTickCount timestamp is stored
	version        string     // 检测到的 QQ 音乐版本号，如 "22.16"
	offsets        dllOffsets // 当前版本的偏移量
}

func (m *QQMusicMem) CheckValid() bool {
	if m.hProcess == 0 {
		return false
	}
	var exitCode uint32
	syscall.GetExitCodeProcess(syscall.Handle(m.hProcess), &exitCode)
	if exitCode != 259 { // STILL_ACTIVE is 259
		m.hProcess = 0
		return false
	}
	return true
}

func ConnectQQMusic() (*QQMusicMem, error) {
	mem := &QQMusicMem{}

	// 1. Find process ID
	hSnap, _, _ := procCreateToolhelp32.Call(uintptr(TH32CS_SNAPPROCESS), 0)
	if hSnap == uintptr(syscall.InvalidHandle) {
		return nil, errors.New("CreateToolhelp32Snapshot failed")
	}
	defer procCloseHandle.Call(hSnap)

	var pe32 PROCESSENTRY32W
	pe32.Size = uint32(unsafe.Sizeof(pe32))

	var pids []uint32
	ret, _, _ := procProcess32First.Call(hSnap, uintptr(unsafe.Pointer(&pe32)))
	for ret != 0 {
		name := syscall.UTF16ToString(pe32.ExeFile[:])
		if strings.ToLower(name) == "qqmusic.exe" {
			pids = append(pids, pe32.ProcessID)
		}
		ret, _, _ = procProcess32Next.Call(hSnap, uintptr(unsafe.Pointer(&pe32)))
	}

	if len(pids) == 0 {
		return nil, errors.New("QQMusic.exe not found")
	}

	for _, pid := range pids {
		hProcess, _, _ := procOpenProcess.Call(uintptr(PROCESS_ALL_ACCESS), 0, uintptr(pid))
		if hProcess == 0 {
			continue
		}

		var modules [1024]uintptr
		var cbNeeded uint32

		retEnum, _, _ := procEnumProcessModulesEx.Call(
			hProcess,
			uintptr(unsafe.Pointer(&modules[0])),
			uintptr(unsafe.Sizeof(modules)),
			uintptr(unsafe.Pointer(&cbNeeded)),
			uintptr(LIST_MODULES_ALL),
		)

		foundDll := false
		if retEnum != 0 {
			// cbNeeded 是「**所需**字节数」不是「已写入字节数」：EnumProcessModulesEx 在缓冲区
			// 不足时仍返回 TRUE，只把实际需要的大小写进 cbNeeded（MSDN 明确如此）。不截断的话，
			// 进程模块数 > 1024 时 numModules > len(modules) → modules[i] 越界 panic。
			// 而全仓 recover() 零命中 → 任一 goroutine panic = **整个进程死亡**，不只是取词停摆。
			//
			// 截断而非按 MSDN 的两遍模式重新分配：漏找几个 DLL 的降级后果是 ConnectQQMusic 返错
			// → qqmusic 的重试循环继续转，已经够用；两遍模式要多一次动态分配 + 一次 syscall。
			numModules := cbNeeded / uint32(unsafe.Sizeof(modules[0]))
			if numModules > uint32(len(modules)) {
				log.Warn("QQMusic.exe 模块数 %d 超出缓冲区 %d，只扫前 %d 个（找不到 DLL 会走重试）",
					numModules, len(modules), len(modules))
				numModules = uint32(len(modules))
			}
			for i := uint32(0); i < numModules; i++ {
				var name [256]uint16
				procGetModuleBaseNameW.Call(hProcess, modules[i], uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
				modName := syscall.UTF16ToString(name[:])
				lowerName := strings.ToLower(modName)

				if lowerName == "qqmusic.dll" {
					mem.qqmusicDllBase = modules[i]
					foundDll = true
				}
				// v22.16: QQMusic_GFWrapper.dll, v22.22+: 仍存在但也新增 GF.dll
				if lowerName == "qqmusic_gfwrapper.dll" || lowerName == "gf.dll" {
					// 优先使用 GFWrapper（滑块 AOB 在其中），GF.dll 作为备选
					if mem.gfWrapperBase == 0 || lowerName == "qqmusic_gfwrapper.dll" {
						mem.gfWrapperBase = modules[i]
						var minfo MODULEINFO
						procGetModuleInformation.Call(hProcess, modules[i], uintptr(unsafe.Pointer(&minfo)), uintptr(unsafe.Sizeof(minfo)))
						mem.gfWrapperSize = minfo.SizeOfImage
					}
				}
				if lowerName == "kernel32.dll" {
					mem.kernel32Base = modules[i]
				}
			}
		}

		if foundDll {
			mem.pid = pid
			mem.hProcess = hProcess

			// 检测版本并加载偏移表
			mem.detectVersion()

			return mem, nil
		}
		procCloseHandle.Call(hProcess)
	}

	return nil, errors.New("QQMusic.dll not found in any QQMusic.exe process")
}

// detectVersion 从 QQMusic.exe 的 PE 版本资源中提取版本号并匹配偏移表
func (m *QQMusicMem) detectVersion() {
	// 优先使用 QueryFullProcessImageNameW：直接从已打开的 hProcess 获取 exe 路径，
	// 避免 findQQMusicExePath 误返回其他版本的安装路径
	exePath := ""
	procQueryFullProcessImageNameW := modkernel32.NewProc("QueryFullProcessImageNameW")
	buf := make([]uint16, 512)
	size := uint32(len(buf))
	ret, _, _ := procQueryFullProcessImageNameW.Call(
		m.hProcess, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if ret != 0 && size > 0 {
		exePath = syscall.UTF16ToString(buf[:size])
	}

	// 回退：模块快照或已知安装路径
	if exePath == "" {
		exePath = findQQMusicExePath(m.pid)
	}

	if exePath == "" {
		log.Warn("无法获取 QQMusic.exe 路径，使用默认偏移 (v22.16)")
		m.version = "22.16"
		m.offsets = knownVersions["22.16"]
		return
	}

	major, minor := getFileVersion(exePath)
	if major == 0 && minor == 0 {
		log.Warn("版本检测失败，使用默认偏移 (v22.16)")
		m.version = "22.16"
		m.offsets = knownVersions["22.16"]
		return
	}

	m.version = fmt.Sprintf("%d.%02d", major, minor)

	if offsets, ok := knownVersions[m.version]; ok {
		m.offsets = offsets
		log.Info("QQ 音乐版本: %s (偏移已匹配)", m.version)
	} else {
		log.Warn("QQ 音乐版本 %s 无已知偏移表，回退到 v22.16", m.version)
		m.offsets = knownVersions["22.16"]

		// 「预期之外」：这台机器上的 QQ 音乐是我们没见过的版本。回退偏移读出来的多半是垃圾
		// （见 AGENTS.md §5.1），所以这条不是「降级运行」，是「正在读错内存」。
		// 上报是为了知道野外有哪些版本 —— 那正是补偏移表的输入。
		telemetry.ReportOnce("qqmusic.unknown_version",
			"QQ 音乐版本无已知偏移表，已回退到 v22.16（读到的可能是垃圾）",
			map[string]any{
				"version":  m.version,
				"fallback": "22.16",
			})
	}
}

// findQQMusicExePath 通过 PID 获取 QQMusic.exe 的完整路径
// 优先从进程模块快照读取实际路径，避免误读其他版本的安装路径
func findQQMusicExePath(pid uint32) string {
	// 方法 1: 通过模块快照获取目标进程的真实 exe 路径（优先）
	hSnap, _, _ := procCreateToolhelp32.Call(uintptr(TH32CS_SNAPMODULE|TH32CS_SNAPMODULE32), uintptr(pid))
	if hSnap != uintptr(syscall.InvalidHandle) {
		defer procCloseHandle.Call(hSnap)
		var me32 MODULEENTRY32W
		me32.Size = uint32(unsafe.Sizeof(me32))
		ret, _, _ := procModule32First.Call(hSnap, uintptr(unsafe.Pointer(&me32)))
		for ret != 0 {
			modName := syscall.UTF16ToString(me32.Module[:])
			if strings.EqualFold(modName, "QQMusic.exe") {
				return syscall.UTF16ToString(me32.ExePath[:])
			}
			ret, _, _ = procModule32Next.Call(hSnap, uintptr(unsafe.Pointer(&me32)))
		}
	}

	// 方法 2: 回退到已知安装路径（仅当模块快照失败时）
	knownPaths := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Tencent", "QQMusic", "QQMusic.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Tencent", "QQMusic", "QQMusic.exe"),
		filepath.Join(os.Getenv("LocalAppData"), "Tencent", "QQMusic", "QQMusic.exe"),
	}
	for _, p := range knownPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// VS_FIXEDFILEINFO Windows PE 版本信息结构体
type vsFixedFileInfo struct {
	Signature        uint32
	StrucVersion     uint32
	FileVersionMS    uint32
	FileVersionLS    uint32
	ProductVersionMS uint32
	ProductVersionLS uint32
	FileFlagsMask    uint32
	FileFlags        uint32
	FileOS           uint32
	FileType         uint32
	FileSubtype      uint32
	FileDateMS       uint32
	FileDateLS       uint32
}

// getFileVersion 从 PE 文件中提取 ProductVersion (major, minor)
func getFileVersion(path string) (major, minor uint16) {
	pathUTF16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0
	}

	size, _, _ := procGetFileVersionInfoSizeW.Call(uintptr(unsafe.Pointer(pathUTF16)), 0)
	if size == 0 {
		return 0, 0
	}

	buf := make([]byte, size)
	ret, _, _ := procGetFileVersionInfoW.Call(
		uintptr(unsafe.Pointer(pathUTF16)),
		0,
		size,
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if ret == 0 {
		return 0, 0
	}

	subBlock, _ := syscall.UTF16PtrFromString(`\`)
	var infoPtr uintptr
	var infoLen uint32
	ret, _, _ = procVerQueryValueW.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(subBlock)),
		uintptr(unsafe.Pointer(&infoPtr)),
		uintptr(unsafe.Pointer(&infoLen)),
	)
	if ret == 0 || infoPtr == 0 {
		return 0, 0
	}

	// infoPtr 指向 buf 内部，但它是 uintptr——不持有对 buf 的引用。直接
	// (*vsFixedFileInfo)(unsafe.Pointer(infoPtr)) 会让 GC 有机会在下面解引用之前回收 buf。
	// 改为算偏移再回 buf 取地址：buf 因此保持存活，同时顺带做了越界校验。
	base := uintptr(unsafe.Pointer(&buf[0]))
	if infoPtr < base {
		return 0, 0
	}
	off := infoPtr - base
	if off+unsafe.Sizeof(vsFixedFileInfo{}) > uintptr(len(buf)) {
		return 0, 0
	}
	info := (*vsFixedFileInfo)(unsafe.Pointer(&buf[off]))
	if info.Signature != 0xFEEF04BD {
		return 0, 0
	}

	// ProductVersionMS: HIWORD=major, LOWORD=minor
	major = uint16(info.ProductVersionMS >> 16)
	minor = uint16(info.ProductVersionMS & 0xFFFF)
	return major, minor
}

// Version 返回检测到的 QQ 音乐版本号
func (m *QQMusicMem) Version() string {
	return m.version
}

func (m *QQMusicMem) Offsets() dllOffsets {
	return m.offsets
}

func (m *QQMusicMem) ReadUint32(addr uintptr) uint32 {
	var val uint32
	var bytesRead uintptr
	procReadProcessMemory.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&val)), 4, uintptr(unsafe.Pointer(&bytesRead)))
	return val
}

func (m *QQMusicMem) ReadFloat32(addr uintptr) float32 {
	var val float32
	var bytesRead uintptr
	procReadProcessMemory.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&val)), 4, uintptr(unsafe.Pointer(&bytesRead)))
	return val
}

func (m *QQMusicMem) ReadBytes(addr uintptr, size uint32) []byte {
	buf := make([]byte, size)
	var bytesRead uintptr
	procReadProcessMemory.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&buf[0])), uintptr(size), uintptr(unsafe.Pointer(&bytesRead)))
	return buf
}

func (m *QQMusicMem) WriteBytes(addr uintptr, data []byte) bool {
	var bytesWritten uintptr
	ret, _, _ := procWriteProcessMemory.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), uintptr(unsafe.Pointer(&bytesWritten)))
	return ret != 0
}

func (m *QQMusicMem) ReadString(addr uintptr, maxLen uint32) string {
	buf := m.ReadBytes(addr, maxLen)
	idx := bytes.IndexByte(buf, 0)
	if idx != -1 {
		buf = buf[:idx]
	}
	return string(buf)
}

func (m *QQMusicMem) ReadWideString(addr uintptr, maxLen uint32) string {
	buf := m.ReadBytes(addr, maxLen*2)
	u16 := make([]uint16, len(buf)/2)
	for i := 0; i < len(u16); i++ {
		u16[i] = binary.LittleEndian.Uint16(buf[i*2:])
		if u16[i] == 0 {
			u16 = u16[:i]
			break
		}
	}
	return string(utf16.Decode(u16))
}

// aobMatch scans data for pattern with optional mask (nil mask = exact match)
func aobMatch(data, pattern, mask []byte) int {
	pLen := len(pattern)
	for i := 0; i <= len(data)-pLen; i++ {
		matched := true
		for j := 0; j < pLen; j++ {
			if mask != nil && mask[j] == 0x00 {
				continue // wildcard
			}
			if data[i+j] != pattern[j] {
				matched = false
				break
			}
		}
		if matched {
			return i
		}
	}
	return -1
}

// AOB Injection for Accompaniment Slider
func (m *QQMusicMem) InjectSliderAOB() error {
	if m.gfWrapperBase == 0 {
		return errors.New("QQMusic_GFWrapper.dll not found")
	}
	// Pattern: 39 86 F0000000 74 ?? 8B CE 89 86 F0000000
	// New version: cmp [esi+F0],eax / je +XX / mov ecx,esi / mov [esi+F0],eax
	// We target the 'mov [esi+F0],eax' at offset +10 from pattern start
	patterns := []struct {
		pat         []byte
		mask        []byte // 0xFF = exact match, 0x00 = wildcard
		writeOff    int
		stolenBytes []byte // original bytes at the write instruction
		captureReg  byte   // register index for 'mov [ptr], reg' in codecave
	}{
		{
			// Current version: cmp [esi+F0],eax / je ?? / mov ecx,esi / mov [esi+F0],eax
			pat:         []byte{0x39, 0x86, 0xF0, 0x00, 0x00, 0x00, 0x74, 0x00, 0x8B, 0xCE, 0x89, 0x86, 0xF0, 0x00, 0x00, 0x00},
			mask:        []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
			writeOff:    10,                                         // offset to 'mov [esi+F0],eax'
			stolenBytes: []byte{0x89, 0x86, 0xF0, 0x00, 0x00, 0x00}, // mov [esi+F0],eax
			captureReg:  0x35,                                       // 'mov [addr], esi' -> 89 35 (capture esi = this pointer)
		},
		{
			// Legacy version: cmp esi,eax / cmovle esi,eax / mov [edi+F0],esi / pop edi / pop esi
			pat:         []byte{0x3B, 0xC6, 0x0F, 0x4E, 0xF0, 0x89, 0xB7, 0xF0, 0x00, 0x00, 0x00, 0x5F, 0x5E},
			mask:        nil,                                        // all exact
			writeOff:    5,                                          // offset to 'mov [edi+F0],esi'
			stolenBytes: []byte{0x89, 0xB7, 0xF0, 0x00, 0x00, 0x00}, // mov [edi+F0],esi
			captureReg:  0x3D,                                       // 'mov [addr], edi' -> 89 3D (capture edi = this pointer)
		},
	}

	chunkSize := uint32(m.gfWrapperSize)
	moduleData := m.ReadBytes(m.gfWrapperBase, chunkSize)

	var targetAddr uintptr
	var matchedPattern int = -1
	for pi, p := range patterns {
		offset := aobMatch(moduleData, p.pat, p.mask)
		if offset != -1 {
			targetAddr = m.gfWrapperBase + uintptr(offset) + uintptr(p.writeOff)
			matchedPattern = pi
			break
		}
	}
	if matchedPattern == -1 {
		return errors.New("aob pattern not found in memory (might be already injected)")
	}

	log.Info("AOB 滑块目标地址: 0x%X (模式 %d)", targetAddr, matchedPattern)

	// Check if already hooked
	firstByte := m.ReadBytes(targetAddr, 1)
	if len(firstByte) > 0 && firstByte[0] == 0xE9 {
		log.Info("滑块 Hook 已激活")
		return nil
	}

	// Allocate codecave
	caveAddr, _, _ := procVirtualAllocEx.Call(m.hProcess, 0, 0x1000, MEM_COMMIT|MEM_RESERVE, PAGE_EXECUTE_READWRITE)
	if caveAddr == 0 {
		return errors.New("VirtualAllocEx failed")
	}

	m.sliderPointer = caveAddr + 0x100
	log.Detail("Codecave @ 0x%X, 指针 @ 0x%X", caveAddr, m.sliderPointer)

	// Build assembly
	buf := new(bytes.Buffer)

	pat := patterns[matchedPattern]

	// Create "mov [m.sliderPointer], reg" to capture the object pointer
	buf.Write([]byte{0x89, pat.captureReg})
	binary.Write(buf, binary.LittleEndian, uint32(m.sliderPointer))

	// Write the original stolen bytes
	buf.Write(pat.stolenBytes)

	// calculate JMP rel32 back to targetAddr + 6
	returnAddr := targetAddr + 6
	caveJmpAddr := caveAddr + uintptr(buf.Len())
	rel32Back := uint32(returnAddr - caveJmpAddr - 5)

	buf.WriteByte(0xE9)
	binary.Write(buf, binary.LittleEndian, rel32Back)

	// Write codecave payload
	m.WriteBytes(caveAddr, buf.Bytes())

	// Unprotect target memory to write JMP
	var oldProtect uint32
	procVirtualProtectEx.Call(m.hProcess, targetAddr, 6, PAGE_EXECUTE_READWRITE, uintptr(unsafe.Pointer(&oldProtect)))

	// Write JMP to target
	jmpBuf := new(bytes.Buffer)
	jmpBuf.WriteByte(0xE9) // JMP
	rel32Target := uint32(caveAddr - targetAddr - 5)
	binary.Write(jmpBuf, binary.LittleEndian, rel32Target)
	jmpBuf.WriteByte(0x90) // NOP, because original was 6 bytes, JMP is 5 bytes

	m.WriteBytes(targetAddr, jmpBuf.Bytes())

	// Restore protect
	procVirtualProtectEx.Call(m.hProcess, targetAddr, 6, uintptr(oldProtect), uintptr(unsafe.Pointer(&oldProtect)))

	log.Info("滑块 AOB Hook 注入成功")
	return nil
}

// InjectProgressAOB hooks the progress write instruction (QQMusic.dll+488B75:
// mov [eax+1AC], esi) to also save ESI (progress ms) and GetTickCount() to
// fixed addresses. This enables precise local-clock interpolation.
//
// Codecave assembly:
//
//	mov [pProgressMs], esi     ; save progress value
//	pushad                      ; save all registers
//	call GetTickCount           ; EAX = wall-clock ms
//	mov [pTimeStamp], eax       ; save timestamp
//	popad                       ; restore registers
//	mov [eax+1AC], esi          ; original stolen bytes
//	jmp returnAddr
func (m *QQMusicMem) InjectProgressAOB() error {
	// AOB context: E8 ?? ?? ?? ?? 89 45 E8 | 89 B0 AC 01 00 00 | 8B F0
	// We hook the 6-byte instruction: 89 B0 AC 01 00 00 = mov [eax+000001AC], esi
	targetOffset := uintptr(0x488B75)
	targetAddr := m.qqmusicDllBase + targetOffset

	// Verify the target bytes
	expect := []byte{0x89, 0xB0, 0xAC, 0x01, 0x00, 0x00}
	actual := m.ReadBytes(targetAddr, 6)
	if !bytes.Equal(actual, expect) {
		// Check if already hooked (first byte = JMP = 0xE9)
		if len(actual) > 0 && actual[0] == 0xE9 {
			log.Info("进度 Hook 已激活")
			return nil
		}
		return fmt.Errorf("unexpected bytes at dll+%X: got %X, want %X", targetOffset, actual, expect)
	}

	// Resolve tick count: use KUSER_SHARED_DATA (fixed at 0x7FFE0000 in all Windows processes).
	// This avoids needing to resolve GetTickCount across 32/64 bit process boundaries.
	// TickCount.LowPart = [0x7FFE0320], TickCountMultiplier = [0x7FFE0004]
	// Result = (LowPart * Multiplier) >> 24 = milliseconds
	log.Detail("使用 KUSER_SHARED_DATA 获取 tick count")

	// Allocate codecave
	caveAddr, _, _ := procVirtualAllocEx.Call(m.hProcess, 0, 0x1000, MEM_COMMIT|MEM_RESERVE, PAGE_EXECUTE_READWRITE)
	if caveAddr == 0 {
		return errors.New("VirtualAllocEx failed for progress hook")
	}

	// Data area at cave+0x100 (progressMs) and cave+0x104 (timestamp)
	m.progressPtr = caveAddr + 0x100
	m.progressTsPtr = caveAddr + 0x104
	log.Detail("进度 Hook Cave @ 0x%X, progressPtr @ 0x%X, tsPtr @ 0x%X",
		caveAddr, m.progressPtr, m.progressTsPtr)

	// Build codecave assembly
	buf := new(bytes.Buffer)

	// mov [pProgressMs], esi  →  89 35 <addr32>
	buf.Write([]byte{0x89, 0x35})
	binary.Write(buf, binary.LittleEndian, uint32(m.progressPtr))

	// pushad → 60 (save all registers)
	buf.WriteByte(0x60)

	// Inline GetTickCount via KUSER_SHARED_DATA:
	// mov eax, [0x7FFE0320]    ; TickCount.LowPart    → A1 20 03 FE 7F
	buf.Write([]byte{0xA1, 0x20, 0x03, 0xFE, 0x7F})
	// mov edx, [0x7FFE0004]    ; TickCountMultiplier   → 8B 15 04 00 FE 7F
	buf.Write([]byte{0x8B, 0x15, 0x04, 0x00, 0xFE, 0x7F})
	// mul edx                  ; EDX:EAX = LowPart * Multiplier → F7 E2
	buf.Write([]byte{0xF7, 0xE2})
	// shrd eax, edx, 24        ; EAX = (result >> 24)  → 0F AC D0 18
	buf.Write([]byte{0x0F, 0xAC, 0xD0, 0x18})

	// mov [pTimeStamp], eax  →  A3 <addr32>
	buf.WriteByte(0xA3)
	binary.Write(buf, binary.LittleEndian, uint32(m.progressTsPtr))

	// popad → 61 (restore all registers)
	buf.WriteByte(0x61)

	// stolen bytes: mov [eax+000001AC], esi  →  89 B0 AC 01 00 00
	buf.Write(expect)

	// jmp returnAddr  →  E9 <rel32>
	returnAddr := targetAddr + 6
	jmpSite := caveAddr + uintptr(buf.Len())
	rel32Back := uint32(returnAddr - jmpSite - 5)
	buf.WriteByte(0xE9)
	binary.Write(buf, binary.LittleEndian, rel32Back)

	// Write codecave
	m.WriteBytes(caveAddr, buf.Bytes())

	// Unprotect target and write JMP
	var oldProtect uint32
	procVirtualProtectEx.Call(m.hProcess, targetAddr, 6, PAGE_EXECUTE_READWRITE, uintptr(unsafe.Pointer(&oldProtect)))

	jmpBuf := new(bytes.Buffer)
	jmpBuf.WriteByte(0xE9) // JMP rel32
	rel32Target := uint32(caveAddr - targetAddr - 5)
	binary.Write(jmpBuf, binary.LittleEndian, rel32Target)
	jmpBuf.WriteByte(0x90) // NOP pad (6 bytes original, 5 byte JMP)

	m.WriteBytes(targetAddr, jmpBuf.Bytes())

	// Restore protection
	procVirtualProtectEx.Call(m.hProcess, targetAddr, 6, uintptr(oldProtect), uintptr(unsafe.Pointer(&oldProtect)))

	log.Info("进度 Hook 注入成功")
	return nil
}

type SongMetadata struct {
	Name       string
	Singer     string
	SongID     uint32
	SongMid    string // QQ Music 字母数字 MID（v20.05 从 URL 参数解析，其他版本空）
	ProgressMs uint32
	DurationMs uint32
	SliderVal  uint32
}

// ssoFromBuf 从 24 字节的 MSVC 32-bit std::string 快照里解出字符串。
// 布局：+0x00 是内联缓冲区**或**堆指针，+0x10 是 length，+0x14 是 capacity；
// capacity > 15 = 堆分配，<= 15 = 内联。readHeap 用于把堆上的 n 字节读回来。
//
// 抽成纯函数是为了可测：调用方 extractSSO 是 ReadAllMetadata 里的闭包，
// 而这段判定是「一错就往 overlay 推乱码标题」的路径，必须有门禁。
//
// **堆/内联的判定必须独立于 length 的合法性检查，别把它们挤回同一个 `&&`。**
// 挤在一起时（原写法 `capacity > 15 && length > 0 && length < 1000`），
// 「已确诊是堆字符串、但 length 不合法」会掉进内联分支 —— 而那里 buf[0:4] 装的是
// **堆指针不是文本**，指针的原始字节被当歌名返回；sanitizeString 只按 rune 过滤
// （0xB1 解成 RuneError、>= 32 被保留），于是 overlay 闪一个乱码标题。
//
// 触发主力是 clear()：capacity 保留（仍 > 15）而 length 归 0 —— 实测原写法这条 96.9%
// 出乱码。版本偏移错位（detectVersion 对未知版本回退 22.16）也能进来，实测原写法 8.5%
// 出乱码、其余靠内联分支「找不到 NUL 就返回空」侥幸兜住。两条现在都收敛为返回空。
//
// 上界保持 1000：同文件的 ReadSSOString 用的是 2048，别顺手抄过来，那是放宽两倍上界。
func ssoFromBuf(buf []byte, readHeap func(ptr, n uint32) []byte) string {
	if len(buf) < 24 {
		return ""
	}
	length := binary.LittleEndian.Uint32(buf[0x10:0x14])
	capacity := binary.LittleEndian.Uint32(buf[0x14:0x18])

	if capacity > 15 {
		if length > 0 && length < 1000 {
			if ptr := binary.LittleEndian.Uint32(buf[0x00:0x04]); ptr > 0x10000 {
				return string(bytes.Trim(readHeap(ptr, length), "\x00"))
			}
		}
		// 堆模式下读不出来就到此为止，绝不 fallthrough 到内联分支
		return ""
	}

	// 内联分支（capacity <= 15）：逻辑原样保留。
	//
	// 注意 idx 算完即弃、真正用的是未 clamp 的 inlineIdx —— 看着像 bug，但**别顺手改**：
	// 它对短名是已验证正确的路径（21.81 条目的注释记着验证用例「无地自容」= 12 字节 →
	// cap==15 → 走这里），动它没有收益、只有回归风险。上面的堆分支拦住之后，进到这里的
	// 就只剩真·内联字符串了。
	idx := bytes.IndexByte(buf[:16], 0)
	if idx == -1 {
		idx = 16
	}
	inlineIdx := bytes.IndexByte(buf, 0)
	if inlineIdx > 0 {
		return string(buf[:inlineIdx])
	}
	return ""
}

func (m *QQMusicMem) ReadSSOString(addr uintptr) string {
	// SSO String is 24 bytes (0x18)
	// +0x10 : uint32 len
	// +0x14 : uint32 cap
	buf := m.ReadBytes(addr, 24)
	if len(buf) < 24 {
		return ""
	}

	strLen := binary.LittleEndian.Uint32(buf[0x10:0x14])
	strCap := binary.LittleEndian.Uint32(buf[0x14:0x18])

	if strCap > 15 {
		// It's a pointer at +0x00
		ptr := binary.LittleEndian.Uint32(buf[0x00:0x04])
		if ptr == 0 || strLen == 0 || strLen > 2048 {
			return ""
		}
		strBytes := m.ReadBytes(uintptr(ptr), strLen)
		return string(strBytes)
	}

	// Inline string
	if strLen > 15 {
		return ""
	}
	return string(buf[:strLen])
}

func extractString(m *QQMusicMem, buf []byte) string {
	if len(buf) < 24 {
		return ""
	}
	strLen := binary.LittleEndian.Uint32(buf[0x10:0x14])
	strCap := binary.LittleEndian.Uint32(buf[0x14:0x18])

	// Check if pointer
	if strCap > 15 && strCap < 0xFFFFFF && strLen > 0 && strLen < 4096 {
		ptr := binary.LittleEndian.Uint32(buf[0x00:0x04])
		if ptr > 0x10000 {
			s := m.ReadBytes(uintptr(ptr), strLen)
			return string(s)
		}
	}

	// Check if completely inline
	idx := bytes.IndexByte(buf, 0)
	if idx > 0 {
		return string(buf[:idx])
	}
	return ""
}

func sanitizeString(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r >= 32 && r != 127 { // only printable characters
			sb.WriteRune(r)
		}
	}
	return strings.TrimSpace(sb.String())
}

var lastCachedCookie string
var lastCookieTime time.Time

func (m *QQMusicMem) FindCookie() string {
	if lastCachedCookie != "" && time.Since(lastCookieTime) < 5*time.Minute {
		return lastCachedCookie
	}

	var mbi MEMORY_BASIC_INFORMATION
	addr := uintptr(0)
	pattern := []byte("qm_keyst=")

	for {
		ret, _, _ := procVirtualQueryEx.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&mbi)), uintptr(unsafe.Sizeof(mbi)))
		if ret == 0 {
			break
		}

		if mbi.State == MEM_COMMIT && (mbi.Protect == PAGE_READWRITE || mbi.Protect == PAGE_EXECUTE_READWRITE) {
			buf := make([]byte, mbi.RegionSize)
			var bytesRead uintptr
			procReadProcessMemory.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&buf[0])), mbi.RegionSize, uintptr(unsafe.Pointer(&bytesRead)))
			if bytesRead > 0 {
				idx := bytes.Index(buf[:bytesRead], pattern)
				if idx != -1 {
					// We found "qm_keyst=" ! Parse until ';' or '\0'
					start := idx
					end := start
					for end < int(bytesRead) && buf[end] != 0 && buf[end] != ';' && buf[end] != ' ' {
						end++
					}
					// Actually, let's grab the whole block like CheatEngine showed:
					// "qqmusic_key=...; qqmusic_uin=...; qm_keyst=..."
					// Let's just find the first \0 after idx and stringify the whole thing if it contains qqmusic_key.

					// Safest approach: Extract the full string starting from whatever looks like a cookie block.
					// We can walk left to see where it started.
					left := start
					for left > 0 && buf[left-1] != 0 && (buf[left-1] >= 32 && buf[left-1] <= 126) {
						left--
					}
					right := start
					for right < int(bytesRead) && buf[right] != 0 && (buf[right] >= 32 && buf[right] <= 126) {
						right++
					}

					// Full string
					cookieStr := string(buf[left:right])
					if strings.Contains(cookieStr, "qqmusic_key=") || strings.Contains(cookieStr, "qm_keyst=") {
						lastCachedCookie = cookieStr
						lastCookieTime = time.Now()
						log.Detail("提取到 Cookie: %s...", cookieStr[:min(50, len(cookieStr))])

						return cookieStr
					}
				}
			}
		}

		addr = mbi.BaseAddress + mbi.RegionSize
		// Keep searching reasonably
		if addr > 0x7FFFFFFF {
			break
		}
	}
	return ""
}

// FindSongMid extracts the current song's MID directly from QQ Music's internal
// JSON cache in process memory. CE analysis confirmed the JSON block format:
//
//	"remainingTime" : 182846,
//	"songPlayTime" : 183000,    ← this matches meta.DurationMs
//	"songmid" : "000mDR751jtpPf",
//
// We match songPlayTime against the known duration to identify the correct block.
func (m *QQMusicMem) FindSongMid(durationMs uint32) string {
	if durationMs == 0 {
		return ""
	}

	// 播放上报 JSON 有两种编码（见 extractSongMid），两种都要能匹配。用两个 playTime
	// needle 做区域快速排除：命中任一才对该区细扫，其余区直接跳过。
	needles := [][]byte{
		[]byte(fmt.Sprintf(`"songPlayTime":%d`, durationMs)),
		[]byte(fmt.Sprintf(`"songPlayTime" : %d`, durationMs)),
	}

	var mbi MEMORY_BASIC_INFORMATION
	addr := uintptr(0)
	for {
		ret, _, _ := procVirtualQueryEx.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&mbi)), uintptr(unsafe.Sizeof(mbi)))
		if ret == 0 {
			break
		}

		if mbi.State == MEM_COMMIT && (mbi.Protect == PAGE_READWRITE || mbi.Protect == PAGE_EXECUTE_READWRITE) && mbi.RegionSize < 100*1024*1024 {
			buf := make([]byte, mbi.RegionSize)
			var bytesRead uintptr
			procReadProcessMemory.Call(m.hProcess, addr, uintptr(unsafe.Pointer(&buf[0])), mbi.RegionSize, uintptr(unsafe.Pointer(&bytesRead)))
			if bytesRead > 0 {
				data := buf[:bytesRead]
				hit := false
				for _, n := range needles {
					if bytes.Contains(data, n) {
						hit = true
						break
					}
				}
				if hit {
					if mid := extractSongMid(data, durationMs); mid != "" {
						log.Detail("找到 songmid '%s' (songPlayTime=%d)", mid, durationMs)
						return mid
					}
				}
			}
		}

		addr = mbi.BaseAddress + mbi.RegionSize
		if addr > 0x7FFFFFFF {
			break
		}
	}
	return ""
}

// extractSongMid 从一块内存里按时长定位当前歌的 songMid。QQ 播放上报 JSON 两种编码：
//
//	紧凑（v22.41）："songPlayTime":189623,"songid":0,"songmid":"000S48Kb1DJdkJ"
//	带空格（旧）  ："songPlayTime" : 189623 ... "songmid" : "000S48Kb1DJdkJ"
//
// 以 songPlayTime==durationMs 锚定，取邻近（±500B）的 songmid——避免把别的歌的 mid
// 安到当前歌上。抽成纯函数是为了可测：这是「一错就整首放错歌词」的路径。
// playTime 命中后校验其后一字节非数字，挡掉「137223 命中 1372230 前缀」的误匹配。
func extractSongMid(data []byte, durationMs uint32) string {
	formats := []struct{ playTime, mid string }{
		{fmt.Sprintf(`"songPlayTime":%d`, durationMs), `"songmid":"`},
		{fmt.Sprintf(`"songPlayTime" : %d`, durationMs), `"songmid" : "`},
	}
	for _, f := range formats {
		needle := []byte(f.playTime)
		pt := bytes.Index(data, needle)
		if pt < 0 {
			continue
		}
		if after := pt + len(needle); after < len(data) && data[after] >= '0' && data[after] <= '9' {
			continue // playTime 是更长数字的前缀（如 137223 命中 1372230），非精确匹配
		}
		lo := pt - 500
		if lo < 0 {
			lo = 0
		}
		hi := pt + 500
		if hi > len(data) {
			hi = len(data)
		}
		window := data[lo:hi]
		mi := bytes.Index(window, []byte(f.mid))
		if mi < 0 {
			continue
		}
		start := mi + len(f.mid)
		end := bytes.IndexByte(window[start:], '"')
		if end <= 0 {
			continue
		}
		mid := string(window[start : start+end])
		if len(mid) >= 10 && len(mid) <= 20 {
			return mid
		}
	}
	return ""
}

func (m *QQMusicMem) ReadAllMetadata() (*SongMetadata, error) {
	if m.qqmusicDllBase == 0 {
		return nil, errors.New("process not attached")
	}

	meta := &SongMetadata{}

	// Read Struct 1 (C87C80)
	extractSSO := func(addr uintptr) string {
		buf := m.ReadBytes(addr, 24)
		if len(buf) < 24 {
			return ""
		}
		return ssoFromBuf(buf, func(ptr, n uint32) []byte {
			return m.ReadBytes(uintptr(ptr), n)
		})
	}

	var name1, singer1 string
	var songId, duration, progress uint32

	// CE verified: struct is directly embedded at QQMusic.dll+offsets.Struct1, NOT a pointer
	struct1 := m.qqmusicDllBase + m.offsets.Struct1

	if m.offsets.UseWideStrings {
		// v20.05+: 歌名/歌手字段为 WCHAR* 指针，指向堆上的 UTF-16 字符串
		namePtr := m.ReadUint32(struct1 + m.offsets.NameOff)
		if namePtr != 0 {
			name1 = m.ReadWideString(uintptr(namePtr), 256)
		}
		singerPtr := m.ReadUint32(struct1 + m.offsets.SingerOff)
		if singerPtr != 0 {
			singer1 = m.ReadWideString(uintptr(singerPtr), 256)
		}
	} else {
		name1 = extractSSO(struct1 + m.offsets.NameOff)
		singer1 = extractSSO(struct1 + m.offsets.SingerOff)
	}

	if len(name1) > 1 {
		meta.Name = name1
	}
	if len(singer1) > 1 {
		meta.Singer = singer1
	}

	duration = m.ReadUint32(struct1 + m.offsets.DurationOff)
	if m.offsets.DurationInSeconds {
		duration *= 1000 // 秒转毫秒（v20.05 时长单位为秒）
	}

	songId = m.ReadUint32(struct1 + m.offsets.SongIDOff)
	if m.offsets.SongIDDurCheckOff != 0 {
		// v22.41：稳定 songID 在独立的「播放会话」结构里（见 knownVersions 注释），换歌瞬间
		// 可能短暂滞后于显示结构体。用它自带的时长与显示时长交叉核对：不一致=该 songID 尚未
		// 落位到当前歌 → 作废（songId=0），让上层回退到 songMid 堆扫描。绝不拿上一首的 songID
		// 去请求——那会整首推错歌词，比空白更糟。稳态下两处时长精确相等（CE 实测）。
		if m.ReadUint32(struct1+m.offsets.SongIDDurCheckOff) != duration {
			songId = 0
		}
	}

	// Fast progress timer: QQMusic.dll+offsets.FastTimerPtr -> [ptr]+offsets.FastTimerOff
	// Updates every ~1 second (vs slow timer which updates every ~3-5 seconds)
	if m.progressPtr != 0 {
		// AOB Hook 注入的精确进度计数器（优先）
		progress = m.ReadUint32(m.progressPtr)
	} else if m.offsets.FastTimerPtr != 0 {
		fastTimerPtr := uintptr(m.ReadUint32(m.qqmusicDllBase + m.offsets.FastTimerPtr))
		if fastTimerPtr > 0x10000 {
			progress = m.ReadUint32(fastTimerPtr + m.offsets.FastTimerOff)
		} else {
			progress = m.ReadUint32(struct1 + m.offsets.ProgressOff)
		}
	} else if m.offsets.ProgressDllOff != 0 {
		// v20.05: DLL 基址直接偏移，DWORD 即准确播放毫秒数
		progress = m.ReadUint32(m.qqmusicDllBase + m.offsets.ProgressDllOff)
	} else {
		// 回退：使用结构体内慢速进度字段
		progress = m.ReadUint32(struct1 + m.offsets.ProgressOff)
	}

	if songId > 0 && songId < 0x3F000000 {
		meta.SongID = songId
	}
	meta.DurationMs = duration
	meta.ProgressMs = progress

	// v20.05 songMid 提取（两路，优先用流 URL）
	// 路径1: struct1+StreamURLOff → WCHAR* → "http://.../00281PXu4DHKNp.wma"
	//        取最后 '/' 之后、最后 '.' 之前的部分即 songMid
	if m.offsets.StreamURLOff != 0 && meta.SongMid == "" {
		urlPtr := m.ReadUint32(struct1 + m.offsets.StreamURLOff)
		if urlPtr > 0x10000 {
			urlStr := m.ReadWideString(uintptr(urlPtr), 256)
			if slash := strings.LastIndex(urlStr, "/"); slash >= 0 {
				filename := urlStr[slash+1:]
				if dot := strings.Index(filename, "."); dot > 0 {
					mid := filename[:dot]
					if len(mid) >= 10 && len(mid) <= 20 {
						meta.SongMid = mid
					}
				}
			}
		}
	}
	// 路径2（备选）: struct1+SongMidParamsOff → WCHAR* → "0=<songMid>&2=..."
	if m.offsets.SongMidParamsOff != 0 && meta.SongMid == "" {
		paramsPtr := m.ReadUint32(struct1 + m.offsets.SongMidParamsOff)
		if paramsPtr > 0x10000 {
			paramsStr := m.ReadWideString(uintptr(paramsPtr), 128)
			if idx := strings.Index(paramsStr, "0="); idx != -1 {
				rest := paramsStr[idx+2:]
				end := strings.IndexByte(rest, '&')
				if end == -1 {
					end = len(rest)
				}
				mid := rest[:end]
				if len(mid) > 0 {
					meta.SongMid = mid
				}
			}
		}
	}

	// Read Struct 2 (backup source, not available in all versions)
	var name2, singer2 string
	if m.offsets.Struct2Ptr != 0 {
		struct2 := uintptr(m.ReadUint32(m.qqmusicDllBase + m.offsets.Struct2Ptr))
		if struct2 != 0 {
			namePtr := m.ReadUint32(struct2 + 0x64)
			if namePtr != 0 {
				name2 = m.ReadWideString(uintptr(namePtr), 256)
			}
			singerPtr := m.ReadUint32(struct2 + 0x68)
			if singerPtr != 0 {
				singer2 = m.ReadWideString(uintptr(singerPtr), 256)
			}
		}
	}

	name := name2
	if len(name) < 2 {
		name = name1
	}
	singer := singer2
	if len(singer) < 2 {
		singer = singer1
	}

	// sanitize
	name = sanitizeString(name)
	singer = sanitizeString(singer)

	// Read dynamic slider value
	var sliderVal uint32
	if m.sliderPointer != 0 {
		edi := m.ReadUint32(m.sliderPointer)
		if edi != 0 {
			sliderVal = m.ReadUint32(uintptr(edi) + 0xF0)
		}
	}

	return &SongMetadata{
		Name:       name,
		Singer:     singer,
		SongID:     songId,
		SongMid:    meta.SongMid,
		ProgressMs: progress,
		DurationMs: duration,
		SliderVal:  sliderVal,
	}, nil
}
