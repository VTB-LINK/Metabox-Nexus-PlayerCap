//go:build windows

// Command qqstateprobe 是只读探测工具（issue #172），用于适配 QQ 音乐新版本的播放状态字段（player/qqmusic/mem.go
// knownVersions 的 PlayState）：录下 QQMusic.exe、QQMusic.dll、QQMusicPlayer.dll、QQMusicCommon.dll 的可写节
// （.data 等）随时间的变化，连同当时的歌名与进度（按 versions.go 的版本表读），离线逐拍对照，人工找出播放状态字段。
// 设计见 doc/design/qqmusic-start-stall-design.md §6.8。
//
// 只读红线：OpenProcess 只申请 PROCESS_QUERY_INFORMATION | PROCESS_VM_READ；本工具不声明 WriteProcessMemory /
// VirtualAllocEx / VirtualProtectEx / CreateRemoteThread，不挂起线程、不注入；record 每拍读取总量不超过 16 MiB
// （maxSampleBytes），sample 的上限见文末。Win32 薄封装照抄 tools/qqprobe。versions.go 的偏移照抄 knownVersions，
// 二者由 player/qqmusic 的测试比对，漏同步即红。
//
// 用法：
//
//	qqstateprobe record -seconds 300 -out qq.bin
//	qqstateprobe dump -in qq.bin [-max 60] [-watch QQMusic.dll:0xRVA ...]
//	qqstateprobe sample -seconds N -field QQMusic.dll:0xRVA -out qq.tsv [-interval 5ms] [-stop 文件]
//
// 流程（每个新版本）：
//  1. 先按现有流程定出元数据偏移（qqprobe 等），同步进 mem.go 与 versions.go。
//  2. record 录 300 s，请人在 QQ 里依次操作并记下大致时刻：播放 10 s；暂停 5 s 后恢复，两次；「下一首」两次，
//     间隔 10 s，换歌后 5 s 内不动；「下一首」后 1 s 内暂停、5 s 后恢复，两次；播放中拖动两次（前跳、回拖各一），
//     间隔 10 s；暂停中拖动一次，等 5 s 再恢复；单曲循环模式下拖到结尾前约 5 s，等它回到开头一次。
//  3. dump -max 60 列出变化不超过 60 次的 dword，找取值只有少数几个、在每次暂停与恢复时翻转的。22.31 起的版本可先
//     在 Struct1 + 0x70000 附近找（22.82 为 +0x70740，22.52 为 +0x705F0，不固定）；20.05 的字段是另一种（0、1）。
//     22.16 起各版（21.81 同此，22.52 未核对）播放会话结构的 SongIDOff + 0x10 是 A 类字段（0 暂停、1 在播、2、3 载入），
//     可先看这里，仍须按核实清单录像确认。
//  4. dump -watch QQMusic.dll:<候选> 逐拍对照（取值或进度变化的拍各占一行），按下面的核实清单第 1～5 项逐项核对。
//  5. 重启 QQ，再录 60 s（暂停与恢复两次、「下一首」一次），同一 RVA 按第 6 项核对。
//  6. 填 knownVersions[版本].PlayState，注释写工具、日期、构建目录、取值、MinPause 的依据与核实清单各项的录像时刻，
//     同时改 player/qqmusic/playstate_test.go 的 TestKnownVersionsPlayState。探不到或核对不过的版本留空。
//
// 核实清单（全部见到才填；运行时的交叉核对发现不了「卡在在播值而每次暂停都短」，这份清单是唯一的保证）：
//  1. 暂停与恢复：每次为「在播值→暂停值→在播值」；暂停值在读数最后一次前进之后一两拍内出现，暂停中的补写期间保持
//     暂停值；在播值在读数起步之前出现。
//  2. 换歌：不出现暂停值，或只闪一下；闪的取它在录像里的最长长度（按拍取样只能得到上界：前后两个非暂停拍之差）定
//     MinPause。
//  3. 起步停顿：换歌、拖动、回到开头、恢复之后读数停走期间为在播值。
//  4. 换歌后立刻暂停：为暂停值，含暂停中的补写。
//  5. 全程没有取值集合以外的值。
//  6. 重启 QQ 之后，同一 RVA 重复 1～4。
//
// 取值不是「一个暂停集合、一个在播集合」能描述的（如要看两个字段才能定），回到设计。
//
// sample 是单字段密采样，用于实测某个取值最短持续多久（#218：在 22.82 上量载入值的最短持续时间，核定载入值的采样
// 门槛）。record 固定 200 ms 一拍、每拍读全部可写节，做不到 10 ms 以内的间隔。每个样本只读 -field 的一个 DWORD
// 与歌名（readName），不读进度与可写节。参数：-seconds 1～1800，到时即停；-field 写法同 -watch，模块须已加载
// （wantModules 之一），RVA 须 4 字节对齐、整个 DWORD 落在模块映像之内；-interval 1 ms～10 ms，默认 5 ms；-stop
// 指定的文件出现即停（每 100 ms 看一次，开始时已存在即报错），Ctrl+C 同样停；写输出失败（文件头、样本行、汇总行
// 任一）即停止采样、不再读目标进程，以错误退出，不打印汇总。第 i 个样本的目标时刻为开始后 i 个间隔，Sleep 到目标前
// 2 ms，其余自旋；落后一个间隔以上不补采，缺口照实留在样本时刻里。时刻取
// QueryPerformanceCounter（qpcClock）。读取量：每个样本至多 4 字节加 metaBytes，样本数至多 seconds × 1000 /
// interval（ms），结束时打印实际读取量。版本号不在 versions.go 里时照常采样（字段按 RVA 定址），第二行注明歌名可能
// 读错。输出为 UTF-8 文本，每个样本一行，制表符分隔，# 开头为注释：
//
//	# qqstateprobe sample
//	# pid 12345, QQ 音乐 22.82, 偏移表 22.82（精确匹配）
//	# field QQMusic.dll:0xD3BBA8 = 0x61B5BBA8, interval 5ms, start 2026-11-25T10:00:00.000+08:00
//	t_us	state	name
//	0	3	"Lettersong (致十年后的我)"
//	5012	3	=
//	10031	2	"QQ音乐 听我想听"
//	15020	-	=
//	20044	2	-
//	25008	3	"Lettersong (致十年后的我)"
//	# samples 6, state_failed 1, name_failed 1, max_gap_us 5024, gaps_over_10ms 0, bytes_read 2604
//
// t_us 为样本时刻（读字段之前取），距开始的微秒；state 为字段的十进制取值，读失败为 -（取值 0 照常写 0）；name 与
// 上一个样本读成的歌名相同为 =，不同（含第一个样本、读失败之后的第一个读成）为 Go 引号串（空歌名为 ""），读失败
// 为 -。末行汇总样本数、两类读失败数、相邻样本的最大间隔（微秒）、间隔严格超过 10 ms 的次数与实际读取字节数，同时
// 打到标准输出。
package main

