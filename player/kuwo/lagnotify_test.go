package kuwo

// This file tests only the session's seek-lag notice (issue #131,
// doc/design/kuwo-seek-lag-design.md §3.3 "通知"), with the corrector's
// LagShiftMs and LagAdopted set by hand: on a tick that carries either, the
// public position is compared with a model of the front end that follows the
// positioned events enqueued for the song, and lagNotifyMs or more apart is
// published as a seek (playback_resume) until a positioned event is enqueued.
// Sequences through the corrector are in lagsession_test.go.

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/lyric"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

// observedLag is observedSong with the corrector's fields for this tick.
func observedLag(s *session, rid string, status int, ms int32, at time.Time, shift int32, adopted bool) bool {
	return s.observe(cdp.Snapshot{RID: rid, Title: "song " + rid, Artist: "artist", Status: status},
		pos.Sample{RID: rid, PositionMs: ms, DurationMs: 100000, ObservedAt: at, LagShiftMs: shift, LagAdopted: adopted})
}

func ofType(events []player.Event, typ string) []player.Event {
	var out []player.Event
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// positioned returns the four positioned public events, in order.
func positioned(events []player.Event) []player.Event {
	var out []player.Event
	for _, e := range events {
		switch e.Type {
		case player.EventAllLyrics, player.EventLyricUpdate, player.EventPlaybackPause, player.EventPlaybackResume:
			out = append(out, e)
		}
	}
	return out
}

// published lists the playback_resume and playback_pause events, in order, as
// type@ms.
func published(events []player.Event) []string {
	var out []string
	for _, e := range events {
		if e.Type == player.EventPlaybackResume || e.Type == player.EventPlaybackPause {
			out = append(out, fmt.Sprintf("%s@%d", e.Type, eventMs(e)))
		}
	}
	return out
}

func resumeAt(ms int32) string { return fmt.Sprintf("%s@%d", player.EventPlaybackResume, ms) }
func pauseAt(ms int32) string  { return fmt.Sprintf("%s@%d", player.EventPlaybackPause, ms) }

// fillEvents leaves the event channel full, so the next emit is dropped.
func fillEvents(p *KuwoPlayer) {
	for len(p.EventCh) < cap(p.EventCh) {
		p.EventCh <- player.Event{Type: "filler"}
	}
}

func after(t0 time.Time, ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

// acceptLines delivers lyrics with one line at each of lineMs.
func acceptLines(s *session, at time.Time, lineMs ...int) {
	var lines []lyric.Line
	for _, l := range lineMs {
		lines = append(lines, lyric.Line{TimeMs: l, Text: "line"})
	}
	s.accept(assetResult{gen: s.gen, rid: s.rid, lines: lines}, at)
}

// The front end anchors on all_lyrics at 10000 and interpolates. None of these
// steps is a seek by CoreClock's own criterion (back, or more than elapsed +
// 250 ms ahead); the behind cases move the kernel 250 ms in 250 ms while e_cur
// grows.
//
// Mutation (verified): comparing with > instead of >=, or checking one side
// only, turns it red; judging on every tick turns "neither" red; dropping the
// LagAdopted trigger turns "first adoption, no change" red.
func TestLagNoticeThreshold(t *testing.T) {
	cases := []struct {
		name    string
		at      int // ms after the anchor
		ms      int32
		shift   int32
		adopted bool
		want    bool
	}{
		{"ahead 220", 50, 10270, 220, false, true},
		{"ahead 200", 50, 10250, 200, false, true},
		{"ahead 199", 50, 10249, 199, false, false},
		{"behind 200", 250, 10050, -200, false, true},
		{"behind 199", 250, 10051, -199, false, false},
		{"first adoption, no change", 50, 10270, 0, true, true},
		{"neither", 50, 10270, 0, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New(0, 50)
			s := newSession(p)
			t0 := time.Now()
			observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
			drainEvents(p)
			observedLag(s, "MUSIC_1", 1, c.ms, after(t0, c.at), c.shift, c.adopted)
			got := published(drainEvents(p))
			var want []string
			if c.want {
				want = []string{resumeAt(c.ms)}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

// Two decreases, +150 and then +100 ms, with no positioned event between: on
// the second the public position is 250 ms ahead of the front-end model. A
// positioned event enqueued between anchors the model at the position it
// carries, leaving 100 ms; position_sample is internal and does not anchor.
//
// Mutation (verified): emitPos not anchoring the model turns the four anchored
// cases red.
func TestLagNoticeAnchors(t *testing.T) {
	cases := []struct {
		name          string
		first, second int      // status of t0 and t1; of the middle tick and t2
		ms            [4]int32 // positions of t0, t1, the middle tick and t2
		between       func(s *session, at time.Time)
		mark          string // event the middle tick must emit
		want          int
	}{
		{"no positioned event", 1, 1, [4]int32{10000, 10200, 10250, 10400}, nil, "", 1},
		{"position_sample only", 1, 1, [4]int32{10000, 10200, 10250, 10400},
			func(s *session, at time.Time) { s.p.EmitPositionSample(at, s.clock.Position(), s.duration) },
			player.EventPositionSample, 1},
		{"all_lyrics", 1, 1, [4]int32{10000, 10200, 10250, 10400},
			func(s *session, _ time.Time) { s.emitAll() }, player.EventAllLyrics, 0},
		{"lyric_update", 1, 1, [4]int32{10000, 10200, 10250, 10400},
			func(s *session, at time.Time) { s.emitLine(at) }, player.EventLyricUpdate, 0},
		{"playback_pause", 1, 2, [4]int32{10000, 10200, 10250, 10350}, nil, player.EventPlaybackPause, 0},
		{"playback_resume", 2, 1, [4]int32{10000, 10150, 10200, 10350}, nil, player.EventPlaybackResume, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New(0, 50)
			s := newSession(p)
			t0 := time.Now()
			observedLag(s, "MUSIC_1", c.first, c.ms[0], t0, 0, false)
			observedLag(s, "MUSIC_1", c.first, c.ms[1], after(t0, 50), 150, false)
			drainEvents(p)
			observedLag(s, "MUSIC_1", c.second, c.ms[2], after(t0, 100), 0, false)
			if c.between != nil {
				c.between(s, after(t0, 100))
			}
			mid := drainEvents(p)
			if c.mark != "" && len(ofType(mid, c.mark)) == 0 {
				t.Fatalf("middle tick did not emit %s: %+v", c.mark, mid)
			}
			if c.want == 1 && len(positioned(mid)) != 0 {
				t.Fatalf("middle tick emitted positioned events: %+v", mid)
			}
			observedLag(s, "MUSIC_1", c.second, c.ms[3], after(t0, 150), 100, false)
			if got := len(ofType(drainEvents(p), player.EventPlaybackResume)); got != c.want {
				t.Fatalf("%d resumes on the second decrease, want %d", got, c.want)
			}
		})
	}
}

// The model follows the events the front end receives, by the contract
// (doc/API_RESPONSE_EXAMPLES.md "时间插值"), not the player's own state
// (design §4, review round 15).
//
// Mutation (verified): the model following the player's playing state turns
// "all_lyrics while paused" red; lyric_update starting the interpolation turns
// "lyric_update while paused" red.
func TestLagNoticeFollowsEvents(t *testing.T) {
	// Paused at 9850; lyrics arrive and all_lyrics goes out with no
	// playback_pause after it, so the front end interpolates from it. 500 ms
	// later the correction drops 150 ms: the front end is at 10350, the public
	// position 10000.
	t.Run("all_lyrics while paused", func(t *testing.T) {
		for _, lyrics := range []bool{true, false} {
			p := New(0, 50)
			s := newSession(p)
			t0 := time.Now()
			observedLag(s, "MUSIC_1", 1, 9800, t0, 0, false)
			observedLag(s, "MUSIC_1", 2, 9850, after(t0, 50), 0, false)
			if lyrics {
				acceptLines(s, after(t0, 50), 1000)
			}
			if got := published(drainEvents(p)); !slices.Equal(got, []string{pauseAt(9850)}) {
				t.Fatalf("lyrics %v: setup published %v", lyrics, got)
			}
			observedLag(s, "MUSIC_1", 2, 10000, after(t0, 550), 150, false)
			var want []string
			if lyrics {
				want = []string{resumeAt(10000), pauseAt(10000)}
			}
			if got := published(drainEvents(p)); !slices.Equal(got, want) {
				t.Fatalf("lyrics %v: got %v, want %v", lyrics, got, want)
			}
		}
	})
	// A seek while paused publishes resume then pause; with one slot left in the
	// channel the pause is dropped (#134), leaving the front end interpolating
	// from the resume. It is corrected not by another resume (that would restart
	// it) but by resending the pause once the channel drains: while pausePending
	// is set the notice-triggered resume is held back, and resendPauseIfPending
	// retries the pause at the current position. The adoption tick therefore
	// publishes only the resent pause, and the front end stops.
	//
	// Mutation (verified): dropping the !s.pausePending gate makes the adoption
	// tick publish a resume again (red); a no-op resendPauseIfPending makes it
	// publish nothing and leaves the front end running (red).
	t.Run("resume enqueued, pause dropped", func(t *testing.T) {
		for _, drop := range []bool{true, false} {
			p := New(0, 50)
			s := newSession(p)
			t0 := time.Now()
			observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
			observedLag(s, "MUSIC_1", 2, 10050, after(t0, 50), 0, false)
			drainEvents(p)
			if drop {
				fillEvents(p)
				<-p.EventCh
			}
			observedLag(s, "MUSIC_1", 2, 30000, after(t0, 100), 0, false)
			want := []string{resumeAt(30000), pauseAt(30000)}
			if got := published(drainEvents(p)); drop && !slices.Equal(got, want[:1]) || !drop && !slices.Equal(got, want) {
				t.Fatalf("drop %v: the seek published %v", drop, got)
			}
			observedLag(s, "MUSIC_1", 2, 30000, after(t0, 350), 0, true)
			s.resendPauseIfPending() // runSession calls this each tick after observe
			got := published(drainEvents(p))
			if drop && !slices.Equal(got, want[1:]) || !drop && got != nil {
				t.Fatalf("drop %v: the adoption tick published %v", drop, got)
			}
			if s.front.running {
				t.Fatalf("drop %v: front end still running after the pause", drop)
			}
		}
	})
	// A line change while paused anchors the front end but does not start it.
	t.Run("lyric_update while paused", func(t *testing.T) {
		p := New(0, 50)
		s := newSession(p)
		t0 := time.Now()
		observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
		acceptLines(s, t0, 1000, 10100)
		observedLag(s, "MUSIC_1", 2, 10050, after(t0, 50), 0, false)
		s.emitLine(after(t0, 50))
		drainEvents(p)
		observedLag(s, "MUSIC_1", 2, 10150, after(t0, 100), 100, false)
		s.emitLine(after(t0, 100))
		ev := drainEvents(p)
		if lu := ofType(ev, player.EventLyricUpdate); len(lu) != 1 || eventMs(lu[0]) != 10150 || published(ev) != nil {
			t.Fatalf("want one lyric_update at 10150 and nothing published, got %+v", ev)
		}
		observedLag(s, "MUSIC_1", 2, 10150, after(t0, 1100), 0, true)
		if got := published(drainEvents(p)); got != nil {
			t.Fatalf("the front end stopped at 10150 was told %v", got)
		}
	})
	// Paused: the model stays at the pause anchor, so a change of 200 ms is
	// enough; resume then pause, at the same position, as a seek while paused.
	t.Run("stopped at the pause anchor", func(t *testing.T) {
		for _, c := range []struct {
			ms   int32
			want []string
		}{{10250, []string{resumeAt(10250), pauseAt(10250)}}, {10249, nil}} {
			p := New(0, 50)
			s := newSession(p)
			t0 := time.Now()
			observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
			observedLag(s, "MUSIC_1", 2, 10050, after(t0, 50), 0, false)
			drainEvents(p)
			observedLag(s, "MUSIC_1", 2, c.ms, after(t0, 100), c.ms-10050, false)
			if got := published(drainEvents(p)); !slices.Equal(got, c.want) {
				t.Fatalf("at %d: got %v, want %v", c.ms, got, c.want)
			}
		}
	})
}

// Until a positioned event of the song is enqueued the front end has no anchor
// for it, and a correction change is not judged; the first one carries the
// corrected position and the model starts from it.
//
// Mutation (verified): judging without an anchor turns it red.
func TestLagNoticeNoAnchorYet(t *testing.T) {
	p := New(0, 50)
	s := newSession(p)
	t0 := time.Now()
	fillEvents(p)
	observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
	drainEvents(p)
	observedLag(s, "MUSIC_1", 1, 10270, after(t0, 50), 220, false)
	if got := published(drainEvents(p)); got != nil {
		t.Fatalf("no anchor yet: published %v", got)
	}
	observedLag(s, "MUSIC_1", 1, 5000, after(t0, 100), 0, false)
	observedLag(s, "MUSIC_1", 1, 5270, after(t0, 150), 220, false)
	if got := published(drainEvents(p)); !slices.Equal(got, []string{resumeAt(5000), resumeAt(5270)}) {
		t.Fatalf("after the seek: got %v", got)
	}
}

// A dropped event (full channel) never reached the front end: it neither
// anchors the model nor delivers the notice.
//
// Mutation (verified): dropped events anchoring the model turns "dropped
// lyric_update" red; dropped events clearing the notice, "dropped resume".
func TestLagNoticeQueueFull(t *testing.T) {
	t.Run("dropped lyric_update", func(t *testing.T) {
		p := New(0, 50)
		s := newSession(p)
		t0 := time.Now()
		observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
		drainEvents(p)
		observedLag(s, "MUSIC_1", 1, 10200, after(t0, 50), 150, false)
		fillEvents(p)
		s.emitLine(after(t0, 50))
		if n := len(ofType(drainEvents(p), player.EventLyricUpdate)); n != 0 {
			t.Fatalf("lyric_update was not dropped: %d", n)
		}
		observedLag(s, "MUSIC_1", 1, 10350, after(t0, 100), 100, false)
		if got := published(drainEvents(p)); !slices.Equal(got, []string{resumeAt(10350)}) {
			t.Fatalf("after the dropped lyric_update: got %v", got)
		}
	})
	t.Run("dropped resume", func(t *testing.T) {
		p := New(0, 50)
		s := newSession(p)
		t0 := time.Now()
		observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
		drainEvents(p)
		fillEvents(p)
		observedLag(s, "MUSIC_1", 1, 10270, after(t0, 50), 220, false)
		if n := len(ofType(drainEvents(p), player.EventPlaybackResume)); n != 0 {
			t.Fatalf("resume was not dropped: %d", n)
		}
		for i, want := range [][]string{{resumeAt(10320)}, nil} {
			observedLag(s, "MUSIC_1", 1, 10320+int32(i)*50, after(t0, 100+50*i), 0, false)
			if got := published(drainEvents(p)); !slices.Equal(got, want) {
				t.Fatalf("tick %d after the drop: got %v, want %v", i+1, got, want)
			}
		}
	})
}

// A seek by CoreClock and the notice on the same tick publish one resume.
func TestLagNoticeSameTickAsSeek(t *testing.T) {
	p := New(0, 50)
	s := newSession(p)
	t0 := time.Now()
	observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
	drainEvents(p)
	observedLag(s, "MUSIC_1", 1, 5000, after(t0, 50), 300, true)
	if got := published(drainEvents(p)); !slices.Equal(got, []string{resumeAt(5000)}) {
		t.Fatalf("got %v, want one resume", got)
	}
}

// A song change publishes no resume, even with a notice of the previous song
// pending and the change tick carrying a shift and an adoption. The new song's
// model starts from its all_lyrics, or not at all when that was dropped.
//
// Mutation (verified): keeping the pending notice or the previous song's model
// across the change turns it red.
func TestLagNoticeSongChange(t *testing.T) {
	for _, full := range []bool{false, true} {
		p := New(0, 50)
		s := newSession(p)
		t0 := time.Now()
		observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
		drainEvents(p)
		fillEvents(p)
		observedLag(s, "MUSIC_1", 1, 10270, after(t0, 50), 220, false)
		drainEvents(p)
		if full {
			fillEvents(p)
		}
		if !observedLag(s, "MUSIC_2", 1, 2000, after(t0, 100), 300, true) {
			t.Fatal("song change not detected")
		}
		if got := published(drainEvents(p)); got != nil {
			t.Fatalf("full=%v: the change tick published %v", full, got)
		}
		observedLag(s, "MUSIC_2", 1, 2150, after(t0, 300), 100, false)
		if got := published(drainEvents(p)); got != nil {
			t.Fatalf("full=%v: the new song's first tick published %v", full, got)
		}
	}
}
