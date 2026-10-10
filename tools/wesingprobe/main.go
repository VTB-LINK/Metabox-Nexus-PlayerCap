//go:build windows

// Command wesingprobe 是全民K歌歌词的内存探测工具（只读、仅本地调试）。
//
// 探测结论（2026-10-09 订正）：**音译在内存里。** LyricHost 里并排放着几条歌词轨（间距 0x70、虚表
// 相同）：FindLyricHost 返回的子结构是主歌词，路径以 _Roma.qrc 结尾的那条是音译，两者逐行对应
// （行数相同、行起始时间相等）。生产代码的读取见 player/wesing/lyric/roma.go。本工具把各条轨列出来
// （路径、行数、前几行），并对指定行的 LyricEntry 及其前若干 CharElement 逐 4 字节 dump（float 解读 +
// 跟随指针读 UTF-16 字符串）。
//
// 本文件 2026-09 的旧结论「罗马音任何形态在可写区与全可读区均 0 命中，音译系运行时动态转写、不落
// 字符串」（issue #34）不成立。当时是到 LyricEntry+0x30 去找「第二条字符向量」——LyricEntry 只有
// 0x20 字节，+0x30 已经是相邻的下一个 LyricEntry——没有往 LyricHost 的其它子结构看。那次全内存搜索
// 为什么没搜到，没有留下可复查的记录。
//
// 只读约束（AGENTS.md §0）：仅经 wesing/proc 的只读原语读取——OpenProc 只申请
// PROCESS_VM_READ|PROCESS_QUERY_INFORMATION，句柄层面无写权限；绝不写目标进程。
// 自包含、仅 Windows 构建，不改动任何生产代码，不进出货二进制。
//
// 用法（需管理员权限读进程内存；K歌开着「音译歌词」开关、播放或暂停在某行）：
//
//	go run ./tools/wesingprobe --line "手を伸"   // 只 dump 主文本含该子串的行
//	go run ./tools/wesingprobe -n 3              // 各条轨列前 3 行，并 dump 主歌词前 3 行的结构
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"math"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"Metabox-Nexus-PlayerCap/player/wesing/lyric"
	"Metabox-Nexus-PlayerCap/player/wesing/proc"
)

// looksPtr 粗判一个 32 位值是否像用户态堆/映像指针。
func looksPtr(v uint32) bool { return v > 0x00100000 && v < 0x7FFF0000 }

// looksText 判定解码后的字符串是否像正常文本（非空、无解码失败标记、无控制字符）。
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

// readWide 读以 NUL 结尾的 UTF-16LE 字符串，最多 maxChars 码元。
func readWide(h syscall.Handle, addr uint32, maxChars int) string {
	if addr < 0x00100000 {
		return ""
	}
	b, err := proc.ReadBytes(h, addr, uint32(maxChars*2))
	if err != nil || len(b) < 2 {
		return ""
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := uint16(b[i]) | uint16(b[i+1])<<8
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// utf16le 把字符串编码为 UTF-16LE 的 AOBScan pattern（全 mask=true）。
func utf16le(s string) ([]byte, []bool) {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	m := make([]bool, len(u)*2)
	for i, c := range u {
		b[i*2] = byte(c)
		b[i*2+1] = byte(c >> 8)
		m[i*2] = true
		m[i*2+1] = true
	}
	return b, m
}

// ── 自包含全可读区扫描（proc.EnumWritableRegions 只覆盖可写区；音译若在只读区靠它兜底）。
// 只读原语：VirtualQueryEx + ReadProcessMemory。 ──
var (
	scKernel32          = syscall.NewLazyDLL("kernel32.dll")
	scVirtualQueryEx    = scKernel32.NewProc("VirtualQueryEx")
	scReadProcessMemory = scKernel32.NewProc("ReadProcessMemory")
)

type scMBI struct {
	BaseAddress       uintptr
	AllocationBase    uintptr
	AllocationProtect uint32
	RegionSize        uintptr
	State             uint32
	Protect           uint32
	Type              uint32
}

func scReadable(protect uint32) bool {
	if protect&0x100 != 0 || protect&0x01 != 0 { // PAGE_GUARD / PAGE_NOACCESS
		return false
	}
	switch protect &^ (0x100 | 0x200 | 0x400) {
	case 0x02, 0x04, 0x08, 0x20, 0x40, 0x80: // READONLY/READWRITE/WRITECOPY/EXECUTE_*
		return true
	}
	return false
}

// searchAllReadable 遍历目标全部 commit 可读区，搜偶对齐的 UTF-16 needle，返回命中地址。
func searchAllReadable(h syscall.Handle, needle []byte) []uint32 {
	var hits []uint32
	var addr uintptr
	for {
		var mbi scMBI
		r, _, _ := scVirtualQueryEx.Call(uintptr(h), addr, uintptr(unsafe.Pointer(&mbi)), unsafe.Sizeof(mbi))
		if r == 0 {
			break
		}
		next := uint64(mbi.BaseAddress) + uint64(mbi.RegionSize)
		if mbi.State == 0x1000 && scReadable(mbi.Protect) && mbi.RegionSize > 0 && uint64(mbi.RegionSize) < 256*1024*1024 {
			buf := make([]byte, mbi.RegionSize)
			var n uintptr
			scReadProcessMemory.Call(uintptr(h), mbi.BaseAddress,
				uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&n)))
			data := buf[:n]
			for i := 0; ; {
				j := bytes.Index(data[i:], needle)
				if j < 0 {
					break
				}
				pos := i + j
				if pos%2 == 0 {
					hits = append(hits, uint32(mbi.BaseAddress)+uint32(pos))
				}
				i = pos + 2
			}
		}
		if next <= uint64(addr) || next > 0xFFFF0000 {
			break
		}
		addr = uintptr(next)
	}
	return hits
}