import (
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/gob"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	modkernel32              = syscall.NewLazyDLL("kernel32.dll")
	procCreateToolhelp32     = modkernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32First       = modkernel32.NewProc("Process32FirstW")
	procProcess32Next        = modkernel32.NewProc("Process32NextW")
	procOpenProcess          = modkernel32.NewProc("OpenProcess")
	procCloseHandle          = modkernel32.NewProc("CloseHandle")
	procReadProcessMemory    = modkernel32.NewProc("ReadProcessMemory")
	procQueryFullProcessName = modkernel32.NewProc("QueryFullProcessImageNameW")
	procQueryPerfCounter     = modkernel32.NewProc("QueryPerformanceCounter")
	procQueryPerfFrequency   = modkernel32.NewProc("QueryPerformanceFrequency")

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
	PROCESS_QUERY_INFORMATION = 0x0400
	PROCESS_VM_READ           = 0x0010
	TH32CS_SNAPPROCESS        = 0x00000002
	LIST_MODULES_ALL          = 0x03
	IMAGE_SCN_MEM_WRITE       = 0x80000000

	// readNowPlaying 每拍读取上界：结构体 2 歌名 4+4+512、SSO 24+<1000、宽字符歌名 4+512、进度 ≤8。
	// 也是 readName 的读取上界（sample 用）。
	metaBytes = 2 << 10

	tickInterval   = 200 * time.Millisecond
	maxSampleBytes = 16 << 20
	readChunk      = 64 << 10
)

// 选取的模块（按此顺序编号节）。
var wantModules = []string{"QQMusic.exe", "QQMusic.dll", "QQMusicPlayer.dll", "QQMusicCommon.dll"}

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

type module struct {
	name string
	base uint64
	size uint32
}

