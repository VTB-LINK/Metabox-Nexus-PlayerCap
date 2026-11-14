package kuwo

// This file tests lyric_update before the first line (issue #175). index:-1
// means the platform returned no lyrics (doc/openapi.yaml /lyric_update), so a
// song with lines sends no lyric_update before its first line, neither at the
// start nor after a seek back, as the other players. index:-1 waits for the
// lyric fetch to settle (lyrics, possibly none; ErrNoLyrics; or a failure with
// no retry left): nothing goes out on the song change tick, nor while the fetch
// is in flight or waiting for a retry. Ticks run in runSession's order: observe,
// accept when a fetch result arrives, emitLine, resendPauseIfPending.

import (
	"errors"
	"slices"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/lyric"
)

// lineTick runs one poll at atMs after t0 with the position at posMs and
// returns its events.
func lineTick(s *session, t0 time.Time, rid string, status, atMs int, posMs int32) []player.Event {
	at := after(t0, atMs)
	observedSong(s, rid, status, posMs, at)
	s.emitLine(at)
	s.resendPauseIfPending()
	return drainEvents(s.p)
}

// lyricsTick is lineTick with lines delivered between observe and emitLine.
func lyricsTick(s *session, t0 time.Time, rid string, atMs int, posMs int32, lines []lyric.Line, err error) []player.Event {
	at := after(t0, atMs)
	observedSong(s, rid, 1, posMs, at)
	s.accept(assetResult{gen: s.gen, rid: s.rid, lines: lines, err: err}, at)
	s.emitLine(at)
	return drainEvents(s.p)
}

// startSong plays rid from posMs at t0; its lines (one at each of lineMs)
// arrive on the next tick, 50 ms later. It returns the events from that tick.
func startSong(s *session, t0 time.Time, rid string, posMs int32, lineMs ...int) []player.Event {
	lineTick(s, t0, rid, 1, 0, posMs) // lyrics still being fetched
	return lyricsTick(s, t0, rid, 50, posMs+50, lyricLines(lineMs), nil)
}

// indexes lists the index of each lyric_update.
func indexes(events []player.Event) []int {
	var out []int
	for _, e := range ofType(events, player.EventLyricUpdate) {
		out = append(out, e.Data.(*player.LyricUpdate).Index)
	}
	return out
}

// From the lyrics' arrival to the first line nothing but all_lyrics anchors the
// front end; the first line goes out when it starts, position samples go on.
//
// Mutation (verified): the pre-#175 emitLine sends index:-1 on the arrival tick.
func TestPreludeSendsNoLyricUpdate(t *testing.T) {
	p := New(0, 50)
	s := newSession(p)
	t0 := time.Now()
	ev := startSong(s, t0, "MUSIC_1", 0, 10000, 20000)
	if all := ofType(ev, player.EventAllLyrics); len(all) != 1 || all[0].Data.(*player.AllLyricsData).Count != 2 || indexes(ev) != nil {
		t.Fatalf("arrival tick: want all_lyrics with 2 lines and no lyric_update, got %+v", ev)
	}
	if !s.front.ok || !s.front.running || s.front.ms != 50 {
		t.Fatalf("front end not anchored on all_lyrics: %+v", s.front)
	}
	samples := 0
	for ms := 100; ms < 10000; ms += 50 {
		ev := lineTick(s, t0, "MUSIC_1", 1, ms, int32(ms))
		if got := indexes(ev); got != nil {
			t.Fatalf("t=%d ms: lyric_update %v in the prelude", ms, got)
		}
		samples += len(ofType(ev, player.EventPositionSample))
	}
	if samples == 0 {
		t.Fatal("no position_sample in the prelude")
	}
	if got := indexes(lineTick(s, t0, "MUSIC_1", 1, 10000, 10000)); !slices.Equal(got, []int{0}) {
		t.Fatalf("first line: got %v, want [0]", got)
	}
}

