package kuwo

// This file tests only the hold of a step back to the start of the published
// song through runSession (#185, doc/design/kuwo-switch-fallback-design.md §5.2,
// §8): on a song switch PlayerCore's position falls to about 0 while its RID and
// the bridge still report the old song (inferred from recordings, design §3),
// and the session must not publish that as a seek; a real step back to the start
// is published once the bridge confirms the song. The bridge and the core are
// one fake, fallSrc, advanced by call count: the bridge's lag behind the fall is
// counted in snapshots answered after the first fall read. The hold runs on the
// fake's clock (50 ms per core read, snapshotNow swapped for it) and events are
// filed by the tick that emitted them (tickLog), so the assertions count ticks,
// not wall time; the wall clock only bounds a run. The interplay with the
// #131/#135 notices is in switchfallbacknotice_test.go, with the #134 pause
// resend in pauseresend_test.go.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

const (
	fallA = "MUSIC_1"
	fallB = "MUSIC_2"
)

// tagged is an event with the index of the core read of the tick that emitted it.
type tagged struct {
	player.Event
	read int
}

// tickLog files the player's events by tick. A fake reader calls read at each
// core read, which first drains the events emitted so far: runSession emits a
// tick's events after that tick's read and before the next one, so each event is
// tagged with the read of its tick (no-song ticks, which read nothing, count with
// the latest read).
type tickLog struct {
	mu     sync.Mutex
	p      *KuwoPlayer
	reads  int
	events []tagged
}

func (l *tickLog) drainLocked() {
	for {
		select {
		case e := <-l.p.Events():
			l.events = append(l.events, tagged{e, l.reads - 1})
		default:
			return
		}
	}
}

// read files the events so far and returns this read's index.
func (l *tickLog) read() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.drainLocked()
	l.reads++
	return l.reads - 1
}

func (l *tickLog) all() []tagged {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.drainLocked()
	return append([]tagged(nil), l.events...)
}

func plain(ev []tagged) []player.Event {
	out := make([]player.Event, len(ev))
	for i, e := range ev {
		out[i] = e.Event
	}
	return out
}

// fallRead is one core read of song A from the fall on: the raw core position
// and the public one (pub, -1 for the same), or an error.
type fallRead struct {
	core, pub int32
	err       error
}

func rawAt(core int32) fallRead { return fallRead{core: core, pub: -1} }

// ramp is n reads from `from`, step ms apart.
func ramp(from, step int32, n int) []fallRead {
	out := make([]fallRead, n)
	for i := range out {
		out[i] = rawAt(from + step*int32(i))
	}
	return out
}

// fallSrc is the bridge and the core of song A through a fall, then song B. A
// plays from startMs, 50 ms per read; after fallAfter reads (with onSnap, at the
// first read after that which follows a snapshot) the core falls: A's reads take
// fall in turn, the last one repeated. The bridge reports A (with status, 1 when
// 0) until lag snapshots have been answered after the first fall read, then the
// steps of then (the last endless). B's reads go 0, 50, ... Its clock (now)
// advances 50 ms per read and stamps the samples.
type fallSrc struct {
	fallSpec
	log *tickLog
	mu  sync.Mutex

	vt                                   time.Time
	aReads, fallReads, bReads, afterFall int
	snapped, fallen, backSnap, failed    bool
	refreshed                            map[int]bool // read index → a snapshot was answered on its tick
	fallIdx, backIdx, resumeIdx          int          // first fall read; first A read after then reported A; first good read after a failed one
}

// fallSpec is what a test sets of a fallSrc.
type fallSpec struct {
	status    int
	startMs   int32
	fallAfter int
	onSnap    bool
	fall      []fallRead
	lag       int
	then      []snapStep
}

func newFallSrc(p *KuwoPlayer, spec fallSpec) *fallSrc {
	return &fallSrc{fallSpec: spec, log: &tickLog{p: p}, vt: time.Unix(2000, 0), refreshed: map[int]bool{}, fallIdx: -1, backIdx: -1, resumeIdx: -1}
}

func (f *fallSrc) now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.vt }

