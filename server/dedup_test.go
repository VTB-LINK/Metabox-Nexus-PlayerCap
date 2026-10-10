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

// TestSongInfoHashIncludesCoverContent 钉死封面 base64 的**内容**进哈希：同一首歌、都带 base64、
// 只是图片不同的两条 song_info_update 必须哈希不同。applemusic 的封面来自进程内存、没有 URL，
// 换歌瞬间可能先带着上一首的封面发出、封面换好后再补发；只比「有无 base64」会把补发的那条
// 判成重复，根订阅者整首停在上一首的封面上。
//
// 变异自证：删掉 dedup.go 里把 CoverBase64 写进哈希的那一行，「只有图片不同」一例变红。
func TestSongInfoHashIncludesCoverContent(t *testing.T) {
	base := &player.SongInfo{Name: "演义", Singer: "唐朝乐队", Title: "唐朝乐队 - 演义", CoverBase64: "data:image/jpeg;base64,AAAA"}
	otherCover := *base
	otherCover.CoverBase64 = "data:image/jpeg;base64,BBBB"
	noCover := *base
	noCover.CoverBase64 = ""
	same := *base

	h := hashEventData(player.EventSongInfoUpdate, base)
	if h == hashEventData(player.EventSongInfoUpdate, &otherCover) {
		t.Error("只有图片不同的两条 song_info_update 哈希相同：补发的封面会被 switchSkip 吞掉")
	}
	if h == hashEventData(player.EventSongInfoUpdate, &noCover) {
		t.Error("有无 base64 的两条哈希相同")
	}
	if h != hashEventData(player.EventSongInfoUpdate, &same) {
		t.Error("内容完全相同的两条哈希不同：切换后的重复事件抑制不了")
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

// TestHashDistinguishesRomaTextDetailed 逐字音译（roma_text_detailed）进内容哈希：只差它的两条事件
// 内容并不相同，不能被 switchSkip 当成重复跳过。
//
// 变异自证：把 dedup.go 里 all_lyrics 分支的 hashRomaDetailed 调用删掉，第一段红；删掉 lyric_update
// 分支的那一处，第二段红。
func TestHashDistinguishesRomaTextDetailed(t *testing.T) {
	roma := func(second string) player.LyricTextDetailed {
		return player.LyricTextDetailed{Timestamp: 13.711, Duration: 0.69, Words: []player.LyricTextDetailedWord{
			{Timestamp: 13.711, Duration: 0.2, Text: "hi "},
			{Timestamp: 13.911, Duration: 0.49, Text: second},
		}}
	}
	all := func(d player.LyricTextDetailed) *player.AllLyricsData {
		return &player.AllLyricsData{Title: "x", Duration: 200, Count: 1,
			Lyrics: []player.LyricLine{{Index: 0, Timestamp: 13.711, Text: "ひか", RomaText: "hi ka", RomaTextDetailed: d}}}
	}
	if hashEventData(player.EventAllLyrics, all(roma("ka "))) == hashEventData(player.EventAllLyrics, all(roma("ga "))) {
		t.Error("all_lyrics 哈希没算上 roma_text_detailed：只改逐字音译的两条被判成内容相同")
	}
	if hashEventData(player.EventAllLyrics, all(roma("ka "))) == hashEventData(player.EventAllLyrics, all(player.LyricTextDetailed{})) {
		t.Error("all_lyrics 哈希没算上 roma_text_detailed：有逐字音译与没有的两条被判成内容相同")
	}

	lu := func(d player.LyricTextDetailed) *player.LyricUpdate {
		return &player.LyricUpdate{Index: 0, Text: "ひか", RomaText: "hi ka", Timestamp: 13.711, RomaTextDetailed: d}
	}
	if hashEventData(player.EventLyricUpdate, lu(roma("ka "))) == hashEventData(player.EventLyricUpdate, lu(roma("ga "))) {
		t.Error("lyric_update 哈希没算上 roma_text_detailed：只改逐字音译的两条被判成内容相同")
	}
}
