package pos

import (
	"fmt"
	"testing"
	"time"
)

// This file tests only the seek-lag corrector, LagCorrector.Apply: the
// deduction it applies to core samples and the shift it reports
// (doc/design/kuwo-seek-lag-design.md §3.3 and the §4 corrector cases). Event
// counts are tested in package kuwo.

const (
	lagTestRID  = "MUSIC_568213041"
	lagTestRID2 = "MUSIC_658118183"
	lagTestDur  = 240000 // ms
	lagD        = 23200  // packet duration, µs
)

var lagT0 = time.Unix(1000, 0)

// noLag is an invalid sample.
var noLag LagSample

// lagSeg yields the valid samples of one output segment.
type lagSeg struct {
	key     LagKey
	written uint32
}

func newLagSeg(base uint32) *lagSeg {
	return &lagSeg{key: LagKey{Ctx: 0x05a32e40, Ring: 0x06120a00, Base: base}}
}

// next is a sample taken after more output was written.
func (s *lagSeg) next(eUs, dUs int64) LagSample {
	s.written += 4096
	return s.held(eUs, dUs)
}

// held is a sample with nothing written since the previous one (paused).
func (s *lagSeg) held(eUs, dUs int64) LagSample {
	return LagSample{Valid: true, Key: s.key, Written: s.written, EUs: eUs, DUs: dUs}
}

// lagRig feeds one corrector; times are ms after lagT0.
type lagRig struct {
	t   *testing.T
	c   LagCorrector
	rid string
}

func newLagRig(t *testing.T) *lagRig { return &lagRig{t: t, rid: lagTestRID} }

// step applies one tick and checks that the fields other than the position
// and the shift pass through, and that LagAppliedMs is the deduction applied.
func (r *lagRig) step(ms int, core int32, lag LagSample) (Sample, LagNote) {
	r.t.Helper()
	// CoreMs mirrors PositionMs here: the transient filter is not in this test,
	// so the raw core and the (unfiltered) position are equal. Rule 5 reads CoreMs.
	in := Sample{RID: r.rid, PositionMs: core, CoreMs: core, DurationMs: lagTestDur, ObservedAt: lagT0.Add(time.Duration(ms) * time.Millisecond)}
	out, note := r.c.Apply(in, lag)
	if out.RID != in.RID || out.DurationMs != in.DurationMs || !out.ObservedAt.Equal(in.ObservedAt) {
		r.t.Fatalf("t=%d ms: output %+v does not carry the core fields of %+v", ms, out, in)
	}
	if out.PositionMs+out.LagAppliedMs != core {
		r.t.Fatalf("t=%d ms: position %d + applied %d is not the core %d", ms, out.PositionMs, out.LagAppliedMs, core)
	}
	return out, note
}

// want applies one tick and checks the output position and shift.
func (r *lagRig) want(ms int, core int32, lag LagSample, pos, shift int32) LagNote {
	r.t.Helper()
	out, note := r.step(ms, core, lag)
	if out.PositionMs != pos || out.LagShiftMs != shift {
		r.t.Fatalf("t=%d ms, core %d: got position %d, shift %d; want %d, %d", ms, core, out.PositionMs, out.LagShiftMs, pos, shift)
	}
	return note
}

// adopt confirms e = 314 ms in seg at t = 0 and 50 ms, the core at core and
// core + 50.
func (r *lagRig) adopt(seg *lagSeg, core int32) {
	r.t.Helper()
	r.want(0, core, seg.next(314000, lagD), core, 0)
	r.want(50, core+50, seg.next(314000, lagD), core+50-314, -314)
}

// lagStep is one valid sample and the deduction expected after it.
type lagStep struct {
	e, d int64
	ded  int32
}

// runLagSteps feeds steps of one segment to a new corrector 50 ms apart, the
// core advancing adv ms per tick from 10000. No step may disable the song.
func runLagSteps(t *testing.T, adv int32, steps []lagStep) {
	t.Helper()
	r, a := newLagRig(t), newLagSeg(10000)
	var prev int32
	for i, s := range steps {
		core := 10000 + adv*int32(i)
		if n := r.want(50*i, core, a.next(s.e, s.d), core-s.ded, prev-s.ded); n.Kind == LagNoteDrift {
			t.Fatalf("step %d disabled the song: %+v", i, n)
		}
		prev = s.ded
	}
}

