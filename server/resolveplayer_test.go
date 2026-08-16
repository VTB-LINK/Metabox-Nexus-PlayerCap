package server

// 本文件只测 resolvePlayer 的根路径选址：activePlayer=="" 是「没有播放器在放」的**有意
// 信号**，根 HTTP 必须原样跟随（返回 ""），不得自行挑一个有状态的播放器顶上。
// 不测 handler 的快照原子性（race_test.go）、不测 WS 订阅分发。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// standby 让某播放器进入「有 Status、但 router 永不为其设 activePlayer」的状态。
// normalizeStatus 把 standby/waiting_process/waiting_song 全归为 idle，故这是
// 「开机没放歌」的长期稳态，不是瞬态。
func standby(s *Server, name string) {
	s.UpdatePlayerState(player.Event{
		PlayerName: name,
		Type:       player.EventStatusUpdate,
		Data:       &player.StatusInfo{Status: "waiting_process", Detail: "未检测到进程"},
	})
}

// TestResolvePlayerNoActiveReturnsEmpty 钉死本条的核心：**待机播放器不得被复活成活跃播放器**。
//
// 缺陷背景：这里曾有两个 range-map 兜底循环（先找 Status=="playing"，再找任意 Status != nil）。
// 后者条件只要求「曾经发过任何状态」，而四个播放器在对应 App 没开时都会发 waiting_process
// —— 于是「开机没放歌」这个长期稳态下，根 HTTP 会挑一个待机播放器返回它的残留数据，而同
// 一状态下 WS 收到的是干净的 player_clear。它是 9090f0b 从 router 删掉的
// findAnyNonPriorPlayer()（commit 原话「歌词冻结 bug 的根本原因」「不检查播放状态」）的同型残株。
//
// 变异自证：把任一兜底循环加回 resolvePlayer 即红。
func TestResolvePlayerNoActiveReturnsEmpty(t *testing.T) {
	s := NewServer([]string{"wesing", "kugou", "qqmusic", "cloudmusicv3"})
	for _, n := range []string{"wesing", "kugou", "qqmusic", "cloudmusicv3"} {
		standby(s, n)
	}
	// 残留数据：wesing 唱完后 SongInfo 未被清（暂停只发 EventPlaybackPause，不发 ClearSongData）
	s.UpdatePlayerState(player.Event{
		PlayerName: "wesing",
		Type:       player.EventSongInfoUpdate,
		Data:       &player.SongInfo{Name: "上一首", Singer: "某人", Title: "上一首 - 某人"},
	})

	// 多跑几次：range map 顺序随机，单次通过不足以证明兜底已消失
	for range 50 {
		if got := s.resolvePlayer(""); got != "" {
			t.Fatalf("无 activePlayer 时根路径必须返回 \"\"，实得 %q —— 待机播放器被复活了", got)
		}
	}
}

// TestResolvePlayerNoActiveIgnoresPlaying 钉死另一半：即使有播放器 raw 状态是 playing，
// 根路径也必须跟随 activePlayer 而非自行抢跑。
//
// 这是被删的第一个循环（找 Status=="playing"）的领地。它唯一可达的场景是并发瞬态窗口：
// router.go Run() 里 UpdatePlayerState 与 updateRouting 是两次独立加锁，中间 s.mu 空闲，
// HTTP 可观测到「新 raw playing + 旧 activePlayer 为空」。该窗口内返回 "" 才是与 WS 一致的
// 答案（buildInitEvents 在同一把锁下读同一个 activePlayer，返回 player_clear）；循环给的是
// 一个抢跑的、且在多个 playing 时随机的答案。
//
// 变异自证：把 `Status.Status == "playing"` 那个循环加回即红。
func TestResolvePlayerNoActiveIgnoresPlaying(t *testing.T) {
	s := NewServer([]string{"wesing", "kugou"})
	standby(s, "wesing")
	s.UpdatePlayerState(player.Event{
		PlayerName: "kugou",
		Type:       player.EventStatusUpdate,
		Data:       &player.StatusInfo{Status: "playing", Detail: "播放中"},
	})

	for range 50 {
		if got := s.resolvePlayer(""); got != "" {
			t.Fatalf("activePlayer 未设时根路径必须返回 \"\"（由 Router 决定归属），实得 %q", got)
		}
	}
}