// dumpStruct 逐 4 字节 dump [base, base+maxOff]：float 解读 + 跟随指针读 UTF-16。
// 音译若是「某偏移 → RenderData(起始即 UTF-16 文本)」，会直接以 → %q 显示出音译文本。
func dumpStruct(h syscall.Handle, base, maxOff uint32, label string) {
	fmt.Printf("  %s @0x%08X:\n", label, base)
	for off := uint32(0); off <= maxOff; off += 4 {
		v, err := proc.ReadUint32(h, base+off)
		if err != nil {
			fmt.Printf("    +0x%02X = <读失败>\n", off)
			continue
		}
		line := fmt.Sprintf("    +0x%02X = 0x%08X", off, v)
		if f := math.Float32frombits(v); f > 0.0009 && f < 100000 {
			line += fmt.Sprintf("  f32=%.3f", f)
		}
		if looksPtr(v) {
			if s := readWide(h, v, 40); looksText(s) {
				line += fmt.Sprintf("  → %q", s)
			}
		}
		fmt.Println(line)
	}
}

// entryMainText 拼出一行主文本（CharElement+0x00 → RenderData → UTF-16），
// 并回传字符向量 begin 与字符数，供后续逐 CharElement dump。
func entryMainText(h syscall.Handle, entryPtr uint32) (text string, charBegin, numChars uint32) {
	charBegin, _ = proc.ReadUint32(h, entryPtr+0x08)
	charEnd, _ := proc.ReadUint32(h, entryPtr+0x0C)
	if charBegin == 0 || charEnd <= charBegin {
		return "", charBegin, 0
	}
	numChars = (charEnd - charBegin) / 4
	if numChars > 500 {
		return "", charBegin, 0
	}
	var sb strings.Builder
	for c := uint32(0); c < numChars; c++ {
		ce, _ := proc.ReadUint32(h, charBegin+c*4)
		if ce == 0 {
			continue
		}
		rd, _ := proc.ReadUint32(h, ce)
		sb.WriteString(readWide(h, rd, 16))
	}
	return sb.String(), charBegin, numChars
}

// lyricTrackStride 是 LyricHost 里相邻两条歌词轨的间距（与 player/wesing/lyric/roma.go 的 trackStride 同值）。
const lyricTrackStride = 0x70

// readCString 读 ATL 式 CString：指针指向字符，字符数在指针前 12 字节。
func readCString(h syscall.Handle, ptr uint32) string {
	if !looksPtr(ptr) {
		return ""
	}
	n, err := proc.ReadUint32(h, ptr-12)
	if err != nil || n == 0 || n > 520 {
		return ""
	}
	return readWide(h, ptr, int(n))
}

