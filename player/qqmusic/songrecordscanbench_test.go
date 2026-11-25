package qqmusic

// 本文件只测 scanRecord 在 256 MiB 地址空间上的吞吐（设计 §6.10「读当前歌曲记录」、§7「大曲库」）：假的读取 seam 按地址生成内容、
// 不真分配，末尾放一条当前歌曲的记录；断言认出它、缓冲区只分配一次（分配数不随区域数增长）。只计扫描本身的 CPU 开销，不含
// ReadProcessMemory 由内核复制 QQ 页面的开销。

import (
	"math/rand/v2"
	"testing"

	"golang.org/x/sys/windows"

	"Metabox-Nexus-PlayerCap/player/qqmusic/assoc"
)

const (
	genBase     = 0x10000000 // 生成的地址空间自此起
	genSize     = 256 << 20
	genPageSize = 64 << 10 // 内容按地址取自这一页，genBase 与各区域都按它对齐
	genName     = "大曲库"
	genMid      = "003GenSpace01"
)

// genSpace 是 memSource 的桩：genSize 字节等分为 regions 个相接的私有可写区域（自 genBase 起），此前为空闲、此后地址空间
// 到头。地址 a 处的内容为 page[a % genPageSize]：伪随机字节，另有两条同专辑、同歌手、别的歌名的记录（前缀只差在歌名）；
// 最后一个区域末尾之前 512 字节处覆盖一条当前歌曲的记录。read 只拷贝，不分配。
type genSpace struct {
	regionSize uintptr
	page, rec  []byte
	recAt      uintptr
}

func newGenSpace(regions int) *genSpace {
	r := rand.New(rand.NewPCG(11, 12))
	page := make([]byte, genPageSize)
	for i := range page {
		page[i] = byte(r.IntN(256))
	}
	copy(page[1000:], recordBytes(fakeAlbum, fakeSinger, "别的歌一", midURL("003Other0001"), "003Other0001"))
	copy(page[40000:], recordBytes(fakeAlbum, fakeSinger, "别的歌二", midURL("003Other0002"), "003Other0002"))
	rec := recordBytes(fakeAlbum, fakeSinger, genName, midURL(genMid), genMid)
	return &genSpace{regionSize: genSize / uintptr(regions), page: page, rec: rec, recAt: genBase + genSize - 512 - uintptr(len(rec))}
}

func (s *genSpace) source() memSource { return memSource{query: s.query, read: s.read} }

func (s *genSpace) query(addr uintptr) (memRegion, error) {
	switch {
	case addr < genBase:
		return memRegion{base: addr, size: genBase - addr, state: memFree}, nil
	case addr < genBase+genSize:
		base := addr - (addr-genBase)%s.regionSize
		return memRegion{base: base, size: s.regionSize, state: windows.MEM_COMMIT, protect: windows.PAGE_READWRITE, typ: memPrivate}, nil
	}
	return memRegion{}, windows.ERROR_INVALID_PARAMETER
}

func (s *genSpace) read(addr uintptr, buf []byte) (int, error) {
	for i := 0; i < len(buf); {
		i += copy(buf[i:], s.page[(addr+uintptr(i))%genPageSize:])
	}
	if lo, hi := max(addr, s.recAt), min(addr+uintptr(len(buf)), s.recAt+uintptr(len(s.rec))); lo < hi {
		copy(buf[lo-addr:hi-addr], s.rec[lo-s.recAt:])
	}
	return len(buf), nil
}

// 256 MiB 分成 64 个 4 MiB 的区域（各一块），每次扫描认出末尾那条当前歌曲的记录。计时之前先断言：1、64、4096 个区域的分配
// 次数相同（缓冲区只分配一次，不随区域数增长），三种分法都认出同一条、扫描完整。分配次数取 3 次的平均（取整）：单次计数偶尔
// 多出 1 次与扫描无关的分配（实测只跑 1 次时偶见 7 对 6，计时循环里也偶见 7 allocs/op），取整能吸收至多 2 次。
// 变异自证（实测）：缓冲区改为每个区域分配一次而红。
func BenchmarkScanRecord256MiB(b *testing.B) {
	needle, ok := assoc.RecordNeedle(fakeAlbum, fakeSinger, genName)
	if !ok {
		b.Fatal("拼不出前缀")
	}
	scan := func(s *genSpace) (assoc.RecordConclusion, recordScanStats) {
		sc, st, aborted := scanRecord(s.source(), needle, recordScanLimits, never)
		if aborted || !sc.Complete || sc.Malformed != 0 {
			b.Fatalf("aborted %v、Complete %v、Malformed %d，want 扫完、完整、截取都合格", aborted, sc.Complete, sc.Malformed)
		}
		return assoc.ConcludeRecord(sc, 0, genMid), st
	}
	var allocs []float64
	for _, regions := range []int{1, 64, 4096} {
		s := newGenSpace(regions)
		c, st := scan(s)
		if c.Verdict != assoc.RecordFound || c.URL != midURL(genMid) || c.Qualified != 1 || st.read < genSize {
			b.Fatalf("%d 个区域：结论 %+v、读取 %d 字节，want 认出 %s、合格 1 处、读满 256 MiB", regions, c, st.read, midURL(genMid))
		}
		allocs = append(allocs, testing.AllocsPerRun(3, func() { scan(s) }))
	}
	if allocs[0] != allocs[1] || allocs[0] != allocs[2] {
		b.Fatalf("1、64、4096 个区域各分配 %v 次，want 相同", allocs)
	}
	s := newGenSpace(64)
	b.ReportAllocs()
	b.SetBytes(genSize)
	for b.Loop() {
		if c, _ := scan(s); c.Verdict != assoc.RecordFound {
			b.Fatalf("结论 %+v，want 认出", c)
		}
	}
}
