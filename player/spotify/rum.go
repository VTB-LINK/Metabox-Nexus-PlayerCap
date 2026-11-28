package spotify

import (
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/spotify/lyric"
	"Metabox-Nexus-PlayerCap/rum"
)

type spotifyAttachMeta struct {
	attachMs int64
}

// spotifyBlockTracker 跟踪 Spotify 外层门禁阻断态（store_install/no_admin/spotify_flag_absent/cdp_port_not_ready）的边沿去重与恢复耗时。
type spotifyBlockTracker struct {
	reason string
	since  time.Time
	now    func() time.Time
}

func (b *spotifyBlockTracker) nowTime() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *spotifyBlockTracker) onBlocked(reason string) {
	if reason == "" {
		return
	}
	if b.reason == reason && !b.since.IsZero() {
		return
	}
	b.reason = reason
	b.since = b.nowTime()
	rum.Track(rum.CatPlayer, "player_blocked", rum.Props{
		"player": PlayerName,
		"phase":  "blocked",
		"reason": reason,
	})
}

func (b *spotifyBlockTracker) onNotRunning() {
	b.reason = ""
	b.since = time.Time{}
}

func (b *spotifyBlockTracker) onRecovered() {
	if b.reason == "" || b.since.IsZero() {
		return
	}
	blockedSec := int64(b.nowTime().Sub(b.since).Seconds())
	if blockedSec < 0 {
		blockedSec = 0
	}
	reason := b.reason
	b.onNotRunning()
	rum.Track(rum.CatPlayer, "player_blocked", rum.Props{
		"player":      PlayerName,
		"phase":       "recovered",
		"reason":      reason,
		"blocked_sec": blockedSec,
	})
}

type spotifyPendingTrack struct {
	gen         uint64
	startedAt   time.Time
	clockMode   string
	lyricSource string
	lyricFormat string
	lineCount   int
	isPureMusic bool
	lyricMs     int64

	lyricReady bool
	coverWait  bool
	coverReady bool
	coverOut   player.CoverOutcome
	emitted    bool
}

// spotifySessionRUM 管理单次 Spotify 会话的 RUM 生命周期、兼容退化限频、识曲结算与退出完成屏障。
// 所有可变字段受 mu 保护；持锁期间绝不调用 rum.Track 或 player.NoteTrackRecognized。
type spotifySessionRUM struct {
	mu            sync.Mutex
	version       string
	startedAt     time.Time
	tracksPlayed  int
	compatSent    map[string]bool
	metaBlockedAt time.Time
	pending       *spotifyPendingTrack
	ended         bool
	endDone       chan struct{}
	trackEmitWG   sync.WaitGroup

	// testBeforeTrackEmit / testBeforeDetachEmit 仅供单测注入并发交错屏障，生产恒为 nil。
	testBeforeTrackEmit  func()
	testBeforeDetachEmit func()
}

// FlushRUM 在主进程退出前同步结算活动 Spotify 会话的待报曲目与 detached 事件（并发安全且幂等）。
func (p *SpotifyPlayer) FlushRUM() {
	p.rumGate.Flush(func(s *spotifySessionRUM) {
		s.end("stopped")
	})
}

func (p *SpotifyPlayer) startSessionRUM(version string, attachMs int64) *spotifySessionRUM {
	ver := player.SanitizeVersion(version)
	if attachMs < 0 {
		attachMs = 0
	}
	sess, ok := p.rumGate.Register(
		func() *spotifySessionRUM {
			return &spotifySessionRUM{
				version:    ver,
				startedAt:  time.Now(),
				compatSent: make(map[string]bool),
				endDone:    make(chan struct{}),
			}
		},
		func(*spotifySessionRUM) {
			if p.testBeforeAttach != nil {
				p.testBeforeAttach()
			}
			rum.Track(rum.CatPlayer, "player_session", rum.Props{
				"player":         PlayerName,
				"action":         "attached",
				"player_version": ver,
				"attach_ms":      attachMs,
			})
		},
	)
	if !ok {
		return nil
	}
	return sess
}

func (p *SpotifyPlayer) endSessionRUM(s *spotifySessionRUM, cause string) {
	if s == nil {
		return
	}
	p.rumGate.End(s, func(sess *spotifySessionRUM) {
		sess.end(cause)
	})
}

func (p *SpotifyPlayer) setSessionAttachMeta(attachMs int64) {
	p.attachMetaMu.Lock()
	p.attachMeta = spotifyAttachMeta{attachMs: attachMs}
	p.attachMetaMu.Unlock()
}