// A seek back before the first line publishes the seek and no lyric_update;
// the first line goes out again when it starts, also when it was the line
// shown before the seek. A seek onto the first line sends it on the seek tick.
func TestPreludeAfterSeek(t *testing.T) {
	cases := []struct {
		name     string
		from, to int32 // positions before and after the seek
		paused   bool
		want     []string
		wantIdx  []int // on the seek tick
		fromLine int
	}{
		{"back from the second line", 25000, 5000, false, []string{resumeAt(5000)}, nil, 1},
		{"back from the first line", 12000, 5000, false, []string{resumeAt(5000)}, nil, 0},
		{"back while paused", 25000, 5000, true, []string{resumeAt(5000), pauseAt(5000)}, nil, 1},
		{"forward within the prelude", 2000, 8000, false, []string{resumeAt(8000)}, nil, -1},
		{"back onto the first line", 25000, 10000, false, []string{resumeAt(10000)}, []int{0}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New(0, 50)
			s := newSession(p)
			t0 := time.Now()
			ev := startSong(s, t0, "MUSIC_1", c.from-50, 10000, 20000)
			if c.fromLine >= 0 && !slices.Equal(indexes(ev), []int{c.fromLine}) {
				t.Fatalf("setup: lyric_update %v, want [%d]", indexes(ev), c.fromLine)
			}
			status := 1
			if c.paused {
				status = 2
				lineTick(s, t0, "MUSIC_1", 2, 100, c.from)
			}
			ev = lineTick(s, t0, "MUSIC_1", status, 150, c.to)
			if got := published(ev); !slices.Equal(got, c.want) {
				t.Fatalf("seek tick published %v, want %v", got, c.want)
			}
			if got := indexes(ev); !slices.Equal(got, c.wantIdx) {
				t.Fatalf("seek tick lyric_update %v, want %v", got, c.wantIdx)
			}
			if c.wantIdx != nil {
				return
			}
			if c.paused {
				if got := indexes(lineTick(s, t0, "MUSIC_1", 1, 200, c.to)); got != nil {
					t.Fatalf("resume before the first line: lyric_update %v", got)
				}
			}
			for ms := 250; c.to+int32(ms-250) < 10000; ms += 50 {
				if got := indexes(lineTick(s, t0, "MUSIC_1", 1, ms, c.to+int32(ms-250))); got != nil {
					t.Fatalf("t=%d ms: lyric_update %v before the first line", ms, got)
				}
			}
			at := 250 + int(10000-c.to)
			if got := indexes(lineTick(s, t0, "MUSIC_1", 1, at, 10000)); !slices.Equal(got, []int{0}) {
				t.Fatalf("first line after the seek: got %v, want [0]", got)
			}
		})
	}
}

// The first line starts at LyricDisplayStart (the first word when it is earlier
// than the line time) plus the configured offset; 10 ms either side of that,
// the line has not started (no lyric_update) or has (index 0).
func TestFirstLineBoundary(t *testing.T) {
	cases := []struct {
		name          string
		offsetMs      int
		words         []lyric.Word
		before, after int32
	}{
		{"line time", 0, nil, 9990, 10010},
		{"delay offset", 200, nil, 10190, 10210},
		{"advance offset", -300, nil, 9690, 9710},
		{"first word early", 0, []lyric.Word{{TimeMs: 9500, DurationMs: 400, Text: "a"}}, 9490, 9510},
		{"first word early, delay offset", 200, []lyric.Word{{TimeMs: 9500, DurationMs: 400, Text: "a"}}, 9690, 9710},
		{"first word late", 0, []lyric.Word{{TimeMs: 10300, DurationMs: 400, Text: "a"}}, 9990, 10010},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New(c.offsetMs, 50)
			s := newSession(p)
			t0 := time.Now()
			lines := []lyric.Line{{TimeMs: 10000, Text: "first", Words: c.words}, {TimeMs: 20000, Text: "second"}}
			lineTick(s, t0, "MUSIC_1", 1, 0, c.before-100)
			if got := indexes(lyricsTick(s, t0, "MUSIC_1", 100, c.before, lines, nil)); got != nil {
				t.Fatalf("at %d ms: lyric_update %v before the first line", c.before, got)
			}
			ev := lineTick(s, t0, "MUSIC_1", 1, 100+int(c.after-c.before), c.after)
			if got := indexes(ev); !slices.Equal(got, []int{0}) {
				t.Fatalf("at %d ms: got %v, want [0]", c.after, got)
			}
			if u := ofType(ev, player.EventLyricUpdate)[0].Data.(*player.LyricUpdate); u.Text != "first" || eventMs(ofType(ev, player.EventLyricUpdate)[0]) != c.after {
				t.Fatalf("first line payload %+v", u)
			}
		})
	}
}

