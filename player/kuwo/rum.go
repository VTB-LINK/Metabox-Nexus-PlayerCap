package kuwo

import (
	"errors"
	"strings"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cover"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
	"Metabox-Nexus-PlayerCap/player/kuwo/watchdog"
	"Metabox-Nexus-PlayerCap/rum"
)

type kuwoAttachMeta struct {
	attachMs int64
	build    string
}

// extractKuwoChannelSuffix 从酷我构建串（如 "9.6.0.0_UG1"）或裸后缀提取并经白名单正则校验渠道名，非法归一 "other"。
func extractKuwoChannelSuffix(build string) string {
	s := strings.TrimSpace(build)
	if len(s) > len("9.6.0.0_") && strings.EqualFold(s[:len("9.6.0.0_")], "9.6.0.0_") {
		s = s[len("9.6.0.0_"):]
	}
	return watchdog.SanitizeChannelSuffix(s)
}

func kuwoClockModeFromDevice(d pos.DeviceKind) string {
	switch d {
	case pos.DeviceDirectSound:
		return "ds"
	case pos.DeviceWaveOut:
		return "waveout"
	case pos.DeviceWASAPI:
		return "wasapi"
	default:
		return ""
	}
}

// kuwoBlockTracker 跟踪酷我受阻态（store_install/no_admin/kuwo_unsupported_core/kuwo_ambiguous_proc/cdp_port_not_ready/version_unsupported）
// 的边沿去重与恢复耗时。
type kuwoBlockTracker struct {
	reason string
	since  time.Time
	now    func() time.Time
}

