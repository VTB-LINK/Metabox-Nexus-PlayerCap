package server

import (
	"fmt"
	"hash/fnv"

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
			fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%t", msg.Name, msg.Singer, msg.Title, msg.Cover, msg.CoverBase64 != "")
		}
	case player.EventAllLyrics:
		if msg, ok := data.(*player.AllLyricsData); ok {
			fmt.Fprintf(h, "%s\x00%d\x00%.3f", msg.Title, msg.Count, msg.Duration)
			// 加入首尾歌词文本以区分同名不同内容
			if len(msg.Lyrics) > 0 {
				fmt.Fprintf(h, "\x00%s", msg.Lyrics[0].Text)
				if len(msg.Lyrics) > 1 {
					fmt.Fprintf(h, "\x00%s", msg.Lyrics[len(msg.Lyrics)-1].Text)
				}
			}
		}
	case player.EventLyricUpdate:
		if msg, ok := data.(*player.LyricUpdate); ok {
			fmt.Fprintf(h, "%d\x00%s\x00%.3f", msg.Index, msg.Text, msg.Timestamp)
		}
	default:
		// 未知类型使用 fmt 兜底（不应走到这里，status_update 不参与去重）
		fmt.Fprintf(h, "%v", data)
	}
	return h.Sum64()
}
