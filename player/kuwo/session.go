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
		event := player.EventPlaybackPause
		if playing || seek {
			event = player.EventPlaybackResume
		}
		s.p.Emit(event, &player.PlaybackTimeInfo{Position: s.clock.Position(), Progress: player.ClampProgress(s.clock.Position(), s.duration)})
		// Seek uses resume as a position correction, but must not leave paused
		// consumers extrapolating. Preserve ordering and the same sampled position.
		if seek && !playing {
			s.p.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: s.clock.Position(), Progress: player.ClampProgress(s.clock.Position(), s.duration)})
		}
		s.lineIndex = -2
	}
	return changed
}

func (s *session) emitAll() {
	s.p.Emit(player.EventAllLyrics, &player.AllLyricsData{Title: s.song.Title, Duration: s.duration, Position: s.clock.Position(), Progress: player.ClampProgress(s.clock.Position(), s.duration), Count: len(s.lines), Lyrics: s.lines})
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
			s.p.Emit(player.EventLyricUpdate, &player.LyricUpdate{Index: -1, Position: position, Progress: player.ClampProgress(position, s.duration)})
		} else {
			line := s.lines[i]
			s.p.Emit(player.EventLyricUpdate, player.BuildLyricUpdate(i, line.Timestamp, line.Text, line.SubText, line.RomaText, line.TextDetailed, float32(s.p.offsetMs)/1000, position, s.duration))
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
