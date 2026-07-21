package server

// 本文件只测一件事：**缓存里的 position 与 progress 必须来自同一时刻**。
//
// 它们是同一个量的两个投影（progress = position / duration）。缓存是 buildInitEvents、
// NotifySubscribersFullState、以及全部 HTTP 端点的共同数据源——任何一个投影没跟上，
// 那三条出口就会同时吐出自相矛盾的 all_lyrics。
//
// 真实缺陷（实测 280 次 HTTP 采样，零例外）：lyric_update 只更新了整曲位置缓存，
// 于是 ps.Progress 从切歌那一刻起永远停在 0：
//
//	GET /cloudmusicv3/all_lyrics → position=134.86  duration=234.308  progress=0
//	                                                （它该是 0.5756）
//
// 症状：切播放器时根订阅者收到 FullState 的 all_lyrics，进度条归零而时间在走；
// 中途连入的 overlay 同理。WS 上**真实的** all_lyrics 事件反而是自洽的——它只在切歌时发，
// 那时 position 本来就是 0，两个投影碰巧都对。这正是它藏了这么久的原因。
//
// 另一处接线也在这里钉住：lyric_update 的 position（整曲实时位置）与 play_time（本行播出
// 时间）是两个量，喂 all_lyrics.position 的必须是前者。下面的 lyric_update 故意让二者取不同
// 值，若谁把缓存写成 `ps.Position = msg.PlayTime`（本行播出时间）即被抓红。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

const (
	testDuration = 234.308
	testPosition = 134.86
	testLinePlay = 130.0                       // 本行播出时间，与整曲位置不同，用于区分两个量
	testProgress = testPosition / testDuration // 0.5756…
)

// seedSongAt 灌入「切歌那一刻」的 all_lyrics：歌刚开始，position 与 progress 都是 0。
func seedSongAt(s *Server, name string) {
	s.UpdatePlayerState(player.Event{
		PlayerName: name,
		Type:       player.EventAllLyrics,
		Data: &player.AllLyricsData{
			Title:    "某歌 - 某人",
			Duration: testDuration,
			Position: 0,
			Progress: 0,
			Count:    1,
			Lyrics:   []player.LyricLine{{Index: 0, Text: "一行"}},
		},
	})
}

// initAllLyrics 从 buildInitEvents 里取出 all_lyrics 载荷。
//
// 用 buildInitEvents 而非直接读 ps 字段：它是缓存对外的出口之一，测它才是测接线。
func initAllLyrics(t *testing.T, s *Server, name string) *AllLyrics {
	t.Helper()
	for _, e := range s.buildInitEvents(name) {
		if e.Type == player.EventAllLyrics {
			al, ok := e.Data.(*AllLyrics)
			if !ok {
				t.Fatalf("all_lyrics 载荷类型是 %T，期望 *AllLyrics", e.Data)
			}
			return al
		}
	}
	t.Fatal("buildInitEvents 里没有 all_lyrics")
	return nil
}

// TestLyricUpdateRefreshesProgressCache 钉死 lyric_update 会把 position 与 progress 一起刷新，
// 且喂缓存的是 position（整曲位置）而非 play_time（本行播出时间）。
//
// 变异自证：删掉 UpdatePlayerState 里 EventLyricUpdate 分支的 `ps.Position = msg.Position`
// 或 `ps.Progress = msg.Progress` 任一即红；把 `ps.Position = msg.Position` 写成
// `= msg.PlayTime` 也红（al.Position 会变成 testLinePlay 而非 testPosition）。
func TestLyricUpdateRefreshesProgressCache(t *testing.T) {
	s := NewServer([]string{"wesing"})
	seedSongAt(s, "wesing")

	// 前提自检：切歌那一刻两个投影都是 0，此时无法区分「更新了」和「没更新」
	if al := initAllLyrics(t, s, "wesing"); al.Position != 0 || al.Progress != 0 {
		t.Fatalf("前提不成立：切歌时应为 0/0，实得 position=%v progress=%v", al.Position, al.Progress)
	}

	// 歌播到 134.86s —— 只有 lyric_update 会报告这件事，all_lyrics 整首歌只发一次。
	s.UpdatePlayerState(player.Event{
		PlayerName: "wesing",
		Type:       player.EventLyricUpdate,
		Data:       &player.LyricUpdate{Index: 45, Text: "某行", PlayTime: testLinePlay, Position: testPosition, Progress: testProgress},
	})

	al := initAllLyrics(t, s, "wesing")
	if al.Position != testPosition {
		t.Errorf("position = %v，期望 %v —— 缓存要么没刷新、要么误存了本行 play_time", al.Position, testPosition)
	}
	if al.Progress != testProgress {
		t.Errorf("progress = %v，期望 %v —— lyric_update 没把 progress 一起刷进缓存，"+
			"于是它从切歌起永远停在旧值（实测停在 0）", al.Progress, testProgress)
	}
}

// TestInitAllLyricsIsSelfConsistent 钉死出口的两个投影自洽：progress ≈ position / duration。
//
// 这条比上一条更本质：它不关心「哪个字段被谁更新」，只要求**吐出去的东西不自相矛盾**。
// 未来若有人改用别的方式维护缓存，上一条可能失效，这条仍然成立。
//
// 变异自证：删掉 `ps.Position = msg.Position` 或 `ps.Progress = msg.Progress` 即红
// （一个让 position 停在 0、另一个让 progress 停在 0，两个投影随即打架）。
func TestInitAllLyricsIsSelfConsistent(t *testing.T) {
	s := NewServer([]string{"wesing"})
	seedSongAt(s, "wesing")
	s.UpdatePlayerState(player.Event{
		PlayerName: "wesing",
		Type:       player.EventLyricUpdate,
		Data:       &player.LyricUpdate{Index: 45, Text: "某行", PlayTime: testLinePlay, Position: testPosition, Progress: testProgress},
	})

	al := initAllLyrics(t, s, "wesing")
	if al.Duration == 0 {
		t.Fatal("duration 为 0，无法校验自洽性")
	}
	want := al.Position / al.Duration
	if diff := want - al.Progress; diff > 0.01 || diff < -0.01 {
		t.Errorf("position=%v / duration=%v = %.4f，但 progress=%v —— 两个投影不是同一时刻的，"+
			"下游拿 position 显示时间、拿 progress 画进度条，两者会打架",
			al.Position, al.Duration, want, al.Progress)
	}
}