func (f *fallSrc) Snapshot(context.Context) (cdp.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapped = true
	a := songSnap(fallA, "A")
	if f.status != 0 {
		a.Status = f.status
	}
	if !f.fallen {
		return a, nil
	}
	n := f.afterFall - f.lag
	f.afterFall++
	if n < 0 {
		return a, nil
	}
	step := f.then[len(f.then)-1]
	for _, st := range f.then {
		if n < st.calls {
			step = st
			break
		}
		n -= st.calls
	}
	if step.err == nil && step.snap.RID == fallA {
		f.backSnap = true
	}
	return step.snap, step.err
}

func (f *fallSrc) Read(expected string, _ bool) (pos.Sample, error) {
	idx := f.log.read()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vt = f.vt.Add(50 * time.Millisecond)
	f.refreshed[idx], f.snapped = f.snapped, false
	rid := pos.RIDWithPrefix(expected)
	sample := func(core, pub int32) pos.Sample {
		if pub < 0 {
			pub = core
		}
		return pos.Sample{RID: rid, PositionMs: pub, CoreMs: core, DurationMs: 200000, ObservedAt: f.vt}
	}
	if rid != fallA {
		v := 50 * int32(f.bReads)
		f.bReads++
		return sample(v, v), nil
	}
	if !f.fallen && (f.aReads < f.fallAfter || f.onSnap && !f.refreshed[idx]) {
		v := f.startMs + 50*int32(f.aReads)
		f.aReads++
		return sample(v, v), nil
	}
	if !f.fallen {
		f.fallen, f.fallIdx = true, idx
	}
	if f.backSnap && f.backIdx < 0 {
		f.backIdx = idx
	}
	r := f.fall[min(f.fallReads, len(f.fall)-1)]
	f.fallReads++
	if r.err != nil {
		f.failed = true
		return pos.Sample{}, r.err
	}
	if f.failed && f.resumeIdx < 0 {
		f.resumeIdx = idx
	}
	return sample(r.core, r.pub), nil
}

func (f *fallSrc) Close() {}

func (f *fallSrc) reads() int { f.mu.Lock(); defer f.mu.Unlock(); return f.fallReads }

func (f *fallSrc) marks() (fallIdx, backIdx, resumeIdx int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fallIdx, f.backIdx, f.resumeIdx
}

// allRefreshed reports whether every read from first to last took a snapshot on
// its own tick.
func (f *fallSrc) allRefreshed(first, last int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := first; i <= last; i++ {
		if !f.refreshed[i] {
			return false
		}
	}
	return true
}

// runFall runs a session of p over src, with the hold on src's clock unless
// clock is nil, until done holds on the filed events; then it stops the session
// and returns them. The wall clock only bounds the run (5 s).
func runFall(t *testing.T, p *KuwoPlayer, log *tickLog, src snapshotSource, open func() (positionSource, error), clock func() time.Time, done func([]tagged) bool) []tagged {
	t.Helper()
	if clock != nil {
		defer func(v func() time.Time) { snapshotNow = v }(snapshotNow)
		snapshotNow = clock
	}
	ctx, cancel := context.WithCancel(context.Background())
	end := make(chan error, 1)
	go func() { end <- p.runSession(ctx, src, open) }()
	finish := func() []tagged {
		cancel()
		if err := <-end; !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected exit: %v", err)
		}
		return log.all()
	}
	deadline := time.After(5 * time.Second)
	for !done(log.all()) {
		select {
		case err := <-end:
			t.Fatalf("session ended: %v; events %v", err, eventTypes(plain(log.all())))
		case <-deadline:
			got := finish()
			t.Fatalf("condition not met within 5 s; events %v", eventTypes(plain(got)))
		case <-time.After(10 * time.Millisecond):
		}
	}
	return finish()
}

// runFallSrc runs src as its own reader on its clock.
func runFallSrc(t *testing.T, src *fallSrc, done func([]tagged) bool) []tagged {
	t.Helper()
	return runFall(t, src.log.p, src.log, src, func() (positionSource, error) { return src, nil }, src.now, done)
}

// songSpan is the events from song name's song_info_update up to the next
// clear_song_data (a switch, or the session's end).
func songSpan(ev []tagged, name string) []tagged {
	start := -1
	for i, e := range ev {
		if si, ok := e.Data.(*player.SongInfo); ok && si.Name == name && start < 0 {
			start = i
		}
		if e.Type == player.EventClearSongData && start >= 0 {
			return ev[start:i]
		}
	}
	if start < 0 {
		return nil
	}
	return ev[start:]
}