// dumpTracks 列出 LyricHost 里的各条歌词轨：从主歌词子结构起每隔 lyricTrackStride 一条，虚表与主歌词
// 相同的才算。每条打印路径（+0x04）、行数（+0x48/+0x4C；+0x50 是容量末尾）与前 maxLines 行的文本。
func dumpTracks(h syscall.Handle, mainTrack uint32, maxLines int) {
	vtable, _ := proc.ReadUint32(h, mainTrack)
	for i := uint32(0); i < 6; i++ {
		track := mainTrack + i*lyricTrackStride
		vt, err := proc.ReadUint32(h, track)
		if err != nil || vt != vtable {
			fmt.Printf("  轨 %d @0x%08X：虚表 0x%08X 与主歌词不同，到此为止\n", i, track, vt)
			return
		}
		pathPtr, _ := proc.ReadUint32(h, track+0x04)
		begin, _ := proc.ReadUint32(h, track+0x48)
		end, _ := proc.ReadUint32(h, track+0x4C)
		capEnd, _ := proc.ReadUint32(h, track+0x50)
		fmt.Printf("  轨 %d @0x%08X  路径=%q\n", i, track, readCString(h, pathPtr))
		if !looksPtr(begin) || end <= begin {
			fmt.Println("      （没有行）")
			continue
		}
		n := (end - begin) / 4
		fmt.Printf("      行数=%d（容量 %d）\n", n, (capEnd-begin)/4)
		for j := uint32(0); j < n && int(j) < maxLines; j++ {
			entry, _ := proc.ReadUint32(h, begin+j*4)
			t, _ := proc.ReadFloat32(h, entry)
			text, _, _ := entryMainText(h, entry)
			fmt.Printf("      [%2d] %7.2fs  %q\n", j, t, text)
		}
	}
}

