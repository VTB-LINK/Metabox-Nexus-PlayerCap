package applemusic

import (
	"math"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/krc"
	"Metabox-Nexus-PlayerCap/rum"
)

// amBlockTracker 跟踪 Apple Music 外层权限阻断态边沿与恢复耗时。
type amBlockTracker struct {
	reason string
	since  time.Time
	now    func() time.Time
}

func (b *amBlockTracker) nowTime() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *amBlockTracker) onBlocked(reason string) {
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

func (b *amBlockTracker) onNotRunning() {
	b.reason = ""
	b.since = time.Time{}
}

func (b *amBlockTracker) onRecovered() {
	if b.reason == "" || b.since.IsZero() {
		return
	}
	blockedSec := int64(b.nowTime().Sub(b.since).Seconds())
	if blockedSec < 0 {
		blockedSec = 0
	}
	reason := b.reason
	b.reason = ""
	b.since = time.Time{}
	rum.Track(rum.CatPlayer, "player_blocked", rum.Props{
		"player":      PlayerName,
		"phase":       "recovered",
		"reason":      reason,
		"blocked_sec": blockedSec,
	})
}

// amPendingTrack 保存当前已宣告曲目的脱敏 RUM 结算快照，受 amSessionRUM.mu 保护，
// 使退出路径的 FlushRUM 可脱离 session 轮询状态安全结算，零数据竞争。
type amPendingTrack struct {
	songChangedAt time.Time
	lyricReadyAt  time.Time
	lyricSource   string
	lyricFormat   string
	lineCount     int
	hasVerbatim   bool
	hasTrans      bool
	isPureMusic   bool
	coverOut      player.CoverOutcome
	emitted       bool
}

// amSessionRUM 归属单个 session，所有可变字段受 mu 保护，供轮询线程与退出信号侧 FlushRUM 并发访问；
// 持锁期间绝不调用 rum.Track 或 player.NoteTrackRecognized。
type amSessionRUM struct {
	mu            sync.Mutex
	nowFn         func() time.Time
	sessionActive bool
	sessionStart  time.Time
	tracksPlayed  int
	compatSent    map[string]bool

	metaBlockedAt time.Time

	announcedAt    time.Time
	lyricReadyAt   time.Time
	trackReported  bool
	stagedCoverOut player.CoverOutcome
	pending        *amPendingTrack
	ended          bool
	endDone        chan struct{}
	trackEmitWG    sync.WaitGroup

	// testBeforeTrackEmit / testBeforeDetachEmit 仅供单测注入并发交错屏障，生产恒为 nil。
	testBeforeTrackEmit  func()
	testBeforeDetachEmit func()
}

// FlushRUM 在主进程退出前同步结算活动 Apple Music 会话的待报曲目与 detached 事件（并发安全且幂等）。
func (p *AppleMusicPlayer) FlushRUM() {
	p.rumGate.Flush(func(r *amSessionRUM) {
		r.end("stopped")
	})
}

func (p *AppleMusicPlayer) registerSessionRUM(s *session, attachMs int64) *amSessionRUM {
	if attachMs < 0 {
		attachMs = 0
	}
	now := s.now()
	rumSess, ok := p.rumGate.Register(
		func() *amSessionRUM {
			r := s.rum
			if r == nil {
				r = newAMSessionRUM(s.now)
			}
			r.mu.Lock()
			r.sessionActive = true
			r.sessionStart = now
			r.ended = false
			r.endDone = make(chan struct{})
			r.mu.Unlock()
			return r
		},
		func(*amSessionRUM) {
			if p.testBeforeAttach != nil {
				p.testBeforeAttach()
			}
			rum.Track(rum.CatPlayer, "player_session", rum.Props{
				"player":    PlayerName,
				"action":    "attached",
				"attach_ms": attachMs,
			})
		},
	)
	if !ok {
		s.rum = nil
		return nil
	}
	s.rum = rumSess
	return rumSess
}

func (p *AppleMusicPlayer) endSessionRUM(r *amSessionRUM, cause string) {
	if r == nil {
		return
	}
	p.rumGate.End(r, func(sess *amSessionRUM) {
		sess.end(cause)
	})
}

func newAMSessionRUM(clock func() time.Time) *amSessionRUM {
	if clock == nil {
		clock = time.Now
	}
	return &amSessionRUM{
		nowFn:          clock,
		compatSent:     make(map[string]bool),
		stagedCoverOut: player.CoverOutcome{Action: player.CoverActionNone},
	}
}

func (r *amSessionRUM) now() time.Time {
	if r != nil && r.nowFn != nil {
		return r.nowFn()
	}
	return time.Now()
}

func amLyricSnapshot(docHash uint64, lyrics []krc.Line, pureHint string) (src, format string, lineCount int, hasVerbatim, hasTrans, isPureMusic bool) {
	src, format = "none", "none"
	if docHash != 0 {
		src, format = "memory_ttml", "ttml"
	}
	lineCount = len(lyrics)
	for _, l := range lyrics {
		if len(l.Detailed.Words) > 0 {
			hasVerbatim = true
		}
		if l.SubText != "" {
			hasTrans = true
		}
	}
	isPureMusic = pureHint != ""
	return
}

func (r *amSessionRUM) isTrackReported() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	v := r.trackReported
	r.mu.Unlock()
	return v
}

