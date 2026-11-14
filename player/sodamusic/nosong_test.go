package sodamusic

// 本文件守 runSession 读到没有歌曲（mediaId 与歌名都为空）时的状态（issue #193）：
//   - 已宣告过歌曲：连续没有歌曲满 noSongTolerance 才报 waiting_song（no_song）并清歌曲数据，先状态后清空，只一次；
//     其间不发上一首的位置与状态事件（本文件的读数不带封面 URL，容忍期内没有任何事件）；
//   - 清空的两条事件没能入队（通道满）时逐拍补发，先状态后清空；补发之前宣告了新歌即作罢；
//   - 会话开始时没有歌曲：当即报 waiting_song，持续没有歌曲不重发；
//   - 反例：短于容忍期的没有歌曲之后换到新歌、或同一首继续，都不清空、不报；几段各自短于容忍期、合计超过的，
//     计时每次读到歌曲即清零；mediaId 已变、歌名未到（换歌过渡）不算没有歌曲；
//   - 清空之后同一首歌再出现按换歌重新宣告，落锚照常过新鲜度闸（不算本会话第一首）；会话第一首在中途照常按读数落锚。
//
// 真机未覆盖：汽水会自动填充播放列表，正常操作碰不到没有歌曲（issue #193）。客户端是 exitrelease_test.go 的
// scriptClient，会话在 testing/synctest 的泡里跑，轮询的 time.Sleep 走假时钟。
//
// 变异自证（在独立 worktree 里逐条注入 sodamusic.go、跑本文件、还原，均实测）记在各用例的注释里。

import (
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/sodamusic/cdp"
)

var waitingNoSong = "waiting_song|" + i18n.T("Soda Music has no current song") + "|" + player.ReasonNoSong

// noSong 是取到的播放态里没有歌曲时的读数。
var noSong = &cdp.ExtractionData{OK: true}

// songB 是另一首歌（《乙曲》）在 progress 秒处、播放中的读数。
func songB(progress float32) *cdp.ExtractionData {
	return &cdp.ExtractionData{
		OK: true, IsPlaying: true, ProgressSeconds: progress, DurationSeconds: 180,
		MediaID: "8", Name: "乙曲", Artists: []string{"乙"},
	}
}

const playingB = "playing|乙 - 乙曲|"

type timedEvent struct {
	at  time.Duration // 自连上起
	evt player.Event
}

// String 把状态记为「状态|detail|reason」，其余记事件名。
func (e timedEvent) String() string {
	if st, ok := e.evt.Data.(*player.StatusInfo); ok {
		return st.Status + "|" + st.Detail + "|" + st.Reason
	}
	return e.evt.Type
}

// sessionEvents 在 synctest 泡里以 script 跑一次 runSession，返回它发出的全部事件。script 须自己断开（返回 errLost），
// sessionLimit 兜底。
func sessionEvents(t *testing.T, script func(time.Duration) (*cdp.ExtractionData, error)) []timedEvent {
	t.Helper()
	var evs []timedEvent
	synctest.Test(t, func(t *testing.T) {
		p := New(0, 0)
		c := newScriptClient(func(e time.Duration) (*cdp.ExtractionData, error) {
			if e >= sessionLimit {
				return nil, errLost
			}
			return script(e)
		})
		done := make(chan struct{})
		got := make(chan []timedEvent)
		go func() {
			var out []timedEvent
			for {
				select {
				case evt := <-p.EventCh:
					out = append(out, timedEvent{time.Since(c.t0), evt})
				case <-done:
					got <- out
					return
				}
			}
		}()
		p.runSession(c)
		synctest.Wait()
		close(done)
		evs = <-got
	})
	return evs
}

// filler 是塞满事件通道用的占位事件类型，收取时丢掉。
const filler = "filler"

