package mem

// 本文件只测位置结构的形态判定：parseTwin（读数是否可信）、isTwinCandidate（发现时收不收），以及全扫
// 逐窗找候选的两步——twinDurInRange（按时长字段原始位的预筛）与 appendTwinCandidates（一块内存里收候选）。

import (
	"encoding/binary"
	"math"
	"math/rand"
	"slices"
	"testing"
)

func twinBytes(d, p, p2 float64) []byte {
	b := make([]byte, twinSize)
	binary.LittleEndian.PutUint64(b[0:], math.Float64bits(d))
	binary.LittleEndian.PutUint64(b[8:], math.Float64bits(p))
	binary.LittleEndian.PutUint64(b[16:], math.Float64bits(p2))
	return b
}

// 变异自证：把 parseTwin 的接受式 `p >= 0 && p <= d+1` 反写成拒绝式 `p < 0 || p > d+1`，
// NaN 位置那一例变红（AGENTS §3.7：反写的拒绝式会让 NaN 通过）。
func TestParseTwin(t *testing.T) {
	nan := math.NaN()
	cases := []struct {
		name    string
		b       []byte
		want    Twin
		wantOK  bool
		isCandi bool
	}{
		{"播放中", twinBytes(285, 192, 192), Twin{285, 192}, true, true},
		{"开头", twinBytes(206, 0, 0), Twin{206, 0}, true, true},
		{"加载中归零", twinBytes(0, 0, 0), Twin{}, true, false},
		{"时长为 0 但位置非 0", twinBytes(0, 3, 3), Twin{}, false, false},
		{"两份位置不等", twinBytes(285, 192, 193), Twin{}, false, false},
		{"位置超过时长+1", twinBytes(100, 102, 102), Twin{}, false, false},
		{"位置为负", twinBytes(100, -1, -1), Twin{}, false, false},
		{"NaN 时长", twinBytes(nan, 0, 0), Twin{}, false, false},
		{"NaN 位置", twinBytes(100, nan, nan), Twin{}, false, false},
		{"超过两小时", twinBytes(7201, 1, 1), Twin{}, false, false},
		{"非整数时长：可读不收", twinBytes(282.766, 10, 10), Twin{282.766, 10}, true, false},
		{"太短：可读不收", twinBytes(14, 1, 1), Twin{14, 1}, true, false},
		{"不足 24 字节", twinBytes(285, 1, 1)[:23], Twin{}, false, false},
	}
	for _, c := range cases {
		got, ok := parseTwin(c.b)
		if ok != c.wantOK || got != c.want {
			t.Errorf("%s: parseTwin = (%+v,%v), want (%+v,%v)", c.name, got, ok, c.want, c.wantOK)
		}
		if cand := isTwinCandidate(c.b); cand != c.isCandi {
			t.Errorf("%s: isTwinCandidate = %v, want %v", c.name, cand, c.isCandi)
		}
	}
}

// 预筛只比时长字段的原始位，结果须与浮点比较「minTwinDur ≤ d ≤ maxTwinDur」逐值一致：边界两侧、0 与 -0、
// 负数、NaN（含负的与带载荷的）、±Inf、次正规数、极大值，外加两个边界附近的连续位型与随机位型。
//
// 变异自证（隔离副本，实测）：把 twinDurInRange 的下界 `>=` 改成 `>`，15 一例变红；上界 `<=` 改成 `<`，
// 7200 一例变红；去掉上界（只留下界），7200.0001、7201、1e300、各负数、NaN、±Inf 变红；去掉下界，
// 0、1、14、14.999 与次正规数变红。
func TestTwinDurInRangeMatchesFloatCompare(t *testing.T) {
	inRange := func(d float64) bool { return d >= minTwinDur && d <= maxTwinDur }
	var b [8]byte
	check := func(bits uint64) bool {
		binary.LittleEndian.PutUint64(b[:], bits)
		d := math.Float64frombits(bits)
		if got, want := twinDurInRange(b[:]), inRange(d); got != want {
			t.Errorf("twinDurInRange(%v，位 %#016x) = %v, want %v", d, bits, got, want)
			return false
		}
		return true
	}
	vals := []float64{
		0, math.Copysign(0, -1), 1, 14, 14.999, math.Nextafter(15, 0), 15, math.Nextafter(15, 16), 15.5, 285,
		7199.5, math.Nextafter(7200, 0), 7200, math.Nextafter(7200, 8000), 7200.0001, 7201, 1e300, math.MaxFloat64,
		-1, -15, -285, -7200, -1e300, math.NaN(), math.Inf(1), math.Inf(-1),
		math.SmallestNonzeroFloat64, math.Float64frombits(0x000FFFFFFFFFFFFF), math.Float64frombits(0x0010000000000000), // 次正规数的两端、最小的正规数
	}
	for _, v := range vals {
		check(math.Float64bits(v))
	}
	for _, bits := range []uint64{
		0xFFF8000000000001, 0x7FF0000000000001, 0x7FFFFFFFFFFFFFFF, 0xFFFFFFFFFFFFFFFF, // 负 NaN、带载荷的 NaN、全 1
		1<<63 | math.Float64bits(285), 1<<63 | math.Float64bits(15), // 范围内数值的相反数
	} {
		check(bits)
	}
	// 以下两段是成片的位型，错一个往往错一片，报出第一个即止。
	for _, edge := range []uint64{math.Float64bits(minTwinDur), math.Float64bits(maxTwinDur)} {
		for i := uint64(0); i < 4096; i++ {
			if !check(edge-i) || !check(edge+i) {
				return
			}
		}
	}
	rng := rand.New(rand.NewSource(1))
	in := 0
	for i := 0; i < 2_000_000; i++ {
		bits := rng.Uint64()
		if !check(bits) {
			return
		}
		if inRange(math.Float64frombits(bits)) {
			in++
		}
	}
	if in == 0 {
		t.Fatal("随机位型没有一个落在范围内，对照无效")
	}
}

