package lyric

// 进度与总时长的首选来源：KSongsAVCapture.dll 里的 CAudioDecodeForPCM，即解码伴奏 PCM 文件的对象（#145，
// 设计见 doc/design/wesing-pcm-clock-design.md）。
//
//	+0x00  虚表
//	+0x04  CString  所解码文件的路径，伴奏形如 …\<mid>\<mid>_accompany.pcm
//	+0x0C  uint32   整首的 PCM 字节数
//	+0x10  uint32   已解码的 PCM 字节数
//
// 两者除以 176400（44.1 kHz、16 bit、双声道）得秒，每 50 ms 前进 8820 字节（2026-11-03 真机，全民K歌
// 2.21.176.1220）。可写区里有三个实例：伴奏、原唱（_kongsinger.pcm）与一个路径为空的，伴奏与原唱的计数
// 完全同步。实例地址整场复用，路径随歌更换；切歌时 +0x0C 先清零约 0.4 s 再写入新总长，关窗、无歌时为 0。
//
// 现有的播放时间（timer.go）是同一个计数、晚 0 到 1 拍，CKSongsGraph +0x528 与它恒等。区别在于窗口最小化
// 与拖动时它照常走，现有的播放时间停住、之后跳着追上。
//
// 定位同 CKSongsGraph：模块映像里按 RTTI 找主虚表（会话内一次），可写区里搜虚表值得实例。实例每首重扫、
// 不跨歌缓存：已释放的对象可能保留虚表、路径与合法计数，只复核缓存发现不了新分配的实例，唯一性必须在
// 完整的集合上判定（设计审 F1）。

import (
	"strings"
	"syscall"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player/wesing/proc"
)

const (
	pcmClass    = "CAudioDecodeForPCM"
	pcmTypeName = ".?AV" + pcmClass + "@@\x00"

	pcmPathOff  = 0x04
	pcmTotalOff = 0x0C
	pcmPosOff   = 0x10
	pcmHeadLen  = 0x14 // 每拍读 +0x00..+0x14

	pcmMaxInstances = 8 // 4 字节对齐的命中多于这么多即判为不唯一
	pcmMinPathPtr   = 0x10000
	pcmMaxPathChars = 1024
	pcmAccompSuffix = "_accompany.pcm"
	pcmMaxTotal     = maxSongSeconds * pcmBytesPerSec

	pcmRetries   = 5 // 等待就绪、mid 交叉核对各至多再试几次
	pcmRetryStep = 200 * time.Millisecond

	pcmRateSpan     = 5 * pcmBytesPerSec // 速率核对每段所需的推进量（字节）
	pcmRateSegs     = 2                  // 速率核对测几段
	pcmRateMin      = 0.95
	pcmRateMax      = 1.05
	pcmStallTimeout = 5 * time.Second // 口径同 wesing.go 的 deadAddrTimeout（#44）

	// pcmRateDeadline 速率核对的完成期限（#177）：只累计推进超出范围的拍各自的拍间时长（#190，见 Observe），满了
	// 仍未测满两段即判为不符。runSession 的 Warn 文案写的是 30s，改这里要一起改。
	pcmRateDeadline = 30 * time.Second

	// pcmRateCapPerSec 是每拍推进上限的系数：合格速率的上限 pcmRateMax 换算成每秒的字节数（185220）。常量运算
	// 是精确的，换算不出整数时编译不过。
	pcmRateCapPerSec = int64(pcmRateMax * pcmBytesPerSec)

	// pcmRateSlack 是每拍推进上限的宽余：0.2 s（35280 字节，4 个 50 ms 台阶）。设计见
	// doc/design/wesing-small-seek-design.md §3.3（#190）。
	//   - 下限：上一拍读到的值在它读数之前不到一个台阶内生效，所以合格速率下两次读数之间的推进不超过
	//     1.05 ×（拍间时长 + 上一拍读数之后、取时刻之前被抢占的时长）+ 一个台阶。0.2 s 容得下约 0.14 s 的抢占，
	//     以及粗到 0.15 s 的计数台阶（#145 代码审 F4）。取 0.1 s 时，192 kHz 在 30 ms 一拍下约六成的拍超出，要到期
	//     才判出。
	//   - 漏判（设计审第 1 轮 F1，已接受的残余，设计 §3.5）：跳变那一拍的正常推进可以为 0（30 ms 的拍里不落
	//     50 ms 台阶），漏过的前跳至多「1.05 × 拍间时长 + 0.2 s」，30 ms 一拍约 0.23 s，所在的一段可测得约 1.0545。
	//     收紧宽余会贴近下限，并使比例偏高的伴奏反复被作废、退出变慢（设计 §8 第 2 轮 F3）。
	pcmRateSlack = pcmBytesPerSec / 5
)

