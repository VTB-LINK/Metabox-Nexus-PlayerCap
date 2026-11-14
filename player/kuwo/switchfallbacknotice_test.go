package kuwo

// This file tests only how the #185 hold of a step back to the start keeps the
// front end anchored (doc/design/kuwo-switch-fallback-design.md §5.2): the
// front-end notices of #131 and #135 ("重核前端偏差", "事件条数"), and the
// lyrics and lines that would otherwise go out during a hold, through the real
// corrected source, filter and corrector of lagRig with its hold on.

import (
	"math"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

// Review round 1, item 2: a correction change on a held tick. With e = 220 ms
// adopted, the fall to 0 makes rule 5 zero e on a held tick (LagShiftMs 220
// there); the lag samples stay invalid, so no later tick carries a flag, and the
// release is no seek by CoreClock's own criterion (the public position gains the
// elapsed time plus 220 ms, within its 250 ms). The front end is 220 ms behind
// unless the first observe after the hold judges it anyway. Three ways the hold
// ends with the song going on: confirmed; a failed read (runSession drops the
// hold) then the core goes on; no song (dropped, the reader reopened with a fresh
// corrector) then the same song.
// 变异自证：observe 的通知条件去掉 s.recheck，三个子用例都没有 playback_resume 而变红（实测）。
func TestSwitchFallRecheckAfterHold(t *testing.T) {
	for _, c := range []struct {
		name string
		gap  func(r *lagRig, at time.Time) // what happens at 1100 ms instead of a read; nil: read on
	}{
		{"confirmed", nil},
		{"read failed", func(r *lagRig, at time.Time) { r.s.dropHold(at) }},
		{"no song, reader reopened", func(r *lagRig, at time.Time) {
			r.s.dropHold(at)
			r.src = newCorrectedSource(&r.core, &r.lag, &r.warned, r.p.pollMs)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newLagRig(t)
			r.hold = true
			seg := newFeedSeg(140000)
			r.step(0, 149000, noLag)
			r.step(50, 149050, seg.next(220000))
			r.step(100, 149100, seg.next(220000)) // e = 220 adopted: the notice re-anchors the front end
			for ms := 150; ms <= 1000; ms += 50 {
				r.step(ms, 149000+int32(ms), seg.next(220000))
			}
			mark := r.mark()
			r.step(1050, 0, noLag) // the fall, held; rule 5 zeroes e
			from := 1100
			if c.gap != nil {
				c.gap(r, lagT0.Add(1100*time.Millisecond))
				from = 1150
			}
			for ms := from; ms <= 1500; ms += 50 {
				r.step(ms, 149000+int32(ms), noLag)
			}
			if got := countResumes(r.events[mark:]); got != 1 {
				t.Fatalf("%d playback_resume after the hold, want 1: %v", got, eventsMs(positioned(r.events[mark:])))
			}
			if d := math.Abs(float64(r.frontMs() - r.out.PositionMs)); d >= 200 {
				t.Fatalf("front end %d ms, public %d ms: %v apart", r.frontMs(), r.out.PositionMs, d)
			}
		})
	}
}

// Review round 1, item 4: paused, dragged to 0:00, resumed. DirectSound starts
// about 0.95 s later. The confirmation (300 ms) publishes the step at 0; the
// TransientResync of the Play-issued tick after it publishes the second resume
// the front end needs, as TestLagTransientPauseDragDSTwoResumes without the hold.
// With a 1000 ms confirmation that tick is held: the release publishes the one
// resume at the filtered position and the front end agrees with it after.
// 变异自证：确认条件另加「CoreMs 不低于 startBand」，确认等不到、playback_resume 少于预期，两个子用例都变红（实测）。
func TestSwitchFallConfirmThenTransientResync(t *testing.T) {
	defer func(v time.Duration) { switchConfirmAfter = v }(switchConfirmAfter)
	for _, c := range []struct {
		name    string
		confirm time.Duration
		want    int
	}{
		{"confirmed before Play", 300 * time.Millisecond, 2},
		{"Play while held", 1000 * time.Millisecond, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			switchConfirmAfter = c.confirm
			r := newLagRig(t)
			r.hold = true
			r.dsTick(0, 1, 10000, 10000, 20000, noLag)  // playing, steady at 10000
			r.dsTick(50, 2, 10000, 10000, 20000, noLag) // paused
			r.dsTick(100, 1, 313, 0, 100, noLag)        // resumed and dragged to 0:00: held
			for ms := 150; ms < 950; ms += 50 {
				r.dsTick(ms, 1, 600, 0, 600, noLag)
			}
			for ms := 950; ms <= 1300; ms += 50 { // fed crosses at 950: Play issued
				r.dsTick(ms, 1, 914, 0, 800, noLag)
			}
			if got := countResumes(r.events); got != c.want {
				t.Fatalf("%d playback_resume, want %d: %v", got, c.want, eventsMs(positioned(r.events)))
			}
			if d := math.Abs(float64(r.frontMs() - r.out.PositionMs)); d >= 200 {
				t.Fatalf("front end %d ms, public %d ms: %v apart", r.frontMs(), r.out.PositionMs, d)
			}
		})
	}
}

