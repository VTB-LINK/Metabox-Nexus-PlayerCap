package server

// 本文件只测一件事：per-player 通道解除无活跃隐藏之后，向该播放器的连接补发整套缓存（issue #69）。
//
// 隐藏态下的连接只有一条 player_clear（SSE 是 in-band 清除或什么都没有）。播放器恢复时若只下发触发
// 解除的那条事件，这些连接要到下一次换歌才有歌名、封面与整首歌词。补发在解除之后第一条「触发补发」
// 的事件上进行（idleResumeTrigger）；触发事件自身比照根路径 switchSkip 的规则处理。
//
// 手法：feed 按 router.Run 对每条事件的做法回放——先 UpdatePlayerState，再 NotifySubscribers。
// 隐藏沿用 idlehide_test.go 的做法，把 now+Δ 传给 sweepIdleHide。

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/player"
)

const (
	idlePlayer  = "cloudmusicv3"
	otherPlayer = "kugou"
	oldSong     = "旧歌"
	newSong     = "新歌"
)

func songInfoEvt(name, song string) player.Event {
	return player.Event{PlayerName: name, Type: player.EventSongInfoUpdate,
		Data: &player.SongInfo{Name: song, Singer: "某人", Title: song + " - 某人"}}
}

func allLyricsEvt(name, song string) player.Event {
	return player.Event{PlayerName: name, Type: player.EventAllLyrics, Data: &player.AllLyricsData{
		Title: song + " - 某人", Duration: testDuration, Count: 2,
		Lyrics: []player.LyricLine{{Index: 0, Text: song + "第一行"}, {Index: 1, Text: song + "第二行"}},
	}}
}

func lineEvt(name, song string, index int, pos float32) player.Event {
	return player.Event{PlayerName: name, Type: player.EventLyricUpdate, Data: &player.LyricUpdate{
		Index: index, Text: song + "某行", Position: pos, Progress: player.ClampProgress(pos, testDuration),
	}}
}

func timeEvt(name, typ string, pos float32) player.Event {
	return player.Event{PlayerName: name, Type: typ,
		Data: &player.PlaybackTimeInfo{Position: pos, Progress: player.ClampProgress(pos, testDuration)}}
}

func clearEvt(name string) player.Event {
	return player.Event{PlayerName: name, Type: player.EventClearSongData}
}

func idleEvt(name string) player.Event {
	return player.Event{PlayerName: name, Type: player.EventLyricIdle, Data: struct{}{}}
}

// feed 按 router.Run 的顺序回放事件：先写缓存，再广播（不跳过根订阅者）。
func feed(s *Server, evts ...player.Event) {
	for _, e := range evts {
		s.UpdatePlayerState(e)
		s.NotifySubscribers(e, false)
	}
}

// hideIdle 把 idlePlayer 的通道巡检至隐藏；隐藏不成立即中止测试。
func hideIdle(t *testing.T, s *Server, after time.Duration) {
	t.Helper()
	s.sweepIdleHide(time.Now().Add(after))
	if !s.playerStates[idlePlayer].IdleHidden {
		t.Fatal("前置：通道应已隐藏")
	}
}

// newHiddenChannel 起一台开了 15s 无活跃隐藏的 Server：idlePlayer 的缓存里有状态（paused）、
// 歌曲信息、整首歌词与当前行（位置 100s），通道已隐藏。
func newHiddenChannel(t *testing.T) *Server {
	t.Helper()
	s := NewServer([]string{idlePlayer, otherPlayer})
	s.SetIdleHideResolver(func(string) int { return 15 })
	feed(s, statusEvt(idlePlayer, "paused"), songInfoEvt(idlePlayer, oldSong),
		allLyricsEvt(idlePlayer, oldSong), lineEvt(idlePlayer, oldSong, 0, 100))
	hideIdle(t, s, 20*time.Second)
	return s
}

// idleConns 是 idlePlayer 的三种连接：WS 与两条类型过滤的 SSE。
type idleConns struct {
	ws, lyric, song *subscriber
}

func joinIdle(s *Server) idleConns {
	return idleConns{
		ws:    s.subscribe(idlePlayer, nil, true, "ws"),
		lyric: s.subscribe(idlePlayer, []string{player.EventLyricUpdate}, false, "sse-lyric"),
		song:  s.subscribe(idlePlayer, []string{player.EventSongInfoUpdate}, false, "sse-song"),
	}
}

