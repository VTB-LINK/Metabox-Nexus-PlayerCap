package sodamusic

// 本文件守 runSession 读到没有歌曲（mediaId 与歌名都为空）时的状态（issue #193；#231，设计
// doc/design/converge-player-behavior-231-design.md 4.3 第 1～3 条）：
//   - 已宣告过歌曲：当拍报 waiting_song（no_song）并清歌曲数据，先状态后清空，只一次，持续没有歌曲不重发；不按时长保留上一首；
//   - 会话开始时没有歌曲：当拍报 waiting_song 并清歌曲数据（上一次连接宣告过的歌可能仍留在画面上），只一次；
//   - 清空的两条事件没能入队（通道满）时在没有歌曲的拍上逐拍补发，先状态后清空；补发之前读到了歌曲即不再补；waiting_song
//     已入队而本拍被去重（中间的歌名状态因通道满被丢弃）不挡清空；
//   - 反例：一拍没有歌曲随后换到新歌或同一首继续、几段短暂的没有歌曲，每段都当拍清空，歌曲回来按换歌重新宣告；
//     mediaId 已变、歌名未到（换歌过渡）不算没有歌曲；
//   - 清空之后同一首歌再出现按换歌重新宣告，落锚照常过新鲜度闸（不算本会话第一首）；会话第一首在中途照常按读数落锚。
//
// 真机未覆盖：汽水会自动填充播放列表，正常操作碰不到没有歌曲（issue #193）。客户端是 exitrelease_test.go 的
// scriptClient，会话在 testing/synctest 的泡里跑，轮询的 time.Sleep 走假时钟。
//
// 变异自证（在临时副本里逐条注入 sodamusic.go、跑本文件、还原，均实测）记在各用例的注释里。

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

// clearsFollowStatus 报告每条 waiting_song 是否都紧跟一条 clear_song_data、每条 clear_song_data 是否都紧跟在 waiting_song 之后。
func clearsFollowStatus(evs []timedEvent) bool {
	var seq []string
	for _, e := range evs {
		if s := e.String(); s == waitingNoSong || s == player.EventClearSongData {
			seq = append(seq, s)
		}
	}
	if len(seq)%2 != 0 {
		return false
	}
	for i := 0; i < len(seq); i += 2 {
		if seq[i] != waitingNoSong || seq[i+1] != player.EventClearSongData {
			return false
		}
	}
	return true
}

// 已宣告的歌之后一直没有歌曲：没有歌曲的第一拍即先报 waiting_song、后清空，各一次；之前与之后不发任何事件。
// 播放中与暂停中各跑一遍。
//
// 变异自证（均实测）：恢复 2 s 等待（`noSongSince` 计时满 2 s 才报状态并清空），两个子例因清空晚于 10s 变红；状态改为每拍经
// TryEmit 发，因每拍重发变红；先清空后报状态，因顺序变红；清空时不复位 lastMediaID，因每拍重复清空变红。
// 未能自证的一处：删掉清空分支（不复位已宣告的歌），本例仍绿——会话开始时置的待发清空没被消耗，没有歌曲时照样先状态后清空
// 各一次；这一变异由 TestNoSongOneTickThenSameSong、TestNoSongEachGapClears、TestNoSongClearedSongAnnouncedAgain 守（实测变红）。
func TestNoSongAfterSongClearsAtOnce(t *testing.T) {
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
			clearAt := 10 * time.Second
			if got := between(evs, clearAt, clearAt+time.Nanosecond); len(got) != 2 ||
				got[0] != waitingNoSong || got[1] != player.EventClearSongData {
				t.Fatalf("没有歌曲的第一拍应先报 waiting_song 再清空，实为 %q", got)
			}
			if got := between(evs, clearAt+time.Nanosecond, sessionLimit); len(got) != 0 {
				t.Fatalf("清空之后持续没有歌曲不应再发事件，实为 %q", got)
			}
		})
	}
}