// TestResolvePlayerFollowsActive 反方向回归：别把功能删坏了。
// 没有这条，「resolvePlayer 直接 return \"\"」这种把根端点整个废掉的假修复能通过上面两条。
func TestResolvePlayerFollowsActive(t *testing.T) {
	s := NewServer([]string{"wesing", "kugou"})
	standby(s, "wesing")
	standby(s, "kugou")

	s.SetActivePlayer("kugou")
	if got := s.resolvePlayer(""); got != "kugou" {
		t.Errorf("根路径应跟随 activePlayer=kugou，实得 %q", got)
	}
	s.SetActivePlayer("wesing")
	if got := s.resolvePlayer(""); got != "wesing" {
		t.Errorf("activePlayer 切换后根路径应跟随，实得 %q", got)
	}
	// clearActivePlayer 的语义：置空即「没有播放器在放」，不是「回退到猜」
	s.SetActivePlayer("")
	if got := s.resolvePlayer(""); got != "" {
		t.Errorf("activePlayer 被清空后根路径应返回 \"\"，实得 %q", got)
	}
}

// TestResolvePlayerPerPlayerPathUnaffected per-player 路径不经 activePlayer，
// 无论根路径怎么变都必须原样返回指定播放器（doc/API_RESPONSE_EXAMPLES.md:5 的契约）。
func TestResolvePlayerPerPlayerPathUnaffected(t *testing.T) {
	s := NewServer([]string{"wesing", "kugou"})
	if got := s.resolvePlayer("kugou"); got != "kugou" {
		t.Errorf("per-player 路径应原样返回 kugou，实得 %q", got)
	}
	s.SetActivePlayer("wesing")
	if got := s.resolvePlayer("kugou"); got != "kugou" {
		t.Errorf("per-player 路径不得受 activePlayer 影响，实得 %q", got)
	}
	// 未注册的播放器名也原样返回：handler 的 ps != nil 守卫负责兜住
	if got := s.resolvePlayer("nonexistent"); got != "nonexistent" {
		t.Errorf("per-player 路径应原样返回，实得 %q", got)
	}
}

// TestRootHTTPMatchesWSWhenNoActive 端到端：同一逻辑状态下，根 HTTP 与 WS 必须给出一致的答案。
//
// 这是本条的真正 why —— HTTP 说「wesing 在放《上一首》」、WS 说 player_clear，两个消费者
// 拿到互相矛盾的世界观。四个 handler 各自独立调 resolvePlayer，兜底还让它们之间也不一致
// （同一秒内 /song_info 与 /status_update 可解析到不同播放器）。
//
// 变异自证：加回任一兜底循环，/song_info 会吐出 player:"wesing" + 残留 SongInfo 即红。
func TestRootHTTPMatchesWSWhenNoActive(t *testing.T) {
	s := NewServer([]string{"wesing", "kugou"})
	standby(s, "wesing")
	standby(s, "kugou")
	s.UpdatePlayerState(player.Event{
		PlayerName: "wesing",
		Type:       player.EventSongInfoUpdate,
		Data:       &player.SongInfo{Name: "上一首", Singer: "某人", Title: "上一首 - 某人"},
	})

	// WS 侧的答案
	evts := s.buildInitEvents("", true)
	if len(evts) != 1 || evts[0].Type != player.EventPlayerClear {
		t.Fatalf("前置条件：WS 在无 activePlayer 时应收到单条 player_clear，实得 %+v", evts)
	}

	// HTTP 侧四个根端点必须给出等价的「无数据」答案
	endpoints := map[string]func(string) http.HandlerFunc{
		"/song_info":     s.handleSongInfo,
		"/status_update": s.handleStatusUpdate,
		"/lyric_update":  s.handleLyricUpdate,
		"/all_lyrics":    s.handleAllLyrics,
	}
	for path, mk := range endpoints {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mk("")(rec, httptest.NewRequest("GET", path, nil))

			var resp struct {
				Code   int             `json:"code"`
				Player string          `json:"player"`
				Data   json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("响应不是合法 JSON: %v (%s)", err, rec.Body.String())
			}
			if resp.Player != "" {
				t.Errorf("无 activePlayer 时 player 字段应为空串，实得 %q —— 与 WS 的 player_clear 矛盾", resp.Player)
			}
			if string(resp.Data) != "{}" {
				t.Errorf("无 activePlayer 时 data 应为 {}，实得 %s —— 陈旧数据泄漏上屏", resp.Data)
			}
		})
	}
}