// sessionEventsChoked 同 sessionEvents，但事件在每次取数之前同步收取，时刻记为收取时刻（比发出晚一拍）。
// [from, to) 内不收，并把事件通道塞到只剩 free 个空位，模拟路由消费跟不上；此后第一次取数把通道里的事件全部收走，
// 占位事件不计。
func sessionEventsChoked(t *testing.T, from, to time.Duration, free int, script func(time.Duration) (*cdp.ExtractionData, error)) []timedEvent {
	t.Helper()
	var evs []timedEvent
	synctest.Test(t, func(t *testing.T) {
		p := New(0, 0)
		drain := func(at time.Duration) {
			for {
				select {
				case evt := <-p.EventCh:
					if evt.Type != filler {
						evs = append(evs, timedEvent{at, evt})
					}
				default:
					return
				}
			}
		}
		c := newScriptClient(func(e time.Duration) (*cdp.ExtractionData, error) {
			if e >= from && e < to {
				for len(p.EventCh) < cap(p.EventCh)-free {
					p.EventCh <- player.Event{Type: filler}
				}
			} else {
				drain(e)
			}
			if e >= sessionLimit {
				return nil, errLost
			}
			return script(e)
		})
		p.runSession(c)
		synctest.Wait()
		drain(time.Since(c.t0))
	})
	return evs
}

// between 返回 [from, to) 内的事件描述。
func between(evs []timedEvent, from, to time.Duration) []string {
	var out []string
	for _, e := range evs {
		if e.at >= from && e.at < to {
			out = append(out, e.String())
		}
	}
	return out
}

func count(evs []timedEvent, desc string) int {
	n := 0
	for _, e := range evs {
		if e.String() == desc {
			n++
		}
	}
	return n
}

// allLyrics 返回各条 all_lyrics 的时刻与位置。
func allLyrics(evs []timedEvent) (at []time.Duration, pos []float32) {
	for _, e := range evs {
		if d, ok := e.evt.Data.(*player.AllLyricsData); ok {
			at, pos = append(at, e.at), append(pos, d.Position)
		}
	}
	return at, pos
}

// 已宣告的歌之后一直没有歌曲：满容忍期的那一拍先报 waiting_song、后清空，各一次；之前与之后不发任何事件。
// 播放中与暂停中各跑一遍。
//
// 变异自证：去掉容忍期（宣告过即当拍清空）、容忍期少一拍（`>= noSongTolerance-200*time.Millisecond`），两个子例
// 因清空早于 12s 变红；清空分支整个删掉（恢复静默等待），因 10s 之后没有事件变红；状态改用 p.Emit 直接发，因每拍
// 重发变红；先清空后报状态，因顺序变红；清空时不复位 lastMediaID，因每拍重复清空变红。
func TestNoSongAfterSongClearsAfterTolerance(t *testing.T) {
	for _, tc := range []struct {
		name string
		read func(time.Duration) *cdp.ExtractionData
	}{
		{"播放中", playingAt},
		{"暂停中", func(time.Duration) *cdp.ExtractionData { return song(42, false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evs := sessionEvents(t, func(e time.Duration) (*cdp.ExtractionData, error) {
				switch {
				case e < 10*time.Second:
					return tc.read(e), nil
				case e < 40*time.Second:
					return noSong, nil
				}
				return nil, errLost
			})
			clearAt := 10*time.Second + noSongTolerance
			if got := between(evs, 10*time.Second, clearAt); len(got) != 0 {
				t.Fatalf("容忍期内不应发出任何事件，实为 %q", got)
			}
			if got := between(evs, clearAt, clearAt+time.Nanosecond); len(got) != 2 ||
				got[0] != waitingNoSong || got[1] != player.EventClearSongData {
				t.Fatalf("满容忍期的那一拍应先报 waiting_song 再清空，实为 %q", got)
			}
			if got := between(evs, clearAt+time.Nanosecond, sessionLimit); len(got) != 0 {
				t.Fatalf("清空之后持续没有歌曲不应再发事件，实为 %q", got)
			}
		})
	}
}

// 到期那一拍事件通道已满，或只剩一个空位、被 waiting_song 占去：没能入队的 waiting_song 与 clear_song_data 逐拍补发，
// 通道腾出后各入队一次，先状态后清空。脚本：10s 起没有歌曲（12s 到期），11.8s～15s 通道塞满（路由消费跟不上）。
//
// 变异自证：clear_song_data 改回 p.Emit、发过即作罢（不看是否入队），「只剩一个空位」因始终没有清空变红（「通道满」时
// 状态也没入队，清空跟着等到通道腾出才发，不受这一变异影响）；清空挪到状态之前尝试、且不等状态入队，两个子例都因
// 顺序变红。
// 未能自证的一处：「状态入队之后才发清空」这道闸只在路由恰好于两次 TryEmit 之间腾出空位时起作用，桩里通道只在取数时
// 收取，造不出这一时序；单去掉这道闸、或清空时的状态改用不看入队的 Emit，两个子例仍绿（实测），这一点靠读代码。
func TestNoSongClearRetriedWhenChannelFull(t *testing.T) {
	for _, tc := range []struct {
		name string
		free int
	}{{"通道满", 0}, {"只剩一个空位", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			evs := sessionEventsChoked(t, 11800*time.Millisecond, 15*time.Second, tc.free, func(e time.Duration) (*cdp.ExtractionData, error) {
				switch {
				case e < 10*time.Second:
					return playingAt(e), nil
				case e < 30*time.Second:
					return noSong, nil
				}
				return nil, errLost
			})
			if got := between(evs, 10*time.Second+time.Nanosecond, sessionLimit); len(got) != 2 ||
				got[0] != waitingNoSong || got[1] != player.EventClearSongData {
				t.Fatalf("通道腾出后应先 waiting_song、后 clear_song_data，各一次，实为 %q", got)
			}
		})
	}
}

