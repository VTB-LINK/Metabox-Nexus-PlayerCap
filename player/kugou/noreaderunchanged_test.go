package kugou

// 本文件只测没有读取者时会话的对外事件与接入本地歌词之前逐条相同（#205，设计 §7「kugou 会话」末条）：启动器为
// nil（EnsurePatched 走「其他错误」的会话）、或启动器返回 nil（上一个会话的读取者还没退出），事件的类型与载荷
// 都与基线相同。
//
// 基线取自接入之前的 runSession（53d5864，旧签名 runSession(client)）：以 sessionstatus_test.go 的
// TestMidSessionTransientStaysSilent「上一首在播」一例的读数跑一次，取词换成本文件的 baselineLyrics，逐条记下
// 全部事件的类型与 JSON 载荷（含位置采样），抄成下面的字面量。A 有两行歌词（首行带逐字、翻译与音译，offset 为
// -300 ms），B 只有纯音乐提示语，两条 emitLyrics 的分支各走一次。
//
// 变异自证（工作树的隔离副本里注入、跑完即还原，实测）：emitLyrics 遇到纯音乐提示语不清行，两个子例都红。

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// baselineLyrics 是基线的在线取词结果。每次调用返回新切片：会话会就地套 offset。
func baselineLyrics(hash string) []klyric.Line {
	if hash == "B" {
		return []klyric.Line{{Index: 0, Time: 0, Text: "纯音乐，请欣赏"}}
	}
	return []klyric.Line{
		{Index: 0, Time: 0, Text: "第一行", SubText: "line one", RomaText: "di yi hang",
			Detailed: player.LyricTextDetailed{Timestamp: 0, Duration: 1.2, Words: []player.LyricTextDetailedWord{
				{Text: "第", Timestamp: 0, Duration: 0.4}, {Text: "一", Timestamp: 0.4, Duration: 0.4}, {Text: "行", Timestamp: 0.8, Duration: 0.4}}}},
		{Index: 1, Time: 5, Text: "第二行"},
	}
}

// baselineEvents 是 53d5864 的 runSession 以上述读数与取词发出的全部事件。
var baselineEvents = []struct{ typ, data string }{
	{"status_update", `{"status":"playing","detail":"歌手 - 歌A"}`},
	{"all_lyrics", `{"title":"歌手 - 歌A","duration":200,"position":0,"progress":0,"count":2,"lyrics":[{"index":0,"timestamp":0,"play_time":0,"text":"第一行","sub_text":"line one","roma_text":"di yi hang","text_detailed":{"timestamp":0,"play_time":0,"duration":1.2,"words":[{"timestamp":0,"play_time":0,"duration":0.4,"text":"第"},{"timestamp":0.4,"play_time":0.099999994,"duration":0.4,"text":"一"},{"timestamp":0.8,"play_time":0.5,"duration":0.4,"text":"行"}]},"roma_text_detailed":{},"furigana":[]},{"index":1,"timestamp":5,"play_time":4.7,"text":"第二行","sub_text":"","roma_text":"","text_detailed":{},"roma_text_detailed":{},"furigana":[]}]}`},
	{"lyric_update", `{"index":0,"text":"第一行","sub_text":"line one","roma_text":"di yi hang","timestamp":0,"play_time":0,"position":0,"progress":0,"text_detailed":{"timestamp":0,"play_time":0,"duration":1.2,"words":[{"timestamp":0,"play_time":0,"duration":0.4,"text":"第"},{"timestamp":0.4,"play_time":0.099999994,"duration":0.4,"text":"一"},{"timestamp":0.8,"play_time":0.5,"duration":0.4,"text":"行"}]},"roma_text_detailed":{},"furigana":[]}`},
	{"position_sample", `{"position":0,"progress":0}`},
	{"all_lyrics", `{"title":"歌手 - 歌B","duration":200,"position":0,"progress":0,"count":0,"lyrics":[]}`},
	{"lyric_update", `{"index":-1,"text":"纯音乐，请欣赏","sub_text":"","roma_text":"","timestamp":0,"play_time":0,"position":0,"progress":0,"text_detailed":{},"roma_text_detailed":{},"furigana":[]}`},
	{"playback_resume", `{"position":0,"progress":0}`},
	{"status_update", `{"status":"playing","detail":"歌手 - 歌B"}`},
}

// startLocal 为 nil、或启动器返回 nil：事件序列与基线逐条相同（类型与载荷）。
func TestSessionNilStarterUnchanged(t *testing.T) {
	cases := []struct {
		name  string
		start localLyricsStarter
	}{
		{"启动器为 nil", nil},
		{"启动器返回 nil", func(context.Context) localLyrics { return nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := fetchLyrics
			fetchLyrics = func(hash string, _ int, _, _ string) ([]klyric.Line, error) { return baselineLyrics(hash), nil }
			defer func() { fetchLyrics = orig }()

			p := New(-300, 0)
			src := &scriptedSource{reads: []*cdp.PlayInfo{song("A", "playing"), song("A", "playing"), song("B", "stopped"), song("B", "stopped"), song("B", "playing")}}
			synctest.Test(t, func(*testing.T) { p.runSession(src, c.start) })

			got := drainAll(p)
			if len(got) != len(baselineEvents) {
				t.Errorf("事件 %d 条，基线 %d 条", len(got), len(baselineEvents))
			}
			for i := range min(len(got), len(baselineEvents)) {
				b, err := json.Marshal(got[i].Data)
				if err != nil {
					t.Fatalf("第 %d 条 %s 载荷序列化失败：%v", i, got[i].Type, err)
				}
				if want := baselineEvents[i]; got[i].Type != want.typ || string(b) != want.data {
					t.Errorf("第 %d 条\n got  %s %s\n want %s %s", i, got[i].Type, b, want.typ, want.data)
				}
			}
		})
	}
}
