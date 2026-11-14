package kuwo

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
)

// On a switch the bridge reports no song (RID empty or Status 3) for a moment
// before the next song: 0.57 s and 1.10 s from waiting_song to the next song in
// the 2026-11-13 recording (#182). These tests drive runSession with that shape
// and a shrunken noSongTolerance, with the call-count fakes of transient_test.go:
// a step answered by a successful snapshot lasts one metadata period (200 ms plus
// up to one poll) per call, an ErrSongChanged step one poll per call.

// noSongKinds are the two shapes of no song, each seen by one of the two tests
// in runSession only.
var noSongKinds = []struct {
	name string
	snap cdp.Snapshot
}{
	{"rid_empty", cdp.Snapshot{Status: 1}},                                         // Status still 1
	{"status_3", cdp.Snapshot{RID: "MUSIC_1", Title: "A", Artist: "x", Status: 3}}, // A's RID kept: playback ended
}

func (s *phasedSnapshots) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// noSongClock records when the first no-song snapshot was answered, and the
// first one answered after an error.
type noSongClock struct {
	phasedSnapshots
	first, resumed time.Time // guarded by phasedSnapshots.mu
}

func (s *noSongClock) Snapshot(ctx context.Context) (cdp.Snapshot, error) {
	snap, err := s.phasedSnapshots.Snapshot(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil && (snap.RID == "" || snap.Status == 3) {
		if s.first.IsZero() {
			s.first = time.Now()
		}
		if s.errCalls > 0 && s.resumed.IsZero() {
			s.resumed = time.Now()
		}
	}
	return snap, err
}

// closingReader is a switchReader that counts Close calls.
type closingReader struct {
	switchReader
	closes int // guarded by switchReader.mu
}

func (r *closingReader) Close() { r.mu.Lock(); r.closes++; r.mu.Unlock() }

func (r *closingReader) closed() int { r.mu.Lock(); defer r.mu.Unlock(); return r.closes }

func statusCount(events []player.Event, status string) int {
	n := 0
	for _, e := range events {
		if si, ok := e.Data.(*player.StatusInfo); ok && si.Status == status {
			n++
		}
	}
	return n
}

func typeCount(events []player.Event, typ string) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// songAt returns the index of the first song_info_update of song name, or -1.
func songAt(events []player.Event, name string) int {
	for i, e := range events {
		if si, ok := e.Data.(*player.SongInfo); ok && si.Name == name {
			return i
		}
	}
	return -1
}

func eventTypes(events []player.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		t := e.Type
		if si, ok := e.Data.(*player.StatusInfo); ok {
			t += ":" + si.Status
		}
		out = append(out, t)
	}
	return out
}

// startSession runs a session in the background with events collected. done
// reports the session's end (for waitEvents); stop cancels it and returns its
// error, and also runs at cleanup.
func startSession(t *testing.T, src snapshotSource, open func() (positionSource, error)) (events func() []player.Event, done <-chan error, stop func() error) {
	t.Helper()
	p := New(0, 50)
	ctx, cancel := context.WithCancel(context.Background())
	quit := make(chan struct{})
	events = collect(p, quit)
	notify := make(chan error, 1)
	finished := make(chan struct{})
	var result error
	go func() {
		result = p.runSession(ctx, src, open)
		notify <- result
		close(finished)
	}()
	var once sync.Once
	stop = func() error {
		cancel()
		<-finished
		once.Do(func() { close(quit) })
		return result
	}
	t.Cleanup(func() { stop() })
	return events, notify, stop
}

// A no-song moment shorter than the tolerance, then the next song: A is not
// cleared on its own (no waiting_song, no clear_song_data but the one the switch
// to B sends right before B), and B is announced as on a direct switch. The
// reader is still closed for the gap and reopened for B.
// 变异自证：把 runSession 无歌分支改回当拍 s.clear()，两个子用例都变红（实测）。
func TestNoSongShortGapThenNextSong(t *testing.T) {
	defer func(v time.Duration) { noSongTolerance = v }(noSongTolerance)
	noSongTolerance = time.Second
	for _, k := range noSongKinds {
		t.Run(k.name, func(t *testing.T) {
			src := &phasedSnapshots{steps: []snapStep{
				{calls: 1, snap: songSnap("MUSIC_1", "A")},
				{calls: 2, snap: k.snap}, // about 400-500 ms < 1 s
				{calls: 1 << 30, snap: songSnap("MUSIC_2", "B")},
			}}
			reader := &closingReader{}
			events := runUntilSong(t, src, reader, "B")
			if n := statusCount(events, "waiting_song"); n != 0 {
				t.Fatalf("waiting_song sent %d times during a switch: %v", n, eventTypes(events))
			}
			a, b := songAt(events, "A"), songAt(events, "B")
			if a < 0 || typeCount(events[a:b], player.EventClearSongData) != 1 || events[b-1].Type != player.EventClearSongData {
				t.Fatalf("A was cleared on its own before B: %v", eventTypes(events))
			}
			// Once for the gap, once when the session ended.
			if c := reader.closed(); c != 2 {
				t.Fatalf("reader closed %d times, want 2", c)
			}
		})
	}
}