// expectTypes 取空订阅通道并断言事件类型序列，返回取到的事件。
func expectTypes(t *testing.T, label string, sub *subscriber, want ...string) []WSEvent {
	t.Helper()
	evs := drainCh(sub.ch)
	if got := evTypes(evs); !reflect.DeepEqual(got, append([]string{}, want...)) {
		t.Errorf("%s：\n got  %v\n want %v", label, got, want)
	}
	return evs
}

// songsIn 返回事件里出现过的歌名：song_info_update 的 name、all_lyrics 的 title、lyric_update 的 text。
func songsIn(evs []WSEvent) []string {
	var out []string
	for _, e := range evs {
		switch d := e.Data.(type) {
		case *player.SongInfo:
			out = append(out, d.Name)
		case *player.AllLyricsData:
			out = append(out, d.Title)
		case *player.LyricUpdate:
			out = append(out, d.Text)
		}
	}
	return out
}

var fullSet = []string{player.EventStatusUpdate, player.EventSongInfoUpdate, player.EventAllLyrics, player.EventLyricUpdate}

// TestIdleResumePushesFullState 钉住补发本身：内容与顺序同 buildInitEvents，WS 与 SSE 各按类型过滤，
// 隐藏前已在线的连接同样收到，根订阅者与别的播放器的连接不受影响；缓存里没有的类型不补。
//
// 变异自证：删掉 NotifySubscribers 里投递整套的那个循环，「缓存齐全」「只有状态」「组装晚于写缓存」
// 三例红（WS 只剩触发事件）。
func TestIdleResumePushesFullState(t *testing.T) {
	t.Run("缓存齐全", func(t *testing.T) {
		s := NewServer([]string{idlePlayer, otherPlayer})
		s.SetIdleHideResolver(func(string) int { return 15 })
		early := s.subscribe(idlePlayer, nil, true, "ws-early") // 隐藏前已在线
		feed(s, statusEvt(idlePlayer, "paused"), songInfoEvt(idlePlayer, oldSong),
			allLyricsEvt(idlePlayer, oldSong), lineEvt(idlePlayer, oldSong, 0, 100))
		drainCh(early.ch)
		hideIdle(t, s, 20*time.Second)

		c := joinIdle(s)
		root := s.subscribe("", nil, true, "root")
		other := s.subscribe(otherPlayer, nil, true, "other")
		if init := evTypes(s.buildInitEvents(idlePlayer, true)); !reflect.DeepEqual(init, []string{player.EventPlayerClear}) {
			t.Fatalf("前置：隐藏态下的连接补发应只有 player_clear，实得 %v", init)
		}

		feed(s, timeEvt(idlePlayer, player.EventPlaybackResume, 42.5))

		want := append(append([]string{}, fullSet...), player.EventPlaybackResume)
		evs := expectTypes(t, "隐藏期间连入的 WS", c.ws, want...)
		expectTypes(t, "隐藏前已在线的 WS", early, append([]string{player.EventPlayerClear}, want...)...)
		expectTypes(t, "lyric_update-SSE", c.lyric, player.EventLyricUpdate)
		expectTypes(t, "song_info-SSE", c.song, player.EventSongInfoUpdate)
		expectTypes(t, "根订阅者（无活跃播放器）", root)
		expectTypes(t, "另一个播放器的连接", other)

		// 整套里的 all_lyrics 与 lyric_update 带同一个位置：最近一次采样（playback_resume 的 42.5），
		// 不是缓存的 lyric_update 里留着的 100。
		var al *AllLyrics
		var lu *LyricUpdate
		for _, e := range evs {
			switch d := e.Data.(type) {
			case *AllLyrics:
				al = d
			case *LyricUpdate:
				lu = d
			}
		}
		if al == nil || lu == nil {
			t.Fatal("整套缺 all_lyrics 或 lyric_update")
		}
		if al.Position != 42.5 || lu.Position != 42.5 || al.Progress != lu.Progress {
			t.Errorf("整套的位置应同为 42.5：all_lyrics (%v, %v)，lyric_update (%v, %v)",
				al.Position, al.Progress, lu.Position, lu.Progress)
		}
	})

	t.Run("只有状态", func(t *testing.T) {
		s := NewServer([]string{idlePlayer})
		s.SetIdleHideResolver(func(string) int { return 15 })
		feed(s, statusEvt(idlePlayer, "paused"))
		hideIdle(t, s, 20*time.Second)
		c := joinIdle(s)

		feed(s, timeEvt(idlePlayer, player.EventPlaybackResume, 1))

		expectTypes(t, "WS", c.ws, player.EventStatusUpdate, player.EventPlaybackResume)
		expectTypes(t, "lyric_update-SSE", c.lyric)
		expectTypes(t, "song_info-SSE", c.song)
	})

	t.Run("缓存全空", func(t *testing.T) {
		s := NewServer([]string{idlePlayer})
		s.SetIdleHideResolver(func(string) int { return 15 })
		feed(s, idleEvt(idlePlayer)) // 只有一条不写缓存的事件：通道活跃过，缓存里什么都没有
		hideIdle(t, s, 20*time.Second)
		c := joinIdle(s)

		feed(s, timeEvt(idlePlayer, player.EventPlaybackResume, 1))

		expectTypes(t, "WS", c.ws, player.EventPlaybackResume)
	})

	// 整套在 NotifySubscribers 里组装，位置外推到那一刻：状态转 playing 之后过了 2s 才广播，
	// 补发的位置是 102 而不是写缓存时的 100。
	t.Run("组装晚于写缓存", func(t *testing.T) {
		s := NewServer([]string{livePlayer})
		s.SetIdleHideResolver(func(string) int { return 15 })
		seedLive(s, "paused", testDuration, 100)
		s.sweepIdleHide(time.Now().Add(20 * time.Second))
		ws := s.subscribe(livePlayer, nil, true, "ws")

		evt := statusEvt(livePlayer, "playing")
		s.UpdatePlayerState(evt)
		backdate(s, 2*time.Second)
		s.NotifySubscribers(evt, false)

		for _, e := range drainCh(ws.ch) {
			if al, ok := e.Data.(*AllLyrics); ok {
				if !(al.Position >= 102 && al.Position <= 102+slack) {
					t.Errorf("position = %v，期望在 [102, %v]", al.Position, 102+slack)
				}
				return
			}
		}
		t.Error("整套缺 all_lyrics")
	})
}

