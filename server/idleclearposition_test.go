package server

// 本文件只测一件事：无活跃隐藏推给 lyric_update-SSE 的清行 lyric_update(index:-1) 带停下那一刻的
// position 与 progress（issue #176）。
//
// 契约：position = progress × duration，每条 lyric_update 都带、与 index 无关。清行此前只写了 Progress，
// 对外发出 position=0 而 progress 是停下时的值。隐藏只在状态不是 playing 时发生，此时 livePosition
// 不外推，HTTP 快照与连接补发给的都是缓存里最近一次采样的那一对，清行与之相同。
//
// 手法同 idlehide_test.go：sweepIdleHide 以 now+Δ 模拟空闲时长；位置的写入沿用 initposition_test.go 的
// seedLive / backdate。

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

// takeIdleClear 巡检至隐藏，返回 lyric_update-SSE 收到的那条清行；不是恰好一条 index:-1 的 lyric_update 即中止。
func takeIdleClear(t *testing.T, s *Server, name string, sub *subscriber) *LyricUpdate {
	t.Helper()
	s.sweepIdleHide(time.Now().Add(20 * time.Second))
	if !s.playerStates[name].IdleHidden {
		t.Fatal("前置：通道应已隐藏")
	}
	evs := drainCh(sub.ch)
	if len(evs) != 1 || evs[0].Type != player.EventLyricUpdate {
		t.Fatalf("lyric_update-SSE 应恰好收到一条 lyric_update，实得 %v", evTypes(evs))
	}
	lu, ok := evs[0].Data.(*LyricUpdate)
	if !ok || lu.Index != -1 {
		t.Fatalf("清行应为 index:-1 的 lyric_update，实得 %+v", evs[0].Data)
	}
	if _, err := json.Marshal(evs[0]); err != nil {
		t.Fatalf("清行无法序列化（NaN/Inf 进了载荷）: %v", err)
	}
	return lu
}