func (b *kuwoBlockTracker) nowTime() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *kuwoBlockTracker) onBlocked(reason string) {
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

func (b *kuwoBlockTracker) onNotRunning() {
	b.reason = ""
	b.since = time.Time{}
}

func (b *kuwoBlockTracker) onRecoveredExcept(excluded ...string) {
	if b.reason == "" || b.since.IsZero() {
		return
	}
	for _, ex := range excluded {
		if b.reason == ex {
			return
		}
	}
	b.onRecovered()
}

func (b *kuwoBlockTracker) onRecovered() {
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

func kuwoLyricCapabilities(lines []player.LyricLine) (format string, hasVerbatim, hasTrans bool) {
	if len(lines) == 0 {
		return "none", false, false
	}
	for _, l := range lines {
		if len(l.TextDetailed.Words) > 0 {
			hasVerbatim = true
		}
		if l.SubText != "" {
			hasTrans = true
		}
	}
	if hasVerbatim {
		format = "lrcx"
	} else {
		format = "lrc"
	}
	return format, hasVerbatim, hasTrans
}

type kuwoPendingTrack struct {
	gen                uint64
	startedAt          time.Time
	clockMode          string
	lyricSource        string
	lyricFormat        string
	lineCount          int
	hasVerbatim        bool
	hasTrans           bool
	isPureMusic        bool
	supplementDecision string
	lyricMs            int64

	lyricReady bool
	coverWait  bool
	coverReady bool
	coverOut   player.CoverOutcome
	emitted    bool
}

// kuwoSessionRUM 管理单次酷我会话的 RUM 生命周期、兼容退化限频、识曲结算与退出完成屏障。
// 所有共享状态均受 mu 保护；持锁期间绝不调用 rum.Track 或 player.NoteTrackRecognized。
type kuwoSessionRUM struct {
	mu           sync.Mutex
	version      string
	exeVariant   string
	startedAt    time.Time
	tracksPlayed int
	compatSent   map[string]bool
	pending      *kuwoPendingTrack
	ended        bool
	endDone      chan struct{}
	trackEmitWG  sync.WaitGroup

	// testBeforeTrackEmit / testBeforeDetachEmit 仅供单测注入并发交错屏障，生产恒为 nil。
	testBeforeTrackEmit  func()
	testBeforeDetachEmit func()
}

// FlushRUM 在主进程退出前同步结算活动酷我会话的待报曲目与 detached 事件（并发安全且幂等）。
func (p *KuwoPlayer) FlushRUM() {
	p.rumGate.Flush(func(s *kuwoSessionRUM) {
		s.end("stopped")
	})
}

func (p *KuwoPlayer) startSessionRUM(build string, attachMs int64) *kuwoSessionRUM {
	ver := player.SanitizeVersion(build)
	variant := extractKuwoChannelSuffix(build)
	if attachMs < 0 {
		attachMs = 0
	}
	sess, ok := p.rumGate.Register(
		func() *kuwoSessionRUM {
			return &kuwoSessionRUM{
				version:    ver,
				exeVariant: variant,
				startedAt:  time.Now(),
				compatSent: make(map[string]bool),
				endDone:    make(chan struct{}),
			}
		},
		func(*kuwoSessionRUM) {
			if p.testBeforeAttach != nil {
				p.testBeforeAttach()
			}
			rum.Track(rum.CatPlayer, "player_session", rum.Props{
				"player":         PlayerName,
				"action":         "attached",
				"player_version": ver,
				"exe_variant":    variant,
				"attach_ms":      attachMs,
			})
		},
	)
	if !ok {
		return nil
	}
	return sess
}

func (p *KuwoPlayer) endSessionRUM(s *kuwoSessionRUM, cause string) {
	if s == nil {
		return
	}
	p.rumGate.End(s, func(sess *kuwoSessionRUM) {
		sess.end(cause)
	})
}

func (p *KuwoPlayer) setSessionAttachMeta(attachMs int64, build string) {
	p.attachMetaMu.Lock()
	p.attachMeta = kuwoAttachMeta{
		attachMs: attachMs,
		build:    build,
	}
	p.attachMetaMu.Unlock()
}

func (p *KuwoPlayer) takeSessionAttachMeta() kuwoAttachMeta {
	p.attachMetaMu.Lock()
	m := p.attachMeta
	p.attachMeta = kuwoAttachMeta{}
	p.attachMetaMu.Unlock()
	return m
}

func newKuwoSessionRUM(build string, attachMs int64) *kuwoSessionRUM {
	ver := player.SanitizeVersion(build)
	variant := extractKuwoChannelSuffix(build)
	if attachMs < 0 {
		attachMs = 0
	}
	rum.Track(rum.CatPlayer, "player_session", rum.Props{
		"player":         PlayerName,
		"action":         "attached",
		"player_version": ver,
		"exe_variant":    variant,
		"attach_ms":      attachMs,
	})
	return &kuwoSessionRUM{
		version:    ver,
		exeVariant: variant,
		startedAt:  time.Now(),
		compatSent: make(map[string]bool),
		endDone:    make(chan struct{}),
	}
}

func (s *kuwoSessionRUM) onCompat(signal, detailCode string) {
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

func (s *kuwoSessionRUM) finishTrackEmit(p *kuwoPendingTrack, hook func()) {
	defer s.trackEmitWG.Done()
	if hook != nil {
		hook()
	}
	emitKuwoTrackRecognized(p)
}

func (s *kuwoSessionRUM) beginTrack(gen uint64, clockMode string) {
	if s == nil {
		return
	}
	if clockMode == "" {
		clockMode = "native_state"
	}
	var flush *kuwoPendingTrack
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
	s.pending = &kuwoPendingTrack{
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

func (s *kuwoSessionRUM) updateClockMode(gen uint64, clockMode string) {
	if s == nil || clockMode == "" {
		return
	}
	s.mu.Lock()
	if !s.ended && s.pending != nil && s.pending.gen == gen && !s.pending.emitted {
		s.pending.clockMode = clockMode
	}
	s.mu.Unlock()
}

func (s *kuwoSessionRUM) noteSupplement(gen uint64, dec string) {
	if s == nil || dec == "" {
		return
	}
	s.mu.Lock()
	if !s.ended && s.pending != nil && s.pending.gen == gen && !s.pending.emitted {
		s.pending.supplementDecision = player.CombineSupplementDecisions(s.pending.supplementDecision, dec)
	}
	s.mu.Unlock()
}

func (s *kuwoSessionRUM) onCoverOutcome(gen uint64, art cover.Result, err error) {
	if s == nil {
		return
	}
	out := art.Outcome
	if err != nil {
		if errors.Is(err, cover.ErrNoCover) {
			out = player.CoverOutcome{Action: player.CoverActionNone, DurationMs: out.DurationMs}
		} else if out.Action == "" {
			out = player.CoverOutcome{Action: player.CoverActionErr, FallbackReason: "http_err", DurationMs: out.DurationMs}
		}
	} else if out.Action == "" && art.DataURI != "" {
		out.Action = player.CoverActionResized800
	}
	var readyToEmit *kuwoPendingTrack
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

func (s *kuwoSessionRUM) onTrackLyricSettled(gen uint64, lyricSource string, lines []player.LyricLine, pureHint string, lyricMs int64, waitCover bool) {
	if s == nil {
		return
	}
	var readyToEmit *kuwoPendingTrack
	var hook func()
	s.mu.Lock()
	p := s.pending
	if !s.ended && p != nil && p.gen == gen && !p.emitted && !p.lyricReady {
		if lyricMs <= 0 && !p.startedAt.IsZero() {
			lyricMs = time.Since(p.startedAt).Milliseconds()
		}
		if lyricMs < 0 {
			lyricMs = 0
		}
		p.lyricMs = lyricMs
		p.lyricSource = lyricSource
		p.lineCount = len(lines)
		p.isPureMusic = pureHint != ""
		p.lyricFormat, p.hasVerbatim, p.hasTrans = kuwoLyricCapabilities(lines)
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

func (s *kuwoSessionRUM) end(disconnectCause string) {
	if s == nil {
		return
	}
	var flush *kuwoPendingTrack
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
	variant := s.exeVariant
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
		emitKuwoTrackRecognized(flush)
	}
	if detachHook != nil {
		detachHook()
	}
	rum.Track(rum.CatPlayer, "player_session", rum.Props{
		"player":           PlayerName,
		"action":           "detached",
		"player_version":   ver,
		"exe_variant":      variant,
		"disconnect_cause": disconnectCause,
		"duration_sec":     durSec,
		"tracks_played":    tracks,
	})
}

func emitKuwoTrackRecognized(p *kuwoPendingTrack) {
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
	clkMode := p.clockMode
	if clkMode == "" {
		clkMode = "native_state"
	}
	player.NoteTrackRecognized()
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{
		"player":                PlayerName,
		"clock_mode":            clkMode,
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
		"supplement_decision":   p.supplementDecision,
		"lyric_ms":              p.lyricMs,
		"cover_ms":              p.coverOut.DurationMs,
	})
}

func emitKuwoClockGuard(guardKind, outcome string, deltaMs, heldMs int64) {
	props := rum.Props{
		"player":     PlayerName,
		"guard_kind": guardKind,
		"outcome":    outcome,
	}
	if deltaMs != 0 {
		props["delta_ms"] = deltaMs
	}
	if heldMs > 0 {
		props["held_ms"] = heldMs
	}
	rum.Track(rum.CatDiagFine, "playback_clock_guard", props)
}
