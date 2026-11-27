package qqmusic

// 本文件是会话侧的歌词状态机（设计 §6.8「会话的状态」与「应用期望状态」第 1、2 步）：保存本首歌的在线原始行与当前采纳的
// 期望状态，把读取者的每条结果折算成要显示的行，指纹不变时不要求重发。只由会话的轮询线程调用，不加锁。

import (
	"crypto/sha256"
	"fmt"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
)

// localDecideWindow 是换歌认领时等待本首首轮本地文件认定的硬上界（#248，设计 §2.3）：
// 仅当关联表可能存在本地条目且在线返回 0 行时生效，绝不等待后续 1/2/4/8 秒重试日程。
// localDecidePollStep 是未决态期间缩短后的轮询休眠步长，避免长轮询配置（最高 2000ms）拖延未决态突破 1s 硬上界。
const (
	localDecideWindow   = time.Second
	localDecidePollStep = 50 * time.Millisecond
)

// String 返回来源名，供注入日志；在源头经 i18n.T（AGENTS.md §6.4 ⑤）。定义之外的值按无。
func (s lyricSource) String() string {
	switch s {
	case srcOnline:
		return i18n.T("QQ Music online lyrics")
	case srcAssoc:
		return i18n.T("QQ Music association")
	case srcLocal:
		return i18n.T("QQ Music local file association")
	}
	return i18n.T("none")
}

// lyricPrint 是给出的一份歌词的指纹：来源、条目偏移、内容摘要、本地首轮未决态（#248）与无歌词提示原文（#231）。
// 内容摘要在线取在线原始行的，关联与本地文件取结果的 digest，无为零值（偏移也为 0）。配置的 offset 在一个会话内不变，不计入。
type lyricPrint struct {
	src           lyricSource
	lyricOffsetMs int
	content       [32]byte
	undecided     bool
	hint          string
}

// lyricState 是会话侧的歌词状态，换歌时 reset，同一首重开时 reopened。零值即代次（0，0）、什么都没有。
type lyricState struct {
	gen, reopen  uint64      // 当前代次，期望状态的代次与之不完全相同即丢弃
	stopped      bool        // 已收到停用：本会话的终态，此后不再接受结果，reset 也不清
	waitLocal    bool        // 本 gen 是否仍在等待读取者首轮本地文件认定答复（#248）
	waitDeadline time.Time   // waitLocal 的硬上界
	title        string      // 本首认领时的标题，供关联/本地歌词与未补取时的两行「歌名行 + 提示句」识别（#231）；reset 清零
	onlineTitle  string      // 本首补取到的在线歌词元数据标题，仅用于在线双行提示识别、不对外发（#229、#231）；reset 清零
	onlineRaw    []lyricLine // 在线原始行：fetchLRC 返回、未平移、未套 offset 的深拷贝；在线取词失败或还没取时为空
	onlineSum    [32]byte    // onlineRaw 的内容摘要
	want         assocResult // 本 gen 最近接受的期望状态；尚未接受时为零值，按「无条目」处理
	adopted      bool        // 本 gen 是否已给出过行
	shown        lyricPrint  // 最近给出的那份的指纹
	shownSum     [32]byte    // 最近给出的那份折算后（平移、套 offset 之后）的行的摘要
}

// reset 换到 gen、reopen 归零：清空标题、在线原始行、接受的结果与已给出的指纹，停用保留；未停用且 waitLocal 为真时记下等待首轮
// 本地认定的截止时刻 deadline（#248）。此后第一次 apply 或 refresh 必定要求重发。localPath 在此之后为空，换歌时先取它作
// 下一首的 excludeLocal。
func (s *lyricState) reset(gen uint64, waitLocal bool, deadline time.Time) {
	*s = lyricState{
		gen:          gen,
		stopped:      s.stopped,
		waitLocal:    !s.stopped && waitLocal,
		waitDeadline: deadline,
	}
}

// setTitle 记录本首歌认领时的标题，供 pureMusicHint 识别两行「歌名行 + 提示句」（#231）。
func (s *lyricState) setTitle(title string) {
	s.title = title
}

