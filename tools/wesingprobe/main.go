//go:build windows

// Command wesingprobe 是全民K歌「音译歌词」内存探测工具（只读、仅本地调试）。
//
// 探测结论（2026-09）：wesing 歌词堆里只有主歌词假名/汉字 + 逐字时间；罗马音/音译任何形态
// （连写 / 带空格 / 单音节）在可写区与全可读区均 0 命中——K 歌音译系运行时动态转写、不落
// 字符串。故 wesing 音译无法从内存直读、保持不支持。本工具为该结论的可复现证据留档（issue #34）。
//
// 复用 wesing 生产定位链（FindLyricHost → LyricEntry → CharElement），对指定行的
// LyricEntry 及其前若干 CharElement 逐 4 字节 dump，float 解读 + 跟随每个指针读
// UTF-16 字符串，用以定位音译（罗马音 / 韩罗 / 粤拼 / 俄拉丁…）挂在哪个偏移：
// 可能是 CharElement 的某个偏移指向音译 RenderData（逐字），也可能是 LyricEntry 下
// 第二条并行字符向量（整行音译轨）。
//
// 只读约束（AGENTS.md §0）：仅经 wesing/proc 的只读原语读取——OpenProc 只申请
// PROCESS_VM_READ|PROCESS_QUERY_INFORMATION，句柄层面无写权限；绝不写目标进程。
// 自包含、仅 Windows 构建，不改动任何生产代码，不进出货二进制。
//
// 用法（需管理员权限读进程内存；K歌开着「音译歌词」开关、播放或暂停在某行）：
//
//	go run ./tools/wesingprobe --line "手を伸"   // 只 dump 主文本含该子串的行
//	go run ./tools/wesingprobe -n 3              // dump 前 3 行
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

// dumpSecondVector 把 LyricEntry+0x30/+0x34 当第二字符向量遍历，逐元素读主文本
// （CharElement+0x00 → RenderData → UTF-16），以 | 分隔打印；再 dump 首元素结构，
// 用以确认它是否为整行音译轨。
func dumpSecondVector(h syscall.Handle, entryPtr uint32) {
	begin, _ := proc.ReadUint32(h, entryPtr+0x30)
	end, _ := proc.ReadUint32(h, entryPtr+0x34)
	if !looksPtr(begin) || end <= begin {
		fmt.Printf("  ++ 第二向量(+0x30): 无（begin=0x%X end=0x%X）\n", begin, end)
		return
	}
	n := (end - begin) / 4
	if n > 500 {
		fmt.Printf("  ++ 第二向量(+0x30): 元素数异常 %d\n", n)
		return
	}
	var sb strings.Builder
	for c := uint32(0); c < n; c++ {
		ce, _ := proc.ReadUint32(h, begin+c*4)
		if ce == 0 {
			continue
		}
		rd, _ := proc.ReadUint32(h, ce)
		sb.WriteString(readWide(h, rd, 16))
		sb.WriteString("|")
	}
	fmt.Printf("  ++ 第二向量(+0x30) %d 元素文本: %q\n", n, sb.String())
	if ce0, _ := proc.ReadUint32(h, begin); ce0 != 0 {
		dumpStruct(h, ce0, 0x18, "第二向量CharElement[0]")
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
		// LyricEntry 本体：+0x00 time、+0x08/+0x0C 主字符向量已知；重点看 +0x10 起有没有
		// 第二对 begin/end 指针（整行音译轨候选）。
		dumpStruct(h, entryPtr, 0x48, "LyricEntry")
		dumpSecondVector(h, entryPtr)
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
