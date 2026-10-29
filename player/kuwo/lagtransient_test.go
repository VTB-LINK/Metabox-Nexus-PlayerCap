package kuwo

// This file tests the #135 wiring (doc/design/kuwo-output-transient-design.md
// §3.2, §3.6, §3.8): the transient filter sits between the core read and the
// #131 corrector in correctedSource, the raw core CoreMs reaches the three
// consumers (the mismatch rule, the stall guard), and TransientResync reaches
// the session's notice. The filter's own state machine is tested offline in
// package pos (transient_test.go); here it runs through the real correctedSource
// and session. Each assertion has a mutation that turns it red; the list is in
// the task report.

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

const tbpsK = 176400 // 44.1 kHz, 16-bit, stereo: bytes per second

// fedBytesK converts a fed duration in ms to the DirectSound byte count.
func fedBytesK(ms int64) uint32 { return uint32(ms * tbpsK / 1000) }

func dsOutK(base uint32, fedMs int64) pos.OutputState {
	return pos.OutputState{Valid: true, Device: pos.DeviceDirectSound, Base: base, Bps: tbpsK, Fed: fedBytesK(fedMs)}
}

// dsTick drives one poll with the status set and a DirectSound output state.
func (r *lagRig) dsTick(ms, status int, core int32, base uint32, fedMs int64, lag pos.LagSample) {
	r.status = status
	r.lag.out = dsOutK(base, fedMs)
	r.poll(ms, core, lag, nil)
}

func countResumes(events []player.Event) int { return len(ofType(events, player.EventPlaybackResume)) }

// Rule 5 (mismatch) reads the raw core CoreMs, not the filtered PositionMs. In a
// transient the filter pins PositionMs at the base (low) while the raw core
// overshoots (high); with an old segment's e_cur still in effect the pinned
// PositionMs can be below it and the raw core above it, so the two sides of the
// comparison disagree.
//
// Mutation (verified): comparing core.PositionMs turns this red — the output
// becomes 200 instead of 0.
func TestLagRuleFiveReadsCoreMs(t *testing.T) {
	c := &pos.LagCorrector{}
	const rid = "MUSIC_1"
	// Adopt e_cur = 314 ms in segment A (base 30000).
	segA := newFeedSeg(30000)
	c.Apply(pos.Sample{RID: rid, PositionMs: 30000, CoreMs: 30000, DurationMs: lagDur, ObservedAt: lagT0}, segA.next(314000))
	c.Apply(pos.Sample{RID: rid, PositionMs: 30050, CoreMs: 30050, DurationMs: lagDur, ObservedAt: after(lagT0, 50)}, segA.next(314000))
	// A new segment (base 200): the filter pinned PositionMs at the base 200,
	// below e_cur 314, while the raw core overshot to 600, above e_cur. e_cur
	// carries over until the new segment confirms. Rule 5 must read CoreMs = 600
	// (>= 314, no mismatch, e_cur kept) so the output clamps to 0; reading
	// PositionMs = 200 (< 314) would zero e_cur and publish 200.
	segB := newFeedSeg(200)
	out, _ := c.Apply(pos.Sample{RID: rid, PositionMs: 200, CoreMs: 600, DurationMs: lagDur, ObservedAt: after(lagT0, 100)}, segB.next(0))
	if out.PositionMs != 0 {
		t.Fatalf("rule 5 output = %d, want 0 (it must read CoreMs, not PositionMs)", out.PositionMs)
	}
}

// pinnedCore reports a frozen PositionMs with an advancing CoreMs, as the
// transient filter does when it pins the public position at the base while the
// raw core overshoots.
type pinnedCore struct {
	n      int
	closes int
}

func (c *pinnedCore) Read(rid string, _ bool) (pos.Sample, error) {
	c.n++
	return pos.Sample{RID: pos.RIDWithPrefix(rid), PositionMs: 30000, CoreMs: int32(30000 + c.n*100), DurationMs: 240000, ObservedAt: time.Now()}, nil
}
func (c *pinnedCore) Close() { c.closes++ }