// No song for longer than the tolerance: one clear_song_data and one
// waiting_song, clear first, not before the tolerance has passed and not
// repeated after. status_3 is playback ending with no next song.
// 变异自证：改回当拍 s.clear() 则两个子用例的下界变红；把清空条件改成恒假则等不到 waiting_song 而红（实测）。
func TestNoSongBeyondToleranceClearsOnce(t *testing.T) {
	defer func(v time.Duration) { noSongTolerance = v }(noSongTolerance)
	noSongTolerance = 300 * time.Millisecond
	for _, k := range noSongKinds {
		t.Run(k.name, func(t *testing.T) {
			src := &noSongClock{phasedSnapshots: phasedSnapshots{steps: []snapStep{
				{calls: 1, snap: songSnap("MUSIC_1", "A")},
				{calls: 1 << 30, snap: k.snap},
			}}}
			events, done, stop := startSession(t, src, func() (positionSource, error) { return &switchReader{}, nil })
			waitEvents(t, done, events, func(ev []player.Event) bool { return statusCount(ev, "waiting_song") > 0 })
			seen := time.Now()
			time.Sleep(3 * noSongTolerance) // the clear must not repeat
			got := events()
			if err := stop(); !errors.Is(err, context.Canceled) {
				t.Fatalf("unexpected exit: %v", err)
			}
			src.mu.Lock()
			first := src.first
			src.mu.Unlock()
			held := seen.Sub(first)
			if held < noSongTolerance-30*time.Millisecond {
				t.Fatalf("cleared %v after the song left, before the %v tolerance", held, noSongTolerance)
			}
			if held > noSongTolerance+800*time.Millisecond {
				t.Fatalf("cleared %v after the song left, long after the %v tolerance", held, noSongTolerance)
			}
			a := songAt(got, "A")
			if a < 0 {
				t.Fatalf("A never published: %v", eventTypes(got))
			}
			after := got[a:]
			if statusCount(after, "waiting_song") != 1 || typeCount(after, player.EventClearSongData) != 1 {
				t.Fatalf("want one clear_song_data and one waiting_song: %v", eventTypes(after))
			}
			for _, e := range after {
				if si, ok := e.Data.(*player.StatusInfo); ok && si.Status == "waiting_song" {
					t.Fatalf("waiting_song before clear_song_data: %v", eventTypes(after))
				}
				if e.Type == player.EventClearSongData {
					break
				}
			}
		})
	}
}

// Counterexample: a session that has published no song reports waiting_song at
// once, without the tolerance, and once.
// 变异自证：去掉清空条件里的 s.rid == "" 则两个子用例都变红（实测）。
func TestNoSongAtSessionStartReportsAtOnce(t *testing.T) {
	defer func(v time.Duration) { noSongTolerance = v }(noSongTolerance)
	noSongTolerance = 2 * time.Second
	for _, k := range noSongKinds {
		t.Run(k.name, func(t *testing.T) {
			src := &phasedSnapshots{steps: []snapStep{{calls: 1 << 30, snap: k.snap}}}
			start := time.Now()
			events, done, stop := startSession(t, src, func() (positionSource, error) { return &switchReader{}, nil })
			waitEvents(t, done, events, func(ev []player.Event) bool { return statusCount(ev, "waiting_song") > 0 })
			if since := time.Since(start); since >= noSongTolerance/2 {
				t.Fatalf("waiting_song after %v: a session with no song must not wait for the tolerance", since)
			}
			time.Sleep(300 * time.Millisecond)
			got := events()
			if err := stop(); !errors.Is(err, context.Canceled) {
				t.Fatalf("unexpected exit: %v", err)
			}
			if statusCount(got, "waiting_song") != 1 || typeCount(got, player.EventClearSongData) != 1 || typeCount(got, player.EventSongInfoUpdate) != 0 {
				t.Fatalf("want one clear_song_data and one waiting_song only: %v", eventTypes(got))
			}
		})
	}
}