// 清空没能入队期间宣告了新歌：待发的清空作罢（新歌的宣告覆盖缓存），不在之后某次读到没有歌曲时提前发出。
// 脚本：12s 到期时通道满（11.8s～13s），13s 起是新歌，20s 起再次没有歌曲，应到 22s 才报 waiting_song 并清空。
//
// 变异自证：宣告新歌时不复位待发的清空，本例因 20s 那一拍就报 waiting_song 并清空变红。
func TestNoSongPendingClearDroppedOnNewSong(t *testing.T) {
	evs := sessionEventsChoked(t, 11800*time.Millisecond, 13*time.Second, 0, func(e time.Duration) (*cdp.ExtractionData, error) {
		switch {
		case e < 10*time.Second:
			return playingAt(e), nil
		case e < 13*time.Second, e >= 20*time.Second && e < 30*time.Second:
			return noSong, nil
		case e < 20*time.Second:
			return songB(float32((e - 13*time.Second) / time.Second)), nil
		}
		return nil, errLost
	})
	var got []timedEvent
	for _, e := range evs {
		if s := e.String(); s == waitingNoSong || s == player.EventClearSongData {
			got = append(got, e)
		}
	}
	if len(got) != 2 || got[0].String() != waitingNoSong || got[0].at <= 22*time.Second || got[1].at <= 22*time.Second {
		t.Fatalf("应只在 22s 到期后报一次 waiting_song 并清空一次，实为 %v", got)
	}
	if count(evs, playingB) != 1 {
		t.Fatalf("新歌应宣告一次：%q", between(evs, 0, sessionLimit))
	}
}