// TestIdleResumeTriggerItself 钉住触发事件自身的处理，比照根路径 switchSkip：song_info_update、
// all_lyrics、lyric_update 与整套里同类型那条内容相同才不单发，status_update 不比对、整套之后再发
// 一次，其余类型在整套之后发。整套取触发事件写入缓存之后的状态。
//
// 变异自证：把 inFullState 里的哈希比较改成恒 false（触发事件一律照发），三种缓存类型的四例红；
// 改成恒 true（只按类型、不比哈希），「歌词内容与缓存不同」一例红；把 status_update 也纳入比对，
// 「playing 状态」一例红。
func TestIdleResumeTriggerItself(t *testing.T) {
	// all_lyrics 的 count 与 lyrics 不符：缓存按 lyrics 实际行数给 count，与触发事件的哈希不同。
	odd := allLyricsEvt(idlePlayer, newSong)
	odd.Data.(*player.AllLyricsData).Count = 99

	withSet := func(more ...string) []string { return append(append([]string{}, fullSet...), more...) }
	noLine := fullSet[:3] // all_lyrics 写缓存时清掉了当前行

	cases := []struct {
		name string
		evt  player.Event
		want []string // WS 收到的类型序列
	}{
		{"playing 状态", statusEvt(idlePlayer, "playing"), withSet(player.EventStatusUpdate)},
		{"song_info_update", songInfoEvt(idlePlayer, newSong), withSet()},
		{"all_lyrics", allLyricsEvt(idlePlayer, newSong), noLine},
		{"lyric_update", lineEvt(idlePlayer, oldSong, 1, 120), withSet()},
		{"index:-1 的 lyric_update", player.Event{PlayerName: idlePlayer, Type: player.EventLyricUpdate,
			Data: &player.LyricUpdate{Index: -1, Position: 5, Progress: player.ClampProgress(5, testDuration)}}, withSet()},
		{"playback_pause", timeEvt(idlePlayer, player.EventPlaybackPause, 30), withSet(player.EventPlaybackPause)},
		// lyrics 为 nil 的 all_lyrics 写不出缓存条目：整套里没有它，触发事件照发。
		{"缓存里没有该类型的条目", player.Event{PlayerName: idlePlayer, Type: player.EventAllLyrics,
			Data: &player.AllLyricsData{Title: newSong}}, noLine},
		{"歌词内容与缓存不同", odd, append(append([]string{}, noLine...), player.EventAllLyrics)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newHiddenChannel(t)
			conns := joinIdle(s)
			feed(s, c.evt)
			evs := expectTypes(t, "WS", conns.ws, c.want...)

			// 整套取触发事件写入之后的缓存：触发事件带来的内容出现在整套里，而不是它之前的那一份。
			switch d := c.evt.Data.(type) {
			case *player.SongInfo:
				if got := songsIn(evs); len(got) == 0 || got[0] != d.Name {
					t.Errorf("整套里的歌曲信息应是触发事件写入的 %q，实得 %v", d.Name, got)
				}
			case *player.LyricUpdate:
				if len(evs) == 0 {
					break
				}
				if last, ok := evs[len(evs)-1].Data.(*LyricUpdate); !ok || last.Index != d.Index {
					t.Errorf("整套里的当前行应是触发事件写入的 index %d，实得 %+v", d.Index, evs[len(evs)-1].Data)
				}
			}
		})
	}
}