// setOnlineTitle 记录补取到的在线歌词元数据标题（歌词标签回退本拍内存），仅供在线来源的双行无歌词提示识别，
// 不改变对外冻结的宣告标题，也不影响关联/本地歌词按认领标题的判定（#229、#231）。
func (s *lyricState) setOnlineTitle(title string) {
	s.onlineTitle = title
}

// reopened 在同一首重开时换到 reopen（设计 §6.8「重开」）：只换代次，标题、在线原始行、已接受的结果与指纹都保留，新代次的结果
// 到来之前照旧按已接受的结果显示，此前代次的结果此后一律丢弃。
func (s *lyricState) reopened(reopen uint64) {
	s.reopen = reopen
}

// setOnlineRaw 保存在线原始行的深拷贝与内容摘要，不改变接受的结果；来源为在线时，随后的 apply 或 refresh 按新行重发。
// 空行的摘要为零值，与从未保存相同。
func (s *lyricState) setOnlineRaw(lines []lyricLine) {
	s.onlineRaw, s.onlineSum = cloneLines(lines), linesSum(lines)
}

// linesSum 是行的内容摘要，空行为零值。
func linesSum(lines []lyricLine) (sum [32]byte) {
	if len(lines) == 0 {
		return sum
	}
	h := sha256.New()
	for _, l := range lines {
		fmt.Fprintf(h, "%#v\n", l) // %#v 写出全部字段，字符串带引号、浮点取最短的精确写法，内容相同即摘要相同
	}
	copy(sum[:], h.Sum(nil))
	return sum
}

// apply 接受 res 并按 refresh 折算出要显示的行（设计 §6.8「输出」与「应用期望状态」第 1、2 步；#248）。停用不比代次：任何
// 代次的停用都使本会话转为停用并解除 waitLocal，按「无条目」折算。当前代次的 resUnknown 不覆盖 want，但若仍在等待首轮
// 本地认定，置 waitLocal 为假并经 refresh 解除未决态；此前未在等待时仍不动作。已停用之后、代次不符或种类不在定义之内：
// 不接受，返回 nil 与假，状态不变。
func (s *lyricState) apply(res assocResult, songHasIDs bool, offsetSec float32) (lines []lyricLine, changed bool) {
	switch {
	case s.stopped:
		return nil, false
	case res.kind == resStopped:
		s.stopped, s.waitLocal = true, false
	case res.gen != s.gen || res.reopen != s.reopen:
		return nil, false
	case res.kind == resUnknown:
		if !s.waitLocal {
			return nil, false
		}
		s.waitLocal = false
	case res.kind == resDesired:
		s.waitLocal = false
		s.want = res
	default:
		return nil, false
	}
	return s.refresh(songHasIDs, offsetSec)
}

// expireWait 在等待本首首轮本地文件认定到达硬上界 waitDeadline 时解除 waitLocal 并重新折算（#248）。
func (s *lyricState) expireWait(now time.Time, songHasIDs bool, offsetSec float32) ([]lyricLine, bool) {
	if !s.waitLocal || now.Before(s.waitDeadline) {
		return nil, false
	}
	s.waitLocal = false
	return s.refresh(songHasIDs, offsetSec)
}

// undecided 报告当前已给出的在线 0 行是否仍处于等待首轮本地文件认定的未决态（#248）。
func (s *lyricState) undecided() bool {
	return s.adopted && s.shown.undecided
}

// pollSleep 返回本拍轮询休眠时长（#248）：非未决态原样返回 poll（零额外唤醒）；未决态缩短至不超过 localDecidePollStep，
// 距离 waitDeadline 剩余正时长更短时精准对齐该余量，余量已耗尽时仍返回 step 避免极端情况下忙等自旋。
func (s *lyricState) pollSleep(poll time.Duration, now time.Time) time.Duration {
	if !s.undecided() {
		return poll
	}
	step := min(poll, localDecidePollStep)
	if rem := s.waitDeadline.Sub(now); rem > 0 && rem < step {
		return rem
	}
	return step
}