func (r *amSessionRUM) onAnnounced(now, songChangedAt time.Time, docHash uint64, lyrics []krc.Line, pureHint string, lyricsReady bool) {
	if r == nil {
		return
	}
	src, format, lineCount, hasVerbatim, hasTrans, isPure := amLyricSnapshot(docHash, lyrics, pureHint)
	r.mu.Lock()
	r.announcedAt = now
	if lyricsReady && r.lyricReadyAt.IsZero() {
		r.lyricReadyAt = now
	}
	if !r.ended && !r.trackReported {
		r.pending = &amPendingTrack{
			songChangedAt: songChangedAt,
			lyricReadyAt:  r.lyricReadyAt,
			lyricSource:   src,
			lyricFormat:   format,
			lineCount:     lineCount,
			hasVerbatim:   hasVerbatim,
			hasTrans:      hasTrans,
			isPureMusic:   isPure,
			coverOut:      r.stagedCoverOut,
		}
	}
	r.mu.Unlock()
}

func (r *amSessionRUM) onLyricsUpdated(now time.Time, docHash uint64, lyrics []krc.Line, pureHint string) {
	if r == nil {
		return
	}
	src, format, lineCount, hasVerbatim, hasTrans, isPure := amLyricSnapshot(docHash, lyrics, pureHint)
	r.mu.Lock()
	r.lyricReadyAt = now
	if r.pending != nil && !r.pending.emitted {
		r.pending.lyricReadyAt = now
		r.pending.lyricSource = src
		r.pending.lyricFormat = format
		r.pending.lineCount = lineCount
		r.pending.hasVerbatim = hasVerbatim
		r.pending.hasTrans = hasTrans
		r.pending.isPureMusic = isPure
	}
	r.mu.Unlock()
}

func (r *amSessionRUM) onVerdict(now time.Time, docHash uint64, lyrics []krc.Line, pureHint string) {
	if r == nil {
		return
	}
	src, format, lineCount, hasVerbatim, hasTrans, isPure := amLyricSnapshot(docHash, lyrics, pureHint)
	r.mu.Lock()
	if r.lyricReadyAt.IsZero() {
		r.lyricReadyAt = now
	}
	if r.pending != nil && !r.pending.emitted {
		if r.pending.lyricReadyAt.IsZero() {
			r.pending.lyricReadyAt = r.lyricReadyAt
		}
		r.pending.lyricSource = src
		r.pending.lyricFormat = format
		r.pending.lineCount = lineCount
		r.pending.hasVerbatim = hasVerbatim
		r.pending.hasTrans = hasTrans
		r.pending.isPureMusic = isPure
	}
	r.mu.Unlock()
}

func (r *amSessionRUM) onCoverOutcome(out player.CoverOutcome) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stagedCoverOut = out
	if r.pending != nil && !r.pending.emitted {
		r.pending.coverOut = out
	}
	r.mu.Unlock()
}

func (r *amSessionRUM) emitPending() {
	if r == nil {
		return
	}
	var flush *amPendingTrack
	var hook func()
	r.mu.Lock()
	if !r.ended && !r.trackReported && r.pending != nil && !r.pending.emitted {
		cp := *r.pending
		r.pending.emitted = true
		r.trackReported = true
		r.tracksPlayed++
		r.trackEmitWG.Add(1)
		hook = r.testBeforeTrackEmit
		flush = &cp
	}
	nowFn := r.nowFn
	r.mu.Unlock()

	if flush != nil {
		defer r.trackEmitWG.Done()
		if hook != nil {
			hook()
		}
		now := time.Now()
		if nowFn != nil {
			now = nowFn()
		}
		emitAMTrackRecognized(flush, now)
	}
}

