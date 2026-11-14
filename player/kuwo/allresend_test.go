package kuwo

// This file tests the dropped-all_lyrics resend (issue #178). emitAll records
// in allSent whether its all_lyrics was enqueued; observe sends a dropped one
// again on each later tick, at that tick's position and with the song's current
// lines and duration, before the tick's resume and pause, until it enqueues.
// Until then emitLine sends nothing, so no lyric_update goes out ahead of the
// all_lyrics it belongs to; once it enqueues, the current line goes out again.
// A resend that enqueues while paused is followed by a playback_pause, which
// resendPauseIfPending retries like a dropped one (#134); an all_lyrics sent the
// first time is not. Ticks run in runSession's order: observe, accept when a
// fetch result arrives, emitLine, resendPauseIfPending.

import (
	"cmp"
	"fmt"
	"slices"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

// chState is the event channel during one phase of a tick.
type chState int

const (
	room    chState = iota // emits enqueue
	full                   // emits are dropped
	oneSlot                // the first emit enqueues, the rest are dropped
)

// allStep is one poll, 50 ms after the previous one.
type allStep struct {
	rid    string       // the song; MUSIC_1 when empty
	status int          // 1 playing, 2 paused
	ms     int32        // position
	durMs  int32        // duration; 100000 when 0
	result *fetchResult // delivered between observe and emitLine
	ch     [3]chState   // during observe, accept, and emitLine with resendPauseIfPending
	want   []string     // the positioned events the tick enqueues, in order
}

// allAt is an all_lyrics with count lines at ms; lineAt a lyric_update of index.
func allAt(count int, ms int32) string {
	return fmt.Sprintf("%s/%d@%d", player.EventAllLyrics, count, ms)
}
func lineAt(index int, ms int32) string {
	return fmt.Sprintf("%s/%d@%d", player.EventLyricUpdate, index, ms)
}

// setChannel sets the drained channel to c.
func setChannel(p *KuwoPlayer, c chState) {
	if c != room {
		fillEvents(p)
	}
	if c == oneSlot {
		<-p.EventCh
	}
}

// drainReal drains the channel, leaving out fillEvents' fillers.
func drainReal(p *KuwoPlayer) []player.Event {
	var out []player.Event
	for _, e := range drainEvents(p) {
		if e.Type != "filler" {
			out = append(out, e)
		}
	}
	return out
}

// runAllSteps plays steps on a new session and checks each tick's positioned
// events. Every positioned event of a tick carries the tick's position; an
// all_lyrics also the tick's song title and duration, with the progress that
// position gives. The front-end model is anchored at the tick's position when
// the tick enqueues a positioned event; otherwise it does not move, or on a song
// change has no anchor.
func runAllSteps(t *testing.T, steps []allStep) *session {
	t.Helper()
	p := New(0, 50)
	s := newSession(p)
	t0 := time.Now()
	for k, st := range steps {
		at := after(t0, 50*k)
		rid, dur := cmp.Or(st.rid, "MUSIC_1"), cmp.Or(st.durMs, 100000)
		front := s.front
		var ev []player.Event
		setChannel(p, st.ch[0])
		changed := s.observe(cdp.Snapshot{RID: rid, Title: "song " + rid, Artist: "artist", Status: st.status},
			pos.Sample{RID: rid, PositionMs: st.ms, DurationMs: dur, ObservedAt: at})
		ev = append(ev, drainReal(p)...)
		setChannel(p, st.ch[1])
		if st.result != nil {
			s.accept(assetResult{gen: s.gen, rid: s.rid, lines: st.result.lines, err: st.result.err}, at)
		}
		ev = append(ev, drainReal(p)...)
		setChannel(p, st.ch[2])
		s.emitLine(at)
		s.resendPauseIfPending()
		ev = append(ev, drainReal(p)...)

		var got []string
		for _, e := range positioned(ev) {
			ms := eventMs(e)
			if ms != st.ms {
				t.Fatalf("tick %d: %s at %d, want %d", k, e.Type, ms, st.ms)
			}
			switch d := e.Data.(type) {
			case *player.AllLyricsData:
				if want := "song " + rid + " - artist"; d.Title != want || d.Duration != float32(dur)/1000 || d.Progress != player.ClampProgress(d.Position, d.Duration) || d.Progress == 0 {
					t.Fatalf("tick %d: all_lyrics %q duration %v progress %v; want %q, %v, the position's", k, d.Title, d.Duration, d.Progress, want, float32(dur)/1000)
				}
				got = append(got, allAt(d.Count, ms))
			case *player.LyricUpdate:
				got = append(got, lineAt(d.Index, ms))
			default:
				got = append(got, fmt.Sprintf("%s@%d", e.Type, ms))
			}
		}
		if !slices.Equal(got, st.want) {
			t.Fatalf("tick %d: enqueued %v, want %v", k, got, st.want)
		}
		if changed && got == nil {
			front = frontModel{}
		}
		if got == nil && s.front != front || got != nil && s.front.ms != st.ms {
			t.Fatalf("tick %d: front end %+v (was %+v) after %v", k, s.front, front, got)
		}
		// A pending pause while playing would hold back notice-triggered resumes.
		if s.playing && s.pausePending {
			t.Fatalf("tick %d: pausePending while playing", k)
		}
	}
	return s
}

var (
	noDrop          = [3]chState{}
	dropObserve     = [3]chState{full, room, room}
	dropAccept      = [3]chState{room, full, room}
	dropAll         = [3]chState{full, full, full}
	lastSlotObserve = [3]chState{oneSlot, full, full} // the first emit of observe enqueues, nothing after it
)

// A dropped all_lyrics is sent again on the next tick with room, once, at that
// tick's position and with the song's lines and duration at that tick, before
// the tick's resume and pause; no lyric_update of the song goes out before it
// enqueues, and the current line goes out after it. Enqueued while paused, it is
// followed by a pause unless the tick sends its own; enqueued while playing, by
// none.
//
// Mutation (verified): the pre-#178 session.go turns every case red but "change
// tick dropped, lyrics enqueue first", where the lyrics' all_lyrics makes up for
// the drop, as before; keeping allSent from emitPos but dropping the !allSent
// retry in observe, the same cases but "song change before the resend";
// dropping the !allSent check from emitLine, the eleven cases where emitLine has
// room on the tick the all_lyrics was dropped (lyric_update ahead of it); moving
// the retry to the start of emitLine, every case: it resends on the drop tick
// when emitLine has room, and "paused on the resend tick" sends the pause before
// the all_lyrics. No pause after a resend while paused turns "lyrics dropped
// while paused", "pause enqueued before the resend", "change tick dropped while
// paused" and "pause after the resend dropped" red; one after a resend while
// playing too leaves pausePending set while playing in every case that resends
// while playing. Setting lineIndex to -2 only when the all_lyrics enqueues
// changes nothing (emitLine sends nothing until it does); not setting it at all
// turns "duration change dropped" red.
func TestDroppedAllLyricsResent(t *testing.T) {
	cases := []struct {
		name  string
		steps []allStep
	}{
		// The change tick's all_lyrics, with no lines yet, goes out on the next
		// tick; the lyrics' all_lyrics follows when they arrive, and nothing more.
		{"change tick dropped, lyrics later", []allStep{
			{"", 1, 1000, 0, nil, dropObserve, nil},
			{"", 1, 1050, 0, nil, noDrop, []string{allAt(0, 1050)}},
			{"", 1, 1100, 0, nil, noDrop, nil},
			{"", 1, 1150, 0, &gotLyrics, noDrop, []string{allAt(2, 1150)}},
			{"", 1, 1200, 0, nil, noDrop, nil},
		}},
		// The lyrics arrive on the tick the resend goes out: the resend, in
		// observe, carries no lines yet, and the lyrics' all_lyrics follows it.
		{"lyrics on the first retry tick", []allStep{
			{"", 1, 12000, 0, nil, dropObserve, nil},
			{"", 1, 12050, 0, &gotLyrics, noDrop, []string{allAt(0, 12050), allAt(2, 12050), lineAt(0, 12050)}},
			{"", 1, 12100, 0, nil, noDrop, nil},
		}},
		// While the change tick's all_lyrics is still pending, the lyrics arrive
		// and their all_lyrics enqueues: nothing is resent after it.
		{"change tick dropped, lyrics enqueue first", []allStep{
			{"", 1, 12000, 0, nil, dropObserve, nil},
			{"", 1, 12050, 0, &gotLyrics, [3]chState{full, room, room}, []string{allAt(2, 12050), lineAt(0, 12050)}},
			{"", 1, 12100, 0, nil, noDrop, nil},
		}},
		// The lyrics' all_lyrics is dropped and emitLine has room on that tick:
		// the current line waits for the resend, which goes out at the next
		// tick's position, the line after it.
		{"lyrics dropped", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 1, 12050, 0, &gotLyrics, dropAccept, nil},
			{"", 1, 12100, 0, nil, noDrop, []string{allAt(2, 12100), lineAt(0, 12100)}},
			{"", 1, 12150, 0, nil, noDrop, nil},
		}},
		{"lyrics dropped in the prelude", []allStep{
			{"", 1, 1000, 0, nil, noDrop, []string{allAt(0, 1000)}},
			{"", 1, 1050, 0, &gotLyrics, dropAccept, nil},
			{"", 1, 1100, 0, nil, noDrop, []string{allAt(2, 1100)}},
			{"", 1, 1150, 0, nil, noDrop, nil},
		}},
		// Resent while paused: a pause follows the all_lyrics and the current
		// line, so the front end stops again.
		{"lyrics dropped while paused", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 2, 12050, 0, nil, noDrop, []string{pauseAt(12050)}},
			{"", 2, 12050, 0, &gotLyrics, dropAccept, nil},
			{"", 2, 12050, 0, nil, noDrop, []string{allAt(2, 12050), lineAt(0, 12050), pauseAt(12050)}},
			{"", 2, 12050, 0, nil, noDrop, nil},
		}},
		// The pause turns up while the all_lyrics is pending; observe drops the
		// resend, the paused status and the pause, and resendPauseIfPending sends
		// the pause at the end of the tick. The resend on the next tick restarted
		// the front end, so a pause follows it again.
		{"pause enqueued before the resend", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 1, 12050, 0, &gotLyrics, dropAccept, nil},
			{"", 2, 12100, 0, nil, dropObserve, []string{pauseAt(12100)}},
			{"", 2, 12100, 0, nil, noDrop, []string{allAt(2, 12100), lineAt(0, 12100), pauseAt(12100)}},
			{"", 2, 12100, 0, nil, noDrop, nil},
		}},
		// A song changed while paused gets no pause with its all_lyrics (the
		// status says paused); resent later, its all_lyrics does.
		{"change tick dropped while paused", []allStep{
			{"", 2, 3000, 0, nil, dropObserve, nil},
			{"", 2, 3000, 0, nil, noDrop, []string{allAt(0, 3000), pauseAt(3000)}},
			{"", 2, 3000, 0, nil, noDrop, nil},
		}},
		// The pause after the resend is dropped: it is retried each tick, after
		// the current line, and the all_lyrics is not repeated.
		{"pause after the resend dropped", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 2, 12050, 0, nil, noDrop, []string{pauseAt(12050)}},
			{"", 2, 12050, 0, &gotLyrics, dropAccept, nil},
			{"", 2, 12050, 0, nil, [3]chState{room, room, full}, []string{allAt(2, 12050)}},
			{"", 2, 12050, 0, nil, dropAll, nil},
			{"", 2, 12050, 0, nil, dropAll, nil},
			{"", 2, 12050, 0, nil, noDrop, []string{lineAt(0, 12050), pauseAt(12050)}},
			{"", 2, 12050, 0, nil, noDrop, nil},
		}},
		// Playback resumes before the pause after the resend enqueues: the resume
		// goes out and the stale pause does not.
		{"playing before the pause after the resend", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 2, 12050, 0, nil, noDrop, []string{pauseAt(12050)}},
			{"", 2, 12050, 0, &gotLyrics, dropAccept, nil},
			{"", 2, 12050, 0, nil, [3]chState{room, room, full}, []string{allAt(2, 12050)}},
			{"", 1, 12100, 0, nil, noDrop, []string{resumeAt(12100), lineAt(0, 12100)}},
			{"", 1, 12150, 0, nil, noDrop, nil},
		}},
		// The channel stays full: every tick retries and nothing else moves; the
		// first tick with room sends it once.
		{"channel stays full", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 1, 12050, 0, &gotLyrics, dropAll, nil},
			{"", 1, 12100, 0, nil, dropAll, nil},
			{"", 1, 12150, 0, nil, dropAll, nil},
			{"", 1, 12200, 0, nil, noDrop, []string{allAt(2, 12200), lineAt(0, 12200)}},
			{"", 1, 12250, 0, nil, noDrop, nil},
			{"", 1, 12300, 0, nil, noDrop, nil},
		}},
		// The resend takes the last slot and the current line is dropped: the
		// line goes out on the next tick (#175), the all_lyrics is not repeated.
		{"resend takes the last slot", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 1, 12050, 0, &gotLyrics, dropAccept, nil},
			{"", 1, 12100, 0, nil, lastSlotObserve, []string{allAt(2, 12100)}},
			{"", 1, 12150, 0, nil, noDrop, []string{lineAt(0, 12150)}},
			{"", 1, 12200, 0, nil, noDrop, nil},
		}},
		// The pause turns up on the resend tick: all_lyrics goes out before it,
		// so the front end ends up stopped.
		{"paused on the resend tick", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 1, 12050, 0, &gotLyrics, dropAll, nil},
			{"", 2, 12100, 0, nil, noDrop, []string{allAt(2, 12100), pauseAt(12100), lineAt(0, 12100)}},
			{"", 2, 12100, 0, nil, noDrop, nil},
		}},
		// The pause is dropped along with the resend (#134): both go out on the
		// next tick with room, the all_lyrics first, the pause last.
		{"pause dropped with it", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 1, 12050, 0, &gotLyrics, dropAccept, nil},
			{"", 2, 12100, 0, nil, dropAll, nil},
			{"", 2, 12100, 0, nil, noDrop, []string{allAt(2, 12100), lineAt(0, 12100), pauseAt(12100)}},
			{"", 2, 12100, 0, nil, noDrop, nil},
		}},
		// No lyrics: the change tick's all_lyrics goes out before index:-1.
		{"change tick dropped, no lyrics", []allStep{
			{"", 1, 1000, 0, nil, dropObserve, nil},
			{"", 1, 1050, 0, &gotNoLyrics, dropAll, nil},
			{"", 1, 1100, 0, nil, noDrop, []string{allAt(0, 1100), lineAt(-1, 1100)}},
			{"", 1, 1150, 0, nil, noDrop, nil},
		}},
		{"zero lines dropped", []allStep{
			{"", 1, 1000, 0, nil, noDrop, []string{allAt(0, 1000)}},
			{"", 1, 1050, 0, &gotNone, dropAccept, nil},
			{"", 1, 1100, 0, nil, noDrop, []string{allAt(0, 1100), lineAt(-1, 1100)}},
			{"", 1, 1150, 0, nil, noDrop, nil},
		}},
		// A duration change's all_lyrics is resent with the new duration, the
		// current line after it.
		{"duration change dropped", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 1, 12050, 0, &gotLyrics, noDrop, []string{allAt(2, 12050), lineAt(0, 12050)}},
			{"", 1, 12100, 200000, nil, dropObserve, nil},
			{"", 1, 12150, 200000, nil, noDrop, []string{allAt(2, 12150), lineAt(0, 12150)}},
			{"", 1, 12200, 200000, nil, noDrop, nil},
		}},
		// A song change before the resend: the new song's all_lyrics goes out,
		// with its own title and no lines, never the previous song's.
		{"song change before the resend", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 1, 12050, 0, &gotLyrics, dropAccept, nil},
			{"MUSIC_2", 1, 3000, 0, nil, noDrop, []string{allAt(0, 3000)}},
			{"MUSIC_2", 1, 3050, 0, nil, noDrop, nil},
		}},
		{"song change dropped too", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 1, 12050, 0, &gotLyrics, dropAccept, nil},
			{"MUSIC_2", 1, 3000, 0, nil, dropObserve, nil},
			{"MUSIC_2", 1, 3050, 0, nil, noDrop, []string{allAt(0, 3050)}},
			{"MUSIC_2", 1, 3100, 0, nil, noDrop, nil},
		}},
	}
	// The front end ends up stopped, its position no longer advancing, in every
	// case that ends paused; it runs in those that end playing.
	stopped := map[string]bool{
		"lyrics dropped while paused":      true,
		"pause enqueued before the resend": true,
		"change tick dropped while paused": true,
		"pause after the resend dropped":   true,
		"paused on the resend tick":        true,
		"pause dropped with it":            true,
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := runAllSteps(t, c.steps)
			if !s.allSent || s.front.running == stopped[c.name] {
				t.Fatalf("at the end: allSent %v, front end %+v", s.allSent, s.front)
			}
		})
	}
}