// Counterexample: the same song reported again within the tolerance goes on as
// it was: read again, not announced again, no waiting_song, no clear.
// 变异自证：改回当拍 s.clear() 则 A 被清空后重新宣告，两个子用例都变红（实测）。
func TestNoSongSameSongGoesOn(t *testing.T) {
	defer func(v time.Duration) { noSongTolerance = v }(noSongTolerance)
	noSongTolerance = time.Second
	for _, k := range noSongKinds {
		t.Run(k.name, func(t *testing.T) {
			src := &phasedSnapshots{steps: []snapStep{
				{calls: 2, snap: songSnap("MUSIC_1", "A")},
				{calls: 2, snap: k.snap}, // about 400-500 ms < 1 s
				{calls: 1 << 30, snap: songSnap("MUSIC_1", "A")},
			}}
			opens := 0
			events, done, stop := startSession(t, src, func() (positionSource, error) { opens++; return &switchReader{}, nil })
			// Three answers after the gap: at least two ticks of A again.
			waitEvents(t, done, events, func([]player.Event) bool { return src.callCount() >= 7 })
			got := events()
			if err := stop(); !errors.Is(err, context.Canceled) {
				t.Fatalf("unexpected exit: %v", err)
			}
			if opens != 2 {
				t.Fatalf("reader opened %d times, want 2 (A read again after the gap)", opens)
			}
			if statusCount(got, "waiting_song") != 0 || typeCount(got, player.EventClearSongData) != 1 ||
				typeCount(got, player.EventSongInfoUpdate) != 1 || typeCount(got, player.EventAllLyrics) != 1 {
				t.Fatalf("A must go on without being cleared or announced again: %v", eventTypes(got))
			}
		})
	}
}

// The no-song clock and the settling clock (ErrSongChanged) are separate budgets:
// each phase is shorter than its own tolerance, the two together (about 800-900
// ms) are longer than either, in both orders.
// 变异自证：在快照出错分支也起算 noSongAt（把空状态算进无歌时长），settle_first 的两个子用例变红（实测）。
func TestNoSongAndSettlingUseSeparateBudgets(t *testing.T) {
	defer func(a, b time.Duration) { transientTolerance, noSongTolerance = a, b }(transientTolerance, noSongTolerance)
	transientTolerance = 600 * time.Millisecond
	noSongTolerance = 700 * time.Millisecond
	settle := snapStep{calls: 8, err: cdp.ErrSongChanged} // about 400 ms < 600 ms
	for _, k := range noSongKinds {
		noSong := snapStep{calls: 2, snap: k.snap} // about 400-500 ms < 700 ms
		for _, order := range []struct {
			name   string
			phases []snapStep
		}{
			{"settle_first", []snapStep{settle, noSong}},
			{"nosong_first", []snapStep{noSong, settle}},
		} {
			t.Run(k.name+"/"+order.name, func(t *testing.T) {
				steps := append([]snapStep{{calls: 1, snap: songSnap("MUSIC_1", "A")}}, order.phases...)
				src := &phasedSnapshots{steps: append(steps, snapStep{calls: 1 << 30, snap: songSnap("MUSIC_2", "B")})}
				events := runUntilSong(t, src, &switchReader{}, "B")
				if _, n := src.errStats(); n != settle.calls {
					t.Fatalf("settling phase was not exercised: %d error calls", n)
				}
				if statusCount(events, "waiting_song") != 0 || waitingProcessSeen(events) {
					t.Fatalf("switch reported as no song or disconnect: %v", eventTypes(events))
				}
			})
		}
	}
}

// No song, a settling window (ErrSongChanged), no song again, then B: about
// 600-750 ms of no song in all, under the 1 s tolerance, while the wall time
// since the first no-song answer is past 1 s as soon as no song is reported
// again (the #182 review's counterexample, scaled down). The no-song clock is
// paused through the settling window, so A is not cleared.
// 变异自证：撤回暂停（删去成功快照处对 noSongAt 的顺延），两个子用例都变红（实测）。
func TestNoSongClockPausedWhileSettling(t *testing.T) {
	defer func(a, b time.Duration) { transientTolerance, noSongTolerance = a, b }(transientTolerance, noSongTolerance)
	transientTolerance = time.Second
	noSongTolerance = time.Second
	for _, k := range noSongKinds {
		t.Run(k.name, func(t *testing.T) {
			src := &phasedSnapshots{steps: []snapStep{
				{calls: 1, snap: songSnap("MUSIC_1", "A")},
				{calls: 2, snap: k.snap},             // about 400-500 ms
				{calls: 14, err: cdp.ErrSongChanged}, // about 700 ms < 1 s
				{calls: 1, snap: k.snap},             // about 200-250 ms
				{calls: 1 << 30, snap: songSnap("MUSIC_2", "B")},
			}}
			events := runUntilSong(t, src, &switchReader{}, "B")
			if _, n := src.errStats(); n != 14 {
				t.Fatalf("settling phase was not exercised: %d error calls", n)
			}
			if statusCount(events, "waiting_song") != 0 || waitingProcessSeen(events) {
				t.Fatalf("switch reported as no song or disconnect: %v", eventTypes(events))
			}
			a, b := songAt(events, "A"), songAt(events, "B")
			if a < 0 || typeCount(events[a:b], player.EventClearSongData) != 1 || events[b-1].Type != player.EventClearSongData {
				t.Fatalf("A was cleared on its own before B: %v", eventTypes(events))
			}
		})
	}
}