// TestIdleResumeDeferredPastTerminalEvents 钉住补发不由终止类事件触发。
//
// 各播放器在进程退出、未启动、无歌处都是先发状态、后发 clear_song_data，wesing 轮询结束时还先发一条
// lyric_idle。这些事件若触发补发，推出去的是即将被清掉的上一首：clear_song_data 不外发，两条 SSE 也
// 收不到状态事件，旧歌名与旧歌词留在画面上；进程未启动时周期重发的 waiting_process 又使通道不再隐藏。
//
// 变异自证：把 idleResumeTrigger 改成恒返回 true，四例全红（第一条事件就把旧歌的整套推了出去）；
// 只把其中 lyric_idle 改成触发，wesing 的两例红。
func TestIdleResumeDeferredPastTerminalEvents(t *testing.T) {
	gone := func(status string) []player.Event {
		return []player.Event{statusEvt(idlePlayer, status), clearEvt(idlePlayer)}
	}
	var exited []player.Event // 进程退出，其后每约 2s 一组 waiting_process + clear_song_data
	exited = append(exited, gone("standby")...)
	for range 3 {
		exited = append(exited, gone("waiting_process")...)
	}
	cases := []struct {
		name string
		seq  []player.Event
	}{
		{"进程退出后一直未启动", exited},
		{"applemusic 清歌：waiting_song", gone("waiting_song")},
		{"wesing 关闭 K 歌窗口", append(append([]player.Event{idleEvt(idlePlayer)}, gone("waiting_song")...), idleEvt(idlePlayer))},
		{"wesing 进程退出", append(append([]player.Event{idleEvt(idlePlayer)}, gone("standby")...), gone("waiting_process")...)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newHiddenChannel(t)
			conns := joinIdle(s)
			feed(s, c.seq...)

			// 序列里的对外事件照常下发，此外什么都没有。
			var external []string
			for _, e := range c.seq {
				if !internalEvents[e.Type] {
					external = append(external, e.Type)
				}
			}
			expectTypes(t, "终止序列期间的 WS", conns.ws, external...)
			expectTypes(t, "终止序列期间的 lyric_update-SSE", conns.lyric)
			expectTypes(t, "终止序列期间的 song_info-SSE", conns.song)

			// 与 HTTP 快照相符：通道已解除隐藏，状态是最新的，其余已清空。
			if httpEmpty(t, s.handleStatusUpdate(idlePlayer), "/status_update") {
				t.Error("通道已解除隐藏，/status_update 应返回最新状态")
			}
			for name, h := range map[string]http.HandlerFunc{
				"song_info":    s.handleSongInfo(idlePlayer),
				"all_lyrics":   s.handleAllLyrics(idlePlayer),
				"lyric_update": s.handleLyricUpdate(idlePlayer),
			} {
				if !httpEmpty(t, h, "/"+name) {
					t.Errorf("clear_song_data 之后 /%s 应为空", name)
				}
			}

			// 新歌开播：第一条 playing 状态触发补发，此时缓存里只有这条状态。
			feed(s, statusEvt(idlePlayer, "playing"), songInfoEvt(idlePlayer, newSong), allLyricsEvt(idlePlayer, newSong))

			evs := expectTypes(t, "新歌开播后的 WS", conns.ws,
				player.EventStatusUpdate, player.EventStatusUpdate, player.EventSongInfoUpdate, player.EventAllLyrics)
			songEvs := expectTypes(t, "新歌开播后的 song_info-SSE", conns.song, player.EventSongInfoUpdate)
			expectTypes(t, "新歌开播后的 lyric_update-SSE", conns.lyric)
			if got, want := songsIn(evs), []string{newSong, newSong + " - 某人"}; !reflect.DeepEqual(got, want) {
				t.Errorf("WS 拿到的歌曲信息与歌词应是新歌的，实得 %v", got)
			}
			if got := songsIn(songEvs); !reflect.DeepEqual(got, []string{newSong}) {
				t.Errorf("song_info-SSE 拿到的应是新歌，实得 %v", got)
			}
		})
	}
}

