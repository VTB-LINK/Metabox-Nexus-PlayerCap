package server

// 本文件只测一件事：真停播（waiting_song 任意 reason，或 standby / metadata_unavailable）后接
// clear_song_data 时，服务端派生清除的推送行为（#231，设计 §4.5～§4.8）。
//
// 契约：
//   - 真停播时向该播放器的专属出口推清除（WS 收 player_clear，歌词 SSE 收 index:-1 且带清空前的
//     livePosition，歌曲 SSE 收空 song_info_update）；若路由评估后根仍指向该播放器（如另一家处于
//     暂停保持），根出口同步收到同样的清除（player 为该播放器名，不配 player_switch）。
//   - 退出与断开（waiting_process、standby 的其余 reason）不推派生清除（2026-10-22 决定）。
//   - 仍处于 playing / paused 时收 clear_song_data（如 kuwo 换歌）不推清除。
//   - 去重按与缓存分开维护的 ShowsSong 判定：断开清缓存时不复位，重连第一拍即无歌仍能清除专属画面；
//     推过派生清除或无活跃隐藏清除时复位，不重复推送；专属已隐藏而根仍在保持时，根仍按缓存存在推清除。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/player"
)

const (
	scPlayerA = "cloudmusicv3"
	scPlayerB = "kugou"
	scPlayerC = "qqmusic"
)

func statusReasonEvt(name, status, reason, detail string) player.Event {
	return player.Event{
		PlayerName: name,
		Type:       player.EventStatusUpdate,
		Data:       &player.StatusInfo{Status: status, Detail: detail, Reason: reason},
	}
}

type subTriple struct {
	ws, lyric, song *subscriber
}

func subscribeTriple(s *Server, playerName string) subTriple {
	tag := playerName
	if tag == "" {
		tag = "root"
	}
	return subTriple{
		ws:    s.subscribe(playerName, nil, true, tag+"-ws"),
		lyric: s.subscribe(playerName, []string{player.EventLyricUpdate}, false, tag+"-sse-lyric"),
		song:  s.subscribe(playerName, []string{player.EventSongInfoUpdate}, false, tag+"-sse-song"),
	}
}

func (st subTriple) drain() {
	drainCh(st.ws.ch)
	drainCh(st.lyric.ch)
	drainCh(st.song.ch)
}

func (st subTriple) expectEmpty(t *testing.T, label string) {
	t.Helper()
	expectTypes(t, label+" WS", st.ws)
	expectTypes(t, label+" lyric_update-SSE", st.lyric)
	expectTypes(t, label+" song_info-SSE", st.song)
}

// stopClearRig 串联真实 Server 与 Router，按 router.Run 的完整四步同步回放事件，
// 并同时挂载根、目标、辅助与不相干播放器的 WS 与两条 SSE。
type stopClearRig struct {
	srv    *Server
	router *Router
	root   subTriple
	a      subTriple
	b      subTriple
	other  subTriple
}

func newStopClearRig(players []string, prior []string) *stopClearRig {
	cfg := config.DefaultConfig()
	cfg.PriorPlayer = prior
	srv := NewServer(players)
	r := NewRouter(&cfg, srv, players)
	rig := &stopClearRig{
		srv:    srv,
		router: r,
		root:   subscribeTriple(srv, ""),
		a:      subscribeTriple(srv, players[0]),
		other:  subscribeTriple(srv, players[len(players)-1]),
	}
	if len(players) >= 3 {
		rig.b = subscribeTriple(srv, players[1])
	}
	return rig
}

// send 按 router.Run 顺序同步处理一条事件：UpdatePlayerState → updateRouting → switchSkip → NotifySubscribers。
func (rig *stopClearRig) send(evts ...player.Event) {
	for _, evt := range evts {
		rig.srv.UpdatePlayerState(evt)
		switched := rig.router.updateRouting(evt)
		if !switched {
			rig.router.mu.Lock()
			if rig.router.switchSkipPlayer == evt.PlayerName && rig.router.switchSkipHashes != nil {
				if oldHash, ok := rig.router.switchSkipHashes[evt.Type]; ok {
					newHash := hashEventData(evt.Type, evt.Data)
					delete(rig.router.switchSkipHashes, evt.Type)
					if newHash == oldHash {
						switched = true
					}
					if len(rig.router.switchSkipHashes) == 0 {
						rig.router.switchSkipPlayer = ""
						rig.router.switchSkipHashes = nil
					}
				}
			}
			rig.router.mu.Unlock()
		}
		rig.srv.NotifySubscribers(evt, switched)
	}
}

func (rig *stopClearRig) drainAll() {
	rig.root.drain()
	rig.a.drain()
	if rig.b.ws != nil {
		rig.b.drain()
	}
	rig.other.drain()
}

// announcePlaying 让指定播放器宣告一首正在播放的歌（歌名、全文歌词、当前行位置 pos）。
func (rig *stopClearRig) announcePlaying(name, song string, pos float32) {
	rig.send(
		statusReasonEvt(name, "playing", "", song+" - 某人"),
		songInfoEvt(name, song),
		allLyricsEvt(name, song),
		lineEvt(name, song, 0, pos),
	)
}

func assertPlayerClearEvt(t *testing.T, label string, e WSEvent, wantPlayer string) {
	t.Helper()
	if e.Type != player.EventPlayerClear {
		t.Errorf("%s：期望 type=%q，实得 %q", label, player.EventPlayerClear, e.Type)
	}
	if e.Player != wantPlayer {
		t.Errorf("%s：期望 player=%q，实得 %q", label, wantPlayer, e.Player)
	}
	if e.Data != struct{}{} {
		t.Errorf("%s：player_clear 的 data 应为空对象 struct{}{}，实得 %#v", label, e.Data)
	}
}

func assertLyricClearEvt(t *testing.T, label string, e WSEvent, wantPlayer string, lo, hi, dur float32) {
	t.Helper()
	if e.Type != player.EventLyricUpdate {
		t.Errorf("%s：期望 type=%q，实得 %q", label, player.EventLyricUpdate, e.Type)
		return
	}
	if e.Player != wantPlayer {
		t.Errorf("%s：期望 player=%q，实得 %q", label, wantPlayer, e.Player)
	}
	lu, ok := e.Data.(*LyricUpdate)
	if !ok || lu == nil {
		t.Errorf("%s：期望 *LyricUpdate，实得 %T (%#v)", label, e.Data, e.Data)
		return
	}
	if lu.Index != -1 {
		t.Errorf("%s：清行 index 应为 -1，实得 %d", label, lu.Index)
	}
	if !(lu.Position >= lo && lu.Position <= hi) {
		t.Errorf("%s：清行 position = %v，期望在 [%v, %v]", label, lu.Position, lo, hi)
	}
	checkProgress(t, label, lu.Position, lu.Progress, dur)
	if _, err := json.Marshal(e); err != nil {
		t.Errorf("%s：清行无法序列化: %v", label, err)
	}
}

func assertSongClearEvt(t *testing.T, label string, e WSEvent, wantPlayer string) {
	t.Helper()
	if e.Type != player.EventSongInfoUpdate {
		t.Errorf("%s：期望 type=%q，实得 %q", label, player.EventSongInfoUpdate, e.Type)
	}
	if e.Player != wantPlayer {
		t.Errorf("%s：期望 player=%q，实得 %q", label, wantPlayer, e.Player)
	}
	if e.Data != struct{}{} {
		t.Errorf("%s：空 song_info_update 的 data 应为空对象 struct{}{}，实得 %#v", label, e.Data)
	}
}

type httpSnapshot struct {
	Player string          `json:"player"`
	Data   json.RawMessage `json:"data"`
}