// probeVectors 对 base+[0,maxOff] 每个像指针的偏移，尝试解释为「LyricEntry* 向量的
// begin」：读首个 LyricEntry → 其字符向量(+0x08) → 首字主文本。主歌词向量首字是主歌词
// （"Hey"），若某偏移读出的首字是罗马音，那条向量即音译轨。
func probeVectors(h syscall.Handle, base, maxOff uint32) {
	for off := uint32(0); off <= maxOff; off += 4 {
		v, err := proc.ReadUint32(h, base+off)
		if err != nil || !looksPtr(v) {
			continue
		}
		entry0, _ := proc.ReadUint32(h, v)
		if !looksPtr(entry0) {
			continue
		}
		cb, _ := proc.ReadUint32(h, entry0+0x08)
		if !looksPtr(cb) {
			continue
		}
		ce0, _ := proc.ReadUint32(h, cb)
		if !looksPtr(ce0) {
			continue
		}
		rd, _ := proc.ReadUint32(h, ce0)
		if s := readWide(h, rd, 16); looksText(s) {
			end, _ := proc.ReadUint32(h, base+off+4)
			nEntries := int32(0)
			if looksPtr(end) && end > v {
				nEntries = int32((end - v) / 4)
			}
			fmt.Printf("    +0x%02X → 向量? 首行首字=%q  行数≈%d  (begin=0x%X entry0=0x%X)\n", off, s, nEntries, v, entry0)
		}
	}
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("[wesingprobe] ")
	lineFilter := flag.String("line", "", "只 dump 主文本含此子串的行（如 手を伸）")
	maxLines := flag.Int("n", 3, "最多 dump 几行")
	perLineChars := flag.Int("c", 4, "每行 dump 前几个 CharElement")
	searchText := flag.String("search", "", "全内存(可写区)搜此 UTF-16 文本，报告命中地址与上下文")
	flag.Parse()

	pid, err := proc.FindProcess("WeSing.exe")
	if err != nil {
		log.Fatalf("找不到 WeSing.exe：%v（K歌没开？）", err)
	}
	h, err := proc.OpenProc(pid)
	if err != nil {
		log.Fatalf("OpenProc 失败：%v（读进程内存需要管理员权限）", err)
	}
	defer proc.CloseProc(h)

	if *searchText != "" {
		pat, _ := utf16le(*searchText)
		all := searchAllReadable(h, pat)
		fmt.Printf("搜索 %q（UTF-16，全可读区）：%d 处命中\n", *searchText, len(all))
		for i, a := range all {
			if i >= 30 {
				fmt.Printf("  ...另 %d 处略\n", len(all)-30)
				break
			}
			fmt.Printf("  0x%08X  上下文=%q\n", a, readWide(h, a, 120))
		}
		return
	}

	modules, err := proc.EnumModules(pid)
	if err != nil {
		log.Fatalf("EnumModules 失败：%v", err)
	}
	hostAddr, subStruct, err := lyric.FindLyricHost(h, modules)
	if err != nil {
		log.Fatalf("FindLyricHost 失败：%v（K歌正在显示歌词吗？）", err)
	}
	fmt.Printf("WeSing.exe pid=%d  LyricHost=0x%08X  subStruct=0x%08X\n", pid, hostAddr, subStruct)

	// 找音译向量：主歌词向量在 subStruct+0x48。音译可能是 subStruct 下另一组 begin/end，
	// 或 LyricHost 另一子结构。扫两者周边，把每个像指针的偏移当歌词向量试读首行首字。
	fmt.Println("---- LyricHost 周边找歌词向量 ----")
	probeVectors(h, hostAddr, 0x40)
	fmt.Println("---- subStruct 周边找歌词向量 ----")
	probeVectors(h, subStruct, 0x80)

	// 多 LyricHost 实例：K歌可能主歌词、音译各一个实例（同 vtable）。实例首字段即 vtable，
	// 用它 AOBScan 全部实例，各读首行首字——首字是罗马音的那个实例就是音译轨。
	fmt.Println("---- 所有 LyricHost 实例（同 vtable）----")
	vtable, _ := proc.ReadUint32(h, hostAddr)
	regions := proc.EnumWritableRegions(h)
	pat, msk := proc.Uint32ToAOB(vtable)
	hosts := proc.AOBScan(h, pat, msk, regions)
	fmt.Printf("vtable=0x%08X，实例数=%d\n", vtable, len(hosts))
	for _, host := range hosts {
		sub := host + 0x0C
		b, _ := proc.ReadUint32(h, sub+0x48)
		e, _ := proc.ReadUint32(h, sub+0x50)
		if !looksPtr(b) || e <= b {
			continue
		}
		e0, _ := proc.ReadUint32(h, b)
		cb, _ := proc.ReadUint32(h, e0+0x08)
		ce0, _ := proc.ReadUint32(h, cb)
		rd, _ := proc.ReadUint32(h, ce0)
		fmt.Printf("  host=0x%08X sub=0x%08X 行数=%d 首字=%q\n", host, sub, (e-b)/4, readWide(h, rd, 16))
	}
	fmt.Println()

	// 各条歌词轨（主歌词 / 音译 …）。音译轨与主歌词逐行对应，纯英文行在音译轨里是单独一个空格或照抄原文。
	fmt.Println("---- LyricHost 里的各条歌词轨 ----")
	dumpTracks(h, subStruct, *maxLines)
	fmt.Println()

	beginPtr, _ := proc.ReadUint32(h, subStruct+0x48)
	endPtr, _ := proc.ReadUint32(h, subStruct+0x50)
	if beginPtr == 0 || endPtr <= beginPtr {
		log.Fatalf("歌词向量为空 begin=0x%X end=0x%X（换歌或等歌词加载后重试）", beginPtr, endPtr)
	}
	numEntries := (endPtr - beginPtr) / 4
	fmt.Printf("歌词行数=%d\n\n", numEntries)

	dumped := 0
	for i := uint32(0); i < numEntries && dumped < *maxLines; i++ {
		entryPtr, _ := proc.ReadUint32(h, beginPtr+i*4)
		if entryPtr == 0 {
			continue
		}
		mainText, charBegin, numChars := entryMainText(h, entryPtr)
		if mainText == "" {
			continue
		}
		if *lineFilter != "" && !strings.Contains(mainText, *lineFilter) {
			continue
		}
		timeVal, _ := proc.ReadFloat32(h, entryPtr)
		fmt.Printf("========== 行 %d  time=%.2fs  %q  (%d 字符) ==========\n", i, timeVal, mainText, numChars)
		// LyricEntry 本体（0x20 字节）：+0x00 行起始秒、+0x04 行时长秒、+0x08/+0x0C/+0x10 字级单元向量
		// 的 begin / end / 容量末尾。
		dumpStruct(h, entryPtr, 0x1C, "LyricEntry")
		// 前若干 CharElement：+0x00 主文本 RenderData、+0x04 起始、+0x08 时长已知。
		for c := uint32(0); c < numChars && c < uint32(*perLineChars); c++ {
			ce, _ := proc.ReadUint32(h, charBegin+c*4)
			if ce == 0 {
				continue
			}
			rd, _ := proc.ReadUint32(h, ce)
			fmt.Printf("  -- CharElement[%d] 主文本=%q --\n", c, readWide(h, rd, 16))
			dumpStruct(h, ce, 0x30, fmt.Sprintf("CharElement[%d]", c))
		}
		fmt.Println()
		dumped++
	}
	if dumped == 0 {
		fmt.Println("没 dump 到匹配行（--line 过滤没命中？或歌词还没加载）")
	}
}