func songShown(name string) func([]tagged) bool {
	return func(ev []tagged) bool { return songNamed(plain(ev), name) }
}

func resumesOf(ev []tagged) []tagged {
	var out []tagged
	for _, e := range ev {
		if e.Type == player.EventPlaybackResume {
			out = append(out, e)
		}
	}
	return out
}

func resumeReads(ev []tagged) []int {
	var out []int
	for _, e := range resumesOf(ev) {
		out = append(out, e.read)
	}
	return out
}

// noFallPublished fails if song A published a seek, a pause, a status change
// after its announcement, or a lyric_update below fromMs.
func noFallPublished(t *testing.T, ev []tagged, fromMs int32) {
	t.Helper()
	span := songSpan(ev, "A")
	if len(span) == 0 {
		t.Fatalf("A never published: %v", eventTypes(plain(ev)))
	}
	statuses := 0
	for _, e := range span {
		switch e.Type {
		case player.EventPlaybackResume, player.EventPlaybackPause:
			t.Fatalf("A published %s at %d ms: %v", e.Type, eventMs(e.Event), eventsMs(positioned(plain(span))))
		case player.EventLyricUpdate:
			if ms := eventMs(e.Event); ms < fromMs {
				t.Fatalf("A published lyric_update at %d ms: %v", ms, eventsMs(positioned(plain(span))))
			}
		case player.EventStatusUpdate:
			statuses++
		}
	}
	if statuses != 1 {
		t.Fatalf("A published %d status updates, want 1: %v", statuses, eventTypes(plain(span)))
	}
}

func noSongThenB(noSong cdp.Snapshot) []snapStep {
	return []snapStep{{calls: 2, snap: noSong}, {calls: 1 << 30, snap: songSnap(fallB, "B")}}
}

var (
	ridEmpty = cdp.Snapshot{Status: 1}
	status3  = cdp.Snapshot{RID: fallA, Title: "A", Artist: "x", Status: 3}
)

// The fall before a switch is not published, whichever way the bridge and the
// core then report the switch, and with A paused.
// 变异自证：hold 恒返回 false（不持有），全部子用例出现位置 0 的 playback_resume 而变红（实测）。
func TestSwitchFallNotPublished(t *testing.T) {
	cases := []struct {
		name   string
		status int
		fall   []fallRead
		lag    int
		then   []snapStep
	}{
		{"no song rid empty, lag 1", 1, []fallRead{rawAt(0)}, 1, noSongThenB(ridEmpty)},
		{"no song rid empty, lag 3", 1, []fallRead{rawAt(0)}, 3, noSongThenB(ridEmpty)},
		{"no song status 3, lag 1", 1, []fallRead{rawAt(0)}, 1, noSongThenB(status3)},
		{"no song status 3, lag 3", 1, []fallRead{rawAt(0)}, 3, noSongThenB(status3)},
		{"settling then B", 1, []fallRead{rawAt(0)}, 2, []snapStep{{calls: 4, err: cdp.ErrSongChanged}, {calls: 1 << 30, snap: songSnap(fallB, "B")}}},
		{"B directly", 1, []fallRead{rawAt(0)}, 2, []snapStep{{calls: 1 << 30, snap: songSnap(fallB, "B")}}},
		{"core changes song first", 1, []fallRead{rawAt(0), {err: pos.ErrSongChanged}}, 3, []snapStep{{calls: 1 << 30, snap: songSnap(fallB, "B")}}},
		{"paused", 2, []fallRead{rawAt(0)}, 2, noSongThenB(ridEmpty)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := newFallSrc(New(0, 50), fallSpec{status: c.status, startMs: 150000, fallAfter: 8, fall: c.fall, lag: c.lag, then: c.then})
			ev := runFallSrc(t, src, songShown("B"))
			noFallPublished(t, ev, 150000)
			if n := statusCount(plain(ev), "waiting_song"); n != 0 {
				t.Fatalf("waiting_song sent %d times during a switch", n)
			}
		})
	}
}