type target struct {
	h       uintptr
	pid     uint32
	version string
	mods    []module
	dll     uintptr    // QQMusic.dll 基址
	off     verOffsets // 按 version 选出的偏移
	offKey  string     // 所选偏移表的版本号
	exact   bool       // offKey == version
}

// bytesRead 累计 ReadProcessMemory 实际拷贝的字节数（sample 的汇总用）。本工具只在一个 goroutine 里读目标进程。
var bytesRead uint64

// readFull 分块只读拷贝 [addr, addr+len(buf))，任一块读不全即返回 false。
func readFull(h, addr uintptr, buf []byte) bool {
	for off := 0; off < len(buf); off += readChunk {
		end := min(off+readChunk, len(buf))
		var n uintptr
		r, _, _ := procReadProcessMemory.Call(h, addr+uintptr(off),
			uintptr(unsafe.Pointer(&buf[off])), uintptr(end-off), uintptr(unsafe.Pointer(&n)))
		bytesRead += uint64(n)
		if r == 0 || int(n) != end-off {
			return false
		}
	}
	return true
}

// readWide 读 NUL 结尾的 UTF-16LE 字符串，最多 256 码元；容忍读到可读区末尾的部分拷贝。
// 地址为 0 或过低时为空串、ok 为真；一个码元也没读到时 ok 为假。
func (t *target) readWide(addr uintptr) (string, bool) {
	if addr <= 0x10000 {
		return "", true
	}
	b := make([]byte, 512)
	var n uintptr
	procReadProcessMemory.Call(t.h, addr, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&n)))
	bytesRead += uint64(n)
	if n < 2 {
		return "", false
	}
	u := make([]uint16, 0, n/2)
	for i := 0; i+1 < int(n); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u)), true
}

// readU32 读一个 DWORD，读不全返回 0（同 mem.go ReadUint32）。
func (t *target) readU32(addr uintptr) uint32 {
	v, _ := t.readU32OK(addr)
	return v
}

// readU32OK 读一个 DWORD；ReadProcessMemory 失败或读到的不足 4 字节时 ok 为假（同 mem.go readPlayState），
// 取值为 0，调用方不得当作读到了 0。
func (t *target) readU32OK(addr uintptr) (uint32, bool) {
	var b [4]byte
	if !readFull(t.h, addr, b[:]) {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b[:]), true
}

// readName 按所选版本的偏移读歌名，取法同 mem.go ReadAllMetadata：优先结构体 2，不足 2 字节再取 Struct1（宽字符
// 或 SSO）。ok 为假 = 改取 Struct1 时这一路有一步读不全（结构体 2 读不成即改取 Struct1，不单独算失败）；字符串指针
// 为 0 或过低时歌名为空，不算失败。读取量不超过 metaBytes。
func (t *target) readName() (string, bool) {
	o := t.off
	var name string
	if o.Struct2Ptr != 0 {
		if s2 := uintptr(t.readU32(t.dll + o.Struct2Ptr)); s2 != 0 {
			name, _ = t.readWide(uintptr(t.readU32(s2 + 0x64)))
		}
	}
	ok := true
	if len(name) < 2 {
		s1 := t.dll + o.Struct1
		if o.Wide {
			p, pok := t.readU32OK(s1 + o.NameOff)
			name, ok = t.readWide(uintptr(p))
			ok = ok && pok
		} else if b := make([]byte, 24); readFull(t.h, s1+o.NameOff, b) {
			name = ssoFromBuf(b, func(ptr, n uint32) []byte {
				heap := make([]byte, n) // n < 1000，由 ssoFromBuf 保证
				ok = readFull(t.h, uintptr(ptr), heap)
				return heap
			})
		} else {
			ok = false
		}
	}
	return sanitizeString(name), ok
}