func getHTTPSnapshot(t *testing.T, h http.HandlerFunc, path string) httpSnapshot {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", path, nil))
	var snap httpSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("解析 %s 响应失败: %v (%s)", path, err, rec.Body.String())
	}
	return snap
}

func assertFourEndpointsClearedWithStatus(t *testing.T, s *Server, reqPlayer, wantPlayer, wantStatus, wantReason string) {
	t.Helper()
	for _, ep := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"song_info", s.handleSongInfo(reqPlayer)},
		{"all_lyrics", s.handleAllLyrics(reqPlayer)},
		{"lyric_update", s.handleLyricUpdate(reqPlayer)},
	} {
		snap := getHTTPSnapshot(t, ep.h, "/"+ep.name)
		if snap.Player != wantPlayer {
			t.Errorf("%s(%q)：player = %q，期望 %q", ep.name, reqPlayer, snap.Player, wantPlayer)
		}
		if string(snap.Data) != "{}" {
			t.Errorf("%s(%q)：data = %s，期望 {}", ep.name, reqPlayer, snap.Data)
		}
	}

	stSnap := getHTTPSnapshot(t, s.handleStatusUpdate(reqPlayer), "/status_update")
	if stSnap.Player != wantPlayer {
		t.Errorf("status_update(%q)：player = %q，期望 %q", reqPlayer, stSnap.Player, wantPlayer)
	}
	if wantStatus == "" {
		if string(stSnap.Data) != "{}" {
			t.Errorf("status_update(%q)：data = %s，期望 {}", reqPlayer, stSnap.Data)
		}
		return
	}
	var st StatusMessage
	if err := json.Unmarshal(stSnap.Data, &st); err != nil {
		t.Fatalf("解析 status_update(%q) 失败: %v (%s)", reqPlayer, err, stSnap.Data)
	}
	if st.Status != wantStatus || st.Reason != wantReason {
		t.Errorf("status_update(%q)：实得 status=%q reason=%q，期望 status=%q reason=%q",
			reqPlayer, st.Status, st.Reason, wantStatus, wantReason)
	}
}

// TestStopClearThreeColumnMatrix 钉住设计 §4.5 的三列核心矩阵：
//   - 唯一活跃播放器真停播：根走路由清屏（player_switch(to="") + player_clear，根 SSE 各一条 in-band），
//     不重复发派生清除；专属收 waiting_song 后 player_clear 与两条 in-band 清除。
//   - 活跃播放器真停播、另一家暂停保持（根仍停在它上面）：根收 waiting_song 后派生 player_clear（player=该播放器，
//     不配 player_switch）与两条 in-band 清除；专属同上。
//   - 非活跃播放器真停播：根 WS、根 SSE、根 HTTP 不受影响；专属收清除。
//
// 变异自证（均实测）：
//   - notifyStopClear 不发 → 三列专属出口与第 2 列根出口红；
//   - 根推送条件去掉 activePlayer == name → 第 1 列根 WS 多出一条 player_clear（且根 SSE 多发一条）、第 3 列根收到清除而红；
//   - notifyStopClear 发给所有专属订阅者 → 不相干播放器与 B 的专属订阅者收到清除而红。
func TestStopClearThreeColumnMatrix(t *testing.T) {
	t.Run("唯一活跃播放器真停播", func(t *testing.T) {
		rig := newStopClearRig([]string{scPlayerA, scPlayerB, scPlayerC}, nil)
		rig.announcePlaying(scPlayerA, oldSong, 80)
		rig.drainAll()

		rig.send(
			statusReasonEvt(scPlayerA, "waiting_song", player.ReasonNoSong, "没有歌曲"),
			clearEvt(scPlayerA),
		)

		// 根 WS：只收路由清屏的 player_switch(to="") 与 player_clear(player="") 各一条，不再有派生 player_clear
		rootWSEvs := expectTypes(t, "根 WS", rig.root.ws, player.EventPlayerSwitch, player.EventPlayerClear)
		if len(rootWSEvs) == 2 {
			if sw, ok := rootWSEvs[0].Data.(*player.PlayerSwitchInfo); !ok || sw.From != scPlayerA || sw.To != "" || rootWSEvs[0].Player != "" {
				t.Errorf("根 WS player_switch 异常：%#v", rootWSEvs[0])
			}
			assertPlayerClearEvt(t, "根 WS player_clear", rootWSEvs[1], "")
		}
		// 根两条 SSE：各收一条路由清屏的 in-band 清除（player=""）
		if evs := expectTypes(t, "根 lyric_update-SSE", rig.root.lyric, player.EventLyricUpdate); len(evs) == 1 {
			assertLyricClearEvt(t, "根 lyric_update-SSE", evs[0], "", 80, 80+slack, testDuration)
		}
		if evs := expectTypes(t, "根 song_info-SSE", rig.root.song, player.EventSongInfoUpdate); len(evs) == 1 {
			assertSongClearEvt(t, "根 song_info-SSE", evs[0], "")
		}

		// 专属 WS：waiting_song 在前，player_clear(player=A) 在后
		perWSEvs := expectTypes(t, "专属 WS", rig.a.ws, player.EventStatusUpdate, player.EventPlayerClear)
		if len(perWSEvs) == 2 {
			assertPlayerClearEvt(t, "专属 WS player_clear", perWSEvs[1], scPlayerA)
		}
		// 专属两条 SSE：各收一条 in-band 清除（player=A）
		if evs := expectTypes(t, "专属 lyric_update-SSE", rig.a.lyric, player.EventLyricUpdate); len(evs) == 1 {
			assertLyricClearEvt(t, "专属 lyric_update-SSE", evs[0], scPlayerA, 80, 80+slack, testDuration)
		}
		if evs := expectTypes(t, "专属 song_info-SSE", rig.a.song, player.EventSongInfoUpdate); len(evs) == 1 {
			assertSongClearEvt(t, "专属 song_info-SSE", evs[0], scPlayerA)
		}

		// 不相干播放器无事件
		rig.b.expectEmpty(t, "辅助播放器 B")
		rig.other.expectEmpty(t, "不相干播放器")

		// 根 HTTP 四端点：player=""，data={}
		assertFourEndpointsClearedWithStatus(t, rig.srv, "", "", "", "")
		// 专属 HTTP：三端点 {}，status_update 为 waiting_song
		assertFourEndpointsClearedWithStatus(t, rig.srv, scPlayerA, scPlayerA, "waiting_song", player.ReasonNoSong)
	})

	t.Run("活跃播放器真停播且另一家暂停保持", func(t *testing.T) {
		rig := newStopClearRig([]string{scPlayerA, scPlayerB, scPlayerC}, nil)
		// 先让 B 播再暂停，形成普通组 holding
		rig.announcePlaying(scPlayerB, newSong, 30)
		rig.send(statusReasonEvt(scPlayerB, "paused", "", newSong+" - 某人"))
		// 再让 A 播，A 成为 activePlayer
		rig.announcePlaying(scPlayerA, oldSong, 95)
		if got := rig.router.GetActivePlayer(); got != scPlayerA {
			t.Fatalf("前置：activePlayer 应为 %q，实得 %q", scPlayerA, got)
		}
		rig.drainAll()

		rig.send(
			statusReasonEvt(scPlayerA, "waiting_song", player.ReasonNoSong, "没有歌曲"),
			clearEvt(scPlayerA),
		)

		if got := rig.router.GetActivePlayer(); got != scPlayerA {
			t.Fatalf("另一家暂停保持时，根应仍停在 %q 上，实得 %q", scPlayerA, got)
		}

		// 根 WS：waiting_song 后派生 player_clear(player=A)，不配 player_switch
		rootWSEvs := expectTypes(t, "根 WS", rig.root.ws, player.EventStatusUpdate, player.EventPlayerClear)
		if len(rootWSEvs) == 2 {
			assertPlayerClearEvt(t, "根 WS 派生 player_clear", rootWSEvs[1], scPlayerA)
		}
		// 根两条 SSE：各一条 in-band 清除（player=A）
		if evs := expectTypes(t, "根 lyric_update-SSE", rig.root.lyric, player.EventLyricUpdate); len(evs) == 1 {
			assertLyricClearEvt(t, "根 lyric_update-SSE", evs[0], scPlayerA, 95, 95+slack, testDuration)
		}
		if evs := expectTypes(t, "根 song_info-SSE", rig.root.song, player.EventSongInfoUpdate); len(evs) == 1 {
			assertSongClearEvt(t, "根 song_info-SSE", evs[0], scPlayerA)
		}

		// 专属 WS 与两条 SSE：与上同
		perWSEvs := expectTypes(t, "专属 WS", rig.a.ws, player.EventStatusUpdate, player.EventPlayerClear)
		if len(perWSEvs) == 2 {
			assertPlayerClearEvt(t, "专属 WS player_clear", perWSEvs[1], scPlayerA)
		}
		if evs := expectTypes(t, "专属 lyric_update-SSE", rig.a.lyric, player.EventLyricUpdate); len(evs) == 1 {
			assertLyricClearEvt(t, "专属 lyric_update-SSE", evs[0], scPlayerA, 95, 95+slack, testDuration)
		}
		if evs := expectTypes(t, "专属 song_info-SSE", rig.a.song, player.EventSongInfoUpdate); len(evs) == 1 {
			assertSongClearEvt(t, "专属 song_info-SSE", evs[0], scPlayerA)
		}

		// 暂停保持的 B 与不相干播放器零事件
		rig.b.expectEmpty(t, "暂停保持的 B")
		rig.other.expectEmpty(t, "不相干播放器")

		// 根 HTTP 与专属 HTTP：player=A，三端点 {}，status_update 为 waiting_song
		assertFourEndpointsClearedWithStatus(t, rig.srv, "", scPlayerA, "waiting_song", player.ReasonNoSong)
		assertFourEndpointsClearedWithStatus(t, rig.srv, scPlayerA, scPlayerA, "waiting_song", player.ReasonNoSong)
	})

	t.Run("非活跃播放器真停播", func(t *testing.T) {
		rig := newStopClearRig([]string{scPlayerA, scPlayerB, scPlayerC}, nil)
		// A 先播，B 后播抢到 activePlayer；A 成为非活跃播放器
		rig.announcePlaying(scPlayerA, oldSong, 60)
		time.Sleep(2 * time.Millisecond)
		rig.announcePlaying(scPlayerB, newSong, 20)
		if got := rig.router.GetActivePlayer(); got != scPlayerB {
			t.Fatalf("前置：activePlayer 应为 %q，实得 %q", scPlayerB, got)
		}
		rig.drainAll()

		rig.send(
			statusReasonEvt(scPlayerA, "waiting_song", player.ReasonNoSong, "没有歌曲"),
			clearEvt(scPlayerA),
		)

		// 根 WS、根 SSE 无任何事件
		rig.root.expectEmpty(t, "根出口")

		// 根 HTTP 四端点照旧反映活跃播放器 B（非空）
		for _, ep := range []struct {
			name string
			h    http.HandlerFunc
		}{
			{"song_info", rig.srv.handleSongInfo("")},
			{"all_lyrics", rig.srv.handleAllLyrics("")},
			{"lyric_update", rig.srv.handleLyricUpdate("")},
			{"status_update", rig.srv.handleStatusUpdate("")},
		} {
			snap := getHTTPSnapshot(t, ep.h, "/"+ep.name)
			if snap.Player != scPlayerB || string(snap.Data) == "{}" {
				t.Errorf("根 HTTP /%s 应照旧反映 %q 且非空，实得 player=%q data=%s", ep.name, scPlayerB, snap.Player, snap.Data)
			}
		}

		// 停播者 A 的专属出口收清除
		perWSEvs := expectTypes(t, "A 专属 WS", rig.a.ws, player.EventStatusUpdate, player.EventPlayerClear)
		if len(perWSEvs) == 2 {
			assertPlayerClearEvt(t, "A 专属 WS player_clear", perWSEvs[1], scPlayerA)
		}
		if evs := expectTypes(t, "A 专属 lyric_update-SSE", rig.a.lyric, player.EventLyricUpdate); len(evs) == 1 {
			assertLyricClearEvt(t, "A 专属 lyric_update-SSE", evs[0], scPlayerA, 60, 60+slack, testDuration)
		}
		if evs := expectTypes(t, "A 专属 song_info-SSE", rig.a.song, player.EventSongInfoUpdate); len(evs) == 1 {
			assertSongClearEvt(t, "A 专属 song_info-SSE", evs[0], scPlayerA)
		}
		assertFourEndpointsClearedWithStatus(t, rig.srv, scPlayerA, scPlayerA, "waiting_song", player.ReasonNoSong)

		// 活跃播放器 B 与不相干播放器零事件
		rig.b.expectEmpty(t, "活跃播放器 B")
		rig.other.expectEmpty(t, "不相干播放器")
	})
}

