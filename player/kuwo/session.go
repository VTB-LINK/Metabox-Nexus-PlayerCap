package kuwo

import (
	"errors"
	"sort"
	"time"

	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/lyric"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
)

// livenessThreshold is how long a song may play with the core position frozen
// before the session fails closed. It is a package var so tests can shrink it.
var livenessThreshold = 20 * time.Second

// lagNotifyMs is how far, in milliseconds, the public position may be from the
// front-end model on a tick whose seek-lag correction changed or whose segment
// was first adopted, before the session publishes a seek
// (doc/design/kuwo-seek-lag-design.md §3.3 "通知"). It is a package var so
// tests can change it.
var lagNotifyMs int64 = 200

// session has one owner: the polling loop. Workers return values, never emit
// events. Generation AND RID checks prevent late assets crossing songs/idle.
type session struct {
	p                        *KuwoPlayer
	gen                      uint64
	rid                      string
	song                     player.SongInfo
	clock                    pos.CoreClock
	duration                 float32
	playing, haveState, idle bool
	lines                    []player.LyricLine
	lineIndex                int
	allSent                  bool
	tries                    int
	retryAt                  time.Time
	stall                    livenessWatch
	// outMs and outAt are this tick's public position and observation time,
	// set in observe before any event of the tick: the position every
	// positioned event of the tick, and of accept until the next tick, carries.
	outMs int32
	outAt time.Time
	front frontModel
	// notice: the front end is to be told the position as a seek, until a
	// positioned event is enqueued.
	notice bool
	// pausePending: a playback_pause was judged to be sent but its enqueue was
	// dropped (full channel), so the front end still interpolates past the pause
	// (#134). resendPauseIfPending retries it each tick until it enqueues; while
	// it is set, observe does not send a notice-triggered resume (a real seek
	// still does), which would restart the front end the pause should have stopped.
	pausePending bool
}

// frontModel follows a front end that interpolates as doc/API_RESPONSE_EXAMPLES.md
// "时间插值" asks, from the positioned events of the song that were enqueued:
// each anchors it at the position it carries; all_lyrics and playback_resume
// start the interpolation, playback_pause stops it, lyric_update leaves it as
// it is. It never reads the player's own state, which the front end does not
// see: a paused session that sends all_lyrics, or whose playback_pause was
// dropped after a playback_resume (#134), leaves the front end interpolating.
type frontModel struct {
	ok      bool // anchored in this song
	ms      int32
	at      time.Time
	running bool
}

// positionMs is where the front end is at now.
func (f *frontModel) positionMs(now time.Time) int64 {
	if !f.running {
		return int64(f.ms)
	}
	return int64(f.ms) + now.Sub(f.at).Milliseconds()
}

// livenessWatch is a second line of evidence behind the structure match: it
// accumulates playing wall-time while the core position stays at its first
// value. It resets with the session on a song change; a pause zeros the
// accumulator; any position movement disarms it for the rest of the song.
type livenessWatch struct {
	have        bool
	base        int32
	moved       bool
	prevPlaying bool
	acc         time.Duration
	last        time.Time
}

// stalled records one sample and reports whether the position has stayed put
// through at least livenessThreshold of continuous playback.
func (w *livenessWatch) stalled(playing bool, posMs int32, now time.Time) bool {
	if !w.have {
		w.have, w.base, w.last, w.prevPlaying = true, posMs, now, playing
		return false
	}
	delta := now.Sub(w.last)
	if delta < 0 {
		delta = 0
	}
	w.last = now
	if posMs != w.base {
		w.moved = true
	}
	wasPlaying := w.prevPlaying
	w.prevPlaying = playing
	if !playing {
		w.acc = 0
		return false
	}
	if w.moved || !wasPlaying {
		// The interval ending at the first playing sample after a pause was spent
		// paused: it is not playing time.
		return false
	}
	w.acc += delta
	return w.acc >= livenessThreshold
}

func newSession(p *KuwoPlayer) *session { return &session{p: p, lineIndex: -2} }

func (s *session) clear() bool {
	if s.idle {
		return false
	}
	s.p.NewSongGen()
	s.p.Emit(player.EventClearSongData, nil)
	s.p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_song", Detail: i18n.T("Kuwo has no current song"), Reason: player.ReasonNoSong})
	p := s.p
	*s = session{p: p, idle: true, lineIndex: -2}
	return true
}