func TestLagCorrectorConfirmTolerance(t *testing.T) {
	cases := []struct {
		name  string
		steps []lagStep
	}{
		{"1.0 ms apart confirms the larger, rising", []lagStep{{313000, lagD, 0}, {314000, lagD, 314}}},
		{"1.0 ms apart confirms the larger, falling", []lagStep{{314000, lagD, 0}, {313000, lagD, 314}}},
		{"1.1 ms apart does not confirm", []lagStep{{314000, lagD, 0}, {312900, lagD, 0}}},
		// A first sample one packet short does not pair with the correct one.
		{"short first sample", []lagStep{{290800, lagD, 0}, {314000, lagD, 0}, {314000, lagD, 314}}},
		// Neither a single short sample nor two equal ones lower the value.
		{"never lowered", []lagStep{{314000, lagD, 0}, {314000, lagD, 314}, {290800, lagD, 314}, {290800, lagD, 314}, {314000, lagD, 314}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runLagSteps(t, 50, tc.steps) })
	}
}

// c0 = 290.8 ms and D0 = 23.2 ms bound every raise of the segment at 315.0 ms.
func TestLagCorrectorRaiseBound(t *testing.T) {
	cases := []struct {
		name  string
		steps []lagStep
	}{
		{"315.0 accepted", []lagStep{{290800, lagD, 0}, {290800, lagD, 291}, {315000, lagD, 291}, {315000, lagD, 315}}},
		{"315.1 rejected", []lagStep{{290800, lagD, 0}, {290800, lagD, 291}, {315100, lagD, 291}, {315100, lagD, 291}}},
		// A raise neither moves the bound nor widens it, even by a pair with a
		// larger D.
		{"bound kept after a raise to 300.9", []lagStep{
			{290800, lagD, 0}, {290800, lagD, 291}, {300900, 40000, 291}, {300900, 40000, 301},
			{315100, lagD, 301}, {315100, lagD, 301}, {315000, lagD, 301}, {315000, lagD, 315},
		}},
		{"D0 is the larger D, given first", []lagStep{{290800, lagD, 0}, {290800, 20000, 291}, {315000, lagD, 291}, {315000, lagD, 315}}},
		{"D0 is the larger D, given second", []lagStep{{290800, 20000, 0}, {290800, lagD, 291}, {315000, lagD, 291}, {315000, lagD, 315}}},
		{"a raise needs at least 1 ms", []lagStep{{314000, lagD, 0}, {314000, lagD, 314}, {314900, lagD, 314}, {314900, lagD, 314}, {315000, lagD, 315}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runLagSteps(t, 50, tc.steps) })
	}
}

// Round 2 review: true e 314 ms, two equal short samples confirm 290.8 first.
// The core stands still (as right after a DirectSound seek), so the raise
// moves the output back.
func TestLagCorrectorRound2Raise(t *testing.T) {
	runLagSteps(t, 0, []lagStep{{290800, lagD, 0}, {290800, lagD, 291}, {314000, lagD, 291}, {314000, lagD, 314}, {314000, lagD, 314}})
}

// Round 5 review: two different short values before the true one.
func TestLagCorrectorRound5Raises(t *testing.T) {
	steps := []lagStep{{290800, lagD, 0}, {290800, lagD, 291}, {300900, 13100, 291}, {300900, 13100, 301}, {314000, lagD, 301}, {314000, lagD, 314}}
	for range 10 {
		steps = append(steps, lagStep{314000, lagD, 314})
	}
	runLagSteps(t, 50, steps)
}

// Confirmed on the tick the core jumps: that tick already outputs target - e.
func TestLagCorrectorConfirmOnJumpTick(t *testing.T) {
	r, b := newLagRig(t), newLagSeg(30000)
	r.want(0, 10000, noLag, 10000, 0)
	r.want(50, 10050, b.next(314000, lagD), 10050, 0)
	r.want(100, 30000, b.next(314000, lagD), 30000-314, -314)
}

// Round 2 review: confirmed two ticks before the core jumps.
func TestLagCorrectorConfirmBeforeJump(t *testing.T) {
	r, b := newLagRig(t), newLagSeg(30000)
	r.want(0, 10000, noLag, 10000, 0)
	r.want(50, 10000, b.next(314000, lagD), 10000, 0)
	r.want(100, 10000, b.next(314000, lagD), 9686, -314)
	r.want(150, 30000, b.next(314000, lagD), 30000-314, 0)
	r.want(200, 30050, b.next(314000, lagD), 30050-314, 0)
}

