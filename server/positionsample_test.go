package server

// 本文件只测一件事：position_sample 是只校准缓存位置的内部事件（issue #70）。
//
// livePosition 在两次采样之间按墙钟外推。播放器一侧不发事件的停顿与重锚（短于暂停门槛的卡顿、
// 门槛以内的小幅跳变）使外推值偏离，无歌词歌曲发过 index:-1 之后再没有采样，偏差随时长累计。
// 各播放器因此在播放中按固定间隔报一次位置。它只写位置缓存：不广播、不计作播放器活动、不参与
// 路由评估，其余缓存不动。
//
// 手法同 initposition_test.go：backdate 把采样时刻往前挪，等价于已经过了那么久。

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/player"
)

// sampleEvt 构造一条 livePlayer 的位置采样。
func sampleEvt(pos, dur float32) player.Event {
	return posEvt(player.EventPositionSample, pos, dur)
}

// TestPositionSampleUpdatesPosition 钉住采样写入位置、进度与采样时刻，并清除暂停标记：其后的补发与
// HTTP 快照从采样值起算，状态不是 playing 时不外推。
//
// 变异自证：UpdatePlayerState 的位置分支去掉 EventPositionSample，三例红（位置仍从旧采样算）；
// 把暂停标记的赋值改成 `evt.Type != player.EventPlaybackResume`（采样也置标记），第一、三例红
// （停在采样值不再外推）。
func TestPositionSampleUpdatesPosition(t *testing.T) {
	cases := []struct {
		name   string
		status string
		before func(s *Server) // 采样之前
		sample float32
		after  time.Duration // 采样之后经过的时长
		lo, hi float32
	}{
		{"播放中：从采样值起外推", "playing",
			func(s *Server) { backdate(s, 60*time.Second) }, 140.7, 2 * time.Second, 142.7, 142.7 + slack},
		{"暂停中：写入但不外推", "paused",
			func(s *Server) { backdate(s, 60*time.Second) }, 30, 10 * time.Second, 30, 30},
		{"playback_pause 之后：清除暂停标记", "playing",
			func(s *Server) { s.UpdatePlayerState(posEvt(player.EventPlaybackPause, 15, testDuration)) }, 20, 3 * time.Second, 23, 23 + slack},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewServer([]string{livePlayer})
			seedLive(s, c.status, testDuration, 100)
			c.before(s)

			s.UpdatePlayerState(sampleEvt(c.sample, testDuration))
			backdate(s, c.after)

			expectInit(t, s, c.name, c.lo, c.hi)
			var got struct {
				Position float32 `json:"position"`
			}
			if err := json.Unmarshal(httpData(t, s.handleAllLyrics(livePlayer)), &got); err != nil {
				t.Fatal(err)
			}
			if !(got.Position >= c.lo && got.Position <= c.hi) {
				t.Errorf("HTTP all_lyrics 的 position = %v，期望在 [%v, %v]", got.Position, c.lo, c.hi)
			}
		})
	}
}

