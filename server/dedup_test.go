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

// TestHashDistinguishesRomaText 钉死 roma_text（音译）进内容哈希：两条只差音译的事件必须
// 哈希不同，否则 switchSkip 会把它们判成「内容相同」而静默跳过后一条——与 sub_text 同理。
// roma_text 与 sub_text 是两条独立轨，哈希里必须一同参与。
//
// 变异自证：把 dedup.go 里 all_lyrics 或 lyric_update 哈希新增的 RomaText 退回去，对应分支即红。
func TestHashDistinguishesRomaText(t *testing.T) {
	// all_lyrics：某行只有 roma_text 不同
	baseAll := &player.AllLyricsData{
		Title: "夜に駆ける", Duration: 260, Count: 1,
		Lyrics: []player.LyricLine{{Index: 0, Timestamp: 12.34, Text: "夜に駆ける", SubText: "向夜晚奔去", RomaText: "yoru ni kakeru"}},
	}
	changedRomaAll := *baseAll
	changedRomaAll.Lyrics = []player.LyricLine{{Index: 0, Timestamp: 12.34, Text: "夜に駆ける", SubText: "向夜晚奔去", RomaText: "changed"}}
	if hashEventData(player.EventAllLyrics, baseAll) == hashEventData(player.EventAllLyrics, &changedRomaAll) {
		t.Error("all_lyrics 哈希没算上 roma_text：只改音译的两条被判成内容相同，会被 switchSkip 静默跳过")
	}

	// lyric_update：只有 roma_text 不同
	baseLU := &player.LyricUpdate{Index: 0, Text: "夜に駆ける", SubText: "向夜晚奔去", RomaText: "yoru ni kakeru"}
	changedRomaLU := *baseLU
	changedRomaLU.RomaText = "changed"
	if hashEventData(player.EventLyricUpdate, baseLU) == hashEventData(player.EventLyricUpdate, &changedRomaLU) {
		t.Error("lyric_update 哈希没算上 roma_text：只改音译的两条被判成内容相同，会被 switchSkip 静默跳过")
	}
}
