package kuwo

// This file tests only the song-switch wiring of #198
// (doc/design/kuwo-switch-start-transient-design.md §5.1 A-1, A-2, A-5, §7
// items 13-22): runSession keeps the corrected source through a switch's no-song
// and settling ticks and has it observe the output there and on failed core
// reads, so the new song's opening is filtered with the tracked crossing. The
// filter's own decisions are tested in package pos (switchstart_test.go).

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

var swT0 = time.Unix(5000, 0)

const (
	swRidA = "MUSIC_1"
	swRidB = "MUSIC_2"
)

// switchWorld scripts a switch for runSession through a real correctedSource on
// a virtual clock. A runSession tick calls the bridge, the core and the output,
// each at most once and in that order, so a call that does not come after the
// previous one in that order starts a new tick; tick k is at swT0 + k*50 ms.
// The bridge, core and output are functions of the tick, so the scripts hold
// whichever calls the session makes on a tick (the bridge every 200 ms of real
// time, or every tick while settling or holding).
type switchWorld struct {
	mu     sync.Mutex
	tick   int
	last   int // order of the latest call: 0 bridge, 1 core, 2 output
	bridge func(k int) (cdp.Snapshot, error)
	core   func(k int) (rid string, ms int32) // rid "" fails the read
	out    func(k int) pos.OutputState
	lag    func(k int) pos.LagSample // nil: invalid samples
	// bare has open return the core reader alone, as withLag does when OpenLag
	// fails.
	bare    bool
	opens   int
	closes  int
	settled int  // output reads on ticks whose bridge call failed
	failed  bool // this tick's bridge call failed
	warned  bool
}

func (w *switchWorld) step(order int) int {
	if order <= w.last {
		w.tick++
		w.failed = false
	}
	w.last = order
	return w.tick
}

func swAt(k int) time.Time { return swT0.Add(time.Duration(k) * 50 * time.Millisecond) }

func (w *switchWorld) now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return swAt(w.tick)
}

func (w *switchWorld) ticks() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tick
}

func (w *switchWorld) Snapshot(context.Context) (cdp.Snapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	snap, err := w.bridge(w.step(0))
	if err != nil {
		w.failed = true
	}
	return snap, err
}

type swCore struct{ w *switchWorld }

func (c swCore) Read(expected string, _ bool) (pos.Sample, error) {
	w := c.w
	w.mu.Lock()
	defer w.mu.Unlock()
	k := w.step(1)
	rid, ms := w.core(k)
	if rid == "" || rid != pos.RIDWithPrefix(expected) {
		return pos.Sample{}, pos.ErrSongChanged
	}
	return pos.Sample{RID: rid, PositionMs: ms, CoreMs: ms, DurationMs: 300000, ObservedAt: swAt(k)}, nil
}

func (c swCore) Close() {
	c.w.mu.Lock()
	c.w.closes++
	c.w.mu.Unlock()
}

type swLag struct{ w *switchWorld }

func (l swLag) Read() (pos.LagSample, pos.OutputState, error) {
	w := l.w
	w.mu.Lock()
	defer w.mu.Unlock()
	k := w.step(2)
	if w.failed {
		w.settled++
	}
	var ls pos.LagSample
	if w.lag != nil {
		ls = w.lag(k)
	}
	return ls, w.out(k), nil
}

func (l swLag) ReadOutput() pos.OutputState {
	w := l.w
	w.mu.Lock()
	defer w.mu.Unlock()
	k := w.step(2)
	if w.failed {
		w.settled++
	}
	return w.out(k)
}

func (l swLag) Close() {}

// outLag is a stubLag that also reads the output state alone; its reads count
// only Read.
type outLag struct{ *stubLag }

func (l outLag) ReadOutput() pos.OutputState { return l.out }

func (w *switchWorld) open() (positionSource, error) {
	w.mu.Lock()
	w.opens++
	bare := w.bare
	w.mu.Unlock()
	if bare {
		return swCore{w}, nil
	}
	return newCorrectedSource(swCore{w}, swLag{w}, &w.warned, 50), nil
}

func (w *switchWorld) counts() (opens, closes, settled int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.opens, w.closes, w.settled
}

// runWorld runs a session on the world until its clock reaches tick n, then
// stops it and returns the events; the no-song tolerance is widened so real-time
// scheduling cannot cut a scripted gap short.
func runWorld(t *testing.T, w *switchWorld, n int) []player.Event {
	t.Helper()
	return runWorldWith(t, w, n, 5*time.Second)
}