var errFetch = errors.New("lyric fetch failed")

// fetchResult is a lyric fetch outcome delivered on a tick.
type fetchResult struct {
	lines []lyric.Line
	err   error
}

var (
	gotLyrics    = fetchResult{lines: lyricLines([]int{10000, 20000})}
	gotNone      = fetchResult{}
	gotPureMusic = fetchResult{lines: []lyric.Line{{TimeMs: 0, Text: "纯音乐，请欣赏"}}}
	gotNoLyrics  = fetchResult{err: lyric.ErrNoLyrics}
	gotFailure   = fetchResult{err: errFetch}

	// Kuwo's own hints (#201, ws/27-batch1-1114), and near misses that are lyrics.
	gotKuwoDJHint   = fetchResult{lines: []lyric.Line{{TimeMs: 790, Text: "此歌曲为DJ舞曲，请您欣赏"}}}
	gotKuwoPureHint = fetchResult{lines: []lyric.Line{{TimeMs: 790, Text: "此歌曲为没有填词的纯音乐，请您欣赏"}}}
	gotHintAmongTwo = fetchResult{lines: []lyric.Line{{TimeMs: 10000, Text: "此歌曲为DJ舞曲，请您欣赏"}, {TimeMs: 20000, Text: "第二行"}}}
	gotHintNoSuffix = fetchResult{lines: []lyric.Line{{TimeMs: 10000, Text: "此歌曲为DJ舞曲"}}}
	gotHintNoPrefix = fetchResult{lines: []lyric.Line{{TimeMs: 10000, Text: "请您欣赏"}}}
	gotKuwoGaiHint  = fetchResult{lines: []lyric.Line{{TimeMs: 790, Text: "该歌曲为纯音乐请欣赏"}}}
	// The title line plus the hint (ws/29-batch2-1114b, Go Time: "Go Time-Mark Petrie"
	// for "Go Time - Mark Petrie"); playFetch's song is titled "song MUSIC_1 - artist"
	// (observedSong).
	gotTitleThenHint = fetchResult{lines: []lyric.Line{{TimeMs: 0, Text: "song MUSIC_1-artist"}, {TimeMs: 110, Text: "该歌曲为纯音乐请欣赏"}}}
	gotOtherThenHint = fetchResult{lines: []lyric.Line{{TimeMs: 10000, Text: "song MUSIC_2-artist"}, {TimeMs: 20000, Text: "该歌曲为纯音乐请欣赏"}}}
	gotTitleThenLine = fetchResult{lines: []lyric.Line{{TimeMs: 10000, Text: "song MUSIC_1-artist"}, {TimeMs: 20000, Text: "第一句歌词"}}}
)