// TestIdleResumeNonTriggerKeepsPending 钉住不触发补发的事件照常解除隐藏、照常下发，补发留给其后
// 第一条触发的事件。缓存未被清空时，那一次补发仍是完整的。
//
// 变异自证：把 NotifySubscribers 里取走标志的条件去掉 idleResumeTrigger（任何事件都取走标志但只在
// 触发时补发），三例红（其后的 playback_resume 不再补发）；idleResumeTrigger 的状态分支去掉
// normalizeStatus 判断（任何状态都触发），paused 与 standby 两例红。
func TestIdleResumeNonTriggerKeepsPending(t *testing.T) {
	cases := []struct {
		name string
		evt  player.Event
	}{
		{"paused 状态", statusEvt(idlePlayer, "paused")},
		{"standby 状态", statusEvt(idlePlayer, "standby")},
		{"lyric_idle", idleEvt(idlePlayer)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newHiddenChannel(t)
			conns := joinIdle(s)

			feed(s, c.evt)
			expectTypes(t, "不触发的事件之后的 WS", conns.ws, c.evt.Type)
			expectTypes(t, "不触发的事件之后的 lyric_update-SSE", conns.lyric)
			if s.playerStates[idlePlayer].IdleHidden {
				t.Error("任何对外事件都应解除隐藏")
			}
			if init := evTypes(s.buildInitEvents(idlePlayer, true)); !reflect.DeepEqual(init, fullSet) {
				t.Errorf("解除隐藏后的连接补发应是完整缓存，实得 %v", init)
			}

			feed(s, timeEvt(idlePlayer, player.EventPlaybackResume, 1))
			expectTypes(t, "其后 playback_resume 的 WS", conns.ws, append(append([]string{}, fullSet...), player.EventPlaybackResume)...)
			expectTypes(t, "其后 playback_resume 的 lyric_update-SSE", conns.lyric, player.EventLyricUpdate)
		})
	}
}

// TestIdleResumeRootGetsOnlyTrigger 钉住根订阅者不在补发范围内：该播放器恰是活跃播放器时，根订阅者
// 也只收到触发事件一条（AGENTS §3.4：per-player 的视图状态不得进入根路径）。
//
// 变异自证：在 NotifySubscribers 根订阅者的分支里也投一遍整套，根订阅者多收四条、根 SSE 多收一条而红。
func TestIdleResumeRootGetsOnlyTrigger(t *testing.T) {
	s := newHiddenChannel(t)
	s.SetActivePlayer(idlePlayer)
	root := s.subscribe("", nil, true, "root")
	rootSSE := s.subscribe("", []string{player.EventLyricUpdate}, false, "root-sse")

	feed(s, timeEvt(idlePlayer, player.EventPlaybackResume, 1))

	expectTypes(t, "根订阅者", root, player.EventPlaybackResume)
	expectTypes(t, "根 lyric_update-SSE", rootSSE)
}

// TestIdleResumeOnce 钉住补发与隐藏一一对应：未隐藏时不补发；一次解除只补发一次；再次隐藏、再次
// 解除时再补发一次。
//
// 变异自证：NotifySubscribers 取走标志时不清除它，「补发后的下一条事件」处多出整套而红；
// UpdatePlayerState 不论是否隐藏都置标志，「未隐藏」处多出整套而红。
func TestIdleResumeOnce(t *testing.T) {
	s := NewServer([]string{idlePlayer})
	s.SetIdleHideResolver(func(string) int { return 15 })
	c := joinIdle(s)
	feed(s, statusEvt(idlePlayer, "paused"), songInfoEvt(idlePlayer, oldSong), allLyricsEvt(idlePlayer, oldSong))
	drainCh(c.ws.ch)

	feed(s, lineEvt(idlePlayer, oldSong, 0, 100))
	expectTypes(t, "未隐藏", c.ws, player.EventLyricUpdate)

	hideIdle(t, s, 20*time.Second)
	expectTypes(t, "隐藏", c.ws, player.EventPlayerClear)

	withSet := append(append([]string{}, fullSet...), player.EventPlaybackResume)
	feed(s, timeEvt(idlePlayer, player.EventPlaybackResume, 100))
	expectTypes(t, "第一次解除", c.ws, withSet...)

	feed(s, timeEvt(idlePlayer, player.EventPlaybackPause, 101))
	expectTypes(t, "补发后的下一条事件", c.ws, player.EventPlaybackPause)

	hideIdle(t, s, 40*time.Second)
	expectTypes(t, "再次隐藏", c.ws, player.EventPlayerClear)

	feed(s, timeEvt(idlePlayer, player.EventPlaybackResume, 101))
	expectTypes(t, "第二次解除", c.ws, withSet...)
}

