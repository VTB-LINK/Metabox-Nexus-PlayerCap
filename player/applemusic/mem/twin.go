package mem

// 本文件是纯函数：播放位置结构（twin）的形态判定，以及在一块内存里按形态收候选。
//
// 位置结构 = 连续三个 float64：[时长][位置][位置]，位置在 +8 与 +16 各存一份。2026-10-05 与
// 2026-10-08 两次真机实测：
//   - 时长是真实音频时长、恒为整数秒，与 SMTC 的 EndTime 一致（285、206、347、264…）；TTML 的
//     <body dur> 比它长 0.03~3.77s，不能拿来定位。
//   - 位置以整秒推进，约 1Hz 原地刷新；暂停即停住。
//   - 同一 Apple Music 进程内地址不变，跨歌复用；每次换歌时长与位置先归零（加载中），0.5~1s 后
//     写入新歌时长。Apple Music 重启后地址变，须重新发现。
//   - 形态相同的静止结构很多（本机约 530 个，含 [时长][0][0]），只凭形态分不出哪个是真的；
//     真的那个会走，发现必须看活性（见 applemusic 包的候选观察）。

import (
	"encoding/binary"
	"math"
)

const (
	twinSize   = 24
	minTwinDur = 15   // 发现时只收 15s 以上的时长，挡掉大量小整数巧合
	maxTwinDur = 7200 // 两小时
)

// Twin 是位置结构的一次读数（秒）。Dur==0 且 Pos==0 是换歌加载中的合法态。
type Twin struct {
	Dur float64
	Pos float64
}

// parseTwin 校验 24 字节是否为位置结构并取值：两份位置逐位相等、数值有限；Dur==0 时 Pos 也须为 0
// （加载中）；否则 0 ≤ Pos ≤ Dur+1。合法性判定一律写成接受式，NaN 不得通过（AGENTS §3.7）。
func parseTwin(b []byte) (Twin, bool) {
	if len(b) < twinSize {
		return Twin{}, false
	}
	d := math.Float64frombits(binary.LittleEndian.Uint64(b[0:]))
	pBits := binary.LittleEndian.Uint64(b[8:])
	if pBits != binary.LittleEndian.Uint64(b[16:]) {
		return Twin{}, false
	}
	p := math.Float64frombits(pBits)
	if !(d >= 0 && d <= maxTwinDur) {
		return Twin{}, false
	}
	if d == 0 {
		return Twin{}, p == 0
	}
	if !(p >= 0 && p <= d+1) {
		return Twin{}, false
	}
	return Twin{Dur: d, Pos: p}, true
}

// isTwinCandidate 判定发现扫描时值得观察的形态：在 parseTwin 之上要求时长为 [15,7200] 的整数秒。
// 整数约束是实测得来（见文件头），把本机候选从约 4360 个压到约 530 个（2026-10-08 实测）。
func isTwinCandidate(b []byte) bool {
	t, ok := parseTwin(b)
	return ok && t.Dur >= minTwinDur && t.Dur == math.Trunc(t.Dur)
}

// 时长上下界的 float64 原始位，twinDurInRange 用。
var (
	minTwinDurBits = math.Float64bits(minTwinDur)
	maxTwinDurBits = math.Float64bits(maxTwinDur)
)

// twinDurInRange 是 isTwinCandidate 之前的预筛：b 开头 8 字节（时长字段）的原始位是否落在
// [minTwinDur, maxTwinDur] 对应的位区间内。它与浮点比较「minTwinDur ≤ 时长 ≤ maxTwinDur」等价：正的
// float64 位序即数值序；负数（符号位为 1）、NaN、+Inf 的位都大于 maxTwinDur 的位，0 与次正规数的位都
// 小于 minTwinDur 的位。isTwinCandidate 接受的时长都在这个范围内，所以预筛不会漏掉候选。
//
// 为什么要它：全扫对每 8 字节一个窗逐个判定（2026-10-08 本机可读区 476MB，即约 6000 万个窗），绝大多数窗
// 在这一次整数比较上就被挡掉，不必走 parseTwin。2026-10-09 真机复测（可读区 436MB）：逐窗找候选连同读内存，
// 不带预筛 0.31~0.39s，带预筛 0.14~0.17s，候选数相同。b 须不短于 8 字节。
func twinDurInRange(b []byte) bool {
	bits := binary.LittleEndian.Uint64(b)
	return bits >= minTwinDurBits && bits <= maxTwinDurBits
}

// appendTwinCandidates 把 data（基址为 base 的一块内存）里的位置候选地址追加到 dst：按 8 字节对齐逐窗，
// 先过 twinDurInRange 再判 isTwinCandidate，地址升序。dst 已有 maxTwinCandidates 个而又遇到候选时停下，
// truncated=true。
func appendTwinCandidates(dst []uintptr, base uintptr, data []byte) (_ []uintptr, truncated bool) {
	for off := 0; off+twinSize <= len(data); off += 8 {
		if !twinDurInRange(data[off:]) || !isTwinCandidate(data[off:off+twinSize]) {
			continue
		}
		if len(dst) == maxTwinCandidates {
			return dst, true
		}
		dst = append(dst, base+uintptr(off))
	}
	return dst, false
}