// 没有歌曲的第一拍事件通道已满，或只剩一个空位、被 waiting_song 占去：没能入队的 waiting_song 与 clear_song_data 逐拍补发，
// 通道腾出后各入队一次，先状态后清空。脚本：10s 起没有歌曲，9.8s～15s 通道塞满（路由消费跟不上）。
//
// 变异自证（均实测）：清空挪到状态之前尝试、且不等状态入队，两个子例都因顺序变红；状态改为每拍经 TryEmit 发，两个子例都因
// 重发变红；清空时不复位 lastMediaID，两个子例都因重复清空变红；清空改为只在本拍 EmitStatusIfChanged 返回真时才发，「只剩
// 一个空位」因 waiting_song 入队之后被去重、清空一直等不到而变红。
// 未能自证的一处：「状态入队之后才发清空」这道闸只在路由恰好于两次发送之间腾出空位时起作用，桩里通道只在取数时
// 收取，造不出这一时序；单去掉这道闸（只看 clearData），本文件各用例仍绿（实测），这一点靠读代码。
func TestNoSongClearRetriedWhenChannelFull(t *testing.T) {
	for _, tc := range []struct {
		name string
		free int
	}{{"通道满", 0}, {"只剩一个空位", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			evs := sessionEventsChoked(t, 9800*time.Millisecond, 15*time.Second, tc.free, func(e time.Duration) (*cdp.ExtractionData, error) {
				switch {
				case e < 10*time.Second:
					return playingAt(e), nil
				case e < 30*time.Second:
					return noSong, nil
				}
				return nil, errLost
			})
			if got := between(evs, 10*time.Second, sessionLimit); len(got) != 2 ||
				got[0] != waitingNoSong || got[1] != player.EventClearSongData {
				t.Fatalf("通道腾出后应先 waiting_song、后 clear_song_data，各一次，实为 %q", got)
			}
		})
	}
}