// announced returns the position, in ms, of song name's first all_lyrics and the
// events from its song_info_update on.
func announced(t *testing.T, events []player.Event, name string) (int32, []player.Event) {
	t.Helper()
	i := songAt(events, name)
	if i < 0 {
		t.Fatalf("%s never announced: %v", name, eventTypes(events))
	}
	for _, e := range events[i:] {
		if d, ok := e.Data.(*player.AllLyricsData); ok {
			return int32(math.Round(float64(d.Position) * 1000)), events[i:]
		}
	}
	t.Fatalf("%s has no all_lyrics", name)
	return 0, nil
}

func resumesMs(events []player.Event) []int32 {
	return eventsMs(ofType(events, player.EventPlaybackResume))
}

// Scripting helpers. A plays on its own segment (base 0, fed far along) until
// aLast; B's output then falls and fills as fed gives, then B's core stalls at
// 1515 until its sound, which starts L = 106 ms after the crossing, reaches it.

func songA(k int) int32 { return int32(150000 + 50*k) }

func aOut(k int) pos.OutputState { return dsOutK(0, int64(150000+50*k)) }

// bCore is B's raw core at tick k given the crossing tick: the open ladder's
// stall at 1515, then 1:1 with the sound.
func bCore(k, crossTick int) int32 {
	sound := int64(k-crossTick)*50 - 106
	if sound > 1515 {
		return int32(sound)
	}
	return 1515
}

// fedFrom builds B's output from a fall tick and (tick, fed ms) points; between
// points fed holds, and after the last one it grows 50 ms per tick.
func fedFrom(fallTick int, points map[int]int64) func(k int) (pos.OutputState, bool) {
	return func(k int) (pos.OutputState, bool) {
		if k < fallTick {
			return pos.OutputState{}, false
		}
		var fed int64
		lastK := fallTick
		for pk, v := range points {
			if pk <= k && pk >= lastK {
				lastK, fed = pk, v
			}
		}
		if k > lastK && lastK == maxKey(points) {
			fed += int64(k-lastK) * 50
		}
		return dsOutK(0, fed), true
	}
}

func maxKey(m map[int]int64) int {
	n := math.MinInt
	for k := range m {
		n = max(n, k)
	}
	return n
}

func noSongSnap() cdp.Snapshot { return cdp.Snapshot{Status: 1} }

// switchShape is A until aLast, then the bridge's gap (no song and/or
// settling), then B from bFrom with its core at the ladder (first) and the
// stall after; B's output falls at fallTick and fills per points.
type switchShape struct {
	aLast       int
	noSong      [2]int // inclusive tick range, zero for none
	settle      [2]int
	bFrom       int
	firstB      int32 // B's core on bFrom; the stall from bFrom+1 (0: the stall at once)
	crossTick   int
	fallTick    int
	points      map[int]int64
	coreFailing [2]int // B's core read fails on these ticks
}

func (s switchShape) world() *switchWorld {
	b := fedFrom(s.fallTick, s.points)
	in := func(r [2]int, k int) bool { return r[0] != 0 && k >= r[0] && k <= r[1] }
	return &switchWorld{
		last: 2,
		bridge: func(k int) (cdp.Snapshot, error) {
			switch {
			case k <= s.aLast:
				return songSnap(swRidA, "A"), nil
			case in(s.noSong, k):
				return noSongSnap(), nil
			case in(s.settle, k):
				return cdp.Snapshot{}, cdp.ErrSongChanged
			}
			return songSnap(swRidB, "B"), nil
		},
		core: func(k int) (string, int32) {
			if k < s.bFrom {
				return swRidA, songA(k)
			}
			if in(s.coreFailing, k) {
				return "", 0
			}
			if k == s.bFrom && s.firstB != 0 {
				return swRidB, s.firstB
			}
			return swRidB, bCore(k, s.crossTick)
		},
		out: func(k int) pos.OutputState {
			if o, ok := b(k); ok {
				return o
			}
			return aOut(k)
		},
	}
}