// The hold judges the raw core CoreMs: the public position of a fall may be the
// #135 filter's held value (74868 in ws/25-kuwo-185), far above the start.
// 变异自证：判据改看 sample.PositionMs，74868 不低于 startBand 而不持有，发出 74868 的 playback_resume，变红（实测）。
func TestSwitchFallJudgedOnRawCore(t *testing.T) {
	src := newFallSrc(New(0, 50), fallSpec{startMs: 150000, fallAfter: 8, fall: []fallRead{{core: 0, pub: 74868}}, lag: 2, then: noSongThenB(ridEmpty)})
	noFallPublished(t, runFallSrc(t, src, songShown("B")), 150000)
}

// Once a hold begins every sample of the song is held, whatever its value: the
// fall may be followed by the open ladder (313, 1515), the last above startBand.
// 变异自证：每拍重判判据（不在判据内即放行），1515 被放行、发出 1515 的 playback_resume，变红（实测）。
func TestSwitchFallHoldIsSticky(t *testing.T) {
	src := newFallSrc(New(0, 50), fallSpec{startMs: 150000, fallAfter: 8, fall: []fallRead{rawAt(0), rawAt(313), rawAt(1515)}, lag: 3, then: noSongThenB(ridEmpty)})
	noFallPublished(t, runFallSrc(t, src, songShown("B")), 150000)
}

// Review round 1, item 1: a switch within the first second of A. The ladder
// after the fall (1515) is above the position held from (500); a hold released
// when the core is back above that position would publish 1515 as a forward
// seek. With the filter off, and with the real #135 filter degraded to
// passthrough (fed already past the Play threshold on its first sample).
// 变异自证：加回「核心回到持有前位置之上即放行」，两个子用例都发出 1515 的 playback_resume 与歌词而变红（实测）。
func TestSwitchFallNearSongStart(t *testing.T) {
	for _, c := range []struct {
		name     string
		filtered bool
	}{{"filter off", false}, {"filter degraded", true}} {
		t.Run(c.name, func(t *testing.T) {
			src := newFallSrc(New(0, 50), fallSpec{startMs: 500, fallAfter: 1, fall: []fallRead{rawAt(0), rawAt(1515)}, lag: 3, then: noSongThenB(ridEmpty)})
			open := func() (positionSource, error) { return src, nil }
			if c.filtered {
				warned := false
				open = func() (positionSource, error) {
					return newCorrectedSource(src, &stubLag{out: dsOutK(0, 2000)}, &warned, 50), nil
				}
			}
			noFallPublished(t, runFall(t, src.log.p, src.log, src, open, src.now, songShown("B")), 500)
		})
	}
}

// A real step back to the start (a drag to 0:00, a loop back to the start): the
// bridge keeps reporting the song, so the step is published once. With
// switchConfirmAfter 100 ms on the fake's 50 ms per read, the release is the
// third read after the fall (its tick's snapshot is answered at the clock of the
// second, 100 ms after the fall), at that read's position; every held tick takes
// a snapshot of its own.
// 变异自证（实测，两个子用例均变红）：确认改为丢弃，没有 playback_resume；确认不等
// switchConfirmAfter（只要快照不早于持有开始），playback_resume 落在第 1 拍；去掉持有期间的
// nextMeta 清零，持有拍不再各取快照，playback_resume 落在第 4 拍。
func TestSwitchFallRealStepBackConfirmed(t *testing.T) {
	defer func(v time.Duration) { switchConfirmAfter = v }(switchConfirmAfter)
	switchConfirmAfter = 100 * time.Millisecond
	for _, c := range []struct {
		name    string
		startMs int32
	}{{"drag to 0:00", 150000}, {"loop back to the start", 199000}} {
		t.Run(c.name, func(t *testing.T) {
			src := newFallSrc(New(0, 50), fallSpec{startMs: c.startMs, fallAfter: 8, onSnap: true, fall: ramp(0, 50, 1000), lag: 1 << 30})
			ev := runFallSrc(t, src, func([]tagged) bool { return src.reads() >= 12 })
			fall, _, _ := src.marks()
			resumes := resumesOf(ev)
			if len(resumes) != 1 || resumes[0].read != fall+3 || eventMs(resumes[0].Event) != 150 {
				t.Fatalf("playback_resume at reads %v (positions %v), want one at read %d (fall + 3), 150 ms", resumeReads(ev), eventsMs(plain(resumes)), fall+3)
			}
			if !src.allRefreshed(fall+1, fall+3) {
				t.Fatal("a held tick did not take a snapshot of its own")
			}
		})
	}
}

