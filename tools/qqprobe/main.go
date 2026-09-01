//go:build windows

// Command qqprobe 是一次性的 QQ 音乐内存偏移探测工具，用于量出 knownVersions
// 尚未收录的版本（当前目标 22.60）的 dllOffsets。它 attach 本机正在播放的 QQ 音乐进程，
// 以当前歌曲的歌名 / 歌手为锚点，自动定位 QQMusic.dll 内的显示结构体与「播放会话」结构体，
// 打印人类可读的探测报告，并给出一段可直接粘进 player/qqmusic/mem.go 的
// "22.60": dllOffsets{...} Go 字面量（字面量中的数值全部为运行时实测填入）。
//
// 绝对只读约束（AGENTS.md §0 第一条 + issue #39）：
//
//	本工具 attach 的是主播播控机上正直播的 QQ 音乐。它只调用 ReadProcessMemory /
//	VirtualQueryEx / EnumProcessModulesEx 等只读原语，绝不 WriteProcessMemory /
//	VirtualAllocEx / VirtualProtectEx / 任何注入。为在 OS 层面加固这条红线，
//	OpenProcess 只申请 PROCESS_QUERY_INFORMATION | PROCESS_VM_READ，不申请任何写权限——
//	即便代码里误写了写调用，句柄也没有写权限，写不进去。
//
// 本工具自包含：所需的 Win32 薄封装照抄自 player/qqmusic/mem.go 已在生产验证过的实现，
// 不 import player/qqmusic，也不改动任何生产代码。仅 Windows 构建，随手跑不进出货二进制。
//
// 用法：
//
//	go run ./tools/qqprobe                       // 自动定位 QQMusic.exe，用默认锚点
//	go run ./tools/qqprobe --name 歌名 --singer 歌手
//	go run ./tools/qqprobe --pid 44852 --songid 351669598
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// ---------------------------------------------------------------------------
// Win32 薄封装（照抄 player/qqmusic/mem.go；仅保留只读所需的 proc，刻意不声明
// WriteProcessMemory / VirtualAllocEx / VirtualProtectEx）
// ---------------------------------------------------------------------------

var (
	modkernel32              = syscall.NewLazyDLL("kernel32.dll")
	procCreateToolhelp32     = modkernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32First       = modkernel32.NewProc("Process32FirstW")
	procProcess32Next        = modkernel32.NewProc("Process32NextW")
	procOpenProcess          = modkernel32.NewProc("OpenProcess")
	procCloseHandle          = modkernel32.NewProc("CloseHandle")
	procReadProcessMemory    = modkernel32.NewProc("ReadProcessMemory")
	procVirtualQueryEx       = modkernel32.NewProc("VirtualQueryEx")
	procQueryFullProcessName = modkernel32.NewProc("QueryFullProcessImageNameW")

	modpsapi                 = syscall.NewLazyDLL("psapi.dll")
	procEnumProcessModulesEx = modpsapi.NewProc("EnumProcessModulesEx")
	procGetModuleBaseNameW   = modpsapi.NewProc("GetModuleBaseNameW")
	procGetModuleInformation = modpsapi.NewProc("GetModuleInformation")

	modversion                  = syscall.NewLazyDLL("version.dll")
	procGetFileVersionInfoSizeW = modversion.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfoW     = modversion.NewProc("GetFileVersionInfoW")
	procVerQueryValueW          = modversion.NewProc("VerQueryValueW")
)

