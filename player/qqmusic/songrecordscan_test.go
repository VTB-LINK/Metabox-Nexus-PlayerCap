package qqmusic

// 本文件只测 scanRecord 怎样扫 QQ 的内存找当前歌曲的记录（设计 §6.4 第 4 步「扫描」「网址截取」）：只读私有、已提交、
// 保护恰为 PAGE_READWRITE 的区域；分块读、相邻块重叠，命中在块首、跨块、块尾与区域末尾都只报一次；读取总量到上限即停；
// 块读失败或只读到一部分时照常往后、记不完整；命中超过 64 处即停；块间 stop 为真即中止；地址空间到头、区域不前进或
// base+size 溢出时遍历即停；缓冲区复用。地址空间是假的（fakeSpace），上限用小的 scanLimits。

import (
	"bytes"
	"fmt"
	"slices"
	"testing"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/player/qqmusic/assoc"
)

// scanNeedle 是各用例的前缀：专辑 A、歌手 B、歌名 C。
var scanNeedle = []byte("ABChttp")

// scanRec 是一条以 scanNeedle 开头的记录，网址为 mid 键 mid。
func scanRec(mid string) []byte { return recordBytes("A", "B", "C", midURL(mid), mid) }

// scanOverlap 是相邻块的重叠：前缀长度 + 192 − 1（设计 §6.4 第 4 步「扫描」）。
var scanOverlap = len(scanNeedle) + assoc.RecordTailMax - 1

// regionWith 返回 size 字节的区域内容，在 at 的各个偏移处放上对应的记录，其余为 0xEE。
func regionWith(size int, at map[int][]byte) []byte {
	b := bytes.Repeat([]byte{0xEE}, size)
	for off, rec := range at {
		copy(b[off:], rec)
	}
	return b
}

func never() bool { return false }

func tailURLs(sc assoc.RecordScan) []string {
	var out []string
	for _, t := range sc.Tails {
		out = append(out, t.URL)
	}
	return out
}

// wantNoReadPastRegion 断言每次读都在某个区域之内，不越过区域末尾。
func wantNoReadPastRegion(t *testing.T, s *fakeSpace) {
	t.Helper()
	for _, rd := range s.reads {
		ok := false
		for _, r := range s.regions {
			if rd.addr >= r.base && rd.addr-r.base+uintptr(rd.n) <= r.size {
				ok = true
			}
		}
		if !ok || rd.n <= 0 {
			t.Fatalf("读 %#x 起 %d 字节，越过区域末尾或长度不为正", rd.addr, rd.n)
		}
	}
}

func TestRecordScanLimits(t *testing.T) {
	if recordScanLimits != (scanLimits{chunk: 4 << 20, total: 1536 << 20}) {
		t.Fatalf("recordScanLimits = %+v，want 每块 4 MiB、总量 1.5 GiB", recordScanLimits)
	}
	if assoc.RecordHitsMax != 64 {
		t.Fatalf("assoc.RecordHitsMax = %d，want 64", assoc.RecordHitsMax)
	}
}