// FindPCMVtable 在 KSongsAVCapture.dll 的映像里定位 CAudioDecodeForPCM 的主虚表。模块不在、映像大于 64 MB、
// RTTI 解析出的主虚表不是恰好一个，都返回错误。
func FindPCMVtable(handle syscall.Handle, modules []proc.Module) (uint32, error) {
	vtable, err := findPrimaryVtable(handle, modules, pcmClass, pcmTypeName)
	if err != nil {
		return 0, err
	}
	log.Detail("CAudioDecodeForPCM vtable: 0x%08X", vtable)
	return vtable, nil
}

// SelectPCM 在可写区搜 vtable 的实例并挑出伴奏解码器（含等待就绪），每首宣告之前调一次。本首不用 PCM 时
// 返回错误，说明原因。
func SelectPCM(handle syscall.Handle, vtable uint32) (uint32, error) {
	pattern, mask := proc.Uint32ToAOB(vtable)
	hits := proc.AOBScan(handle, pattern, mask, proc.EnumWritableRegions(handle))
	log.Detail("CAudioDecodeForPCM vtable matches: %d", len(hits))
	inst, res := pickReady(procMemory{handle}, vtable, hits, func() { time.Sleep(pcmRetryStep) })
	switch res {
	case pickOK:
		return inst, nil
	case pickNotUnique:
		return 0, i18n.Errorf("the accompaniment decoder is not unique")
	case pickNotReady:
		return 0, i18n.Errorf("the accompaniment decoder is not ready")
	}
	return 0, i18n.Errorf("no accompaniment decoder found")
}

// ConfirmPCM 在宣告之前核对 SelectPCM 选中的实例（mid 交叉核对），返回本首的时钟。mid 是 getSongMeta 取到
// 的歌曲 mid，为空时跳过核对。
func ConfirmPCM(handle syscall.Handle, vtable, inst uint32, mid string) (*PCMClock, error) {
	return confirmPCM(procMemory{handle}, vtable, inst, mid, func() { time.Sleep(pcmRetryStep) })
}

// pcmHead 是一个实例 +0x00..+0x14 的读数。
type pcmHead struct{ vtable, pathPtr, total, pos uint32 }

func readPCMHead(m memory, inst uint32) (pcmHead, bool) {
	b, ok := m.bytes(inst, pcmHeadLen)
	if !ok || len(b) < pcmHeadLen {
		return pcmHead{}, false
	}
	return pcmHead{le32(b, 0), le32(b, pcmPathOff), le32(b, pcmTotalOff), le32(b, pcmPosOff)}, true
}

// readPCMPath 读实例 +0x04 的 CString（ATL 式，字符数在字符指针前 12 字节）。字符指针不小于 0x10000、字符数
// 在 1～1024 才读；读不出（包括后半截落在不可读的页上）返回 ("", false)。
func readPCMPath(m memory, ptr uint32) (string, bool) {
	s, ok := readCStringIn(m, ptr, pcmMinPathPtr, pcmMaxPathChars)
	return s, ok && s != ""
}

// instState 是一个实例对挑选各条的满足情况。
type instState int

const (
	instNo       instState = iota // 不是伴奏实例，或解码字节超过总字节
	instNotReady                  // 是伴奏实例，只是总字节为 0 或超过上限（切歌时清零的那一下）
	instOK
)

// checkInstance 按挑选的四条（设计 §4.1）检查 inst：首 dword 是虚表；路径读得出、以 _accompany.pcm 结尾
// （不分大小写）；总字节大于 0 且不超过 7200 s；解码字节不超过总字节。返回读数与路径。
func checkInstance(m memory, inst, vtable uint32) (pcmHead, string, instState) {
	h, ok := readPCMHead(m, inst)
	if !ok || h.vtable != vtable {
		return h, "", instNo
	}
	path, ok := readPCMPath(m, h.pathPtr)
	if !ok || len(path) < len(pcmAccompSuffix) || !strings.EqualFold(path[len(path)-len(pcmAccompSuffix):], pcmAccompSuffix) {
		return h, path, instNo
	}
	if h.total == 0 || h.total > pcmMaxTotal {
		return h, path, instNotReady
	}
	if h.pos > h.total {
		return h, path, instNo
	}
	return h, path, instOK
}

