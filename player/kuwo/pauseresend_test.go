package kuwo

// This file tests the dropped-playback_pause resend (issue #134,
// doc/design/event-drop-notice-and-resend-design.md §3.2/§3.3/§4). A pause that
// the full channel drops leaves pausePending set; resendPauseIfPending, which
// runSession calls each tick after observe, resends it at the current clock
// position through emitPos until it enqueues, while a notice-triggered resume is
// held back. observe is driven directly here; the resend wiring is simulated by
// calling s.resendPauseIfPending() where runSession would.

import (
	"slices"
	"testing"
	"time"
)

// The resent pause carries the current clock position, and the front-end model
// anchors on that same position (design §4 #134/model-anchor, position): a pause
// dropped at 10000, then a tick still paused at 10050 (crept 50 ms, below the
// seek threshold, so observe sends no new pause), resends a pause at 10050 with
// the model anchored there too.
//
// Mutation (verified): a resend that carries a stored old position (10000)
// instead of the current clock turns the position assertion red; a resend that
// does not anchor leaves front.ms wrong.
func TestPauseResendModelAnchor(t *testing.T) {
	p := New(0, 50)
	s := newSession(p)
	t0 := time.Now()
	observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
	drainEvents(p)
	fillEvents(p)
	observedLag(s, "MUSIC_1", 2, 10000, after(t0, 50), 0, false) // pause dropped
	drainEvents(p)
	if !s.pausePending {
		t.Fatal("a dropped pause must set pausePending")
	}
	observedLag(s, "MUSIC_1", 2, 10050, after(t0, 100), 0, false) // position crept +50 ms
	s.resendPauseIfPending()
	if got := published(drainEvents(p)); !slices.Equal(got, []string{pauseAt(10050)}) {
		t.Fatalf("the resend published %v, want pause@10050", got)
	}
	if s.front.ms != 10050 || s.front.running {
		t.Fatalf("front anchored at ms=%d running=%v, want 10050 stopped", s.front.ms, s.front.running)
	}
	if s.pausePending {
		t.Fatal("a successful resend must clear pausePending")
	}
}

// A real seek by CoreClock while a pause is pending still publishes resume then
// pause at the new position (design §4 #134/real-seek): the !pausePending gate
// holds only the notice-triggered resume, not a seek the user actually made.
//
// Mutation (verified): gating the real seek on pausePending too (holding the
// CoreClock seek) drops the resume, turning this red.
func TestPauseResendRealSeekStillResumes(t *testing.T) {
	p := New(0, 50)
	s := newSession(p)
	t0 := time.Now()
	observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
	drainEvents(p)
	fillEvents(p)
	observedLag(s, "MUSIC_1", 2, 10000, after(t0, 50), 0, false) // pause dropped
	drainEvents(p)
	if !s.pausePending {
		t.Fatal("a dropped pause must set pausePending")
	}
	observedLag(s, "MUSIC_1", 2, 50000, after(t0, 100), 0, false) // real seek while paused
	s.resendPauseIfPending()
	if got := published(drainEvents(p)); !slices.Equal(got, []string{resumeAt(50000), pauseAt(50000)}) {
		t.Fatalf("the real seek published %v, want resume+pause at 50000", got)
	}
	if s.pausePending {
		t.Fatal("the new pause enqueued: pausePending must clear")
	}
}

// Playback resuming before the resend clears pausePending and sends no stale
// pause (design §4 #134/recover): observe publishes the resume and clears the
// pending pause; resendPauseIfPending, guarded on !playing, does nothing.
//
// Mutation (verified): dropping the "if playing { pausePending = false }" clear
// leaves pausePending set, turning the final assertion red.
func TestPauseResendClearedByResume(t *testing.T) {
	p := New(0, 50)
	s := newSession(p)
	t0 := time.Now()
	observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
	drainEvents(p)
	fillEvents(p)
	observedLag(s, "MUSIC_1", 2, 10000, after(t0, 50), 0, false) // pause dropped
	drainEvents(p)
	if !s.pausePending {
		t.Fatal("a dropped pause must set pausePending")
	}
	observedLag(s, "MUSIC_1", 1, 10000, after(t0, 100), 0, false) // playback resumed
	s.resendPauseIfPending()
	if got := published(drainEvents(p)); !slices.Equal(got, []string{resumeAt(10000)}) {
		t.Fatalf("the resume tick published %v, want just a resume", got)
	}
	if s.pausePending {
		t.Fatal("resuming playback must clear pausePending")
	}
}