// The stall guard watches the raw core CoreMs, not the public PositionMs. With
// PositionMs pinned (a transient) but CoreMs advancing, the song is live and
// must not fail closed even with the threshold at a nanosecond.
//
// Mutation (verified): feeding the guard sample.PositionMs (frozen) trips it and
// the session returns ErrClockStalled instead of the snapshot's end error.
func TestLagStallGuardReadsCoreMs(t *testing.T) {
	defer func(v time.Duration) { livenessThreshold = v }(livenessThreshold)
	livenessThreshold = time.Nanosecond
	p := New(0, 50)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reader := &pinnedCore{}
	end := errors.New("snapshot ended")
	calls := 0
	err := p.runSession(ctx, snapshotFunc(func(context.Context) (cdp.Snapshot, error) {
		if calls++; calls >= 6 {
			return cdp.Snapshot{}, end
		}
		return cdp.Snapshot{RID: "MUSIC_1", Status: 1, Title: "t"}, nil
	}), func() (positionSource, error) { return reader, nil })
	if !errors.Is(err, end) {
		t.Fatalf("advancing CoreMs under a pinned PositionMs failed closed: %v", err)
	}
}

// A DirectSound seek: the filter pins the public position at the target through
// the multi-report ladder, so the corrector never sees 30313/30600/30914 and
// the public position is min-capped below the raw core. With e = 0 the segment
// publishes one playback_resume (the kernel jump); when the lag confirms one
// tick late the backward step off the correction publishes the #131 delayed
// second one (design §3.5, not consumed by #135).
//
// Mutation (verified): bypassing the filter publishes the ladder values and
// out.PositionMs reaches the raw core, turning the cap assertions red.
func TestLagTransientDSSeekResumeCount(t *testing.T) {
	cases := []struct {
		name string
		seg  *feedSeg // nil: e stays 0; otherwise confirm e = 314 from t100
		want int
	}{
		{"playing seek, e zero, one resume", nil, 1},
		{"delayed confirm, two resumes", newFeedSeg(30000), 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLagRig(t)
			lagAt := func(ms int) pos.LagSample {
				if tc.seg == nil || ms < 50 {
					return noLag
				}
				return tc.seg.next(314000)
			}
			r.dsTick(0, 1, 10000, 10000, 10000, noLag) // steady: segment base 10000, degraded first tick
			r.dsTick(50, 1, 30313, 30000, 313, lagAt(50))
			r.dsTick(100, 1, 30600, 30000, 600, lagAt(100))
			r.dsTick(150, 1, 30914, 30000, 914, lagAt(150))
			r.dsTick(200, 1, 30914, 30000, 1800, lagAt(200))
			// The transient ticks must publish the target, never the ladder, and the
			// public position must stay at or below the raw core.
			for _, e := range r.events {
				if ms := eventMs(e); ms == 30313 || ms == 30600 || ms == 30914 {
					t.Fatalf("a ladder multi-report value %d was published", ms)
				}
			}
			if r.out.CoreMs != 30914 || r.out.PositionMs > r.out.CoreMs {
				t.Fatalf("last tick: PositionMs %d, CoreMs %d; public must be <= raw core", r.out.PositionMs, r.out.CoreMs)
			}
			if got := countResumes(r.events); got != tc.want {
				t.Fatalf("resumes = %d, want %d", got, tc.want)
			}
		})
	}
}

// Paused-then-dragged DirectSound (design §3.7): the drag lands on the resume
// tick (one resume, the kernel jump), the public position is pinned at the
// target while DirectSound takes ~0.95 s to start, and TransientResync on the
// "Play issued" tick publishes a second resume because the front-end model has
// run ahead. Two resumes in all.
//
// Mutation (verified): dropping sample.TransientResync from the notice condition
// turns this red — only the kernel-jump resume is published.
func TestLagTransientPauseDragDSTwoResumes(t *testing.T) {
	r := newLagRig(t)
	r.dsTick(0, 1, 10000, 10000, 20000, noLag)  // playing, steady at 10000
	r.dsTick(50, 2, 10000, 10000, 20000, noLag) // paused; PlayerCore still reports the old value
	r.dsTick(100, 1, 30313, 30000, 100, noLag)  // resume + drag: kernel jump, resume #1
	// Slow DirectSound start: fed stays below the Play threshold until t950.
	for ms := 150; ms < 950; ms += 50 {
		r.dsTick(ms, 1, 30600, 30000, 600, noLag)
	}
	r.dsTick(950, 1, 30914, 30000, 800, noLag) // fed crosses: TransientResync, resume #2
	if got := countResumes(r.events); got != 2 {
		t.Fatalf("resumes = %d, want 2 (kernel jump + TransientResync)", got)
	}
}