// pickResult 是挑选的结论。
type pickResult int

const (
	pickNone      pickResult = iota // 没有合格的伴奏实例
	pickNotReady                    // 没有合格的，但有伴奏实例只差总字节：值得稍后再挑
	pickNotUnique                   // 合格的不止一个，或对齐的命中多于 8 个
	pickOK
)

// pickAccompaniment 在虚表值的命中 hits 里挑伴奏实例：只看 4 字节对齐的命中（对象的首 dword 必然对齐），
// 恰好一个合格时选中。对齐的命中多于 8 个即判为不唯一、不截断：FindSongsGraphInstances 满 8 个就截断，
// 照搬的话第 9 个合法实例会被漏掉，前 8 个里的残留对象就成了「唯一」的（设计审 F1 复审）。
func pickAccompaniment(m memory, vtable uint32, hits []uint32) (uint32, pickResult) {
	var aligned []uint32
	for _, a := range hits {
		if a%4 == 0 {
			aligned = append(aligned, a)
		}
	}
	if len(aligned) > pcmMaxInstances {
		return 0, pickNotUnique
	}
	var inst uint32
	n, notReady := 0, false
	for _, a := range aligned {
		switch _, _, st := checkInstance(m, a, vtable); st {
		case instOK:
			inst = a
			n++
		case instNotReady:
			notReady = true
		}
	}
	switch {
	case n == 1:
		return inst, pickOK
	case n > 1:
		return 0, pickNotUnique
	case notReady:
		return 0, pickNotReady
	}
	return 0, pickNone
}

// pickReady 挑伴奏实例，结论是 pickNotReady 时隔一步（wait）再挑，至多再挑 pcmRetries 次（1 s），覆盖切歌时
// 总字节清零的那一下。其余结论立即返回：没有伴奏实例、或不唯一，再等也不会变，这段等待又卡在宣告之前。
func pickReady(m memory, vtable uint32, hits []uint32, wait func()) (uint32, pickResult) {
	for try := 0; ; try++ {
		inst, res := pickAccompaniment(m, vtable, hits)
		if res != pickNotReady || try >= pcmRetries {
			return inst, res
		}
		wait()
	}
}

// confirmPCM 在宣告之前再查一次选中的实例：仍满足挑选的四条，并且 mid 非空时路径的文件名是
// <mid>_accompany.pcm（不分大小写）。不过关时隔一步（wait）重读，至多 pcmRetries 次：路径稍后才换成这首歌的、
// 中间还有总字节清零的那一下。mid 为空时不核对路径，也不重读。过关那次的读数即宣告用的总时长（初始位置在
// 宣告之前另行重读，#191），那次的路径给出路径 mid（#146）。
func confirmPCM(m memory, vtable, inst uint32, mid string, wait func()) (*PCMClock, error) {
	for try := 0; ; try++ {
		h, path, st := checkInstance(m, inst, vtable)
		if st == instOK && (mid == "" || strings.EqualFold(path[strings.LastIndexAny(path, `\/`)+1:], mid+pcmAccompSuffix)) {
			return &PCMClock{m: m, addr: inst, vtable: vtable, total: h.total, pos: h.pos, mid: pathMID(path)}, nil
		}
		if mid == "" {
			return nil, i18n.Errorf("the accompaniment decoder changed before the announcement")
		}
		if try >= pcmRetries {
			log.Detail("Accompaniment decoder path %q does not match song mid %q", path, mid)
			return nil, i18n.Errorf("the accompaniment decoder path does not match the song mid")
		}
		wait()
	}
}

// PCMClock 是一首歌选中的伴奏解码器，供 pollLyrics 每拍读位置（设计 §4.2、§4.3）。由 ConfirmPCM 创建，
// 每首一个。
type PCMClock struct {
	m      memory
	addr   uint32
	vtable uint32
	total  uint32 // 宣告时的总字节
	pos    uint32 // 最近一次读到的解码字节
	mid    string // 路径 mid（#146），见 pathMID

	// 停滞检测
	start    time.Time // 第一次 Observe 的时刻，即进入轮询
	last     uint32    // 上一次 Observe 时的解码字节
	advanced bool

	// 速率核对：正在测的一段从 (segAt, segPos) 起算；segs 是已测完的段数，rate 是其中较大的速率
	rateDone bool
	segOn    bool
	segAt    time.Time
	segPos   uint32
	segs     int
	rate     float64

	// 完成期限（#177、#190）：prevAt 是上一次 Observe 的时刻；voided 是推进超出范围的拍累计的拍间时长；timedOut
	// 为真表示速率核对是到期判为不符的，不是测出来的。voidNoted 记本首是否已记过作废的 Detail。
	prevAt    time.Time
	voided    time.Duration
	timedOut  bool
	voidNoted bool
}

