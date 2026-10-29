package kuwo

// This file tests only the seek-lag correction at the event level (issue #131,
// doc/design/kuwo-seek-lag-design.md §3.3-§3.5 and the §4 event counts): the
// corrected position source driven poll by poll through session.observe and
// session.emitLine, in runSession's order, and through runSession for opening
// and failing lag readers. The corrector's own arithmetic is tested in package
// pos (lagcorrector_test.go); the session's notice with synthetic samples in
// lagnotify_test.go.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/lyric"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

const (
	lagDur = 240000 // ms
	lagD   = 23200  // packet duration, µs
)

var (
	lagT0      = time.Unix(1000, 0)
	noLag      pos.LagSample // an invalid sample
	errLagRead = errors.New("lag read failed")
	errLagOpen = errors.New("in_ffaudio.dll not loaded")
	// lagGarbage is what a failed read might leave in its sample: valid-looking,
	// so a source that used it despite the error would adopt 1 s.
	lagGarbage = pos.LagSample{Valid: true, Key: pos.LagKey{Ctx: 0x0dead000, Ring: 0x0dead100}, Written: 4096, EUs: 1000000, DUs: lagD}
)

// stubCore returns the sample set for the poll, or err.
type stubCore struct {
	sample pos.Sample
	err    error
	closes int
}

func (c *stubCore) Read(string, bool) (pos.Sample, error) { return c.sample, c.err }
func (c *stubCore) Close()                                { c.closes++ }

// stubLag returns the lag sample set for the poll, or err. Its output state is
// always invalid, so the transient filter passes the core position through and
// behaviour equals #131 alone (what these tests cover).
type stubLag struct {
	sample        pos.LagSample
	out           pos.OutputState
	err           error
	reads, closes int
}

func (l *stubLag) Read() (pos.LagSample, pos.OutputState, error) {
	l.reads++
	return l.sample, l.out, l.err
}
func (l *stubLag) Close() { l.closes++ }

// feedSeg yields the valid lag samples of one output segment.
type feedSeg struct {
	key     pos.LagKey
	written uint32
}

func newFeedSeg(base uint32) *feedSeg {
	return &feedSeg{key: pos.LagKey{Ctx: 0x05a32e40, Ring: 0x06120a00, Base: base}}
}

// next is a sample taken after more output was written.
func (s *feedSeg) next(eUs int64) pos.LagSample { return s.nextD(eUs, lagD) }

func (s *feedSeg) nextD(eUs, dUs int64) pos.LagSample {
	s.written += 4096
	return pos.LagSample{Valid: true, Key: s.key, Written: s.written, EUs: eUs, DUs: dUs}
}

// held is a sample with nothing written since the previous one (paused).
func (s *feedSeg) held(eUs int64) pos.LagSample {
	return pos.LagSample{Valid: true, Key: s.key, Written: s.written, EUs: eUs, DUs: lagD}
}

// lagRig drives a corrected source and a session one poll at a time, in
// runSession's order: read, observe, emitLine. Times are ms after lagT0.
type lagRig struct {
	t      *testing.T
	p      *KuwoPlayer
	s      *session
	core   stubCore
	lag    stubLag
	warned bool
	src    positionSource
	rid    string
	status int // 1 playing, 2 paused
	dur    int32
	events []player.Event // everything emitted so far
	out    pos.Sample     // the latest output
	at     time.Time      // the latest poll's time
	fe     frontEnd       // the front end, fed every event emitted so far
	next   []int          // lyrics the next poll delivers
}

func newLagRig(t *testing.T) *lagRig {
	r := &lagRig{t: t, p: New(0, 50), rid: "MUSIC_1", status: 1, dur: lagDur}
	r.s = newSession(r.p)
	r.src = newCorrectedSource(&r.core, &r.lag, &r.warned, r.p.pollMs)
	return r
}

// poll runs one poll with the core at core and the lag source answering lag
// and lagErr; it returns the poll's events.
func (r *lagRig) poll(ms int, core int32, lag pos.LagSample, lagErr error) []player.Event {
	r.t.Helper()
	at := lagT0.Add(time.Duration(ms) * time.Millisecond)
	// CoreMs mirrors PositionMs: the lag source reports an invalid output state so
	// the transient filter passes the position through; the raw core equals it.
	r.core.sample = pos.Sample{RID: r.rid, PositionMs: core, CoreMs: core, DurationMs: r.dur, ObservedAt: at}
	r.lag.sample, r.lag.err = lag, lagErr
	out, err := r.src.Read(r.rid, r.status == 1)
	if err != nil {
		r.t.Fatalf("t=%d ms: Read returned %v", ms, err)
	}
	r.out, r.at = out, at
	r.s.observe(cdp.Snapshot{RID: r.rid, Title: "song " + r.rid, Artist: "artist", Status: r.status}, out)
	if r.next != nil {
		r.s.accept(assetResult{gen: r.s.gen, rid: r.s.rid, lines: lyricLines(r.next)}, at)
		r.next = nil
	}
	r.s.emitLine(at)
	got := drainEvents(r.p)
	r.see(got, at)
	return got
}

func (r *lagRig) see(events []player.Event, at time.Time) {
	for _, e := range events {
		r.fe.see(e, at)
	}
	r.events = append(r.events, events...)
}

// frontMs is the front end's position at the latest poll, in ms.
func (r *lagRig) frontMs() int32 { return int32(math.Round(float64(r.fe.now(r.at)) * 1000)) }

func (r *lagRig) step(ms int, core int32, lag pos.LagSample) []player.Event {
	r.t.Helper()
	return r.poll(ms, core, lag, nil)
}

// fail is a poll whose lag read fails, leaving a valid-looking sample.
func (r *lagRig) fail(ms int, core int32) []player.Event {
	r.t.Helper()
	return r.poll(ms, core, lagGarbage, errLagRead)
}

func lyricLines(lineMs []int) []lyric.Line {
	var lines []lyric.Line
	for i, l := range lineMs {
		lines = append(lines, lyric.Line{TimeMs: l, Text: fmt.Sprintf("line %d", i)})
	}
	return lines
}

// lyrics delivers the song's lyrics, one line at each of lineMs, after the poll
// at ms.
func (r *lagRig) lyrics(ms int, lineMs ...int) {
	at := lagT0.Add(time.Duration(ms) * time.Millisecond)
	r.s.accept(assetResult{gen: r.s.gen, rid: r.s.rid, lines: lyricLines(lineMs)}, at)
	r.see(drainEvents(r.p), at)
}

// lyricsNext has the next poll deliver the lyrics between observe and
// emitLine, where runSession drains the asset worker's results.
func (r *lagRig) lyricsNext(lineMs ...int) { r.next = lineMs }

func (r *lagRig) mark() int { return len(r.events) }

// wantResumes checks the positions, in ms, of the playback_resume events since
// mark from.
func (r *lagRig) wantResumes(from int, ms ...int32) {
	r.t.Helper()
	if got := eventsMs(ofType(r.events[from:], player.EventPlaybackResume)); !slices.Equal(got, ms) {
		r.t.Fatalf("playback_resume at %v ms, want %v", got, ms)
	}
}

// eventMs is a positioned event's position in ms; -1 for other events.
func eventMs(e player.Event) int32 {
	var p float32
	switch v := e.Data.(type) {
	case *player.PlaybackTimeInfo:
		p = v.Position
	case *player.LyricUpdate:
		p = v.Position
	case *player.AllLyricsData:
		p = v.Position
	default:
		return -1
	}
	return int32(math.Round(float64(p) * 1000))
}

func eventsMs(events []player.Event) []int32 {
	var out []int32
	for _, e := range events {
		out = append(out, eventMs(e))
	}
	return out
}