// playFetch plays MUSIC_1 from 1000 ms, a tick every 50 ms up to tick 20 (all
// before the first line at 10000 ms), delivering results[k] on tick k and
// paused from tick pausedFrom on (never when negative). Tick 0 is the song
// change. It returns each tick's events.
func playFetch(t *testing.T, results map[int]fetchResult, pausedFrom int) (*session, [][]player.Event) {
	t.Helper()
	p := New(0, 50)
	s := newSession(p)
	t0 := time.Now()
	var ticks [][]player.Event
	for k := 0; k <= 20; k++ {
		status, ms := 1, int32(1000+50*k)
		if pausedFrom >= 0 && k >= pausedFrom {
			status, ms = 2, int32(1000+50*pausedFrom)
		}
		at := after(t0, 50*k)
		observedSong(s, "MUSIC_1", status, ms, at)
		if r, ok := results[k]; ok {
			s.accept(assetResult{gen: s.gen, rid: s.rid, lines: r.lines, err: r.err}, at)
		}
		s.emitLine(at)
		s.resendPauseIfPending()
		ticks = append(ticks, drainEvents(p))
	}
	return s, ticks
}

// noLyricTicks lists the ticks that sent index:-1, checking that each carries a
// non-zero progress matching its position, and that no line was sent.
func noLyricTicks(t *testing.T, ticks [][]player.Event) []int {
	t.Helper()
	var out []int
	for k, ev := range ticks {
		for _, e := range ofType(ev, player.EventLyricUpdate) {
			u := e.Data.(*player.LyricUpdate)
			if u.Index != -1 {
				t.Fatalf("tick %d: line %d before the first line", k, u.Index)
			}
			if u.Progress == 0 || u.Progress != player.ClampProgress(u.Position, 100) {
				t.Fatalf("tick %d: index:-1 with position %v progress %v", k, u.Position, u.Progress)
			}
			out = append(out, k)
		}
	}
	return out
}

// index:-1 goes out once, on the tick the fetch settles with no lines, and
// never before: not on the song change tick, not while a retry is pending, not
// for a song whose lyrics arrive (after any number of failures).
//
// Mutation (verified): dropping the settled condition from emitLine sends
// index:-1 on tick 0 in every case; settling on any failure sends it on the
// first failure. Kuwo's hint (#201): not calling isKuwoHintOnly fails the hint
// cases; dropping the suffix check fails "hint prefix only"; dropping the 该
// prefix fails "kuwo 该 hint"; not comparing the title line fails "another title
// then hint"; not checking the second line is a hint fails "title line then a
// lyric".
func TestIndexMinusOneWaitsForFetch(t *testing.T) {
	cases := []struct {
		name    string
		results map[int]fetchResult
		paused  int
		want    []int // ticks that send index:-1
	}{
		{"in flight", nil, -1, nil},
		{"lyrics arrive", map[int]fetchResult{2: gotLyrics}, -1, nil},
		{"ErrNoLyrics", map[int]fetchResult{2: gotNoLyrics}, -1, []int{2}},
		{"zero lines", map[int]fetchResult{2: gotNone}, -1, []int{2}},
		{"pure music hint", map[int]fetchResult{2: gotPureMusic}, -1, []int{2}},
		{"kuwo DJ hint", map[int]fetchResult{2: gotKuwoDJHint}, -1, []int{2}},
		{"kuwo no-words hint", map[int]fetchResult{2: gotKuwoPureHint}, -1, []int{2}},
		{"kuwo hint among two lines", map[int]fetchResult{2: gotHintAmongTwo}, -1, nil},
		{"hint prefix only", map[int]fetchResult{2: gotHintNoSuffix}, -1, nil},
		{"hint suffix only", map[int]fetchResult{2: gotHintNoPrefix}, -1, nil},
		{"kuwo 该 hint", map[int]fetchResult{2: gotKuwoGaiHint}, -1, []int{2}},
		{"title line then hint", map[int]fetchResult{2: gotTitleThenHint}, -1, []int{2}},
		{"another title then hint", map[int]fetchResult{2: gotOtherThenHint}, -1, nil},
		{"title line then a lyric", map[int]fetchResult{2: gotTitleThenLine}, -1, nil},
		{"one failure, then lyrics", map[int]fetchResult{2: gotFailure, 6: gotLyrics}, -1, nil},
		{"two failures, then lyrics", map[int]fetchResult{2: gotFailure, 4: gotFailure, 6: gotLyrics}, -1, nil},
		{"one failure, waiting", map[int]fetchResult{2: gotFailure}, -1, nil},
		{"two failures, waiting", map[int]fetchResult{2: gotFailure, 4: gotFailure}, -1, nil},
		{"three failures", map[int]fetchResult{2: gotFailure, 4: gotFailure, 6: gotFailure}, -1, []int{6}},
		{"failure, then ErrNoLyrics", map[int]fetchResult{2: gotFailure, 4: gotNoLyrics}, -1, []int{4}},
		{"failure, then zero lines", map[int]fetchResult{2: gotFailure, 4: gotNone}, -1, []int{4}},
		{"paused in flight, ErrNoLyrics while paused", map[int]fetchResult{5: gotNoLyrics}, 2, []int{5}},
		{"paused in flight, zero lines while paused", map[int]fetchResult{5: gotNone}, 2, []int{5}},
		{"paused in flight, lyrics while paused", map[int]fetchResult{5: gotLyrics}, 2, nil},
		{"third failure while paused", map[int]fetchResult{2: gotFailure, 4: gotFailure, 8: gotFailure}, 6, []int{8}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, ticks := playFetch(t, c.results, c.paused)
			if len(ofType(ticks[0], player.EventAllLyrics)) != 1 || ofType(ticks[0], player.EventLyricUpdate) != nil {
				t.Fatalf("change tick: want all_lyrics and no lyric_update, got %+v", ticks[0])
			}
			if got := noLyricTicks(t, ticks); !slices.Equal(got, c.want) {
				t.Fatalf("index:-1 on ticks %v, want %v", got, c.want)
			}
			for k, r := range c.results {
				if r.err == nil && len(ofType(ticks[k], player.EventAllLyrics)) != 1 {
					t.Fatalf("tick %d: fetched lyrics without all_lyrics: %+v", k, ticks[k])
				}
			}
			if want := c.want != nil || len(s.lines) > 0; s.settled != want {
				t.Fatalf("settled %v with %d lines, want %v", s.settled, len(s.lines), want)
			}
		})
	}
}

