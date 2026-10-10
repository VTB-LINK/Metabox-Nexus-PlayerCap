package applemusic

// 本文件只测会话里的位置采样（position_sample，issue #70）：播放中按间隔发、带的是时钟的整曲位置，
// 别的时候不发。
//
// 服务端在两次采样之间按墙钟外推位置。clock 在读数走得比墙钟慢时把位置夹回读数允许的范围、不发事件
// （clock.observe 返回 evNone），无歌词的歌发过 index:-1 之后又没有带位置的事件：不报采样，服务端的
// 位置会一直超前下去。
//
// 服务端一侧用 serverSample.extrapolate 复述 server.PlayerState.livePosition 在 playing 状态下的算式，
// 不经真实的 Server：它的采样时刻取自 time.Now()，本包的假时钟注入不进去。灌同样节奏的采样给真实
// Server 的回放在 server/positionsample_test.go 的 TestPositionSampleBoundsDrift。

import (
	"math"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

// serverSample 是服务端缓存里的一次位置采样。
type serverSample struct {
	pos float64
	at  time.Time
}

// extrapolate 复述 livePosition：playing 时 = 采样位置 + 经过的墙钟，钳到时长。
func (s serverSample) extrapolate(now time.Time, dur float64) float64 {
	return math.Min(s.pos+now.Sub(s.at).Seconds(), dur)
}

// positionOf 取事件携带的整曲位置；不带位置的事件返回 false。
func positionOf(e player.Event) (float64, bool) {
	switch d := e.Data.(type) {
	case *player.LyricUpdate:
		return float64(d.Position), true
	case *player.AllLyricsData:
		return float64(d.Position), true
	case *player.PlaybackTimeInfo:
		return float64(d.Position), true
	}
	return 0, false
}

// issue #70 的推演：时长 300s 的无歌词歌曲，位置读数每 1.5s 才前进一格、连续 40 次（短于暂停门槛，
// 不判暂停、不发事件）。不报采样时，60s 后服务端外推的位置超前播放器一侧 20.00s（实现之前的同一回放）。
// 每 5 秒一条采样之后，超前量限制在一个间隔内累计的量：实测段内最大 1.90s。
//
// 采样带的是 clk.live，与 lyric_update、playback_resume 的 position 同一口径，不含 offset：本例把
// offset 设成 2s，带上 offset 的采样会与 clk.live 差出 2s。
//
// 变异自证：删掉 tick 里对 emitPositionSample 的调用，卡顿段内没有采样、段末超前 20.00s 而红；
// 把传入的位置改成 `s.clk.live(now) + float64(s.offsetSec)`，采样与 clk.live 不等而红。
func TestSessionPositionSamplesBoundDrift(t *testing.T) {
	const (
		dur      = 300.0
		maxAhead = 2.5 // 段内超前量的上界；实测 1.90
	)
	h := newHarness(t)
	h.s.offsetSec = 2
	h.f.songs = np("无词")
	h.setTwin(dur, 100, true)
	h.s.twin = realTwin

	var last serverSample // 服务端最近一次采样：最近一条带位置的事件
	var sampledAt []time.Time
	seen := 0
	// tick 走一轮，把这一轮发出的事件记到服务端一侧，返回这一轮的时刻。
	tick := func() time.Time {
		at := h.now
		h.run(step)
		for _, e := range h.evs[seen:] {
			pos, ok := positionOf(e)
			if !ok {
				continue
			}
			last = serverSample{pos: pos, at: at}
			if e.Type != player.EventPositionSample {
				continue
			}
			sampledAt = append(sampledAt, at)
			info := e.Data.(*player.PlaybackTimeInfo)
			if want := float32(h.s.clk.live(at)); info.Position != want {
				t.Fatalf("采样的位置 = %v，应是此刻的 clk.live %v（不含 offset）", info.Position, want)
			}
			if want := player.ClampProgress(info.Position, dur); info.Progress != want {
				t.Fatalf("采样的进度 = %v，应是 %v", info.Progress, want)
			}
		}
		seen = len(h.evs)
		return at
	}

	// 正常播放段：读数每秒一格，直到 index:-1 发出。记下最后一格的时刻，卡顿段从它接着数。
	raw := h.f.twins[realTwin].Pos
	var flipAt time.Time
	for h.now.Sub(h.baseAt) < lyricGrace+2*time.Second {
		at := tick()
		if p := h.f.twins[realTwin].Pos; p != raw {
			raw, flipAt = p, at
		}
	}
	if n := len(verdicts(h.evs)); n != 1 {
		t.Fatalf("前置：应已发出一条 index:-1，got %d", n)
	}

	// 卡顿段：读数由本测试手动推进，每 1.5s 一格、共 40 格，量到第 40 格那一轮为止。
	h.play = false
	sampledAt = nil
	var end time.Time
	var worst float64
	for flips := 0; flips < 40; {
		if next := flipAt.Add(1500 * time.Millisecond); !h.now.Before(next) {
			raw, flipAt = raw+1, next
			h.f.twins[realTwin] = mem.Twin{Dur: dur, Pos: raw}
			flips++
		}
		end = tick()
		worst = math.Max(worst, last.extrapolate(end, dur)-h.s.clk.live(end))
	}
	if !h.s.clk.playing || count(h.evs, player.EventPlaybackPause) != 0 {
		t.Fatal("前置：卡顿段内不应判暂停")
	}

	ahead := last.extrapolate(end, dur) - h.s.clk.live(end)
	t.Logf("卡顿段内位置采样 %d 条；段末播放器一侧 %.2fs，服务端外推超前 %.2fs，段内最大超前 %.2fs",
		len(sampledAt), h.s.clk.live(end), ahead, worst)
	if len(sampledAt) < 11 {
		t.Fatalf("卡顿段约 60s，应有不少于 11 条位置采样，got %d", len(sampledAt))
	}
	for i := 1; i < len(sampledAt); i++ {
		if gap := sampledAt[i].Sub(sampledAt[i-1]); gap != player.PositionSampleInterval {
			t.Errorf("相邻两条采样相隔 %v，应为 %v", gap, player.PositionSampleInterval)
		}
	}
	if worst > maxAhead {
		t.Errorf("服务端外推最多超前播放器一侧 %.2fs，应不超过 %.1fs", worst, maxAhead)
	}
}

// playingSession 起一个已宣告、在放的会话：无歌词，时长 300s，从 100s 起，已播 8s。
func playingSession(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.f.songs = np("无词")
	h.setTwin(300, 100, true)
	h.s.twin = realTwin
	h.run(8 * time.Second)
	if !h.s.announced || !h.s.clk.playing {
		t.Fatalf("前置：应已宣告并在放，announced=%v playing=%v", h.s.announced, h.s.clk.playing)
	}
	return h
}

// samplesAfter 返回最后一条 typ 事件之后的位置采样条数；没有 typ 事件返回 -1。
func samplesAfter(evs []player.Event, typ string) int {
	n := -1
	for _, e := range evs {
		switch {
		case e.Type == typ:
			n = 0
		case n >= 0 && e.Type == player.EventPositionSample:
			n++
		}
	}
	return n
}

// 采样只在「本曲已宣告、不在加载中、时钟在走」时发。暂停中位置已由 playback_pause 给定；未宣告与
// 加载中，服务端缓存里的时长与歌词还是上一首的，采样会把新歌的位置配给它。
//
// 变异自证：emitPositionSample 的条件去掉 `!s.clk.playing`，「暂停中」「位置结构丢失」「恢复后」三例红
// （恢复前就有采样）；去掉 `!s.announced`，「未宣告」一例红；去掉 `s.loading`，「加载中」一例红
// （sessionstatus_test.go 的 TestSessionNoEventsWhileLoading 同红）；删掉 tick 里对它的调用，
// 「播放中」「恢复后」两例红。
func TestSessionPositionSampleOnlyWhilePlaying(t *testing.T) {
	t.Run("播放中按间隔发", func(t *testing.T) {
		h := playingSession(t)
		from := h.mark()
		h.run(20 * time.Second)
		if n := count(h.since(from), player.EventPositionSample); n != 4 {
			t.Fatalf("20s 内应有 4 条位置采样，got %d", n)
		}
	})

	t.Run("暂停中不发", func(t *testing.T) {
		h := playingSession(t)
		h.play = false // 读数停住
		h.run(15 * time.Second)
		if n := samplesAfter(h.evs, player.EventPlaybackPause); n != 0 {
			t.Fatalf("playback_pause 之后不应再有位置采样，got %d（-1 = 没判出暂停）", n)
		}
	})

	t.Run("位置结构丢失后不发", func(t *testing.T) {
		h := playingSession(t)
		h.play = false
		delete(h.f.twins, realTwin)
		h.run(15 * time.Second)
		if n := samplesAfter(h.evs, player.EventPlaybackPause); n != 0 {
			t.Fatalf("位置结构作废（按暂停报）之后不应再有位置采样，got %d（-1 = 没报暂停）", n)
		}
	})

	t.Run("未宣告不发", func(t *testing.T) {
		h := newHarness(t) // 内存里没有「正在播放」存储：位置在走，却取不到标题
		h.setTwin(300, 100, true)
		h.s.twin = realTwin
		h.run(9 * time.Second)
		if h.s.announced || !h.s.clk.playing {
			t.Fatalf("前置：应未宣告而时钟在走，announced=%v playing=%v", h.s.announced, h.s.clk.playing)
		}
		if n := count(h.evs, player.EventPositionSample); n != 0 {
			t.Fatalf("未宣告时不应有位置采样，got %d", n)
		}
	})

	t.Run("加载中不发", func(t *testing.T) {
		h := playingSession(t)
		from := h.mark()
		h.play = false
		h.f.twins[realTwin] = mem.Twin{} // 时长归零：换歌加载中
		h.run(5500 * time.Millisecond)   // 长于一个采样间隔，短于 loadingGrace
		if !h.s.loading || !h.s.announced {
			t.Fatalf("前置：应在加载中且尚未清歌，loading=%v announced=%v", h.s.loading, h.s.announced)
		}
		if n := count(h.since(from), player.EventPositionSample); n != 0 {
			t.Fatalf("加载中不应有位置采样，got %d", n)
		}
	})

	t.Run("恢复后继续发，且不早于 playback_resume", func(t *testing.T) {
		h := playingSession(t)
		h.play = false
		h.run(10 * time.Second)
		from := h.mark()
		h.setTwin(300, h.f.twins[realTwin].Pos, true)
		h.run(8 * time.Second)
		resumeAt, sampleAt := -1, -1
		for i, e := range h.since(from) {
			switch {
			case e.Type == player.EventPlaybackResume && resumeAt < 0:
				resumeAt = i
			case e.Type == player.EventPositionSample && sampleAt < 0:
				sampleAt = i
			}
		}
		if resumeAt < 0 || sampleAt < resumeAt {
			t.Fatalf("恢复后应先有 playback_resume、再有位置采样：resume 在第 %d 条，采样在第 %d 条", resumeAt, sampleAt)
		}
	})
}