// TestIdleClearCarriesPosition 钉住清行的 position 是停下那一刻的整曲位置、progress 与之及缓存的
// duration 自洽，覆盖暂停、停止、无采样、duration 为 0 等情形。
//
// 变异自证（均实测）：删掉 notifyPerPlayerIdleClear 字面量里的 `Position: position`，除 position 恰为 0 的
// 三条外六条全红；position 改传 progress、progress 改传 position，同样这六条红；position 改按
// progress × duration 现算，「duration 为 0」一条红；sweepIdleHide 改取缓存 lyric_update 的 Position，
// 「播放中转暂停」「playback_pause」两条红，其后第一条没有当前行的用例空指针 panic。
// 等价变异一条，不红：sweepIdleHide 直取 ps.Position / ps.Progress 而不经 livePosition——隐藏只在
// 非 playing 时发生，二者给出同一对值。
func TestIdleClearCarriesPosition(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(s *Server)
		lo, hi float32 // 清行 position 的期望区间
		dur    float32 // 缓存的 duration，progress 须与 position 按它自洽
	}{
		{"暂停中：最近一次采样是当前行", func(s *Server) {
			seedLive(s, "paused", testDuration, 100)
		}, 100, 100, testDuration},
		// 离开 playing 时缓存冻结在外推到那一刻的位置；清行取冻结值，不是当前行写入时的 100。
		{"播放中转暂停：冻结在离开 playing 的那一刻", func(s *Server) {
			seedLive(s, "playing", testDuration, 100)
			backdate(s, 3*time.Second)
			s.UpdatePlayerState(statusEvt(livePlayer, "paused"))
		}, 103, 103 + slack, testDuration},
		{"playback_pause 给出的暂停点", func(s *Server) {
			seedLive(s, "playing", testDuration, 100)
			s.UpdatePlayerState(posEvt(player.EventPlaybackPause, 120, testDuration))
			backdate(s, 2*time.Second) // 暂停点不外推：pause 与 paused 状态之间的间隔不计入
			s.UpdatePlayerState(statusEvt(livePlayer, "paused"))
		}, 120, 120, testDuration},
		{"停止后未清缓存：只发了 standby", func(s *Server) {
			seedLive(s, "paused", testDuration, 100)
			s.UpdatePlayerState(statusEvt(livePlayer, "standby"))
		}, 100, 100, testDuration},
		{"停止后已清缓存：standby 加 clear_song_data", func(s *Server) {
			seedLive(s, "paused", testDuration, 100)
			s.UpdatePlayerState(statusEvt(livePlayer, "standby"))
			s.UpdatePlayerState(clearEvt(livePlayer))
		}, 0, 0, 0},
		{"新通道：只有状态、没有位置采样", func(s *Server) {
			s.UpdatePlayerState(statusEvt(livePlayer, "paused"))
		}, 0, 0, 0},
		{"新歌刚宣告：all_lyrics 的 position 为 0", func(s *Server) {
			s.UpdatePlayerState(statusEvt(livePlayer, "paused"))
			s.UpdatePlayerState(player.Event{PlayerName: livePlayer, Type: player.EventAllLyrics, Data: &player.AllLyricsData{
				Title: "某歌 - 某人", Duration: testDuration, Count: 1, Lyrics: []player.LyricLine{{Index: 0, Text: "一行"}},
			}})
		}, 0, 0, testDuration},
		// 没有 all_lyrics，缓存的 duration 为 0：progress 为 0，position 照常是采样值。
		{"duration 为 0：只有带位置的当前行", func(s *Server) {
			s.UpdatePlayerState(statusEvt(livePlayer, "paused"))
			s.UpdatePlayerState(posEvt(player.EventLyricUpdate, 50, 0))
		}, 50, 50, 0},
		// 从未收到状态事件的通道按 offline 隐藏；livePosition 对 nil 状态同样不外推。
		{"从未收到状态事件", func(s *Server) {
			s.UpdatePlayerState(player.Event{PlayerName: livePlayer, Type: player.EventAllLyrics, Data: &player.AllLyricsData{
				Title: "某歌 - 某人", Duration: testDuration, Count: 1, Lyrics: []player.LyricLine{{Index: 0, Text: "一行"}},
			}})
			s.UpdatePlayerState(posEvt(player.EventLyricUpdate, 100, testDuration))
		}, 100, 100, testDuration},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewServer([]string{livePlayer})
			s.SetIdleHideResolver(func(string) int { return 15 })
			sub := s.subscribe(livePlayer, []string{player.EventLyricUpdate}, false, "sse-lyric")
			c.setup(s)

			lu := takeIdleClear(t, s, livePlayer, sub)
			if !(lu.Position >= c.lo && lu.Position <= c.hi) {
				t.Errorf("清行 position = %v，期望在 [%v, %v]", lu.Position, c.lo, c.hi)
			}
			checkProgress(t, "清行", lu.Position, lu.Progress, c.dur)
			if d := s.playerStates[livePlayer].Duration; d != c.dur {
				t.Errorf("前置：缓存的 duration = %v，期望 %v", d, c.dur)
			}
		})
	}
}