// The outcome belongs to the song: a song change or the session going idle
// starts over unsettled with no tries, and a late result of the previous song
// (another RID, or the same RID before idle: another generation) is ignored.
// The second song plays ticks 0 to 4; its own results come from second.
//
// Mutation (verified): keeping settled across the change turns the "settled"
// cases red; keeping tries, "two failures, then one".
func TestFetchOutcomePerSong(t *testing.T) {
	for _, c := range []struct {
		name   string
		first  []fetchResult       // the first song's outcomes before the change
		idle   bool                // clear() between, and the second song has the first one's RID
		late   *fetchResult        // a result of the first song, delivered on tick 1
		second map[int]fetchResult // the second song's own results by tick
	}{
		{"settled no lyrics, then in flight", []fetchResult{gotNoLyrics}, false, nil, nil},
		{"settled zero lines, then in flight", []fetchResult{gotNone}, false, nil, nil},
		{"settled no lyrics, idle, same RID in flight", []fetchResult{gotNoLyrics}, true, nil, nil},
		{"two failures, then one", []fetchResult{gotFailure, gotFailure}, false, nil, map[int]fetchResult{2: gotFailure}},
		{"late ErrNoLyrics of the first song", []fetchResult{gotFailure}, false, &gotNoLyrics, nil},
		{"late zero lines of the first song", nil, false, &gotNone, nil},
		{"idle, late ErrNoLyrics of the same RID", []fetchResult{gotFailure}, true, &gotNoLyrics, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := New(0, 50)
			s := newSession(p)
			t0 := time.Now()
			lineTick(s, t0, "MUSIC_1", 1, 0, 1000)
			gen, rid := s.gen, s.rid
			for i, r := range c.first {
				at := after(t0, 50*(i+1))
				observedSong(s, "MUSIC_1", 1, int32(1000+50*(i+1)), at)
				s.accept(assetResult{gen: gen, rid: rid, lines: r.lines, err: r.err}, at)
				s.emitLine(at)
				drainEvents(p)
			}
			second := "MUSIC_2"
			if c.idle {
				s.clear()
				second = "MUSIC_1"
			}
			var ticks [][]player.Event
			for k := 0; k <= 4; k++ {
				at := after(t0, 1000+50*k)
				observedSong(s, second, 1, int32(1000+50*k), at)
				if k == 1 && c.late != nil {
					s.accept(assetResult{gen: gen, rid: rid, lines: c.late.lines, err: c.late.err}, at)
				}
				if r, ok := c.second[k]; ok {
					s.accept(assetResult{gen: s.gen, rid: s.rid, lines: r.lines, err: r.err}, at)
				}
				s.emitLine(at)
				ticks = append(ticks, drainEvents(p))
			}
			if got := noLyricTicks(t, ticks); got != nil {
				t.Fatalf("second song: index:-1 on ticks %v before its fetch settled", got)
			}
			if s.settled {
				t.Fatalf("second song settled (tries %d)", s.tries)
			}
			if c.late != nil && len(ofType(ticks[1], player.EventAllLyrics)) != 0 {
				t.Fatalf("late result of the first song was accepted: %+v", ticks[1])
			}
			// Once the second song's fetch settles with no lyrics, index:-1 goes out.
			at := after(t0, 1300)
			observedSong(s, second, 1, 1300, at)
			settleNoLyrics(s, at)
			s.emitLine(at)
			if got := indexes(drainEvents(p)); !slices.Equal(got, []int{-1}) {
				t.Fatalf("second song settled: lyric_update %v, want [-1]", got)
			}
		})
	}
}

