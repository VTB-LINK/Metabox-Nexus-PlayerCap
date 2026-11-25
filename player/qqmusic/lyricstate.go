package qqmusic

// 本文件是会话侧的歌词状态机（设计 §6.8「会话的状态」与「应用期望状态」第 1、2 步）：保存本首歌的在线原始行与当前采纳的
// 期望状态，把读取者的每条结果折算成要显示的行，指纹不变时不要求重发。只由会话的轮询线程调用，不加锁。

import (
	"crypto/sha256"
	"fmt"

	"Metabox-Nexus-PlayerCap/i18n"
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

// lyricPrint 是给出的一份歌词的指纹：来源、条目偏移与内容摘要。内容摘要在线取在线原始行的，关联与本地文件取结果的
// digest，无为零值（偏移也为 0）。配置的 offset 在一个会话内不变，不计入。
type lyricPrint struct {
	src           lyricSource
	lyricOffsetMs int
	content       [32]byte
}

// lyricState 是会话侧的歌词状态，换歌与 gen 变化时 reset，同一首重开时 reopened。零值即代次（0，0）、什么都没有。
type lyricState struct {
	gen, reopen uint64      // 当前代次，期望状态的代次与之不完全相同即丢弃
	stopped     bool        // 已收到停用：本会话的终态，此后不再接受结果，reset 也不清
	onlineRaw   []lyricLine // 在线原始行：fetchLRC 返回、未平移、未套 offset 的深拷贝；在线取词失败或还没取时为空
	onlineSum   [32]byte    // onlineRaw 的内容摘要
	want        assocResult // 本 gen 最近接受的期望状态；尚未接受时为零值，按「无条目」处理
	adopted     bool        // 本 gen 是否已给出过行
	shown       lyricPrint  // 最近给出的那份的指纹
	shownSum    [32]byte    // 最近给出的那份折算后（平移、套 offset 之后）的行的摘要
}

// reset 换到 gen、reopen 归零：清空在线原始行、接受的结果与已给出的指纹，停用保留。此后第一次 apply 或 refresh 必定要求重发。
// localPath 在此之后为空，换歌时先取它作下一首的 excludeLocal。
func (s *lyricState) reset(gen uint64) {
	*s = lyricState{gen: gen, stopped: s.stopped}
}

// reopened 在同一首重开时换到 reopen（设计 §6.8「重开」）：只换代次，在线原始行、已接受的结果与指纹都保留，新代次的结果
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

// apply 接受 res 并按 refresh 折算出要显示的行（设计 §6.8「输出」与「应用期望状态」第 1、2 步）。停用不比代次：任何代次的
// 停用都使本会话转为停用，按「无条目」折算。已停用之后、期望状态的代次（gen 与 reopen）与当前不完全相同、种类为未知或不在
// 定义之内：不接受，返回 nil 与假，状态不变。
func (s *lyricState) apply(res assocResult, songHasIDs bool, offsetSec float32) (lines []lyricLine, changed bool) {
	switch {
	case s.stopped:
		return nil, false
	case res.kind == resStopped:
		s.stopped = true
	case res.kind != resDesired || res.gen != s.gen || res.reopen != s.reopen:
		return nil, false
	default:
		s.want = res
	}
	return s.refresh(songHasIDs, offsetSec)
}

// refresh 按本 gen 接受的结果重新折算要显示的行；已停用或尚未接受时按「无条目」：有编号为在线、偏移 0，编号缺位为无。
// 来源为在线时行取在线原始行（结果不带行），关联与本地文件取结果所带的行，无为空。指纹与上次给出的相同返回 nil 与假；
// 不同则返回新拷贝的行（先按条目偏移平移时间轴，再套配置的 offset）与真，行为空即无歌词。指纹不同而折算后的行非空且与
// 上次给出的逐字段相同（如在线换成指向自身缓存的关联，#244）：记下新指纹，返回 nil 与假。
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
	switch p.src {
	case srcOnline:
		p.content, src = s.onlineSum, s.onlineRaw
	case srcAssoc, srcLocal: // 取结果的 digest 与行
	default:
		p, src = lyricPrint{src: srcNone}, nil
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