// Round 1 review: the core jumps on a tick without a valid sample, the new
// segment is confirmed two ticks later.
func TestLagCorrectorConfirmAfterJump(t *testing.T) {
	r, b := newLagRig(t), newLagSeg(30000)
	r.want(0, 10000, noLag, 10000, 0)
	r.want(50, 30000, noLag, 30000, 0)
	r.want(100, 30050, b.next(314000, lagD), 30050, 0)
	r.want(150, 30100, b.next(314000, lagD), 30100-314, -314)
	r.want(200, 30150, b.next(314000, lagD), 30150-314, 0)
}

// Round 4 review: a 150 ms forward seek, below the jump threshold.
func TestLagCorrectorSmallForwardSeek(t *testing.T) {
	r, b := newLagRig(t), newLagSeg(6000)
	r.want(0, 5850, noLag, 5850, 0)
	r.want(50, 6000, b.next(314000, lagD), 6000, 0)
	r.want(100, 6050, b.next(314000, lagD), 5736, -314)
}

// Round 1 review: every sample fails after a seek. 500 ms after the last valid
// sample the correction drops in one step; the same segment recovering stays
// excluded, a new segment is adopted.
func TestLagCorrectorFailure(t *testing.T) {
	r, a := newLagRig(t), newLagSeg(59000)
	r.adopt(a, 59800)
	for ms := 100; ms <= 950; ms += 50 {
		core := int32(60000 + ms - 100)
		pos, shift := core-314, int32(0)
		switch {
		case ms == 550:
			pos, shift = core, 314
		case ms > 550:
			pos = core
		}
		r.want(ms, core, noLag, pos, shift)
	}
	r.want(1000, 60900, a.next(314000, lagD), 60900, 0)
	r.want(1050, 60950, a.next(314000, lagD), 60950, 0)
	b := newLagSeg(61000)
	r.want(1100, 61000, b.next(314000, lagD), 61000, 0)
	r.want(1150, 61050, b.next(314000, lagD), 61050-314, -314)
}

// Round 4 review: paused at 10000, every sample fails for 10 s.
func TestLagCorrectorFailureWhilePaused(t *testing.T) {
	r, a := newLagRig(t), newLagSeg(10000)
	r.want(0, 10000, a.next(314000, lagD), 10000, 0)
	r.want(50, 10000, a.held(314000, lagD), 9686, -314)
	for ms := 100; ms <= 10050; ms += 50 {
		pos, shift := int32(9686), int32(0)
		switch {
		case ms == 550:
			pos, shift = 10000, 314
		case ms > 550:
			pos = 10000
		}
		r.want(ms, 10000, noLag, pos, shift)
	}
}

// The tick's sample is taken before the timers are judged: a valid sample on
// the 500 ms tick keeps the correction, and the failure timer restarts from it.
func TestLagCorrectorSampleBeforeTimers(t *testing.T) {
	r, a := newLagRig(t), newLagSeg(10000)
	r.adopt(a, 10000)
	for ms := 100; ms <= 1050; ms += 50 {
		core := int32(10000 + ms)
		lag, pos, shift := noLag, core-314, int32(0)
		switch ms {
		case 550:
			lag = a.next(314000, lagD)
		case 1050:
			pos, shift = core, 314
		}
		r.want(ms, core, lag, pos, shift)
	}
}

// A new segment keeps the previous correction while unconfirmed; 500 ms after
// it was first seen the correction is 0; a later confirmation is adopted.
func TestLagCorrectorUnconfirmedTimeout(t *testing.T) {
	r, a, b := newLagRig(t), newLagSeg(10000), newLagSeg(30000)
	r.adopt(a, 10000)
	for ms := 100; ms <= 600; ms += 50 {
		e := int64(100000)
		if ms%100 != 0 {
			e = 150000
		}
		core := int32(30000 + ms - 100)
		pos, shift := core-314, int32(0)
		if ms == 600 {
			pos, shift = core, 314
		}
		r.want(ms, core, b.next(e, lagD), pos, shift)
	}
	r.want(650, 30550, b.next(200000, lagD), 30550, 0)
	r.want(700, 30600, b.next(200000, lagD), 30600-200, -200)
}