// TestIdleResumeInternalEventKeepsHidden 钉住内部事件不解除隐藏、不补发，也不留下待补发的标志；
// 其后触发事件到达时，补发的是清空之后的缓存。
//
// 变异自证：UpdatePlayerState 去掉「非内部事件」的条件（内部事件也解除隐藏并置标志），隐藏态断言红。
func TestIdleResumeInternalEventKeepsHidden(t *testing.T) {
	s := newHiddenChannel(t)
	c := joinIdle(s)

	feed(s, timeEvt(idlePlayer, player.EventPositionSample, 100), clearEvt(idlePlayer))

	ps := s.playerStates[idlePlayer]
	if !ps.IdleHidden || ps.IdleResumePending {
		t.Errorf("内部事件之后应仍隐藏、无待补发：IdleHidden=%v IdleResumePending=%v", ps.IdleHidden, ps.IdleResumePending)
	}
	expectTypes(t, "内部事件之后的 WS", c.ws)

	feed(s, timeEvt(idlePlayer, player.EventPlaybackResume, 1))
	expectTypes(t, "其后解除", c.ws, player.EventStatusUpdate, player.EventPlaybackResume)
}

// TestIdleResumeFullChannelDoesNotBlock 钉住补发的投递与其余发送点一样是非阻塞的（AGENTS §2.1）：
// 订阅通道已满时丢弃，NotifySubscribers 照常返回。
//
// 变异自证：把整套的投递改成 `sub.ch <- e` 阻塞发送，本测试 2s 超时而红。
func TestIdleResumeFullChannelDoesNotBlock(t *testing.T) {
	s := newHiddenChannel(t)
	ws := s.subscribe(idlePlayer, nil, true, "ws")
	for range cap(ws.ch) {
		ws.ch <- WSEvent{Type: "filler"}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		feed(s, timeEvt(idlePlayer, player.EventPlaybackResume, 1))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("订阅通道已满时 NotifySubscribers 被阻塞")
	}
	if n := len(ws.ch); n != cap(ws.ch) {
		t.Errorf("通道里应仍是原先那 %d 条，实得 %d", cap(ws.ch), n)
	}
}

// TestIdleResumeWithRootSwitch 钉住同一条 playing 状态既切根、又触发补发时两路互不影响：根订阅者
// 收到 player_switch 与整套（这条状态由 skipRoot 跳过），per-player 订阅者收到整套与这条状态。
// 按 router.Run 的顺序调用：UpdatePlayerState → updateRouting → NotifySubscribers(evt, switched)。
//
// 变异自证：NotifySubscribers 的扇出循环开头加 `if skipRoot { continue }`（切换时 per-player 也跳过），
// per-player 什么都收不到而红。
func TestIdleResumeWithRootSwitch(t *testing.T) {
	s := newHiddenChannel(t)
	cfg := config.DefaultConfig()
	cfg.PriorPlayer = nil
	r := NewRouter(&cfg, s, []string{idlePlayer, otherPlayer})
	c := joinIdle(s)
	root := s.subscribe("", nil, true, "root")

	evt := statusEvt(idlePlayer, "playing")
	s.UpdatePlayerState(evt)
	switched := r.updateRouting(evt)
	s.NotifySubscribers(evt, switched)

	if !switched {
		t.Fatal("前置：playing 状态应把根切到该播放器")
	}
	expectTypes(t, "根订阅者", root, append([]string{player.EventPlayerSwitch}, fullSet...)...)
	expectTypes(t, "per-player WS", c.ws, append(append([]string{}, fullSet...), player.EventStatusUpdate)...)
}