// 只读已提交、私有、保护恰为 PAGE_READWRITE 的区域。反例：映像、映射、带 PAGE_GUARD、带 PAGE_NOCACHE、PAGE_EXECUTE_READWRITE、
// PAGE_WRITECOPY、PAGE_READONLY、保留与空闲区域——各放一条记录，都不读、记录都不出现。
// 变异自证（实测）：不看类型、保护按位与 PAGE_READWRITE 判、不看状态，三种各自红（分别多读了映像与映射、带 PAGE_GUARD 与
// PAGE_NOCACHE、保留与空闲的区域）。
func TestScanRecordRegionFilter(t *testing.T) {
	type kind struct {
		name                string
		state, protect, typ uint32
	}
	kinds := []kind{
		{"私有可写", windows.MEM_COMMIT, windows.PAGE_READWRITE, memPrivate},
		{"映像", windows.MEM_COMMIT, windows.PAGE_READWRITE, memImage},
		{"映射", windows.MEM_COMMIT, windows.PAGE_READWRITE, memMapped},
		{"PAGE_GUARD", windows.MEM_COMMIT, windows.PAGE_READWRITE | windows.PAGE_GUARD, memPrivate},
		{"PAGE_NOCACHE", windows.MEM_COMMIT, windows.PAGE_READWRITE | windows.PAGE_NOCACHE, memPrivate},
		{"PAGE_EXECUTE_READWRITE", windows.MEM_COMMIT, windows.PAGE_EXECUTE_READWRITE, memPrivate},
		{"PAGE_WRITECOPY", windows.MEM_COMMIT, windows.PAGE_WRITECOPY, memPrivate},
		{"PAGE_READONLY", windows.MEM_COMMIT, windows.PAGE_READONLY, memPrivate},
		{"保留", windows.MEM_RESERVE, windows.PAGE_READWRITE, memPrivate},
		{"空闲", memFree, windows.PAGE_READWRITE, memPrivate},
		{"又一个私有可写", windows.MEM_COMMIT, windows.PAGE_READWRITE, memPrivate},
	}
	s := &fakeSpace{log: true}
	for i, k := range kinds {
		data := regionWith(256, map[int][]byte{8: scanRec(fmt.Sprintf("00%dRegion", i))})
		s.regions = append(s.regions, fakeRegion{memRegion{base: uintptr(0x10000 * (i + 1)), size: 256, state: k.state, protect: k.protect, typ: k.typ}, data})
	}
	sc, st, aborted := scanRecord(s.source(), scanNeedle, scanLimits{chunk: 1024, total: 1 << 20}, never)
	var addrs []uintptr
	for _, rd := range s.reads {
		addrs = append(addrs, rd.addr)
	}
	if want := []uintptr{0x10000, 0x10000 * uintptr(len(kinds))}; !slices.Equal(addrs, want) {
		t.Fatalf("读了 %#x，want 只读两个私有可写区 %#x", addrs, want)
	}
	if want := []string{midURL("000Region"), midURL(fmt.Sprintf("00%dRegion", len(kinds)-1))}; !slices.Equal(tailURLs(sc), want) {
		t.Fatalf("Tails %q，want %q", tailURLs(sc), want)
	}
	if aborted || !sc.Complete || sc.Malformed != 0 || st.failedChunks != 0 || st.capped || st.read != 512 {
		t.Fatalf("aborted %v、Complete %v、Malformed %d、统计 %+v", aborted, sc.Complete, sc.Malformed, st)
	}
}

// 块 1024 字节、重叠 scanOverlap，块 k 自 k·step 起（step = 1024 − scanOverlap），命中起点落在重叠部分的留给下一块。记录放在：
// 前缀跨第 2 块的起点；前缀跨第 1 块所读范围的末尾；尾部跨第 3 块的起点；前缀恰在第 2 块所读范围的末尾（尾部全在下一块）；
// 第 4 块的块首；区域末尾而参数 0 之后被截断（计 Malformed）。各报一次，次序按地址；每次读都不越过区域末尾。
// 变异自证（实测）：去掉重叠（下一块自本块末尾起）而红；在本块报全部命中（不留给下一块）而红。
func TestScanRecordChunkBoundaries(t *testing.T) {
	const chunk = 1024
	step := chunk - scanOverlap
	size := 3*step + 600
	keep := len("ABC") + len(midURL("006Truncated")) + len("0=") + 3 // 区域末尾只留到参数 0 的前 3 个字符
	at := map[int][]byte{
		step - 3:                      scanRec("001Straddle"),  // 前缀跨第 2 块的起点（仍归第 1 块）
		chunk - 3:                     scanRec("002ReadEdge"),  // 前缀跨第 1 块所读范围的末尾（归第 2 块）
		2*step - 30:                   scanRec("003TailCross"), // 前缀归第 2 块，尾部跨第 3 块的起点
		step + chunk - len("ABChttp"): scanRec("004ChunkEnd"),  // 前缀恰好填满第 2 块所读范围的末尾（归第 3 块）
		3 * step:                      scanRec("005Start"),     // 第 4 块的块首
		size - keep:                   scanRec("006Truncated"), // 区域末尾，参数 0 之后的 & 被截掉
	}
	s := &fakeSpace{log: true, regions: []fakeRegion{privRegion(0x10000, regionWith(size, at))}}
	sc, st, _ := scanRecord(s.source(), scanNeedle, scanLimits{chunk: chunk, total: 1 << 20}, never)
	want := []string{midURL("001Straddle"), midURL("002ReadEdge"), midURL("003TailCross"), midURL("004ChunkEnd"), midURL("005Start")}
	if !slices.Equal(tailURLs(sc), want) || sc.Malformed != 1 {
		t.Fatalf("Tails %q、Malformed %d，want %q、1", tailURLs(sc), sc.Malformed, want)
	}
	wantNoReadPastRegion(t, s)
	var starts []uintptr
	for _, rd := range s.reads {
		starts = append(starts, rd.addr-0x10000)
	}
	if want := []uintptr{0, uintptr(step), uintptr(2 * step), uintptr(3 * step)}; !slices.Equal(starts, want) {
		t.Fatalf("各块起点 %d，want %d", starts, want)
	}
	if !sc.Complete || st.failedChunks != 0 || st.read != int64(3*chunk+(size-3*step)) {
		t.Fatalf("Complete %v、统计 %+v", sc.Complete, st)
	}
}

