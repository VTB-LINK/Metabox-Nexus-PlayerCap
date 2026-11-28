package sodamusic

import (
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/krc"
	"Metabox-Nexus-PlayerCap/rum"
)

type sodaAttachMeta struct {
	attachMs int64
	version  string
}

// sodaBlockTracker 跟踪汽水音乐受阻态（store_install/no_admin/cdp_port_not_ready）的边沿去重与恢复耗时。
type sodaBlockTracker struct {
	reason string
	since  time.Time
	now    func() time.Time
}

func (b *sodaBlockTracker) nowTime() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *sodaBlockTracker) onBlocked(reason string) {
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

func (b *sodaBlockTracker) onNotRunning() {
	b.reason = ""
	b.since = time.Time{}
}

func (b *sodaBlockTracker) onRecovered() {
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

func sodaLyricCapabilities(lines []krc.Line) (format string, hasVerbatim, hasTrans, hasRoma, hasRomaDetailed bool) {
	if len(lines) == 0 {
		return "none", false, false, false, false
	}
	format = "krc"
	for _, l := range lines {
		if len(l.Detailed.Words) > 0 {
			hasVerbatim = true
		}
		if l.SubText != "" {
			hasTrans = true
		}
		if l.RomaText != "" {
			hasRoma = true
		}
		if len(l.RomaDetailed.Words) > 0 {
			hasRomaDetailed = true
		}
	}
	return format, hasVerbatim, hasTrans, hasRoma, hasRomaDetailed
}

type sodaPendingTrack struct {
	seq             uint64
	startedAt       time.Time
	lyricSource     string
	lyricFormat     string
	lineCount       int
	hasVerbatim     bool
	hasTrans        bool
	hasRoma         bool
	hasRomaDetailed bool
	isPureMusic     bool
	romaOutcome     string
	lyricMs         int64

	lyricReady bool
	coverWait  bool
	coverReady bool
	coverOut   player.CoverOutcome
	emitted    bool
}

// sodaSessionRUM 管理单次汽水音乐会话的 RUM 生命周期、兼容退化限频、识曲结算与退出完成屏障。
// 所有共享状态均受 mu 保护；持锁期间绝不调用 rum.Track 或 player.NoteTrackRecognized。
type sodaSessionRUM struct {
	mu           sync.Mutex
	version      string
	startedAt    time.Time
	tracksPlayed int
	compatSent   map[string]bool
	pending      *sodaPendingTrack
	ended        bool
	endDone      chan struct{}
	trackEmitWG  sync.WaitGroup

	// testBeforeTrackEmit / testBeforeDetachEmit 仅供单测注入并发交错屏障，生产恒为 nil。
	testBeforeTrackEmit  func()
	testBeforeDetachEmit func()
}

// FlushRUM 在主进程退出前同步结算活动汽水会话的待报曲目与 detached 事件（并发安全且幂等）。
func (p *SodaMusicPlayer) FlushRUM() {
	p.rumGate.Flush(func(s *sodaSessionRUM) {
		s.end("stopped")
	})
}

func (p *SodaMusicPlayer) startSessionRUM(version string, attachMs int64) *sodaSessionRUM {
	ver := player.SanitizeVersion(version)
	if attachMs < 0 {
		attachMs = 0
	}
	sess, ok := p.rumGate.Register(
		func() *sodaSessionRUM {
			return &sodaSessionRUM{
				version:    ver,
				startedAt:  time.Now(),
				compatSent: make(map[string]bool),
				endDone:    make(chan struct{}),
			}
		},
		func(*sodaSessionRUM) {
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

func (p *SodaMusicPlayer) endSessionRUM(s *sodaSessionRUM, cause string) {
	if s == nil {
		return
	}
	p.rumGate.End(s, func(sess *sodaSessionRUM) {
		sess.end(cause)
	})
}

func (p *SodaMusicPlayer) setSessionAttachMeta(attachMs int64, version string) {
	p.attachMetaMu.Lock()
	p.attachMeta = sodaAttachMeta{
		attachMs: attachMs,
		version:  version,
	}
	p.attachMetaMu.Unlock()
}

func (p *SodaMusicPlayer) takeSessionAttachMeta() sodaAttachMeta {
	p.attachMetaMu.Lock()
	m := p.attachMeta
	p.attachMeta = sodaAttachMeta{}
	p.attachMetaMu.Unlock()
	return m
}

func newSodaSessionRUM(version string, attachMs int64) *sodaSessionRUM {
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
	return &sodaSessionRUM{
		version:    ver,
		startedAt:  time.Now(),
		compatSent: make(map[string]bool),
		endDone:    make(chan struct{}),
	}
}

func (s *sodaSessionRUM) onCompat(signal, detailCode string) {
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
	rum.Track(rum.CatPlayer, "player_compat_signal", props)
}

func (s *sodaSessionRUM) finishTrackEmit(p *sodaPendingTrack, hook func()) {
	defer s.trackEmitWG.Done()
	if hook != nil {
		hook()
	}
	emitSodaTrackRecognized(p)
}

func (s *sodaSessionRUM) beginTrack(seq uint64, waitCover bool) {
	if s == nil {
		return
	}
	var flush *sodaPendingTrack
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
	s.pending = &sodaPendingTrack{
		seq:       seq,
		startedAt: time.Now(),
		coverWait: waitCover,
		coverOut:  player.CoverOutcome{Action: player.CoverActionNone},
	}
	s.mu.Unlock()

	if flush != nil {
		s.finishTrackEmit(flush, hook)
	}
}

func (s *sodaSessionRUM) onTrackLyricReady(seq uint64, lyricSource string, lines []krc.Line, pureHint string, lyricMs int64, waitCover bool, romaOutcome string) {
	if s == nil {
		return
	}
	var readyToEmit *sodaPendingTrack
	var hook func()
	s.mu.Lock()
	p := s.pending
	if !s.ended && p != nil && p.seq == seq && !p.emitted && !p.lyricReady {
		if lyricMs < 0 {
			lyricMs = 0
		}
		p.lyricMs = lyricMs
		p.lyricSource = lyricSource
		p.lineCount = len(lines)
		p.isPureMusic = pureHint != ""
		fmtStr, hasV, hasT, hasR, hasRD := sodaLyricCapabilities(lines)
		p.lyricFormat = fmtStr
		p.hasVerbatim = hasV
		p.hasTrans = hasT
		if hasR {
			p.hasRoma = true
		}
		if hasRD {
			p.hasRomaDetailed = true
		}
		if p.romaOutcome == "" || p.romaOutcome == "pending" {
			p.romaOutcome = romaOutcome
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

func (s *sodaSessionRUM) onCoverOutcome(seq uint64, out player.CoverOutcome) {
	if s == nil {
		return
	}
	var readyToEmit *sodaPendingTrack
	var hook func()
	s.mu.Lock()
	if !s.ended && s.pending != nil && s.pending.seq == seq && !s.pending.emitted {
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

// onKugouRomaResolved 记录异步借酷狗音译的最终结果：
// 若当前曲目尚未发出 track_recognized（如仍在等封面），直接回写进 pending；
// 若已发出，则以 track_lyric_updated{reason: "kugou_roma_resolved"} 补充，绝不重复调用 NoteTrackRecognized。
func (s *sodaSessionRUM) onKugouRomaResolved(seq uint64, outcome string, hasRoma, hasRomaDetailed bool, elapsedMs int64) {
	if s == nil || outcome == "" {
		return
	}
	if elapsedMs < 0 {
		elapsedMs = 0
	}
	var (
		emitUpdate      bool
		sinceAnnounceMs int64
	)
	s.mu.Lock()
	if !s.ended && s.pending != nil && s.pending.seq == seq {
		if !s.pending.emitted {
			s.pending.romaOutcome = outcome
			if hasRoma {
				s.pending.hasRoma = true
			}
			if hasRomaDetailed {
				s.pending.hasRomaDetailed = true
			}
		} else {
			sinceAnnounceMs = time.Since(s.pending.startedAt).Milliseconds()
			if sinceAnnounceMs < 0 {
				sinceAnnounceMs = 0
			}
			s.trackEmitWG.Add(1)
			emitUpdate = true
		}
	}
	s.mu.Unlock()

	if emitUpdate {
		defer s.trackEmitWG.Done()
		rum.Track(rum.CatLyricTrack, "track_lyric_updated", rum.Props{
			"player":                  PlayerName,
			"reason":                  "kugou_roma_resolved",
			"soda_kugou_roma_outcome": outcome,
			"has_roma":                hasRoma,
			"has_roma_detailed":       hasRomaDetailed,
			"since_announce_ms":       sinceAnnounceMs,
			"elapsed_ms":              elapsedMs,
		})
	}
}

func (s *sodaSessionRUM) end(disconnectCause string) {
	if s == nil {
		return
	}
	var flush *sodaPendingTrack
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
		emitSodaTrackRecognized(flush)
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

func emitSodaTrackRecognized(p *sodaPendingTrack) {
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
	romaOutcome := p.romaOutcome
	if romaOutcome == "" {
		romaOutcome = "skipped"
	}
	player.NoteTrackRecognized()
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{
		"player":                  PlayerName,
		"clock_mode":              "cdp_edge",
		"lyric_source":            lyricSrc,
		"lyric_format":            lyricFmt,
		"line_count":              p.lineCount,
		"has_verbatim":            p.hasVerbatim,
		"has_trans":               p.hasTrans,
		"has_roma":                p.hasRoma,
		"has_roma_detailed":       p.hasRomaDetailed,
		"has_furigana":            false,
		"is_pure_music":           p.isPureMusic,
		"soda_kugou_roma_outcome": romaOutcome,
		"cover_action":            coverAction,
		"cover_fallback_reason":   p.coverOut.FallbackReason,
		"lyric_ms":                p.lyricMs,
		"cover_ms":                p.coverOut.DurationMs,
	})
}