// With nothing dropped the behavior is as before #178: each all_lyrics goes out
// once, on the song change, the lyrics' arrival and a duration change, and is
// not repeated; paused or not, and no pause follows it while paused; with lines
// or without (index:-1).
//
// Mutation (verified): the pre-#178 session.go passes every case; a pause after
// any all_lyrics observe enqueues while paused turns "song change while paused"
// and "duration change while paused" red, one after the song change's alone
// (resend without !changed) the former; not setting lineIndex to -2 in emitAll
// turns both duration change cases red.
func TestEnqueuedAllLyricsNotResent(t *testing.T) {
	cases := []struct {
		name  string
		steps []allStep
	}{
		{"lyrics", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 1, 12050, 0, &gotLyrics, noDrop, []string{allAt(2, 12050), lineAt(0, 12050)}},
			{"", 1, 12100, 0, nil, noDrop, nil},
			{"", 1, 12150, 0, nil, noDrop, nil},
		}},
		{"lyrics while paused", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 2, 12050, 0, nil, noDrop, []string{pauseAt(12050)}},
			{"", 2, 12050, 0, &gotLyrics, noDrop, []string{allAt(2, 12050), lineAt(0, 12050)}},
			{"", 2, 12050, 0, nil, noDrop, nil},
			{"", 2, 12050, 0, nil, noDrop, nil},
		}},
		// An all_lyrics sent the first time while paused gets no pause: on the
		// song change, the lyrics' arrival or a duration change.
		{"song change while paused", []allStep{
			{"", 2, 3000, 0, nil, noDrop, []string{allAt(0, 3000)}},
			{"", 2, 3000, 0, &gotLyrics, noDrop, []string{allAt(2, 3000)}},
			{"", 2, 3000, 0, nil, noDrop, nil},
		}},
		{"duration change while paused", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 1, 12050, 0, &gotLyrics, noDrop, []string{allAt(2, 12050), lineAt(0, 12050)}},
			{"", 2, 12100, 0, nil, noDrop, []string{pauseAt(12100), lineAt(0, 12100)}},
			{"", 2, 12100, 200000, nil, noDrop, []string{allAt(2, 12100), lineAt(0, 12100)}},
			{"", 2, 12100, 200000, nil, noDrop, nil},
		}},
		{"no lyrics", []allStep{
			{"", 1, 1000, 0, nil, noDrop, []string{allAt(0, 1000)}},
			{"", 1, 1050, 0, &gotNoLyrics, noDrop, []string{lineAt(-1, 1050)}},
			{"", 1, 1100, 0, nil, noDrop, nil},
			{"", 1, 1150, 0, nil, noDrop, nil},
		}},
		{"no lyrics while paused", []allStep{
			{"", 1, 1000, 0, nil, noDrop, []string{allAt(0, 1000)}},
			{"", 2, 1050, 0, nil, noDrop, []string{pauseAt(1050)}},
			{"", 2, 1050, 0, &gotNoLyrics, noDrop, []string{lineAt(-1, 1050)}},
			{"", 2, 1050, 0, nil, noDrop, nil},
		}},
		{"duration change", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 1, 12050, 0, &gotLyrics, noDrop, []string{allAt(2, 12050), lineAt(0, 12050)}},
			{"", 1, 12100, 200000, nil, noDrop, []string{allAt(2, 12100), lineAt(0, 12100)}},
			{"", 1, 12150, 200000, nil, noDrop, nil},
		}},
		// A dropped lyric_update (#175) is resent alone; the all_lyrics is not.
		{"line dropped", []allStep{
			{"", 1, 12000, 0, nil, noDrop, []string{allAt(0, 12000)}},
			{"", 1, 12050, 0, &gotLyrics, [3]chState{room, room, full}, []string{allAt(2, 12050)}},
			{"", 1, 12100, 0, nil, noDrop, []string{lineAt(0, 12100)}},
			{"", 1, 12150, 0, nil, noDrop, nil},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runAllSteps(t, c.steps) })
	}
}