// The line sentinel starts over with each song and after the session went idle:
// a second song without lines gets its own index:-1, a second song at the same
// line index gets its line, and one with lines starts with a silent prelude.
func TestLineSentinelResetsPerSong(t *testing.T) {
	t.Run("no lyrics, then no lyrics", func(t *testing.T) {
		p := New(0, 50)
		s := newSession(p)
		t0 := time.Now()
		lyricsTick(s, t0, "MUSIC_1", 0, 1000, nil, lyric.ErrNoLyrics)
		lineTick(s, t0, "MUSIC_1", 1, 50, 1050)
		if got := indexes(lyricsTick(s, t0, "MUSIC_2", 100, 1000, nil, lyric.ErrNoLyrics)); !slices.Equal(got, []int{-1}) {
			t.Fatalf("second song: got %v, want [-1]", got)
		}
	})
	t.Run("same line index", func(t *testing.T) {
		p := New(0, 50)
		s := newSession(p)
		t0 := time.Now()
		if got := indexes(startSong(s, t0, "MUSIC_1", 12000, 10000)); !slices.Equal(got, []int{0}) {
			t.Fatalf("first song: got %v, want [0]", got)
		}
		lineTick(s, t0, "MUSIC_2", 1, 100, 12000)
		ev := lyricsTick(s, t0, "MUSIC_2", 150, 12050, lyricLines([]int{10000}), nil)
		if lu := ofType(ev, player.EventLyricUpdate); len(lu) != 1 || lu[0].Data.(*player.LyricUpdate).Index != 0 || lu[0].Data.(*player.LyricUpdate).Text != "line 0" {
			t.Fatalf("second song: got %+v, want its line 0", lu)
		}
	})
	t.Run("into a prelude", func(t *testing.T) {
		p := New(0, 50)
		s := newSession(p)
		t0 := time.Now()
		startSong(s, t0, "MUSIC_1", 25000, 10000, 20000)
		lineTick(s, t0, "MUSIC_2", 1, 100, 0)
		if got := indexes(lyricsTick(s, t0, "MUSIC_2", 150, 50, lyricLines([]int{10000}), nil)); got != nil {
			t.Fatalf("second song's prelude: lyric_update %v", got)
		}
		if got := indexes(lineTick(s, t0, "MUSIC_2", 1, 10100, 10000)); !slices.Equal(got, []int{0}) {
			t.Fatalf("second song's first line: got %v, want [0]", got)
		}
	})
	t.Run("after idle", func(t *testing.T) {
		p := New(0, 50)
		s := newSession(p)
		t0 := time.Now()
		lyricsTick(s, t0, "MUSIC_1", 0, 1000, nil, lyric.ErrNoLyrics)
		if !s.clear() {
			t.Fatal("clear did nothing")
		}
		drainEvents(p)
		if got := indexes(lyricsTick(s, t0, "MUSIC_1", 100, 1000, nil, lyric.ErrNoLyrics)); !slices.Equal(got, []int{-1}) {
			t.Fatalf("same song after idle: got %v, want [-1]", got)
		}
	})
}

