package lyric

// 歌曲总时长的另一来源：KSongsAVCapture.dll 里的 CKSongsGraph 对象（#139，设计见
// doc/design/wesing-songsgraph-duration-design.md）。
//
//	+0x524  uint32  整首的 PCM 字节数
//	+0x528  uint32  已解码的 PCM 字节数（解码位置）
//
// 两者除以 176400（44.1 kHz、16 bit、双声道）得秒（2026-11-02 真机，全民K歌 2.21.176.1220）。
//
// 界面上的「mm:ss | mm:ss」（FindSongDuration）会留下上一首的残留副本，切歌瞬间可能读到旧时长。
// 这里不在宣告时替换它，而是在播放中确认 CKSongsGraph 的解码位置与现有的播放时间逐秒同步推进之后，
// 以它的总时长为准、纠正宣告时的值，每首至多一次。
//
// 定位：在 KSongsAVCapture.dll 的映像里按 MSVC x86 RTTI 找 CKSongsGraph 的主虚表（每个会话一次），
// 再在可写区搜这个虚表值得到实例（每首一次，不跨歌缓存：已释放的对象首 dword 可能仍是虚表）。

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"syscall"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/wesing/proc"
)

const (
	songsGraphModule = "KSongsAVCapture.dll"
	songsGraphClass  = "CKSongsGraph"
	// x86 的 TypeDescriptor 是 {虚表指针, 保留, 修饰名}，修饰名在 TD + 8。
	songsGraphTypeName = ".?AV" + songsGraphClass + "@@\x00"
	tdNameOff          = 8
	maxImageSize       = 64 << 20
	imageChunk         = 64 << 10
	imagePage          = 4 << 10
	maxGraphInstances  = 8

	graphTotalOff = 0x524
	graphPosOff   = 0x528
	// pcmBytesPerSec 是 44.1 kHz、16 bit、双声道的 PCM 每秒字节数。CKSongsGraph 与 CAudioDecodeForPCM（pcm.go）
	// 的计数都按它换算成秒。
	pcmBytesPerSec = 176400
)

// FindSongsGraphVtable 在 KSongsAVCapture.dll 的映像里定位 CKSongsGraph 的主虚表。
// 模块不在、映像大于 64 MB、RTTI 解析出的主虚表不是恰好一个，都返回错误（视为不支持）。
func FindSongsGraphVtable(handle syscall.Handle, modules []proc.Module) (uint32, error) {
	vtable, err := findPrimaryVtable(handle, modules, songsGraphClass, songsGraphTypeName)
	if err != nil {
		return 0, err
	}
	log.Detail("CKSongsGraph vtable: 0x%08X", vtable)
	return vtable, nil
}

// findPrimaryVtable 在 KSongsAVCapture.dll 的映像里按 RTTI 定位类 class（修饰名 typeName）的主虚表。模块不在、
// 映像大于 64 MB、主虚表不是恰好一个，都返回错误。
func findPrimaryVtable(handle syscall.Handle, modules []proc.Module, class, typeName string) (uint32, error) {
	var mod *proc.Module
	for i := range modules {
		if strings.EqualFold(modules[i].Name, songsGraphModule) {
			mod = &modules[i]
			break
		}
	}
	if mod == nil {
		return 0, i18n.Errorf("KSongsAVCapture.dll not found")
	}
	if mod.Size == 0 || mod.Size > maxImageSize {
		return 0, i18n.Errorf("KSongsAVCapture.dll image size 0x%X is out of range", mod.Size)
	}
	img := readImage(procMemory{handle}, mod.Base, mod.Size)
	vtable, n := parsePrimaryVtables(img, mod.Base, typeName)
	if n != 1 {
		return 0, i18n.Errorf("%s RTTI yields %d primary vtables, expected exactly 1", class, n)
	}
	return vtable, nil
}