// 两区共 1200 字节、读取总量上限 1000：读取量恰为 1000，到上限即停、不完整；上限之内的两条记录都在（含第二区里预算截短的末块），
// 上限之外的那条不出现，此后不再读。
// 变异自证（实测）：不封顶而红；末块按预算截短时仍留重叠给下一块（第二区上限内的那条没有块报它）而红。
func TestScanRecordReadCap(t *testing.T) {
	s := &fakeSpace{log: true, regions: []fakeRegion{
		privRegion(0x10000, regionWith(600, map[int][]byte{20: scanRec("001First")})),
		privRegion(0x20000, regionWith(600, map[int][]byte{10: scanRec("002WithinCap"), 300: scanRec("003BeyondCap")})),
	}}
	sc, st, aborted := scanRecord(s.source(), scanNeedle, scanLimits{chunk: 512, total: 1000}, never)
	if want := []string{midURL("001First"), midURL("002WithinCap")}; !slices.Equal(tailURLs(sc), want) {
		t.Fatalf("Tails %q，want %q", tailURLs(sc), want)
	}
	total := 0
	for _, rd := range s.reads {
		total += rd.n
	}
	if aborted || st.read != 1000 || total != 1000 || !st.capped || sc.Complete || st.failedChunks != 0 {
		t.Fatalf("读取量 %d（各次合计 %d）、统计 %+v、Complete %v，want 恰为 1000、capped、不完整", st.read, total, st, sc.Complete)
	}
}

// 某块读失败（一个字节也没读到），或只读到一半：都计 failedChunks、照常往后读、不完整；失败块里没读到的记录不出现，只读到一半
// 的那块里读到的部分照常处理。
// 变异自证（实测）：失败即停止扫描，两例都红；只读到一部分时仍按整块处理，两例都红；只读到一部分不计 failedChunks，
// 只读到一半一例红。
func TestScanRecordIncompleteOnReadFailure(t *testing.T) {
	for _, c := range []struct {
		name string
		got  int // 第二区那次读报告读到的字节数
		want []string
	}{
		{"整块读失败", 0, []string{midURL("001Before"), midURL("004After")}},
		{"只读到一半", 400, []string{midURL("001Before"), midURL("002FirstHalf"), midURL("004After")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &fakeSpace{regions: []fakeRegion{
				privRegion(0x10000, regionWith(800, map[int][]byte{0: scanRec("001Before")})),
				privRegion(0x20000, regionWith(800, map[int][]byte{100: scanRec("002FirstHalf"), 500: scanRec("003LostHalf")})),
				privRegion(0x30000, regionWith(800, map[int][]byte{0: scanRec("004After")})),
			}}
			s.fail = func(addr uintptr, n int) int {
				if addr == 0x20000 {
					return c.got
				}
				return -1
			}
			sc, st, aborted := scanRecord(s.source(), scanNeedle, scanLimits{chunk: 1024, total: 1 << 20}, never)
			if !slices.Equal(tailURLs(sc), c.want) || aborted || sc.Complete || st.failedChunks != 1 || st.capped {
				t.Fatalf("Tails %q、aborted %v、Complete %v、统计 %+v，want %q、失败 1 块、不完整", tailURLs(sc), aborted, sc.Complete, st, c.want)
			}
		})
	}
}

