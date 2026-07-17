package server

// 本文件只测 hashEventData 的内容哈希：它决定播放器切换时哪些缓存事件算「内容相同、
// 可跳过重发」。漏算字段会把不同内容判成相同，导致该发的事件被静默跳过。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

func TestAllLyricsHashIncludesDurationAndDetailedLyrics(t *testing.T) {
	base := &player.AllLyricsData{
		Title:    "泪桥",
		Duration: 225.186,
		Lyrics: []player.LyricLine{{
			Index:     0,
			Timestamp: 14.64,
			PlayTime:  14.14,
			Text:      "无心过问你的心里我的吻",
			TextDetailed: player.LyricTextDetailed{
				Timestamp: 14.64,
				PlayTime:  14.14,
				Duration:  6.07,
				Words: []player.LyricTextDetailedWord{{
					Timestamp: 14.64,
					PlayTime:  14.14,
					Duration:  0.42,
					Text:      "无",
				}},
			},
		}},
		Count: 1,
	}
	changedDuration := *base
	changedDuration.Duration = 260
	changedDetailed := *base
	changedDetailed.Lyrics = []player.LyricLine{{Index: 0, Timestamp: 14.64, PlayTime: 14.14, Text: "无心过问你的心里我的吻"}}

	baseHash := hashEventData(player.EventAllLyrics, base)
	if baseHash == hashEventData(player.EventAllLyrics, &changedDuration) {
		t.Fatalf("hash did not change when duration changed")
	}
	if baseHash == hashEventData(player.EventAllLyrics, &changedDetailed) {
		t.Fatalf("hash did not change when text_detailed changed")
	}
}