// Design §7 item 20 (with 16): the recorded failure shapes. B's output opens and
// crosses while the bridge reports no song (ws/23 愿与愁; ws/25 Middle Of The
// Night, whose first sample is at 600), then B is announced. Kept through the
// gap, the corrected source saw the crossing: B is announced near 0, no
// playback_resume follows, and the reader was opened once.
// 变异自证：无歌分支恢复当拍关闭读取器，两例分别宣告 1515、600（实测）。
func TestSwitchStartAnnouncedFiltered(t *testing.T) {
	cases := []struct {
		name  string
		shape switchShape
	}{
		{"stall at 1515", switchShape{aLast: 4, noSong: [2]int{5, 12}, bFrom: 13, crossTick: 11, fallTick: 8,
			points: map[int]int64{8: 0, 9: 300, 10: 600, 11: 900, 12: 1600}}},
		{"ladder at 600", switchShape{aLast: 4, noSong: [2]int{5, 12}, bFrom: 13, firstB: 600, crossTick: 12, fallTick: 9,
			points: map[int]int64{9: 0, 10: 300, 11: 600, 12: 900}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := c.shape.world()
			events := runWorld(t, w, 50)
			ms, fromB := announced(t, events, "B")
			if ms > 200 {
				t.Fatalf("B announced at %d ms, want <= 200", ms)
			}
			if got := resumesMs(fromB); len(got) != 0 {
				t.Fatalf("playback_resume after B's announcement: %v", got)
			}
			if opens, _, _ := w.counts(); opens != 1 {
				t.Fatalf("reader opened %d times, want 1 (kept through the switch)", opens)
			}
		})
	}
}

// Design §7 item 17 (review round 1, item 1): B's output falls and crosses while
// the bridge's status is settling (cdp.ErrSongChanged), alone or after no song.
// The settling branch observes the output: B is announced near 0, and the output
// was read on failed bridge ticks.
// 变异自证：空状态分支不观测输出，两例宣告 1515（实测）。
func TestSwitchStartSettlingObserved(t *testing.T) {
	cases := []struct {
		name  string
		shape switchShape
	}{
		{"settling then B", switchShape{aLast: 4, settle: [2]int{5, 16}, bFrom: 17, crossTick: 14, fallTick: 11,
			points: map[int]int64{11: 0, 12: 300, 13: 600, 14: 900, 15: 1600}}},
		{"no song, settling, then B", switchShape{aLast: 4, noSong: [2]int{5, 10}, settle: [2]int{11, 18}, bFrom: 19, crossTick: 17, fallTick: 9,
			points: map[int]int64{9: 0, 15: 300, 16: 600, 17: 900, 18: 1600}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := c.shape.world()
			events := runWorld(t, w, 40)
			ms, fromB := announced(t, events, "B")
			if ms > 200 {
				t.Fatalf("B announced at %d ms, want <= 200", ms)
			}
			if got := resumesMs(fromB); len(got) != 0 {
				t.Fatalf("playback_resume after B's announcement: %v", got)
			}
			if _, _, settled := w.counts(); settled < 2 {
				t.Fatalf("output read on %d settling ticks, want the crossing seen there", settled)
			}
		})
	}
}

// Design §7 item 20, delayed anchor (review round 1, item 3): B is announced at
// 0 before its Play; its core reads then fail across the crossing, which only the
// output observation sees. On recovery the tracked anchor is taken on a tick
// after the announcement, the session judges the front end (running 300 ms from
// 0) and sends one playback_resume at the filtered position.
// 变异自证：跟踪锚点一律不置 TransientResync，没有这条 playback_resume（实测）；核心读失败拍不观测输出，恢复拍降级、发 1515 的 resume（实测）。
func TestSwitchStartDelayedAnchorNotified(t *testing.T) {
	shape := switchShape{aLast: 4, noSong: [2]int{5, 7}, bFrom: 8, crossTick: 12, fallTick: 8,
		points:      map[int]int64{8: 0, 9: 313, 10: 600, 11: 700, 12: 914, 13: 1600},
		coreFailing: [2]int{11, 13}}
	w := shape.world()
	inner := w.core
	w.core = func(k int) (string, int32) { // B's own ladder before the stall
		switch k {
		case 8:
			return swRidB, 0
		case 9:
			return swRidB, 313
		case 10:
			return swRidB, 600
		}
		return inner(k)
	}
	events := runWorld(t, w, 40)
	ms, fromB := announced(t, events, "B")
	if ms != 0 {
		t.Fatalf("B announced at %d ms, want 0", ms)
	}
	if got := resumesMs(fromB); len(got) != 1 || got[0] > 50 {
		t.Fatalf("playback_resume after B's announcement: %v, want one near 0", got)
	}
}

