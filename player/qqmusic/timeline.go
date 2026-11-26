package qqmusic

import (
	"slices"

	"Metabox-Nexus-PlayerCap/player"
)

// shiftTimeline 把 QQ 关联条目的 LyricOffset（毫秒，正数使歌词提前，与 LRC 的 [offset:] 同义）作用在歌词时间轴上
// （设计 §6.7）：各行 Time，以及 Detailed、RomaDetailed 的行起始与逐字起始，都减去 lyricOffsetMs/1000 秒，下限 0。
// 就地修改；时长与 PlayTime 不动，没有逐字的 Detailed、RomaDetailed 保持零值。
//
// 它是对歌词时间轴的修正，不是 PlayerCap 的 offset 配置：在 applyDetailedOffset 之前做，配置的 offset 另外叠加。
func shiftTimeline(lines []lyricLine, lyricOffsetMs int) {
	if lyricOffsetMs == 0 {
		return
	}
	shift := float32(lyricOffsetMs) / 1000
	at := func(t float32) float32 { return max(t-shift, 0) }
	for i := range lines {
		lines[i].Time = at(lines[i].Time)
		for _, d := range []*player.LyricTextDetailed{&lines[i].Detailed, &lines[i].RomaDetailed} {
			if len(d.Words) == 0 {
				continue
			}
			d.Timestamp = at(d.Timestamp)
			for j := range d.Words {
				d.Words[j].Timestamp = at(d.Words[j].Timestamp)
			}
		}
	}
}

// cloneLines 深拷贝 lines：逐字切片 Detailed.Words、RomaDetailed.Words 与振假名切片 Furigana 另行分配，改副本不影响原件。
// shiftTimeline 与 applyDetailedOffset 都就地改写，每次应用都从原始行拷贝一份再套。
func cloneLines(lines []lyricLine) []lyricLine {
	out := slices.Clone(lines)
	for i := range out {
		out[i].Detailed.Words = slices.Clone(out[i].Detailed.Words)
		out[i].RomaDetailed.Words = slices.Clone(out[i].RomaDetailed.Words)
		out[i].Furigana = slices.Clone(out[i].Furigana)
	}
	return out
}