// With no lyric_update in the prelude the seek-lag notice is judged against the
// all_lyrics anchor (1050 ms, 50 ms earlier): 230 ms ahead of it publishes a
// resume (#131), 150 ms does not. Neither step is a seek by CoreClock.
func TestPreludeLagNoticeFromAllLyrics(t *testing.T) {
	for _, c := range []struct {
		ms   int32
		want []string
	}{{1330, []string{resumeAt(1330)}}, {1250, nil}} {
		p := New(0, 50)
		s := newSession(p)
		t0 := time.Now()
		startSong(s, t0, "MUSIC_1", 1000, 10000)
		observedLag(s, "MUSIC_1", 1, c.ms, after(t0, 100), 220, false)
		s.emitLine(after(t0, 100))
		ev := drainEvents(p)
		if got := published(ev); !slices.Equal(got, c.want) {
			t.Fatalf("at %d: published %v, want %v", c.ms, got, c.want)
		}
		if got := indexes(ev); got != nil {
			t.Fatalf("at %d: lyric_update %v in the prelude", c.ms, got)
		}
	}
}

// A pause dropped in the prelude (#134) is resent alone on a later tick, and
// the front end stops.
func TestPreludePauseResend(t *testing.T) {
	p := New(0, 50)
	s := newSession(p)
	t0 := time.Now()
	startSong(s, t0, "MUSIC_1", 1000, 10000)
	fillEvents(p)
	lineTick(s, t0, "MUSIC_1", 2, 100, 1100) // pause dropped
	if !s.pausePending {
		t.Fatal("a dropped pause must set pausePending")
	}
	ev := lineTick(s, t0, "MUSIC_1", 2, 150, 1100)
	if got := positioned(ev); len(got) != 1 || got[0].Type != player.EventPlaybackPause || eventMs(got[0]) != 1100 {
		t.Fatalf("resend tick: positioned events %+v, want one pause at 1100", got)
	}
	if s.pausePending || s.front.running {
		t.Fatalf("after the resend: pausePending %v, front running %v", s.pausePending, s.front.running)
	}
}

// dropStep is one poll of TestDroppedLyricUpdateResent, 50 ms after the
// previous one.
type dropStep struct {
	status int // 1 playing, 2 paused
	ms     int32
	result *fetchResult // delivered between observe and emitLine
	full   bool         // the channel is full when emitLine runs
	want   []int        // lyric_update indexes the tick enqueues
}