// With the filter not engaged — WASAPI, or an invalid output state — the
// corrected source is sample-for-sample what #131 produces alone. The reference
// corrector is fed the same raw samples with no filter.
//
// Mutation (verified): having the filter alter PositionMs on a passthrough tick
// turns this red.
func TestLagTransientDisabledMatchesSeekLagAlone(t *testing.T) {
	// The sequence is a DirectSound-shaped seek (base 30000, a multi-report
	// ladder); only the device decides whether it is filtered. Were WASAPI not
	// degraded, the filter would pin P_f at 30000 and the output would diverge
	// from the raw core, so the ladder makes the passthrough contract observable.
	seq := []struct {
		ms    int
		core  int32
		base  uint32
		fedMs int64
		lag   pos.LagSample
	}{
		{0, 10000, 10000, 10000, noLag},
		{50, 30313, 30000, 313, noLag},
		{100, 30600, 30000, 600, noLag},
		{150, 30914, 30000, 914, noLag},
		{200, 30914, 30000, 1800, noLag},
	}
	for _, dev := range []struct {
		name   string
		device pos.DeviceKind
		valid  bool
	}{
		{"wasapi", pos.DeviceWASAPI, true},
		{"invalid", pos.DeviceUnknown, false},
	} {
		t.Run(dev.name, func(t *testing.T) {
			r := newLagRig(t)
			ref := &pos.LagCorrector{}
			for _, k := range seq {
				at := after(lagT0, k.ms)
				out := pos.OutputState{}
				if dev.valid {
					out = pos.OutputState{Valid: true, Device: dev.device, Base: k.base, Bps: tbpsK, Fed: fedBytesK(k.fedMs)}
				}
				r.core.sample = pos.Sample{RID: "MUSIC_1", PositionMs: k.core, CoreMs: k.core, DurationMs: lagDur, ObservedAt: at}
				r.lag.sample, r.lag.err, r.lag.out = k.lag, nil, out
				got, err := r.src.Read("MUSIC_1", true)
				if err != nil {
					t.Fatalf("t=%d ms: Read returned %v", k.ms, err)
				}
				want, _ := ref.Apply(pos.Sample{RID: "MUSIC_1", PositionMs: k.core, CoreMs: k.core, DurationMs: lagDur, ObservedAt: at}, k.lag)
				if got.PositionMs != want.PositionMs || got.LagAppliedMs != want.LagAppliedMs || got.LagShiftMs != want.LagShiftMs || got.LagAdopted != want.LagAdopted || got.TransientResync {
					t.Fatalf("t=%d ms: filtered %+v != seek-lag alone %+v", k.ms, got, want)
				}
			}
		})
	}
}

// observedResync is observedLag with TransientResync set, for the session's
// notice condition (design §3.6).
func observedResync(s *session, rid string, status int, ms int32, at time.Time, shift int32, resync bool) bool {
	return s.observe(cdp.Snapshot{RID: rid, Title: "song " + rid, Artist: "artist", Status: status},
		pos.Sample{RID: rid, PositionMs: ms, DurationMs: 100000, ObservedAt: at, LagShiftMs: shift, TransientResync: resync})
}