func (r *amSessionRUM) end(disconnectCause string) {
	if r == nil {
		return
	}
	var flush *amPendingTrack
	r.mu.Lock()
	if r.ended {
		done := r.endDone
		r.mu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	if !r.sessionActive {
		r.mu.Unlock()
		return
	}
	if r.endDone == nil {
		r.endDone = make(chan struct{})
	}
	done := r.endDone
	r.ended = true
	r.sessionActive = false
	if !r.trackReported && r.pending != nil && !r.pending.emitted {
		cp := *r.pending
		r.pending.emitted = true
		r.trackReported = true
		r.tracksPlayed++
		flush = &cp
	}
	tracks := r.tracksPlayed
	start := r.sessionStart
	nowFn := r.nowFn
	trackHook := r.testBeforeTrackEmit
	detachHook := r.testBeforeDetachEmit
	r.mu.Unlock()

	defer close(done)
	r.trackEmitWG.Wait()
	now := time.Now()
	if nowFn != nil {
		now = nowFn()
	}
	if flush != nil {
		if trackHook != nil {
			trackHook()
		}
		emitAMTrackRecognized(flush, now)
	}
	if detachHook != nil {
		detachHook()
	}
	durSec := int64(now.Sub(start).Seconds())
	if durSec < 0 {
		durSec = 0
	}
	rum.Track(rum.CatPlayer, "player_session", rum.Props{
		"player":           PlayerName,
		"action":           "detached",
		"disconnect_cause": disconnectCause,
		"duration_sec":     durSec,
		"tracks_played":    tracks,
	})
}

func emitAMTrackRecognized(p *amPendingTrack, now time.Time) {
	if p == nil {
		return
	}
	var lyricMs int64
	if !p.songChangedAt.IsZero() {
		endAt := p.lyricReadyAt
		if endAt.IsZero() {
			endAt = now
		}
		lyricMs = endAt.Sub(p.songChangedAt).Milliseconds()
		if lyricMs < 0 {
			lyricMs = 0
		}
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
	player.NoteTrackRecognized()
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{
		"player":                PlayerName,
		"clock_mode":            "memory_twin",
		"lyric_source":          lyricSrc,
		"lyric_format":          lyricFmt,
		"line_count":            p.lineCount,
		"has_verbatim":          p.hasVerbatim,
		"has_trans":             p.hasTrans,
		"has_roma":              false,
		"has_roma_detailed":     false,
		"has_furigana":          false,
		"is_pure_music":         p.isPureMusic,
		"cover_action":          coverAction,
		"cover_fallback_reason": p.coverOut.FallbackReason,
		"lyric_ms":              lyricMs,
		"cover_ms":              p.coverOut.DurationMs,
	})
}

func (s *session) startSessionRUM(attachMs int64) {
	if s.p != nil {
		s.p.registerSessionRUM(s, attachMs)
		return
	}
	if s.rum == nil {
		return
	}
	if attachMs < 0 {
		attachMs = 0
	}
	now := s.now()
	s.rum.mu.Lock()
	s.rum.sessionActive = true
	s.rum.sessionStart = now
	s.rum.ended = false
	s.rum.endDone = make(chan struct{})
	s.rum.mu.Unlock()

	rum.Track(rum.CatPlayer, "player_session", rum.Props{
		"player":    PlayerName,
		"action":    "attached",
		"attach_ms": attachMs,
	})
}

func (s *session) endSessionRUM(disconnectCause string) {
	if s.p != nil {
		s.p.endSessionRUM(s.rum, disconnectCause)
		return
	}
	s.rum.end(disconnectCause)
}

func (s *session) resetTrackRUM(keepReported bool) {
	if s.rum == nil {
		return
	}
	s.rum.mu.Lock()
	s.rum.announcedAt = time.Time{}
	s.rum.lyricReadyAt = time.Time{}
	s.rum.stagedCoverOut = player.CoverOutcome{Action: player.CoverActionNone}
	s.rum.pending = nil
	if !keepReported {
		s.rum.trackReported = false
	}
	s.rum.mu.Unlock()
}

func (s *session) emitCompat(signal string, elapsedMs int64) {
	if s.rum == nil || signal == "" {
		return
	}
	s.rum.mu.Lock()
	if s.rum.ended {
		s.rum.mu.Unlock()
		return
	}
	if s.rum.compatSent == nil {
		s.rum.compatSent = make(map[string]bool)
	}
	if s.rum.compatSent[signal] {
		s.rum.mu.Unlock()
		return
	}
	s.rum.compatSent[signal] = true
	s.rum.mu.Unlock()

	props := rum.Props{
		"player": PlayerName,
		"signal": signal,
	}
	if elapsedMs > 0 {
		props["elapsed_ms"] = elapsedMs
	}
	rum.Track(rum.CatPlayer, "player_compat_signal", props)
}

func (s *session) onMetadataBlocked(now time.Time) {
	if s.rum == nil {
		return
	}
	s.rum.mu.Lock()
	if s.rum.ended || !s.rum.metaBlockedAt.IsZero() {
		s.rum.mu.Unlock()
		return
	}
	s.rum.metaBlockedAt = now
	s.rum.mu.Unlock()

	rum.Track(rum.CatPlayer, "player_blocked", rum.Props{
		"player": PlayerName,
		"phase":  "blocked",
		"reason": "metadata_unavailable_10s",
	})
}

func (s *session) onMetadataRecovered(now time.Time) {
	if s.rum == nil {
		return
	}
	s.rum.mu.Lock()
	if s.rum.metaBlockedAt.IsZero() {
		s.rum.mu.Unlock()
		return
	}
	blockedSec := int64(now.Sub(s.rum.metaBlockedAt).Seconds())
	if blockedSec < 0 {
		blockedSec = 0
	}
	s.rum.metaBlockedAt = time.Time{}
	s.rum.mu.Unlock()

	rum.Track(rum.CatPlayer, "player_blocked", rum.Props{
		"player":      PlayerName,
		"phase":       "recovered",
		"reason":      "metadata_unavailable_10s",
		"blocked_sec": blockedSec,
	})
}

func (s *session) tryEmitTrackRecognized() {
	if !s.announced {
		return
	}
	if s.needLyrics && !s.verdictSent {
		return
	}
	// 仅在当前曲目确实有待获取封面且缩放仍在飞时等待；上一首残留的 coverFly 不得阻塞无封面新歌结算。
	if s.song.coverHash != 0 && s.song.coverHash != s.coverLast.hash && s.coverFly {
		return
	}
	s.rum.emitPending()
}

func (s *session) flushTrackRecognized() {
	if !s.announced {
		return
	}
	s.rum.emitPending()
}

func (s *session) emitLatePanelLyricUpdate(now time.Time) {
	if s.rum == nil {
		return
	}
	s.rum.mu.Lock()
	if s.rum.ended {
		s.rum.mu.Unlock()
		return
	}
	annAt := s.rum.announcedAt
	s.rum.mu.Unlock()

	var sinceMs int64
	if !annAt.IsZero() {
		sinceMs = now.Sub(annAt).Milliseconds()
		if sinceMs < 0 {
			sinceMs = 0
		}
	}
	rum.Track(rum.CatLyricTrack, "track_lyric_updated", rum.Props{
		"player":            PlayerName,
		"reason":            "late_panel_opened",
		"since_announce_ms": sinceMs,
	})
}

func emitAMClockGuard(outcome string, deltaSec float64, held time.Duration) {
	heldMs := held.Milliseconds()
	if heldMs < 0 {
		heldMs = 0
	}
	rum.Track(rum.CatDiagFine, "playback_clock_guard", rum.Props{
		"player":     PlayerName,
		"guard_kind": "jump_hold_300ms",
		"outcome":    outcome,
		"delta_ms":   int64(math.Round(deltaSec * 1000)),
		"held_ms":    heldMs,
	})
}

func emitAMScanSample(trigger string, durMs int64, capped bool, verdict string) {
	if durMs < 0 {
		durMs = 0
	}
	rum.Track(rum.CatDiagFine, "scan_perf_sample", rum.Props{
		"player":      PlayerName,
		"scan_kind":   "am_full_scan",
		"trigger":     trigger,
		"duration_ms": durMs,
		"capped":      capped,
		"verdict":     verdict,
	})
}
