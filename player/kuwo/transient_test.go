package kuwo

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

// On every song switch the bridge answers the status call with an empty string
// for 0.2-0.5s (captured live), which the parser reports as cdp.ErrSongChanged.
// These tests drive runSession with that shape and a shrunken tolerance.
//
// The fakes advance by call count, not by wall clock: a scheduling delay can
// stretch a phase (and fail a test loudly) but can never skip one and let a test
// pass without having exercised the transient.

type snapStep struct {
	calls int // number of Snapshot calls this step answers; the last step is endless
	snap  cdp.Snapshot
	err   error
}

type phasedSnapshots struct {
	mu         sync.Mutex
	steps      []snapStep
	calls      int
	firstErrAt time.Time // time of the first call answered with an error
	errCalls   int       // calls answered with an error
}

func (s *phasedSnapshots) Snapshot(context.Context) (cdp.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.calls
	s.calls++
	step := s.steps[len(s.steps)-1]
	for _, st := range s.steps {
		if n < st.calls {
			step = st
			break
		}
		n -= st.calls
	}
	if step.err != nil {
		if s.errCalls == 0 {
			s.firstErrAt = time.Now()
		}
		s.errCalls++
	}
	return step.snap, step.err
}

func (s *phasedSnapshots) errStats() (time.Time, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstErrAt, s.errCalls
}

// switchReader answers any expected RID, except that the first badReads reads of
// badRID report a song change (the core lagging the bridge).
type switchReader struct {
	mu       sync.Mutex
	badRID   string
	badReads int
	pos      int32
}

func (r *switchReader) Read(expected string) (pos.Sample, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pos.RIDWithPrefix(expected) == r.badRID && r.badReads > 0 {
		r.badReads--
		return pos.Sample{}, pos.ErrSongChanged
	}
	r.pos += 50
	return pos.Sample{RID: pos.RIDWithPrefix(expected), PositionMs: r.pos, DurationMs: 200000, ObservedAt: time.Now()}, nil
}

func (r *switchReader) Close() {}

// collect drains the player's events into a slice until stop is closed.
func collect(p *KuwoPlayer, stop <-chan struct{}) func() []player.Event {
	var mu sync.Mutex
	var got []player.Event
	go func() {
		for {
			select {
			case e := <-p.Events():
				mu.Lock()
				got = append(got, e)
				mu.Unlock()
			case <-stop:
				return
			}
		}
	}()
	return func() []player.Event { mu.Lock(); defer mu.Unlock(); return append([]player.Event(nil), got...) }
}

func songNamed(events []player.Event, name string) bool {
	for _, e := range events {
		if si, ok := e.Data.(*player.SongInfo); ok && si.Name == name {
			return true
		}
	}
	return false
}

func waitingProcessSeen(events []player.Event) bool {
	for _, e := range events {
		if si, ok := e.Data.(*player.StatusInfo); ok && si.Status == "waiting_process" {
			return true
		}
	}
	return false
}

func songSnap(rid, title string) cdp.Snapshot {
	return cdp.Snapshot{RID: rid, Title: title, Artist: "x", Status: 1}
}

// runUntilSong runs a session until song `name` is published, failing if the
// session ends first, then cancels it and returns the events seen.
func runUntilSong(t *testing.T, src snapshotSource, reader positionSource, name string) []player.Event {
	t.Helper()
	p := New(0, 50)
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	defer close(stop)
	events := collect(p, stop)
	done := make(chan error, 1)
	go func() {
		done <- p.runSession(ctx, src, func() (positionSource, error) { return reader, nil })
	}()
	deadline := time.After(5 * time.Second)
	for !songNamed(events(), name) {
		select {
		case err := <-done:
			cancel()
			t.Fatalf("session ended before %q was published: %v", name, err)
		case <-deadline:
			cancel()
			t.Fatalf("song %q never appeared", name)
		case <-time.After(10 * time.Millisecond):
		}
	}
	got := events()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected exit: %v", err)
	}
	return got
}