// Review Focus 1: with a 1000 ms poll, one stray invalid sample neither zeroes
// the correction nor keeps a new segment from being confirmed by its second
// sample.
func TestLagCorrectorSlowPoll(t *testing.T) {
	t.Run("one invalid sample", func(t *testing.T) {
		r, a := newLagRig(t), newLagSeg(10000)
		r.want(0, 10000, a.next(314000, lagD), 10000, 0)
		r.want(1000, 11000, a.next(314000, lagD), 11000-314, -314)
		r.want(2000, 12000, noLag, 12000-314, 0)
		r.want(3000, 13000, a.next(314000, lagD), 13000-314, 0)
		r.want(4000, 14000, noLag, 14000-314, 0)
		r.want(5000, 15000, noLag, 15000, 314)
	})
	t.Run("new segment, consecutive samples", func(t *testing.T) {
		r, a, b := newLagRig(t), newLagSeg(10000), newLagSeg(30000)
		r.want(0, 10000, a.next(314000, lagD), 10000, 0)
		r.want(1000, 11000, a.next(314000, lagD), 11000-314, -314)
		r.want(2000, 30000, b.next(200000, lagD), 30000-314, 0)
		r.want(3000, 31000, b.next(200000, lagD), 31000-200, 114)
	})
	t.Run("new segment, invalid sample between", func(t *testing.T) {
		r, a, b := newLagRig(t), newLagSeg(10000), newLagSeg(30000)
		r.want(0, 10000, a.next(314000, lagD), 10000, 0)
		r.want(1000, 11000, a.next(314000, lagD), 11000-314, -314)
		r.want(2000, 30000, b.next(200000, lagD), 30000-314, 0)
		r.want(3000, 31000, noLag, 31000-314, 0)
		r.want(4000, 32000, b.next(200000, lagD), 32000-200, 114)
	})
}

func TestLagCorrectorSongChange(t *testing.T) {
	t.Run("drops at once, earlier segment not adopted", func(t *testing.T) {
		r, a, b := newLagRig(t), newLagSeg(50000), newLagSeg(0)
		r.adopt(a, 50000)
		// The decoder opens the next song before the core reports it.
		r.want(100, 50100, b.next(20000, lagD), 50100-314, 0)
		r.rid = lagTestRID2
		for ms := 150; ms <= 500; ms += 50 {
			core := int32(1000 + ms - 150)
			r.want(ms, core, b.next(20000, lagD), core, 0)
		}
	})
	t.Run("seek during the change adopted", func(t *testing.T) {
		r, a, c := newLagRig(t), newLagSeg(50000), newLagSeg(30000)
		r.adopt(a, 50000)
		r.rid = lagTestRID2
		r.want(100, 30000, c.next(314000, lagD), 30000, 0)
		r.want(150, 30050, c.next(314000, lagD), 30050-314, -314)
	})
}

// The session's first sample passes through; its segment is adopted once
// confirmed.
func TestLagCorrectorFirstSample(t *testing.T) {
	r, a := newLagRig(t), newLagSeg(10000)
	if n := r.want(0, 10000, a.next(314000, lagD), 10000, 0); n.Kind != LagNoteNone {
		t.Fatalf("first sample noted %+v", n)
	}
	r.want(50, 10050, a.next(314000, lagD), 10050-314, -314)
}

// A seek to the same whole second keeps the key but restarts Written: a new
// segment, whose lower value is adopted.
func TestLagCorrectorSameSecondSeek(t *testing.T) {
	r, a := newLagRig(t), newLagSeg(30000)
	r.adopt(a, 30000)
	b := &lagSeg{key: a.key}
	r.want(100, 30000, b.next(200000, lagD), 30000-314, 0)
	r.want(150, 30050, b.next(200000, lagD), 30050-200, 114)
}

