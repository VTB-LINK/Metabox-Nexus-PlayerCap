package kugou

import (
	"slices"

	"Metabox-Nexus-PlayerCap/player"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// shiftTimeline 把酷狗侧的歌词快慢偏移 lyricOffsetMs（毫秒，正数使歌词提前，与 LRC 的 [offset:] 同义）作用在
// 歌词时间轴上（设计 §6.5）：各行 Time，以及有逐字的 Detailed、RomaDetailed 的行起始与逐字起始，都减去
// lyricOffsetMs/1000 秒并下限钳到 0。就地修改；时长（Duration）与 PlayTime 不动，无逐字的 Detailed / RomaDetailed
// 保持零值。
//
// 它是对歌词时间轴的修正，不是 PlayerCap 的 offset 配置：在 applyDetailedOffset 之前执行，配置 offset 另外叠加。
func shiftTimeline(lines []klyric.Line, lyricOffsetMs int) {
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

// cloneLines 深拷贝 lines：切片本身及每行的 Detailed.Words、RomaDetailed.Words、RomaWords 另行分配，修改副本
// 绝不影响原 rawLines。shiftTimeline 与 applyDetailedOffset 均就地改写，每次显示前都从原始行拷贝再叠加。
func cloneLines(lines []klyric.Line) []klyric.Line {
	out := slices.Clone(lines)
	for i := range out {
		out[i].Detailed.Words = slices.Clone(out[i].Detailed.Words)
		out[i].RomaDetailed.Words = slices.Clone(out[i].RomaDetailed.Words)
		out[i].RomaWords = slices.Clone(out[i].RomaWords)
	}
	return out
}

// buildDisplayLyrics 从未偏移的 raw 生成待发出的显示歌词：先深拷贝、套酷狗侧时间轴偏移 lyricOffsetMs，再叠加
// PlayerCap 的播放器级 offsetSec。raw 为空时返回 nil。
func buildDisplayLyrics(raw []klyric.Line, lyricOffsetMs int, offsetSec float32) []klyric.Line {
	if len(raw) == 0 {
		return nil
	}
	out := cloneLines(raw)
	shiftTimeline(out, lyricOffsetMs)
	applyDetailedOffset(out, offsetSec)
	return out
}
