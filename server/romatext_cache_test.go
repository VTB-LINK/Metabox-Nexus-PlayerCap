package server

// 本文件只测一件事：lyric_update 经**缓存回放**时，roma_text（音译）与 sub_text（翻译）
// 必须都原值保留。
//
// UpdatePlayerState 的 EventLyricUpdate 分支手动逐字段把收到的 *player.LyricUpdate 复制进
// ps.LyricUpdate。这个手复制曾漏搬 RomaText（只搬了 SubText），后果是 cloudmusicv3 经
// HTTP /lyric_update、新连接的 buildInitEvents 初始补发、player_switch 的 FullState 这些
// 缓存回放路径时，roma_text 被清成空串（字段还在、值丢了），与实时 WS 直发的有音译不一致。
// buildInitEvents 与 HTTP handler 都直接透传同一个 ps.LyricUpdate，故那次手复制是唯一的收口，
// 测它即测全部缓存出口。
//
// 变异自证：把 UpdatePlayerState EventLyricUpdate 分支里的 `RomaText: msg.RomaText` 删掉，
// 本文件前两条测试即红（roma 变空串）；sub_text 那条并列守着翻译轨不被一起丢。
//
// roma_text_detailed（逐字音译）是后来加的字段，走的是同一处手复制，同样会漏：
// 把那里的 `RomaTextDetailed: msg.RomaTextDetailed` 删掉，TestLyricUpdateCachePreservesRomaTextDetailed 即红。

import (
	"encoding/json"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// initLyricUpdate 从 buildInitEvents 里取出 lyric_update 载荷——缓存对外的出口之一。
// 用 buildInitEvents 而非直接读 ps 字段：它是缓存对外的出口，测它才是测接线。
func initLyricUpdate(t *testing.T, s *Server, name string) *LyricUpdate {
	t.Helper()
	for _, e := range s.buildInitEvents(name, true) {
		if e.Type == player.EventLyricUpdate {
			lu, ok := e.Data.(*LyricUpdate)
			if !ok {
				t.Fatalf("lyric_update 载荷类型是 %T，期望 *LyricUpdate", e.Data)
			}
			return lu
		}
	}
	t.Fatal("buildInitEvents 里没有 lyric_update")
	return nil
}

// seedRomaLyricUpdate 灌入一条既有翻译又有音译的 cloudmusicv3 lyric_update。
func seedRomaLyricUpdate(s *Server) {
	s.UpdatePlayerState(player.Event{
		PlayerName: "cloudmusicv3",
		Type:       player.EventLyricUpdate,
		Data: &player.LyricUpdate{
			Index: 37, Text: "夜に駆ける",
			SubText:   "向夜晚奔去",          // 翻译轨
			RomaText:  "yoru ni kakeru", // 音译轨
			Timestamp: 157.22, PlayTime: 156.46, Position: 156.4659, Progress: 0.6297,
		},
	})
}

// TestLyricUpdateCachePreservesRomaAndSubText 钉死缓存回放同时保住音译与翻译两条独立轨。
func TestLyricUpdateCachePreservesRomaAndSubText(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	seedRomaLyricUpdate(s)

	lu := initLyricUpdate(t, s, "cloudmusicv3")
	if lu.SubText != "向夜晚奔去" {
		t.Errorf("sub_text = %q，期望「向夜晚奔去」—— 缓存回放丢了翻译", lu.SubText)
	}
	if lu.RomaText != "yoru ni kakeru" {
		t.Errorf("roma_text = %q，期望「yoru ni kakeru」—— 缓存回放丢了音译"+
			"（UpdatePlayerState 手复制漏搬 RomaText）", lu.RomaText)
	}
}

// TestLyricUpdateCacheWireEmitsRomaText 更进一步：缓存出口经 JSON 序列化后，roma_text 必须
// 带着原值出现在 wire 上（HTTP /lyric_update 与 WS 补发都序列化同一个 ps.LyricUpdate）。
func TestLyricUpdateCacheWireEmitsRomaText(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	seedRomaLyricUpdate(s)

	b, err := json.Marshal(initLyricUpdate(t, s, "cloudmusicv3"))
	if err != nil {
		t.Fatalf("序列化 lyric_update 失败: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"roma_text":"yoru ni kakeru"`) {
		t.Errorf("wire 上缺 roma_text 原值，实得: %s", got)
	}
	if !strings.Contains(got, `"sub_text":"向夜晚奔去"`) {
		t.Errorf("wire 上缺 sub_text 原值，实得: %s", got)
	}
}

// TestLyricUpdateCachePreservesRomaTextDetailed 缓存回放保住逐字音译：字段是新加的，手复制处最容易漏。
func TestLyricUpdateCachePreservesRomaTextDetailed(t *testing.T) {
	s := NewServer([]string{"wesing"})
	s.UpdatePlayerState(player.Event{
		PlayerName: "wesing",
		Type:       player.EventLyricUpdate,
		Data: &player.LyricUpdate{
			Index: 4, Text: "ひかり", RomaText: "hi ka ri",
			Timestamp: 13.711, PlayTime: 13.511, Position: 13.8, Progress: 0.05,
			RomaTextDetailed: player.LyricTextDetailed{Timestamp: 13.711, PlayTime: 13.511, Duration: 0.95, Words: []player.LyricTextDetailedWord{
				{Timestamp: 13.711, PlayTime: 13.511, Duration: 0.2, Text: "hi "},
				{Timestamp: 13.911, PlayTime: 13.711, Duration: 0.49, Text: "ka "},
				{Timestamp: 14.401, PlayTime: 14.201, Duration: 0.26, Text: "ri "},
			}},
		},
	})
	lu := initLyricUpdate(t, s, "wesing")
	if n := len(lu.RomaTextDetailed.Words); n != 3 || lu.RomaTextDetailed.Words[1].Text != "ka " {
		t.Fatalf("roma_text_detailed 经缓存回放后剩 %d 个音节，期望 3 个——UpdatePlayerState 手复制漏搬 RomaTextDetailed", n)
	}
	b, err := json.Marshal(lu)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"roma_text_detailed":{"timestamp":13.711`) {
		t.Errorf("wire 上缺 roma_text_detailed 原值，实得: %s", b)
	}
}