// TestStopClearMetadataUnavailable 钉住 standby / metadata_unavailable（曲目在而读不到曲目信息）与
// waiting_song / window_not_open 同属真停播，接 clear_song_data 时照常向专属出口（及仍指向它的根）推清除。
func TestStopClearMetadataUnavailable(t *testing.T) {
	cases := []struct {
		name   string
		status string
		reason string
	}{
		{"standby/metadata_unavailable", "standby", player.ReasonMetadataUnavailable},
		{"waiting_song/window_not_open", "waiting_song", player.ReasonWindowNotOpen},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rig := newStopClearRig([]string{scPlayerA, scPlayerB, scPlayerC}, nil)
			// B 暂停保持，使 A 真停播后根仍停在 A 上，同时验专属与根两条线
			rig.announcePlaying(scPlayerB, newSong, 10)
			rig.send(statusReasonEvt(scPlayerB, "paused", "", newSong+" - 某人"))
			rig.announcePlaying(scPlayerA, oldSong, 45)
			rig.drainAll()

			rig.send(
				statusReasonEvt(scPlayerA, c.status, c.reason, "不可用"),
				clearEvt(scPlayerA),
			)

			rootWSEvs := expectTypes(t, "根 WS", rig.root.ws, player.EventStatusUpdate, player.EventPlayerClear)
			if len(rootWSEvs) == 2 {
				assertPlayerClearEvt(t, "根 WS player_clear", rootWSEvs[1], scPlayerA)
			}
			if evs := expectTypes(t, "根 lyric_update-SSE", rig.root.lyric, player.EventLyricUpdate); len(evs) == 1 {
				assertLyricClearEvt(t, "根 lyric_update-SSE", evs[0], scPlayerA, 45, 45+slack, testDuration)
			}
			if evs := expectTypes(t, "根 song_info-SSE", rig.root.song, player.EventSongInfoUpdate); len(evs) == 1 {
				assertSongClearEvt(t, "根 song_info-SSE", evs[0], scPlayerA)
			}

			perWSEvs := expectTypes(t, "专属 WS", rig.a.ws, player.EventStatusUpdate, player.EventPlayerClear)
			if len(perWSEvs) == 2 {
				assertPlayerClearEvt(t, "专属 WS player_clear", perWSEvs[1], scPlayerA)
			}
			if evs := expectTypes(t, "专属 lyric_update-SSE", rig.a.lyric, player.EventLyricUpdate); len(evs) == 1 {
				assertLyricClearEvt(t, "专属 lyric_update-SSE", evs[0], scPlayerA, 45, 45+slack, testDuration)
			}
			if evs := expectTypes(t, "专属 song_info-SSE", rig.a.song, player.EventSongInfoUpdate); len(evs) == 1 {
				assertSongClearEvt(t, "专属 song_info-SSE", evs[0], scPlayerA)
			}
			rig.other.expectEmpty(t, "不相干播放器")
		})
	}
}