func (s *session) observe(meta cdp.Snapshot, sample pos.Sample) bool {
	if (meta.Status != 1 && meta.Status != 2) || sample.RID != pos.RIDWithPrefix(meta.RID) {
		return false
	}
	changed := s.rid != sample.RID
	if changed {
		gen := s.p.NewSongGen()
		p := s.p
		*s = session{p: p, gen: gen, rid: sample.RID, lineIndex: -2, lines: []player.LyricLine{}}
		s.p.Emit(player.EventClearSongData, nil)
	}
	s.idle = false
	seek := s.clock.Observe(sample)
	s.outMs, s.outAt = sample.PositionMs, sample.ObservedAt
	// Seek-lag notice, before any event of this tick. Judged only on a tick
	// whose correction changed, whose segment was first adopted, or on which the
	// #135 transient filter signals DirectSound started playing (TransientResync,
	// design §3.6): after a DirectSound or WaveOut seek the kernel overshoots and
	// stalls, moving the public position off the wall clock on other ticks, and
	// judging those would anchor the front end on the transient. The model
	// includes any silent advance (a small seek within CoreClock's tolerance)
	// since the front end's anchor. Without an anchor in this song the front end
	// has nothing to correct. A song change starts over: the session was just
	// reset.
	if !changed {
		if s.front.ok && (sample.LagShiftMs != 0 || sample.LagAdopted || sample.TransientResync) {
			if d := int64(s.outMs) - s.front.positionMs(s.outAt); d >= lagNotifyMs || d <= -lagNotifyMs {
				s.notice = true
			}
		}
		// A dropped playback_pause (pausePending) left the front end running; a
		// notice-triggered resume would restart it (#134). Hold the notice until the
		// pause is resent. A real seek judged by CoreClock.Observe (the seek variable)
		// is not held: the user dragged, resume+pause go out at the new position.
		seek = seek || (s.notice && !s.pausePending)
	}
	oldDuration := s.duration
	s.duration = float32(sample.DurationMs) / 1000
	info := player.SongInfo{Name: meta.Title, Singer: meta.Artist, Title: meta.Title}
	if meta.Artist != "" {
		info.Title += " - " + meta.Artist
	}
	metadataChanged := changed || info.Name != s.song.Name || info.Singer != s.song.Singer
	if metadataChanged {
		info.Cover, info.CoverBase64 = s.song.Cover, s.song.CoverBase64
		s.song = info
		s.p.Emit(player.EventSongInfoUpdate, &info)
	}
	playing := meta.Status == 1
	stateChanged := !s.haveState || s.playing != playing
	s.playing, s.haveState = playing, true
	if metadataChanged || stateChanged {
		status := "paused"
		if playing {
			status = "playing"
		}
		s.p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: status, Detail: s.song.Title})
	}
	if metadataChanged || oldDuration != s.duration {
		s.emitAll()
	}
	if !changed && (stateChanged || seek) {
		if playing || seek {
			s.emitPos(player.EventPlaybackResume, &player.PlaybackTimeInfo{Position: s.clock.Position(), Progress: player.ClampProgress(s.clock.Position(), s.duration)})
		}
		if playing {
			// Resumed playback: no pending pause to resend (#134).
			s.pausePending = false
		}
		// Paused: playback_pause, after the resume when this tick is a seek. Seek
		// uses resume as a position correction, but must not leave paused
		// consumers extrapolating. Preserve ordering and the same sampled position.
		// A dropped pause sets pausePending so resendPauseIfPending retries it (#134).
		if !playing {
			s.pausePending = !s.emitPos(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: s.clock.Position(), Progress: player.ClampProgress(s.clock.Position(), s.duration)})
		}
		s.lineIndex = -2
	}
	return changed
}

// emitPos sends a positioned public event (all_lyrics, lyric_update,
// playback_pause, playback_resume). The front end anchors on it, so an
// enqueued event updates the front-end model and delivers the notice; a
// dropped one (full channel) never reached the front end and changes neither,
// so a dropped notice is sent again on the next tick. It returns whether the
// event was enqueued, so the pause path can mark pausePending on a drop (#134).
// These four events go out only through here (✅ player/kuwoanchor_lint_test.go).
func (s *session) emitPos(evtType string, data interface{}) bool {
	if s.p.TryEmit(evtType, data) {
		s.anchored(evtType)
		return true
	}
	return false
}

// resendPauseIfPending retries a dropped playback_pause (#134), at the current
// clock position, until it enqueues. Driven by runSession each tick after
// observe, independent of observe's own event branch (observe sends no event on
// a tick with no change, so a tied retry would never fire). It goes through
// emitPos: the AST gate (kuwoanchor_lint_test.go) requires it, emitPos builds
// the *PlaybackTimeInfo the server needs, and anchored lands the front-end model
// on this tick's public position, the same one the resent pause carries. Only
// while still paused: a resume already cleared pausePending, and resendPauseIfPending
// must not send a stale pause over a running front end.
func (s *session) resendPauseIfPending() {
	if s.pausePending && !s.playing {
		if s.emitPos(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: s.clock.Position(), Progress: player.ClampProgress(s.clock.Position(), s.duration)}) {
			s.pausePending = false
		}
	}
}