// The §4 sequences that state their event counts. The kernel advances 50 ms per
// 50 ms poll unless a case says otherwise; e = 314 ms.
//
// Mutation (verified): returning the core sample without Apply turns every case
// red except the song change, whose output is the kernel's either way.
func TestLagEventCounts(t *testing.T) {
	t.Run("confirmed on the jump tick", func(t *testing.T) {
		r, b := newLagRig(t), newFeedSeg(30000)
		r.step(0, 10000, noLag)
		r.step(50, 10050, b.next(314000))
		r.step(100, 30000, b.next(314000))
		r.step(150, 30050, b.next(314000))
		r.wantResumes(0, 29686)
	})
	// Review round 2: the kernel holds 10000 for two polls while the new
	// segment is confirmed, then jumps.
	t.Run("confirmed two polls before the jump", func(t *testing.T) {
		r, b := newLagRig(t), newFeedSeg(30000)
		r.step(0, 10000, noLag)
		r.step(50, 10000, b.next(314000))
		r.step(100, 10000, b.next(314000))
		r.step(150, 30000, b.next(314000))
		r.step(200, 30050, b.next(314000))
		r.wantResumes(0, 9686, 29686)
	})
	// Review round 1: the jump comes first, the lag two polls later.
	t.Run("confirmed after the jump", func(t *testing.T) {
		r, b := newLagRig(t), newFeedSeg(30000)
		r.step(0, 10000, noLag)
		r.step(50, 30000, noLag)
		r.step(100, 30050, b.next(314000))
		r.step(150, 30100, b.next(314000))
		r.step(200, 30150, b.next(314000))
		r.wantResumes(0, 30000, 29786)
		if r.out.PositionMs != 30150-314 {
			t.Fatalf("output %d after the confirmation, want %d", r.out.PositionMs, 30150-314)
		}
	})
	// Review round 4: a 150 ms forward seek is no jump; the confirmation moves
	// the output back.
	t.Run("small forward seek", func(t *testing.T) {
		r, b := newLagRig(t), newFeedSeg(6000)
		r.step(0, 5850, noLag)
		r.step(50, 6000, b.next(314000))
		r.step(100, 6050, b.next(314000))
		r.wantResumes(0, 5736)
	})
	// Review round 3: after the correction failed, a seek is published once on
	// the jump and once on the confirmation.
	t.Run("seek after a failure", func(t *testing.T) {
		r, a, b := newLagRig(t), newFeedSeg(9000), newFeedSeg(30000)
		r.step(0, 9000, a.next(314000))
		r.step(50, 9050, a.next(314000))
		for ms := 100; ms <= 1000; ms += 50 {
			r.step(ms, int32(9000+ms), noLag)
		}
		mark := r.mark()
		r.step(1050, 30000, noLag)
		r.step(1100, 30050, b.next(314000))
		r.step(1150, 30100, b.next(314000))
		r.wantResumes(0, 8736, 9550, 30000, 29786)
		r.wantResumes(mark, 30000, 29786)
	})
	// Review round 1: after a 150 ms seek every read fails; 500 ms later the
	// output steps back to the kernel. The same segment recovering is not
	// adopted again, a new segment is.
	t.Run("failure", func(t *testing.T) {
		r, a, b := newLagRig(t), newFeedSeg(59000), newFeedSeg(61000)
		r.step(0, 59800, a.next(314000))
		r.step(50, 59850, a.next(314000))
		for ms := 100; ms <= 950; ms += 50 {
			core := int32(60000 + ms - 100)
			r.fail(ms, core)
			want := core - 314
			if ms >= 550 {
				want = core
			}
			if r.out.PositionMs != want {
				t.Fatalf("t=%d ms: output %d, want %d", ms, r.out.PositionMs, want)
			}
		}
		r.step(1000, 60900, a.next(314000))
		r.step(1050, 60950, a.next(314000))
		if r.out.PositionMs != 60950 {
			t.Fatalf("recovered failed segment adopted: output %d", r.out.PositionMs)
		}
		r.step(1100, 61000, b.next(314000))
		r.step(1150, 61050, b.next(314000))
		r.wantResumes(0, 59536, 60450, 60736)
	})
	// Review round 4: paused at 10000, every sample invalid for 10 s. The
	// correction drops at 500 ms: resume, then pause, at the kernel position.
	t.Run("failure while paused", func(t *testing.T) {
		r, a := newLagRig(t), newFeedSeg(10000)
		r.status = 2
		r.step(0, 10000, a.next(314000))
		r.step(50, 10000, a.held(314000))
		mark := r.mark()
		for ms := 100; ms <= 10050; ms += 50 {
			r.step(ms, 10000, noLag)
		}
		var got []string
		for _, e := range r.events[mark:] {
			if e.Type == player.EventPlaybackResume || e.Type == player.EventPlaybackPause {
				got = append(got, fmt.Sprintf("%s@%d", e.Type, eventMs(e)))
			}
		}
		want := []string{player.EventPlaybackResume + "@10000", player.EventPlaybackPause + "@10000"}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("song change", func(t *testing.T) {
		r, a, b := newLagRig(t), newFeedSeg(50000), newFeedSeg(0)
		r.step(0, 50000, a.next(314000))
		r.step(50, 50050, a.next(314000))
		mark := r.mark()
		// The decoder opens the next song before the core reports it.
		r.step(100, 50100, b.next(20000))
		r.rid = "MUSIC_2"
		for ms := 150; ms <= 700; ms += 50 {
			core := int32(1000 + ms - 150)
			r.step(ms, core, b.next(20000))
			if r.out.PositionMs != core {
				t.Fatalf("t=%d ms: output %d, want the kernel %d", ms, r.out.PositionMs, core)
			}
		}
		r.wantResumes(mark)
	})
	// Review round 1: a seek done during the change is first seen after it.
	t.Run("seek during the song change", func(t *testing.T) {
		r, a, c := newLagRig(t), newFeedSeg(50000), newFeedSeg(30000)
		r.step(0, 50000, a.next(314000))
		r.step(50, 50050, a.next(314000))
		mark := r.mark()
		r.rid = "MUSIC_2"
		r.step(100, 30000, c.next(314000))
		r.step(150, 30050, c.next(314000))
		r.wantResumes(mark, 29736)
	})
	// Review round 11, mismatch rule: the new segment is confirmed while the
	// kernel still reports 100 ms. That poll goes uncorrected; from the jump on
	// the segment's 314 ms applies and stays. One resume.
	//
	// Mutation (verified): dropping the rule (a second resume at 0) or
	// excluding the segment as well (no correction after the jump) turns it red.
	t.Run("new segment early", func(t *testing.T) {
		r, b := newLagRig(t), newFeedSeg(30000)
		r.step(0, 100, noLag)
		r.step(50, 100, b.next(314000))
		r.step(100, 100, b.next(314000))
		if r.out.PositionMs != 100 {
			t.Fatalf("t=100 ms: output %d, want the kernel 100", r.out.PositionMs)
		}
		for ms := 150; ms <= 500; ms += 50 {
			core := int32(30000 + ms - 150)
			r.step(ms, core, b.next(314000))
			if r.out.PositionMs != core-314 {
				t.Fatalf("t=%d ms: output %d, want %d", ms, r.out.PositionMs, core-314)
			}
		}
		r.wantResumes(0, 29686)
	})
	t.Run("session's first sample", func(t *testing.T) {
		r, a := newLagRig(t), newFeedSeg(10000)
		r.step(0, 10000, a.next(314000))
		if r.out.PositionMs != 10000 {
			t.Fatalf("first sample corrected: %d", r.out.PositionMs)
		}
		r.step(50, 10050, a.next(314000))
		r.wantResumes(0, 9736)
	})
	// A drop of 220 ms moves the output 270 ms in 50 ms, no jump; the front end
	// is 220 ms behind and the notice publishes it. 199 ms does not reach the
	// threshold.
	t.Run("notice threshold", func(t *testing.T) {
		for _, c := range []struct {
			eUs  int64
			want []int32
		}{{220000, []int32{10550}}, {199000, nil}} {
			r, a := newLagRig(t), newFeedSeg(10000)
			r.step(0, 10000, a.next(c.eUs))
			r.step(50, 10050, a.next(c.eUs))
			mark := r.mark()
			for ms := 100; ms <= 700; ms += 50 {
				r.step(ms, int32(10000+ms), noLag)
			}
			r.wantResumes(mark, c.want...)
		}
	})
	// Review round 6: two decreases, 250 → 100 (+150) and 100 → 0 on failure
	// (+100). The line change on the first anchors the front end; when the
	// channel is full and drops that lyric_update, the second decrease leaves
	// the public position 250 ms ahead of the front end and publishes.
	t.Run("event channel full", func(t *testing.T) {
		for _, full := range []bool{true, false} {
			r, a, b := newLagRig(t), newFeedSeg(10000), newFeedSeg(20000)
			r.step(0, 10000, a.next(250000))
			r.lyrics(0, 1000, 10000, 20000)
			r.step(50, 10050, a.next(250000))
			mark := r.mark()
			r.step(100, 10100, b.next(100000))
			if full {
				fillEvents(r.p)
			}
			lu := ofType(r.step(150, 10150, b.next(100000)), player.EventLyricUpdate)
			if full && len(lu) != 0 || !full && (len(lu) != 1 || eventMs(lu[0]) != 10050) {
				t.Fatalf("full %v: lyric_update on the first decrease %+v", full, lu)
			}
			for ms := 200; ms <= 800; ms += 50 {
				r.step(ms, int32(10000+ms), noLag)
			}
			if full {
				r.wantResumes(mark, 10650)
			} else {
				r.wantResumes(mark)
			}
		}
	})
	// Review round 5: 250 ms polls, e_cur 264, then 114, 114, 314, 314 with
	// D = 200. The +150 is anchored by a lyric_update on its tick; the raise
	// moves the output only 50 ms in 250 ms, so the front end is 200 ms ahead.
	t.Run("opposite change after an anchor", func(t *testing.T) {
		r, a, b := newLagRig(t), newFeedSeg(9000), newFeedSeg(10000)
		r.step(0, 10000, a.next(264000))
		r.lyrics(0, 5000, 10600, 20000)
		r.step(250, 10250, a.next(264000))
		r.step(500, 10500, b.nextD(114000, 200000))
		ev := r.step(750, 10750, b.nextD(114000, 200000))
		if lu := ofType(ev, player.EventLyricUpdate); len(lu) != 1 || eventMs(lu[0]) != 10636 || len(ofType(ev, player.EventPlaybackResume)) != 0 {
			t.Fatalf("t=750 ms: want one lyric_update at 10636 and no resume, got %+v", ev)
		}
		r.step(1000, 11000, b.nextD(314000, 200000))
		r.step(1250, 11250, b.nextD(314000, 200000))
		r.wantResumes(0, 9986, 10936)
	})
}

// Round 2 review, as corrected in 9090022: confirmed 290.8, raised to 314
// (23 ms). On the raise tick the output moves back, and a resume is due, only
// when the kernel advances less than 23 ms; at 50 ms per poll the front end is
// 23 ms ahead and nothing is published.
func TestLagRaiseAfterConfirm(t *testing.T) {
	for _, c := range []struct {
		adv  int32
		want int
	}{{0, 2}, {20, 2}, {50, 1}} {
		r, a := newLagRig(t), newFeedSeg(10000)
		for i, e := range []int64{290800, 290800, 314000, 314000, 314000} {
			r.step(50*i, 10000+c.adv*int32(i), a.next(e))
			if i == 3 && c.adv == 50 && (r.out.LagShiftMs != -23 || r.frontMs()-r.out.PositionMs != 23) {
				t.Fatalf("advance %d: on the raise shift %d, front end %d ahead; want -23 and 23", c.adv, r.out.LagShiftMs, r.frontMs()-r.out.PositionMs)
			}
		}
		if got := len(ofType(r.events, player.EventPlaybackResume)); got != c.want {
			t.Fatalf("advance %d ms per poll: %d resumes, want %d", c.adv, got, c.want)
		}
	}
}

// Silent seeks (design §4 "无声拖动", final review): the kernel moves ahead by no
// more than elapsed + 250 ms, which CoreClock does not report, and the new
// segment's first adoption is the tick on which the front end is told. Each
// case plays at 50 ms polls with the old segment's e = 314 ms adopted.
//
// Mutation (verified): dropping the LagAdopted trigger turns "same value" red;
// dropped events clearing the notice, "notice dropped".
func TestLagSilentSeek(t *testing.T) {
	// start plays to t=100 ms, the kernel at 59800 and the front end at the
	// public position 59486, and returns the segment of a seek to 60 s.
	start := func(t *testing.T, lineMs ...int) (*lagRig, *feedSeg) {
		t.Helper()
		r, a := newLagRig(t), newFeedSeg(59000)
		r.step(0, 59700, a.next(314000))
		if lineMs != nil {
			r.lyrics(0, lineMs...)
		}
		r.step(50, 59750, a.next(314000))
		r.step(100, 59800, a.next(314000))
		if r.out.PositionMs != 59486 || r.frontMs() != 59486 {
			t.Fatalf("setup: output %d, front end %d; want 59486", r.out.PositionMs, r.frontMs())
		}
		return r, newFeedSeg(60000)
	}
	// The kernel moves 200 ms in 50 ms into a segment of e = 200, confirmed on
	// the next poll: +114 ms of correction, but the public position 59850 is
	// 264 ms ahead of the front end (59586).
	t.Run("final review", func(t *testing.T) {
		r, b := start(t)
		mark := r.mark()
		r.step(150, 60000, b.next(200000))
		r.step(200, 60050, b.next(200000))
		if r.out.LagShiftMs != 114 || !r.out.LagAdopted {
			t.Fatalf("t=200 ms: shift %d, adopted %v; want 114, true", r.out.LagShiftMs, r.out.LagAdopted)
		}
		r.step(250, 60100, b.next(200000))
		r.wantResumes(mark, 59850)
	})
	// Review round 14: a lyric_update anchors the front end at 59436 just
	// before the seek; the confirmation leaves it 314 ms behind 59850.
	t.Run("anchored before the seek", func(t *testing.T) {
		r, a := newLagRig(t), newFeedSeg(59000)
		r.step(0, 59600, a.next(314000))
		r.lyrics(0, 1000, 59400, 120000)
		r.step(50, 59650, a.next(314000))
		r.step(100, 59700, a.next(314000))
		if lu := ofType(r.step(150, 59750, noLag), player.EventLyricUpdate); len(lu) != 1 || eventMs(lu[0]) != 59436 {
			t.Fatalf("t=150 ms: want a lyric_update at 59436, got %+v", lu)
		}
		mark := r.mark()
		b := newFeedSeg(60000)
		r.step(200, 60000, b.next(200000))
		r.step(250, 60050, b.next(200000))
		r.step(300, 60100, b.next(200000))
		r.wantResumes(mark, 59850)
	})
	// The new segment confirms the old value: no correction change, the first
	// adoption alone is judged. 260 ms in 50 ms leaves the front end 210 ms
	// behind; 200 ms, 150 ms.
	t.Run("same value", func(t *testing.T) {
		for _, c := range []struct {
			adv  int32
			want []int32
		}{{260, []int32{59796}}, {200, nil}} {
			r, b := start(t)
			mark := r.mark()
			r.step(150, 59800+c.adv, b.next(314000))
			r.step(200, 59850+c.adv, b.next(314000))
			if r.out.LagShiftMs != 0 || !r.out.LagAdopted {
				t.Fatalf("advance %d: shift %d, adopted %v; want 0, true", c.adv, r.out.LagShiftMs, r.out.LagAdopted)
			}
			r.step(250, 59900+c.adv, b.next(314000))
			r.wantResumes(mark, c.want...)
		}
	})
	// A line change on the seek tick anchors the front end at 59686: 114 ms
	// behind on the confirmation, nothing to publish.
	t.Run("lyric_update on the seek tick", func(t *testing.T) {
		r, b := start(t, 1000, 59600, 120000)
		mark := r.mark()
		if lu := ofType(r.step(150, 60000, b.next(200000)), player.EventLyricUpdate); len(lu) != 1 || eventMs(lu[0]) != 59686 {
			t.Fatalf("t=150 ms: want a lyric_update at 59686, got %+v", lu)
		}
		r.step(200, 60050, b.next(200000))
		r.step(250, 60100, b.next(200000))
		r.wantResumes(mark)
	})
	// Review round 13: the channel is full on the confirmation, so the resume
	// and the lyric_update after it are dropped. The next poll publishes at
	// 59900, the one after that nothing.
	t.Run("notice dropped", func(t *testing.T) {
		r, b := start(t)
		mark := r.mark()
		r.step(150, 60000, b.next(200000))
		fillEvents(r.p)
		r.step(200, 60050, b.next(200000))
		r.wantResumes(mark)
		r.step(250, 60100, b.next(200000))
		r.wantResumes(mark, 59900)
		r.step(300, 60150, b.next(200000))
		r.wantResumes(mark, 59900)
	})
}

// A first adoption or a correction change compares the public position with
// where the front end is, not with what the correction did, so a silent advance
// the front end already follows publishes nothing (design §4 "无声前进不误发").
// The single-track repeat with the lag late is TestLagLoopToStart, the new
// segment confirmed early and zeroed by the mismatch rule TestLagEventCounts'
// "new segment early".
//
// Mutation (verified): judging on every tick turns "DirectSound stall" red.
func TestLagSilentAdvanceNoFalseNotice(t *testing.T) {
	// Review round 13: the opening segment's first valid sample comes a poll
	// late; on its adoption the front end is where the public position is.
	t.Run("opening segment late", func(t *testing.T) {
		r, z := newLagRig(t), newFeedSeg(0)
		r.step(0, 0, noLag)
		r.step(50, 50, z.next(0))
		r.step(100, 100, z.next(0))
		if !r.out.LagAdopted {
			t.Fatal("t=100 ms: the opening segment was not adopted")
		}
		for ms := 150; ms <= 500; ms += 50 {
			r.step(ms, int32(ms), z.next(0))
		}
		r.wantResumes(0)
	})
	t.Run("opening segment late after a song change", func(t *testing.T) {
		r, a, z := newLagRig(t), newFeedSeg(50000), newFeedSeg(0)
		r.step(0, 50000, a.next(314000))
		r.step(50, 50050, a.next(314000))
		mark := r.mark()
		r.rid = "MUSIC_2"
		r.step(100, 1000, noLag)
		r.step(150, 1050, noLag)
		r.step(200, 1100, z.next(0))
		r.step(250, 1150, z.next(0))
		if !r.out.LagAdopted {
			t.Fatal("t=250 ms: the opening segment was not adopted")
		}
		r.step(300, 1200, z.next(0))
		r.wantResumes(mark)
	})
	// A large seek published on the jump with the old value; the new segment
	// confirms the same value later.
	t.Run("large seek, lag late, same value", func(t *testing.T) {
		r, a, b := newLagRig(t), newFeedSeg(10000), newFeedSeg(30000)
		r.step(0, 10000, a.next(314000))
		r.step(50, 10050, a.next(314000))
		mark := r.mark()
		r.step(100, 30000, noLag)
		r.step(150, 30050, b.next(314000))
		r.step(200, 30100, b.next(314000))
		if !r.out.LagAdopted || r.out.LagShiftMs != 0 {
			t.Fatalf("t=200 ms: adopted %v, shift %d; want true, 0", r.out.LagAdopted, r.out.LagShiftMs)
		}
		r.step(250, 30150, b.next(314000))
		r.wantResumes(mark, 29686)
	})
	// DirectSound after a seek: T, T+313, T+600, then still for 0.9 s while the
	// front end runs on. The correction and the segment do not change, so the
	// stall is not judged (non-goal).
	t.Run("DirectSound stall", func(t *testing.T) {
		r, a := newLagRig(t), newFeedSeg(30000)
		r.step(0, 10000, noLag)
		r.step(50, 10050, noLag)
		r.step(100, 30000, a.next(314000))
		r.step(150, 30313, a.next(314000))
		for ms := 200; ms <= 1500; ms += 50 {
			core := int32(30600)
			if ms > 1100 {
				core += int32(ms - 1100)
			}
			r.step(ms, core, a.next(314000))
			if ms == 1100 && r.frontMs()-r.out.PositionMs < 600 {
				t.Fatalf("t=1100 ms: the front end is only %d ms ahead", r.frontMs()-r.out.PositionMs)
			}
		}
		r.wantResumes(0, 30000, 29999)
	})
}

// Review round 15: paused with e_cur 150 ms, the lyrics arrive and all_lyrics
// goes out with no playback_pause after it. By the contract the front end
// interpolates from it, so when the correction fails 500 ms later the front
// end is at 10350 and the public position at 10000: resume, then pause.
//
// Mutation (verified): the model following the player's playing state instead
// of the events turns it red.
func TestLagPausedAllLyrics(t *testing.T) {
	r, a := newLagRig(t), newFeedSeg(10000)
	r.status = 2
	r.step(0, 10000, a.next(150000))
	r.step(50, 10000, a.held(150000))
	r.lyricsNext(1000, 20000)
	if al := ofType(r.step(100, 10000, a.held(150000)), player.EventAllLyrics); len(al) != 1 || eventMs(al[0]) != 9850 {
		t.Fatalf("t=100 ms: want all_lyrics at 9850, got %+v", al)
	}
	mark := r.mark()
	for ms := 150; ms <= 1000; ms += 50 {
		r.step(ms, 10000, noLag)
	}
	if got := published(r.events[mark:]); !slices.Equal(got, []string{resumeAt(10000), pauseAt(10000)}) {
		t.Fatalf("got %v, want resume then pause at 10000", got)
	}
}

// The lag source never gives a usable sample, or the correction is off: every
// output sample equals the kernel's, and the jump at t=50 ms is published on
// its own tick.
//
// Mutation (verified): returning the lag error from Read, or using the sample
// that came with it, turns the "read fails" case red.
func TestLagNeverValid(t *testing.T) {
	for _, mode := range []string{"invalid", "read fails", "correction off"} {
		t.Run(mode, func(t *testing.T) {
			r := newLagRig(t)
			if mode == "correction off" {
				r.src = withLag(&r.core, func() (lagReader, string, error) { return nil, "", errLagOpen }, &r.warned, r.p.pollMs)
				if r.src != positionSource(&r.core) {
					t.Fatalf("correction off must keep the core reader itself, got %T", r.src)
				}
			}
			for ms := 0; ms <= 2000; ms += 50 {
				core := int32(10000)
				if ms > 0 {
					core = int32(30000 + ms - 50)
				}
				if mode == "read fails" {
					r.fail(ms, core)
				} else {
					r.step(ms, core, noLag)
				}
				if r.out != r.core.sample {
					t.Fatalf("t=%d ms: output %+v, want the kernel sample %+v", ms, r.out, r.core.sample)
				}
			}
			r.wantResumes(0, 30000)
		})
	}
}

// Review Focus 2 and the mismatch rule: single-track repeat. With e_cur 314 the
// same RID jumps from 180000 back to 0; the output reopens (base 0, e = 0) and
// its first valid sample is 1, 3, 4 or 8 polls late. From the jump on the
// output equals the kernel, never held at 0, the lyrics restart from the first
// line, and the jump publishes exactly one resume.
//
// Mutation (verified): dropping the mismatch rule turns every case red: the
// output is held at 0 until the new segment is confirmed.
func TestLagLoopToStart(t *testing.T) {
	for _, late := range []int{1, 3, 4, 8} {
		t.Run(fmt.Sprintf("lag %d polls late", late), func(t *testing.T) {
			r, a := newLagRig(t), newFeedSeg(179000)
			r.dur = 180000
			z := &feedSeg{key: pos.LagKey{Ctx: 0x05a32e40, Ring: 0x06120a00}}
			r.step(0, 179850, a.next(314000))
			var lines []int
			for ms := 1000; ms < 180000; ms += 5000 {
				lines = append(lines, ms)
			}
			r.lyrics(0, lines...)
			r.step(50, 179900, a.next(314000))
			r.step(100, 179950, a.next(314000))
			r.step(150, 180000, a.next(314000))
			mark := r.mark()
			for i := 0; i <= 30; i++ {
				core := int32(50 * i)
				lag := noLag
				if i >= late {
					lag = z.next(0)
				}
				ev := r.step(200+50*i, core, lag)
				if r.out.PositionMs != core {
					t.Fatalf("kernel at %d: output %d, want the kernel", core, r.out.PositionMs)
				}
				if lu := ofType(ev, player.EventLyricUpdate); core == 1000 && (len(lu) != 1 || lu[0].Data.(*player.LyricUpdate).Index != 0 || eventMs(lu[0]) != 1000) {
					t.Fatalf("kernel at 1000: want the first line at 1000 ms, got %+v", lu)
				}
			}
			r.wantResumes(mark, 0)
		})
	}
}

// Upper clamp, which the property test keeps out of: with e = −12 ms the
// output stops at the duration within the kernel's last 12 ms, the shifts there
// add up to no less than −12 ms, and nothing is published.
func TestLagUpperClamp(t *testing.T) {
	r, a := newLagRig(t), newFeedSeg(239000)
	for ms := 0; ms <= 150; ms += 50 {
		r.step(ms, int32(239800+ms), a.next(-12000))
	}
	total := int32(0)
	for i, core := range []int32{239990, 239995, lagDur} {
		ev := r.step(200+50*i, core, a.next(-12000))
		if r.out.PositionMs != lagDur || len(ev) != 0 {
			t.Fatalf("kernel at %d: output %d (want the duration), events %+v (want none)", core, r.out.PositionMs, ev)
		}
		total += r.out.LagShiftMs
	}
	if total < -12 || total > 0 {
		t.Fatalf("shifts in the clamp add up to %d ms, want [-12, 0]", total)
	}
}

// frozenCore always reports the same position, as a core whose position field
// no longer updates would.
type frozenCore struct{}

func (frozenCore) Read(rid string, _ bool) (pos.Sample, error) {
	return pos.Sample{RID: pos.RIDWithPrefix(rid), PositionMs: 1000, CoreMs: 1000, DurationMs: 200000, ObservedAt: time.Now()}, nil
}
func (frozenCore) Close() {}

// The liveness watch sees the core position before the correction: with the
// core frozen, an adopted correction moves the public position once (to 686),
// and the session still fails closed after livenessThreshold of playing.
//
// Mutation (verified): feeding the watch the public position turns it red.
func TestLagLivenessSeesCorePosition(t *testing.T) {
	defer func(v time.Duration) { livenessThreshold = v }(livenessThreshold)
	livenessThreshold = 300 * time.Millisecond
	p := New(0, 50)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lag := &seqLag{seg: *newFeedSeg(1000), valid: -1}
	warned := false
	err := p.runSession(ctx, snapshotFunc(func(context.Context) (cdp.Snapshot, error) { return songSnap("MUSIC_1", "A"), nil }),
		func() (positionSource, error) {
			return withLag(frozenCore{}, func() (lagReader, string, error) { return lag, "9.6.0.0_UG1", nil }, &warned, p.pollMs), nil
		})
	if !errors.Is(err, pos.ErrClockStalled) {
		t.Fatalf("frozen core did not fail closed: %v", err)
	}
	if resumes := eventsMs(ofType(drainEvents(p), player.EventPlaybackResume)); !slices.Equal(resumes, []int32{686}) {
		t.Fatalf("resumes %v, want the adoption at 686", resumes)
	}
}

// Review Focus 5: paused 10 s with the valid samples unchanged, then resumed.
// Only the state changes publish: one pause, one resume, at the corrected
// position.
func TestLagLongPause(t *testing.T) {
	r, a := newLagRig(t), newFeedSeg(10000)
	r.step(0, 10000, a.next(314000))
	r.step(50, 10050, a.next(314000))
	r.status = 2
	mark := r.mark()
	for ms := 100; ms <= 10100; ms += 50 {
		r.step(ms, 10050, a.held(314000))
	}
	r.status = 1
	for ms := 10150; ms <= 10650; ms += 50 {
		r.step(ms, int32(ms-50), a.next(314000))
	}
	pauses := eventsMs(ofType(r.events[mark:], player.EventPlaybackPause))
	if !slices.Equal(pauses, []int32{9736}) {
		t.Fatalf("pauses %v, want [9736]", pauses)
	}
	r.wantResumes(mark, 9786)
}

// lyric_update, position_sample and the line matching use the corrected
// position: line 0 (10.000 s) starts when the kernel reaches 10314 ms.
func TestLagCorrectedPositionEverywhere(t *testing.T) {
	r, a := newLagRig(t), newFeedSeg(9000)
	r.step(0, 9000, a.next(314000))
	r.lyrics(0, 10000, 12000)
	r.step(50, 9050, a.next(314000))
	lineAt, samples := -1, 0
	for ms := 100; ms <= 5500; ms += 50 {
		core := int32(9000 + ms)
		for _, e := range r.step(ms, core, a.next(314000)) {
			switch e.Type {
			case player.EventLyricUpdate, player.EventPositionSample:
				if got := eventMs(e); got != core-314 {
					t.Fatalf("t=%d ms: %s at %d, want %d", ms, e.Type, got, core-314)
				}
				if e.Type == player.EventPositionSample {
					samples++
				} else if e.Data.(*player.LyricUpdate).Index == 0 {
					lineAt = ms
				}
			}
		}
	}
	if lineAt != 1350 || samples != 1 {
		t.Fatalf("line 0 at t=%d ms (want 1350), %d position samples (want 1)", lineAt, samples)
	}
}

// A core failure is returned as it is, without reading the lag; Close closes
// both readers.
//
// Mutation (verified): reading the lag before the core, swallowing the core
// error, or closing only one reader turns it red.
func TestLagSourceCoreFailure(t *testing.T) {
	core, lag := &stubCore{err: pos.ErrSongChanged}, &stubLag{}
	var warned bool
	src := newCorrectedSource(core, lag, &warned, 50)
	if _, err := src.Read("MUSIC_1", true); !errors.Is(err, pos.ErrSongChanged) || lag.reads != 0 {
		t.Fatalf("core failure: err %v, %d lag reads; want the core error and none", err, lag.reads)
	}
	core.err = nil
	core.sample = pos.Sample{RID: "MUSIC_1", PositionMs: 1000, CoreMs: 1000, DurationMs: lagDur, ObservedAt: lagT0}
	if _, err := src.Read("MUSIC_1", true); err != nil || lag.reads != 1 {
		t.Fatalf("err %v, %d lag reads; want nil and one", err, lag.reads)
	}
	src.Close()
	if core.closes != 1 || lag.closes != 1 {
		t.Fatalf("closes: core %d, lag %d; want 1 and 1", core.closes, lag.closes)
	}
}

// captureLog collects the standard logger's output for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	w := stdlog.Writer()
	stdlog.SetOutput(&buf)
	t.Cleanup(func() { stdlog.SetOutput(w) })
	return &buf
}

// logIcons returns the level icons of the logged lines that contain s.
func logIcons(buf *bytes.Buffer, s string) []string {
	var out []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.Contains(line, s) {
			continue
		}
		for _, icon := range []string{"[!]", "[+]"} {
			if strings.Contains(line, icon) {
				out = append(out, icon)
			}
		}
	}
	return out
}