// Review Focus 5: paused 10 s with nothing written, then resumed; the
// correction never changes.
func TestLagCorrectorPauseKeepsCorrection(t *testing.T) {
	r, a := newLagRig(t), newLagSeg(10000)
	r.adopt(a, 10000)
	for ms := 100; ms <= 10050; ms += 50 {
		if n := r.want(ms, 10050, a.held(314000, lagD), 10050-314, 0); n.Kind != LagNoteNone {
			t.Fatalf("t=%d ms noted %+v", ms, n)
		}
	}
	for ms := 10100; ms <= 10500; ms += 50 {
		core := int32(ms)
		if n := r.want(ms, core, a.next(314000, lagD), core-314, 0); n.Kind != LagNoteNone {
			t.Fatalf("t=%d ms noted %+v", ms, n)
		}
	}
}

// DirectSound after a seek: the core reports T, T+313, T+600, then stops for
// about 0.9 s while the output is written. The correction holds.
func TestLagCorrectorDirectSoundStall(t *testing.T) {
	r, a := newLagRig(t), newLagSeg(30000)
	r.want(0, 30000, a.next(314000, lagD), 30000, 0)
	r.want(50, 30313, a.next(314000, lagD), 30313-314, -314)
	for ms := 100; ms <= 1300; ms += 50 {
		core := int32(30600)
		if ms > 1000 {
			core += int32(ms - 1000)
		}
		r.want(ms, core, a.next(314000, lagD), core-314, 0)
	}
}

// The drift band of conf 314 ms for D 23.2 ms is [285.8, 342.2] ms.
func TestLagCorrectorDrift(t *testing.T) {
	drift := func(seg *lagSeg) LagNote { return LagNote{Kind: LagNoteDrift, EUs: 350000, Key: seg.key} }
	t.Run("three out of band disable the song", func(t *testing.T) {
		r, a := newLagRig(t), newLagSeg(10000)
		r.adopt(a, 10000)
		r.want(100, 10100, a.next(350000, lagD), 10100-314, 0)
		r.want(150, 10150, a.next(350000, lagD), 10150-314, 0)
		if n := r.want(200, 10200, a.next(350000, lagD), 10200, 314); n != drift(a) {
			t.Fatalf("note %+v, want %+v", n, drift(a))
		}
		b := newLagSeg(20000)
		for ms := 250; ms <= 500; ms += 50 {
			seg := a
			if ms > 350 {
				seg = b
			}
			if n := r.want(ms, int32(10000+ms), seg.next(314000, lagD), int32(10000+ms), 0); n.Kind != LagNoteNone {
				t.Fatalf("t=%d ms noted %+v", ms, n)
			}
		}
	})
	// keep feeds (e, D) pairs to an adopted segment and expects 314 ms kept.
	keep := func(t *testing.T, ed ...int64) {
		t.Helper()
		r, a := newLagRig(t), newLagSeg(10000)
		r.adopt(a, 10000)
		for i := 0; i < len(ed); i += 2 {
			ms := 100 + 25*i
			if n := r.want(ms, int32(10000+ms), a.next(ed[i], ed[i+1]), int32(10000+ms-314), 0); n.Kind != LagNoteNone {
				t.Fatalf("t=%d ms noted %+v", ms, n)
			}
		}
	}
	t.Run("two out of band and the band edges do not", func(t *testing.T) {
		keep(t, 350000, lagD, 350000, lagD, 342200, lagD,
			350000, lagD, 350000, lagD, 285800, lagD,
			250000, lagD, 250000, lagD, 314000, lagD)
	})
	t.Run("band widens with the sample's D", func(t *testing.T) {
		keep(t, 350000, lagD, 350000, lagD, 350000, 40000, 350000, lagD)
	})
	t.Run("invalid samples do not break a run", func(t *testing.T) {
		r, a := newLagRig(t), newLagSeg(10000)
		r.adopt(a, 10000)
		r.want(100, 10100, a.next(350000, lagD), 10100-314, 0)
		r.want(150, 10150, noLag, 10150-314, 0)
		r.want(200, 10200, a.next(350000, lagD), 10200-314, 0)
		r.want(250, 10250, noLag, 10250-314, 0)
		if n := r.want(300, 10300, a.next(350000, lagD), 10300, 314); n != drift(a) {
			t.Fatalf("note %+v, want %+v", n, drift(a))
		}
	})
	t.Run("an unadopted segment does not drift", func(t *testing.T) {
		r, a, b := newLagRig(t), newLagSeg(10000), newLagSeg(30000)
		r.adopt(a, 10000)
		for i, e := range []int64{100000, 150000, 100000, 160000} {
			ms := 100 + 50*i
			r.want(ms, int32(30000+ms), b.next(e, lagD), int32(30000+ms-314), 0)
		}
		r.want(300, 30300, b.next(160000, lagD), 30300-160, 154)
	})
	t.Run("re-enabled on a song change, each disable noted", func(t *testing.T) {
		r, a, c := newLagRig(t), newLagSeg(10000), newLagSeg(0)
		r.adopt(a, 10000)
		notes := 0
		for ms := 100; ms <= 200; ms += 50 {
			if _, n := r.step(ms, int32(10000+ms), a.next(350000, lagD)); n.Kind == LagNoteDrift {
				notes++
			}
		}
		r.rid = lagTestRID2
		r.want(250, 1000, c.next(314000, lagD), 1000, 0)
		r.want(300, 1050, c.next(314000, lagD), 1050-314, -314)
		for ms := 350; ms <= 450; ms += 50 {
			if _, n := r.step(ms, int32(ms+750), c.next(350000, lagD)); n.Kind == LagNoteDrift {
				if n != drift(c) {
					t.Fatalf("note %+v, want %+v", n, drift(c))
				}
				notes++
			}
		}
		r.want(500, 1250, c.next(314000, lagD), 1250, 0)
		if notes != 2 {
			t.Fatalf("%d drift notes, want 2", notes)
		}
	})
}