// readNowPlaying 按所选版本的偏移读歌名（readName）与进度，取法同 mem.go ReadAllMetadata：
// 进度依次取快速计时器、DLL 直接偏移、结构体内字段。不含 mem.go 中需注入的 AOB 进度。
func (t *target) readNowPlaying() (string, uint32) {
	o := t.off
	s1 := t.dll + o.Struct1
	name, _ := t.readName()
	var prog uint32
	switch {
	case o.FastTimerPtr != 0:
		if p := uintptr(t.readU32(t.dll + o.FastTimerPtr)); p > 0x10000 {
			prog = t.readU32(p + o.FastTimerOff)
		} else {
			prog = t.readU32(s1 + o.ProgressOff)
		}
	case o.ProgressDllOff != 0:
		prog = t.readU32(t.dll + o.ProgressDllOff)
	default:
		prog = t.readU32(s1 + o.ProgressOff)
	}
	return name, prog
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

// enumModules 返回 wantModules 中在目标进程里存在的模块（按 wantModules 顺序）。
func enumModules(h uintptr) []module {
	var mods [1024]uintptr
	var cbNeeded uint32
	r, _, _ := procEnumProcessModulesEx.Call(h, uintptr(unsafe.Pointer(&mods[0])), unsafe.Sizeof(mods),
		uintptr(unsafe.Pointer(&cbNeeded)), uintptr(LIST_MODULES_ALL))
	if r == 0 {
		return nil
	}
	// cbNeeded 是所需字节数，模块数超过缓冲区时须截断（同 qqprobe）。
	cnt := min(cbNeeded/uint32(unsafe.Sizeof(mods[0])), uint32(len(mods)))
	found := map[string]module{}
	for i := uint32(0); i < cnt; i++ {
		var name [256]uint16
		procGetModuleBaseNameW.Call(h, mods[i], uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
		n := syscall.UTF16ToString(name[:])
		var mi MODULEINFO
		procGetModuleInformation.Call(h, mods[i], uintptr(unsafe.Pointer(&mi)), unsafe.Sizeof(mi))
		found[strings.ToLower(n)] = module{name: n, base: uint64(mods[i]), size: mi.SizeOfImage}
	}
	var out []module
	for _, w := range wantModules {
		if m, ok := found[strings.ToLower(w)]; ok {
			out = append(out, m)
		}
	}
	return out
}

func openTarget() (*target, error) {
	pids, err := enumQQMusicPIDs()
	if err != nil {
		return nil, err
	}
	if len(pids) == 0 {
		return nil, fmt.Errorf("未找到 QQMusic.exe 进程")
	}
	for _, pid := range pids {
		h, _, _ := procOpenProcess.Call(uintptr(PROCESS_QUERY_INFORMATION|PROCESS_VM_READ), 0, uintptr(pid))
		if h == 0 {
			continue
		}
		mods := enumModules(h)
		for _, m := range mods {
			if strings.EqualFold(m.name, "QQMusic.dll") {
				ver := readExeVersion(h)
				key, exact := pickOffsets(ver)
				return &target{h: h, pid: pid, version: ver, mods: mods, dll: uintptr(m.base),
					off: knownVersions[key], offKey: key, exact: exact}, nil
			}
		}
		procCloseHandle.Call(h)
	}
	return nil, fmt.Errorf("候选进程中都没有 QQMusic.dll（pid=%v）", pids)
}

// readExeVersion 读 QQMusic.exe 的 ProductVersion（major.minor），读不到为 "unknown"。照抄 qqprobe。
func readExeVersion(h uintptr) string {
	buf := make([]uint16, 512)
	size := uint32(len(buf))
	r, _, _ := procQueryFullProcessName.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r == 0 || size == 0 {
		return "unknown"
	}
	p, err := syscall.UTF16PtrFromString(syscall.UTF16ToString(buf[:size]))
	if err != nil {
		return "unknown"
	}
	infoSize, _, _ := procGetFileVersionInfoSizeW.Call(uintptr(unsafe.Pointer(p)), 0)
	if infoSize == 0 {
		return "unknown"
	}
	data := make([]byte, infoSize)
	if r, _, _ = procGetFileVersionInfoW.Call(uintptr(unsafe.Pointer(p)), 0, infoSize, uintptr(unsafe.Pointer(&data[0]))); r == 0 {
		return "unknown"
	}
	sub, _ := syscall.UTF16PtrFromString(`\`)
	var infoPtr uintptr
	var infoLen uint32
	r, _, _ = procVerQueryValueW.Call(uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(sub)),
		uintptr(unsafe.Pointer(&infoPtr)), uintptr(unsafe.Pointer(&infoLen)))
	base := uintptr(unsafe.Pointer(&data[0]))
	if r == 0 || infoPtr < base || infoPtr-base+unsafe.Sizeof(vsFixedFileInfo{}) > uintptr(len(data)) {
		return "unknown"
	}
	info := (*vsFixedFileInfo)(unsafe.Pointer(&data[infoPtr-base]))
	if info.Signature != 0xFEEF04BD {
		return "unknown"
	}
	return fmt.Sprintf("%d.%02d", uint16(info.ProductVersionMS>>16), uint16(info.ProductVersionMS&0xFFFF))
}

// writableSections 从目标内存读 PE 头，返回带 IMAGE_SCN_MEM_WRITE 的节。
func (t *target) writableSections(m module) []Section {
	hdr := make([]byte, 0x1000)
	if !readFull(t.h, uintptr(m.base), hdr) || hdr[0] != 'M' || hdr[1] != 'Z' {
		log.Printf("%s：读不到 PE 头，跳过", m.name)
		return nil
	}
	le32 := func(o uint32) uint32 { return binary.LittleEndian.Uint32(hdr[o:]) }
	le16 := func(o uint32) uint32 { return uint32(binary.LittleEndian.Uint16(hdr[o:])) }
	lfanew := le32(0x3C)
	if lfanew+24 > uint32(len(hdr)) || string(hdr[lfanew:lfanew+4]) != "PE\x00\x00" {
		log.Printf("%s：PE 签名不对，跳过", m.name)
		return nil
	}
	fh := lfanew + 4
	tab := fh + 20 + le16(fh+16)
	var out []Section
	for i := uint32(0); i < le16(fh+2); i++ {
		e := tab + 40*i
		if e+40 > uint32(len(hdr)) {
			break
		}
		if le32(e+36)&IMAGE_SCN_MEM_WRITE == 0 {
			continue
		}
		size := le32(e + 8)
		if size == 0 {
			size = le32(e + 16)
		}
		size = (size + 3) &^ 3 // 节按页对齐映射，补到 4 字节不越页
		if size == 0 {
			continue
		}
		out = append(out, Section{Module: m.name, ModBase: m.base, Name: strings.TrimRight(string(hdr[e:e+8]), "\x00"),
			RVA: le32(e + 12), Size: size})
	}
	return out
}

// fitBudget 让每拍读取总量 ≤ maxSampleBytes，超出时从最大的节开始丢。
func fitBudget(secs []Section) ([]Section, int) {
	total := metaBytes
	for _, s := range secs {
		total += int(s.Size)
	}
	for total > maxSampleBytes {
		big := 0
		for i, s := range secs {
			if s.Size > secs[big].Size {
				big = i
			}
		}
		log.Printf("每拍 %d 字节超过上限 %d，丢弃最大的节 %s %s（%d 字节）", total, maxSampleBytes,
			secs[big].Module, secs[big].Name, secs[big].Size)
		total -= int(secs[big].Size)
		secs = append(secs[:big], secs[big+1:]...)
	}
	return secs, total
}

// diffDwords 把 a→b 变化的 4 字节对齐 dword 追加进 rec（超过 maxChangesPerTick 后只计数），返回变化个数。
func diffDwords(a, b []byte, sec uint16, rec *Tick) int {
	n := 0
	for off := 0; off+4 <= len(b); off += 4 {
		v := binary.LittleEndian.Uint32(b[off:])
		if binary.LittleEndian.Uint32(a[off:]) == v {
			continue
		}
		n++
		if len(rec.Off) <= maxChangesPerTick {
			rec.Sec = append(rec.Sec, sec)
			rec.Off = append(rec.Off, uint32(off))
			rec.Val = append(rec.Val, v)
		}
	}
	return n
}

func record(seconds int, out string) error {
	if seconds <= 0 || out == "" {
		return fmt.Errorf("需要 -seconds N（N>0）与 -out 文件")
	}
	t, err := openTarget()
	if err != nil {
		return err
	}
	defer procCloseHandle.Call(t.h)
	match := "精确匹配"
	if !t.exact {
		match = "表中无此版本，按版本号距离回退，歌名/进度可能读错"
	}
	fmt.Printf("pid %d，QQ 音乐 %s，偏移表 %s（%s），struct1 0x%X\n", t.pid, t.version, t.offKey, match, t.dll+t.off.Struct1)

	var secs []Section
	for _, w := range wantModules {
		var m *module
		for i := range t.mods {
			if strings.EqualFold(t.mods[i].name, w) {
				m = &t.mods[i]
			}
		}
		if m == nil {
			fmt.Printf("模块 %s：未加载，跳过\n", w)
			continue
		}
		fmt.Printf("模块 %s：base 0x%X size 0x%X\n", m.name, m.base, m.size)
		for _, s := range t.writableSections(*m) {
			fmt.Printf("  %-8s RVA 0x%08X size 0x%X (%d)\n", s.Name, s.RVA, s.Size, s.Size)
			secs = append(secs, s)
		}
	}
	secs, total := fitBudget(secs)
	if len(secs) == 0 {
		return fmt.Errorf("没有可录的可写节")
	}
	fmt.Printf("共 %d 个节，每拍读取 %d 字节（上限 %d）\n", len(secs), total, maxSampleBytes)
	name, prog := t.readNowPlaying()
	fmt.Printf("当前：%q 进度 %d ms\n", name, prog)

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	enc := gob.NewEncoder(gz)
	start := time.Now()
	if err := enc.Encode(&Header{PID: t.pid, Version: t.version, Offsets: t.offKey, Struct1: uint64(t.dll + t.off.Struct1),
		IntervalMs: int(tickInterval / time.Millisecond), Start: start, Sections: secs}); err != nil {
		return err
	}

	n := len(secs)
	prev, cur := make([][]byte, n), make([][]byte, n)
	resync := make([]bool, n) // true=下一次读成功时整节写出（首拍、溢出拍之后）
	for i, s := range secs {
		prev[i], cur[i] = make([]byte, s.Size), make([]byte, s.Size)
		resync[i] = true
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	tk := time.NewTicker(tickInterval)
	defer tk.Stop()
	deadline := start.Add(time.Duration(seconds) * time.Second)
	ticks := 0
loop:
	for {
		rec := Tick{T: time.Since(start).Milliseconds()}
		rec.Name, rec.Prog = t.readNowPlaying()
		changed := 0
		for i, s := range secs {
			if !readFull(t.h, uintptr(s.ModBase)+uintptr(s.RVA), cur[i]) {
				rec.Failed = append(rec.Failed, uint16(i)) // 本拍跳过，prev 保持上次成功的内容
				continue
			}
			if resync[i] {
				if rec.Full == nil {
					rec.Full = make([][]byte, n)
				}
				rec.Full[i] = cur[i] // Encode 同步完成后才会再写这块缓冲
				resync[i] = false
			} else {
				changed += diffDwords(prev[i], cur[i], uint16(i), &rec)
			}
			prev[i], cur[i] = cur[i], prev[i]
		}
		if changed > maxChangesPerTick {
			rec.Sec, rec.Off, rec.Val, rec.Full = nil, nil, nil, nil
			rec.Overflow = changed
			for i := range resync {
				resync[i] = true
			}
		}
		if err := enc.Encode(&rec); err != nil {
			return err
		}
		if ticks%25 == 0 {
			fmt.Printf("t=%5.1fs %q 进度 %d ms，本拍变动 %d 个 dword\n", float64(rec.T)/1000, rec.Name, rec.Prog, changed)
		}
		ticks++
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			break loop
		case <-tk.C:
		}
	}
	if err := gz.Close(); err != nil {
		return err
	}
	st, _ := f.Stat()
	fmt.Printf("已写 %d 拍到 %s（%d 字节）\n", ticks, out, st.Size())
	return nil
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("[qqstateprobe] ")
	if len(os.Args) < 2 {
		log.Fatal("用法：qqstateprobe record -seconds N -out 文件 | dump -in 文件 [-max N] [-watch 模块:0xRVA ...] | " +
			"sample -seconds N -field 模块:0xRVA -out 文件 [-interval 5ms] [-stop 文件]")
	}
	var err error
	switch os.Args[1] {
	case "record":
		fs := flag.NewFlagSet("record", flag.ExitOnError)
		seconds := fs.Int("seconds", 0, "录制秒数")
		out := fs.String("out", "", "输出文件")
		fs.Parse(os.Args[2:])
		err = record(*seconds, *out)
	case "dump":
		fs := flag.NewFlagSet("dump", flag.ExitOnError)
		in := fs.String("in", "", "record 产出的文件")
		maxChanges := fs.Int("max", 80, "只列变化次数不超过它的 dword")
		var watches watchList
		fs.Var(&watches, "watch", "逐拍列出的地址，模块:0xRVA（如 QQMusic.dll:0xD3BBA8），可重复；RVA 非 4 对齐时按字节列")
		fs.Parse(os.Args[2:])
		err = dump(*in, *maxChanges, watches)
	case "sample":
		var o sampleOpts
		if o, err = parseSampleArgs(os.Args[2:]); err == nil {
			err = sample(o)
		}
	default:
		err = fmt.Errorf("未知子命令 %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}