// Code review round 2: lyrics kept during a hold meet a release that pauses. A
// plays at 150000 and falls to 0 (held) at t=50; the lyrics come at t=100; from
// t=200 the bridge reports A paused, the core still at 0; t=350 releases it (no
// song change, the channel free, no valid lag sample). The release publishes the
// step and the pause, and the kept all_lyrics, which starts the front end, must
// go out before the pause: after it, the front end runs on with no pause to stop
// it (the pause enqueued, pausePending clear) and is 500 ms on by t=850.
// 变异自证（实测）：把留存结果的接收挪回 resume/pause 分支之后（即上一版 d5a28a0 的写法），放行拍的
// 事件顺序变为 playback_resume、playback_pause、all_lyrics 而变红。
func TestSwitchFallKeptLyricsBeforePause(t *testing.T) {
	defer func(v time.Duration) { switchConfirmAfter = v }(switchConfirmAfter)
	switchConfirmAfter = 300 * time.Millisecond
	r := newLagRig(t)
	r.hold = true
	r.step(0, 150000, noLag)
	r.step(50, 0, noLag) // the fall: held
	r.lyricsNext(100000, 200000)
	r.step(100, 0, noLag) // the lyrics come: kept
	r.step(150, 0, noLag)
	r.status = 2 // paused from here on, the core still at 0
	r.step(200, 0, noLag)
	r.step(250, 0, noLag)
	r.step(300, 0, noLag)
	mark := r.mark()
	r.step(350, 0, noLag) // released: 50 + 300
	got := r.events[mark:]
	pub := positioned(got)
	if len(pub) == 0 || pub[len(pub)-1].Type != player.EventPlaybackPause || len(ofType(got, player.EventAllLyrics)) != 1 {
		t.Fatalf("the release published %v, want the kept all_lyrics, then the step, ending on playback_pause", eventTypes(pub))
	}
	if r.s.front.running {
		t.Fatal("the release left the front-end model running while paused")
	}
	for ms := 400; ms <= 850; ms += 50 {
		r.step(ms, 0, noLag)
	}
	if r.fe.running || r.frontMs() != 0 || r.out.PositionMs != 0 {
		t.Fatalf("at 850 ms the front end is at %d ms (running %v), public %d ms; want both held at 0", r.frontMs(), r.fe.running, r.out.PositionMs)
	}
}

// Code review round 1, item 1: nothing of the song goes out during a hold; what
// would have goes out on the release, anchored there. "lyrics": the lyrics come
// on a held tick (t=250); an all_lyrics then would carry 150000 and anchor the
// model at t=0, so the release at t=350 (150350, no seek, the model in step)
// would leave the front end, interpolating from its receipt at t=250, at 150100.
// "dropped line": a line change dropped on a full channel at t=100 is resent by
// emitLine on the next tick, the fall; resent then, it would anchor the model at
// t=100. play returns the event marks of the first held poll and of the release.
// 变异自证（实测）：accept 不在持有期间留存结果，lyrics 子用例持有期间发出 all_lyrics（位置
// 150000）而变红；emitLine 去掉持有判断，dropped_line 子用例持有期间补发 lyric_update（150100）而
// 变红；放行时不接收留存的结果，lyrics 子用例放行后没有歌词行而变红。修复前的代码上，两个子用例
// 分别在持有期间发出 all_lyrics 与 lyric_update（150000）、lyric_update（150100）而变红。
func TestSwitchFallNothingPublishedWhileHeld(t *testing.T) {
	for _, c := range []struct {
		name string
		play func(r *lagRig) (from, to int)
	}{
		{"lyrics", func(r *lagRig) (int, int) {
			r.step(0, 150000, noLag)
			from := r.mark()
			r.step(50, 0, noLag) // the fall: held
			for ms := 100; ms <= 200; ms += 50 {
				r.step(ms, 150000+int32(ms), noLag)
			}
			r.lyricsNext(100000, 200000) // the current line lasts to 200000
			r.step(250, 150250, noLag)
			r.step(300, 150300, noLag)
			to := r.mark()
			r.step(350, 150350, noLag) // confirmed: 50 + 300
			return from, to
		}},
		{"dropped line", func(r *lagRig) (int, int) {
			r.step(0, 150000, noLag)
			r.lyrics(0, 100000, 150100)
			r.step(50, 150050, noLag) // line 0 goes out
			fillEvents(r.p)
			r.step(100, 150100, noLag) // line 1 is dropped
			from := r.mark()
			r.step(150, 0, noLag) // the fall: held
			for ms := 200; ms <= 400; ms += 50 {
				r.step(ms, 150000+int32(ms), noLag)
			}
			to := r.mark()
			r.step(450, 150450, noLag) // confirmed: 150 + 300
			return from, to
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func(v time.Duration) { switchConfirmAfter = v }(switchConfirmAfter)
			switchConfirmAfter = 300 * time.Millisecond
			r := newLagRig(t)
			r.hold = true
			from, to := c.play(r)
			if held := r.events[from:to]; len(held) != 0 {
				t.Fatalf("published while held: %v at %v", eventTypes(held), eventsMs(held))
			}
			released := r.events[to:]
			if len(ofType(released, player.EventLyricUpdate)) == 0 {
				t.Fatalf("the release published no line: %v", eventTypes(released))
			}
			if d := math.Abs(float64(r.frontMs() - r.out.PositionMs)); d >= 200 {
				t.Fatalf("front end %d ms, public %d ms after the release: %v apart", r.frontMs(), r.out.PositionMs, d)
			}
		})
	}
}