// TestStopClearAcrossDisconnectAndReconnect 钉住第 3 轮设计审的反例（关闭无活跃隐藏）：
// 宣告歌曲 → standby/cdp_disconnected → clear_song_data（清空缓存但不清专属画面）→ 重连后
// waiting_song/no_song → clear_song_data（专属 WS 与两条 SSE 各收一次清除）→ 再一次 waiting_song +
// clear_song_data（不再推）。
//
// 变异自证（实测）：UpdatePlayerState 在 clear_song_data 时复位 ShowsSong，重连即无歌这一步收不到清除而红；
// NotifySubscribers 推完派生清除不复位 ShowsSong，再次真停播多推一遍清除而红。
func TestStopClearAcrossDisconnectAndReconnect(t *testing.T) {
	rig := newStopClearRig([]string{scPlayerA, scPlayerC}, nil)
	rig.srv.SetIdleHideResolver(func(string) int { return 0 })
	rig.announcePlaying(scPlayerA, oldSong, 70)
	rig.drainAll()

	// 1. 断开 + 清缓存：专属出口按 2026-10-22 决定不推清除，但缓存已清空
	rig.send(
		statusReasonEvt(scPlayerA, "standby", player.ReasonCDPDisconnected, "连接断开"),
		clearEvt(scPlayerA),
	)
	expectTypes(t, "断开时专属 WS", rig.a.ws, player.EventStatusUpdate)
	expectTypes(t, "断开时专属 lyric_update-SSE", rig.a.lyric)
	expectTypes(t, "断开时专属 song_info-SSE", rig.a.song)
	rig.root.drain() // 根已在断开时走路由清屏，此处清掉

	// 2. 重连后第一拍即无歌：缓存已空，但专属画面仍停在上一首，必须各推一次清除
	rig.send(
		statusReasonEvt(scPlayerA, "waiting_song", player.ReasonNoSong, "没有歌曲"),
		clearEvt(scPlayerA),
	)
	perWSEvs := expectTypes(t, "重连即无歌 专属 WS", rig.a.ws, player.EventStatusUpdate, player.EventPlayerClear)
	if len(perWSEvs) == 2 {
		assertPlayerClearEvt(t, "重连即无歌 专属 WS player_clear", perWSEvs[1], scPlayerA)
	}
	if evs := expectTypes(t, "重连即无歌 专属 lyric_update-SSE", rig.a.lyric, player.EventLyricUpdate); len(evs) == 1 {
		assertLyricClearEvt(t, "重连即无歌 专属 lyric_update-SSE", evs[0], scPlayerA, 0, 0, 0)
	}
	if evs := expectTypes(t, "重连即无歌 专属 song_info-SSE", rig.a.song, player.EventSongInfoUpdate); len(evs) == 1 {
		assertSongClearEvt(t, "重连即无歌 专属 song_info-SSE", evs[0], scPlayerA)
	}
	rig.root.expectEmpty(t, "重连即无歌 根出口")

	// 3. 再一次 waiting_song + clear_song_data：已清除过，不再推
	rig.send(
		statusReasonEvt(scPlayerA, "waiting_song", player.ReasonNoSong, "没有歌曲"),
		clearEvt(scPlayerA),
	)
	expectTypes(t, "再次无歌 专属 WS", rig.a.ws, player.EventStatusUpdate)
	expectTypes(t, "再次无歌 专属 lyric_update-SSE", rig.a.lyric)
	expectTypes(t, "再次无歌 专属 song_info-SSE", rig.a.song)
	rig.root.expectEmpty(t, "再次无歌 根出口")
	rig.other.expectEmpty(t, "不相干播放器")
}

// TestStopClearNotSentOnExitOrDisconnect 钉住 2026-10-22 决定：播放器退出或连接断开（waiting_process
// 任意 reason，以及 standby 除 metadata_unavailable 外的全部 reason）后接 clear_song_data，专属 WS 与
// 专属两条 SSE 不收到任何清除。
//
// 变异自证（实测）：isTrueStop 接受 cdp_disconnected，「standby/cdp_disconnected」一例红。
func TestStopClearNotSentOnExitOrDisconnect(t *testing.T) {
	cases := []struct {
		name   string
		status string
		reason string
	}{
		{"waiting_process/process_exited", "waiting_process", player.ReasonProcessExited},
		{"waiting_process/process_not_running", "waiting_process", player.ReasonProcessNotRunning},
		{"standby/cdp_disconnected", "standby", player.ReasonCDPDisconnected},
		{"standby/version_unsupported", "standby", player.ReasonVersionUnsupported},
		{"standby/no_admin", "standby", player.ReasonNoAdmin},
		{"standby/install_not_found", "standby", player.ReasonInstallNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rig := newStopClearRig([]string{scPlayerA, scPlayerB, scPlayerC}, nil)
			// B 保持 holding 使根仍停在 A 上，确保即使根仍指向 A，退出/断开也不触发派生清除
			rig.announcePlaying(scPlayerB, newSong, 15)
			rig.send(statusReasonEvt(scPlayerB, "paused", "", newSong+" - 某人"))
			rig.announcePlaying(scPlayerA, oldSong, 55)
			rig.drainAll()

			rig.send(
				statusReasonEvt(scPlayerA, c.status, c.reason, "退出或断开"),
				clearEvt(scPlayerA),
			)

			// 专属 WS 只收到 status_update，专属两条 SSE 无任何事件
			expectTypes(t, "专属 WS", rig.a.ws, player.EventStatusUpdate)
			expectTypes(t, "专属 lyric_update-SSE", rig.a.lyric)
			expectTypes(t, "专属 song_info-SSE", rig.a.song)
			// 根也不推派生清除（根仍停在 A 上，只收到 status_update）
			expectTypes(t, "根 WS", rig.root.ws, player.EventStatusUpdate)
			expectTypes(t, "根 lyric_update-SSE", rig.root.lyric)
			expectTypes(t, "根 song_info-SSE", rig.root.song)
			rig.other.expectEmpty(t, "不相干播放器")
		})
	}
}

// TestStopClearNeverShownSong 钉住从未推过歌曲时，waiting_song → clear_song_data 不推任何清除。
func TestStopClearNeverShownSong(t *testing.T) {
	rig := newStopClearRig([]string{scPlayerA, scPlayerC}, nil)
	rig.drainAll()

	rig.send(
		statusReasonEvt(scPlayerA, "waiting_song", player.ReasonNoSong, "没有歌曲"),
		clearEvt(scPlayerA),
	)

	expectTypes(t, "专属 WS", rig.a.ws, player.EventStatusUpdate)
	expectTypes(t, "专属 lyric_update-SSE", rig.a.lyric)
	expectTypes(t, "专属 song_info-SSE", rig.a.song)
	rig.root.expectEmpty(t, "根出口")
	rig.other.expectEmpty(t, "不相干播放器")
}