// QQ 在扫描中途退出，此后每块都读失败：扫描照常走完、每块计一次失败、不完整，不 panic、不死循环。
func TestScanRecordAllReadsFail(t *testing.T) {
	s := &fakeSpace{log: true, regions: []fakeRegion{
		privRegion(0x10000, regionWith(3000, map[int][]byte{0: scanRec("001Gone")})),
		privRegion(0x20000, regionWith(100, nil)),
	}}
	s.fail = func(uintptr, int) int { return 0 }
	sc, st, aborted := scanRecord(s.source(), scanNeedle, scanLimits{chunk: 1024, total: 1 << 20}, never)
	if aborted || sc.Complete || len(sc.Tails) != 0 || sc.Malformed != 0 || st.failedChunks != len(s.reads) || len(s.reads) != 5 {
		t.Fatalf("aborted %v、Complete %v、Tails %q、读 %d 次、统计 %+v，want 第一区 4 块与第二区 1 块各失败一次、不完整",
			aborted, sc.Complete, tailURLs(sc), len(s.reads), st)
	}
}

// 第 65 处前缀命中即停、记 OverCap，之后不再读（其后的区域一次也不读）；恰 64 处不算（反例）。截取不合的命中也算命中。
// 变异自证（实测）：第 66 处才停，65 处一例红；记 OverCap 后不停，65 处一例红（后一区被读）。
func TestScanRecordOverCap(t *testing.T) {
	for _, n := range []int{64, 65} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			at := map[int][]byte{}
			for i := range n {
				rec := scanRec(fmt.Sprintf("%03dHit", i))
				if i == 0 {
					rec = []byte("ABChttp://not.a.record/")
				}
				at[i*100] = rec
			}
			s := &fakeSpace{log: true, regions: []fakeRegion{
				privRegion(0x10000, regionWith(n*100+100, at)),
				privRegion(0x100000, regionWith(200, nil)),
			}}
			sc, _, _ := scanRecord(s.source(), scanNeedle, scanLimits{chunk: 1 << 20, total: 1 << 30}, never)
			readAfter := slices.ContainsFunc(s.reads, func(r fakeRead) bool { return r.addr == 0x100000 })
			if n == 64 && (sc.OverCap || len(sc.Tails) != 63 || sc.Malformed != 1 || !readAfter) {
				t.Fatalf("64 处：OverCap %v、Tails %d、Malformed %d、读后一区 %v，want 不超、合格 63、不合 1、照常读后一区",
					sc.OverCap, len(sc.Tails), sc.Malformed, readAfter)
			}
			if n == 65 && (!sc.OverCap || len(sc.Tails)+sc.Malformed != 64 || readAfter) {
				t.Fatalf("65 处：OverCap %v、Tails %d、Malformed %d、读后一区 %v，want OverCap、看了 64 处、不再读",
					sc.OverCap, len(sc.Tails), sc.Malformed, readAfter)
			}
		})
	}
}

// 每块之前调用 stop：第二块之前为真即中止、aborted，之后不再读；第一块之前就为真则一次也不读。
// 变异自证（实测）：只在区域的第一块之前查 stop，同一区域的第二块照常读，第二块一例红。
func TestScanRecordStops(t *testing.T) {
	for _, stopAt := range []int{1, 2} {
		t.Run(fmt.Sprint(stopAt), func(t *testing.T) {
			s := &fakeSpace{log: true, regions: []fakeRegion{privRegion(0x10000, regionWith(3000, nil))}}
			calls := 0
			_, _, aborted := scanRecord(s.source(), scanNeedle, scanLimits{chunk: 1024, total: 1 << 20}, func() bool {
				calls++
				return calls >= stopAt
			})
			if !aborted || len(s.reads) != stopAt-1 || calls != stopAt {
				t.Fatalf("aborted %v、读 %d 次、stop 调用 %d 次，want 中止、读 %d 次", aborted, len(s.reads), calls, stopAt-1)
			}
		})
	}
}

