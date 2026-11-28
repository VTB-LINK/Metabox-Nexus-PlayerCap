package cloudmusic

import (
	"math"
	"strings"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/cdp"
	"Metabox-Nexus-PlayerCap/rum"
)

type cmAttachMeta struct {
	attachMs int64
	version  string
}

// cmBlockTracker 跟踪网易云受阻态（商店版/v2/无权限/CDP未就绪）的边沿去重与恢复耗时。
type cmBlockTracker struct {
	reason  string
	version string
	since   time.Time
	now     func() time.Time
}

func (b *cmBlockTracker) nowTime() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *cmBlockTracker) onBlocked(reason, version string) {
	if reason == "" {
		return
	}
	ver := player.SanitizeVersion(version)
	if b.reason == reason && b.version == ver && !b.since.IsZero() {
		return
	}
	b.reason = reason
	b.version = ver
	b.since = b.nowTime()

	props := rum.Props{
		"player": PlayerName,
		"phase":  "blocked",
		"reason": reason,
	}
	if ver != "" {
		props["player_version"] = ver
	}
	rum.Track(rum.CatPlayer, "player_blocked", props)
}

func (b *cmBlockTracker) onNotRunning() {
	b.reason = ""
	b.version = ""
	b.since = time.Time{}
}

func (b *cmBlockTracker) onRecovered() {
	if b.reason == "" || b.since.IsZero() {
		return
	}
	blockedSec := int64(b.nowTime().Sub(b.since).Seconds())
	if blockedSec < 0 {
		blockedSec = 0
	}
	reason := b.reason
	ver := b.version
	b.onNotRunning()

	props := rum.Props{
		"player":      PlayerName,
		"phase":       "recovered",
		"reason":      reason,
		"blocked_sec": blockedSec,
	}
	if ver != "" {
		props["player_version"] = ver
	}
	rum.Track(rum.CatPlayer, "player_blocked", props)
}

func emitCMControlledRestart(outcome string, elapsedMs int64) {
	if elapsedMs < 0 {
		elapsedMs = 0
	}
	rum.Track(rum.CatPlayer, "player_intervention", rum.Props{
		"player":     PlayerName,
		"kind":       "controlled_restart",
		"outcome":    outcome,
		"elapsed_ms": elapsedMs,
	})
}

// classifyExtractErrCode 将 Fiber/Redux 提取报错归类为固定枚举，绝不透传错误原文或业务敏感字段。
func classifyExtractErrCode(err error) string {
	if err == nil {
		return "other"
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "no react container"):
		return "no_react_container"
	case strings.Contains(s, "store not found"):
		return "store_not_found"
	case strings.Contains(s, "closed") || strings.Contains(s, "eof"):
		return "cdp_closed"
	case strings.Contains(s, "timeout"):
		return "cdp_timeout"
	default:
		return "extract_err"
	}
}

func cmLyricCapabilities(lines []cdp.ExtractedLyric) (format string, hasVerbatim, hasTrans, hasRoma, hasRomaDetailed bool) {
	if len(lines) == 0 {
		return "none", false, false, false, false
	}
	for _, l := range lines {
		if len(l.TextDetailed.Words) > 0 {
			hasVerbatim = true
		}
		if l.SubText != "" {
			hasTrans = true
		}
		if l.RomaText != "" {
			hasRoma = true
		}
		if len(l.RomaTextDetailed.Words) > 0 {
			hasRomaDetailed = true
		}
	}
	if hasVerbatim {
		format = "yrc"
	} else {
		format = "lrc"
	}
	return format, hasVerbatim, hasTrans, hasRoma, hasRomaDetailed
}

type cmPendingTrack struct {
	gen             uint64
	startedAt       time.Time
	lyricSource     string
	lyricFormat     string
	lineCount       int
	hasVerbatim     bool
	hasTrans        bool
	hasRoma         bool
	hasRomaDetailed bool
	isPureMusic     bool
	lyricMs         int64

	lyricReady bool
	coverGen   uint64
	coverWait  bool
	coverReady bool
	coverOut   player.CoverOutcome
	emitted    bool
}

// cmSessionRUM 管理单次网易云会话的 RUM 生命周期、兼容退化限频、识曲结算与退出完成屏障。
// 所有可变字段受 mu 保护；持锁期间绝不调用 rum.Track 或 player.NoteTrackRecognized。
type cmSessionRUM struct {
	mu           sync.Mutex
	version      string
	startedAt    time.Time
	tracksPlayed int
	compatSent   map[string]bool
	pending      *cmPendingTrack
	ended        bool
	endDone      chan struct{}
	trackEmitWG  sync.WaitGroup

	// testBeforeTrackEmit / testBeforeDetachEmit 仅供单测注入并发交错屏障，生产恒为 nil。
	testBeforeTrackEmit  func()
	testBeforeDetachEmit func()
}

// FlushRUM 在主进程退出前同步结算活动网易云会话的待报曲目与 detached 事件（并发安全且幂等）。
func (p *CloudMusicPlayer) FlushRUM() {
	p.rumGate.Flush(func(s *cmSessionRUM) {
		s.end("stopped")
	})
}