// TestStopClearRepeatNoDuplicate 钉住同一首真停播之后，重复的 clear_song_data 不再推清除。
//
// 变异自证（实测）：NotifySubscribers 推完清除不复位 ShowsSong，重复 clear_song_data 时专属多收清除而红。
func TestStopClearRepeatNoDuplicate(t *testing.T) {
	rig := newStopClearRig([]string{scPlayerA, scPlayerC}, nil)
	rig.announcePlaying(scPlayerA, oldSong, 40)
	rig.drainAll()

	rig.send(
		statusReasonEvt(scPlayerA, "waiting_song", player.ReasonNoSong, "没有歌曲"),
		clearEvt(scPlayerA),
	)
	expectTypes(t, "首次真停播 专属 WS", rig.a.ws, player.EventStatusUpdate, player.EventPlayerClear)
	expectTypes(t, "首次真停播 专属 lyric_update-SSE", rig.a.lyric, player.EventLyricUpdate)
	expectTypes(t, "首次真停播 专属 song_info-SSE", rig.a.song, player.EventSongInfoUpdate)
	rig.root.drain()

	// 重复的 clear_song_data（模拟外层循环每 2～3s 补发）
	rig.send(clearEvt(scPlayerA), clearEvt(scPlayerA))

	rig.a.expectEmpty(t, "重复 clear_song_data 专属出口")
	rig.root.expectEmpty(t, "重复 clear_song_data 根出口")
}

// TestStopClearWithIdleHide 钉住无活跃隐藏与真停播清除的交互：
//   - 子例 A：活跃播放器暂停 → sweepIdleHide 隐藏专属通道（专属已收清除），同时另一家暂停保持使根仍停在它上面 →
//     真停播 → 专属出口不重复推清除，根出口照规则推清除（WS + 两条 SSE）。
//   - 子例 B：专属通道隐藏 → 经 playback_resume（或 playing 状态）解除隐藏并补发整套缓存 → 随后真停播 →
//     专属出口正常推清除。
//
// 变异自证（均实测）：
//   - sweepIdleHide 不复位 ShowsSong → 子例 A 专属出口重复收到清除而红；
//   - UpdatePlayerState 仅按 ShowsSong 门控 rootCandidate（忽略 RootShowsSong）→ 子例 A 根出口收不到清除而红；
//   - NotifySubscribers 补发整套时不置 ShowsSong → 子例 B 专属出口收不到清除而红。
func TestStopClearWithIdleHide(t *testing.T) {
	t.Run("专属已无活跃隐藏且根仍在保持", func(t *testing.T) {
		rig := newStopClearRig([]string{scPlayerA, scPlayerB, scPlayerC}, nil)
		// 只给 A 开无活跃隐藏，B 保持在 paused holding
		rig.srv.SetIdleHideResolver(func(name string) int {
			if name == scPlayerA {
				return 15
			}
			return 0
		})
		rig.announcePlaying(scPlayerB, newSong, 20)
		rig.send(statusReasonEvt(scPlayerB, "paused", "", newSong+" - 某人"))
		rig.announcePlaying(scPlayerA, oldSong, 100)
		rig.send(statusReasonEvt(scPlayerA, "paused", "", oldSong+" - 某人"))
		rig.drainAll()

		// 隐藏 A 的专属通道（专属收到一次无活跃隐藏清除，缓存保留）
		rig.srv.sweepIdleHide(time.Now().Add(20 * time.Second))
		if !rig.srv.playerStates[scPlayerA].IdleHidden {
			t.Fatal("前置：A 的专属通道应已隐藏")
		}
		expectTypes(t, "隐藏时 A 专属 WS", rig.a.ws, player.EventPlayerClear)
		expectTypes(t, "隐藏时 A 专属 lyric_update-SSE", rig.a.lyric, player.EventLyricUpdate)
		expectTypes(t, "隐藏时 A 专属 song_info-SSE", rig.a.song, player.EventSongInfoUpdate)
		rig.root.expectEmpty(t, "隐藏时根出口不受影响")

		// A 真停播：waiting_song 解除隐藏但不触发补发；clear_song_data 时专属不重复推，根仍停在 A 上照规则推清除
		rig.send(
			statusReasonEvt(scPlayerA, "waiting_song", player.ReasonNoSong, "没有歌曲"),
			clearEvt(scPlayerA),
		)

		// 专属：只收 status_update，不再收 player_clear 与 SSE 清除
		expectTypes(t, "真停播 A 专属 WS", rig.a.ws, player.EventStatusUpdate)
		expectTypes(t, "真停播 A 专属 lyric_update-SSE", rig.a.lyric)
		expectTypes(t, "真停播 A 专属 song_info-SSE", rig.a.song)

		// 根：仍停在 A 上，WS 收 waiting_song + player_clear(player=A)，两条 SSE 收带 100s 的 in-band 清除
		rootWSEvs := expectTypes(t, "真停播 根 WS", rig.root.ws, player.EventStatusUpdate, player.EventPlayerClear)
		if len(rootWSEvs) == 2 {
			assertPlayerClearEvt(t, "真停播 根 WS player_clear", rootWSEvs[1], scPlayerA)
		}
		if evs := expectTypes(t, "真停播 根 lyric_update-SSE", rig.root.lyric, player.EventLyricUpdate); len(evs) == 1 {
			assertLyricClearEvt(t, "真停播 根 lyric_update-SSE", evs[0], scPlayerA, 100, 100+slack, testDuration)
		}
		if evs := expectTypes(t, "真停播 根 song_info-SSE", rig.root.song, player.EventSongInfoUpdate); len(evs) == 1 {
			assertSongClearEvt(t, "真停播 根 song_info-SSE", evs[0], scPlayerA)
		}
		rig.other.expectEmpty(t, "不相干播放器")
	})

	for _, resumeCase := range []struct {
		name    string
		trigger player.Event
		wantPos float32
	}{
		{"经 playback_resume 解除隐藏后真停播", timeEvt(scPlayerA, player.EventPlaybackResume, 120), 120},
		{"经 playing 状态解除隐藏后真停播", statusReasonEvt(scPlayerA, "playing", "", oldSong+" - 某人"), 100},
	} {
		t.Run(resumeCase.name, func(t *testing.T) {
			rig := newStopClearRig([]string{scPlayerA, scPlayerC}, nil)
			rig.srv.SetIdleHideResolver(func(string) int { return 15 })
			rig.announcePlaying(scPlayerA, oldSong, 100)
			rig.send(statusReasonEvt(scPlayerA, "paused", "", oldSong+" - 某人"))
			rig.drainAll()

			rig.srv.sweepIdleHide(time.Now().Add(20 * time.Second))
			if !rig.srv.playerStates[scPlayerA].IdleHidden {
				t.Fatal("前置：通道应已隐藏")
			}
			rig.drainAll()

			// 解除隐藏并补发整套缓存，ShowsSong 同步置位
			rig.send(resumeCase.trigger)
			rig.drainAll()

			// 随后真停播：专属出口必须正常推清除
			rig.send(
				statusReasonEvt(scPlayerA, "waiting_song", player.ReasonNoSong, "没有歌曲"),
				clearEvt(scPlayerA),
			)
			perWSEvs := expectTypes(t, "真停播 A 专属 WS", rig.a.ws, player.EventStatusUpdate, player.EventPlayerClear)
			if len(perWSEvs) == 2 {
				assertPlayerClearEvt(t, "真停播 A 专属 WS player_clear", perWSEvs[1], scPlayerA)
			}
			if evs := expectTypes(t, "真停播 A 专属 lyric_update-SSE", rig.a.lyric, player.EventLyricUpdate); len(evs) == 1 {
				assertLyricClearEvt(t, "真停播 A 专属 lyric_update-SSE", evs[0], scPlayerA, resumeCase.wantPos, resumeCase.wantPos+slack, testDuration)
			}
			if evs := expectTypes(t, "真停播 A 专属 song_info-SSE", rig.a.song, player.EventSongInfoUpdate); len(evs) == 1 {
				assertSongClearEvt(t, "真停播 A 专属 song_info-SSE", evs[0], scPlayerA)
			}
		})
	}
}