// anchored records an enqueued positioned event at this tick's public position.
func (s *session) anchored(evtType string) {
	f := &s.front
	f.ok, f.ms, f.at = true, s.outMs, s.outAt
	switch evtType {
	case player.EventAllLyrics, player.EventPlaybackResume:
		f.running = true
	case player.EventPlaybackPause:
		f.running = false
	}
	s.notice = false
}

func (s *session) emitAll() {
	s.emitPos(player.EventAllLyrics, &player.AllLyricsData{Title: s.song.Title, Duration: s.duration, Position: s.clock.Position(), Progress: player.ClampProgress(s.clock.Position(), s.duration), Count: len(s.lines), Lyrics: s.lines})
	s.allSent = true
	s.lineIndex = -2
}

func (s *session) accept(r assetResult, now time.Time) {
	if s.rid == "" || r.gen != s.gen || r.rid != s.rid {
		return
	}
	if r.cover {
		if r.err != nil {
			log.Detail("Kuwo cover unavailable: %v", r.err)
			return
		}
		s.song.Cover, s.song.CoverBase64 = r.art.URL, r.art.DataURI
		copy := s.song
		s.p.Emit(player.EventSongInfoUpdate, &copy)
		return
	}
	s.tries++
	if r.err != nil {
		if !errors.Is(r.err, lyric.ErrNoLyrics) && s.tries < 3 {
			s.retryAt = now.Add(5 * time.Second)
		}
		log.Detail("Kuwo lyrics unavailable: %v", r.err)
		return
	}
	s.retryAt = time.Time{}
	s.lines = toLyricLines(r.lines, float32(s.p.offsetMs)/1000)
	if player.IsPureMusicOnly(s.lines) {
		s.lines = []player.LyricLine{}
	}
	s.emitAll()
}

func (s *session) retryDue(now time.Time) bool {
	return s.rid != "" && s.playing && !s.retryAt.IsZero() && !now.Before(s.retryAt)
}

func (s *session) emitLine(now time.Time) {
	if !s.allSent || s.rid == "" {
		return
	}
	position := s.clock.Position()
	match := player.LyricMatchTime(position, float32(s.p.offsetMs)/1000)
	i := sort.Search(len(s.lines), func(i int) bool {
		return player.LyricDisplayStart(s.lines[i].Timestamp, s.lines[i].TextDetailed) > match
	}) - 1
	if i != s.lineIndex {
		s.lineIndex = i
		if i < 0 {
			s.emitPos(player.EventLyricUpdate, &player.LyricUpdate{Index: -1, Position: position, Progress: player.ClampProgress(position, s.duration)})
		} else {
			line := s.lines[i]
			s.emitPos(player.EventLyricUpdate, player.BuildLyricUpdate(i, line.Timestamp, line.Text, line.SubText, line.RomaText, line.TextDetailed, float32(s.p.offsetMs)/1000, position, s.duration))
		}
	}
	if s.playing {
		s.p.EmitPositionSample(now, position, s.duration)
	}
}

func toLyricLines(lines []lyric.Line, offsetSec float32) []player.LyricLine {
	out := make([]player.LyricLine, 0, len(lines))
	for _, line := range lines {
		var detail player.LyricTextDetailed
		end := float32(0)
		for _, word := range line.Words {
			if word.Text == "" {
				continue
			}
			timestamp := float32(word.TimeMs) / 1000
			duration := float32(word.DurationMs) / 1000
			if len(detail.Words) == 0 {
				detail.Timestamp = timestamp
				detail.PlayTime = player.AdjustLyricPlayTime(timestamp, offsetSec)
				end = timestamp + duration
			}
			if timestamp+duration > end {
				end = timestamp + duration
			}
			detail.Words = append(detail.Words, player.LyricTextDetailedWord{Timestamp: timestamp, PlayTime: player.AdjustLyricPlayTime(timestamp, offsetSec), Duration: duration, Text: word.Text})
		}
		if len(detail.Words) > 0 {
			detail.Duration = end - detail.Timestamp
		}
		out = append(out, player.BuildLyricLine(len(out), float32(line.TimeMs)/1000, line.Text, line.SubText, "", detail, offsetSec))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return player.LyricDisplayStart(out[i].Timestamp, out[i].TextDetailed) < player.LyricDisplayStart(out[j].Timestamp, out[j].TextDetailed)
	})
	for i := range out {
		out[i].Index = i
	}
	return out
}
