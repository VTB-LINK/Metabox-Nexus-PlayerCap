package kuwo

import (
	"errors"
	"sort"
	"strings"
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

// switchStartBandMs and switchConfirmAfter govern the hold of a step back to the
// start of the published song (#185, doc/design/kuwo-switch-fallback-design.md
// §5.2). On a song switch PlayerCore's position falls to about 0 while its RID
// and the bridge still report the old song (inferred from recordings, design
// §3); such a step is published only once a snapshot answered at least
// switchConfirmAfter after it still reports the song. switchStartBandMs (1000)
// also covers the first two open ladder steps, 313 and 600 ms (#135).
// switchConfirmAfter starts at 300 ms: the one bound the recordings give, itself
// inferred, is the bridge reporting no song within about 180 ms of the fall
// (2026-11-13, ws/17-kuwo-40, design §3.4), with margin; the hold log of the
// real-device acceptance measures it (design §7). Package vars so tests can
// change them.
var (
	switchStartBandMs  int32 = 1000
	switchConfirmAfter       = 300 * time.Millisecond
)

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
	// (#134), or an all_lyrics resent while paused restarted it (#178).
	// resendPauseIfPending sends it each tick until it enqueues; while
	// it is set, observe does not send a notice-triggered resume (a real seek
	// still does), which would restart the front end the pause should have stopped.
	pausePending bool
	// settled: the lyric fetch has an outcome, lyrics (possibly none), ErrNoLyrics,
	// or a failure with no retry left. While it is in flight or waiting for a
	// retry the song's lyrics are unknown, and emitLine sends no index:-1 (#175).
	settled bool
	// coreMs is the raw core position (Sample.CoreMs) of the latest sample
	// observe took for this song (haveCore): what a step back is judged against
	// (#185).
	coreMs   int32
	haveCore bool
	// heldAt is the observation time of the first sample of a held step back
	// (#185), zero when none is held; heldMs is that sample's CoreMs, heldStatus
	// the status of the latest snapshot of a held tick.
	heldAt     time.Time
	heldMs     int32
	heldStatus int
	// recheck: a hold began, so the next observe of the song judges the front-end
	// model whatever the tick's correction flags (#185).
	recheck bool
	// later holds the asset results accepted while a step back was held (#185);
	// the next observe of the song accepts them after setting its position.
	later []assetResult
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

// hold reports whether this tick's sample is held instead of observed
// (doc/design/kuwo-switch-fallback-design.md §5.2, #185). On a song switch
// PlayerCore's position falls to about 0 while its RID and the bridge still
// report the old song (inferred from recordings, design §3), and on its tick
// that cannot be told from a drag to the start or a loop back to it. So a raw core step back on the published song that
// lands below switchStartBandMs starts a hold, and from then on every sample of
// the song is held, whatever its value (the fall may be followed by the open
// ladder, above the position held from), until a snapshot answered (snapAt) at
// least switchConfirmAfter after the first held sample still reports the song:
// the sample is then released to observe. The core was read with the snapshot's
// RID, so the sample's RID is the snapshot's. A sample of another song ends the
// hold unpublished, as dropHold does for no song and a failed read. It judges
// the raw CoreMs: the public position of a fall may be the #135 filter's held
// value, far from the start (74868 ms in ws/25-kuwo-185). A hold sets recheck,
// so the first observe of the song after it judges the front end whatever that
// tick's flags: the flags of the held ticks were not judged.
func (s *session) hold(meta cdp.Snapshot, snapAt time.Time, sample pos.Sample) bool {
	if s.rid == "" || sample.RID != s.rid {
		s.dropHold(sample.ObservedAt)
		return false
	}
	if s.heldAt.IsZero() {
		if !s.haveCore || sample.CoreMs >= s.coreMs || sample.CoreMs >= switchStartBandMs {
			return false
		}
		s.heldAt, s.heldMs, s.recheck = sample.ObservedAt, sample.CoreMs, true
	}
	s.heldStatus = meta.Status
	if snapAt.Before(s.heldAt.Add(switchConfirmAfter)) {
		return true
	}
	log.Detail("Kuwo core position went back at %s to %d ms; held %d ms, released: the bridge still reports the song",
		s.heldAt.Format("15:04:05.000"), s.heldMs, sample.ObservedAt.Sub(s.heldAt).Milliseconds())
	s.heldAt = time.Time{}
	return false
}

// dropHold ends a hold unpublished: the bridge reported no song, the core read
// failed, or another song was read (#185). The log gives the fall's time and
// value and how long the hold lasted, which for a switch is the time from the
// fall to the first sign of the switch (design §7).
func (s *session) dropHold(now time.Time) {
	if s.heldAt.IsZero() {
		return
	}
	log.Detail("Kuwo core position went back at %s to %d ms; held %d ms, discarded unpublished",
		s.heldAt.Format("15:04:05.000"), s.heldMs, now.Sub(s.heldAt).Milliseconds())
	s.heldAt = time.Time{}
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
	s.coreMs, s.haveCore = sample.CoreMs, true
	seek := s.clock.Observe(sample)
	s.outMs, s.outAt = sample.PositionMs, sample.ObservedAt
	// Seek-lag notice, before any event of this tick. Judged only on a tick
	// whose correction changed, whose segment was first adopted, or on which the
	// #135 transient filter signals DirectSound started playing (TransientResync,
	// design §3.6): after a DirectSound or WaveOut seek the kernel overshoots and
	// stalls, moving the public position off the wall clock on other ticks, and
	// judging those would anchor the front end on the transient. Also on the
	// first tick observed after a #185 hold (recheck), whatever its flags: the
	// held ticks' flags were not judged, and the release may be no seek. The model
	// includes any silent advance (a small seek within CoreClock's tolerance)
	// since the front end's anchor. Without an anchor in this song the front end
	// has nothing to correct. A song change starts over: the session was just
	// reset.
	if !changed {
		if s.front.ok && (sample.LagShiftMs != 0 || sample.LagAdopted || sample.TransientResync || s.recheck) {
			if d := int64(s.outMs) - s.front.positionMs(s.outAt); d >= lagNotifyMs || d <= -lagNotifyMs {
				s.notice = true
			}
		}
		s.recheck = false
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
	// A dropped all_lyrics (!allSent, #178) goes out again on each later tick until
	// it enqueues: at the tick's position, with the song's current lines and
	// duration, and before the tick's resume and pause, as on a duration change.
	// Enqueued while paused, the resend starts the front end interpolating and
	// clears the server's pause mark, over any pause already sent: a playback_pause
	// follows it, this tick's own below, or else resendPauseIfPending's at the end
	// of the tick, after the current line. An all_lyrics sent the first time (song
	// change, duration or title change, accept) gets none, as before.
	if metadataChanged || oldDuration != s.duration || !s.allSent {
		resend := !changed && !s.allSent
		s.emitAll()
		if resend && s.allSent && !playing {
			s.pausePending = true
		}
	}
	// Asset results that came while a step back was held (#185) are taken now:
	// after this tick's position, metadata and state, so an all_lyrics they send
	// carries them and the front-end model anchors at this tick, as the front end
	// does on receipt; and before this tick's resume and pause. all_lyrics starts
	// the front end interpolating, so a pause this tick sends must come after it:
	// taken after the pause, the lyrics would restart a paused front end with no
	// pause to follow (the pause enqueued, pausePending is clear). On a song
	// change the session was reset above and they were dropped with it.
	later := s.later
	s.later = nil
	for _, r := range later {
		s.accept(r, s.outAt)
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
// event was enqueued, so the pause path can mark pausePending on a drop (#134),
// emitLine keeps its line index for a retry (#175) and emitAll its allSent (#178).
// These four events go out only through here (✅ player/kuwoanchor_lint_test.go).
func (s *session) emitPos(evtType string, data interface{}) bool {
	if s.p.TryEmit(evtType, data) {
		s.anchored(evtType)
		return true
	}
	return false
}

// resendPauseIfPending retries a dropped playback_pause (#134), or sends the one
// that follows an all_lyrics resent while paused (#178), at the current clock
// position, until it enqueues. Driven by runSession each tick after
// observe, independent of observe's own event branch (observe sends no event on
// a tick with no change, so a tied retry would never fire). It goes through
// emitPos: the AST gate (kuwoanchor_lint_test.go) requires it, emitPos builds
// the *PlaybackTimeInfo the server needs, and anchored lands the front-end model
// on this tick's public position, the same one the resent pause carries. Only
// while still paused: a resume already cleared pausePending, and resendPauseIfPending
// must not send a stale pause over a running front end. While a step back is held
// (#185) observe has not taken the snapshot's state, so s.playing may be stale:
// only a held tick whose snapshot is still paused resends; one playing has made
// the pause void, and the release's observe clears pausePending.
func (s *session) resendPauseIfPending() {
	if !s.heldAt.IsZero() && s.heldStatus != 2 {
		return
	}
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

// emitAll sends all_lyrics. allSent records whether it was enqueued (#178): a
// dropped one (full channel) is sent again by observe on later ticks, and until
// it enqueues emitLine sends nothing, so no lyric_update goes out ahead of the
// all_lyrics it belongs to. lineIndex -2 has emitLine send the current line again once
// allSent holds: all_lyrics clears the server's cached lyric_update.
func (s *session) emitAll() {
	s.allSent = s.emitPos(player.EventAllLyrics, &player.AllLyricsData{Title: s.song.Title, Duration: s.duration, Position: s.clock.Position(), Progress: player.ClampProgress(s.clock.Position(), s.duration), Count: len(s.lines), Lyrics: s.lines})
	s.lineIndex = -2
}

func (s *session) accept(r assetResult, now time.Time) {
	if s.rid == "" || r.gen != s.gen || r.rid != s.rid {
		return
	}
	// While a step back is held (#185) the song publishes nothing: an all_lyrics
	// now would carry the position from before the hold and anchor the front-end
	// model at that tick's time, while the front end starts from its receipt. The
	// result waits for the next observe of the song.
	if !s.heldAt.IsZero() {
		s.later = append(s.later, r)
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
		} else {
			s.settled = true
		}
		log.Detail("Kuwo lyrics unavailable: %v", r.err)
		return
	}
	s.retryAt = time.Time{}
	s.settled = true
	s.lines = toLyricLines(r.lines, float32(s.p.offsetMs)/1000)
	if player.IsPureMusicOnly(s.lines) || isKuwoHintOnly(s.lines, s.song.Title) {
		s.lines = []player.LyricLine{}
	}
	s.emitAll()
}

// isKuwoHintOnly reports whether the lyrics are only Kuwo's own hint for a song
// without words, which the API docs treat as no lyrics, like the shared hint
// player.IsPureMusicOnly knows (#201). Seen (2026-11-14): one line
// "此歌曲为DJ舞曲，请您欣赏", "此歌曲为没有填词的纯音乐，请您欣赏" or
// "该歌曲为纯音乐请欣赏"; or the song's title line ("Go Time-Mark Petrie" for
// "Go Time - Mark Petrie") followed by "该歌曲为纯音乐请欣赏". A title line counts
// only when it equals the song's title once whitespace is removed; any other
// lyrics, a song that happens to sing the hint included, are kept.
func isKuwoHintOnly(lines []player.LyricLine, title string) bool {
	switch len(lines) {
	case 1:
		return isKuwoHint(lines[0].Text)
	case 2:
		return isKuwoHint(lines[1].Text) && title != "" && squashSpace(lines[0].Text) == squashSpace(title)
	}
	return false
}

func isKuwoHint(text string) bool {
	return (strings.HasPrefix(text, "此歌曲为") || strings.HasPrefix(text, "该歌曲为")) && strings.HasSuffix(text, "欣赏")
}

func squashSpace(s string) string { return strings.Join(strings.Fields(s), "") }

func (s *session) retryDue(now time.Time) bool {
	return s.rid != "" && s.playing && !s.retryAt.IsZero() && !now.Before(s.retryAt)
}

func (s *session) emitLine(now time.Time) {
	// While a step back is held (#185) the song publishes nothing: a line resent
	// now (a dropped one, #175) would anchor the front-end model at the tick before
	// the hold. The next observe of the song sets the position it goes out at.
	if !s.allSent || s.rid == "" || !s.heldAt.IsZero() {
		return
	}
	position := s.clock.Position()
	match := player.LyricMatchTime(position, float32(s.p.offsetMs)/1000)
	i := sort.Search(len(s.lines), func(i int) bool {
		return player.LyricDisplayStart(s.lines[i].Timestamp, s.lines[i].TextDetailed) > match
	}) - 1
	// index:-1 means the platform returned no lyrics (doc/openapi.yaml /lyric_update),
	// so it goes out only once the fetch has settled with no lines, not while the
	// fetch is in flight or waiting for a retry; lineIndex stays -2 until then, so it
	// goes out on the tick the fetch settles, paused or not. Before the first line of
	// a song with lines, at the start or after a seek back, nothing goes out and
	// lineIndex is kept, as in the other players; the front end is anchored there by
	// all_lyrics, or by the seek's playback_resume and playback_pause (#175).
	// lineIndex advances only when the event is enqueued: a dropped one (full
	// channel) is sent again on the next tick, as the line at that tick's position,
	// paused or not. For a song without lines index:-1 is the only notice (#175).
	if i != s.lineIndex && (i >= 0 || s.settled && len(s.lines) == 0) {
		var sent bool
		if i < 0 {
			sent = s.emitPos(player.EventLyricUpdate, &player.LyricUpdate{Index: -1, Position: position, Progress: player.ClampProgress(position, s.duration)})
		} else {
			line := s.lines[i]
			sent = s.emitPos(player.EventLyricUpdate, player.BuildLyricUpdate(i, line.Timestamp, line.Text, line.SubText, line.RomaText, line.TextDetailed, float32(s.p.offsetMs)/1000, position, s.duration))
		}
		if sent {
			s.lineIndex = i
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