// A switch is song A, a settling window (about 200ms here), then song B: the
// session must ride it out and show B without ever reporting a disconnect.
// 变异自证：把 runSession 里的 cdp.ErrSongChanged 容忍分支去掉，本用例变红（实测）。
func TestSwitchSettlingWindowKeepsSession(t *testing.T) {
	defer func(v time.Duration) { transientTolerance = v }(transientTolerance)
	transientTolerance = 500 * time.Millisecond
	src := &phasedSnapshots{steps: []snapStep{
		{calls: 1, snap: songSnap("MUSIC_1", "A")},
		{calls: 4, err: cdp.ErrSongChanged}, // empty status
		{calls: 1 << 30, snap: songSnap("MUSIC_2", "B")},
	}}
	events := runUntilSong(t, src, &switchReader{}, "B")
	if _, n := src.errStats(); n != 4 {
		t.Fatalf("settling phase was not exercised: %d error calls", n)
	}
	if waitingProcessSeen(events) {
		t.Fatal("player reported waiting_process during a switch")
	}
}

// A bridge that never settles still fails closed once the tolerance is spent,
// and only after it has actually retried for that long.
// 变异自证：去掉容忍分支则首次瞬态即返回（重试次数不足），或把“< transientTolerance”
// 改成恒真则永不返回，本用例都变红（实测）。
func TestPersistentSettlingFailsClosed(t *testing.T) {
	defer func(v time.Duration) { transientTolerance = v }(transientTolerance)
	transientTolerance = 200 * time.Millisecond
	src := &phasedSnapshots{steps: []snapStep{
		{calls: 1, snap: songSnap("MUSIC_1", "A")},
		{calls: 1 << 30, err: cdp.ErrSongChanged},
	}}
	p := New(0, 50)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := p.runSession(ctx, src, func() (positionSource, error) { return &switchReader{}, nil })
	end := time.Now()
	if !errors.Is(err, cdp.ErrSongChanged) {
		t.Fatalf("want ErrSongChanged after the tolerance, got %v", err)
	}
	first, n := src.errStats()
	if n < 3 {
		t.Fatalf("gave up after %d error calls: the tolerance branch did not retry", n)
	}
	// Bounded on both sides: a tolerance that never expires would only stop when
	// the 3s context ends, with the same error, so the upper bound is what catches it.
	held := end.Sub(first)
	if held < transientTolerance-30*time.Millisecond {
		t.Fatalf("failed closed after %v, before the %v tolerance", held, transientTolerance)
	}
	if held > transientTolerance+800*time.Millisecond {
		t.Fatalf("failed closed after %v, long after the %v tolerance", held, transientTolerance)
	}
}

// The bridge settling and the core reconciling are separate budgets: each is
// shorter than the tolerance, together they are longer. With one shared clock
// this switch would end the session.
// 变异自证：把快照瞬态改回与核心样本共用 failedAt，本用例变红（实测）。
func TestSettlingAndCoreReconcileUseSeparateBudgets(t *testing.T) {
	defer func(v time.Duration) { transientTolerance = v }(transientTolerance)
	transientTolerance = 500 * time.Millisecond
	src := &phasedSnapshots{steps: []snapStep{
		{calls: 1, snap: songSnap("MUSIC_1", "A")},
		{calls: 6, err: cdp.ErrSongChanged}, // about 300ms < 500ms
		{calls: 1 << 30, snap: songSnap("MUSIC_2", "B")},
	}}
	// After the bridge settles the core still disagrees for 6 more polls (about
	// 300ms < 500ms); the two phases together exceed the tolerance.
	reader := &switchReader{badRID: "MUSIC_2", badReads: 6}
	runUntilSong(t, src, reader, "B")
	if _, n := src.errStats(); n != 6 {
		t.Fatalf("settling phase was not exercised: %d error calls", n)
	}
	reader.mu.Lock()
	left := reader.badReads
	reader.mu.Unlock()
	if left != 0 {
		t.Fatalf("core reconcile phase was not exercised: %d bad reads left", left)
	}
}