// 换歌时短于容忍期的没有歌曲（最后一拍距起点 noSongTolerance-200ms），随后是新歌：不报 waiting_song、
// 不清空，新歌照常宣告。
//
// 变异自证：去掉容忍期、容忍期少一拍（同上），本例都因出现 waiting_song 与 clear_song_data 变红。
func TestNoSongShortGapThenNextSong(t *testing.T) {
	gapEnd := 10*time.Second + noSongTolerance // 这一拍起是新歌
	evs := sessionEvents(t, func(e time.Duration) (*cdp.ExtractionData, error) {
		switch {
		case e < 10*time.Second:
			return playingAt(e), nil
		case e < gapEnd:
			return noSong, nil
		case e < 30*time.Second:
			return songB(float32((e - gapEnd) / time.Second)), nil
		}
		return nil, errLost
	})
	if n, c := count(evs, waitingNoSong), count(evs, player.EventClearSongData); n != 0 || c != 0 {
		t.Fatalf("短于容忍期不应报 waiting_song 或清空，实为 %d 条、%d 条：%q", n, c, between(evs, 0, sessionLimit))
	}
	if got := between(evs, gapEnd, gapEnd+time.Nanosecond); len(got) == 0 || got[0] != playingB {
		t.Fatalf("新歌到来的那一拍应宣告新歌，实为 %q", got)
	}
}

// 反例：短于容忍期的没有歌曲之后同一首继续：不重新宣告、不报 waiting_song、不清空。
//
// 变异自证：去掉容忍期，本例因清空并重新宣告变红。
func TestNoSongSameSongGoesOn(t *testing.T) {
	evs := sessionEvents(t, func(e time.Duration) (*cdp.ExtractionData, error) {
		switch {
		case e >= 30*time.Second:
			return nil, errLost
		case e >= 10*time.Second && e < 11*time.Second:
			return noSong, nil
		}
		return playingAt(e), nil
	})
	if at, _ := allLyrics(evs); len(at) != 1 || count(evs, playingSong) != 1 ||
		count(evs, waitingNoSong) != 0 || count(evs, player.EventClearSongData) != 0 {
		t.Fatalf("同一首继续不应清空或重新宣告：%q", between(evs, 0, sessionLimit))
	}
}

// 反例：几段没有歌曲各自短于容忍期，之间各有一拍读到同一首歌，合计远超容忍期：计时每次读到歌曲即清零，不清空。
//
// 变异自证：删掉读到歌曲时的 `noSongSince = time.Time{}`，本例因在第二段里清空变红；去掉容忍期同样变红。
func TestNoSongToleranceRestartsOnSong(t *testing.T) {
	gap := noSongTolerance - 400*time.Millisecond
	evs := sessionEvents(t, func(e time.Duration) (*cdp.ExtractionData, error) {
		if e >= 30*time.Second {
			return nil, errLost
		}
		// 10s 起每段 gap 没有歌曲、接一拍（200ms）歌曲，共四段。
		if s := e - 10*time.Second; s >= 0 && s < 4*(gap+200*time.Millisecond) && s%(gap+200*time.Millisecond) < gap {
			return noSong, nil
		}
		return playingAt(e), nil
	})
	if n, c := count(evs, waitingNoSong), count(evs, player.EventClearSongData); n != 0 || c != 0 {
		t.Fatalf("各段短于容忍期不应清空，实为 waiting_song %d 条、clear_song_data %d 条", n, c)
	}
}

// 会话开始时没有歌曲：第一拍即报 waiting_song，持续没有歌曲不重发，也不发 clear_song_data（Start 连接前已发过）；
// 歌曲到来照常宣告。
//
// 变异自证：报状态的条件去掉 `lastMediaID == ""`（只在清空时报），本例因没有 waiting_song 变红；状态改用 p.Emit
// 直接发，本例因每拍重发变红。
func TestNoSongAtSessionStartReportsAtOnce(t *testing.T) {
	evs := sessionEvents(t, func(e time.Duration) (*cdp.ExtractionData, error) {
		switch {
		case e < 30*time.Second:
			return noSong, nil
		case e < 40*time.Second:
			return playingAt(e - 30*time.Second), nil
		}
		return nil, errLost
	})
	if got := between(evs, 0, 30*time.Second); len(got) != 1 || got[0] != waitingNoSong || evs[0].at != 0 {
		t.Fatalf("会话开始时没有歌曲应当即报一次 waiting_song，此外不发事件，实为 %q", got)
	}
	if got := between(evs, 30*time.Second, 30*time.Second+time.Nanosecond); len(got) == 0 || got[0] != playingSong {
		t.Fatalf("歌曲到来应宣告，实为 %q", got)
	}
}