// Addr 是实例的地址。
func (c *PCMClock) Addr() uint32 { return c.addr }

// MID 是核对通过那次的路径里的歌曲 mid（#146，设计 §3.1），路径不合形状时为空串。PCM 模式的歌曲信息与封面
// 都按它查。
func (c *PCMClock) MID() string { return c.mid }

// pathMID 取伴奏路径 …\<mid>\<mid>_accompany.pcm 里的歌曲 mid（设计 §3.1）：目录名与文件名去掉后缀的部分须
// 相同，且是 14 位字母数字，否则返回空串。后缀不分大小写，同 checkInstance。
func pathMID(path string) string {
	i := strings.LastIndexAny(path, `\/`)
	file := path[i+1:]
	if len(file) < len(pcmAccompSuffix) || !strings.EqualFold(file[len(file)-len(pcmAccompSuffix):], pcmAccompSuffix) {
		return ""
	}
	mid := file[:len(file)-len(pcmAccompSuffix)]
	if i < 0 || !validMID(mid) {
		return ""
	}
	dir := path[:i]
	if dir[strings.LastIndexAny(dir, `\/`)+1:] != mid {
		return ""
	}
	return mid
}

// Total 是宣告用的总时长（秒）。
func (c *PCMClock) Total() float32 { return float32(float64(c.total) / pcmBytesPerSec) }

// Position 是最近一次读到的位置（秒），创建时是核对那次的。由整数换算，不会是 NaN 或负数。
func (c *PCMClock) Position() float32 { return float32(float64(c.pos) / pcmBytesPerSec) }

// Rate 是速率核对已测完的各段中较大的 Δpos / Δ墙钟，一段都没测完时为 0。
func (c *PCMClock) Rate() float64 { return c.rate }

// RateTimedOut 报告速率核对是否因完成期限到期而判为不符（#177，设计 §4.9）：此时 Rate 不是判定的依据。
func (c *PCMClock) RateTimedOut() bool { return c.timedOut }

// PCMRead 是每拍读数的结论。
type PCMRead int

const (
	PCMOK PCMRead = iota
	// PCMBad：读不出、首 dword 不再是虚表、或解码字节超过总字节。记为读失败，口径同 pollLyrics 的 failCount：
	// 连续 3 s 即按换歌重来。
	PCMBad
	// PCMSongChanged：总字节不等于宣告时的值，换歌了，立即重来。
	PCMSongChanged
)

// Read 每拍调一次：读 +0x00..+0x14（20 字节）并逐拍核对，返回位置（秒）。读失败时位置为 0，Position 仍是
// 上一次读到的。
func (c *PCMClock) Read() (float32, PCMRead) {
	h, ok := readPCMHead(c.m, c.addr)
	if !ok || h.vtable != c.vtable {
		return 0, PCMBad
	}
	if h.total != c.total {
		return 0, PCMSongChanged
	}
	if h.pos > h.total {
		return 0, PCMBad
	}
	c.pos = h.pos
	return c.Position(), PCMOK
}

// PCMVerdict 是速率核对与停滞检测的结论。
type PCMVerdict int

const (
	PCMKeep PCMVerdict = iota
	// PCMRateOff：推进速率不在 [0.95, 1.05]，采样格式不是 44.1 kHz、16 bit、双声道，换算比例不成立；或速率
	// 核对到期未能测满（RateTimedOut）。这一首改走兜底，下一首仍先试 PCM（#196，见 wesing.go 的 pcmSkip）。
	PCMRateOff
	// PCMStalled：进入轮询后 5 s 内一次都没推进。
	PCMStalled
)