// TransientResync drives the seek notice like LagShiftMs and LagAdopted: it is
// judged against the front-end model, merges with a same-tick shift into one
// resume, is re-delivered after a drop, and is ignored on a song change.
//
// Mutation (verified): dropping TransientResync from the notice condition turns
// "resync alone" and "redelivered after a drop" red.
func TestLagTransientResyncNotice(t *testing.T) {
	// Resync alone (no shift): the public position is 220 ms off the model, so it
	// publishes one resume; that step is not a CoreClock seek on its own.
	t.Run("resync alone", func(t *testing.T) {
		p := New(0, 50)
		s := newSession(p)
		t0 := time.Now()
		observedResync(s, "MUSIC_1", 1, 10000, t0, 0, false) // anchors the model via all_lyrics
		drainEvents(p)
		observedResync(s, "MUSIC_1", 1, 10270, after(t0, 50), 0, true)
		if got := published(drainEvents(p)); !slices.Equal(got, []string{resumeAt(10270)}) {
			t.Fatalf("resync alone published %v, want one resume", got)
		}
	})
	// Resync and a shift on the same tick: one resume, not two.
	t.Run("merges with a shift", func(t *testing.T) {
		p := New(0, 50)
		s := newSession(p)
		t0 := time.Now()
		observedResync(s, "MUSIC_1", 1, 10000, t0, 0, false)
		drainEvents(p)
		observedResync(s, "MUSIC_1", 1, 10270, after(t0, 50), 220, true)
		if n := len(ofType(drainEvents(p), player.EventPlaybackResume)); n != 1 {
			t.Fatalf("resync and shift on one tick published %d resumes, want 1", n)
		}
	})
	// A dropped resume (full channel) is re-delivered on the next tick.
	t.Run("redelivered after a drop", func(t *testing.T) {
		p := New(0, 50)
		s := newSession(p)
		t0 := time.Now()
		observedResync(s, "MUSIC_1", 1, 10000, t0, 0, false)
		drainEvents(p)
		fillEvents(p)
		observedResync(s, "MUSIC_1", 1, 10270, after(t0, 50), 0, true)
		if n := len(ofType(drainEvents(p), player.EventPlaybackResume)); n != 0 {
			t.Fatalf("resume was not dropped: %d", n)
		}
		observedResync(s, "MUSIC_1", 1, 10320, after(t0, 100), 0, false)
		if got := published(drainEvents(p)); !slices.Equal(got, []string{resumeAt(10320)}) {
			t.Fatalf("after the drop: got %v, want the redelivered resume", got)
		}
	})
	// A song change publishes no resume even with TransientResync set on the
	// change tick; the notice is cleared with the session.
	t.Run("ignored on a song change", func(t *testing.T) {
		p := New(0, 50)
		s := newSession(p)
		t0 := time.Now()
		observedResync(s, "MUSIC_1", 1, 10000, t0, 0, false)
		drainEvents(p)
		if !observedResync(s, "MUSIC_2", 1, 2000, after(t0, 50), 0, true) {
			t.Fatal("song change not detected")
		}
		if got := published(drainEvents(p)); got != nil {
			t.Fatalf("the change tick published %v", got)
		}
	})
}

// The corrected source logs one Detail as a segment first enters filtering
// (design §3.8), naming the method and base, and does not repeat it on the
// following filtering ticks.
//
// Mutation (verified): dropping the engage log turns this red; TransientResync
// cannot stand in for it (it marks only the Play-issued tick, a different tick).
func TestLagTransientEngageLogsOnce(t *testing.T) {
	buf := captureLog(t)
	r := newLagRig(t)
	r.dsTick(0, 1, 10000, 10000, 10000, noLag) // steady segment base 10000, degraded first tick
	r.dsTick(50, 1, 30313, 30000, 313, noLag)  // seek: new segment base 30000, engage here
	r.dsTick(100, 1, 30600, 30000, 600, noLag) // same segment, no repeat
	r.dsTick(150, 1, 30914, 30000, 914, noLag)
	if got := logIcons(buf, "output transient filter engaged"); !slices.Equal(got, []string{"[+]"}) {
		t.Fatalf("engage log %v, want one [+], in %q", got, buf.String())
	}
	if got := logIcons(buf, "base 30000 ms"); !slices.Equal(got, []string{"[+]"}) {
		t.Fatalf("engage base log %v, want one [+], in %q", got, buf.String())
	}
}