// A lyric_update that the full channel drops is sent again on the next tick
// with room, once, as the line at that tick's position (a line passed while
// the channel was full is not sent); a dropped one does not anchor the front
// end. One that enqueues is not repeated.
//
// Mutation (verified): advancing lineIndex before emitPos, as before, turns
// every case with a full tick red except "next line reached while dropped",
// which checks that the passed line is not sent and holds either way.
func TestDroppedLyricUpdateResent(t *testing.T) {
	twoLines := fetchResult{lines: lyricLines([]int{10000, 10100})}
	cases := []struct {
		name  string
		steps []dropStep
	}{
		{"index:-1 dropped while playing", []dropStep{
			{1, 1000, nil, false, nil},
			{1, 1050, &gotNoLyrics, true, nil},
			{1, 1100, nil, false, []int{-1}},
			{1, 1150, nil, false, nil},
		}},
		{"index:-1 dropped while paused", []dropStep{
			{1, 1000, nil, false, nil},
			{2, 1050, nil, false, nil},
			{2, 1050, &gotNoLyrics, true, nil},
			{2, 1050, nil, false, []int{-1}},
			{2, 1050, nil, false, nil},
		}},
		{"index:-1 dropped after zero lines", []dropStep{
			{1, 1000, nil, false, nil},
			{1, 1050, &gotNone, true, nil},
			{1, 1100, nil, false, []int{-1}},
			{1, 1150, nil, false, nil},
		}},
		{"index:-1 dropped on three ticks", []dropStep{
			{1, 1000, nil, false, nil},
			{1, 1050, &gotNoLyrics, true, nil},
			{1, 1100, nil, true, nil},
			{1, 1150, nil, true, nil},
			{1, 1200, nil, false, []int{-1}},
			{1, 1250, nil, false, nil},
		}},
		{"line dropped", []dropStep{
			{1, 9900, nil, false, nil},
			{1, 9950, &gotLyrics, false, nil},
			{1, 10000, nil, true, nil},
			{1, 10050, nil, false, []int{0}},
			{1, 10100, nil, false, nil},
		}},
		{"line dropped after a seek while paused", []dropStep{
			{1, 9000, nil, false, nil},
			{1, 9050, &gotLyrics, false, nil},
			{2, 9050, nil, false, nil},
			{2, 12000, nil, true, nil},
			{2, 12000, nil, false, []int{0}},
			{2, 12000, nil, false, nil},
		}},
		{"next line reached while dropped", []dropStep{
			{1, 9900, nil, false, nil},
			{1, 9950, &twoLines, false, nil},
			{1, 10000, nil, true, nil},
			{1, 10050, nil, true, nil},
			{1, 10100, nil, false, []int{1}},
			{1, 10150, nil, false, nil},
		}},
		{"enqueued, not repeated", []dropStep{
			{1, 9900, nil, false, nil},
			{1, 9950, &gotLyrics, false, nil},
			{1, 10000, nil, false, []int{0}},
			{1, 10050, nil, false, nil},
			{1, 10100, nil, false, nil},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New(0, 50)
			s := newSession(p)
			t0 := time.Now()
			for k, st := range c.steps {
				at := after(t0, 50*k)
				observedSong(s, "MUSIC_1", st.status, st.ms, at)
				if st.result != nil {
					s.accept(assetResult{gen: s.gen, rid: s.rid, lines: st.result.lines, err: st.result.err}, at)
				}
				if st.full {
					fillEvents(p)
				}
				front := s.front
				s.emitLine(at)
				s.resendPauseIfPending()
				ev := drainEvents(p)
				if got := indexes(ev); !slices.Equal(got, st.want) {
					t.Fatalf("tick %d: lyric_update %v, want %v", k, got, st.want)
				}
				for _, e := range ofType(ev, player.EventLyricUpdate) {
					if eventMs(e) != st.ms || s.front.ms != st.ms {
						t.Fatalf("tick %d: lyric_update at %d, front end at %d; want both %d", k, eventMs(e), s.front.ms, st.ms)
					}
				}
				if st.full && s.front != front {
					t.Fatalf("tick %d: a dropped lyric_update moved the front end from %+v to %+v", k, front, s.front)
				}
			}
		})
	}
}