// drift adopts 314 ms in a new segment from t=ms, then three samples at 350 ms
// disable the correction for the song.
func (r *lagRig) drift(ms int, core int32, base uint32) {
	a := newFeedSeg(base)
	r.step(ms, core, a.next(314000))
	r.step(ms+50, core+50, a.next(314000))
	for i := 2; i <= 4; i++ {
		r.step(ms+50*i, core+50*int32(i), a.next(350000))
	}
}

// The logs of design §3.5. The tests run in Chinese, so they check the level
// icons and the values only.
func TestLagLogs(t *testing.T) {
	// Mutation (verified): dropping either log line turns it red.
	t.Run("connect", func(t *testing.T) {
		buf := captureLog(t)
		var core stubCore
		var warned bool
		withLag(&core, func() (lagReader, string, error) { return nil, "", errLagOpen }, &warned, 50)
		if src := withLag(&core, func() (lagReader, string, error) { return &stubLag{}, "9.6.0.0_UG1", nil }, &warned, 50); src == positionSource(&core) {
			t.Fatal("an opened lag reader must wrap the core reader")
		}
		if got := logIcons(buf, errLagOpen.Error()); !slices.Equal(got, []string{"[+]"}) {
			t.Fatalf("disabled log %v in %q", got, buf.String())
		}
		if got := logIcons(buf, "9.6.0.0_UG1"); !slices.Equal(got, []string{"[+]"}) {
			t.Fatalf("enabled log %v in %q", got, buf.String())
		}
	})
	// Mutation (verified): dropping the log turns it red.
	t.Run("adopted", func(t *testing.T) {
		buf := captureLog(t)
		r, a := newLagRig(t), newFeedSeg(30000)
		r.step(0, 30000, a.next(314000))
		r.step(50, 30050, a.next(314000))
		r.step(100, 30100, a.next(314000))
		for _, s := range []string{"314.0 ms", "0x05A32E40", "0x06120A00", "30000 ms"} {
			if got := logIcons(buf, s); !slices.Equal(got, []string{"[+]"}) {
				t.Fatalf("%q: %v in %q", s, got, buf.String())
			}
		}
	})
	// The first drift of a connection is a warning, later ones, in the same
	// source or a later one of the connection, are details.
	//
	// Mutation (verified): logging every drift as a warning or as a detail,
	// or keeping the flag per source, turns it red.
	t.Run("drift warns once per connection", func(t *testing.T) {
		buf := captureLog(t)
		r := newLagRig(t)
		r.drift(0, 10000, 10000)
		r.rid = "MUSIC_2"
		r.drift(250, 1000, 0)
		again := newLagRig(t)
		again.src = newCorrectedSource(&again.core, &again.lag, &r.warned, again.p.pollMs)
		again.drift(0, 20000, 20000)
		newLagRig(t).drift(0, 30000, 30000)
		if got := logIcons(buf, "350.0 ms"); !slices.Equal(got, []string{"[!]", "[+]", "[+]", "[!]"}) {
			t.Fatalf("drift logs %v, want [!] [+] [+] [!], in %q", got, buf.String())
		}
	})
}

