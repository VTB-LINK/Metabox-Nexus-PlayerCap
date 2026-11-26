package kugou

// 本文件只守一件事：kugou 会话开始时连续读到无歌（nil 或空 hash），在发出 waiting_song/no_song 之后
// 紧跟恰好一条 clear_song_data，且事件通道满或仅余 1 槽时在后续无歌拍重试补齐（#231，r1 P1）。
//
// 背景：服务端仅在 clear_song_data 时派生真停播清除（设计 §4.5）。若断线清缓存后重连无歌只发
// waiting_song 不发 clear_song_data，专属 WS 与两条 SSE 会永久停留在断线前的旧歌。
//
// 变异自证（均实测）：
//   - runSession 无歌分支去掉 TryEmit(EventClearSongData, nil) → 全部用例缺 clear_song_data 而红；
//   - 去掉 noSongClear 一次性标志（每拍都发）→ TestNoSongAtConnectEmitsSingleClear 多出 clear_song_data 而红；
//   - 改用 EmitStatusIfChanged 返回值门控清空（而非 LastStatus）→ TestNoSongClearRetriesWhenQueueFull「余 1 槽」子例红；
//   - 换歌时不把 noSongClear 置 false → 若队列满后直接来歌再无歌（不适用于 lastHash!=""，但此处明确保证换歌清除待发）。

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

const kugouFiller = "filler"

// chokeSource 按步返回读数并在读取前控制事件通道的可用空位，模拟路由消费暂时跟不上。
type chokeSource struct {
	reads  []*cdp.PlayInfo
	step   int
	before func(step int)
}

func (s *chokeSource) GetPlayInfo() (*cdp.PlayInfo, error) {
	if s.before != nil {
		s.before(s.step)
	}
	if s.step >= len(s.reads) {
		return nil, errors.New("script exhausted")
	}
	r := s.reads[s.step]
	s.step++
	return r, nil
}

func (s *chokeSource) IsClosed() bool { return false }

func drainAllNoFiller(p *KuGouPlayer) []player.Event {
	var out []player.Event
	for {
		select {
		case evt := <-p.EventCh:
			if evt.Type != kugouFiller {
				out = append(out, evt)
			}
		default:
			return out
		}
	}
}

func summarizeNoSongEvents(evs []player.Event) []string {
	var out []string
	for _, evt := range evs {
		switch evt.Type {
		case player.EventStatusUpdate:
			st := evt.Data.(*player.StatusInfo)
			out = append(out, st.Status+"|"+st.Reason)
		case player.EventClearSongData:
			out = append(out, player.EventClearSongData)
		}
	}
	return out
}

// TestNoSongAtConnectEmitsSingleClear 钉住新会话连续收到 nil 与空 hash 时，先发 waiting_song/no_song
// 再紧跟恰好一条 clear_song_data，后续无歌拍不再重发；跨断开后重连无歌同理发出该配对。
//
// 变异自证（实测）：runSession 无歌分支去掉 TryEmit(EventClearSongData, nil) 即红；去掉 noSongClear 一次性
// 守卫每拍都发即红。
func TestNoSongAtConnectEmitsSingleClear(t *testing.T) {
	orig := fetchLyrics
	fetchLyrics = func(string, int, string, string) ([]klyric.Line, error) { return nil, nil }
	defer func() { fetchLyrics = orig }()

	t.Run("连续无歌只发一对waiting_song与clear_song_data", func(t *testing.T) {
		p := New(0, 0)
		synctest.Test(t, func(*testing.T) {
			p.runSession(&scriptedSource{reads: []*cdp.PlayInfo{nil, &cdp.PlayInfo{}, nil, &cdp.PlayInfo{}}}, nil)
		})
		got := summarizeNoSongEvents(drainAllNoFiller(p))
		want := []string{"waiting_song|" + player.ReasonNoSong, player.EventClearSongData}
		if !slices.Equal(got, want) {
			t.Fatalf("无歌事件序列\n got  %q\n want %q", got, want)
		}
	})

	t.Run("播歌断开清缓存后重连无歌补发clear_song_data", func(t *testing.T) {
		p := New(0, 0)
		// 会话 1：播放歌曲 A
		synctest.Test(t, func(*testing.T) {
			p.runSession(&scriptedSource{reads: []*cdp.PlayInfo{song("A", "playing"), song("A", "playing")}}, nil)
		})
		drainAllNoFiller(p)

		// 模拟 Start 会话结束：发 standby/cdp_disconnected + clear_song_data（断开清缓存）
		p.EmitStatusIfChanged(player.ProcessStatus(true, true, kugouDetails(), player.Blocked{
			Reason: player.ReasonCDPDisconnected,
			Detail: "disconnected",
		}))
		p.Emit(player.EventClearSongData, nil)
		drainAllNoFiller(p)

		// 会话 2：重连后持续无歌，必须发出 waiting_song/no_song 后紧跟恰好一条 clear_song_data
		synctest.Test(t, func(*testing.T) {
			p.runSession(&scriptedSource{reads: []*cdp.PlayInfo{nil, &cdp.PlayInfo{}, nil}}, nil)
		})
		got := summarizeNoSongEvents(drainAllNoFiller(p))
		want := []string{"waiting_song|" + player.ReasonNoSong, player.EventClearSongData}
		if !slices.Equal(got, want) {
			t.Fatalf("重连无歌事件序列\n got  %q\n want %q", got, want)
		}
	})
}

// TestNoSongClearRetriesWhenQueueFull 钉住事件通道余 1 槽或全满时，后续无歌拍按「先状态后清空」补齐：
//   - 余 1 槽：首拍 waiting_song 占最后 1 槽入队，clear_song_data 丢弃；第 2 拍通道腾空，
//     EmitStatusIfChanged 去重但 LastStatus 仍为 no_song，补发 clear_song_data。
//   - 全满（0 槽）：首拍两条都丢弃；第 2 拍通道腾空，两条按序补发。
//
// 变异自证（实测）：无歌分支去掉 TryEmit(EventClearSongData, nil)，两子例均红；改用 EmitStatusIfChanged 返回值
// 门控清空而非 LastStatus，「通道仅余1槽」子例红。
func TestNoSongClearRetriesWhenQueueFull(t *testing.T) {
	orig := fetchLyrics
	fetchLyrics = func(string, int, string, string) ([]klyric.Line, error) { return nil, nil }
	defer func() { fetchLyrics = orig }()

	for _, tc := range []struct {
		name string
		free int
	}{
		{"通道仅余1槽", 1},
		{"通道全满0槽", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(0, 0)
			var collected []player.Event
			src := &chokeSource{
				reads: []*cdp.PlayInfo{nil, nil, nil},
				before: func(step int) {
					if step == 0 {
						for len(p.EventCh) < cap(p.EventCh)-tc.free {
							p.EventCh <- player.Event{Type: kugouFiller}
						}
					} else {
						collected = append(collected, drainAllNoFiller(p)...)
					}
				},
			}
			synctest.Test(t, func(*testing.T) { p.runSession(src, nil) })
			collected = append(collected, drainAllNoFiller(p)...)

			got := summarizeNoSongEvents(collected)
			want := []string{"waiting_song|" + player.ReasonNoSong, player.EventClearSongData}
			if !slices.Equal(got, want) {
				t.Fatalf("%s 补发序列\n got  %q\n want %q", tc.name, got, want)
			}
		})
	}
}