// TestStopClearSodamusicIdleHideDisabled 钉住第 1 轮设计审反例：sodamusic-idle-hide 为 0（默认关闭）时，
// 真停播当拍专属歌曲 SSE 收到空 song_info_update，专属歌词 SSE 收到 index:-1，专属 WS 收到 player_clear。
func TestStopClearSodamusicIdleHideDisabled(t *testing.T) {
	rig := newStopClearRig([]string{"sodamusic", scPlayerC}, nil)
	rig.srv.SetIdleHideResolver(func(string) int { return 0 })
	rig.announcePlaying("sodamusic", oldSong, 33)
	rig.drainAll()

	rig.send(
		statusReasonEvt("sodamusic", "waiting_song", player.ReasonNoSong, "没有歌曲"),
		clearEvt("sodamusic"),
	)

	if evs := expectTypes(t, "sodamusic 专属 song_info-SSE", rig.a.song, player.EventSongInfoUpdate); len(evs) == 1 {
		assertSongClearEvt(t, "sodamusic 专属 song_info-SSE", evs[0], "sodamusic")
	}
	if evs := expectTypes(t, "sodamusic 专属 lyric_update-SSE", rig.a.lyric, player.EventLyricUpdate); len(evs) == 1 {
		assertLyricClearEvt(t, "sodamusic 专属 lyric_update-SSE", evs[0], "sodamusic", 33, 33+slack, testDuration)
	}
	perWSEvs := expectTypes(t, "sodamusic 专属 WS", rig.a.ws, player.EventStatusUpdate, player.EventPlayerClear)
	if len(perWSEvs) == 2 {
		assertPlayerClearEvt(t, "sodamusic 专属 WS player_clear", perWSEvs[1], "sodamusic")
	}
}

// TestStopClearWhilePlayingOrPaused 钉住仍处于上一首的 playing / paused 时收到 clear_song_data（如 kuwo 换歌
// 先清空再宣告新歌），不推任何清除；且 ShowsSong 保持置位，后续真停播仍能正常清屏。
func TestStopClearWhilePlayingOrPaused(t *testing.T) {
	for _, st := range []string{"playing", "paused"} {
		t.Run(st, func(t *testing.T) {
			rig := newStopClearRig([]string{"kuwo", scPlayerC}, nil)
			rig.announcePlaying("kuwo", oldSong, 50)
			if st == "paused" {
				rig.send(statusReasonEvt("kuwo", "paused", "", oldSong+" - 某人"))
			}
			rig.drainAll()

			rig.send(clearEvt("kuwo"))

			rig.a.expectEmpty(t, "换歌清空 专属出口")
			rig.root.expectEmpty(t, "换歌清空 根出口")
			rig.other.expectEmpty(t, "换歌清空 不相干播放器")
		})
	}
}