// 区域的 base+size 溢出，或 query 返回的区域不前进（末尾不在查询地址之后）：遍历即停、不死循环，记不完整（区域查询失败或
// 不前进）——查询点之后的内存没读到。
// 变异自证（实测）：只认 query 报错为停，两例都红（查询转到桩的 1000 次上限）；停下而不记不完整，两例都红。
func TestScanRecordAddressWrap(t *testing.T) {
	for _, c := range []struct {
		name string
		q    func(addr uintptr) (memRegion, error)
	}{
		{"base+size 溢出", func(addr uintptr) (memRegion, error) {
			if addr == 0 {
				return memRegion{base: 0, size: 0x1000, state: memFree}, nil
			}
			return memRegion{base: addr, size: ^uintptr(0) - 0x10, state: windows.MEM_COMMIT, protect: windows.PAGE_READWRITE, typ: memPrivate}, nil
		}},
		{"区域不前进", func(addr uintptr) (memRegion, error) {
			return memRegion{base: 0, size: 0x1000, state: memFree}, nil
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			queries, reads := 0, 0
			src := memSource{
				query: func(addr uintptr) (memRegion, error) {
					if queries++; queries > 1000 {
						return memRegion{}, windows.ERROR_INVALID_PARAMETER
					}
					return c.q(addr)
				},
				read: func(uintptr, []byte) (int, error) { reads++; return 0, errFakeNoAccess },
			}
			sc, st, aborted := scanRecord(src, scanNeedle, scanLimits{chunk: 1024, total: 1 << 20}, never)
			if aborted || queries > 2 || reads != 0 || sc.Complete || !st.queryFailed {
				t.Fatalf("查询 %d 次、读 %d 次、Complete %v、统计 %+v，want 不超过 2 次查询即停、不读、记区域查询失败、不完整",
					queries, reads, sc.Complete, st)
			}
		})
	}
}

// 区域查询报错（设计 §6.4 第 4 步「结论」：扫描不完整时不下认出或找不到的结论）：只有 VirtualQueryEx 报
// ERROR_INVALID_PARAMETER（地址超出进程可访问的最高地址）才是地址空间到头，扫描照常完整；其余错误即停、记不完整，之前读到的
// 记录照常在 Tails 里，查询点之后的区域一次也不读。QQ 中途退出时之后的查询与读取都失败，按同一口径不完整，读取者按日程再读，
// 会话随后由 CheckValid 结束。
// 变异自证（实测）：任何查询错误都当作到头（不记不完整），「中途报别的错」「第一次查询就报错」「QQ 中途退出」三例红；
// Complete 不看查询失败，前两例红（「QQ 中途退出」另有块读失败，照样不完整）；ERROR_INVALID_PARAMETER 也记不完整，
// 「到头」一例红。
func TestScanRecordQueryFailure(t *testing.T) {
	space := func() *fakeSpace {
		return &fakeSpace{log: true, regions: []fakeRegion{
			privRegion(0x10000, regionWith(3000, map[int][]byte{0: scanRec("001Old")})),
			privRegion(0x30000, regionWith(800, map[int][]byte{0: scanRec("002Current")})),
		}}
	}
	from := func(at uintptr) func(uintptr) error {
		return func(addr uintptr) error {
			if addr >= at {
				return windows.ERROR_ACCESS_DENIED
			}
			return nil
		}
	}
	for _, c := range []struct {
		name     string
		setup    func(s *fakeSpace)
		complete bool
		tails    []string
		failed   int // 块读失败的次数
	}{
		{"地址空间到头（ERROR_INVALID_PARAMETER）", func(*fakeSpace) {}, true, []string{midURL("001Old"), midURL("002Current")}, 0},
		{"中途报别的错", func(s *fakeSpace) { s.qfail = from(0x10000 + 3000) }, false, []string{midURL("001Old")}, 0},
		{"第一次查询就报错", func(s *fakeSpace) { s.qfail = from(0) }, false, nil, 0},
		{"QQ 中途退出", func(s *fakeSpace) {
			s.qfail = from(0x10000 + 3000)
			s.fail = func(addr uintptr, _ int) int {
				if addr > 0x10000 {
					return 0
				}
				return -1
			}
		}, false, []string{midURL("001Old")}, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := space()
			c.setup(s)
			sc, st, aborted := scanRecord(s.source(), scanNeedle, scanLimits{chunk: 1024, total: 1 << 20}, never)
			readCurrent := slices.ContainsFunc(s.reads, func(r fakeRead) bool { return r.addr == 0x30000 })
			if aborted || sc.Complete != c.complete || st.queryFailed == c.complete || !slices.Equal(tailURLs(sc), c.tails) ||
				st.failedChunks != c.failed || readCurrent != c.complete {
				t.Fatalf("Complete %v、统计 %+v、Tails %q、读了后一区 %v，want Complete %v、Tails %q、失败 %d 块",
					sc.Complete, st, tailURLs(sc), readCurrent, c.complete, c.tails, c.failed)
			}
		})
	}
}