// A song change before the resend clears pausePending with the session reset and
// sends no stale pause from the previous song (design §4 #134/song-change).
func TestPauseResendClearedBySongChange(t *testing.T) {
	p := New(0, 50)
	s := newSession(p)
	t0 := time.Now()
	observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
	drainEvents(p)
	fillEvents(p)
	observedLag(s, "MUSIC_1", 2, 10000, after(t0, 50), 0, false) // pause dropped
	drainEvents(p)
	if !s.pausePending {
		t.Fatal("a dropped pause must set pausePending")
	}
	if !observedLag(s, "MUSIC_2", 1, 2000, after(t0, 100), 0, false) {
		t.Fatal("song change not detected")
	}
	s.resendPauseIfPending()
	if s.pausePending {
		t.Fatal("a song change must clear pausePending")
	}
	if got := published(drainEvents(p)); got != nil {
		t.Fatalf("the song change published a stale pause: %v", got)
	}
}

// A pause that enqueues on the first try never sets pausePending, and later
// paused ticks resend nothing (design §4 #134/first-success).
func TestPauseResendFirstTrySucceeds(t *testing.T) {
	p := New(0, 50)
	s := newSession(p)
	t0 := time.Now()
	observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
	drainEvents(p)
	observedLag(s, "MUSIC_1", 2, 10000, after(t0, 50), 0, false) // pause enqueues
	if s.pausePending {
		t.Fatal("an enqueued pause must not set pausePending")
	}
	if got := published(drainEvents(p)); !slices.Equal(got, []string{pauseAt(10000)}) {
		t.Fatalf("published %v, want pause@10000", got)
	}
	observedLag(s, "MUSIC_1", 2, 10050, after(t0, 100), 0, false)
	s.resendPauseIfPending()
	if got := published(drainEvents(p)); got != nil {
		t.Fatalf("nothing was dropped, so nothing to resend, got %v", got)
	}
}

// While the channel stays full the per-tick retry is a non-blocking TryEmit that
// fails, so the pause stays pending and nothing is resumed in its place (the
// notice-triggered resume is held by !pausePending); once the channel drains the
// retry enqueues (design §4 #134/channel-full).
//
// Mutation (verified): dropping the !pausePending gate lets the drifting
// notice try to resume while full; removing the resend never clears pausePending
// after the drain.
func TestPauseResendChannelStaysFull(t *testing.T) {
	p := New(0, 50)
	s := newSession(p)
	t0 := time.Now()
	observedLag(s, "MUSIC_1", 1, 10000, t0, 0, false)
	drainEvents(p)
	fillEvents(p)
	observedLag(s, "MUSIC_1", 2, 10000, after(t0, 50), 0, false) // pause dropped
	if !s.pausePending {
		t.Fatal("a dropped pause must set pausePending")
	}
	// Channel still full across several ticks that carry an adoption (which would
	// set a notice): the pause stays pending, nothing is enqueued, and the front
	// end keeps running because no pause reached it.
	for i := 1; i <= 3; i++ {
		observedLag(s, "MUSIC_1", 2, 10000, after(t0, 50+i*50), 0, true)
		s.resendPauseIfPending()
		if !s.pausePending {
			t.Fatalf("tick %d: channel full, pause must stay pending", i)
		}
		if !s.front.running {
			t.Fatalf("tick %d: nothing enqueued, the front end must stay running", i)
		}
	}
	drainEvents(p)
	observedLag(s, "MUSIC_1", 2, 10000, after(t0, 300), 0, false)
	s.resendPauseIfPending()
	if s.pausePending {
		t.Fatal("after the channel drained, the resend must succeed")
	}
	if got := published(drainEvents(p)); !slices.Equal(got, []string{pauseAt(10000)}) {
		t.Fatalf("after the drain, published %v, want pause@10000", got)
	}
}