const (
	// 只读访问权限：EnumProcessModulesEx / GetModuleInformation / VirtualQueryEx 需
	// PROCESS_QUERY_INFORMATION，ReadProcessMemory 需 PROCESS_VM_READ。刻意不申请
	// PROCESS_VM_WRITE / PROCESS_VM_OPERATION —— 见文件头的只读红线。
	PROCESS_QUERY_INFORMATION = 0x0400
	PROCESS_VM_READ           = 0x0010

	TH32CS_SNAPPROCESS = 0x00000002
	LIST_MODULES_ALL   = 0x03

	MEM_COMMIT = 0x1000

	PAGE_NOACCESS          = 0x01
	PAGE_READONLY          = 0x02
	PAGE_READWRITE         = 0x04
	PAGE_WRITECOPY         = 0x08
	PAGE_EXECUTE_READ      = 0x20
	PAGE_EXECUTE_READWRITE = 0x40
	PAGE_EXECUTE_WRITECOPY = 0x80
	PAGE_GUARD             = 0x100
	PAGE_NOCACHE           = 0x200
	PAGE_WRITECOMBINE      = 0x400

	// 32 位目标地址空间上界；扫描到此为止（也防 addr 回绕死循环）。
	addrCeil = uint64(0xFFFF0000)
	// 单区一次性读取上限，避免超大保留区触发巨额分配。
	maxRegion = uint64(256 * 1024 * 1024)

	minDurMs = 1000            // 合理时长下界（1 秒）
	maxDurMs = 2 * 3600 * 1000 // 合理时长上界（2 小时）
	// 稳定 songID 的合理上界，抄 ReadAllMetadata 的 songId < 0x3F000000 判定。
	maxSongID = uint32(0x3F000000)
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

// vsFixedFileInfo 用于从 PE 版本资源里读 ProductVersion（照抄 mem.go）。
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

// ---------------------------------------------------------------------------
// 只读内存原语
// ---------------------------------------------------------------------------

type target struct {
	h       uintptr
	pid     uint32
	dllBase uintptr
	dllSize uint32
	version string // QQMusic.exe 的 ProductVersion，读不到为 "unknown"
}

// readMem 只读拷贝目标进程 [addr, addr+size) 的字节。容忍 ERROR_PARTIAL_COPY：
// 只要读到 >0 字节就返回（字符串位于可读区末尾时会遇到），与生产逻辑一致。
func (t *target) readMem(addr uintptr, size uint32) ([]byte, bool) {
	if size == 0 {
		return nil, false
	}
	buf := make([]byte, size)
	var n uintptr
	procReadProcessMemory.Call(t.h, addr,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(size), uintptr(unsafe.Pointer(&n)))
	if n == 0 {
		return nil, false
	}
	return buf[:n], true
}

// readU32 读一个 32 位小端值（目标为 32 位进程，指针恒 4 字节）。失败返回 0。
func (t *target) readU32(addr uintptr) uint32 {
	b, ok := t.readMem(addr, 4)
	if !ok || len(b) < 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

// readWide 读一个以 NUL 结尾的 UTF-16LE 堆字符串，最多 maxChars 个码元。
func (t *target) readWide(addr uintptr, maxChars uint32) string {
	if addr <= 0x10000 {
		return ""
	}
	b, ok := t.readMem(addr, maxChars*2)
	if !ok || len(b) < 2 {
		return ""
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// readImage 分块只读整块 DLL 映像到一个连续缓冲区，按段容错（读不到的页留零）。
// 返回缓冲区下标 i 对应目标地址 dllBase+i，便于后续精确算偏移。
func (t *target) readImage() []byte {
	buf := make([]byte, t.dllSize)
	const chunk = 0x10000
	for off := uint32(0); off < t.dllSize; off += chunk {
		n := uint32(chunk)
		if off+n > t.dllSize {
			n = t.dllSize - off
		}
		if data, ok := t.readMem(t.dllBase+uintptr(off), n); ok {
			copy(buf[off:], data)
		}
	}
	return buf
}

// walkRegions 遍历目标进程所有 MEM_COMMIT 且可读的区，逐区只读拷贝后回调。
// 遍历骨架照抄 FindSongMid：VirtualQueryEx 步进 + 区内 ReadProcessMemory。
func (t *target) walkRegions(fn func(base uintptr, data []byte)) {
	var addr uintptr
	for {
		var mbi MEMORY_BASIC_INFORMATION
		ret, _, _ := procVirtualQueryEx.Call(t.h, addr,
			uintptr(unsafe.Pointer(&mbi)), unsafe.Sizeof(mbi))
		if ret == 0 {
			break
		}
		next := uint64(mbi.BaseAddress) + uint64(mbi.RegionSize)
		if mbi.State == MEM_COMMIT && isReadable(mbi.Protect) &&
			uint64(mbi.RegionSize) > 0 && uint64(mbi.RegionSize) < maxRegion {
			if data, ok := t.readMem(mbi.BaseAddress, uint32(mbi.RegionSize)); ok {
				fn(mbi.BaseAddress, data)
			}
		}
		// 无进展 / 回绕 / 越过 32 位空间上界都终止（防死循环）。
		if next <= uint64(addr) || next > addrCeil {
			break
		}
		addr = uintptr(next)
	}
}

// isReadable 判定页保护是否可安全只读：排除 PAGE_NOACCESS 与 PAGE_GUARD
// （读 guard 页会失败/触发目标侧异常语义），其余可读保护一律接受。
func isReadable(protect uint32) bool {
	if protect&PAGE_GUARD != 0 || protect&PAGE_NOACCESS != 0 {
		return false
	}
	switch protect &^ (PAGE_GUARD | PAGE_NOCACHE | PAGE_WRITECOMBINE) {
	case PAGE_READONLY, PAGE_READWRITE, PAGE_WRITECOPY,
		PAGE_EXECUTE_READ, PAGE_EXECUTE_READWRITE, PAGE_EXECUTE_WRITECOPY:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 进程 / 模块定位（照抄 ConnectQQMusic 的 attach 流程，含 cbNeeded 截断处理）
// ---------------------------------------------------------------------------

// openTarget 定位并只读打开 QQMusic 进程，取 QQMusic.dll 基址与大小。
// wantPID==0 时自动枚举所有 QQMusic.exe，选第一个含 QQMusic.dll 的。
func openTarget(wantPID uint32) (*target, error) {
	var pids []uint32
	if wantPID != 0 {
		pids = []uint32{wantPID}
	} else {
		var err error
		pids, err = enumQQMusicPIDs()
		if err != nil {
			return nil, err
		}
		if len(pids) == 0 {
			return nil, fmt.Errorf("未找到 QQMusic.exe 进程")
		}
	}

	for _, pid := range pids {
		h, _, _ := procOpenProcess.Call(
			uintptr(PROCESS_QUERY_INFORMATION|PROCESS_VM_READ), 0, uintptr(pid))
		if h == 0 {
			continue // 权限不足或进程已退出，换下一个
		}
		base, size, ok := findDLL(h)
		if ok {
			t := &target{h: h, pid: pid, dllBase: base, dllSize: size}
			t.version = readExeVersion(h)
			return t, nil
		}
		procCloseHandle.Call(h)
	}
	return nil, fmt.Errorf("QQMusic.dll 未在候选进程中找到（pid=%v）", pids)
}

func enumQQMusicPIDs() ([]uint32, error) {
	hSnap, _, _ := procCreateToolhelp32.Call(uintptr(TH32CS_SNAPPROCESS), 0)
	if hSnap == uintptr(syscall.InvalidHandle) {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot 失败")
	}
	defer procCloseHandle.Call(hSnap)

	var pe PROCESSENTRY32W
	pe.Size = uint32(unsafe.Sizeof(pe))
	var pids []uint32
	ret, _, _ := procProcess32First.Call(hSnap, uintptr(unsafe.Pointer(&pe)))
	for ret != 0 {
		if strings.EqualFold(syscall.UTF16ToString(pe.ExeFile[:]), "qqmusic.exe") {
			pids = append(pids, pe.ProcessID)
		}
		ret, _, _ = procProcess32Next.Call(hSnap, uintptr(unsafe.Pointer(&pe)))
	}
	return pids, nil
}

// findDLL 枚举模块找到 QQMusic.dll，返回基址与映像大小。
func findDLL(h uintptr) (base uintptr, size uint32, ok bool) {
	var modules [1024]uintptr
	var cbNeeded uint32
	ret, _, _ := procEnumProcessModulesEx.Call(
		h,
		uintptr(unsafe.Pointer(&modules[0])),
		unsafe.Sizeof(modules),
		uintptr(unsafe.Pointer(&cbNeeded)),
		uintptr(LIST_MODULES_ALL),
	)
	if ret == 0 {
		return 0, 0, false
	}
	// cbNeeded 是「所需」而非「已写入」字节数：模块数 >1024 时仍返回 TRUE，只把需要的
	// 大小写进 cbNeeded。不截断则越界 panic（照抄 mem.go 第 397-409 行的处理）。
	numModules := cbNeeded / uint32(unsafe.Sizeof(modules[0]))
	if numModules > uint32(len(modules)) {
		numModules = uint32(len(modules))
	}
	for i := uint32(0); i < numModules; i++ {
		var name [256]uint16
		procGetModuleBaseNameW.Call(h, modules[i],
			uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
		if strings.EqualFold(syscall.UTF16ToString(name[:]), "qqmusic.dll") {
			var mi MODULEINFO
			procGetModuleInformation.Call(h, modules[i],
				uintptr(unsafe.Pointer(&mi)), unsafe.Sizeof(mi))
			return modules[i], mi.SizeOfImage, true
		}
	}
	return 0, 0, false
}

// readExeVersion 尽力读出 QQMusic.exe 的 ProductVersion（major.minor），仅供报告
// 头部核对目标版本，读不到返回 "unknown"。全部为只读信息查询。
func readExeVersion(h uintptr) string {
	buf := make([]uint16, 512)
	size := uint32(len(buf))
	ret, _, _ := procQueryFullProcessName.Call(h, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if ret == 0 || size == 0 {
		return "unknown"
	}
	path := syscall.UTF16ToString(buf[:size])

	pathUTF16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "unknown"
	}
	infoSize, _, _ := procGetFileVersionInfoSizeW.Call(uintptr(unsafe.Pointer(pathUTF16)), 0)
	if infoSize == 0 {
		return "unknown"
	}
	data := make([]byte, infoSize)
	ret, _, _ = procGetFileVersionInfoW.Call(uintptr(unsafe.Pointer(pathUTF16)), 0,
		infoSize, uintptr(unsafe.Pointer(&data[0])))
	if ret == 0 {
		return "unknown"
	}
	sub, _ := syscall.UTF16PtrFromString(`\`)
	var infoPtr uintptr
	var infoLen uint32
	ret, _, _ = procVerQueryValueW.Call(
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(unsafe.Pointer(sub)),
		uintptr(unsafe.Pointer(&infoPtr)),
		uintptr(unsafe.Pointer(&infoLen)))
	if ret == 0 || infoPtr == 0 {
		return "unknown"
	}
	// infoPtr 指向 data 内部；算偏移回 data 取地址，避免 GC 提前回收 data（照抄 mem.go）。
	baseP := uintptr(unsafe.Pointer(&data[0]))
	if infoPtr < baseP {
		return "unknown"
	}
	off := infoPtr - baseP
	if off+unsafe.Sizeof(vsFixedFileInfo{}) > uintptr(len(data)) {
		return "unknown"
	}
	info := (*vsFixedFileInfo)(unsafe.Pointer(&data[off]))
	if info.Signature != 0xFEEF04BD {
		return "unknown"
	}
	return fmt.Sprintf("%d.%02d", uint16(info.ProductVersionMS>>16), uint16(info.ProductVersionMS&0xFFFF))
}

// ---------------------------------------------------------------------------
// 扫描 / 编码工具
// ---------------------------------------------------------------------------

// utf16le 把字符串编码为 UTF-16LE 字节串（堆宽字符锚点）。
func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[i*2:], c)
	}
	return b
}

// gbkBytes 把字符串编码为 GBK 字节串（窄串对照锚点）。失败返回 nil。
func gbkBytes(s string) []byte {
	b, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
	if err != nil {
		return nil
	}
	return b
}

// collectWide 在一块内存里找 needle（UTF-16 串）所有偶对齐、且尾随 UTF-16 NUL 的
// 出现位置，累加其绝对地址到 out（seen 去重）。偶对齐 + NUL 结尾过滤掉「串中子串」噪声。
func collectWide(data []byte, base uintptr, needle []byte, seen map[uintptr]bool, out *[]uintptr) {
	if len(needle) == 0 {
		return
	}
	for i := 0; ; {
		j := bytes.Index(data[i:], needle)
		if j < 0 {
			break
		}
		pos := i + j
		if pos%2 == 0 {
			end := pos + len(needle)
			terminated := end+2 > len(data) || (data[end] == 0 && data[end+1] == 0)
			addr := base + uintptr(pos)
			if terminated && !seen[addr] {
				seen[addr] = true
				*out = append(*out, addr)
			}
		}
		i = pos + 1
	}
}

// countBytes 统计 needle 在 data 中的出现次数（窄串命中计数，不做对齐过滤）。
func countBytes(data, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
	n := 0
	for i := 0; ; {
		j := bytes.Index(data[i:], needle)
		if j < 0 {
			break
		}
		n++
		i = i + j + 1
	}
	return n
}

// findU32InImage 在 DLL 映像缓冲区里找所有 4 字节对齐、值 == val 的位置，返回绝对地址。
// 4 字节对齐符合 MSVC 结构体内 DWORD / 指针字段的自然对齐，用以压制误命中。
func findU32InImage(img []byte, base uintptr, val uint32) []uintptr {
	var needle [4]byte
	binary.LittleEndian.PutUint32(needle[:], val)
	var out []uintptr
	for i := 0; i+4 <= len(img); {
		j := bytes.Index(img[i:], needle[:])
		if j < 0 {
			break
		}
		pos := i + j
		if pos%4 == 0 {
			out = append(out, base+uintptr(pos))
		}
		i = pos + 1
	}
	return out
}

// looksText 判定一个解码后的字符串是否像正常文本：非空、无控制字符、无 UTF-16 解码失败标记。
func looksText(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
		if r < 0x20 && r != '\t' {
			return false
		}
	}
	return true
}

func containsCI(hay, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(hay), strings.ToLower(needle))
}

// ---------------------------------------------------------------------------
// 阶段 A：定位显示结构体 struct1 及字段布局
// ---------------------------------------------------------------------------

// 22.41 / 22.52 宽字符模型下的字段相对偏移（相对 struct1 基址）。
const (
	offName     = uintptr(0x00)
	offSinger   = uintptr(0x04)
	offAlbum    = uintptr(0x08)
	offProgress = uintptr(0x0C)
	offDuration = uintptr(0x10)
)

// structCand 是一个 struct1 基址候选及其按模型读回的证据。
type structCand struct {
	base                uintptr // struct1 绝对地址（= dllBase + Struct1）
	name, singer, album string
	prog, dur           uint32
	singerPtrHit        bool // struct1+offSinger 精确指向已知歌手串地址
	score               int
}

// validateStruct 按 22.41 宽字符模型读回 struct1 各字段并打分。歌名 / 歌手命中权重最高，
// 时长合理性与「歌手指针精确落在已知歌手串地址」为强证据。
func validateStruct(t *target, base uintptr, expName, expSinger string, singerSet map[uintptr]bool) structCand {
	c := structCand{base: base}
	namePtr := t.readU32(base + offName)
	singerPtr := t.readU32(base + offSinger)
	albumPtr := t.readU32(base + offAlbum)
	c.prog = t.readU32(base + offProgress)
	c.dur = t.readU32(base + offDuration)
	c.name = t.readWide(uintptr(namePtr), 128)
	c.singer = t.readWide(uintptr(singerPtr), 128)
	c.album = t.readWide(uintptr(albumPtr), 128)
	c.singerPtrHit = singerSet[uintptr(singerPtr)]

	if containsCI(c.name, expName) && expName != "" {
		c.score += 400
	}
	if c.singerPtrHit {
		c.score += 300
	}
	if containsCI(c.singer, expSinger) && expSinger != "" {
		c.score += 200
	}
	if looksText(c.singer) {
		c.score += 40
	}
	if looksText(c.album) {
		c.score += 30
	}
	if c.dur >= minDurMs && c.dur <= maxDurMs {
		c.score += 80
	}
	if c.prog <= c.dur+2000 {
		c.score += 20
	}
	return c
}

// modelMatched 判定候选是否足够可信可直接采用 22.41 模型。
func (c structCand) modelMatched(expName, expSinger string) bool {
	if !containsCI(c.name, expName) || expName == "" {
		return false
	}
	return c.singerPtrHit ||
		(expSinger != "" && containsCI(c.singer, expSinger)) ||
		(c.dur >= minDurMs && c.dur <= maxDurMs)
}

// phaseA 汇集 struct1 候选（歌名槽位锚 + 歌手槽位锚），验证后返回最佳候选。
func phaseA(t *target, dllData []byte, nameAddrs, singerAddrs []uintptr, singerSet map[uintptr]bool,
	expName, expSinger string) (best structCand, all []structCand, found bool) {

	seen := map[uintptr]bool{}
	consider := func(base uintptr) {
		if seen[base] {
			return
		}
		seen[base] = true
		all = append(all, validateStruct(t, base, expName, expSinger, singerSet))
	}

	// 歌名锚：DLL 内保存 namePtr 的槽位 P 即 struct1+offName，故 struct1 = P - offName。
	for _, na := range nameAddrs {
		for _, p := range findU32InImage(dllData, t.dllBase, uint32(na)) {
			if p >= offName {
				consider(p - offName)
			}
		}
	}
	// 歌手锚（歌名扫不到时的对称兜底）：struct1 = P - offSinger。
	for _, sa := range singerAddrs {
		for _, p := range findU32InImage(dllData, t.dllBase, uint32(sa)) {
			if p >= offSinger {
				consider(p - offSinger)
			}
		}
	}

	if len(all) == 0 {
		return structCand{}, nil, false
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	return all[0], all, true
}

// discovered 是模型不吻合时、在歌名槽位附近实测到的字段偏移（相对歌名字段，NameOff 锚定为 0）。
type discovered struct {
	singerOff, albumOff   uintptr
	progOff, durOff       uintptr
	haveSinger, haveAlbum bool
	haveProg, haveDur     bool
}

// discoverOffsets 在歌名槽位 nameSlot 附近小范围实测各字段真实相对偏移（不强套模型）。
// 用于阶段 A 的 22.41 模型验证失败时如实报告布局漂移。
//
// 扫描从歌名槽位（delta=0）向后：所有已知宽字符版本（22.31/22.41/22.52）歌名都是首字段
// （NameOff=0），其余字段均在其后。据此把 Struct1 锚定在歌名槽位、各偏移非负，恰好对应
// 输出字面量的 NameOff=0 布局；若 22.60 意外把歌名挪离首位，本兜底会读不全 → 报告如实反映，
// 交主会话上 CE 手工核。
func discoverOffsets(t *target, nameSlot uintptr, singerSet map[uintptr]bool, expDur uint32) discovered {
	var d discovered
	const hi = 0x140
	for delta := 0; delta <= hi; delta += 4 {
		addr := nameSlot + uintptr(delta)
		v := t.readU32(addr)
		// 歌手：指针精确落在已知歌手串地址。
		if !d.haveSinger && singerSet[uintptr(v)] {
			d.singerOff = uintptr(delta)
			d.haveSinger = true
		}
		// 时长：DWORD 恰等于阶段 A 实测时长（expDur>0 时才可靠）。
		if !d.haveDur && expDur > 0 && v == expDur {
			d.durOff = uintptr(delta)
			d.haveDur = true
		}
	}
	// 专辑：歌手之后紧邻的指针槽，且能解引用出正常文本。
	if d.haveSinger {
		ap := t.readU32(nameSlot + d.singerOff + 4)
		if looksText(t.readWide(uintptr(ap), 128)) {
			d.albumOff = d.singerOff + 4
			d.haveAlbum = true
		}
	}
	// 进度：时长前一个 DWORD（模型里 Prog 紧邻 Dur 之前），需 <= 时长。
	if d.haveDur && d.durOff >= 4 {
		pd := d.durOff - 4
		if t.readU32(nameSlot+pd) <= expDur {
			d.progOff = pd
			d.haveProg = true
		}
	}
	return d
}

// ---------------------------------------------------------------------------
// 阶段 B：定位 songID「播放会话」结构
// ---------------------------------------------------------------------------

// sessCand 是一个「播放会话」结构候选：songID 字段与其后 +8 的自带时长字段。
type sessCand struct {
	songID       uint32
	songIDAddr   uintptr
	durCheckAddr uintptr
	floatScore   int  // songID 之前 volume/speed 浮点特征分
	exact        bool // 与 --songid 精确匹配
	score        int
}

// phaseB 用阶段 A 实测时长在 DLL 静态区定位「播放会话」结构。
// 模型：结构内 volume/speed 浮点 + songID(DWORD) + (+4 保留) + 时长(DWORD)。
// 时长字段距 songID +8，故命中时长地址 D 时 songID 位于 D-8。
func phaseB(t *target, dllData []byte, struct1Base uintptr, durMs, wantSongID uint32) []sessCand {
	if durMs == 0 {
		return nil
	}
	// 显示结构体自带的时长字段（struct1+offDuration）会同样命中，须排除，避免与
	// 播放会话混淆——它前 8 字节是 albumPtr 而非 songID。
	displayDurAddr := struct1Base + offDuration

	var cands []sessCand
	for _, d := range findU32InImage(dllData, t.dllBase, durMs) {
		if d == displayDurAddr {
			continue
		}
		pos := int(d - t.dllBase)
		if pos-8 < 0 {
			continue
		}
		songID := binary.LittleEndian.Uint32(dllData[pos-8:])
		if songID == 0 || songID >= maxSongID {
			continue // 非合理正 songID
		}
		c := sessCand{
			songID:       songID,
			songIDAddr:   d - 8,
			durCheckAddr: d,
			floatScore:   floatFeature(dllData, pos-8),
		}
		if wantSongID != 0 && songID == wantSongID {
			c.exact = true
		}

		// 综合打分：精确匹配 --songid 权重最高；其次浮点特征；再次合理性。
		if c.exact {
			c.score += 1000
		}
		c.score += c.floatScore
		c.score += 50 // 已过 songID 合理性闸门
		if songID > 1_000_000 {
			c.score += 30 // 真实 QQ songID 通常为较大整数
		}
		cands = append(cands, c)
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	return cands
}

// floatFeature 检查 songID 之前若干 DWORD 是否有 volume/speed 浮点特征（(0,4] 内，
// 或恰为 1.0=0x3F800000），作为「播放会话」结构的软证据。
func floatFeature(img []byte, songIDPos int) int {
	score := 0
	for k := 1; k <= 4; k++ {
		p := songIDPos - 4*k
		if p < 0 {
			break
		}
		v := binary.LittleEndian.Uint32(img[p:])
		if v == 0x3F800000 { // float 1.0
			score += 60
			continue
		}
		f := math.Float32frombits(v)
		if f > 0 && f <= 4.0 {
			score += 20
		}
	}
	return score
}

// ---------------------------------------------------------------------------
// 报告 + Go 字面量输出
// ---------------------------------------------------------------------------

func emitLiteral(t *target, s1 structCand, dm discovered, modelMatched bool, sess []sessCand) {
	struct1Off := s1.base - t.dllBase

	sOff, aOff, pOff, durOff := offSinger, offAlbum, offProgress, offDuration
	layoutNote := "22.41/22.52 宽字符模型逐字段吻合"
	if !modelMatched {
		layoutNote = "22.41 模型不吻合，以下为歌名槽位附近实测偏移（歌名字段锚定为 0）"
		if dm.haveSinger {
			sOff = dm.singerOff
		}
		if dm.haveAlbum {
			aOff = dm.albumOff
		}
		if dm.haveProg {
			pOff = dm.progOff
		}
		if dm.haveDur {
			durOff = dm.durOff
		}
	}

	var songIDOff, durCheckOff int64
	songNote := "阶段B未定位，需主会话用真 songID 重跑后填入"
	if len(sess) > 0 {
		top := sess[0]
		songIDOff = int64(top.songIDAddr) - int64(s1.base)
		durCheckOff = int64(top.durCheckAddr) - int64(s1.base)
		songNote = fmt.Sprintf("候选 songID=%d（%s），主会话请用 QQ 分享链接核对",
			top.songID, ternary(top.exact, "与 --songid 精确匹配", "时长+浮点特征命中"))
	}

	fmt.Println()
	fmt.Println("========== 可粘贴进 knownVersions 的 Go 字面量 ==========")
	fmt.Printf("// qqprobe 实测（32-bit x86，%s）：%s\n", time.Now().Format("2006-01-02"), layoutNote)
	fmt.Printf("// %s\n", songNote)
	fmt.Println(`"22.60": {`)
	fmt.Printf("\tStruct1:           0x%X,\n", struct1Off)
	fmt.Printf("\tNameOff:           0x%02X,\n", offName)
	fmt.Printf("\tSingerOff:         0x%02X,\n", sOff)
	fmt.Printf("\tAlbumOff:          0x%02X,\n", aOff)
	fmt.Printf("\tProgressOff:       0x%02X,\n", pOff)
	fmt.Printf("\tDurationOff:       0x%02X,\n", durOff)
	if len(sess) > 0 {
		fmt.Printf("\tSongIDOff:         0x%X,\n", songIDOff)
		fmt.Printf("\tSongIDDurCheckOff: 0x%X,\n", durCheckOff)
	} else {
		fmt.Printf("\tSongIDOff:         0x0, // %s\n", songNote)
		fmt.Printf("\tSongIDDurCheckOff: 0x0,\n")
	}
	fmt.Println("\tUseWideStrings:    true,")
	fmt.Println("\tSongMidFromHeap:   true,")
	fmt.Println("},")
	fmt.Println("========================================================")
}

func ternary(b bool, a, c string) string {
	if b {
		return a
	}
	return c
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(0)
	log.SetPrefix("[qqprobe] ")

	pid := flag.Uint("pid", 0, "目标 QQMusic.exe pid（0=自动枚举）")
	name := flag.String("name", "奶茶加糖", "当前播放歌名（UTF-16 锚点）")
	singer := flag.String("singer", "贰茉EMO", "当前播放歌手（UTF-16 锚点 / 交叉验证）")
	songid := flag.Uint("songid", 0, "已知真实 songID（可选，uint32，0=不用）")
	flag.Parse()

	t, err := openTarget(uint32(*pid))
	if err != nil {
		log.Fatalf("attach 失败: %v", err)
	}
	defer procCloseHandle.Call(t.h)

	fmt.Println("========== QQ 音乐 22.60 偏移探测报告 ==========")
	fmt.Printf("目标 pid       : %d\n", t.pid)
	fmt.Printf("QQMusic.dll    : base=0x%X size=0x%X\n", t.dllBase, t.dllSize)
	fmt.Printf("exe 版本       : %s", t.version)
	if t.version != "22.60" && t.version != "unknown" {
		fmt.Printf("  (注意：期望 22.60，实为 %s)", t.version)
	}
	fmt.Println()
	fmt.Printf("锚点           : name=%q singer=%q songid=%d\n", *name, *singer, *songid)

	// ---- 全内存扫描锚点 ----
	nameU16 := utf16le(*name)
	singerU16 := utf16le(*singer)
	nameGBK := gbkBytes(*name)

	var nameAddrs, singerAddrs []uintptr
	nameSeen, singerSeen := map[uintptr]bool{}, map[uintptr]bool{}
	gbkHits := 0
	t.walkRegions(func(base uintptr, data []byte) {
		collectWide(data, base, nameU16, nameSeen, &nameAddrs)
		collectWide(data, base, singerU16, singerSeen, &singerAddrs)
		gbkHits += countBytes(data, nameGBK)
	})
	singerSet := map[uintptr]bool{}
	for _, a := range singerAddrs {
		singerSet[a] = true
	}

	fmt.Println()
	fmt.Println("---- 阶段 A：锚点扫描 ----")
	fmt.Printf("歌名 UTF-16 命中 : %d 处 %s\n", len(nameAddrs), sampleAddrs(nameAddrs))
	fmt.Printf("歌手 UTF-16 命中 : %d 处 %s\n", len(singerAddrs), sampleAddrs(singerAddrs))
	fmt.Printf("歌名 GBK 窄串命中: %d 处\n", gbkHits)
	if len(nameAddrs) > 0 && gbkHits == 0 {
		fmt.Println("判定             : UseWideStrings=true（宽字符模型：UTF-16 扫到、GBK 扫不到）")
	} else if gbkHits > 0 && len(nameAddrs) == 0 {
		fmt.Println("判定             : ⚠ 疑似窄字符模型（GBK 扫到、UTF-16 扫不到）——与 22.60 预期不符，需主会话核查")
	} else if len(nameAddrs) == 0 {
		fmt.Println("判定             : ⚠ 歌名两种编码都未命中——确认 --name 与当前播放一致，或换歌重试")
	} else {
		fmt.Println("判定             : UTF-16 命中（GBK 亦有命中，可能为其他缓存），按宽字符模型继续")
	}

	if len(nameAddrs) == 0 && len(singerAddrs) == 0 {
		log.Fatalf("歌名与歌手均未在内存中找到，无法定位 struct1（请确认正在播放且锚点正确）")
	}

	// ---- 阶段 A：定位 struct1 ----
	dllData := t.readImage()
	best, all, ok := phaseA(t, dllData, nameAddrs, singerAddrs, singerSet, *name, *singer)
	if !ok {
		log.Fatalf("在 DLL 静态区未找到任何指向锚点串的槽位，无法定位 struct1")
	}
	matched := best.modelMatched(*name, *singer)

	fmt.Println()
	fmt.Println("---- 阶段 A：struct1 定位 ----")
	fmt.Printf("候选数量         : %d（取分最高者）\n", len(all))
	fmt.Printf("struct1 绝对地址 : 0x%X\n", best.base)
	fmt.Printf("Struct1 (DLL 偏移): 0x%X\n", best.base-t.dllBase)
	fmt.Printf("读回歌名         : %q\n", best.name)
	fmt.Printf("读回歌手         : %q  (指针精确命中歌手串=%v)\n", best.singer, best.singerPtrHit)
	fmt.Printf("读回专辑         : %q\n", best.album)
	fmt.Printf("读回进度/时长    : %d ms / %d ms\n", best.prog, best.dur)
	fmt.Printf("22.41 模型吻合   : %v (score=%d)\n", matched, best.score)

	var dm discovered
	if !matched {
		fmt.Println("⚠ 模型不吻合，改为在歌名槽位附近实测各字段偏移：")
		// 取一个歌名槽位作实测基准（best.base 在歌名锚下即歌名槽位；歌手锚下需回推）。
		nameSlot := best.base + offName
		dm = discoverOffsets(t, nameSlot, singerSet, best.dur)
		fmt.Printf("  实测 SingerOff=0x%X(%v) AlbumOff=0x%X(%v) ProgressOff=0x%X(%v) DurationOff=0x%X(%v)\n",
			dm.singerOff, dm.haveSinger, dm.albumOff, dm.haveAlbum,
			dm.progOff, dm.haveProg, dm.durOff, dm.haveDur)
	}

	// ---- 阶段 B：定位 songID 播放会话结构 ----
	fmt.Println()
	fmt.Println("---- 阶段 B：songID 播放会话结构定位 ----")
	if best.dur == 0 {
		fmt.Println("⚠ 阶段 A 时长为 0，无法用时长锚定位播放会话——请在有歌播放时重跑")
	}
	sess := phaseB(t, dllData, best.base, best.dur, uint32(*songid))
	if len(sess) == 0 {
		fmt.Println("未定位：DLL 静态区无满足「时长锚 + songID 合理 + 浮点特征」的干净候选")
		fmt.Println("        （主会话可用真实 songID 以 --songid 重跑精确匹配）")
	} else {
		fmt.Printf("候选 %d 个（按分排序，取分最高者）：\n", len(sess))
		for i, c := range sess {
			if i >= 5 {
				fmt.Printf("  ... 另有 %d 个候选略\n", len(sess)-5)
				break
			}
			fmt.Printf("  #%d songID=%-11d SongIDOff=0x%X SongIDDurCheckOff=0x%X 浮点分=%d 精确=%v\n",
				i+1, c.songID,
				int64(c.songIDAddr)-int64(best.base),
				int64(c.durCheckAddr)-int64(best.base),
				c.floatScore, c.exact)
		}
	}

	// ---- 输出 Go 字面量 ----
	emitLiteral(t, best, dm, matched, sess)

	fmt.Println()
	fmt.Println("---- 说明与拿不准处 ----")
	fmt.Println("1. 本工具只读，未对目标写入任何字节。")
	fmt.Println("2. Struct1/SongIDOff 为运行时实测；换歌瞬间两结构可能短暂不同步，如读到异常请稳态后重跑。")
	fmt.Println("3. 阶段 B 若多候选或无候选，请用 QQ 分享链接拿真实 songID，以 --songid 精确重跑。")
}

// sampleAddrs 返回地址切片前若干个的十六进制样本，供报告展示。
func sampleAddrs(addrs []uintptr) string {
	if len(addrs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[")
	for i, a := range addrs {
		if i >= 4 {
			sb.WriteString(fmt.Sprintf("... +%d", len(addrs)-4))
			break
		}
		if i > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString(fmt.Sprintf("0x%X", a))
	}
	sb.WriteString("]")
	return sb.String()
}