// walkCore advances 50 ms per read for whatever RID is expected, so its
// positions are multiples of 50 ms; with e = 314 ms a corrected one is 36 mod
// 50.
type walkCore struct {
	pos    int32
	closes int
}

func (c *walkCore) Read(rid string, _ bool) (pos.Sample, error) {
	c.pos += 50
	return pos.Sample{RID: pos.RIDWithPrefix(rid), PositionMs: c.pos, CoreMs: c.pos, DurationMs: 200000, ObservedAt: time.Now()}, nil
}
func (c *walkCore) Close() { c.closes++ }

// seqLag answers valid samples of e = 314 ms for its first valid reads, then
// fails; a negative valid never fails.
type seqLag struct {
	seg           feedSeg
	valid         int
	reads, closes int
}

func (l *seqLag) Read() (pos.LagSample, pos.OutputState, error) {
	l.reads++
	if l.valid >= 0 && l.reads > l.valid {
		return pos.LagSample{}, pos.OutputState{}, errLagRead
	}
	return l.seg.next(314000), pos.OutputState{}, nil
}
func (l *seqLag) Close() { l.closes++ }

func corrected(e player.Event) bool   { ms := eventMs(e); return ms >= 0 && ms%50 == 36 }
func uncorrected(e player.Event) bool { ms := eventMs(e); return ms >= 0 && ms%50 == 0 }