// 清空没能入队期间读到了新歌：待发的清空作罢，不在新歌宣告之后发出（那会清掉新歌）。脚本：10s 起没有歌曲而通道满
// （9.8s～13s），13s 起是新歌，20s 起再次没有歌曲，应到 20s 那一拍才报 waiting_song 并清空，各一次。
//
// 变异自证（均实测）：待发的状态与清空挪到每拍末尾补发（不限没有歌曲的拍），本例因 13s 宣告新歌之后即报 waiting_song 并清空
// 变红；先清空后报状态、状态改为每拍经 TryEmit 发、清空时不复位 lastMediaID，同样变红。
func TestNoSongPendingClearDroppedOnNewSong(t *testing.T) {
	evs := sessionEventsChoked(t, 9800*time.Millisecond, 13*time.Second, 0, func(e time.Duration) (*cdp.ExtractionData, error) {
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
	if len(got) != 2 || got[0].String() != waitingNoSong || got[0].at <= 20*time.Second || got[1].at <= 20*time.Second {
		t.Fatalf("应只在 20s 那一拍报一次 waiting_song 并清空一次（取到的时刻晚一拍），实为 %v", got)
	}
	if count(evs, playingB) != 1 {
		t.Fatalf("新歌应宣告一次：%q", between(evs, 0, sessionLimit))
	}
}

// 审查给的反例（#231 代码审）：宣告过《鲜花》；10s 没有歌曲时通道只剩一个空位，waiting_song 入队而 clear_song_data 没能入队；
// 通道仍满时 10.2s～11s 短暂出现《乙曲》，它的歌名状态与歌曲事件全被丢弃，最近一次入队的状态仍是 waiting_song；11s 起再次
// 没有歌曲，15s 通道腾出。这时的 waiting_song 被去重，清空不能因此一直等下去：通道腾出后应补上 clear_song_data，在
// waiting_song 之后，各一次。
//
// 变异自证（均实测）：清空改为只在本拍 EmitStatusIfChanged 返回真时才发（被去重当作没入队），本例因一直没有清空变红；恢复 2 s
// 等待、先清空后报状态、状态改为每拍经 TryEmit 发、清空时不复位 lastMediaID，同样变红。
func TestNoSongClearNotStuckByDedupedStatus(t *testing.T) {
	evs := sessionEventsChoked(t, 9800*time.Millisecond, 15*time.Second, 1, func(e time.Duration) (*cdp.ExtractionData, error) {
		switch {
		case e < 10*time.Second:
			return playingAt(e), nil
		case e < 10*time.Second+200*time.Millisecond:
			return noSong, nil
		case e < 11*time.Second:
			return songB(0), nil
		case e < 30*time.Second:
			return noSong, nil
		}
		return nil, errLost
	})
	if got := between(evs, 10*time.Second, sessionLimit); len(got) != 2 ||
		got[0] != waitingNoSong || got[1] != player.EventClearSongData {
		t.Fatalf("通道腾出后应先 waiting_song、后 clear_song_data，各一次，实为 %q", got)
	}
}

// 反例：一拍没有歌曲（10s），随后是新歌：那一拍即报 waiting_song 并清空，新歌照常宣告。
//
// 变异自证（均实测）：恢复 2 s 等待，本例因没有 waiting_song 与 clear_song_data 变红；先清空后报状态，因顺序变红。
func TestNoSongOneTickThenNextSong(t *testing.T) {
	gapEnd := 10*time.Second + 200*time.Millisecond // 这一拍起是新歌
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
	if got := between(evs, 10*time.Second, gapEnd); len(got) != 2 || got[0] != waitingNoSong || got[1] != player.EventClearSongData {
		t.Fatalf("没有歌曲的那一拍应先报 waiting_song 再清空，实为 %q", got)
	}
	if got := between(evs, gapEnd, gapEnd+time.Nanosecond); len(got) == 0 || got[0] != playingB {
		t.Fatalf("新歌到来的那一拍应宣告新歌，实为 %q", got)
	}
}

// 反例：一拍没有歌曲（10s），随后同一首继续：那一拍即清空，同一首按换歌重新宣告（all_lyrics 与 playing 各再发一次）。
//
// 变异自证（均实测）：恢复 2 s 等待，本例因没有清空、没有重新宣告变红；先清空后报状态、清空时不复位 lastMediaID、删掉清空
// 分支，同样变红。
func TestNoSongOneTickThenSameSong(t *testing.T) {
	evs := sessionEvents(t, func(e time.Duration) (*cdp.ExtractionData, error) {
		switch {
		case e >= 30*time.Second:
			return nil, errLost
		case e >= 10*time.Second && e < 10*time.Second+200*time.Millisecond:
			return noSong, nil
		}
		return playingAt(e), nil
	})
	if at, _ := allLyrics(evs); len(at) != 2 || count(evs, playingSong) != 2 ||
		count(evs, waitingNoSong) != 1 || count(evs, player.EventClearSongData) != 1 || !clearsFollowStatus(evs) {
		t.Fatalf("一拍没有歌曲应清空一次、同一首重新宣告一次：%q", between(evs, 0, sessionLimit))
	}
}

// 反例：几段没有歌曲（每段 1.6s）之间各有一拍读到同一首歌，共四段：每段都当拍清空、同一首每次重新宣告。
//
// 变异自证（均实测）：恢复 2 s 等待，本例因一次都没有清空变红；先清空后报状态、状态改为每拍经 TryEmit 发、清空时不复位
// lastMediaID、删掉清空分支，同样变红。
func TestNoSongEachGapClears(t *testing.T) {
	gap := 1600 * time.Millisecond
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
	if n, c := count(evs, waitingNoSong), count(evs, player.EventClearSongData); n != 4 || c != 4 || !clearsFollowStatus(evs) {
		t.Fatalf("四段没有歌曲应各报一次 waiting_song、各清空一次且状态在前，实为 waiting_song %d 条、clear_song_data %d 条", n, c)
	}
	if n := count(evs, playingSong); n != 5 {
		t.Fatalf("同一首应宣告 5 次（会话开始一次，每段之后各一次），实为 %d 次", n)
	}
}

// 会话开始时没有歌曲：第一拍即报 waiting_song、随即清空（上一次连接宣告过的歌可能仍留在画面上，设计 4.5），各一次，
// 持续没有歌曲不重发；歌曲到来照常宣告。
//
// 变异自证（均实测）：会话开始时不置待发的清空（`clearData := false`），本例因没有 clear_song_data 变红；
// 状态改为每拍经 TryEmit 发，本例因每拍重发变红；先清空后报状态，本例因顺序变红。
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
	if got := between(evs, 0, 30*time.Second); len(got) != 2 || got[0] != waitingNoSong || got[1] != player.EventClearSongData || evs[1].at != 0 {
		t.Fatalf("会话开始时没有歌曲应当即报一次 waiting_song、随即清空一次，此外不发事件，实为 %q", got)
	}
	if got := between(evs, 30*time.Second, 30*time.Second+time.Nanosecond); len(got) == 0 || got[0] != playingSong {
		t.Fatalf("歌曲到来应宣告，实为 %q", got)
	}
}

// 反例：换歌时 mediaId 先变、歌名晚到（这一段 5s）不算没有歌曲，不清空；歌名到了照常宣告新歌。
//
// 变异自证（实测）：没有歌曲的判据改成只看歌名为空（`data.Name == ""`），本例因清空变红。
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
// 变异自证（均实测）：清空分支整个删掉、清空时不复位 lastMediaID，本例都因没有重新宣告变红；清空之后再来的歌算第一首
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
// 变异自证（实测）：announced 初值为真（第一首也过新鲜度闸），本例因 all_lyrics 带 0 变红。
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