// TestPositionSampleBoundsDrift 回放 issue #70 的情形：播放器一侧以正常速率的三分之二前进 60 秒
// （位置读数每 1.5s 才前进一格），服务端按墙钟外推。每 5 秒（player.PositionSampleInterval）灌一条
// 采样时，每条采样到达前的超前量是这 5 秒里累计的 1.67s，采样之后归零；不灌采样时 60 秒累计 20s。
//
// 变异自证：UpdatePlayerState 的位置分支去掉 EventPositionSample，第一条采样到达后超前量仍是
// 1.67s 而红。
func TestPositionSampleBoundsDrift(t *testing.T) {
	const (
		start    = 100.0
		interval = 5 * time.Second
		steps    = 12                             // 60 秒
		perStep  = 5.0 * 2 / 3                    // 播放器一侧每 5 秒前进的秒数
		lag      = float32(5.0 - perStep)         // 一个间隔里累计的超前量
		total    = float32(steps) * (5 - perStep) // 不采样时 60 秒累计的超前量
	)
	live := func(s *Server) float32 { return initAllLyrics(t, s, livePlayer).Position }

	t.Run("每 5 秒一条采样", func(t *testing.T) {
		s := NewServer([]string{livePlayer})
		seedLive(s, "playing", 300, start)
		pos := float32(start)
		for i := 1; i <= steps; i++ {
			backdate(s, interval)
			pos += perStep
			if ahead := live(s) - pos; !(ahead >= lag-0.01 && ahead <= lag+slack) {
				t.Fatalf("第 %d 个 5 秒、采样到达前：超前 %.2fs，期望在 [%.2f, %.2f]", i, ahead, lag-0.01, lag+slack)
			}
			s.UpdatePlayerState(sampleEvt(pos, 300))
			if ahead := live(s) - pos; !(ahead >= 0 && ahead <= slack) {
				t.Fatalf("第 %d 个 5 秒、采样到达后：超前 %.2fs，期望在 [0, %.2f]", i, ahead, slack)
			}
		}
	})

	t.Run("不采样", func(t *testing.T) {
		s := NewServer([]string{livePlayer})
		seedLive(s, "playing", 300, start)
		backdate(s, steps*interval)
		pos := float32(start + steps*perStep)
		if ahead := live(s) - pos; !(ahead >= total-0.01 && ahead <= total+slack) {
			t.Errorf("60 秒后超前 %.2fs，期望在 [%.2f, %.2f]", ahead, total-0.01, total+slack)
		}
	})
}

// TestPositionSampleNotBroadcast 钉住采样不到达任何订阅者：根与 per-player、WS 与 SSE 都收不到。
// SetActivePlayer 是承重的，理由同 TestClearSongDataNotBroadcast。
//
// 变异自证：把 EventPositionSample 从 internalEvents 里去掉，根 WS 与 per-player WS 各收到一条而红。
func TestPositionSampleNotBroadcast(t *testing.T) {
	s := NewServer([]string{livePlayer})
	s.SetActivePlayer(livePlayer)
	subs := map[string]*subscriber{
		"根 WS":           s.subscribe("", nil, true, "root"),
		"根 SSE":          s.subscribe("", []string{player.EventLyricUpdate}, false, "root-sse"),
		"per-player WS":  s.subscribe(livePlayer, nil, true, "ws"),
		"per-player SSE": s.subscribe(livePlayer, []string{player.EventLyricUpdate}, false, "sse"),
	}

	s.NotifySubscribers(sampleEvt(10, testDuration), false)

	for name, sub := range subs {
		if got := evTypes(drainCh(sub.ch)); len(got) != 0 {
			t.Errorf("%s 收到了 %v —— position_sample 是内部事件，不该出现在公开 API 表面", name, got)
		}
	}
}