// TestStopClearFullChannelDoesNotBlock 钉住 notifyStopClear 全部发送点非阻塞（AGENTS §2.1 承重不变量）：
// 订阅通道已满时丢弃，NotifySubscribers 照常返回。
//
// 变异自证（实测）：把 sendClearToSub 里 player_clear 发送改为阻塞发送 `sub.ch <- clearEvt`，本用例 2s 超时而红。
func TestStopClearFullChannelDoesNotBlock(t *testing.T) {
	rig := newStopClearRig([]string{scPlayerA, scPlayerB, scPlayerC}, nil)
	rig.announcePlaying(scPlayerB, newSong, 10)
	rig.send(statusReasonEvt(scPlayerB, "paused", "", newSong+" - 某人"))
	rig.announcePlaying(scPlayerA, oldSong, 50)
	rig.drainAll()

	// 先发 waiting_song，再塞满所有目标订阅者通道，随后发 clear_song_data
	rig.send(statusReasonEvt(scPlayerA, "waiting_song", player.ReasonNoSong, "没有歌曲"))
	rig.drainAll()
	for _, sub := range []*subscriber{rig.root.ws, rig.root.lyric, rig.root.song, rig.a.ws, rig.a.lyric, rig.a.song} {
		for range cap(sub.ch) {
			sub.ch <- WSEvent{Type: "filler"}
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		rig.send(clearEvt(scPlayerA))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("订阅通道已满时 notifyStopClear 被阻塞（违反 AGENTS §2.1）")
	}
}

// TestStopClearRootAfterIdleHideAndDisconnect 钉住 r1 P2（root_clear_after_idle_hide）组合反例：
// prior-player-expire=0、仅给 A 开启无活跃隐藏；B 播放后暂停，A 播放后暂停；sweepIdleHide 隐藏 A 专属通道
// （根仍保持 A）；A 发 standby/cdp_disconnected + clear_song_data（清缓存）；A 重连发 waiting_song/no_song +
// clear_song_data → 根 WS 与根两条 SSE 均收到清除（专属不再重复收）；再发一次 waiting_song + clear_song_data →
// 根与专属均不再收。另覆盖「根切走再切回一个暂停播放器后真停播仍能清根」。
//
// 变异自证（实测）：去掉 PlayerState.RootShowsSong 改回 ps.ShowsSong || hasCache，隐藏+断线+重连无歌子例中
// 根出口收不到清除而红；NotifySubscribersFullState 不置 RootShowsSong，「切走再切回暂停播放器后真停播」子例红。
func TestStopClearRootAfterIdleHideAndDisconnect(t *testing.T) {
	t.Run("隐藏后断线清缓存再重连无歌仍清根", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.PriorPlayer = nil
		cfg.PriorPlayerExpire = 0
		players := []string{scPlayerA, scPlayerB, scPlayerC}
		srv := NewServer(players)
		srv.SetIdleHideResolver(func(name string) int {
			if name == scPlayerA {
				return 15
			}
			return 0
		})
		r := NewRouter(&cfg, srv, players)
		rig := &stopClearRig{
			srv:    srv,
			router: r,
			root:   subscribeTriple(srv, ""),
			a:      subscribeTriple(srv, scPlayerA),
			b:      subscribeTriple(srv, scPlayerB),
			other:  subscribeTriple(srv, scPlayerC),
		}

		// B 播放后暂停（保持 holding），A 播放后暂停（当前 activePlayer 为 A）
		rig.announcePlaying(scPlayerB, newSong, 20)
		rig.send(statusReasonEvt(scPlayerB, "paused", "", newSong+" - 某人"))
		rig.announcePlaying(scPlayerA, oldSong, 80)
		rig.send(statusReasonEvt(scPlayerA, "paused", "", oldSong+" - 某人"))
		if got := rig.router.GetActivePlayer(); got != scPlayerA {
			t.Fatalf("前置：activePlayer 应为 %q，实得 %q", scPlayerA, got)
		}
		rig.drainAll()

		// 1. sweepIdleHide 隐藏 A 专属通道（A.ShowsSong 复位，A.RootShowsSong 必须保持）
		rig.srv.sweepIdleHide(time.Now().Add(20 * time.Second))
		if !rig.srv.playerStates[scPlayerA].IdleHidden {
			t.Fatal("前置：A 的专属通道应已隐藏")
		}
		expectTypes(t, "隐藏时 A 专属 WS", rig.a.ws, player.EventPlayerClear)
		expectTypes(t, "隐藏时 A 专属 lyric_update-SSE", rig.a.lyric, player.EventLyricUpdate)
		expectTypes(t, "隐藏时 A 专属 song_info-SSE", rig.a.song, player.EventSongInfoUpdate)
		rig.root.expectEmpty(t, "隐藏时根出口不受影响")

		// 2. A 断线 + clear_song_data（缓存被清空，但 B 仍在 holding，根仍停在 A 上；退出/断开不推清除）
		rig.send(
			statusReasonEvt(scPlayerA, "standby", player.ReasonCDPDisconnected, "连接断开"),
			clearEvt(scPlayerA),
		)
		if got := rig.router.GetActivePlayer(); got != scPlayerA {
			t.Fatalf("前置：B 暂停保持期间，A 断线后根应仍停在 %q，实得 %q", scPlayerA, got)
		}
		expectTypes(t, "断线时根 WS", rig.root.ws, player.EventStatusUpdate)
		expectTypes(t, "断线时根 lyric_update-SSE", rig.root.lyric)
		expectTypes(t, "断线时根 song_info-SSE", rig.root.song)
		expectTypes(t, "断线时 A 专属 WS", rig.a.ws, player.EventStatusUpdate)
		expectTypes(t, "断线时 A 专属 lyric_update-SSE", rig.a.lyric)
		expectTypes(t, "断线时 A 专属 song_info-SSE", rig.a.song)

		// 3. A 重连无歌：缓存已空且 A.ShowsSong=false，但 A.RootShowsSong=true，根必须收到清除，专属不再重复收
		rig.send(
			statusReasonEvt(scPlayerA, "waiting_song", player.ReasonNoSong, "没有歌曲"),
			clearEvt(scPlayerA),
		)
		rootWSEvs := expectTypes(t, "重连无歌 根 WS", rig.root.ws, player.EventStatusUpdate, player.EventPlayerClear)
		if len(rootWSEvs) == 2 {
			assertPlayerClearEvt(t, "重连无歌 根 WS player_clear", rootWSEvs[1], scPlayerA)
		}
		if evs := expectTypes(t, "重连无歌 根 lyric_update-SSE", rig.root.lyric, player.EventLyricUpdate); len(evs) == 1 {
			assertLyricClearEvt(t, "重连无歌 根 lyric_update-SSE", evs[0], scPlayerA, 0, 0, 0)
		}
		if evs := expectTypes(t, "重连无歌 根 song_info-SSE", rig.root.song, player.EventSongInfoUpdate); len(evs) == 1 {
			assertSongClearEvt(t, "重连无歌 根 song_info-SSE", evs[0], scPlayerA)
		}
		expectTypes(t, "重连无歌 A 专属 WS（不重复清屏）", rig.a.ws, player.EventStatusUpdate)
		expectTypes(t, "重连无歌 A 专属 lyric_update-SSE", rig.a.lyric)
		expectTypes(t, "重连无歌 A 专属 song_info-SSE", rig.a.song)

		// 4. 再发一次 waiting_song + clear_song_data：根与专属均不再收清除
		rig.send(
			statusReasonEvt(scPlayerA, "waiting_song", player.ReasonNoSong, "没有歌曲"),
			clearEvt(scPlayerA),
		)
		expectTypes(t, "再次无歌 根 WS", rig.root.ws, player.EventStatusUpdate)
		expectTypes(t, "再次无歌 根 lyric_update-SSE", rig.root.lyric)
		expectTypes(t, "再次无歌 根 song_info-SSE", rig.root.song)
		expectTypes(t, "再次无歌 A 专属 WS", rig.a.ws, player.EventStatusUpdate)
		expectTypes(t, "再次无歌 A 专属 lyric_update-SSE", rig.a.lyric)
		expectTypes(t, "再次无歌 A 专属 song_info-SSE", rig.a.song)
		rig.b.expectEmpty(t, "B 专属出口")
		rig.other.expectEmpty(t, "不相干播放器")
	})

	t.Run("根切走再切回已缓存歌曲的播放器后真停播仍清根", func(t *testing.T) {
		// 优先组 C 抢走根（复位 A.RootShowsSong），随后 C 释放并切回正在播歌的 A（未发新歌曲事件，
		// 仅由 NotifySubscribersFullState 恢复 A.RootShowsSong），随后 A 在 B 保持期间真停播，根仍应收到清除。
		rig := newStopClearRig([]string{scPlayerA, scPlayerB, scPlayerC}, []string{scPlayerC})
		rig.announcePlaying(scPlayerB, newSong, 10)
		rig.send(statusReasonEvt(scPlayerB, "paused", "", newSong+" - 某人"))
		rig.announcePlaying(scPlayerA, oldSong, 60)
		// C 抢走根 → SetActivePlayer(C) 复位 A.RootShowsSong
		rig.announcePlaying(scPlayerC, "优先歌", 5)
		if got := rig.router.GetActivePlayer(); got != scPlayerC {
			t.Fatalf("前置：优先组 C 应抢到根，实得 %q", got)
		}
		// C 停播释放 → 路由切回仍在 playing 的 A，经 NotifySubscribersFullState 推 A 完整缓存
		// 注意：evaluatePriorGroup 释放时会清空普通组 holding（B 变 inert），所以先让 B 再暂停一次建立 holding
		rig.send(statusReasonEvt(scPlayerC, "waiting_song", player.ReasonNoSong, "无歌"), clearEvt(scPlayerC))
		if got := rig.router.GetActivePlayer(); got != scPlayerA {
			t.Fatalf("前置：C 释放后根应切回 A，实得 %q", got)
		}
		// 让 A 专属隐藏以剥离 A.ShowsSong，单独验证 A.RootShowsSong 由 NotifySubscribersFullState 恢复
		rig.srv.SetIdleHideResolver(func(name string) int {
			if name == scPlayerA {
				return 15
			}
			return 0
		})
		// B 先激活再暂停以形成 holding，同时 A 保持为 activePlayer（最后播放者优先：B 暂停后 A 重进或先让 B 暂停再让 C 释放？
		// 更直接：先暂停 A 隐藏它、再断线清缓存，只测 RootShowsSong 是否被 FullState 置为 true）
		rig.send(statusReasonEvt(scPlayerB, "playing", "", newSong+" - 某人"))
		rig.send(statusReasonEvt(scPlayerB, "paused", "", newSong+" - 某人"))
		// 此时 B 抢到根；再让 A 发 playing 切回 A（不发新歌名/歌词事件，完全依赖 FullState 置 A.RootShowsSong）
		rig.send(statusReasonEvt(scPlayerA, "paused", "", oldSong+" - 某人"))
		rig.send(statusReasonEvt(scPlayerA, "playing", "", oldSong+" - 某人"))
		if got := rig.router.GetActivePlayer(); got != scPlayerA {
			t.Fatalf("前置：activePlayer 应切回 A，实得 %q", got)
		}
		rig.send(statusReasonEvt(scPlayerA, "paused", "", oldSong+" - 某人"))
		rig.drainAll()

		// 隐藏 A 专属并断线清缓存，使 A.ShowsSong=false 且缓存为空，仅剩 A.RootShowsSong
		rig.srv.sweepIdleHide(time.Now().Add(20 * time.Second))
		rig.drainAll()
		rig.send(statusReasonEvt(scPlayerA, "standby", player.ReasonCDPDisconnected, "断开"), clearEvt(scPlayerA))
		rig.drainAll()

		// 重连无歌：必须靠 FullState 置位的 A.RootShowsSong 触发根清除
		rig.send(statusReasonEvt(scPlayerA, "waiting_song", player.ReasonNoSong, "无歌"), clearEvt(scPlayerA))
		rootWSEvs := expectTypes(t, "切回后再真停播 根 WS", rig.root.ws, player.EventStatusUpdate, player.EventPlayerClear)
		if len(rootWSEvs) == 2 {
			assertPlayerClearEvt(t, "切回后再真停播 根 WS player_clear", rootWSEvs[1], scPlayerA)
		}
		expectTypes(t, "切回后再真停播 根 lyric_update-SSE", rig.root.lyric, player.EventLyricUpdate)
		expectTypes(t, "切回后再真停播 根 song_info-SSE", rig.root.song, player.EventSongInfoUpdate)
	})
}