// Counterexample: a step back to the middle of the song is published on the
// tick it is read, as before #185.
// 变异自证：startBand 改为无穷大（所有向后跳变都持有，即方案 A），playback_resume 推迟到确认那一拍，变红（实测）。
func TestSwitchFallMidSongStepBackAtOnce(t *testing.T) {
	src := newFallSrc(New(0, 50), fallSpec{startMs: 150000, fallAfter: 8, fall: ramp(30000, 50, 1000), lag: 1 << 30})
	ev := runFallSrc(t, src, func([]tagged) bool { return src.reads() >= 10 })
	fall, _, _ := src.marks()
	if r := resumesOf(ev); len(r) != 1 || r[0].read != fall || eventMs(r[0].Event) != 30000 {
		t.Fatalf("playback_resume at reads %v (positions %v), want one at read %d (the fall), 30000 ms", resumeReads(ev), eventsMs(plain(r)), fall)
	}
}

// Counterexample: a forward step within the first second is published on the
// tick it is read; only a step back starts a hold.
// 变异自证：判据不要求后退，开头 1 s 内的样本都被持有，playback_resume 推迟到确认那一拍，变红（实测）。
func TestSwitchFallForwardStepAtOnce(t *testing.T) {
	src := newFallSrc(New(0, 50), fallSpec{startMs: 200, fallAfter: 4, fall: ramp(900, 50, 1000), lag: 1 << 30})
	ev := runFallSrc(t, src, func([]tagged) bool { return src.reads() >= 10 })
	fall, _, _ := src.marks()
	if r := resumesOf(ev); len(r) != 1 || r[0].read != fall || eventMs(r[0].Event) != 900 {
		t.Fatalf("playback_resume at reads %v (positions %v), want one at read %d (the step), 900 ms", resumeReads(ev), eventsMs(plain(r)), fall)
	}
}

// A single fallen read, then the song goes on where it was: held until
// confirmed, then observed with no seek.
// 变异自证：放行那一拍一律当作跳变（置待通知），多出一条 playback_resume，变红（实测）。
func TestSwitchFallOneReadThenGoesOn(t *testing.T) {
	fall := append([]fallRead{rawAt(0)}, ramp(150450, 50, 1000)...)
	src := newFallSrc(New(0, 50), fallSpec{startMs: 150000, fallAfter: 8, fall: fall, lag: 1 << 30})
	noFallPublished(t, runFallSrc(t, src, func([]tagged) bool { return src.reads() >= 12 }), 150000)
}

// #182: a fall followed by no song for longer than the tolerance clears A once,
// with no seek of A.
// 变异自证：hold 恒返回 false，发出位置 0 的 playback_resume，变红（实测）。
func TestSwitchFallThenNoSongBeyondTolerance(t *testing.T) {
	defer func(v time.Duration) { noSongTolerance = v }(noSongTolerance)
	noSongTolerance = 300 * time.Millisecond
	src := newFallSrc(New(0, 50), fallSpec{startMs: 150000, fallAfter: 8, fall: []fallRead{rawAt(0)}, lag: 2, then: []snapStep{{calls: 1 << 30, snap: ridEmpty}}})
	ev := runFallSrc(t, src, func(ev []tagged) bool { return statusCount(plain(ev), "waiting_song") > 0 })
	if n := statusCount(plain(ev), "waiting_song"); n != 1 {
		t.Fatalf("waiting_song sent %d times, want 1", n)
	}
	if r := resumesOf(ev); len(r) != 0 {
		t.Fatalf("A published playback_resume at %v", eventsMs(plain(r)))
	}
}