// 预筛不得漏掉任何候选：isTwinCandidate 为真的窗，twinDurInRange 必为真。构造的窗取遍 0~7300 的整数时长
// （及各自 +0.5 的非整数时长）与合法、非法的位置组合，其中候选的个数是确定的；另加随机窗。
//
// 变异自证（隔离副本，实测）：把 twinDurInRange 的下界 `>=` 改成 `>`，时长 15 的候选被漏掉而红；上界
// `<=` 改成 `<`，时长 7200 的候选被漏掉而红。
func TestTwinPrefilterNeverDropsCandidate(t *testing.T) {
	cands := 0
	check := func(b []byte) {
		if !isTwinCandidate(b) {
			return
		}
		cands++
		if !twinDurInRange(b) {
			t.Fatalf("预筛漏掉了候选 % x", b)
		}
	}
	for d := 0; d <= 7300; d++ {
		fd := float64(d)
		for _, p := range []float64{0, 1, fd / 2, fd, fd + 1, fd + 2, -1} {
			check(twinBytes(fd, p, p))
			check(twinBytes(fd+0.5, p, p))
			check(twinBytes(fd, p, p+1))
		}
	}
	// 整数时长落在 [minTwinDur, maxTwinDur]、两份位置相等且在 [0, 时长+1] 内（上面 7 个位置里的前 5 个）才是候选。
	if want := (maxTwinDur - minTwinDur + 1) * 5; cands != want {
		t.Fatalf("构造的窗里候选 %d 个，want %d（对照无效）", cands, want)
	}
	rng := rand.New(rand.NewSource(2))
	b := make([]byte, twinSize)
	for i := 0; i < 500_000; i++ {
		// 时长一半取 [0, 8000) 的随机整数、一半取随机位型，位置取随机整数且两份相等：全随机的 24 字节
		// 几乎不可能成为候选，起不到对照作用。
		d := math.Float64bits(float64(rng.Intn(8000)))
		if i%2 == 0 {
			d = rng.Uint64()
		}
		p := math.Float64bits(float64(rng.Intn(8000)))
		binary.LittleEndian.PutUint64(b[0:], d)
		binary.LittleEndian.PutUint64(b[8:], p)
		binary.LittleEndian.PutUint64(b[16:], p)
		check(b)
	}
}

