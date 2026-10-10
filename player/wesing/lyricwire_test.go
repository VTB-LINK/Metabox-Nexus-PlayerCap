package wesing

// 本文件只测内存读出的歌词行到对外载荷（all_lyrics 的行、lyric_update）的字段映射：音译进 roma_text、
// 逐字音译进 roma_text_detailed，翻译恒空，互不串。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/wesing/lyric"
)

func wireLines() []lyric.LyricLine {
	return []lyric.LyricLine{
		{Index: 2, Time: 13.711, Text: "ひかり", RomaText: "hi ka ri",
			RomaDetailed: player.LyricTextDetailed{Timestamp: 13.711, Duration: 0.95, Words: []player.LyricTextDetailedWord{
				{Timestamp: 13.711, Duration: 0.2, Text: "hi "},
				{Timestamp: 13.911, Duration: 0.49, Text: "ka "},
				{Timestamp: 14.401, Duration: 0.26, Text: "ri "},
			}}},
		{Index: 5, Time: 14.59, Text: "Yeah get it on nah"},
	}
}

// 变异自证：把 toLyricItems 里传给 BuildLyricLine 的 `"", l.RomaText` 对调成 `l.RomaText, ""`，
// 音译落进 sub_text 而红；lineUpdate 同样对调，第二段而红。
func TestLyricWireRomaText(t *testing.T) {
	lines := wireLines()
	items := toLyricItems(lines, 0.2)
	if len(items) != 2 || items[0].RomaText != "hi ka ri" || items[0].SubText != "" || items[0].Index != 2 || items[0].Text != "ひかり" {
		t.Fatalf("all_lyrics 的行：音译应进 roma_text、翻译为空，got %+v", items)
	}
	if items[1].RomaText != "" || items[1].SubText != "" {
		t.Fatalf("没有音译的行 roma_text 应为空串，got %+v", items[1])
	}

	u := lineUpdate(lines[0], 0.2, 13.8, 204)
	if u.RomaText != "hi ka ri" || u.SubText != "" || u.Index != 2 || u.Text != "ひかり" {
		t.Fatalf("lyric_update：音译应进 roma_text、翻译为空，got %+v", u)
	}
}

// 逐字音译进 roma_text_detailed，各音节的 play_time 与主歌词的逐字一样按 offset 套过；没有音译的行为零值。
//
// 变异自证：把 toLyricItems 里的 `items[i].RomaTextDetailed = l.RomaDetailed` 删掉，第一段没有逐字音译
// 而红；把 lineUpdate 里的 `u.RomaTextDetailed = l.RomaDetailed` 删掉，第二段而红；把 applyDetailedOffset
// 里的 `&lyrics[i].RomaDetailed` 去掉（只套主歌词的逐字），音节的 play_time 仍是 0 而红。
func TestLyricWireRomaTextDetailed(t *testing.T) {
	lines := wireLines()
	applyDetailedOffset(lines, 0.2)

	check := func(where string, d player.LyricTextDetailed) {
		t.Helper()
		if len(d.Words) != 3 || d.Words[1].Text != "ka " || d.Words[1].Timestamp != 13.911 {
			t.Fatalf("%s：roma_text_detailed 应带 3 个音节，got %+v", where, d.Words)
		}
		if got, want := d.Words[1].PlayTime, player.AdjustLyricPlayTime(13.911, 0.2); got != want || got == 0 {
			t.Fatalf("%s：音节的 play_time = %v，want %v（按 offset 套过）", where, got, want)
		}
		if got, want := d.PlayTime, player.AdjustLyricPlayTime(13.711, 0.2); got != want {
			t.Fatalf("%s：整行的 play_time = %v，want %v", where, got, want)
		}
	}
	items := toLyricItems(lines, 0.2)
	check("all_lyrics 的行", items[0].RomaTextDetailed)
	if len(items[1].RomaTextDetailed.Words) != 0 {
		t.Fatalf("没有音译的行 roma_text_detailed 应为零值，got %+v", items[1].RomaTextDetailed)
	}
	check("lyric_update", lineUpdate(lines[0], 0.2, 13.8, 204).RomaTextDetailed)
}