// assertRootEndsWithPlayer 验证在 A 停播清除与路由切到 B 交错后，根 WS 与根两条 SSE 的最终可见状态
// 都是 B 的歌曲数据，绝不出现「B 的完整状态之后紧跟 A 的 player_clear / SSE 清除」。
func assertRootEndsWithPlayer(t *testing.T, label string, root subTriple, wantPlayer string) {
	t.Helper()
	wsEvs := drainCh(root.ws.ch)
	if len(wsEvs) == 0 {
		t.Fatalf("%s 根 WS 无事件", label)
	}
	lastSwitchIdx := -1
	for i, e := range wsEvs {
		if e.Type == player.EventPlayerSwitch && e.Player == wantPlayer {
			lastSwitchIdx = i
		}
	}
	if lastSwitchIdx < 0 {
		t.Fatalf("%s 根 WS 缺少切到 %q 的 player_switch，实得 %+v", label, wantPlayer, wsEvs)
	}
	for _, e := range wsEvs[lastSwitchIdx+1:] {
		if e.Type == player.EventPlayerClear {
			t.Fatalf("%s 根 WS 在切到 %q 之后出现迟到的 player_clear（player=%q），把刚切上的 %q 清成空白！完整序列: %v",
				label, wantPlayer, e.Player, wantPlayer, evTypes(wsEvs))
		}
	}
	if last := wsEvs[len(wsEvs)-1]; last.Player != wantPlayer {
		t.Fatalf("%s 根 WS 末尾事件应属于 %q，实得 %+v", label, wantPlayer, last)
	}

	lyricEvs := drainCh(root.lyric.ch)
	if len(lyricEvs) == 0 {
		t.Fatalf("%s 根 lyric_update-SSE 无事件", label)
	}
	lastLyric := lyricEvs[len(lyricEvs)-1]
	if lu, ok := lastLyric.Data.(*LyricUpdate); !ok || lastLyric.Player != wantPlayer || lu.Index < 0 {
		t.Fatalf("%s 根 lyric_update-SSE 最终状态应为 %q 的真实歌词行，实得 player=%q data=%+v（全部 %d 条）",
			label, wantPlayer, lastLyric.Player, lastLyric.Data, len(lyricEvs))
	}

	songEvs := drainCh(root.song.ch)
	if len(songEvs) == 0 {
		t.Fatalf("%s 根 song_info-SSE 无事件", label)
	}
	lastSong := songEvs[len(songEvs)-1]
	if si, ok := lastSong.Data.(*SongInfoUpdate); !ok || lastSong.Player != wantPlayer || si.Name == "" {
		t.Fatalf("%s 根 song_info-SSE 最终状态应为 %q 的歌曲信息，实得 player=%q data=%+v（全部 %d 条）",
			label, wantPlayer, lastSong.Player, lastSong.Data, len(songEvs))
	}
}

// TestStopClearNoRaceWithRouteSwitch 钉住 r1 P2（stop_clear_route_race）：
// 优先组 A（当前活跃）与优先组 C（暂停保持）、普通组 B（正在播歌）。A 真停播发送 clear_song_data，
// 同时 watchExpire 优先组超时释放切到 B（SetActivePlayer(B) + NotifySubscribersFullState(A, B)）。
// 任何交错下，根 WS 与根两条 SSE 的最终状态必须都是 B 的歌曲数据，绝不能让 A 迟到的清除盖掉刚切上的 B。
//
// 变异自证（实测）：NotifySubscribers(EventClearSongData) 改回先 s.mu.Unlock() 再推 notifyStopClear，
// 「可控交错」子例 100% 稳定变红（B 的完整状态之后紧跟 A 的 player_clear 与 SSE 清除）。
func TestStopClearNoRaceWithRouteSwitch(t *testing.T) {
	setup := func(t *testing.T) *stopClearRig {
		t.Helper()
		// 优先组 [A, C]，普通组 [B]
		rig := newStopClearRig([]string{scPlayerA, scPlayerB, scPlayerC}, []string{scPlayerA, scPlayerC})
		// 普通组 B 正在播歌（有完整缓存）
		rig.announcePlaying(scPlayerB, newSong, 40)
		// 优先组 C 播放后暂停，形成优先组 holding
		rig.announcePlaying(scPlayerC, "优先C", 15)
		rig.send(statusReasonEvt(scPlayerC, "paused", "", "优先C - 某人"))
		// 优先组 A 播歌成为 activePlayer
		rig.announcePlaying(scPlayerA, oldSong, 70)
		if got := rig.router.GetActivePlayer(); got != scPlayerA {
			t.Fatalf("前置：activePlayer 应为 %q，实得 %q", scPlayerA, got)
		}
		// A 报 waiting_song：因 C 仍在优先组 holding，根仍停在 A 上
		rig.send(statusReasonEvt(scPlayerA, "waiting_song", player.ReasonNoSong, "没有歌曲"))
		if got := rig.router.GetActivePlayer(); got != scPlayerA {
			t.Fatalf("前置：C 保持期间根应仍停在 %q，实得 %q", scPlayerA, got)
		}
		rig.drainAll()
		// A 的 clear_song_data 先写缓存记下 stopClear，随后交由 NotifySubscribers 推送
		rig.srv.UpdatePlayerState(clearEvt(scPlayerA))
		rig.router.updateRouting(clearEvt(scPlayerA))
		return rig
	}

	expireToB := func(r *Router) {
		r.mu.Lock()
		forceGroupInert(r.priorStates)
		r.priorGroupPaused = false
		forceGroupInert(r.normalStates)
		r.normalGroupPaused = false
		r.evaluateNormalGroup()
		r.mu.Unlock()
	}

	t.Run("可控交错：路由切换不得插队在判定与推送之间", func(t *testing.T) {
		rig := setup(t)
		switchStarted := make(chan struct{})
		switchDone := make(chan struct{})
		rig.srv.onStopClearBeforeSub = func() {
			close(switchStarted)
			// 若 NotifySubscribers 已经释放 s.mu，expireToB 会立刻完成并关闭 switchDone，
			// 随后本调用会把过时的 A 清除推到 B 完整状态之后；若仍在 s.mu 锁内，expireToB 会被
			// s.mu 挡住直到本调用推完清除并解锁，此处短等超时后放行，绝不死锁。
			select {
			case <-switchDone:
			case <-time.After(80 * time.Millisecond):
			}
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			rig.srv.NotifySubscribers(clearEvt(scPlayerA), false)
		}()
		go func() {
			defer wg.Done()
			<-switchStarted
			expireToB(rig.router)
			close(switchDone)
		}()
		wg.Wait()

		assertRootEndsWithPlayer(t, "可控交错", rig.root, scPlayerB)
	})

	t.Run("真实并发压测（-race 无竞争且顺序始终合法）", func(t *testing.T) {
		for i := range 50 {
			rig := setup(t)
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				rig.srv.NotifySubscribers(clearEvt(scPlayerA), false)
			}()
			go func() {
				defer wg.Done()
				<-start
				expireToB(rig.router)
			}()
			close(start)
			wg.Wait()
			assertRootEndsWithPlayer(t, "并发轮次", rig.root, scPlayerB)
			_ = i
		}
	})
}
