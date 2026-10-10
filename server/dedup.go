package server

import (
	"fmt"
	"hash/fnv"
	"io"

	"Metabox-Nexus-PlayerCap/player"
)

// hashEventData 根据事件类型计算数据内容的 FNV-1a 哈希
// 用于 switchSkip 的内容比对去重，替代原有的时间窗口抑制
func hashEventData(evtType string, data interface{}) uint64 {
	h := fnv.New64a()
	switch evtType {
	case player.EventSongInfoUpdate:
		if msg, ok := data.(*player.SongInfo); ok {
			// 包含 CoverBase64 存在标记：异步封面补发含 base64，哈希不同，不会被 switchSkip 抑制
			fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%t\x00", msg.Name, msg.Singer, msg.Title, msg.Cover, msg.CoverBase64 != "")
			// CoverBase64 的**内容**也要进哈希，只看有无不够：applemusic 的封面直接来自进程内存、
			// 没有 URL（Cover 恒空），同一首歌先带着上一首的封面发出、封面换好后再补发一条时，
			// 两条的 name/singer/title/cover 与「有无 base64」全都相同——只比这些会把补发的那条
			// 判成重复而吞掉，根订阅者整首停在上一首的封面上。
			h.Write([]byte(msg.CoverBase64))
		}
	case player.EventAllLyrics:
		if msg, ok := data.(*player.AllLyricsData); ok {
			fmt.Fprintf(h, "%s\x00%d\x00%.3f", msg.Title, msg.Count, msg.Duration)
			for _, line := range msg.Lyrics {
				fmt.Fprintf(h, "\x00%d\x00%.3f\x00%.3f\x00%s\x00%s\x00%s\x00%.3f\x00%d",
					line.Index, line.Timestamp, line.PlayTime, line.Text, line.SubText, line.RomaText,
					line.TextDetailed.Duration, len(line.TextDetailed.Words))
				for _, word := range line.TextDetailed.Words {
					fmt.Fprintf(h, "\x00%.3f\x00%.3f\x00%.3f\x00%s", word.Timestamp, word.PlayTime, word.Duration, word.Text)
				}
				hashRomaDetailed(h, line.RomaTextDetailed)
			}
		}
	case player.EventLyricUpdate:
		if msg, ok := data.(*player.LyricUpdate); ok {
			fmt.Fprintf(h, "%d\x00%s\x00%s\x00%s\x00%.3f\x00%.3f\x00%.3f\x00%d",
				msg.Index, msg.Text, msg.SubText, msg.RomaText, msg.Timestamp, msg.PlayTime,
				msg.TextDetailed.Duration, len(msg.TextDetailed.Words))
			for _, word := range msg.TextDetailed.Words {
				fmt.Fprintf(h, "\x00%.3f\x00%.3f\x00%.3f\x00%s", word.Timestamp, word.PlayTime, word.Duration, word.Text)
			}
			hashRomaDetailed(h, msg.RomaTextDetailed)
		}
	default:
		// 未知类型使用 fmt 兜底（不应走到这里，status_update 不参与去重）
		fmt.Fprintf(h, "%v", data)
	}
	return h.Sum64()
}

// hashRomaDetailed 把逐字音译（roma_text_detailed）写进哈希。与 roma_text 同理：只有逐字音译不同的
// 两条事件内容并不相同，不能被 switchSkip 当成重复。
func hashRomaDetailed(h io.Writer, d player.LyricTextDetailed) {
	fmt.Fprintf(h, "\x00%.3f\x00%d", d.Duration, len(d.Words))
	for _, word := range d.Words {
		fmt.Fprintf(h, "\x00%.3f\x00%.3f\x00%.3f\x00%s", word.Timestamp, word.PlayTime, word.Duration, word.Text)
	}
}