// Round 2 review: the lag source never yields a valid sample; the output is
// the core, jump included, and the shift is always 0.
func TestLagCorrectorAlwaysInvalid(t *testing.T) {
	r := newLagRig(t)
	for ms := 0; ms <= 2000; ms += 50 {
		core := int32(10000)
		if ms > 0 {
			core = int32(30000 + ms - 50)
		}
		if n := r.want(ms, core, noLag, core, 0); n.Kind != LagNoteNone {
			t.Fatalf("t=%d ms noted %+v", ms, n)
		}
	}
}

// The output stays within the duration; the shift and LagAppliedMs follow the
// deduction actually applied. A positive e_cur never presses the output to 0:
// see TestLagCorrectorMismatch.
func TestLagCorrectorClamp(t *testing.T) {
	r, a := newLagRig(t), newLagSeg(239000)
	r.want(0, lagTestDur-200, a.next(-150000, lagD), lagTestDur-200, 0)
	r.want(50, lagTestDur-100, a.next(-150000, lagD), lagTestDur, 100)
	r.want(100, lagTestDur-50, a.next(-150000, lagD), lagTestDur, -50)
	r.want(150, lagTestDur, a.next(-150000, lagD), lagTestDur, -50)
}

// Rule 5 (mismatch): a core position below e_cur, in ms, belongs to another
// segment. That tick goes uncorrected; the segment keeps its confirmed value
// and eligibility.
//
// Mutation (verified): dropping the rule, comparing with <=, comparing in µs,
// or excluding the current segment as well turns it red.
func TestLagCorrectorMismatch(t *testing.T) {
	// Single-track repeat and seeks to 0:00: e_cur 314 meets the core jumping
	// from 180000 back to 0, and the new segment's first valid sample (e = 0)
	// is 1, 3, 4 or 8 polls late. The output follows the core from the jump
	// on and is never held at 0.
	for _, late := range []int{1, 3, 4, 8} {
		t.Run(fmt.Sprintf("old segment meets a jump to 0, lag %d polls late", late), func(t *testing.T) {
			r, a, z := newLagRig(t), newLagSeg(179000), newLagSeg(0)
			r.adopt(a, 179900)
			r.want(100, 180000, a.next(314000, lagD), 180000-314, 0)
			for i := 0; i <= 12; i++ {
				lag, shift := noLag, int32(0)
				if i >= late {
					lag = z.next(0, lagD)
				}
				if i == 0 {
					shift = 314
				}
				r.want(150+50*i, int32(50*i), lag, int32(50*i), shift)
			}
		})
	}
	// Equal is no mismatch; the comparison is in ms after the output's
	// rounding (314.4 ms deducts 314).
	t.Run("boundary", func(t *testing.T) {
		for _, c := range []struct {
			eUs        int64
			core, want int32
		}{{314000, 314, 0}, {314000, 313, 313}, {314400, 314, 0}, {314400, 313, 313}} {
			r, a := newLagRig(t), newLagSeg(10000)
			r.want(0, 10000, a.next(c.eUs, lagD), 10000, 0)
			r.want(50, 10050, a.next(c.eUs, lagD), 10050-314, -314)
			r.want(100, c.core, noLag, c.want, 314-(c.core-c.want))
		}
	})
	// Review round 11: the new segment is confirmed while the core still reports
	// 100 ms. Those ticks go uncorrected; from the jump on the segment's 314 ms
	// applies and stays.
	t.Run("new segment early", func(t *testing.T) {
		r, b := newLagRig(t), newLagSeg(30000)
		r.want(0, 100, noLag, 100, 0)
		r.want(50, 100, b.next(314000, lagD), 100, 0)
		r.want(100, 100, b.next(314000, lagD), 100, 0)
		r.want(150, 30000, b.next(314000, lagD), 30000-314, -314)
		for ms := 200; ms <= 1000; ms += 50 {
			core := int32(30000 + ms - 150)
			r.want(ms, core, b.next(314000, lagD), core-314, 0)
		}
	})
}