// 奇形区域：比前缀还短；大小为块 + 1（第二块只余重叠加 1 字节，记录跨两块）；末块只读到一部分（记录在读到的部分里）。
// 不越过区域末尾读，命中不重复，不 panic。
func TestScanRecordOddRegions(t *testing.T) {
	const chunk = 1024
	s := &fakeSpace{log: true, regions: []fakeRegion{
		privRegion(0x10000, []byte("ABC")),
		privRegion(0x20000, regionWith(chunk+1, map[int][]byte{chunk + 1 - len(scanRec("001Plus1")): scanRec("001Plus1")})),
		privRegion(0x30000, regionWith(chunk+300, map[int][]byte{chunk - scanOverlap + 20: scanRec("002Partial"), chunk + 200: scanRec("003Unread")})),
	}}
	s.fail = func(addr uintptr, n int) int {
		if addr == 0x30000+uintptr(chunk-scanOverlap) {
			return 150
		}
		return -1
	}
	sc, st, _ := scanRecord(s.source(), scanNeedle, scanLimits{chunk: chunk, total: 1 << 20}, never)
	if want := []string{midURL("001Plus1"), midURL("002Partial")}; !slices.Equal(tailURLs(sc), want) || sc.Malformed != 0 {
		t.Fatalf("Tails %q、Malformed %d，want %q、0", tailURLs(sc), sc.Malformed, want)
	}
	wantNoReadPastRegion(t, s)
	if len(s.reads) != 5 || st.failedChunks != 1 {
		t.Fatalf("读 %d 次、失败 %d 块，want 三区共 5 块、失败 1 块", len(s.reads), st.failedChunks)
	}
}

// 缓冲区复用：1 个区域与 50 个区域（各两块）的分配次数相同。
// 变异自证（实测）：每块新分配缓冲区而红。
func TestScanRecordBufferReuse(t *testing.T) {
	spaceOf := func(n int) *fakeSpace {
		s := &fakeSpace{}
		for i := range n {
			s.regions = append(s.regions, privRegion(uintptr(0x10000*(i+1)), regionWith(1500, nil)))
		}
		return s
	}
	one, fifty := spaceOf(1), spaceOf(50)
	lim := scanLimits{chunk: 1024, total: 1 << 30}
	a1 := testing.AllocsPerRun(10, func() { scanRecord(one.source(), scanNeedle, lim, never) })
	a50 := testing.AllocsPerRun(10, func() { scanRecord(fifty.source(), scanNeedle, lim, never) })
	if a1 != a50 {
		t.Fatalf("1 个区域分配 %v 次、50 个区域 %v 次，want 相同", a1, a50)
	}
}
