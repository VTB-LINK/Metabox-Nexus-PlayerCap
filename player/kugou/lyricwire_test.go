package kugou

// 本文件只测解析出的歌词行到对外载荷（all_lyrics 的行、lyric_update）的逐字音译接线：
// krc.Line.RomaDetailed 进 roma_text_detailed，两个事件同源；offset 套到它的 play_time。
// 逐字音译本身怎么构造见 player/krc 的 romadetailed_test.go。

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/krc"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// wireKRC 是一份自拟的 KRC 明文：行 0 有翻译与逐字音译；行 1 的 type=0 轨只有一个片段（片段数与
// 字数不符，只有行级音译）；行 2 没有音译。
func wireKRC(t *testing.T) []klyric.Line {
	t.Helper()
	doc := `{"content":[` +
		`{"lyricContent":[["你"],["天空"],[""]],"type":1},` +
		`{"lyricContent":[["ki ","mi "],["sora"],[]],"type":0}],"version":1}`
	lines := krc.ParsePlainKRC("[language:" + base64.StdEncoding.EncodeToString([]byte(doc)) + "]\n" +
		"[1000,1000]<0,500,0>き<500,500,0>み\n" +
		"[3000,1000]<0,500,0>そ<500,500,0>ら\n" +
		"[5000,1000]<0,500,0>la<500,500,0>la\n")
	if len(lines) != 3 || len(lines[0].RomaDetailed.Words) != 2 || len(lines[1].RomaDetailed.Words) != 0 {
		t.Fatalf("样例前提不成立：%+v", lines)
	}
	return lines
}