// Design §7 item 18 (counterexample): a bare core reader (OpenLag failed) is
// closed at no song and reopened for B, so OpenLag is tried again.
// 变异自证：任何读取器都在无歌时保留，只打开一次（实测）。
func TestSwitchStartBareReaderReopened(t *testing.T) {
	w := switchShape{aLast: 4, noSong: [2]int{5, 8}, bFrom: 9, crossTick: 7, fallTick: 6,
		points: map[int]int64{6: 0, 7: 900}}.world()
	w.bare = true
	runWorld(t, w, 14)
	if opens, closes, _ := w.counts(); opens != 2 || closes < 1 {
		t.Fatalf("bare reader opened %d, closed %d times; want reopened for B", opens, closes)
	}
}

// Design §7 item 19 (counterexample): once no song outlasts the tolerance the
// session clears and the reader is closed, as before; the next song opens a new
// one.
// 变异自证：清空之后仍保留读取器，只打开一次（实测）。
func TestSwitchStartClearCloses(t *testing.T) {
	w := switchShape{aLast: 4, noSong: [2]int{5, 16}, bFrom: 17, crossTick: 15, fallTick: 14,
		points: map[int]int64{14: 0, 15: 900}}.world()
	events := runWorldWith(t, w, 22, 200*time.Millisecond)
	if statusCount(events, "waiting_song") != 1 {
		t.Fatalf("want one waiting_song: %v", eventTypes(events))
	}
	if opens, closes, _ := w.counts(); opens != 2 || closes < 1 {
		t.Fatalf("reader opened %d, closed %d times; want closed at the clear and reopened", opens, closes)
	}
}