// LagAdopted marks the first tick on which a segment's confirmed value is the
// e_cur in effect (design §3.3 "通知"): once per segment, the opening segment and
// the session's first one included, whether or not the deduction changes; not
// on a raise, not on a tick on which the mismatch rule zeroes it, never for a
// segment that may not be adopted.
//
// Mutation (verified): setting it on a raise turns "first confirmation, not a
// raise" red; on the tick the mismatch rule zeroes, "zeroed by the mismatch
// rule"; dropping the once-per-segment record, both; requiring a non-zero
// deduction or a changed one, "opening segment" and "same value as the previous
// segment"; dropping the eligibility check, "may not be adopted".
func TestLagCorrectorAdoptedFlag(t *testing.T) {
	type tick struct {
		ms   int
		core int32
		lag  LagSample
	}
	run := func(t *testing.T, r *lagRig, ticks []tick, want ...bool) {
		t.Helper()
		var got []bool
		for _, k := range ticks {
			out, _ := r.step(k.ms, k.core, k.lag)
			got = append(got, out.LagAdopted)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("LagAdopted %v, want %v", got, want)
		}
	}
	t.Run("first confirmation, not a raise", func(t *testing.T) {
		r, a := newLagRig(t), newLagSeg(10000)
		var ticks []tick
		for i, e := range []int64{290800, 290800, 314000, 314000, 314000} {
			ticks = append(ticks, tick{50 * i, 10000 + 50*int32(i), a.next(e, lagD)})
		}
		run(t, r, ticks, false, true, false, false, false)
		if r.c.applied != 314 {
			t.Fatalf("not raised: deduction %d", r.c.applied)
		}
	})
	// The new segment confirms the value already in effect: the deduction and
	// the output do not change, the flag alone tells the session.
	t.Run("same value as the previous segment", func(t *testing.T) {
		r, a, b := newLagRig(t), newLagSeg(10000), newLagSeg(10000)
		b.key.Base = 10200
		run(t, r, []tick{
			{0, 10000, a.next(314000, lagD)}, {50, 10050, a.next(314000, lagD)},
			{100, 10300, b.next(314000, lagD)},
		}, false, true, false)
		out, _ := r.step(150, 10350, b.next(314000, lagD))
		if !out.LagAdopted || out.LagShiftMs != 0 || out.PositionMs != 10350-314 {
			t.Fatalf("confirmation of the same value: %+v", out)
		}
		run(t, r, []tick{{200, 10400, b.next(314000, lagD)}}, false)
	})
	// Review round 11: confirmed while the core still reports 100 ms, adopted
	// on the jump tick.
	t.Run("zeroed by the mismatch rule", func(t *testing.T) {
		r, b := newLagRig(t), newLagSeg(30000)
		run(t, r, []tick{
			{0, 100, noLag}, {50, 100, b.next(314000, lagD)}, {100, 100, b.next(314000, lagD)},
			{150, 30000, b.next(314000, lagD)}, {200, 30050, b.next(314000, lagD)},
		}, false, false, false, true, false)
	})
	// The opening segment's value rounds to no deduction: still adopted.
	t.Run("opening segment", func(t *testing.T) {
		r, s0 := newLagRig(t), newLagSeg(0)
		run(t, r, []tick{{0, 0, s0.next(2, lagD)}, {50, 50, s0.next(2, lagD)}, {100, 100, s0.next(2, lagD)}}, false, true, false)
	})
	// A seek to the same whole second is a new segment, adopted again.
	t.Run("new segment under the same key", func(t *testing.T) {
		r, a := newLagRig(t), newLagSeg(30000)
		b := &lagSeg{key: a.key}
		run(t, r, []tick{
			{0, 30000, a.next(314000, lagD)}, {50, 30050, a.next(314000, lagD)},
			{100, 30000, b.next(200000, lagD)}, {150, 30050, b.next(200000, lagD)},
		}, false, true, false, true)
	})
	// Unconfirmed for 500 ms (e_cur 0), then confirmed: adopted on the
	// confirmation.
	t.Run("confirmed after the unconfirmed timeout", func(t *testing.T) {
		r, b := newLagRig(t), newLagSeg(30000)
		var ticks []tick
		for i := range 12 {
			e := int64(100000 + 50000*(i%2))
			if i >= 10 {
				e = 200000
			}
			ticks = append(ticks, tick{50 * i, 30000 + 50*int32(i), b.next(e, lagD)})
		}
		want := make([]bool, 12)
		want[11] = true
		run(t, r, ticks, want...)
	})
	t.Run("may not be adopted", func(t *testing.T) {
		// The next song's opening segment, first seen before the song change:
		// its value (0) equals the e_cur in effect after the change, yet it is
		// never adopted.
		r, a, b, c := newLagRig(t), newLagSeg(50000), newLagSeg(0), newLagSeg(30000)
		run(t, r, []tick{{0, 50000, a.next(314000, lagD)}, {50, 50050, a.next(314000, lagD)}, {100, 50100, b.next(0, lagD)}}, false, true, false)
		r.rid = lagTestRID2
		run(t, r, []tick{{150, 1000, b.next(0, lagD)}, {200, 1050, b.next(0, lagD)}, {250, 30000, c.next(314000, lagD)}, {300, 30050, c.next(314000, lagD)}},
			false, false, false, true)
		// Failed, then the same segment recovers.
		r, a = newLagRig(t), newLagSeg(59000)
		ticks := []tick{{0, 59800, a.next(314000, lagD)}, {50, 59850, a.next(314000, lagD)}}
		for ms := 100; ms <= 600; ms += 50 {
			ticks = append(ticks, tick{ms, int32(59800 + ms), noLag})
		}
		ticks = append(ticks, tick{650, 60450, a.next(314000, lagD)}, tick{700, 60500, a.next(314000, lagD)})
		want := make([]bool, len(ticks))
		want[1] = true
		run(t, r, ticks, want...)
		// Disabled by drift for the song.
		r, a, b = newLagRig(t), newLagSeg(10000), newLagSeg(20000)
		run(t, r, []tick{
			{0, 10000, a.next(314000, lagD)}, {50, 10050, a.next(314000, lagD)},
			{100, 10100, a.next(350000, lagD)}, {150, 10150, a.next(350000, lagD)}, {200, 10200, a.next(350000, lagD)},
			{250, 20000, b.next(314000, lagD)}, {300, 20050, b.next(314000, lagD)},
		}, false, true, false, false, false, false, false)
	})
}

// A confirmed value or a raise is noted when it deducts a non-zero amount.
func TestLagCorrectorAdoptedNote(t *testing.T) {
	r, s0, a := newLagRig(t), newLagSeg(0), newLagSeg(30000)
	steps := []struct {
		seg  *lagSeg
		e    int64
		note LagNote
	}{
		{s0, 2, LagNote{}},
		{s0, 2, LagNote{}}, // 2 µs rounds to no deduction
		{a, 290800, LagNote{}},
		{a, 290800, LagNote{Kind: LagNoteAdopted, EUs: 290800, Key: a.key}},
		{a, 314000, LagNote{}},
		{a, 314000, LagNote{Kind: LagNoteAdopted, EUs: 314000, Key: a.key}},
		{a, 314000, LagNote{}},
	}
	for i, s := range steps {
		ms := 50 * i
		if _, n := r.step(ms, int32(10000+ms), s.seg.next(s.e, lagD)); n != s.note {
			t.Fatalf("step %d: note %+v, want %+v", i, n, s.note)
		}
	}
}
