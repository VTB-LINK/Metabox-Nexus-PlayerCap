package server

// 本文件只测一件事：路由清屏（NotifySubscribersClear）补上根 SSE 的 in-band 清除（#231，设计 §4.5 第 4 条）。
//
// 契约：
//   - 路由全员不活跃触发清屏时（含唯一活跃播放器退出、唯一活跃播放器暂停保持超时释放等路径）：
//     - 根 WS 与不过滤类型的根 SSE 照旧只收 player_switch(from=oldPlayer, to="") 与 player_clear(player="")，
//       不收重复的 in-band 清除。
//     - 根 lyric_update-SSE 收 lyric_update（player=""、index:-1、position 与 progress 取 oldPlayer 的 livePosition）。
//     - 根 song_info-SSE 收空 song_info_update（player=""、data:{}）。
//     - 同时订阅两类的根 SSE 两条都收。
//     - 专属订阅者不受影响。
//   - 全部发送保持 select default 非阻塞（AGENTS §2.1）。

import (
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

// TestRootSSEClearOnRouteClear 覆盖路由清屏的各触发路径与全部订阅者类型：
//   - 唯一活跃播放器退出（waiting_process/process_exited）；
//   - 唯一活跃播放器暂停保持超时释放（模拟 watchExpire 组级超时调用 clearActivePlayer）；
//   - 直接调用 NotifySubscribersClear（含不过滤类型的 SSE、同时订阅两类的 SSE，以及 oldPlayer 处于播放中与无缓存）。
//
// 变异自证（均实测）：
//   - NotifySubscribersClear 不补根 SSE → 全部子例的根两条 SSE 收不到事件而红；
//   - NotifySubscribersClear 发出 player_clear 后不跳过 in-band → 根 WS 与全类型根 SSE 多收两条而红；
//   - NotifySubscribersClear 不读 oldPlayer 的 livePosition（position/progress 写 0）→ 退出、超时释放、直调子例红；
//   - NotifySubscribersClear 去掉 sub.player != "" 过滤 → 专属订阅者收到清除而红。
func TestRootSSEClearOnRouteClear(t *testing.T) {
	t.Run("唯一活跃播放器退出", func(t *testing.T) {
		rig := newStopClearRig([]string{scPlayerA, scPlayerC}, nil)
		rig.announcePlaying(scPlayerA, oldSong, 75)
		rig.drainAll()

		rig.send(
			statusReasonEvt(scPlayerA, "waiting_process", player.ReasonProcessExited, "进程已退出"),
			clearEvt(scPlayerA),
		)

		// 根 WS：只收 player_switch(to="") 与 player_clear(player="")，不收重复 in-band
		rootWSEvs := expectTypes(t, "根 WS", rig.root.ws, player.EventPlayerSwitch, player.EventPlayerClear)
		if len(rootWSEvs) == 2 {
			if sw, ok := rootWSEvs[0].Data.(*player.PlayerSwitchInfo); !ok || sw.From != scPlayerA || sw.To != "" || rootWSEvs[0].Player != "" {
				t.Errorf("根 WS player_switch 异常：%#v", rootWSEvs[0])
			}
			assertPlayerClearEvt(t, "根 WS player_clear", rootWSEvs[1], "")
		}

		// 根两条 SSE：各收一条 in-band 清除（player=""，带 oldPlayer 退出前的位置）
		if evs := expectTypes(t, "根 lyric_update-SSE", rig.root.lyric, player.EventLyricUpdate); len(evs) == 1 {
			assertLyricClearEvt(t, "根 lyric_update-SSE", evs[0], "", 75, 75+slack, testDuration)
		}
		if evs := expectTypes(t, "根 song_info-SSE", rig.root.song, player.EventSongInfoUpdate); len(evs) == 1 {
			assertSongClearEvt(t, "根 song_info-SSE", evs[0], "")
		}

		// 专属通道按 2026-10-22 决定不受路由清屏影响（只收 status_update，不收任何清除）
		expectTypes(t, "A 专属 WS", rig.a.ws, player.EventStatusUpdate)
		expectTypes(t, "A 专属 lyric_update-SSE", rig.a.lyric)
		expectTypes(t, "A 专属 song_info-SSE", rig.a.song)
		rig.other.expectEmpty(t, "不相干播放器")
	})

	t.Run("唯一活跃播放器暂停保持超时释放", func(t *testing.T) {
		rig := newStopClearRig([]string{scPlayerA, scPlayerC}, nil)
		rig.announcePlaying(scPlayerA, oldSong, 110)
		rig.send(statusReasonEvt(scPlayerA, "paused", "", oldSong+" - 某人"))
		if got := rig.router.GetActivePlayer(); got != scPlayerA {
			t.Fatalf("前置：暂停保持期间 activePlayer 应仍为 %q，实得 %q", scPlayerA, got)
		}
		rig.drainAll()

		// 模拟 watchExpire 普通组组级超时释放（server/router.go:491-493）
		rig.router.mu.Lock()
		forceGroupInert(rig.router.normalStates)
		rig.router.normalGroupPaused = false
		cleared := rig.router.clearActivePlayer()
		rig.router.mu.Unlock()
		if !cleared {
			t.Fatal("前置：clearActivePlayer 应返回 true")
		}

		rootWSEvs := expectTypes(t, "根 WS", rig.root.ws, player.EventPlayerSwitch, player.EventPlayerClear)
		if len(rootWSEvs) == 2 {
			assertPlayerClearEvt(t, "根 WS player_clear", rootWSEvs[1], "")
		}
		if evs := expectTypes(t, "根 lyric_update-SSE", rig.root.lyric, player.EventLyricUpdate); len(evs) == 1 {
			assertLyricClearEvt(t, "根 lyric_update-SSE", evs[0], "", 110, 110+slack, testDuration)
		}
		if evs := expectTypes(t, "根 song_info-SSE", rig.root.song, player.EventSongInfoUpdate); len(evs) == 1 {
			assertSongClearEvt(t, "根 song_info-SSE", evs[0], "")
		}

		// 专属订阅者不受影响
		rig.a.expectEmpty(t, "A 专属出口")
		rig.other.expectEmpty(t, "不相干播放器")
	})

	t.Run("全部订阅者过滤形状与外推位置", func(t *testing.T) {
		s := NewServer([]string{livePlayer, otherPlayer})
		seedLive(s, "playing", testDuration, 100)
		backdate(s, 3*time.Second)

		rootWS := s.subscribe("", nil, true, "root-ws")
		rootSSEAll := s.subscribe("", nil, false, "root-sse-all")
		rootLyric := s.subscribe("", []string{player.EventLyricUpdate}, false, "root-sse-lyric")
		rootSong := s.subscribe("", []string{player.EventSongInfoUpdate}, false, "root-sse-song")
		rootBoth := s.subscribe("", []string{player.EventLyricUpdate, player.EventSongInfoUpdate}, false, "root-sse-both")
		perWS := s.subscribe(livePlayer, nil, true, "per-ws")
		perLyric := s.subscribe(livePlayer, []string{player.EventLyricUpdate}, false, "per-sse-lyric")
		perSong := s.subscribe(livePlayer, []string{player.EventSongInfoUpdate}, false, "per-sse-song")
		otherWS := s.subscribe(otherPlayer, nil, true, "other-ws")

		s.NotifySubscribersClear(livePlayer)

		// WS 与不过滤类型的 SSE 只收 player_switch + player_clear
		for label, sub := range map[string]*subscriber{"根 WS": rootWS, "不过滤类型的根 SSE": rootSSEAll} {
			evs := expectTypes(t, label, sub, player.EventPlayerSwitch, player.EventPlayerClear)
			if len(evs) == 2 {
				assertPlayerClearEvt(t, label, evs[1], "")
			}
		}
		// 根歌词 SSE 带外推到此刻的位置（100+3=103s）
		if evs := expectTypes(t, "根 lyric_update-SSE", rootLyric, player.EventLyricUpdate); len(evs) == 1 {
			assertLyricClearEvt(t, "根 lyric_update-SSE", evs[0], "", 103, 103+slack, testDuration)
		}
		// 根歌曲 SSE 收空 song_info_update
		if evs := expectTypes(t, "根 song_info-SSE", rootSong, player.EventSongInfoUpdate); len(evs) == 1 {
			assertSongClearEvt(t, "根 song_info-SSE", evs[0], "")
		}
		// 同时订阅两类的根 SSE 两条都收
		if evs := expectTypes(t, "同时订两类的根 SSE", rootBoth, player.EventLyricUpdate, player.EventSongInfoUpdate); len(evs) == 2 {
			assertLyricClearEvt(t, "同时订两类 lyric", evs[0], "", 103, 103+slack, testDuration)
			assertSongClearEvt(t, "同时订两类 song", evs[1], "")
		}
		// 专属订阅者不受影响
		expectTypes(t, "专属 WS", perWS)
		expectTypes(t, "专属 lyric_update-SSE", perLyric)
		expectTypes(t, "专属 song_info-SSE", perSong)
		expectTypes(t, "其他播放器 WS", otherWS)
	})

	t.Run("oldPlayer 无缓存或不存在时仍发零值位置清除", func(t *testing.T) {
		s := NewServer([]string{scPlayerA})
		rootLyric := s.subscribe("", []string{player.EventLyricUpdate}, false, "root-sse-lyric")
		rootSong := s.subscribe("", []string{player.EventSongInfoUpdate}, false, "root-sse-song")

		s.NotifySubscribersClear("unknown")

		if evs := expectTypes(t, "根 lyric_update-SSE", rootLyric, player.EventLyricUpdate); len(evs) == 1 {
			assertLyricClearEvt(t, "根 lyric_update-SSE", evs[0], "", 0, 0, 0)
		}
		if evs := expectTypes(t, "根 song_info-SSE", rootSong, player.EventSongInfoUpdate); len(evs) == 1 {
			assertSongClearEvt(t, "根 song_info-SSE", evs[0], "")
		}
	})
}

// TestRootSSEClearFullChannelDoesNotBlock 钉住 NotifySubscribersClear 全部发送点非阻塞（AGENTS §2.1）。
func TestRootSSEClearFullChannelDoesNotBlock(t *testing.T) {
	s := NewServer([]string{scPlayerA})
	subs := []*subscriber{
		s.subscribe("", nil, true, "root-ws"),
		s.subscribe("", []string{player.EventLyricUpdate}, false, "root-sse-lyric"),
		s.subscribe("", []string{player.EventSongInfoUpdate}, false, "root-sse-song"),
	}
	for _, sub := range subs {
		for range cap(sub.ch) {
			sub.ch <- WSEvent{Type: "filler"}
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.NotifySubscribersClear(scPlayerA)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("订阅通道已满时 NotifySubscribersClear 被阻塞（违反 AGENTS §2.1）")
	}
}