func (p *SpotifyPlayer) takeSessionAttachMeta() spotifyAttachMeta {
	p.attachMetaMu.Lock()
	m := p.attachMeta
	p.attachMeta = spotifyAttachMeta{}
	p.attachMetaMu.Unlock()
	return m
}

func newSpotifySessionRUM(version string, attachMs int64) *spotifySessionRUM {
	ver := player.SanitizeVersion(version)
	if attachMs < 0 {
		attachMs = 0
	}
	rum.Track(rum.CatPlayer, "player_session", rum.Props{
		"player":         PlayerName,
		"action":         "attached",
		"player_version": ver,
		"attach_ms":      attachMs,
	})
	return &spotifySessionRUM{
		version:    ver,
		startedAt:  time.Now(),
		compatSent: make(map[string]bool),
		endDone:    make(chan struct{}),
	}
}

func (s *spotifySessionRUM) updateVersion(version string) {
	if s == nil {
		return
	}
	ver := player.SanitizeVersion(version)
	if ver == "" {
		return
	}
	s.mu.Lock()
	if !s.ended && s.version == "" {
		s.version = ver
	}
	s.mu.Unlock()
}

func (s *spotifySessionRUM) onCompat(signal, detailCode string, elapsedMs int64) {
	if s == nil || signal == "" {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	key := signal + "\x00" + detailCode
	if s.compatSent[key] {
		s.mu.Unlock()
		return
	}
	s.compatSent[key] = true
	ver := s.version
	s.mu.Unlock()

	props := rum.Props{
		"player":         PlayerName,
		"signal":         signal,
		"player_version": ver,
	}
	if detailCode != "" {
		props["detail_code"] = detailCode
	}
	if elapsedMs > 0 || signal == "passive_token_captured" {
		if elapsedMs < 0 {
			elapsedMs = 0
		}
		props["elapsed_ms"] = elapsedMs
	}
	rum.Track(rum.CatPlayer, "player_compat_signal", props)
}

func (s *spotifySessionRUM) onMetadataBlocked(now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended || !s.metaBlockedAt.IsZero() {
		s.mu.Unlock()
		return
	}
	s.metaBlockedAt = now
	s.mu.Unlock()

	rum.Track(rum.CatPlayer, "player_blocked", rum.Props{
		"player": PlayerName,
		"phase":  "blocked",
		"reason": "metadata_unavailable_10s",
	})
}

func (s *spotifySessionRUM) onMetadataRecovered(now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.metaBlockedAt.IsZero() {
		s.mu.Unlock()
		return
	}
	blockedSec := int64(now.Sub(s.metaBlockedAt).Seconds())
	if blockedSec < 0 {
		blockedSec = 0
	}
	s.metaBlockedAt = time.Time{}
	s.mu.Unlock()

	rum.Track(rum.CatPlayer, "player_blocked", rum.Props{
		"player":      PlayerName,
		"phase":       "recovered",
		"reason":      "metadata_unavailable_10s",
		"blocked_sec": blockedSec,
	})
}

func (s *spotifySessionRUM) finishTrackEmit(p *spotifyPendingTrack, hook func()) {
	defer s.trackEmitWG.Done()
	if hook != nil {
		hook()
	}
	emitSpotifyTrackRecognized(p)
}

func (s *spotifySessionRUM) beginTrack(gen uint64, clockMode string) {
	if s == nil {
		return
	}
	var flush *spotifyPendingTrack
	var hook func()
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	if s.pending != nil && !s.pending.emitted {
		cp := *s.pending
		s.pending.emitted = true
		s.tracksPlayed++
		s.trackEmitWG.Add(1)
		hook = s.testBeforeTrackEmit
		flush = &cp
	}
	s.pending = &spotifyPendingTrack{
		gen:       gen,
		startedAt: time.Now(),
		clockMode: clockMode,
		coverWait: true,
		coverOut:  player.CoverOutcome{Action: player.CoverActionNone},
	}
	s.mu.Unlock()

	if flush != nil {
		s.finishTrackEmit(flush, hook)
	}
}

func (s *spotifySessionRUM) onCoverOutcome(gen uint64, out player.CoverOutcome) {
	if s == nil {
		return
	}
	var readyToEmit *spotifyPendingTrack
	var hook func()
	s.mu.Lock()
	if !s.ended && s.pending != nil && s.pending.gen == gen && !s.pending.emitted {
		s.pending.coverOut = out
		s.pending.coverReady = true
		s.pending.coverWait = false
		if s.pending.lyricReady {
			cp := *s.pending
			s.pending.emitted = true
			s.tracksPlayed++
			s.trackEmitWG.Add(1)
			hook = s.testBeforeTrackEmit
			readyToEmit = &cp
		}
	}
	s.mu.Unlock()

	if readyToEmit != nil {
		s.finishTrackEmit(readyToEmit, hook)
	}
}

func (s *spotifySessionRUM) onTrackLyricSettled(gen uint64, lyricSource string, lines []lyric.Line, isPureMusic bool, lyricMs int64, waitCover bool) {
	if s == nil {
		return
	}
	var readyToEmit *spotifyPendingTrack
	var hook func()
	s.mu.Lock()
	p := s.pending
	if !s.ended && p != nil && p.gen == gen && !p.emitted && !p.lyricReady {
		if lyricMs < 0 {
			lyricMs = 0
		}
		p.lyricMs = lyricMs
		p.lyricSource = lyricSource
		p.lineCount = len(lines)
		p.isPureMusic = isPureMusic
		if len(lines) > 0 {
			p.lyricFormat = "lrc"
		} else {
			p.lyricFormat = "none"
		}
		p.lyricReady = true
		if !waitCover {
			p.coverWait = false
		}
		if p.coverReady || !p.coverWait {
			cp := *p
			p.emitted = true
			s.tracksPlayed++
			s.trackEmitWG.Add(1)
			hook = s.testBeforeTrackEmit
			readyToEmit = &cp
		}
	}
	s.mu.Unlock()

	if readyToEmit != nil {
		s.finishTrackEmit(readyToEmit, hook)
	}
}

func (s *spotifySessionRUM) onLateTokenRetry() {
	if s == nil {
		return
	}
	var sinceMs int64
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	if s.pending != nil && !s.pending.startedAt.IsZero() {
		sinceMs = time.Since(s.pending.startedAt).Milliseconds()
		if sinceMs < 0 {
			sinceMs = 0
		}
	}
	s.mu.Unlock()

	rum.Track(rum.CatLyricTrack, "track_lyric_updated", rum.Props{
		"player":            PlayerName,
		"reason":            "late_token_retry",
		"since_announce_ms": sinceMs,
	})
}

func (s *spotifySessionRUM) end(disconnectCause string) {
	if s == nil {
		return
	}
	var flush *spotifyPendingTrack
	s.mu.Lock()
	if s.ended {
		done := s.endDone
		s.mu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	if s.endDone == nil {
		s.endDone = make(chan struct{})
	}
	done := s.endDone
	s.ended = true
	if s.pending != nil && !s.pending.emitted {
		cp := *s.pending
		s.pending.emitted = true
		s.tracksPlayed++
		flush = &cp
	}
	tracks := s.tracksPlayed
	ver := s.version
	durSec := int64(time.Since(s.startedAt).Seconds())
	if durSec < 0 {
		durSec = 0
	}
	trackHook := s.testBeforeTrackEmit
	detachHook := s.testBeforeDetachEmit
	s.mu.Unlock()

	defer close(done)
	s.trackEmitWG.Wait()
	if flush != nil {
		if trackHook != nil {
			trackHook()
		}
		emitSpotifyTrackRecognized(flush)
	}
	if detachHook != nil {
		detachHook()
	}
	rum.Track(rum.CatPlayer, "player_session", rum.Props{
		"player":           PlayerName,
		"action":           "detached",
		"player_version":   ver,
		"disconnect_cause": disconnectCause,
		"duration_sec":     durSec,
		"tracks_played":    tracks,
	})
}

func emitSpotifyTrackRecognized(p *spotifyPendingTrack) {
	if p == nil {
		return
	}
	coverAction := p.coverOut.Action
	if coverAction == "" {
		coverAction = player.CoverActionNone
	}
	lyricSrc := p.lyricSource
	if lyricSrc == "" {
		lyricSrc = "none"
	}
	lyricFmt := p.lyricFormat
	if lyricFmt == "" {
		lyricFmt = "none"
	}
	clockMode := p.clockMode
	if clockMode == "" {
		clockMode = "dom_icon"
	}
	player.NoteTrackRecognized()
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{
		"player":                PlayerName,
		"clock_mode":            clockMode,
		"lyric_source":          lyricSrc,
		"lyric_format":          lyricFmt,
		"line_count":            p.lineCount,
		"has_verbatim":          false,
		"has_trans":             false,
		"has_roma":              false,
		"has_roma_detailed":     false,
		"has_furigana":          false,
		"is_pure_music":         p.isPureMusic,
		"cover_action":          coverAction,
		"cover_fallback_reason": p.coverOut.FallbackReason,
		"lyric_ms":              p.lyricMs,
		"cover_ms":              p.coverOut.DurationMs,
	})
}