func (p *CloudMusicPlayer) startSessionRUM(version string, attachMs int64) *cmSessionRUM {
	ver := player.SanitizeVersion(version)
	if attachMs < 0 {
		attachMs = 0
	}
	sess, ok := p.rumGate.Register(
		func() *cmSessionRUM {
			return &cmSessionRUM{
				version:    ver,
				startedAt:  time.Now(),
				compatSent: make(map[string]bool),
				endDone:    make(chan struct{}),
			}
		},
		func(*cmSessionRUM) {
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

func (p *CloudMusicPlayer) endSessionRUM(s *cmSessionRUM, cause string) {
	if s == nil {
		return
	}
	p.rumGate.End(s, func(sess *cmSessionRUM) {
		sess.end(cause)
	})
}

func (p *CloudMusicPlayer) setSessionAttachMeta(attachMs int64, version string) {
	p.attachMetaMu.Lock()
	p.attachMeta = cmAttachMeta{
		attachMs: attachMs,
		version:  player.SanitizeVersion(version),
	}
	p.attachMetaMu.Unlock()
}

func (p *CloudMusicPlayer) takeSessionAttachMeta() cmAttachMeta {
	p.attachMetaMu.Lock()
	m := p.attachMeta
	p.attachMeta = cmAttachMeta{}
	p.attachMetaMu.Unlock()
	return m
}

func newCMSessionRUM(version string, attachMs int64) *cmSessionRUM {
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
	return &cmSessionRUM{
		version:    ver,
		startedAt:  time.Now(),
		compatSent: make(map[string]bool),
		endDone:    make(chan struct{}),
	}
}

func (s *cmSessionRUM) onCompat(signal, detailCode string) {
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

func (s *cmSessionRUM) finishTrackEmit(p *cmPendingTrack, hook func()) {
	defer s.trackEmitWG.Done()
	if hook != nil {
		hook()
	}
	emitCMTrackRecognized(p)
}

func (s *cmSessionRUM) beginTrack(gen uint64) {
	if s == nil {
		return
	}
	var flush *cmPendingTrack
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
	s.pending = &cmPendingTrack{
		gen:       gen,
		startedAt: time.Now(),
		coverOut:  player.CoverOutcome{Action: player.CoverActionNone},
	}
	s.mu.Unlock()

	if flush != nil {
		s.finishTrackEmit(flush, hook)
	}
}

func (s *cmSessionRUM) bindCoverGen(trackGen, coverGen uint64, willFetch bool) {
	if s == nil {
		return
	}
	var readyToEmit *cmPendingTrack
	var hook func()
	s.mu.Lock()
	if !s.ended && s.pending != nil && s.pending.gen == trackGen && !s.pending.emitted {
		s.pending.coverGen = coverGen
		if willFetch {
			s.pending.coverWait = true
			s.pending.coverReady = false
		} else {
			s.pending.coverWait = false
			s.pending.coverReady = true
			s.pending.coverOut = player.CoverOutcome{Action: player.CoverActionNone}
			if s.pending.lyricReady {
				cp := *s.pending
				s.pending.emitted = true
				s.tracksPlayed++
				s.trackEmitWG.Add(1)
				hook = s.testBeforeTrackEmit
				readyToEmit = &cp
			}
		}
	}
	s.mu.Unlock()

	if readyToEmit != nil {
		s.finishTrackEmit(readyToEmit, hook)
	}
}

func (s *cmSessionRUM) onCoverOutcome(coverGen uint64, out player.CoverOutcome) {
	if s == nil {
		return
	}
	var readyToEmit *cmPendingTrack
	var hook func()
	s.mu.Lock()
	if !s.ended && s.pending != nil && !s.pending.emitted && s.pending.coverGen == coverGen {
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

func (s *cmSessionRUM) onTrackLyricSettled(gen uint64, lyricSource string, lines []cdp.ExtractedLyric, isPureMusic bool, lyricMs int64) {
	if s == nil {
		return
	}
	var readyToEmit *cmPendingTrack
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
		p.lyricFormat, p.hasVerbatim, p.hasTrans, p.hasRoma, p.hasRomaDetailed = cmLyricCapabilities(lines)
		p.lyricReady = true
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

func (s *cmSessionRUM) end(disconnectCause string) {
	if s == nil {
		return
	}
	var flush *cmPendingTrack
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
		emitCMTrackRecognized(flush)
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

func emitCMTrackRecognized(p *cmPendingTrack) {
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
	player.NoteTrackRecognized()
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{
		"player":                PlayerName,
		"clock_mode":            "cdp_dom_sync",
		"lyric_source":          lyricSrc,
		"lyric_format":          lyricFmt,
		"line_count":            p.lineCount,
		"has_verbatim":          p.hasVerbatim,
		"has_trans":             p.hasTrans,
		"has_roma":              p.hasRoma,
		"has_roma_detailed":     p.hasRomaDetailed,
		"has_furigana":          false,
		"is_pure_music":         p.isPureMusic,
		"cover_action":          coverAction,
		"cover_fallback_reason": p.coverOut.FallbackReason,
		"lyric_ms":              p.lyricMs,
		"cover_ms":              p.coverOut.DurationMs,
	})
}

func emitCMDOMSyncGuard(deltaSec float32) {
	rum.Track(rum.CatDiagFine, "playback_clock_guard", rum.Props{
		"player":     PlayerName,
		"guard_kind": "dom_sync_200ms",
		"outcome":    "calibrated",
		"delta_ms":   int64(math.Round(float64(deltaSec) * 1000)),
	})
}

func fetchCoverWithFallbackOutcome(fetch func(string) (string, player.CoverOutcome), normalized, raw string) (string, player.CoverOutcome) {
	b64, out := fetch(normalized)
	if b64 != "" || normalized == raw {
		return b64, out
	}
	b64Fallback, outFallback := fetch(raw)
	outFallback.DurationMs += out.DurationMs
	return b64Fallback, outFallback
}