// readImage 按 64 KB 分块读模块映像；块读失败则逐页读，读不出的页留零。
func readImage(m memory, base, size uint32) []byte {
	img := make([]byte, size)
	for off := uint32(0); off < size; off += imageChunk {
		n := min(imageChunk, size-off)
		if b, ok := m.bytes(base+off, n); ok {
			copy(img[off:off+n], b)
			continue
		}
		for p := off; p < off+n; p += imagePage {
			pn := min(imagePage, off+n-p)
			if b, ok := m.bytes(base+p, pn); ok {
				copy(img[p:p+pn], b)
			}
		}
	}
	return img
}

// parsePrimaryVtables 在映像 img（装载于 base）里按 MSVC x86 RTTI 找修饰名为 typeName（含结尾的 0）的类的
// 主虚表，返回最后找到的那个与找到的个数；个数不是 1 时调用方视为不支持。
//
// x86 的 CompleteObjectLocator 是 {签名=0, OffsetToTop, CDOffset, pTypeDescriptor, pClassHierarchy}，
// 各字段是绝对地址；虚表的前一项指向 COL（LLVM MicrosoftCXXABI.cpp）。OffsetToTop 为 0 的 COL
// 对应主虚表，不为 0 的是多重继承里的次虚表，不要。64 位的 COL 签名为 1、字段是映像相对的，
// 按这里的规则解析不出，结果是「不支持」而不是读错。
func parsePrimaryVtables(img []byte, base uint32, typeName string) (vtable uint32, n int) {
	name := []byte(typeName)
	for i := 0; ; {
		j := bytes.Index(img[i:], name)
		if j < 0 {
			return vtable, n
		}
		at := i + j
		i = at + 1
		if at < tdNameOff {
			continue // TD 会落在映像之前
		}
		td := base + uint32(at-tdNameOff)
		for _, p := range alignedRefs(img, td) {
			// p 是 COL 的 pTypeDescriptor 字段（COL + 12）：前面依次是签名、OffsetToTop。
			if p < 12 || le32(img, p-12) != 0 || le32(img, p-8) != 0 {
				continue
			}
			col := base + uint32(p-12)
			for _, q := range alignedRefs(img, col) {
				vtable = base + uint32(q) + 4
				n++
			}
		}
	}
}

// alignedRefs 返回 img 里 4 字节对齐、值为 v 的 dword 的偏移。
func alignedRefs(img []byte, v uint32) []int {
	var out []int
	for p := 0; p+4 <= len(img); p += 4 {
		if le32(img, p) == v {
			out = append(out, p)
		}
	}
	return out
}

func le32(b []byte, i int) uint32 { return binary.LittleEndian.Uint32(b[i:]) }

// FindSongsGraphInstances 在可写区搜虚表值，返回至多 8 个 4 字节对齐的候选实例（真机 1 个）。
// 每首歌宣告之后重扫一次，不跨歌沿用。
func FindSongsGraphInstances(handle syscall.Handle, vtable uint32) []uint32 {
	pattern, mask := proc.Uint32ToAOB(vtable)
	hits := proc.AOBScan(handle, pattern, mask, proc.EnumWritableRegions(handle))
	insts := pickGraphInstances(hits)
	log.Detail("CKSongsGraph instances: %d (vtable matches: %d)", len(insts), len(hits))
	return insts
}

// pickGraphInstances 只留 4 字节对齐的命中（对象的首 dword 必然对齐），至多 maxGraphInstances 个。
func pickGraphInstances(hits []uint32) []uint32 {
	var out []uint32
	for _, a := range hits {
		if a%4 != 0 {
			continue
		}
		out = append(out, a)
		if len(out) == maxGraphInstances {
			break
		}
	}
	return out
}