// Observe 在每拍读数成功、本拍的暂停判定之后调用：now 是本拍的时刻，paused 是本拍判定的暂停状态，位置取本拍
// Read 读到的。
//
// 停滞检测：唯一的实例也可能是停住的残留对象，速率核对要等推进满 5 s 才判，拦不住它（设计审 F1）。开歌即
// 暂停的歌会因此本首走兜底，结果与现状相同。「推进」是比上一拍大：重唱归零之后再走也算。
//
// 速率核对：按段测，一段是推进中、没有判为暂停的区间，推进满 5 s 时算一次 Δpos / Δ墙钟；测完一段紧接着
// 从这一拍起测下一段。测满两段后取较大的一个判定，每首只判一次（设计 §4.2，代码审 F1、F4、F5）。不足 1 s
// 的停顿判不成暂停，落在一段里只会拉低这一段的速率，所以较大的一个更接近真实：一段干净即可判对；不按
// 停顿作废区间，否则轮询间隔大时看不见停顿、计数台阶粗时区间永远凑不满。判为暂停时只作废正在测的那一段。
// 读数不高于段起点时从本拍重新起算：停着的拍由此顺延起点，开播前、重唱归零之后停着的那一会儿不算进去。
//
// 每拍上限（#190，设计 doc/design/wesing-small-seek-design.md §3.3）：从第二次调用起，本拍推进（比上一拍的
// 读数，可为负）为负、或超过「1.05 × 拍间时长 + 0.2 s」即超出范围，作废正在测的那一段：跳转与回退不论大小、
// 方向都不算进速率段，也不必经跳转判定（门槛 2 s）来报。上限是合格速率在本拍间隔内可能给出的最大推进加宽余，
// 比例正确时没有跳变的拍不会超出（见 pcmRateSlack）。上限以内的跳变照常计入，是已接受的残余（设计 §3.5）。
//
// 完成期限（#177）：只累计超出范围的拍各自的拍间时长，判为暂停的拍、正常推进的拍不累计也不清零；满
// pcmRateDeadline 仍未测满两段即判为不符。比例不对的伴奏每拍都超出范围时，段每拍作废，没有期限就永远测不满；
// 正常暂停不累计，反复「播几秒、停几秒」不会到期。期限累计的是拍间时长，不是墙钟：超出范围的拍稀疏而段仍测
// 不满时，判出可慢到数百秒（设计 §3.5 残余 2）。
func (c *PCMClock) Observe(now time.Time, paused bool) PCMVerdict {
	pos := c.pos
	if c.start.IsZero() {
		c.start, c.last = now, pos
	}
	adv := int64(pos) - int64(c.last) // 本拍推进（字节）
	if pos > c.last {
		c.advanced = true
	}
	c.last = pos
	prev := c.prevAt
	c.prevAt = now
	if !c.advanced && now.Sub(c.start) > pcmStallTimeout {
		return PCMStalled
	}

	if c.rateDone {
		return PCMKeep
	}
	if dt := now.Sub(prev); !prev.IsZero() && advanceOutOfRange(adv, dt) {
		if !c.voidNoted {
			c.voidNoted = true
			log.Detail("PCM rate segment voided: advanced %.2fs over %.2fs", float64(adv)/pcmBytesPerSec, dt.Seconds())
		}
		c.voided += dt
		if c.voided >= pcmRateDeadline {
			c.rateDone, c.timedOut = true, true
			return PCMRateOff
		}
		c.segOn = false
		return PCMKeep
	}
	if paused {
		c.segOn = false
		return PCMKeep
	}
	if !c.segOn || pos <= c.segPos {
		c.segOn, c.segAt, c.segPos = true, now, pos
		return PCMKeep
	}
	if pos-c.segPos < pcmRateSpan {
		return PCMKeep
	}
	span := now.Sub(c.segAt).Seconds()
	rate := float64(pos-c.segPos) / pcmBytesPerSec / span
	log.Detail("PCM decoder rate: %.3f over %.2fs", rate, span)
	c.rate = max(c.rate, rate)
	c.segs++
	c.segAt, c.segPos = now, pos
	if c.segs < pcmRateSegs {
		return PCMKeep
	}
	c.rateDone = true
	// 接受式：span 为 0 时 rate 是 +Inf，同样不收。
	if !(c.rate >= pcmRateMin && c.rate <= pcmRateMax) {
		return PCMRateOff
	}
	return PCMKeep
}

// advanceOutOfRange 报告一拍推进 adv 字节在拍间时长 dt 内是否超出范围（#190，见 Observe）：为负，或超过
// pcmRateCapPerSec × dt + pcmRateSlack。整数运算、按整秒与零头分开乘，不溢出也没有浮点误差：adv 是整数，
// adv 大于上限的下取整，即大于上限本身。dt 不为正时上限只剩宽余。
func advanceOutOfRange(adv int64, dt time.Duration) bool {
	if adv < 0 {
		return true
	}
	limit := int64(pcmRateSlack)
	if dt > 0 {
		sec, frac := int64(dt/time.Second), int64(dt%time.Second)
		limit += sec*pcmRateCapPerSec + frac*pcmRateCapPerSec/int64(time.Second)
	}
	return adv > limit
}
