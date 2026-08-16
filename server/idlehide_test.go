package server

// 本文件覆盖 issue #47 的 per-player 无活跃自动隐藏（sweepIdleHide / notifyPerPlayerIdleClear /
// perPlayerHidden / buildInitEvents 隐藏态）。这是 router 超时之外**第一个**盯住「隐藏」行为的门禁。
//
// 手法：sweepIdleHide 把「当前时刻」作为入参，故无需真实等待——用 now+Δ 模拟经过的空闲时长。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

func statusEvt(name, status string) player.Event {
	return player.Event{PlayerName: name, Type: player.EventStatusUpdate, Data: &player.StatusInfo{Status: status}}
}

func lyricEvt(name string, index int) player.Event {
	return player.Event{PlayerName: name, Type: player.EventLyricUpdate, Data: &player.LyricUpdate{Index: index, Text: "x"}}
}

// drainCh 非阻塞地取空订阅通道，返回收到的事件。
func drainCh(ch chan WSEvent) []WSEvent {
	var out []WSEvent
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestIdleHideSweepMarksAndPushesClear：非活跃达阈值后，通道被标记隐藏且 WS 订阅者恰好收到一条 player_clear。
func TestIdleHideSweepMarksAndPushesClear(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	s.SetIdleHideResolver(func(string) int { return 15 })
	sub := s.subscribe("cloudmusicv3", nil, true, "ws")

	// 曾活跃过、当前暂停（非播放）。UpdatePlayerState 只写缓存、不推订阅通道。
	s.UpdatePlayerState(statusEvt("cloudmusicv3", "paused"))
	s.UpdatePlayerState(lyricEvt("cloudmusicv3", 3))
	if evs := drainCh(sub.ch); len(evs) != 0 {
		t.Fatalf("UpdatePlayerState 不应向订阅通道推事件，实际 %+v", evs)
	}

	s.sweepIdleHide(time.Now().Add(20 * time.Second)) // 空闲 20s > 15s

	if !s.playerStates["cloudmusicv3"].IdleHidden {
		t.Fatal("非活跃达阈值应标记为 IdleHidden")
	}
	evs := drainCh(sub.ch)
	if len(evs) != 1 || evs[0].Type != player.EventPlayerClear || evs[0].Player != "cloudmusicv3" {
		t.Fatalf("WS 订阅者应恰好收到一条 player_clear(player=cloudmusicv3)，实际 %+v", evs)
	}

	// 一次性：再次巡检不应重复推送（已隐藏则跳过）。
	s.sweepIdleHide(time.Now().Add(40 * time.Second))
	if evs := drainCh(sub.ch); len(evs) != 0 {
		t.Fatalf("已隐藏后不应重复推送，实际 %+v", evs)
	}
}

// TestIdleHidePlayingGuard：status=="playing"（长间奏但歌未停）时绝不隐藏。
func TestIdleHidePlayingGuard(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	s.SetIdleHideResolver(func(string) int { return 15 })
	s.UpdatePlayerState(statusEvt("cloudmusicv3", "playing"))
	s.sweepIdleHide(time.Now().Add(20 * time.Second))
	if s.playerStates["cloudmusicv3"].IdleHidden {
		t.Fatal("播放中不应隐藏")
	}
}

// TestIdleHideDisabled：阈值 0 或未注入 resolver 时绝不隐藏。
func TestIdleHideDisabled(t *testing.T) {
	// 阈值 0
	s := NewServer([]string{"cloudmusicv3"})
	s.SetIdleHideResolver(func(string) int { return 0 })
	s.UpdatePlayerState(statusEvt("cloudmusicv3", "paused"))
	s.sweepIdleHide(time.Now().Add(20 * time.Second))
	if s.playerStates["cloudmusicv3"].IdleHidden {
		t.Fatal("阈值 0 时不应隐藏")
	}

	// 未注入 resolver（nil）
	s2 := NewServer([]string{"cloudmusicv3"})
	s2.UpdatePlayerState(statusEvt("cloudmusicv3", "paused"))
	s2.sweepIdleHide(time.Now().Add(20 * time.Second))
	if s2.playerStates["cloudmusicv3"].IdleHidden {
		t.Fatal("未注入 resolver 时不应隐藏")
	}
}

// TestIdleHideThresholdNotReached：未达阈值不隐藏；从未活跃过（LastEventAt 零值）不隐藏。
func TestIdleHideThresholdNotReached(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	s.SetIdleHideResolver(func(string) int { return 15 })

	// 从未出过事件：不处理
	s.sweepIdleHide(time.Now().Add(20 * time.Second))
	if s.playerStates["cloudmusicv3"].IdleHidden {
		t.Fatal("从未活跃过的通道不应被隐藏")
	}

	// 活跃后未达阈值：不隐藏
	s.UpdatePlayerState(statusEvt("cloudmusicv3", "paused"))
	s.sweepIdleHide(time.Now().Add(5 * time.Second))
	if s.playerStates["cloudmusicv3"].IdleHidden {
		t.Fatal("未达阈值不应隐藏")
	}
}

// TestIdleHideHTTPPerPlayerOnly：隐藏后 per-player 的四个 HTTP 端点都返回空；根路径不受影响（§3.4）。
// 四端点各喂缓存，使「未隐藏非空 / 隐藏后空」对每个都可证伪——删任一 perPlayerHidden 短路即红。
func TestIdleHideHTTPPerPlayerOnly(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	s.SetIdleHideResolver(func(string) int { return 15 })
	s.UpdatePlayerState(statusEvt("cloudmusicv3", "paused"))
	s.UpdatePlayerState(player.Event{PlayerName: "cloudmusicv3", Type: player.EventSongInfoUpdate, Data: &player.SongInfo{Name: "n", Singer: "s", Title: "t"}})
	s.UpdatePlayerState(player.Event{PlayerName: "cloudmusicv3", Type: player.EventAllLyrics, Data: &player.AllLyricsData{Title: "t", Lyrics: make([]player.LyricLine, 3)}})
	s.UpdatePlayerState(lyricEvt("cloudmusicv3", 3))

	handlers := []struct {
		name string
		h    http.HandlerFunc
	}{
		{"all_lyrics", s.handleAllLyrics("cloudmusicv3")},
		{"lyric_update", s.handleLyricUpdate("cloudmusicv3")},
		{"status_update", s.handleStatusUpdate("cloudmusicv3")},
		{"song_info", s.handleSongInfo("cloudmusicv3")},
	}
	for _, tc := range handlers {
		if httpEmpty(t, tc.h, "/cloudmusicv3/"+tc.name) {
			t.Errorf("未隐藏时 per-player %s 应返回缓存数据", tc.name)
		}
	}

	s.sweepIdleHide(time.Now().Add(20 * time.Second))

	for _, tc := range handlers {
		if !httpEmpty(t, tc.h, "/cloudmusicv3/"+tc.name) {
			t.Errorf("隐藏后 per-player %s 应返回空 data:{}", tc.name)
		}
	}

	// 根路径解析到 cloudmusicv3，但根绝不看 per-player 的 IdleHidden（§3.4）。
	s.SetActivePlayer("cloudmusicv3")
	if httpEmpty(t, s.handleLyricUpdate(""), "/lyric_update") {
		t.Error("根路径不应受 per-player 隐藏影响，应仍返回缓存")
	}
}

// TestIdleHideUnhideOnActivity：隐藏后收到新活跃事件应解除隐藏。
func TestIdleHideUnhideOnActivity(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	s.SetIdleHideResolver(func(string) int { return 15 })
	s.UpdatePlayerState(statusEvt("cloudmusicv3", "paused"))
	s.sweepIdleHide(time.Now().Add(20 * time.Second))
	if !s.playerStates["cloudmusicv3"].IdleHidden {
		t.Fatal("应先被隐藏")
	}
	s.UpdatePlayerState(lyricEvt("cloudmusicv3", 5))
	if s.playerStates["cloudmusicv3"].IdleHidden {
		t.Fatal("新活跃事件应解除隐藏")
	}
}

// TestIdleHideReconnectInitCleared：隐藏态下 per-player 重连的初始事件是 player_clear，而非缓存末行。
func TestIdleHideReconnectInitCleared(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	s.SetIdleHideResolver(func(string) int { return 15 })
	s.UpdatePlayerState(lyricEvt("cloudmusicv3", 3))
	s.sweepIdleHide(time.Now().Add(20 * time.Second))

	evs := s.buildInitEvents("cloudmusicv3", true)
	if len(evs) != 1 || evs[0].Type != player.EventPlayerClear || evs[0].Player != "cloudmusicv3" {
		t.Fatalf("隐藏态下 per-player 重连初始态应为单条 player_clear，实际 %+v", evs)
	}
}

// TestIdleHideSSEInBandForms：类型过滤的 SSE 走 in-band 清除——lyric_update-SSE 收 lyric_update(index:-1)、
// song_info-SSE 收空 song_info_update；WS 收 player_clear。三者与根清除的可见结果一致。
func TestIdleHideSSEInBandForms(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	s.SetIdleHideResolver(func(string) int { return 15 })
	wsSub := s.subscribe("cloudmusicv3", nil, true, "ws")
	lySub := s.subscribe("cloudmusicv3", []string{"lyric_update"}, false, "sse-lyric")
	siSub := s.subscribe("cloudmusicv3", []string{"song_info_update"}, false, "sse-song")

	s.UpdatePlayerState(statusEvt("cloudmusicv3", "paused"))
	s.sweepIdleHide(time.Now().Add(20 * time.Second))

	if evs := drainCh(wsSub.ch); len(evs) != 1 || evs[0].Type != player.EventPlayerClear {
		t.Fatalf("WS 应收到 player_clear，实际 %+v", evs)
	}

	evs := drainCh(lySub.ch)
	if len(evs) != 1 || evs[0].Type != player.EventLyricUpdate {
		t.Fatalf("lyric_update-SSE 应收到 lyric_update，实际 %+v", evs)
	}
	if lu, ok := evs[0].Data.(*LyricUpdate); !ok || lu.Index != -1 {
		t.Fatalf("lyric_update-SSE 清行应为 index:-1，实际 %+v", evs[0].Data)
	}

	if evs := drainCh(siSub.ch); len(evs) != 1 || evs[0].Type != player.EventSongInfoUpdate {
		t.Fatalf("song_info-SSE 应收到 song_info_update，实际 %+v", evs)
	}
}

// TestIdleHideClearCarriesProgress：SSE 清行的 lyric_update(index:-1) 必须带停下那刻的真实 progress，
// 而非零值——杀掉「Progress:0」突变体（lint 门禁只查字段在不在、既有 SSE 测试不读 progress，故此前双绿逃逸）。
func TestIdleHideClearCarriesProgress(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	s.SetIdleHideResolver(func(string) int { return 15 })
	lySub := s.subscribe("cloudmusicv3", []string{"lyric_update"}, false, "sse-lyric")
	// 一条带真实 progress 的 lyric_update（设 ps.Progress=0.42），再转非播放
	s.UpdatePlayerState(player.Event{PlayerName: "cloudmusicv3", Type: player.EventLyricUpdate,
		Data: &player.LyricUpdate{Index: 5, Text: "x", Position: 90.0, Progress: 0.42}})
	s.UpdatePlayerState(statusEvt("cloudmusicv3", "paused"))
	s.sweepIdleHide(time.Now().Add(20 * time.Second))

	evs := drainCh(lySub.ch)
	if len(evs) != 1 || evs[0].Type != player.EventLyricUpdate {
		t.Fatalf("应收到一条 lyric_update 清行，实际 %+v", evs)
	}
	lu, ok := evs[0].Data.(*LyricUpdate)
	if !ok || lu.Index != -1 {
		t.Fatalf("清行应为 index:-1，实际 %+v", evs[0].Data)
	}
	if lu.Progress != 0.42 {
		t.Fatalf("清行 Progress 应为停下那刻的 0.42，实际 %v（Progress:0 突变体应在此被抓）", lu.Progress)
	}
}

// TestIdleHideRootSwitchNotAffected：根切换到一个处于 per-player 隐藏态的播放器时，根订阅者必须收到
// 该播放器的完整缓存（含 all_lyrics），而非 player_clear——per-player 隐藏绝不能泄漏进根（§3.4）。
// 守 buildInitEvents 的 honorHidden 接线：把 NotifySubscribersFullState 处误传 true 即红。
func TestIdleHideRootSwitchNotAffected(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	s.SetIdleHideResolver(func(string) int { return 15 })
	s.UpdatePlayerState(player.Event{PlayerName: "cloudmusicv3", Type: player.EventAllLyrics, Data: &player.AllLyricsData{Title: "t", Lyrics: make([]player.LyricLine, 3)}})
	s.UpdatePlayerState(lyricEvt("cloudmusicv3", 2))
	s.UpdatePlayerState(statusEvt("cloudmusicv3", "paused"))
	s.sweepIdleHide(time.Now().Add(20 * time.Second))
	if !s.playerStates["cloudmusicv3"].IdleHidden {
		t.Fatal("前置：cloudmusicv3 应处于隐藏态")
	}

	rootSub := s.subscribe("", nil, true, "root")
	s.SetActivePlayer("cloudmusicv3")
	s.NotifySubscribersFullState("", "cloudmusicv3") // 模拟 router 切根到该播放器

	evs := drainCh(rootSub.ch)
	var hasAllLyrics, hasClear bool
	for _, e := range evs {
		if e.Type == player.EventAllLyrics {
			hasAllLyrics = true
		}
		if e.Type == player.EventPlayerClear {
			hasClear = true
		}
	}
	if !hasAllLyrics {
		t.Errorf("根切到隐藏播放器应收到其完整缓存(含 all_lyrics)，实际 %v", evTypes(evs))
	}
	if hasClear {
		t.Errorf("根切换不应收到 player_clear（per-player 隐藏泄漏进根，违反 §3.4），实际 %v", evTypes(evs))
	}
}

// httpEmpty 跑一个 handler，返回其 data 是否为空对象 {}。
func httpEmpty(t *testing.T, h http.HandlerFunc, path string) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", path, nil))
	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v (%s)", err, rec.Body.String())
	}
	return len(resp.Data) == 0
}

// evTypes 提取事件类型序列，供断言错误信息用。
func evTypes(evs []WSEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}