// pureMusicHint 经共用的 player.PureMusicHint 判断 lines 是否只是平台的无歌词提示（#231，设计 §7.4）。
// 仅在 len(lines) 为 1 或 2 时构造轻量切片交给 player.PureMusicHint，避免对几百行正常歌词反复分配临时切片。
func pureMusicHint(lines []lyricLine, title string) (string, bool) {
	if len(lines) != 1 && len(lines) != 2 {
		return "", false
	}
	items := make([]player.LyricLine, len(lines))
	for i, l := range lines {
		items[i] = player.LyricLine{Index: l.Index, Text: l.Text}
	}
	return player.PureMusicHint(items, title)
}

// refresh 按本 gen 接受的结果重新折算要显示的行；已停用或尚未接受时按「无条目」：有编号为在线、偏移 0，编号缺位为无。
// 同 gen 补齐编号后（songHasIDs 为真），无编号阶段记录的 srcNone 归一为在线、偏移 0（#248）。
// 无论在线、在线关联还是本地文件，取得待显示行后统一先经 pureMusicHint 判定（#231，设计 §7.4）：判为平台无歌词提示时
// 清空待显示行并在指纹中保留提示原文（不改动 s.onlineRaw 或 s.want.lines）。紧接其后，来源为在线且行为空、仍在等待
// 本首首轮本地认定时，指纹记 undecided 为真（#248：在线纯音乐提示同样受首轮未决窗保护）；解除未决后指纹变化，空行重发以补发无歌词。
// 指纹与上次给出的相同返回 nil 与假；不同则返回新拷贝的行（先按条目偏移平移时间轴，再套配置的 offset）与真，行为空即无歌词。
// 指纹不同而折算后的行非空且与上次给出的逐字段相同（如在线换成指向自身缓存的关联，#244）：记下新指纹，返回 nil 与假。
// 供换歌拍与补取在 setOnlineRaw 之后、没有本 gen 的新结果时调用：按已接受的结果重发，已采纳的关联与偏移不因此丢失。
func (s *lyricState) refresh(songHasIDs bool, offsetSec float32) (lines []lyricLine, changed bool) {
	p := lyricPrint{src: s.want.source, lyricOffsetMs: s.want.lyricOffsetMs, content: s.want.digest}
	src := s.want.lines
	if s.stopped || s.want.kind != resDesired {
		p = lyricPrint{src: srcNone}
		if songHasIDs {
			p.src = srcOnline
		}
	}
	if songHasIDs && p.src == srcNone {
		p = lyricPrint{src: srcOnline}
	}
	switch p.src {
	case srcOnline:
		p.content, src = s.onlineSum, s.onlineRaw
	case srcAssoc, srcLocal: // 取结果的 digest 与行
	default:
		p, src = lyricPrint{src: srcNone}, nil
	}
	hintTitle := s.title
	if p.src == srcOnline && s.onlineTitle != "" {
		hintTitle = s.onlineTitle
	}
	if hint, ok := pureMusicHint(src, hintTitle); ok {
		src = nil
		p.hint = hint
	}
	if p.src == srcOnline && len(src) == 0 && s.waitLocal {
		p.undecided = true
	}
	if s.adopted && p == s.shown {
		return nil, false
	}
	lines = cloneLines(src) // 平移与 applyDetailedOffset 都就地改写，不能动原始行与结果的行
	shiftTimeline(lines, p.lyricOffsetMs)
	applyDetailedOffset(lines, offsetSec)
	sum := linesSum(lines)
	same := s.adopted && len(lines) > 0 && sum == s.shownSum
	s.adopted, s.shown, s.shownSum = true, p, sum
	if same {
		return nil, false
	}
	return lines, true
}

// hint 返回最近给出的那份歌词若被判定为平台无歌词提示时的提示原文；非提示、有歌词或尚未给出时为空串（#231）。
func (s *lyricState) hint() string {
	if !s.adopted {
		return ""
	}
	return s.shown.hint
}

// source 返回最近给出的那份歌词的来源；reset 之后尚未给出时为无。
func (s *lyricState) source() lyricSource {
	if !s.adopted {
		return srcNone
	}
	return s.shown.src
}

// localPath 返回最近给出的本地文件歌词所采纳的音频文件；来源不是本地文件时为空。
func (s *lyricState) localPath() string {
	if s.source() != srcLocal {
		return ""
	}
	return s.want.localPath
}