// A hold ends when the bridge reports no song (#182) or the core read fails; if
// the same song then goes on with the core still at the start, it is held anew,
// from its first read after the gap: released on the seventh read after it
// (switchConfirmAfter 300 ms at 50 ms per read).
// 变异自证（实测）：无歌分支不 dropHold，no_song 子用例沿用旧的持有起点、playback_resume 早于第 7 拍
// 而变红；读失败分支不 dropHold，read_failed 子用例的 playback_resume 落在恢复读取的那一拍而变红。
func TestSwitchFallHoldRestartsAfterGap(t *testing.T) {
	for _, c := range []struct {
		name string
		fall []fallRead
		then []snapStep
		lag  int
	}{
		{"no song", ramp(0, 50, 1000), []snapStep{{calls: 2, snap: ridEmpty}, {calls: 1 << 30, snap: songSnap(fallA, "A")}}, 1},
		{"read failed", append(append([]fallRead{rawAt(0)}, repeatErr(8)...), ramp(400, 50, 1000)...), nil, 1 << 30},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := newFallSrc(New(0, 50), fallSpec{startMs: 150000, fallAfter: 8, fall: c.fall, lag: c.lag, then: c.then})
			ev := runFallSrc(t, src, func([]tagged) bool { return src.reads() >= 30 })
			_, back, resume := src.marks()
			from := back
			if c.then == nil {
				from = resume
			}
			if from < 0 {
				t.Fatal("the song did not go on after the gap")
			}
			if r := resumeReads(ev); len(r) != 1 || r[0] != from+7 {
				t.Fatalf("playback_resume at reads %v, want one at read %d (seventh after the song went on)", r, from+7)
			}
		})
	}
}

func repeatErr(n int) []fallRead {
	out := make([]fallRead, n)
	for i := range out {
		out[i] = fallRead{err: pos.ErrSongChanged}
	}
	return out
}

// readerFunc is a positionSource answering by a function.
type readerFunc func(expected string) (pos.Sample, error)

func (f readerFunc) Read(expected string, _ bool) (pos.Sample, error) { return f(expected) }
func (f readerFunc) Close()                                           {}

// Counterexample: a song change is not a step back. B's first sample (0, after A
// at 150000) is announced on the tick it is read.
// 变异自证：持有判据不核对 RID（另一首歌的样本也与上一首的 CoreMs 比较），B 的宣告推迟到确认之后，变红（实测）。
func TestSwitchFallSongChangeNotHeld(t *testing.T) {
	p := New(0, 50)
	log := &tickLog{p: p}
	var mu sync.Mutex
	firstB := -1
	reader := readerFunc(func(expected string) (pos.Sample, error) {
		idx := log.read()
		mu.Lock()
		defer mu.Unlock()
		rid := pos.RIDWithPrefix(expected)
		v := int32(150000)
		if rid == fallB {
			if firstB < 0 {
				firstB = idx
			}
			v = 0
		}
		return pos.Sample{RID: rid, PositionMs: v, CoreMs: v, DurationMs: 200000, ObservedAt: time.Now()}, nil
	})
	src := &phasedSnapshots{steps: []snapStep{{calls: 3, snap: songSnap(fallA, "A")}, {calls: 1 << 30, snap: songSnap(fallB, "B")}}}
	ev := runFall(t, p, log, src, func() (positionSource, error) { return reader, nil }, nil, songShown("B"))
	mu.Lock()
	defer mu.Unlock()
	if span := songSpan(ev, "B"); span[0].read != firstB {
		t.Fatalf("B announced at read %d, want its first read %d", span[0].read, firstB)
	}
}

// A bridge that keeps settling while a step back is held fails the session
// closed after transientTolerance, as before, with no seek of A.
// 变异自证：hold 恒返回 false，失败关闭之前先发出位置 0 的 playback_resume，变红（实测）。
func TestSwitchFallSettlingFailsClosed(t *testing.T) {
	defer func(v time.Duration) { transientTolerance = v }(transientTolerance)
	transientTolerance = 300 * time.Millisecond
	defer func(v func() time.Time) { snapshotNow = v }(snapshotNow)
	src := newFallSrc(New(0, 50), fallSpec{startMs: 150000, fallAfter: 8, fall: []fallRead{rawAt(0)}, lag: 2, then: []snapStep{{calls: 1 << 30, err: cdp.ErrSongChanged}}})
	snapshotNow = src.now
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := src.log.p.runSession(ctx, src, func() (positionSource, error) { return src, nil }); !errors.Is(err, cdp.ErrSongChanged) {
		t.Fatalf("want the settling error, got %v", err)
	}
	if r := resumesOf(src.log.all()); len(r) != 0 {
		t.Fatalf("A published playback_resume at %v", eventsMs(plain(r)))
	}
}