// all_lyrics 的行与 lyric_update 都带逐字音译，且是同一行的同一份；没有逐字音译的行两处都是零值、
// 序列化为 {}。主歌词、翻译、行级音译照常。
//
// 变异自证（隔离的 worktree，实测）：删掉 toLyricLines 里的 `out[i].RomaTextDetailed = l.RomaDetailed`，
// all_lyrics 的行没有逐字音译而红；删掉 lineUpdate 里的 `u.RomaTextDetailed = l.RomaDetailed`，
// lyric_update 没有而红。
func TestLyricWireRomaTextDetailed(t *testing.T) {
	lines := wireKRC(t)
	applyDetailedOffset(lines, -0.25)
	items := toLyricLines(lines, -0.25)
	if len(items) != 3 {
		t.Fatalf("all_lyrics 行数=%d, want 3", len(items))
	}

	d := items[0].RomaTextDetailed
	if len(d.Words) != 2 || d.Words[0].Text != "ki " || d.Words[1].Text != "mi " {
		t.Fatalf("all_lyrics 行0：roma_text_detailed 应带 2 个单元，got %+v", d.Words)
	}
	if d.Words[1].Timestamp != 1.5 || d.Words[1].Duration != 0.5 || d.Timestamp != 1.0 || d.Duration != 1.0 {
		t.Fatalf("all_lyrics 行0：时间应取主歌词的字与行，got %+v", d)
	}
	if items[0].Text != "きみ" || items[0].SubText != "你" || items[0].RomaText != "ki mi" || len(items[0].TextDetailed.Words) != 2 {
		t.Fatalf("all_lyrics 行0：主歌词、翻译、行级音译应照常，got %+v", items[0])
	}
	for _, i := range []int{1, 2} {
		if got := items[i].RomaTextDetailed; len(got.Words) != 0 || got.Timestamp != 0 || got.PlayTime != 0 {
			t.Fatalf("all_lyrics 行%d：没有逐字音译应为零值，got %+v", i, got)
		}
	}
	if items[1].RomaText != "sora" {
		t.Fatalf("all_lyrics 行1：片段数不符只是没有逐字，行级音译应照常，got %q", items[1].RomaText)
	}

	for i := range lines {
		u := lineUpdate(i, lines[i], -0.25, 3.3, 200)
		if !reflect.DeepEqual(u.RomaTextDetailed, items[i].RomaTextDetailed) {
			t.Fatalf("行%d：lyric_update 与 all_lyrics 的 roma_text_detailed 不同源\n lyric_update %+v\n all_lyrics   %+v", i, u.RomaTextDetailed, items[i].RomaTextDetailed)
		}
		if u.Index != i || u.Text != items[i].Text || u.SubText != items[i].SubText || u.RomaText != items[i].RomaText ||
			u.PlayTime != items[i].PlayTime || !reflect.DeepEqual(u.TextDetailed, items[i].TextDetailed) || u.Position != 3.3 {
			t.Fatalf("行%d：lyric_update 其余字段应与 all_lyrics 的行一致，got %+v", i, u)
		}
	}

	// wire：有逐字音译的行带 words，没有的行是 {}。
	b, err := json.Marshal(&player.AllLyricsData{Lyrics: items, Count: len(items)})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"roma_text_detailed":{"timestamp":1,"play_time":0.75,"duration":1,"words":[{"timestamp":1,"play_time":0.75,"duration":0.5,"text":"ki "}`) {
		t.Fatalf("wire 上行0 的 roma_text_detailed 不对：%s", s)
	}
	if n := strings.Count(s, `"roma_text_detailed":{}`); n != 2 {
		t.Fatalf("wire 上应有 2 行的 roma_text_detailed 为 {}，got %d：%s", n, s)
	}
}

// offset 套到逐字音译的 play_time（行级与各单元），正负各一例；主歌词的逐字同样套。负 offset 把
// 播出时间压到 0 以下时钳到 0（AdjustLyricPlayTime 的下限）。
//
// 变异自证（隔离的 worktree，实测）：applyDetailedOffset 里去掉 `&lines[i].RomaDetailed`（只套主歌词
// 的逐字），逐字音译的 play_time 仍是 0 而红；只套行级、不套各单元，单元的 play_time 是 0 而红。
func TestApplyDetailedOffsetCoversRomaDetailed(t *testing.T) {
	for _, off := range []float32{0.25, -0.25, -5} {
		lines := wireKRC(t)
		applyDetailedOffset(lines, off)
		for _, d := range []player.LyricTextDetailed{lines[0].Detailed, lines[0].RomaDetailed} {
			if got, want := d.PlayTime, player.AdjustLyricPlayTime(d.Timestamp, off); got != want {
				t.Fatalf("offset %v：行级 play_time = %v，want %v", off, got, want)
			}
			for j, w := range d.Words {
				if got, want := w.PlayTime, player.AdjustLyricPlayTime(w.Timestamp, off); got != want {
					t.Fatalf("offset %v：单元%d 的 play_time = %v，want %v", off, j, got, want)
				}
			}
		}
		// 具体取值：第二个单元的时间戳 1.5。
		want := map[float32]float32{0.25: 1.75, -0.25: 1.25, -5: 0}[off]
		if got := lines[0].RomaDetailed.Words[1].PlayTime; got != want {
			t.Fatalf("offset %v：第二个单元的 play_time = %v，want %v", off, got, want)
		}
		// 没有逐字音译的行不凭空长出东西。
		if d := lines[1].RomaDetailed; len(d.Words) != 0 || d.PlayTime != 0 {
			t.Fatalf("offset %v：没有逐字音译的行应保持零值，got %+v", off, d)
		}
	}
}

// 套 offset 时两条时间轴互不影响：逐字音译的单元与主歌词的字不共享底层数组。
func TestRomaDetailedOffsetIndependentOfMain(t *testing.T) {
	lines := wireKRC(t)
	applyDetailedOffset(lines, 0.5)
	lines[0].RomaDetailed.Words[0].PlayTime = 99
	if got := lines[0].Detailed.Words[0].PlayTime; got != 1.5 {
		t.Fatalf("改逐字音译的 play_time 影响到了主歌词的逐字：%v", got)
	}
}

// 行级 LRC 回落的歌词（没有逐字、没有音译）：两处都是零值。
func TestLyricWireLRCFallbackHasNoRomaDetailed(t *testing.T) {
	lines := []klyric.Line{{Index: 0, Time: 12.3, Text: "行级歌词"}}
	applyDetailedOffset(lines, 0.3)
	items := toLyricLines(lines, 0.3)
	u := lineUpdate(0, lines[0], 0.3, 12.4, 200)
	if len(items[0].RomaTextDetailed.Words) != 0 || len(u.RomaTextDetailed.Words) != 0 || items[0].RomaText != "" {
		t.Fatalf("行级歌词不应有逐字音译，got %+v / %+v", items[0], u)
	}
	b, _ := json.Marshal(u)
	if !strings.Contains(string(b), `"roma_text_detailed":{}`) {
		t.Fatalf("wire 上应为 {}：%s", b)
	}
}
