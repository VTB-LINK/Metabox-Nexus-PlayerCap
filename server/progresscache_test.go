package server

// 本文件只测一件事：**缓存里的 play_time 与 progress 必须来自同一时刻**。
//
// 它们是同一个量的两个投影（progress = play_time / duration）。缓存是 buildInitEvents、
// NotifySubscribersFullState、以及全部 HTTP 端点的共同数据源——任何一个投影没跟上，
// 那三条出口就会同时吐出自相矛盾的 all_lyrics。
//
// 真实缺陷（实测 280 次 HTTP 采样，零例外）：lyric_update 只更新了 ps.PlayTime，
// 于是 ps.Progress 从切歌那一刻起永远停在 0：
//
//	GET /cloudmusicv3/all_lyrics → play_time=134.86  duration=234.308  progress=0
//	                                                （它该是 0.5756）
//
// 症状：切播放器时根订阅者收到 FullState 的 all_lyrics，进度条归零而时间在走；
// 中途连入的 overlay 同理。WS 上**真实的** all_lyrics 事件反而是自洽的——它只在切歌时发，
// 那时 play_time 本来就是 0，两个投影碰巧都对。这正是它藏了这么久的原因。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

const (
	testDuration = 234.308
	testPlayTime = 134.86
	testProgress = testPlayTime / testDuration // 0.5756…
)

// seedSongAt 灌入「切歌那一刻」的 all_lyrics：歌刚开始，play_time 与 progress 都是 0。
func seedSongAt(s *Server, name string) {
	s.UpdatePlayerState(player.Event{
		PlayerName: name,
		Type:       player.EventAllLyrics,
		Data: &player.AllLyricsData{
			Title:    "某歌 - 某人",
			Duration: testDuration,
			PlayTime: 0,
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

// TestLyricUpdateRefreshesProgressCache 钉死 lyric_update 会把 progress 一起刷新。
//
// 变异自证：删掉 UpdatePlayerState 里 EventLyricUpdate 分支的 `ps.Progress = msg.Progress` 即红。
func TestLyricUpdateRefreshesProgressCache(t *testing.T) {
	s := NewServer([]string{"wesing"})
	seedSongAt(s, "wesing")

	// 前提自检：切歌那一刻两个投影都是 0，此时无法区分「更新了」和「没更新」
	if al := initAllLyrics(t, s, "wesing"); al.PlayTime != 0 || al.Progress != 0 {
		t.Fatalf("前提不成立：切歌时应为 0/0，实得 play_time=%v progress=%v", al.PlayTime, al.Progress)
	}

	// 歌播到 134.86s —— 只有 lyric_update 会报告这件事，all_lyrics 整首歌只发一次
	s.UpdatePlayerState(player.Event{
		PlayerName: "wesing",
		Type:       player.EventLyricUpdate,
		Data:       &player.LyricUpdate{Index: 45, Text: "某行", PlayTime: testPlayTime, Progress: testProgress},
	})

	al := initAllLyrics(t, s, "wesing")
	if al.PlayTime != testPlayTime {
		t.Errorf("play_time = %v，期望 %v", al.PlayTime, testPlayTime)
	}
	if al.Progress != testProgress {
		t.Errorf("progress = %v，期望 %v —— lyric_update 没把 progress 一起刷进缓存，"+
			"于是它从切歌起永远停在旧值（实测停在 0）", al.Progress, testProgress)
	}
}

// TestInitAllLyricsIsSelfConsistent 钉死出口的两个投影自洽：progress ≈ play_time / duration。
//
// 这条比上一条更本质：它不关心「哪个字段被谁更新」，只要求**吐出去的东西不自相矛盾**。
// 未来若有人改用别的方式维护缓存，上一条可能失效，这条仍然成立。
//
// 变异自证：同样删掉 `ps.Progress = msg.Progress` 即红（progress 停在 0，
// 而 play_time/duration 已是 0.5756）。
func TestInitAllLyricsIsSelfConsistent(t *testing.T) {
	s := NewServer([]string{"wesing"})
	seedSongAt(s, "wesing")
	s.UpdatePlayerState(player.Event{
		PlayerName: "wesing",
		Type:       player.EventLyricUpdate,
		Data:       &player.LyricUpdate{Index: 45, Text: "某行", PlayTime: testPlayTime, Progress: testProgress},
	})

	al := initAllLyrics(t, s, "wesing")
	if al.Duration == 0 {
		t.Fatal("duration 为 0，无法校验自洽性")
	}
	want := al.PlayTime / al.Duration
	if diff := want - al.Progress; diff > 0.01 || diff < -0.01 {
		t.Errorf("play_time=%v / duration=%v = %.4f，但 progress=%v —— 两个投影不是同一时刻的，"+
			"下游拿 play_time 显示时间、拿 progress 画进度条，两者会打架",
			al.PlayTime, al.Duration, want, al.Progress)
	}
}