// 反例：换歌时 mediaId 先变、歌名晚到（这一段长于容忍期）不算没有歌曲，不清空；歌名到了照常宣告新歌。
//
// 变异自证：没有歌曲的判据改成只看歌名为空（`data.Name == ""`），本例因清空变红。
func TestNoSongPendingNameNotNoSong(t *testing.T) {
	evs := sessionEvents(t, func(e time.Duration) (*cdp.ExtractionData, error) {
		switch {
		case e < 10*time.Second:
			return playingAt(e), nil
		case e < 15*time.Second:
			d := songB(0)
			d.Name, d.Artists = "", nil
			return d, nil
		case e < 30*time.Second:
			return songB(float32((e - 15*time.Second) / time.Second)), nil
		}
		return nil, errLost
	})
	if n, c := count(evs, waitingNoSong), count(evs, player.EventClearSongData); n != 0 || c != 0 {
		t.Fatalf("歌名未到不是没有歌曲，实为 waiting_song %d 条、clear_song_data %d 条", n, c)
	}
	if got := between(evs, 15*time.Second, 15*time.Second+time.Nanosecond); len(got) == 0 || got[0] != playingB {
		t.Fatalf("歌名到了应宣告新歌，实为 %q", got)
	}
}

// 清空之后同一首歌再出现：按换歌重新宣告（all_lyrics 与 playing 各再发一次）；它不是本会话第一首，落锚照常过
// 新鲜度闸，再出现那一拍的读数不采纳、锚 0，下一个采样到来按前跳纠正。
//
// 变异自证：清空分支整个删掉、清空时不复位 lastMediaID，本例都因没有重新宣告变红；清空之后再来的歌算第一首
// （`firstSong := !announced || lastMediaID == ""`）、清空时不复位 pendingID，本例都因 all_lyrics 带 15s 变红。
func TestNoSongClearedSongAnnouncedAgain(t *testing.T) {
	evs := sessionEvents(t, func(e time.Duration) (*cdp.ExtractionData, error) {
		switch {
		case e < 10*time.Second:
			return playingAt(e), nil
		case e < 15*time.Second:
			return noSong, nil
		case e < 30*time.Second:
			return playingAt(e), nil
		}
		return nil, errLost
	})
	at, pos := allLyrics(evs)
	if len(at) != 2 || at[1] != 15*time.Second || count(evs, playingSong) != 2 {
		t.Fatalf("清空之后同一首应在 15s 重新宣告，all_lyrics 时刻 %v：%q", at, between(evs, 10*time.Second, 17*time.Second))
	}
	if pos[1] != 0 {
		t.Fatalf("清空之后再来的歌不是第一首，再出现那一拍的读数不应采纳，all_lyrics 位置 %v", pos[1])
	}
}

// 反例：会话第一首在中途（61s 处）：照常按读数落锚，不锚 0（服务在歌曲中途启动是最常见的开播姿势）。
//
// 变异自证：announced 初值为真（第一首也过新鲜度闸），本例因 all_lyrics 带 0 变红。
func TestNoSongFirstSongMidwayAnchorsAtReading(t *testing.T) {
	evs := sessionEvents(t, func(e time.Duration) (*cdp.ExtractionData, error) {
		if e >= 5*time.Second {
			return nil, errLost
		}
		return song(61+float32(e/time.Second), true), nil
	})
	if _, pos := allLyrics(evs); len(pos) != 1 || pos[0] != 61 {
		t.Fatalf("会话第一首应按读数 61s 落锚，all_lyrics 位置 %v", pos)
	}
}