// 一块内存里收候选：按 8 字节对齐逐窗，地址 = 基址 + 偏移，升序；不对齐的、被块尾截断的、形态不合的不收。
// 结果另与「不经预筛、只用 isTwinCandidate」的逐窗结果对照。
//
// 变异自证（隔离副本，实测）：
//   - 预筛读错位置（twinDurInRange(data[off+8:])）、预筛结果取反：一个候选都收不到而红。
//   - 步长 8 改成 4：收进没有对齐的那个而红。
//   - 地址不加基址：地址不对而红。
//   - 循环条件 `off+twinSize <= len(data)` 改成 `<`：顶到块尾的那个收不到而红。
//
// 整个去掉预筛（只用 isTwinCandidate）时本文件全绿（实测）：预筛只省时间，不改变结果。
func TestAppendTwinCandidates(t *testing.T) {
	const base = uintptr(0x1F000000000)
	data := make([]byte, 4096)
	put := func(off int, d, p, p2 float64) { copy(data[off:], twinBytes(d, p, p2)) }
	// 位置都取小于 minTwinDur 的值：位置字段自己不会再被相邻的窗当成合法时长。
	put(0, 285, 3, 3)          // 块首
	put(64, 15, 0, 0)          // 时长下界
	put(128, 7200, 7201, 7201) // 时长上界，位置 = 时长 + 1
	put(256, 14, 1, 1)         // 太短
	put(320, 7201, 1, 1)       // 太长
	put(384, 282.766, 10, 10)  // 非整数时长
	put(448, 206, 5, 6)        // 两份位置不等
	put(512, 0, 0, 0)          // 加载中
	put(604, 300, 3, 3)        // 没有按 8 字节对齐
	put(1024, 100, 5, 5)       // 紧挨着的两个
	put(1048, 101, 6, 6)
	put(4096-twinSize, 240, 0, 0) // 正好顶到块尾
	want := []uintptr{base, base + 64, base + 128, base + 1024, base + 1048, base + 4096 - twinSize}

	got, truncated := appendTwinCandidates(nil, base, data)
	if truncated || !slices.Equal(got, want) {
		t.Fatalf("got %#x truncated=%v, want %#x", got, truncated, want)
	}
	var ref []uintptr
	for off := 0; off+twinSize <= len(data); off += 8 {
		if isTwinCandidate(data[off : off+twinSize]) {
			ref = append(ref, base+uintptr(off))
		}
	}
	if !slices.Equal(got, ref) {
		t.Fatalf("与不经预筛的逐窗结果不一致: got %#x, ref %#x", got, ref)
	}

	// 追加到已有结果之后；块尾截掉最后一个字节，顶到块尾的那个不再完整、不收。
	prev := []uintptr{1, 2}
	got, truncated = appendTwinCandidates(prev, base, data[:len(data)-1])
	if truncated || !slices.Equal(got, append([]uintptr{1, 2}, want[:len(want)-1]...)) {
		t.Fatalf("追加: got %#x truncated=%v", got, truncated)
	}

	// 空块、不足一个窗：不收、不越界。
	for n := 0; n < twinSize; n++ {
		if got, truncated := appendTwinCandidates(nil, base, twinBytes(285, 3, 3)[:n]); len(got) != 0 || truncated {
			t.Fatalf("%d 字节的块: got %#x truncated=%v", n, got, truncated)
		}
	}
}

// 候选数上限：收满 maxTwinCandidates 个之后再遇到候选才算截断，已收的不丢、不超收；恰好收满不算截断。
//
// 变异自证（隔离副本，实测）：去掉 appendTwinCandidates 里的上限判断，「超出」一例收到
// maxTwinCandidates+2 个且不报截断而红；把上限判断提前一个（`len(dst)+1 == maxTwinCandidates`），
// 「恰好收满」一例少收一个并报截断而红。
func TestAppendTwinCandidatesCap(t *testing.T) {
	const base = uintptr(0x2000)
	three := slices.Concat(twinBytes(100, 1, 1), twinBytes(100, 1, 1), twinBytes(100, 1, 1))
	almost := make([]uintptr, maxTwinCandidates-1)

	got, truncated := appendTwinCandidates(almost, base, three[:twinSize])
	if truncated || len(got) != maxTwinCandidates || got[maxTwinCandidates-1] != base {
		t.Fatalf("恰好收满: len=%d truncated=%v，want len=%d 且不截断", len(got), truncated, maxTwinCandidates)
	}
	got, truncated = appendTwinCandidates(almost, base, three)
	if !truncated || len(got) != maxTwinCandidates || got[maxTwinCandidates-1] != base {
		t.Fatalf("超出: len=%d truncated=%v，want len=%d 且截断", len(got), truncated, maxTwinCandidates)
	}
	// 一整块里候选多于上限：收前 maxTwinCandidates 个（地址升序）。
	big := make([]byte, 0, (maxTwinCandidates+3)*twinSize)
	for i := 0; i < maxTwinCandidates+3; i++ {
		big = append(big, twinBytes(100, 1, 1)...)
	}
	got, truncated = appendTwinCandidates(nil, base, big)
	if !truncated || len(got) != maxTwinCandidates || got[0] != base || got[maxTwinCandidates-1] != base+uintptr((maxTwinCandidates-1)*twinSize) {
		t.Fatalf("整块超出: len=%d truncated=%v", len(got), truncated)
	}
}