// runWorldWith runs the world with the given no-song tolerance; the settling
// tolerance is widened, and the filter and the #185 hold run on the world's clock.
func runWorldWith(t *testing.T, w *switchWorld, n int, noSong time.Duration) []player.Event {
	t.Helper()
	defer func(a, b time.Duration, c, d func() time.Time) {
		transientTolerance, noSongTolerance, trackNow, snapshotNow = a, b, c, d
	}(transientTolerance, noSongTolerance, trackNow, snapshotNow)
	transientTolerance, noSongTolerance = 5*time.Second, noSong
	trackNow, snapshotNow = w.now, w.now
	p := New(0, 50)
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	events := collect(p, stop)
	done := make(chan error, 1)
	go func() { done <- p.runSession(ctx, w, w.open) }()
	deadline := time.After(20 * time.Second)
	for w.ticks() < n {
		select {
		case err := <-done:
			cancel()
			close(stop)
			t.Fatalf("session ended at tick %d: %v", w.ticks(), err)
		case <-deadline:
			cancel()
			close(stop)
			t.Fatalf("clock stuck at tick %d", w.ticks())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		close(stop)
		t.Fatalf("unexpected exit: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	got := events()
	close(stop)
	return got
}

// Design §7 item 21 (#185): A's core falls to 0 on a switch while the bridge still
// reports A, and #185 holds those ticks; then no song, then B. (a) B's output
// opens after the fall: filtered. (b) It opens on the held ticks and crosses in
// the no-song window: filtered (the crossing is after A's last sample, a held
// one). (c) It crosses on the held ticks: not taken (H2'b), B is announced at
// 1515 as before and the opening passthrough is logged. A publishes nothing after
// the fall in all three.
// 变异自证：条件 4 改为只比较上一首最后一个 C_raw > 0 的样本（设计 §5.1 的候选判据，不计持有的回落拍），(c) 宣告 344 ms 而非 1515（实测）。
func TestSwitchStartAfterHeldFall(t *testing.T) {
	cases := []struct {
		name      string
		fallTick  int
		points    map[int]int64
		crossTick int
		filtered  bool
	}{
		{"opens after the fall", 8, map[int]int64{8: 0, 9: 300, 10: 600, 11: 900}, 11, true},
		{"opens on the held ticks", 5, map[int]int64{5: 0, 6: 300, 7: 600, 8: 700, 9: 900}, 9, true},
		{"crosses on the held ticks", 5, map[int]int64{5: 600, 6: 900}, 6, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := captureLog(t)
			shape := switchShape{aLast: 6, noSong: [2]int{7, 11}, bFrom: 12, crossTick: c.crossTick, fallTick: c.fallTick, points: c.points}
			w := shape.world()
			inner := w.core
			w.core = func(k int) (string, int32) {
				if k == 5 || k == 6 { // the fall, under A
					return swRidA, 0
				}
				return inner(k)
			}
			events := runWorld(t, w, 30)
			a := songAt(events, "A")
			b := songAt(events, "B")
			if a < 0 || b < 0 {
				t.Fatalf("A or B not announced: %v", eventTypes(events))
			}
			for _, e := range events[a:b] {
				if e.Type == player.EventPlaybackResume {
					t.Fatalf("A published a resume at %d ms around the fall", eventMs(e))
				}
			}
			ms, _ := announced(t, events, "B")
			if c.filtered && ms > 200 {
				t.Fatalf("B announced at %d ms, want <= 200", ms)
			}
			if !c.filtered {
				if ms != 1515 {
					t.Fatalf("B announced at %d ms, want 1515 (the crossing on A's held ticks not taken)", ms)
				}
				if !strings.Contains(buf.String(), "不晚于上一首最后一个核心样本") {
					t.Fatalf("opening passthrough not logged: %q", buf.String())
				}
			}
		})
	}
}

// Design §7 item 22: the same song back within the tolerance goes on with the
// same reader, corrector included: the seek-lag correction (e = 300 ms) is kept,
// so no playback_resume follows the gap; only the adoption one at the start.
// Reopened, the corrector would start from e = 0, the public position would jump
// 300 ms ahead and back (two resumes).
// 变异自证：无歌分支恢复当拍关闭读取器，间隙之后多出两条 playback_resume（150850、150600，实测）。
func TestSwitchStartSameSongKeepsCorrection(t *testing.T) {
	key := pos.LagKey{Ctx: 0x05a32e40, Ring: 0x06120a00, Base: 0}
	w := &switchWorld{
		last: 2,
		bridge: func(k int) (cdp.Snapshot, error) {
			if k >= 9 && k <= 16 {
				return noSongSnap(), nil
			}
			return songSnap(swRidA, "A"), nil
		},
		core: func(k int) (string, int32) { return swRidA, songA(k) },
		out:  func(k int) pos.OutputState { return pos.OutputState{} },
		lag: func(k int) pos.LagSample {
			return pos.LagSample{Valid: true, Key: key, Written: uint32(k * 4096), EUs: 300000, DUs: lagD}
		},
	}
	events := runWorld(t, w, 30)
	if got := resumesMs(events); len(got) != 1 {
		t.Fatalf("playback_resume at %v ms, want only the adoption one", got)
	}
	if opens, _, _ := w.counts(); opens != 1 {
		t.Fatalf("reader opened %d times, want 1", opens)
	}
	if n := typeCount(events, player.EventSongInfoUpdate); n != 1 {
		t.Fatalf("A announced %d times, want once", n)
	}
}

// pollT is one poll of a corrected source driven directly: a core read that
// succeeds (ok), one that fails, or an output-only observation (obs).
type pollT struct {
	ms   int
	ok   bool
	obs  bool
	core int32
	rid  string
	out  pos.OutputState
	lag  pos.LagSample
}

// runPolls drives a fresh corrected source and returns the samples of the
// successful reads.
func runPolls(t *testing.T, polls []pollT) []pos.Sample {
	t.Helper()
	defer func(f func() time.Time) { trackNow = f }(trackNow)
	core, lag := &stubCore{}, &stubLag{}
	var warned bool
	src := newCorrectedSource(core, outLag{lag}, &warned, 50).(*correctedSource)
	var outs []pos.Sample
	for _, p := range polls {
		at := lagT0.Add(time.Duration(p.ms) * time.Millisecond)
		trackNow = func() time.Time { return at }
		lag.sample, lag.out = p.lag, p.out
		switch {
		case p.obs:
			src.observeOutput()
		case p.ok:
			core.sample, core.err = pos.Sample{RID: p.rid, PositionMs: p.core, CoreMs: p.core, DurationMs: lagDur, ObservedAt: at}, nil
			out, err := src.Read(p.rid, true)
			if err != nil {
				t.Fatalf("t=%d: %v", p.ms, err)
			}
			outs = append(outs, out)
		default:
			core.err = pos.ErrSongChanged
			if _, err := src.Read(p.rid, true); err == nil {
				t.Fatalf("t=%d: failed core read returned no error", p.ms)
			}
		}
	}
	return outs
}

// Design §7 item 14 (counterexample): a failed core read and an output-only
// observation reach neither the #131 corrector nor the filter's state machine.
// With valid seek-lag samples of one segment on them, the corrector would
// confirm early and not report the adoption on the next successful read; it
// reports it there as without them.
// 变异自证：读失败拍与只观测拍把偏差采样交给修正器，第二个成功样本 LagAdopted 为假（实测）。
func TestSwitchStartObservationSkipsCorrector(t *testing.T) {
	seg := func(w uint32) pos.LagSample {
		return pos.LagSample{Valid: true, Key: pos.LagKey{Ctx: 1, Ring: 2, Base: 0}, Written: w, EUs: 300000, DUs: lagD}
	}
	steady := pos.OutputState{Valid: true, Device: pos.DeviceWASAPI, Bps: tbpsK}
	with := runPolls(t, []pollT{
		{ms: 0, ok: true, rid: swRidA, core: 50000, out: steady, lag: seg(4096)},
		{ms: 50, rid: swRidA, out: steady, lag: seg(8192)},
		{ms: 100, obs: true, out: steady, lag: seg(12288)},
		{ms: 150, ok: true, rid: swRidA, core: 50150, out: steady, lag: seg(16384)},
	})
	without := runPolls(t, []pollT{
		{ms: 0, ok: true, rid: swRidA, core: 50000, out: steady, lag: seg(4096)},
		{ms: 150, ok: true, rid: swRidA, core: 50150, out: steady, lag: seg(16384)},
	})
	for i := range without {
		g, w := with[i], without[i]
		if g.PositionMs != w.PositionMs || g.LagShiftMs != w.LagShiftMs || g.LagAdopted != w.LagAdopted || g.LagAppliedMs != w.LagAppliedMs {
			t.Fatalf("sample %d: %+v, want %+v", i, g, w)
		}
	}
	if !with[1].LagAdopted {
		t.Fatal("control: the second successful read must report the adoption")
	}
}

// Design §7 item 13: the output read on a failed core read feeds the tracking.
// B's output opens and crosses while the core still fails the bridge's RID; B's
// first successful read is anchored at that crossing.
// 变异自证：读失败拍不观测输出，B 的首个样本 1515（实测）。
func TestSwitchStartFailedReadObserved(t *testing.T) {
	polls := []pollT{
		{ms: -100, ok: true, rid: swRidA, core: 50000, out: dsOutK(0, 50000)},
		{ms: 0, rid: swRidB, out: dsOutK(0, 50000)},
		{ms: 50, rid: swRidB, out: dsOutK(0, 0)},
		{ms: 100, rid: swRidB, out: dsOutK(0, 300)},
		{ms: 150, rid: swRidB, out: dsOutK(0, 600)},
		{ms: 200, rid: swRidB, out: dsOutK(0, 900)},
		{ms: 250, ok: true, rid: swRidB, core: 1515, out: dsOutK(0, 1600)},
	}
	outs := runPolls(t, polls)
	if got := outs[1].PositionMs; got != 0 {
		t.Fatalf("B's first sample: %d ms, want 0 (anchored at the crossing seen on a failed read)", got)
	}
}

// Design §7 item 15: the corrected source logs an opening passthrough once, with
// its reason; an in-song seek that degrades logs none.
// 变异自证：去掉 correctedSource 的开播降级日志，第一例没有日志（实测）；拖动段降级也记，第二例多一条（实测）。
func TestSwitchStartPassthroughLogged(t *testing.T) {
	t.Run("opening", func(t *testing.T) {
		buf := captureLog(t)
		r := newLagRig(t)
		r.rid = swRidA
		r.dsTick(0, 1, 50000, 0, 50000, noLag)
		r.rid = swRidB
		r.dsTick(50, 1, 1515, 0, 1700, noLag)
		r.dsTick(100, 1, 1515, 0, 1750, noLag)
		if got := logIcons(buf, "开播瞬态未过滤"); len(got) != 1 || got[0] != "[+]" {
			t.Fatalf("opening passthrough log %v, want one [+], in %q", got, buf.String())
		}
	})
	t.Run("seek", func(t *testing.T) {
		buf := captureLog(t)
		r := newLagRig(t)
		r.rid = "MUSIC_3" // a previous song, so A's segments follow a real song change
		r.dsTick(-100, 1, 90000, 80000, 90000, noLag)
		r.rid = swRidA
		r.dsTick(0, 1, 10000, 10000, 10000, noLag) // A's opening at its base: nothing logged
		r.dsTick(50, 1, 30313, 30000, 313, noLag)
		r.dsTick(1050, 1, 30914, 30000, 2000, noLag) // crossing 1000 ms after the last sample: degrade
		if strings.Contains(buf.String(), "开播瞬态未过滤") {
			t.Fatalf("seek degrade logged as an opening: %q", buf.String())
		}
	})
}