// readGraph 读一个候选实例的总时长与解码位置（秒）。首 dword 不再是虚表、或任一项读不出，ok 为假。
func readGraph(m memory, inst, vtable uint32) (total, pos float64, ok bool) {
	if vt, ok := m.u32(inst); !ok || vt != vtable {
		return 0, 0, false
	}
	tb, ok1 := m.u32(inst + graphTotalOff)
	pb, ok2 := m.u32(inst + graphPosOff)
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return float64(tb) / pcmBytesPerSec, float64(pb) / pcmBytesPerSec, true
}

// 同步窗口的判据（设计 §4.2）。
const (
	syncMaxOffset  = 1.0  // 单拍 |解码位置 − 播放时间| 的上限（秒）
	syncStepMin    = 0.5  // 相邻两拍播放时间的推进下限（秒）
	syncStepMax    = 1.5  // 上限
	syncSpan       = 5.0  // 窗口至少覆盖的播放时间推进（秒）
	syncMinSamples = 6    // 窗口至少的采样数
	syncMaxSamples = 16   // 窗口至多的采样数
	syncMaxSpread  = 0.25 // 窗口内全部采样偏差的极差上限（秒）
	maxSongSeconds = 7200 // 可采用的总时长上限（秒）
)

// syncSample 是同步窗口里的一个采样：t 是播放时间（内存直读），d = 解码位置 − t。
type syncSample struct{ t, d float64 }

// syncWindow 是一个候选实例的采样窗口：一段逐秒连续推进的采样。
type syncWindow struct{ s []syncSample }

// observe 喂入一拍（约每秒一次），返回这一拍是否判为同步。ok 为假表示本拍读不出或首 dword
// 不再是虚表；t 是播放时间的内存直读值，pos、total 是这个候选的解码位置与总时长（秒），
// lastLyric 是末行歌词的起始时间。
//
// 同步要求窗口里有不少于 6 个采样、覆盖至少 5 s 的播放时间推进，相邻采样推进都在 [0.5, 1.5] s，
// 全部采样偏差的极差不超过 0.25 s。只比较两个采样（锚点与当前）挡不住两种错：一个读偏的锚点能
// 抵消采样格式不符带来的漂移，前跳能让两个采样跨满 5 s。取全部采样的极差、并要求逐秒连续，
// 一个读偏的采样只会让极差变大，滑出窗口之后正确的候选照常判为同步（设计 §4.2、§8）。
func (w *syncWindow) observe(ok bool, t, pos, total, lastLyric float64) bool {
	d := pos - t
	if !ok || !(math.Abs(d) <= syncMaxOffset) {
		w.s = w.s[:0]
		return false
	}
	// 暂停、回退、重唱、前跳、轮询被拖住：从本拍重新开始。
	if n := len(w.s); n > 0 {
		if dt := t - w.s[n-1].t; !(dt >= syncStepMin && dt <= syncStepMax) {
			w.s = w.s[:0]
		}
	}
	w.s = append(w.s, syncSample{t, d})
	for len(w.s) > syncMinSamples && w.s[len(w.s)-1].t-w.s[1].t >= syncSpan {
		w.s = w.s[1:]
	}
	if len(w.s) > syncMaxSamples {
		w.s = w.s[len(w.s)-syncMaxSamples:]
	}
	if len(w.s) < syncMinSamples || w.s[len(w.s)-1].t-w.s[0].t < syncSpan {
		return false
	}
	lo, hi := w.s[0].d, w.s[0].d
	for _, s := range w.s[1:] {
		lo, hi = min(lo, s.d), max(hi, s.d)
	}
	return hi-lo <= syncMaxSpread && pos <= total && lastLyric <= total && total <= maxSongSeconds
}

// DurationSource 是宣告时所用总时长的来源，决定 CKSongsGraph 同步之后如何判断它是否不对。
type DurationSource int

const (
	// DurationFromUI 取自界面上的「mm:ss | mm:ss」（FindSongDuration），整秒截断。
	DurationFromUI DurationSource = iota
	// DurationEstimated 是末行歌词起始时间 + 10 s 的估值。
	DurationEstimated
)