// TestPositionSampleNotActivity 钉住采样不计作播放器活动：不刷新无活跃巡检的计时锚点，不解除隐藏，
// 不留下待补发的标志。
//
// 变异自证：把 EventPositionSample 从 internalEvents 里去掉，两例都红（计时被刷新后未到阈值、不再
// 隐藏；隐藏被解除）。
func TestPositionSampleNotActivity(t *testing.T) {
	t.Run("不推迟隐藏", func(t *testing.T) {
		s := NewServer([]string{livePlayer})
		s.SetIdleHideResolver(func(string) int { return 15 })
		seedLive(s, "paused", testDuration, 100)
		// 最后一次对外事件在 10 秒前。此后到达的采样若刷新了计时，再过 6 秒还不到 15 秒的阈值。
		s.mu.Lock()
		at := time.Now().Add(-10 * time.Second)
		s.playerStates[livePlayer].LastEventAt = at
		s.mu.Unlock()

		s.UpdatePlayerState(sampleEvt(100, testDuration))

		if got := s.playerStates[livePlayer].LastEventAt; !got.Equal(at) {
			t.Errorf("采样刷新了 LastEventAt：%v → %v", at, got)
		}
		s.sweepIdleHide(time.Now().Add(6 * time.Second))
		if !s.playerStates[livePlayer].IdleHidden {
			t.Error("静默已满 16 秒，应隐藏；采样不应推迟它")
		}
	})

	t.Run("不解除隐藏", func(t *testing.T) {
		s := NewServer([]string{livePlayer})
		s.SetIdleHideResolver(func(string) int { return 15 })
		seedLive(s, "paused", testDuration, 100)
		s.sweepIdleHide(time.Now().Add(20 * time.Second))
		ws := s.subscribe(livePlayer, nil, true, "ws")

		evt := sampleEvt(100, testDuration)
		s.UpdatePlayerState(evt)
		s.NotifySubscribers(evt, false)

		ps := s.playerStates[livePlayer]
		if !ps.IdleHidden || ps.IdleResumePending {
			t.Errorf("采样之后应仍隐藏、无待补发：IdleHidden=%v IdleResumePending=%v", ps.IdleHidden, ps.IdleResumePending)
		}
		if init := evTypes(s.buildInitEvents(livePlayer, true)); !reflect.DeepEqual(init, []string{player.EventPlayerClear}) {
			t.Errorf("隐藏态下的连接补发应仍只有 player_clear，实得 %v", init)
		}
		if got := evTypes(drainCh(ws.ch)); len(got) != 0 {
			t.Errorf("订阅者不应收到任何事件，实得 %v", got)
		}
	})
}

// newIdleRouter 起一台没有优先播放器的路由，并摆出「livePlayer 在路由里是 playing、却没有活跃播放器」
// 的状态。正常运行中状态事件到达即评估，这个状态不会停留；直接摆出来，是为了让 updateRouting 的
// default 分支（非优先播放器的非状态事件、且无活跃播放器时重评估普通组）有切换可做。
func newIdleRouter(s *Server) *Router {
	cfg := config.DefaultConfig()
	cfg.PriorPlayer = nil
	r := NewRouter(&cfg, s, []string{livePlayer})
	r.normalStates[livePlayer].status = "playing"
	return r
}

// TestPositionSampleNoRouting 钉住采样不参与路由评估：updateRouting 对它直接返回，不切换活跃播放器，
// 根订阅者没有 player_switch。对照组说明摆出的状态确有切换可做：lyric_update 与 clear_song_data
// 都会切过去——clear_song_data 的现有路径没有随之改动。
//
// 变异自证：删掉 updateRouting 开头对 EventPositionSample 的返回，采样一例红（根被切到该播放器）。
func TestPositionSampleNoRouting(t *testing.T) {
	cases := []struct {
		name     string
		evt      player.Event
		switches bool
	}{
		{"position_sample", sampleEvt(10, testDuration), false},
		{"对照：lyric_update", posEvt(player.EventLyricUpdate, 10, testDuration), true},
		{"对照：clear_song_data", player.Event{PlayerName: livePlayer, Type: player.EventClearSongData}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewServer([]string{livePlayer})
			r := newIdleRouter(s)
			root := s.subscribe("", nil, true, "root")

			switched := r.updateRouting(c.evt)

			if switched != c.switches {
				t.Errorf("updateRouting 返回 %v，期望 %v", switched, c.switches)
			}
			wantActive := ""
			if c.switches {
				wantActive = livePlayer
			}
			if got := r.GetActivePlayer(); got != wantActive {
				t.Errorf("活跃播放器 = %q，期望 %q", got, wantActive)
			}
			if got := hasType(drainCh(root.ch), player.EventPlayerSwitch); got != c.switches {
				t.Errorf("根订阅者收到 player_switch = %v，期望 %v", got, c.switches)
			}
		})
	}
}