// Counterexample: no song, a settling window, then no song for longer than the
// rest of the tolerance: cleared once, when the no-song time reaches the
// tolerance, that is the tolerance plus the settling window after the first
// no-song answer.
// 变异自证：撤回暂停则清空早于下界，改成出错即重置无歌计时（连续无歌）则清空晚于上界，两个子用例都变红（实测）。
func TestNoSongAcrossSettlingStillClears(t *testing.T) {
	defer func(a, b time.Duration) { transientTolerance, noSongTolerance = a, b }(transientTolerance, noSongTolerance)
	transientTolerance = time.Second
	noSongTolerance = time.Second
	for _, k := range noSongKinds {
		t.Run(k.name, func(t *testing.T) {
			src := &noSongClock{phasedSnapshots: phasedSnapshots{steps: []snapStep{
				{calls: 1, snap: songSnap("MUSIC_1", "A")},
				{calls: 3, snap: k.snap},            // about 600-750 ms
				{calls: 6, err: cdp.ErrSongChanged}, // about 300 ms
				{calls: 1 << 30, snap: k.snap},
			}}}
			events, done, stop := startSession(t, src, func() (positionSource, error) { return &switchReader{}, nil })
			waitEvents(t, done, events, func(ev []player.Event) bool { return statusCount(ev, "waiting_song") > 0 })
			seen := time.Now()
			time.Sleep(500 * time.Millisecond) // the clear must not repeat
			got := events()
			if err := stop(); !errors.Is(err, context.Canceled) {
				t.Fatalf("unexpected exit: %v", err)
			}
			src.mu.Lock()
			first, resumed, firstErr := src.first, src.resumed, src.firstErrAt
			src.mu.Unlock()
			if resumed.IsZero() {
				t.Fatal("no song was not reported again after the settling window")
			}
			want := noSongTolerance + resumed.Sub(firstErr)
			held := seen.Sub(first)
			if held < want-30*time.Millisecond || held > want+400*time.Millisecond {
				t.Fatalf("cleared %v after the song left, want about %v (tolerance plus the settling window)", held, want)
			}
			a := songAt(got, "A")
			if a < 0 || statusCount(got[a:], "waiting_song") != 1 || typeCount(got[a:], player.EventClearSongData) != 1 {
				t.Fatalf("want one clear_song_data and one waiting_song: %v", eventTypes(got))
			}
		})
	}
}

// A disconnect within the tolerance takes the disconnect path as before:
// clear_song_data then waiting_process, with no waiting_song.
// 变异自证：改回当拍 s.clear() 则多出 waiting_song，两个子用例都变红（实测）。
func TestNoSongDisconnectTakesDisconnectPath(t *testing.T) {
	defer func(v time.Duration) { noSongTolerance = v }(noSongTolerance)
	noSongTolerance = 2 * time.Second
	for _, k := range noSongKinds {
		t.Run(k.name, func(t *testing.T) {
			src := &phasedSnapshots{steps: []snapStep{
				{calls: 1, snap: songSnap("MUSIC_1", "A")},
				{calls: 1, snap: k.snap},
				{calls: 1 << 30, err: cdp.ErrUnavailable},
			}}
			p := New(0, 50)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := p.runSession(ctx, src, func() (positionSource, error) { return &switchReader{}, nil }); !errors.Is(err, cdp.ErrUnavailable) {
				t.Fatalf("want the disconnect error, got %v", err)
			}
			got := drainEvents(p)
			a := songAt(got, "A")
			if a < 0 || len(got) < 2 {
				t.Fatalf("A never published: %v", eventTypes(got))
			}
			after := got[a:]
			last, ok := after[len(after)-1].Data.(*player.StatusInfo)
			if !ok || statusCount(after, "waiting_song") != 0 || typeCount(after, player.EventClearSongData) != 1 ||
				after[len(after)-2].Type != player.EventClearSongData || last.Status != "waiting_process" || last.Reason != player.ReasonCDPDisconnected {
				t.Fatalf("want the disconnect path only: %v", eventTypes(after))
			}
		})
	}
}