// String 返回来源的说明，供日志使用，已本地化。
func (s DurationSource) String() string {
	if s == DurationFromUI {
		return i18n.T("the UI duration string")
	}
	return i18n.T("last lyric line + 10s")
}

// adoptTotal 按设计 §4.3 判断本拍是否采用 CKSongsGraph 的总时长。synced 是本拍判为同步的各候选的
// 总时长，cur 是现用的总时长。
//
// 同步候选不止一个、且总时长相差超过 1 s 时是歧义，不采用。来源是界面字符串时，界面按截断显示
// 整秒，floor(total) 与现值相等即不必纠正——既不会在正常情况下重发，也不放过上一首 180、这首
// 181.4 这类相近的旧值；来源是估值时一律纠正。
func adoptTotal(synced []float64, cur float32, src DurationSource) (float32, bool) {
	if len(synced) == 0 {
		return 0, false
	}
	lo, hi := synced[0], synced[0]
	for _, v := range synced[1:] {
		lo, hi = min(lo, v), max(hi, v)
	}
	if hi-lo > 1 {
		return 0, false
	}
	total := synced[0]
	if src == DurationFromUI && math.Floor(total) == float64(cur) {
		return 0, false
	}
	return float32(total), true
}

// SongsGraphWatch 在一首歌里盯着 CKSongsGraph 的候选实例，判断何时以它的总时长纠正宣告时的值。
// 每首歌宣告之后新建一个；nil 表示本会话 CKSongsGraph 不可用，Check 恒不纠正。
type SongsGraphWatch struct {
	m         memory
	vtable    uint32
	lastLyric float64
	src       DurationSource
	done      bool
	cands     []graphCand
}

type graphCand struct {
	addr uint32
	w    syncWindow
}

// NewSongsGraphWatch 为一首歌建一个 SongsGraphWatch。insts 来自 FindSongsGraphInstances，
// lastLyric 是末行歌词的起始时间，src 是宣告时所用总时长的来源。
func NewSongsGraphWatch(handle syscall.Handle, vtable uint32, insts []uint32, lastLyric float32, src DurationSource) *SongsGraphWatch {
	return newSongsGraphWatch(procMemory{handle}, vtable, insts, lastLyric, src)
}

func newSongsGraphWatch(m memory, vtable uint32, insts []uint32, lastLyric float32, src DurationSource) *SongsGraphWatch {
	g := &SongsGraphWatch{m: m, vtable: vtable, lastLyric: float64(lastLyric), src: src}
	for _, a := range insts {
		g.cands = append(g.cands, graphCand{addr: a})
	}
	return g
}

// Check 约每秒调一次。t 是播放时间的内存直读值（已过 IsPlausiblePlayTime、取在最小化外推之前），
// cur 是现用的总时长。需要纠正时返回新的总时长与 true。调用方把纠正后的 all_lyrics 成功入队之后
// 调 Adopted；入队失败（事件通道满）就不调，下一拍 Check 照常再判一次、再返回 true。Adopted 之后
// 本首恒返回 false，也不再读内存——采用后不撤回（设计 §4.3 末段，用户裁定）。
func (g *SongsGraphWatch) Check(t, cur float32) (float32, bool) {
	if g == nil || g.done {
		return 0, false
	}
	var synced []float64
	for i := range g.cands {
		c := &g.cands[i]
		total, pos, ok := readGraph(g.m, c.addr, g.vtable)
		if c.w.observe(ok, float64(t), pos, total, g.lastLyric) {
			synced = append(synced, total)
		}
	}
	return adoptTotal(synced, cur, g.src)
}

// Adopted 记下本首已按 CKSongsGraph 把总时长从 old 纠正为 d（纠正后的 all_lyrics 已入队），并记一条日志。
func (g *SongsGraphWatch) Adopted(old, d float32) {
	g.done = true
	log.Info("Song duration corrected by CKSongsGraph: %.2fs → %.2fs (previous value from %s)", old, d, g.src)
}