// TestIdleClearMatchesHTTPSnapshot 钉住清行与同一时刻根路径 HTTP 快照给出的 position/progress 相同。
// 根路径不受 per-player 隐藏影响（§3.4），隐藏之后仍可取快照对照。
//
// 变异自证：删掉清行的 `Position: position` 即红（清行 0、快照 103 左右，实测）。
func TestIdleClearMatchesHTTPSnapshot(t *testing.T) {
	s := NewServer([]string{livePlayer})
	s.SetIdleHideResolver(func(string) int { return 15 })
	s.SetActivePlayer(livePlayer)
	sub := s.subscribe(livePlayer, []string{player.EventLyricUpdate}, false, "sse-lyric")
	seedLive(s, "playing", testDuration, 100)
	backdate(s, 3*time.Second)
	s.UpdatePlayerState(statusEvt(livePlayer, "paused"))

	lu := takeIdleClear(t, s, livePlayer, sub)

	rec := httptest.NewRecorder()
	s.handleLyricUpdate("")(rec, httptest.NewRequest("GET", "/lyric_update", nil))
	var resp struct {
		Data LyricUpdate `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v (%s)", err, rec.Body.String())
	}
	if resp.Data.Index != 3 {
		t.Fatalf("前置：根路径快照应是缓存的当前行 index 3，实得 %+v", resp.Data)
	}
	if lu.Position != resp.Data.Position || lu.Progress != resp.Data.Progress {
		t.Errorf("清行 (%v, %v) 与根路径 HTTP 快照 (%v, %v) 的 position/progress 不一致",
			lu.Position, lu.Progress, resp.Data.Position, resp.Data.Progress)
	}
}

// TestIdleClearPerChannelPosition 钉住同一次巡检隐藏多个通道时，各通道的清行带各自的位置。
//
// 变异自证：推送循环里位置统一取最后一个通道的（`toClear[len(toClear)-1].position`），本例红（实测）。
func TestIdleClearPerChannelPosition(t *testing.T) {
	s := NewServer([]string{livePlayer, idlePlayer})
	s.SetIdleHideResolver(func(string) int { return 15 })
	subs := map[string]*subscriber{
		livePlayer: s.subscribe(livePlayer, []string{player.EventLyricUpdate}, false, "sse-a"),
		idlePlayer: s.subscribe(idlePlayer, []string{player.EventLyricUpdate}, false, "sse-b"),
	}
	want := map[string]float32{livePlayer: 30, idlePlayer: 200}
	for name, pos := range want {
		s.UpdatePlayerState(statusEvt(name, "paused"))
		s.UpdatePlayerState(allLyricsEvt(name, oldSong))
		s.UpdatePlayerState(lineEvt(name, oldSong, 0, pos))
	}

	s.sweepIdleHide(time.Now().Add(20 * time.Second))

	for name, pos := range want {
		evs := drainCh(subs[name].ch)
		if len(evs) != 1 {
			t.Fatalf("%s：lyric_update-SSE 应恰好收到一条清行，实得 %v", name, evTypes(evs))
		}
		lu, ok := evs[0].Data.(*LyricUpdate)
		if !ok || lu.Index != -1 || lu.Position != pos || lu.Progress != player.ClampProgress(pos, testDuration) {
			t.Errorf("%s：清行应为 index:-1、position %v，实得 %+v", name, pos, evs[0].Data)
		}
	}
}

// TestIdleClearOtherTransportsUnchanged 钉住清行带上 position 之后，其余订阅者收到的清除不变：
// WS 与不过滤类型的 SSE 只收一条 player_clear；song_info-SSE 收空的 song_info_update；同时订了两类的
// SSE 两条都收；根订阅者与别的播放器的连接什么都收不到；清行不写入缓存。
//
// 变异自证（均实测）：发出 player_clear 之后不跳过 in-band 清除、song_info 的清除改为非空载荷、
// 清行写进缓存的当前行，均红。
func TestIdleClearOtherTransportsUnchanged(t *testing.T) {
	s := NewServer([]string{idlePlayer, otherPlayer})
	s.SetIdleHideResolver(func(string) int { return 15 })
	s.SetActivePlayer(idlePlayer)
	ws := s.subscribe(idlePlayer, nil, true, "ws")
	sseAll := s.subscribe(idlePlayer, nil, false, "sse-all")
	song := s.subscribe(idlePlayer, []string{player.EventSongInfoUpdate}, false, "sse-song")
	both := s.subscribe(idlePlayer, []string{player.EventLyricUpdate, player.EventSongInfoUpdate}, false, "sse-both")
	root := s.subscribe("", nil, true, "root")
	other := s.subscribe(otherPlayer, nil, true, "other")
	feed(s, statusEvt(idlePlayer, "paused"), songInfoEvt(idlePlayer, oldSong),
		allLyricsEvt(idlePlayer, oldSong), lineEvt(idlePlayer, oldSong, 1, 100))
	for _, sub := range []*subscriber{ws, sseAll, song, both, root, other} {
		drainCh(sub.ch)
	}

	hideIdle(t, s, 20*time.Second)

	for label, sub := range map[string]*subscriber{"WS": ws, "不过滤类型的 SSE": sseAll} {
		evs := expectTypes(t, label, sub, player.EventPlayerClear)
		if len(evs) == 1 && (evs[0].Player != idlePlayer || evs[0].Data != struct{}{}) {
			t.Errorf("%s：player_clear 应为 player=%s、data={}，实得 %+v", label, idlePlayer, evs[0])
		}
	}
	if evs := expectTypes(t, "song_info-SSE", song, player.EventSongInfoUpdate); len(evs) == 1 && evs[0].Data != struct{}{} {
		t.Errorf("song_info-SSE 的清除应为空 data，实得 %+v", evs[0].Data)
	}
	evs := expectTypes(t, "同时订两类的 SSE", both, player.EventLyricUpdate, player.EventSongInfoUpdate)
	if len(evs) == 2 {
		if lu, ok := evs[0].Data.(*LyricUpdate); !ok || lu.Index != -1 || lu.Position != 100 {
			t.Errorf("清行应为 index:-1、position 100，实得 %+v", evs[0].Data)
		}
		if evs[1].Data != struct{}{} {
			t.Errorf("song_info_update 的清除应为空 data，实得 %+v", evs[1].Data)
		}
	}
	expectTypes(t, "根订阅者", root)
	expectTypes(t, "另一个播放器的连接", other)

	// 清行只推给在线连接、不进缓存：缓存的当前行仍是 index 1。
	if lu := s.playerStates[idlePlayer].LyricUpdate; lu == nil || lu.Index != 1 || lu.Position != 100 {
		t.Errorf("隐藏后缓存的当前行应不变（index 1、position 100），实得 %+v", lu)
	}
}

// TestIdleClearThenResume 钉住清行之后解除隐藏的补发照旧：lyric_update-SSE 收到的是缓存的当前行、
// 位置是最近一次采样，不是清行。
//
// 变异自证：清行写进缓存的当前行（补发出 index:-1）即红（实测）。
func TestIdleClearThenResume(t *testing.T) {
	s := NewServer([]string{idlePlayer})
	s.SetIdleHideResolver(func(string) int { return 15 })
	c := joinIdle(s)
	feed(s, statusEvt(idlePlayer, "paused"), songInfoEvt(idlePlayer, oldSong),
		allLyricsEvt(idlePlayer, oldSong), lineEvt(idlePlayer, oldSong, 1, 100))
	drainCh(c.ws.ch)
	drainCh(c.lyric.ch)
	drainCh(c.song.ch)

	lu := takeIdleClear(t, s, idlePlayer, c.lyric)
	if lu.Position != 100 {
		t.Fatalf("前置：清行 position 应为 100，实得 %v", lu.Position)
	}
	expectTypes(t, "隐藏时的 WS", c.ws, player.EventPlayerClear)
	expectTypes(t, "隐藏时的 song_info-SSE", c.song, player.EventSongInfoUpdate)

	feed(s, timeEvt(idlePlayer, player.EventPlaybackResume, 42.5))

	expectTypes(t, "解除后的 WS", c.ws, append(append([]string{}, fullSet...), player.EventPlaybackResume)...)
	evs := expectTypes(t, "解除后的 lyric_update-SSE", c.lyric, player.EventLyricUpdate)
	if len(evs) == 1 {
		got, ok := evs[0].Data.(*LyricUpdate)
		if !ok || got.Index != 1 || got.Position != 42.5 || got.Progress != player.ClampProgress(42.5, testDuration) {
			t.Errorf("补发的应是缓存的当前行 index 1、位置 42.5，实得 %+v", evs[0].Data)
		}
	}
	if evs := expectTypes(t, "解除后的 song_info-SSE", c.song, player.EventSongInfoUpdate); len(evs) == 1 {
		if got := songsIn(evs); len(got) != 1 || got[0] != oldSong {
			t.Errorf("song_info-SSE 补发的应是缓存的歌曲信息 %q，实得 %v", oldSong, got)
		}
	}
}
