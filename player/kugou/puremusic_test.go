package kugou

// 本文件只测 emitLyrics 对无歌词提示语的识别与发出（#231，设计 doc/design/converge-player-behavior-231-design.md §7.4、§7.5）：
// 经 player.PureMusicHint 认出单行提示（含 kuwo 形状）与「歌名行 + 提示句」两行，返回 nil 以清空 currentLyrics，
// 发空 all_lyrics（非 nil 切片）与 index:-1 的 lyric_update，且 text 填提示句原文而非歌名行，带上实时位置与进度。
//
// 变异自证（均实测变红）：emitLyrics 仍读 lyricItems[0].Text 而非 PureMusicHint 返回的 hint →
// 「歌名行加 kuwo 提示取第 1 行原文」「歌名行加共用提示取第 1 行原文」红；
// 改回只认「纯音乐，请欣赏」的旧判断 → 「kuwo 形状一行」与两条「歌名行…」红。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

func TestEmitLyricsPureMusicHint(t *testing.T) {
	const (
		sharedHint   = "纯音乐，请欣赏"
		kuwoPureHint = "此歌曲为没有填词的纯音乐，请您欣赏"
		kuwoGaiHint  = "该歌曲为纯音乐请欣赏"
		title        = "Mark Petrie - Go Time"
	)

	cases := []struct {
		name          string
		title         string
		lines         []klyric.Line
		wantNoLyrics  bool
		wantHint      string
		wantLineCount int
	}{
		{
			name:         "共用提示一行",
			title:        title,
			lines:        []klyric.Line{{Index: 0, Time: 0, Text: sharedHint}},
			wantNoLyrics: true,
			wantHint:     sharedHint,
		},
		{
			name:         "kuwo 形状一行",
			title:        title,
			lines:        []klyric.Line{{Index: 0, Time: 0, Text: kuwoPureHint}},
			wantNoLyrics: true,
			wantHint:     kuwoPureHint,
		},
		{
			name:  "歌名行加 kuwo 提示取第 1 行原文",
			title: title,
			lines: []klyric.Line{
				{Index: 0, Time: 0, Text: "Mark Petrie-Go Time"},
				{Index: 1, Time: 0.5, Text: kuwoGaiHint},
			},
			wantNoLyrics: true,
			wantHint:     kuwoGaiHint,
		},
		{
			name:  "歌名行加共用提示取第 1 行原文",
			title: title,
			lines: []klyric.Line{
				{Index: 0, Time: 0, Text: "Mark Petrie-Go Time"},
				{Index: 1, Time: 0.5, Text: sharedHint},
			},
			wantNoLyrics: true,
			wantHint:     sharedHint,
		},
		{
			name:  "反例：首行与标题不同按普通歌词",
			title: title,
			lines: []klyric.Line{
				{Index: 0, Time: 0, Text: "Other Artist-Go Time"},
				{Index: 1, Time: 0.5, Text: kuwoGaiHint},
			},
			wantNoLyrics:  false,
			wantLineCount: 2,
		},
		{
			name:         "零行无歌词 text 为空",
			title:        title,
			lines:        nil,
			wantNoLyrics: true,
			wantHint:     "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New(0, 50)
			gotLines := p.emitLyrics(c.title, 200, 12.5, c.lines, 0)
			evs := drainAll(p)

			if c.wantNoLyrics {
				if len(gotLines) != 0 {
					t.Fatalf("应返回空 currentLyrics，实得 %+v", gotLines)
				}
				if len(evs) != 2 {
					t.Fatalf("应发 all_lyrics 与 lyric_update 共 2 条，实得 %d 条: %+v", len(evs), evs)
				}
				al, ok := evs[0].Data.(*player.AllLyricsData)
				if !ok || al.Count != 0 || al.Lyrics == nil || len(al.Lyrics) != 0 {
					t.Fatalf("第 1 条应为空的非 nil all_lyrics，实得 %+v", evs[0])
				}
				lu, ok := evs[1].Data.(*player.LyricUpdate)
				if !ok || lu.Index != -1 || lu.Text != c.wantHint || lu.Position != 12.5 || lu.Progress != player.ClampProgress(12.5, 200) {
					t.Fatalf("第 2 条 lyric_update = %+v，期望 Index=-1 Text=%q Position=12.5 Progress=%v",
						evs[1].Data, c.wantHint, player.ClampProgress(12.5, 200))
				}
			} else {
				if len(gotLines) != c.wantLineCount {
					t.Fatalf("应保留 %d 行歌词，实得 %d", c.wantLineCount, len(gotLines))
				}
				if len(evs) != 1 {
					t.Fatalf("普通歌词应只发 1 条 all_lyrics（不发 index:-1），实得 %d 条: %+v", len(evs), evs)
				}
				al, ok := evs[0].Data.(*player.AllLyricsData)
				if !ok || al.Count != c.wantLineCount || len(al.Lyrics) != c.wantLineCount {
					t.Fatalf("all_lyrics = %+v，期望 count=%d", evs[0].Data, c.wantLineCount)
				}
			}
		})
	}
}