// waitEvents polls the collected events until cond holds, failing if the
// session ends first or 5 s pass.
func waitEvents(t *testing.T, done <-chan error, events func() []player.Event, cond func([]player.Event) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond(events()) {
		select {
		case err := <-done:
			t.Fatalf("session ended: %v", err)
		case <-deadline:
			t.Fatal("condition not met within 5 s")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Review Focus 3: in_ffaudio is not loaded when the core reader first opens,
// so the correction stays off; after the song stops the core reader is
// reopened, the lag reader opens with it and the second song is corrected.
//
// Mutation (verified): attempting OpenLag only on the first core open, or
// keeping the core reader open while there is no song, turns it red.
func TestLagOpenRetriedOnReopen(t *testing.T) {
	src := &phasedSnapshots{steps: []snapStep{
		{calls: 3, snap: songSnap("MUSIC_1", "A")},
		{calls: 2, snap: cdp.Snapshot{Status: 3}},
		{calls: 1 << 30, snap: songSnap("MUSIC_2", "B")},
	}}
	var cores []*walkCore
	var lags []*seqLag
	lagOpens := 0
	warned := false
	open := func() (positionSource, error) {
		core := &walkCore{pos: 1000}
		cores = append(cores, core)
		return withLag(core, func() (lagReader, string, error) {
			if lagOpens++; lagOpens == 1 {
				return nil, "", errLagOpen
			}
			l := &seqLag{seg: *newFeedSeg(0), valid: -1}
			lags = append(lags, l)
			return l, "9.6.0.0_UG1", nil
		}, &warned, 50), nil
	}
	p := New(0, 50)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	events := collect(p, stop)
	done := make(chan error, 1)
	go func() { done <- p.runSession(ctx, src, open) }()
	songB := func(ev []player.Event) int {
		return slices.IndexFunc(ev, func(e player.Event) bool { si, ok := e.Data.(*player.SongInfo); return ok && si.Name == "B" })
	}
	waitEvents(t, done, events, func(ev []player.Event) bool {
		i := songB(ev)
		return i >= 0 && slices.ContainsFunc(ev[i:], func(e player.Event) bool { return e.Type == player.EventPlaybackResume && corrected(e) })
	})
	got := events()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected exit: %v", err)
	}
	before := positioned(got[:songB(got)])
	if len(before) == 0 || !slices.ContainsFunc(before, uncorrected) || slices.ContainsFunc(before, corrected) {
		t.Fatalf("song A must be published uncorrected: %v", eventsMs(before))
	}
	if len(cores) != 2 || lagOpens != 2 || len(lags) != 1 || cores[0].closes != 1 || cores[1].closes != 1 || lags[0].closes != 1 {
		t.Fatalf("lifecycle: %d core opens, %d lag opens, cores %+v, lags %+v", len(cores), lagOpens, cores, lags)
	}
}

// Review Focus 4: the lag reader starts failing on every read mid-song (for
// example KwService restarted). 500 ms later the position is back to the
// kernel's and the session goes on; with the transient tolerance shrunk, a
// failure that reached runSession would end it before that.
//
// Mutation (verified): returning the lag error from Read turns it red.
func TestLagFailureKeepsSession(t *testing.T) {
	defer func(v time.Duration) { transientTolerance = v }(transientTolerance)
	transientTolerance = 200 * time.Millisecond
	core, lag := &walkCore{pos: 1000}, &seqLag{seg: *newFeedSeg(1000), valid: 4}
	warned := false
	p := New(0, 50)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	events := collect(p, stop)
	done := make(chan error, 1)
	go func() {
		done <- p.runSession(ctx, snapshotFunc(func(context.Context) (cdp.Snapshot, error) { return songSnap("MUSIC_1", "A"), nil }),
			func() (positionSource, error) {
				return withLag(core, func() (lagReader, string, error) { return lag, "9.6.0.0_UG1", nil }, &warned, 50), nil
			})
	}()
	waitEvents(t, done, events, func(ev []player.Event) bool {
		resumes := ofType(ev, player.EventPlaybackResume)
		i := slices.IndexFunc(resumes, corrected)
		return i >= 0 && slices.ContainsFunc(resumes[i:], uncorrected)
	})
	got := events()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected exit: %v", err)
	}
	if waitingProcessSeen(got) {
		t.Fatal("the session reported waiting_process while the lag reader failed")
	}
	if lag.reads < lag.valid+2 {
		t.Fatalf("%d lag reads: the failing reader was not read on", lag.reads)
	}
}

// Front-end simulation (design §4, first item).
const (
	lagPropSequences = 1500
	propDur          = 300000 // ms
)

// propSeg is one output segment of a generated sequence.
type propSeg struct {
	key        pos.LagKey
	eUs, dUs   int64
	written    uint32
	start      int  // poll at which the kernel enters the segment
	driftAfter int  // valid samples before the lag starts to drift; 0: never
	valid      int  // valid samples so far
	silent     bool // entered by a silent seek
}

// propPoll is one poll of the kernel plan.
type propPoll struct {
	ms     int
	rid    string
	status int
	core   int32
	silent int32 // a silent seek on this poll: how far the kernel moved beyond the elapsed time, ms
}

// propStats counts what the generator produced, over all sequences.
type propStats struct {
	stallSeqs, slowSeqs, polls, stalled, toggles, songs, jumps  int
	segs, early, sameTick, late                                 int
	invalid, failures, outages, short, driftSegs, driftSeqs     int
	adoptions, raises, zeroed, resumes, shiftResumes, lyricSeqs int
	nearZero, mismatch                                          int
	silentSeeks, silentEarly, sameAdoptions, adoptionNotices    int
}

// planLag generates a kernel plan and its segments. Away from jumps and silent
// seeks the kernel advances with the wall clock while playing, except during
// stalls (only in stall sequences). Jumps move at least 3 s, so the output
// jumps too; a quarter of those from beyond 3.6 s land within 600 ms of 0, the
// others in [3 s, 290 s]. A silent seek, only while playing, moves the kernel
// 1-250 ms beyond the elapsed time, which is no jump, into a segment whose e
// is the previous segment's half the time. With e at least -38 ms (-12 ms one
// packet short) the output stays out of the upper clamp, which
// TestLagUpperClamp covers.
func planLag(rng *rand.Rand, st *propStats) (poll int, stalls bool, plan []propPoll, segs []propSeg) {
	poll = []int{50, 50, 100, 250, 500, 1000}[rng.IntN(6)]
	stalls = rng.IntN(4) == 0
	newSeg := func(start int, base uint32, eUs int64) {
		s := propSeg{key: pos.LagKey{Ctx: 0x05000000 + 0x40*uint32(len(segs)), Ring: 0x06120a00, Base: base}, eUs: eUs,
			dUs: []int64{13100, 20000, 23200, 26122}[rng.IntN(4)], start: start}
		if rng.IntN(20) == 0 {
			s.driftAfter = 2 + rng.IntN(8)
			st.driftSegs++
		}
		segs = append(segs, s)
	}
	song, status := 1, 1
	if rng.IntN(5) == 0 {
		status = 2
	}
	core := int32(500 + rng.IntN(2500))
	newSeg(0, 0, int64(rng.IntN(4001)-2000)) // opening: e ≈ 0
	ms, stall, silent := 0, 0, int32(0)
	for i := range 120 + rng.IntN(240) {
		if i > 0 {
			dt := poll + rng.IntN(poll/5+1)
			ms += dt
			if rng.IntN(50) == 0 {
				status = 3 - status
				st.toggles++
			}
			if stalls && stall == 0 && rng.IntN(40) == 0 {
				stall = 1 + rng.IntN(20)
			}
			switch r := rng.IntN(100); {
			case r < 1:
				song++
				st.songs++
				core = int32(500 + rng.IntN(2500))
				newSeg(i, 0, int64(rng.IntN(4001)-2000))
			case r < 4 || core > 290000:
				target := core
				eUs := int64(rng.IntN(412001) - 12000)
				if core >= 3600 && rng.IntN(4) == 0 {
					// Repeat or seek to 0:00: the mismatch rule. Within a
					// segment e does not exceed the target.
					target = int32(rng.IntN(600))
					eUs = rng.Int64N(int64(target)*1000 + 1)
					st.nearZero++
				}
				for max(target-core, core-target) < 3000 {
					target = int32(3000 + rng.IntN(287000))
				}
				core = target
				newSeg(i, uint32(target)/1000*1000, eUs)
			case r < 5 && status == 1:
				silent = int32(1 + rng.IntN(250))
				eUs := int64(rng.IntN(412001) - 12000)
				if rng.IntN(2) == 0 {
					eUs = segs[len(segs)-1].eUs
				}
				core += int32(dt) + silent
				newSeg(i, uint32(core)/1000*1000, eUs)
				segs[len(segs)-1].silent = true
				st.silentSeeks++
			case stall > 0:
				stall--
				st.stalled++
			case status == 1:
				core += int32(dt)
			}
		}
		plan = append(plan, propPoll{ms: ms, rid: fmt.Sprintf("MUSIC_%d", song), status: status, core: core, silent: silent})
		silent = 0
	}
	return poll, stalls, plan, segs
}

// lagGen produces the lag samples of one sequence.
type lagGen struct{ outage, short int }

func (g *lagGen) sample(rng *rand.Rand, s *propSeg, st *propStats) (pos.LagSample, error) {
	r := rng.IntN(100)
	switch {
	case g.outage > 0:
		g.outage--
		st.failures++
		return pos.LagSample{}, errLagRead
	case r == 0:
		g.outage = 5 + rng.IntN(25)
		st.outages++
		st.failures++
		return pos.LagSample{}, errLagRead
	case r < 4:
		st.failures++
		return pos.LagSample{}, errLagRead
	case r < 12 || s.written == 0:
		st.invalid++
		return pos.LagSample{}, nil
	}
	e := s.eUs
	switch {
	case g.short > 0:
		g.short--
		e -= s.dUs
		st.short++
	case rng.IntN(30) == 0: // one packet short, alone or twice in a row
		g.short = rng.IntN(2)
		e -= s.dUs
		st.short++
	case rng.IntN(60) == 0:
		e -= 2900
		st.short++
	}
	if s.driftAfter > 0 && s.valid >= s.driftAfter {
		e += int64(s.valid-s.driftAfter+1) * 8000
	}
	s.valid++
	return pos.LagSample{Valid: true, Key: s.key, Written: s.written, EUs: e, DUs: s.dUs}, nil
}

// frontEnd follows doc/API_RESPONSE_EXAMPLES.md "时间插值", independently of the
// session: it anchors on every positioned event; all_lyrics and
// playback_resume start the wall-clock extrapolation, playback_pause stops it,
// lyric_update corrects the anchor only.
type frontEnd struct {
	pos     float32
	at      time.Time
	running bool
}

func (f *frontEnd) see(e player.Event, at time.Time) {
	switch e.Type {
	case player.EventAllLyrics, player.EventPlaybackResume:
		f.running = true
	case player.EventPlaybackPause:
		f.running = false
	case player.EventLyricUpdate:
	default:
		return
	}
	f.pos, f.at = float32(eventMs(e))/1000, at
}

func (f *frontEnd) now(at time.Time) float32 {
	return player.LivePosition(f.pos, f.at, at, f.running, 0)
}

// Random sequences of kernel samples (jumps, silent seeks, stalls, pauses, song
// changes) and lag samples (valid, invalid, new segments switching up to two
// polls before or three after the kernel, one-packet-short and slightly short
// samples, drifting segments, read failures and outages) through the corrected
// source and the session, with lyrics or without, at 50-1000 ms polls. For
// every sequence:
//   - without stalls, the front end and the public position differ by less
//     than 200 ms at every playing poll, plus what silent seeks moved the
//     kernel beyond the elapsed time (CoreClock's own tolerance) until a poll
//     the session judges (a first adoption or a correction change) or the
//     front end anchors;
//   - each segment changes e_cur at most ⌊D₀⌋ + 3 times: a change to a
//     non-zero value belongs to the segment of that poll's valid sample, a
//     change to 0 to the segment whose value it ends;
//   - playback_resume count ≤ kernel jumps + e_cur changes + paused-to-playing
//     changes (each publishes its own resume on the original path) + first
//     adoptions without a change (LagAdopted, at most one per segment).
//
// Mutation (verified): dropping the session's judgement, its LagAdopted
// trigger, or the corrector's LagShiftMs, turns the first property red;
// letting e_cur follow every valid sample once confirmed, the second; judging
// on every poll, the third. Without Apply, or with the corrector's LagAdopted
// never set, the generator check fails (no adoptions; no first adoptions
// without a change).
func TestLagFrontendProperty(t *testing.T) {
	w := stdlog.Writer()
	stdlog.SetOutput(io.Discard)
	t.Cleanup(func() { stdlog.SetOutput(w) })
	rng := rand.New(rand.NewPCG(131, 20261027))
	var st propStats
	for n := range lagPropSequences {
		runLagProperty(t, rng, n, &st)
	}
	t.Logf("%+v", st)
	for _, c := range []struct {
		name string
		n    int
	}{
		{"stall sequences", st.stallSeqs}, {"slow polls", st.slowSeqs}, {"stalled polls", st.stalled},
		{"pause toggles", st.toggles}, {"song changes", st.songs}, {"kernel jumps", st.jumps},
		{"segments switched early", st.early}, {"on the jump tick", st.sameTick}, {"late", st.late},
		{"invalid samples", st.invalid}, {"read failures", st.failures}, {"outages", st.outages},
		{"short samples", st.short}, {"drift disables", st.driftSeqs}, {"adoptions", st.adoptions},
		{"raises", st.raises}, {"zeroings", st.zeroed}, {"resumes from a correction change", st.shiftResumes},
		{"sequences with lyrics", st.lyricSeqs}, {"jumps to near 0", st.nearZero},
		{"polls whose kernel position is below the previous e_cur", st.mismatch},
		{"silent seeks", st.silentSeeks}, {"silent seeks whose lag switched two polls early", st.silentEarly},
		{"first adoptions without a change", st.sameAdoptions},
		{"resumes on a first adoption without a change", st.adoptionNotices},
	} {
		if c.n == 0 {
			t.Errorf("the generator produced no %s", c.name)
		}
	}
}

func runLagProperty(t *testing.T, rng *rand.Rand, n int, st *propStats) {
	poll, stalls, plan, segs := planLag(rng, st)
	st.polls += len(plan)
	st.segs += len(segs)
	if stalls {
		st.stallSeqs++
	}
	if poll >= 500 {
		st.slowSeqs++
	}
	// The lag source switches to a segment up to two polls before the kernel
	// enters it or up to three after.
	switchAt := make([]int, len(segs))
	for s := 1; s < len(segs); s++ {
		switchAt[s] = max(segs[s].start+rng.IntN(6)-2, switchAt[s-1]+1)
		switch d := switchAt[s] - segs[s].start; {
		case d < 0:
			st.early++
			if d == -2 && segs[s].silent {
				st.silentEarly++
			}
		case d == 0:
			st.sameTick++
		default:
			st.late++
		}
	}
	var lines []lyric.Line
	if rng.IntN(3) > 0 {
		st.lyricSeqs++
		gap := 2000 + rng.IntN(5000)
		for ms := 1000; ms < propDur; ms += gap {
			lines = append(lines, lyric.Line{TimeMs: ms, Text: "line"})
		}
	}
	p := New(0, poll)
	s := newSession(p)
	var core stubCore
	var lag stubLag
	warned := false
	src := newCorrectedSource(&core, &lag, &warned, poll)
	var fe frontEnd
	var g lagGen
	ls, owner, songStart := 0, -1, 0
	var ded, silent int32
	changes := make([]int, len(segs))
	var jumps, stateResumes, resumes, totalChanges, sameAdoptions int
	for i, pp := range plan {
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("sequence %d (poll %d ms, stalls %v), poll %d at %d ms: %s", n, poll, stalls, i, pp.ms, fmt.Sprintf(format, args...))
		}
		for ls+1 < len(segs) && switchAt[ls+1] <= i {
			ls++
		}
		seg := &segs[ls]
		if pp.status == 1 {
			seg.written += 4096
		}
		lag.sample, lag.err = g.sample(rng, seg, st)
		validSeg := -1
		if lag.err == nil && lag.sample.Valid {
			validSeg = ls
		}
		at := lagT0.Add(time.Duration(pp.ms) * time.Millisecond)
		core.sample = pos.Sample{RID: pp.rid, PositionMs: pp.core, CoreMs: pp.core, DurationMs: propDur, ObservedAt: at}
		out, err := src.Read(pp.rid, pp.status == 1)
		if err != nil {
			fail("Read returned %v", err)
		}
		// The mismatch rule keeps a positive e_cur from pressing the output to
		// 0 and the generator keeps out of the upper clamp, so the deduction
		// (d below) is e_cur in effect, never a clamp.
		if out.PositionMs < 0 || out.PositionMs >= propDur {
			fail("output %d out of range or clamped at the duration", out.PositionMs)
		}
		if ded > 0 && pp.core < ded {
			st.mismatch++
		}
		kernelEvent := false
		if i > 0 && plan[i-1].rid == pp.rid {
			prev := plan[i-1]
			if step := int64(pp.core) - int64(prev.core); step < 0 || step > int64(pp.ms-prev.ms)+250 {
				jumps++
				kernelEvent = true
			}
			if prev.status == 2 && pp.status == 1 {
				stateResumes++
				kernelEvent = true
			}
		} else {
			songStart = i
		}
		if d := pp.core - out.PositionMs; d != ded {
			totalChanges++
			switch {
			case d != 0 && validSeg < 0:
				fail("e_cur changed to %d ms without a valid sample", d)
			case d != 0:
				if owner == validSeg {
					st.raises++
				} else {
					st.adoptions++
				}
				owner = validSeg
				changes[owner]++
			case owner < 0:
				fail("e_cur changed to 0 from %d ms with no segment owning it", ded)
			default:
				st.zeroed++
				changes[owner]++
				owner = -1
			}
			ded = d
		}
		s.observe(cdp.Snapshot{RID: pp.rid, Title: "song " + pp.rid, Artist: "artist", Status: pp.status}, out)
		if i == songStart+1 && lines != nil {
			s.accept(assetResult{gen: s.gen, rid: s.rid, lines: lines}, at)
		}
		s.emitLine(at)
		// A silent seek moves the front end off by what the kernel moved beyond
		// the elapsed time, until the front end anchors or the session judges a
		// poll. A poll judged with it included brings it back within 200 ms.
		silent += pp.silent
		sameAdoption := out.LagAdopted && out.LagShiftMs == 0
		if out.LagAdopted || out.LagShiftMs != 0 {
			silent = 0
		}
		if sameAdoption {
			sameAdoptions++
		}
		for _, e := range drainEvents(p) {
			if e.Type == player.EventPlaybackResume {
				resumes++
				if !kernelEvent {
					st.shiftResumes++
					if sameAdoption {
						st.adoptionNotices++
					}
				}
			}
			if len(positioned([]player.Event{e})) > 0 {
				silent = 0
			}
			fe.see(e, at)
		}
		if !stalls && pp.status == 1 {
			if d := math.Abs(float64(fe.now(at)) - float64(out.PositionMs)/1000); d >= 0.2+float64(silent)/1000 {
				fail("front end at %.3f s, public position %.3f s (kernel %d ms, silent seeks %d ms)", fe.now(at), float64(out.PositionMs)/1000, pp.core, silent)
			}
		}
	}
	for k, c := range changes {
		if c > int(segs[k].dUs/1000)+3 {
			t.Fatalf("sequence %d: segment %d (D %d µs) changed e_cur %d times", n, k, segs[k].dUs, c)
		}
	}
	if resumes > jumps+totalChanges+stateResumes+sameAdoptions {
		t.Fatalf("sequence %d: %d resumes > %d kernel jumps + %d e_cur changes + %d resumed pauses + %d first adoptions without a change",
			n, resumes, jumps, totalChanges, stateResumes, sameAdoptions)
	}
	st.sameAdoptions += sameAdoptions
	st.jumps += jumps
	st.resumes += resumes
	if warned {
		st.driftSeqs++
	}
}