// TestInitEventsNeverInternal 钉住补发的整套里没有内部事件。switchSkip 的哈希表由这一套的事件类型
// 建出，router.Run 只在表里有该类型时才比对：内部事件不在其中，故不比对、不消耗。
//
// 变异自证：往 buildInitEvents 的结果末尾加一条 Type 为 EventPositionSample 的事件即红。
func TestInitEventsNeverInternal(t *testing.T) {
	s := NewServer([]string{livePlayer})
	seedLive(s, "playing", testDuration, 100)
	seedSongData(s, livePlayer)
	s.UpdatePlayerState(posEvt(player.EventLyricUpdate, 100, testDuration))
	s.UpdatePlayerState(sampleEvt(101, testDuration))

	evs := s.buildInitEvents(livePlayer, true)
	if len(evs) != 4 {
		t.Fatalf("前置：缓存齐全时补发应有 4 条，实得 %v", evTypes(evs))
	}
	for _, e := range evs {
		if internalEvents[e.Type] {
			t.Errorf("补发里出现了内部事件 %q", e.Type)
		}
	}
	if !internalEvents[player.EventPositionSample] {
		t.Error("position_sample 应登记为内部事件")
	}
}

// TestPositionSampleLeavesRestOfCache 钉住采样只写位置：状态、歌曲信息、整首歌词与当前行原样不动；
// 缓存已被 clear_song_data 清空时，采样也不使补发多出条目。
//
// 变异自证：让采样像 all_lyrics 那样顺带清掉 ps.LyricUpdate，「缓存齐全」一例红。
func TestPositionSampleLeavesRestOfCache(t *testing.T) {
	t.Run("缓存齐全", func(t *testing.T) {
		s := NewServer([]string{livePlayer})
		seedLive(s, "playing", testDuration, 100)
		seedSongData(s, livePlayer)
		s.UpdatePlayerState(posEvt(player.EventLyricUpdate, 100, testDuration))
		before := *s.playerStates[livePlayer]

		s.UpdatePlayerState(sampleEvt(120, testDuration))

		after := *s.playerStates[livePlayer]
		if after.Status != before.Status || after.SongInfo != before.SongInfo || after.LyricUpdate != before.LyricUpdate {
			t.Error("采样换掉了状态、歌曲信息或当前行的缓存")
		}
		if !reflect.DeepEqual(after.AllLyrics, before.AllLyrics) || after.Title != before.Title || after.Duration != before.Duration {
			t.Error("采样改动了整首歌词、标题或时长")
		}
		if after.Position != 120 {
			t.Errorf("前置：采样应已写入位置，实得 %v", after.Position)
		}
	})

	t.Run("缓存已清空", func(t *testing.T) {
		s := NewServer([]string{livePlayer})
		seedLive(s, "playing", testDuration, 100)
		s.UpdatePlayerState(player.Event{PlayerName: livePlayer, Type: player.EventClearSongData})

		s.UpdatePlayerState(sampleEvt(120, testDuration))

		if got := evTypes(s.buildInitEvents(livePlayer, true)); !reflect.DeepEqual(got, []string{player.EventStatusUpdate}) {
			t.Errorf("清空之后的补发应只有状态，实得 %v", got)
		}
	})
}

// TestPositionSampleBadPayload 钉住载荷不是 *PlaybackTimeInfo 时整条忽略：不 panic，位置不动。
// 事件载荷一律传结构体指针（AGENTS §3.2）；传错的后果应当是这条采样无效，而不是带崩路由循环。
//
// 变异自证：把位置分支的类型断言改成不带 ok 的 `evt.Data.(*player.PlaybackTimeInfo)`，即 panic 而红。
func TestPositionSampleBadPayload(t *testing.T) {
	for name, data := range map[string]interface{}{
		"nil":  nil,
		"值类型":  player.PlaybackTimeInfo{Position: 5},
		"别的指针": &player.LyricUpdate{Position: 5},
	} {
		t.Run(name, func(t *testing.T) {
			s := NewServer([]string{livePlayer})
			seedLive(s, "paused", testDuration, 100)

			s.UpdatePlayerState(player.Event{PlayerName: livePlayer, Type: player.EventPositionSample, Data: data})

			if got := s.playerStates[livePlayer].Position; got != 100 {
				t.Errorf("position = %v，应保持 100", got)
			}
		})
	}
}
