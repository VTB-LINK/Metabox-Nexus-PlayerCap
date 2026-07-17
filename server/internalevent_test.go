package server

// 本文件只测一件事：**clear_song_data 是内部事件——清缓存，但不外发**。
//
// 它的职责是把 playerState 抹掉，好让后续连入的客户端不会从 buildInitEvents 拿到上一首的
// 残留。载荷恒为 nil，从来不是给客户端看的。它此前会被推给 per-player 订阅者，是
// router.Run 对每个事件无差别调用 NotifySubscribers 的副作用。
//
// 两个方向都必须钉死，因为它们会朝相反的方向坏：
//   - 拦少了 → 内部信号泄漏回公开 API（TestClearSongDataNotBroadcast）
//   - 拦多了 → 缓存不再清空，残留数据复活（TestClearSongDataStillClearsCache）
//
// 第二条尤其要紧：把 internalEvents 拦在 UpdatePlayerState 上是个看起来更「干净」的写法，
// 而它会静默地把这个事件变成空操作——没有任何症状，直到某个客户端在切歌后连入并收到上一首。
//
// 不测 router 的路由决策（routerorder_test.go）、不测根路径选址（resolveplayer_test.go）。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// seedSongData 给某播放器灌入一首歌的完整缓存（歌曲信息 + 歌词）。
func seedSongData(s *Server, name string) {
	s.UpdatePlayerState(player.Event{
		PlayerName: name,
		Type:       player.EventSongInfoUpdate,
		Data:       &player.SongInfo{Name: "上一首", Singer: "某人", Title: "上一首 - 某人"},
	})
	s.UpdatePlayerState(player.Event{
		PlayerName: name,
		Type:       player.EventAllLyrics,
		Data: &player.AllLyricsData{
			Title:    "上一首 - 某人",
			Duration: 100,
			Count:    1,
			Lyrics:   []player.LyricLine{{Index: 0, Text: "一行"}},
		},
	})
}

// hasType 报告事件列表里有没有某个类型。
func hasType(events []WSEvent, t string) bool {
	for _, e := range events {
		if e.Type == t {
			return true
		}
	}
	return false
}

// TestClearSongDataNotBroadcast 钉死内部事件不外发——两类订阅者都收不到。
//
// **SetActivePlayer 是承重的，别删**：不设活跃播放器时，根订阅者本来就会被
// NotifySubscribers 里的 `evt.PlayerName != active` 挡掉，这条断言便恒真——
// 删掉 internalEvents 检查它也照样绿。设了才测的是 internalEvents 本身。
//
// 变异自证：删掉 NotifySubscribers 开头的 `if internalEvents[evt.Type] { return }` 即红
// （per-player 与根订阅者同时收到 clear_song_data）。
func TestClearSongDataNotBroadcast(t *testing.T) {
	s := NewServer([]string{"wesing"})
	root := s.subscribe("", nil, true, "root")
	per := s.subscribe("wesing", nil, true, "per-player")
	s.SetActivePlayer("wesing") // 见上：不设的话根那条断言是假的

	s.NotifySubscribers(player.Event{
		PlayerName: "wesing",
		Type:       player.EventClearSongData,
	}, false)

	for name, sub := range map[string]*subscriber{"根": root, "per-player": per} {
		select {
		case got := <-sub.ch:
			t.Errorf("%s订阅者收到了 %q —— clear_song_data 是内部事件，不该出现在公开 API 表面",
				name, got.Type)
		default:
		}
	}
}

// TestClearSongDataStillClearsCache 钉死拦广播**没有**把清缓存一起拦掉。
//
// 这是 internalEvents 最容易被写坏的方向：拦在 UpdatePlayerState 上语法更「顺」，
// 但那样 clear_song_data 就成了纯空操作，缓存永不清空——而症状要等到某个客户端在
// 切歌/退出后连入、收到上一首的 song_info + all_lyrics 才会显现。
//
// 用 buildInitEvents 而不是直接读 playerState 字段：它是缓存**对外的唯一出口**，
// 测它才是测接线；读字段只能证明赋值发生过。
//
// 变异自证：在 UpdatePlayerState 开头加 `if internalEvents[evt.Type] { return }` 即红。
func TestClearSongDataStillClearsCache(t *testing.T) {
	s := NewServer([]string{"wesing"})
	seedSongData(s, "wesing")

	// 前提自检：不先证明缓存里真有东西，下面的「清空了」就可能是从来没进去过
	if init := s.buildInitEvents("wesing"); !hasType(init, player.EventSongInfoUpdate) {
		t.Fatalf("前提不成立：灌入后 buildInitEvents 里没有 song_info_update，实得 %d 条", len(init))
	}

	s.UpdatePlayerState(player.Event{
		PlayerName: "wesing",
		Type:       player.EventClearSongData,
	})

	for _, unwanted := range []string{player.EventSongInfoUpdate, player.EventAllLyrics} {
		if init := s.buildInitEvents("wesing"); hasType(init, unwanted) {
			t.Errorf("clear_song_data 之后 buildInitEvents 仍吐 %q —— 缓存没被清空，"+
				"后续连入的客户端会收到上一首的残留", unwanted)
		}
	}
}

// TestNormalEventsStillBroadcast 钉死拦截**只**命中 clear_song_data。
//
// internalEvents 是一张会被后人续写的表。往里多塞一个对外事件，本包其余测试大多仍绿
// （它们不经过 NotifySubscribers），而订阅者会静默地再也收不到那类事件。
//
// 变异自证：往 internalEvents 里加 player.EventLyricUpdate 即红。
func TestNormalEventsStillBroadcast(t *testing.T) {
	s := NewServer([]string{"wesing"})
	per := s.subscribe("wesing", nil, true, "per-player")

	for _, evtType := range []string{
		player.EventStatusUpdate,
		player.EventSongInfoUpdate,
		player.EventLyricUpdate,
		player.EventAllLyrics,
		player.EventPlaybackPause,
		player.EventPlaybackResume,
		player.EventLyricIdle,
	} {
		s.NotifySubscribers(player.Event{PlayerName: "wesing", Type: evtType}, false)

		select {
		case got := <-per.ch:
			if got.Type != evtType {
				t.Errorf("收到 %q，期望 %q", got.Type, evtType)
			}
		default:
			t.Errorf("per-player 订阅者没收到 %q —— 它是对外事件，被误当成内部事件拦掉了", evtType)
		}
	}
}
