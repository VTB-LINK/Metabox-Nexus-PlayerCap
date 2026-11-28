package kugou

import (
	"errors"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/assoc"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
	"Metabox-Nexus-PlayerCap/rum"
)

// kugouAttachMeta 保存 Start 到 runSession 之间传递的脱敏接入元数据。
type kugouAttachMeta struct {
	attachMs   int64
	version    string
	exeVariant string
}

// kugouBlockTracker 跟踪酷狗受阻态（商店版/无权限/10.x/libcef失配/运行缺安装/CDP未就绪）的边沿去重与恢复耗时。
type kugouBlockTracker struct {
	reason      string
	version     string
	mismatchIdx int
	since       time.Time
	now         func() time.Time
}

func (b *kugouBlockTracker) nowTime() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *kugouBlockTracker) onBlocked(reason, version string, mismatchIdx int) {
	if reason == "" {
		return
	}
	ver := player.SanitizeVersion(version)
	if b.reason == reason && b.version == ver && b.mismatchIdx == mismatchIdx && !b.since.IsZero() {
		return
	}
	b.reason = reason
	b.version = ver
	b.mismatchIdx = mismatchIdx
	b.since = b.nowTime()

	props := rum.Props{
		"player": PlayerName,
		"phase":  "blocked",
		"reason": reason,
	}
	if ver != "" {
		props["player_version"] = ver
	}
	if mismatchIdx > 0 {
		props["mismatch_idx"] = mismatchIdx
	}
	rum.Track(rum.CatPlayer, "player_blocked", props)
}

func (b *kugouBlockTracker) onNotRunning() {
	b.reason = ""
	b.version = ""
	b.mismatchIdx = 0
	b.since = time.Time{}
}

func (b *kugouBlockTracker) onRecovered() {
	if b.reason == "" || b.since.IsZero() {
		return
	}
	blockedSec := int64(b.nowTime().Sub(b.since).Seconds())
	if blockedSec < 0 {
		blockedSec = 0
	}
	reason := b.reason
	ver := b.version
	mismatchIdx := b.mismatchIdx
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
	if mismatchIdx > 0 {
		props["mismatch_idx"] = mismatchIdx
	}
	rum.Track(rum.CatPlayer, "player_blocked", props)
}

func classifyKugouVersionBlock(err error) (reason, ver string, mismatchIdx int) {
	var vbe *watchdog.VersionBlockError
	if errors.As(err, &vbe) && vbe != nil {
		ver = player.SanitizeVersion(vbe.Version)
		if vbe.IsLibcefMismatch {
			return "libcef_mismatch", ver, vbe.MismatchIdx
		}
		return "version_unsupported", ver, 0
	}
	return "version_unsupported", "", 0
}

// sanitizeKugouPlayStatusCode 将未知 play_status 脱敏为固定短长度档枚举，绝不上报不可控原文。
func sanitizeKugouPlayStatusCode(raw string) string {
	n := len(raw)
	switch {
	case n == 0:
		return "empty"
	case n <= 8:
		return "len_1_8"
	case n <= 32:
		return "len_9_32"
	default:
		return "len_gt_32"
	}
}

func kugouLocalSourceName(src assoc.Source, fromMemory bool) string {
	switch src {
	case assoc.SourceCache:
		return "local_cache"
	case assoc.SourceAssociation:
		if fromMemory {
			return "local_assoc_mem"
		}
		return "local_assoc_db"
	default:
		return "none"
	}
}

func kugouLyricCapabilities(lines []klyric.Line) (format string, hasVerbatim, hasTrans, hasRoma, hasRomaDetailed bool) {
	if len(lines) == 0 {
		return "none", false, false, false, false
	}
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
	if hasVerbatim || hasTrans || hasRoma || hasRomaDetailed {
		format = "krc"
	} else {
		format = "lrc"
	}
	return format, hasVerbatim, hasTrans, hasRoma, hasRomaDetailed
}

type kugouPendingTrack struct {
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
	coverWait  bool
	coverReady bool
	coverOut   player.CoverOutcome
	emitted    bool
}

// kugouSessionRUM 管理单次酷狗会话的 RUM 生命周期、兼容退化去重、首轮识曲结算与退出完成屏障。
// 所有共享状态均受 mu 保护；持锁期间绝不调用 rum.Track 或 player.NoteTrackRecognized。
type kugouSessionRUM struct {
	mu           sync.Mutex
	version      string
	exeVariant   string
	startedAt    time.Time
	tracksPlayed int
	compatSent   map[string]bool
	pending      *kugouPendingTrack
	ended        bool
	endDone      chan struct{}
	trackEmitWG  sync.WaitGroup

	// testBeforeTrackEmit / testBeforeDetachEmit 仅供单测注入并发交错屏障，生产恒为 nil。
	testBeforeTrackEmit  func()
	testBeforeDetachEmit func()
}

// FlushRUM 在主进程退出前同步结算活动酷狗会话的待报曲目与 detached 事件（并发安全且幂等）。
func (p *KuGouPlayer) FlushRUM() {
	p.rumGate.Flush(func(s *kugouSessionRUM) {
		s.end("stopped")
	})
}

func (p *KuGouPlayer) startSessionRUM(version, exeVariant string, attachMs int64) *kugouSessionRUM {
	ver := player.SanitizeVersion(version)
	if attachMs < 0 {
		attachMs = 0
	}
	sess, ok := p.rumGate.Register(
		func() *kugouSessionRUM {
			return &kugouSessionRUM{
				version:    ver,
				exeVariant: exeVariant,
				startedAt:  time.Now(),
				compatSent: make(map[string]bool),
				endDone:    make(chan struct{}),
			}
		},
		func(*kugouSessionRUM) {
			if p.testBeforeAttach != nil {
				p.testBeforeAttach()
			}
			props := rum.Props{
				"player":         PlayerName,
				"action":         "attached",
				"player_version": ver,
				"attach_ms":      attachMs,
			}
			if exeVariant != "" {
				props["exe_variant"] = exeVariant
			}
			rum.Track(rum.CatPlayer, "player_session", props)
		},
	)
	if !ok {
		return nil
	}
	return sess
}

func (p *KuGouPlayer) endSessionRUM(s *kugouSessionRUM, cause string) {
	if s == nil {
		return
	}
	p.rumGate.End(s, func(sess *kugouSessionRUM) {
		sess.end(cause)
	})
}

func (p *KuGouPlayer) setSessionAttachMeta(attachMs int64, ver, exeVariant string) {
	p.attachMetaMu.Lock()
	p.attachMeta = kugouAttachMeta{
		attachMs:   attachMs,
		version:    player.SanitizeVersion(ver),
		exeVariant: exeVariant,
	}
	p.attachMetaMu.Unlock()
}

func (p *KuGouPlayer) takeSessionAttachMeta() kugouAttachMeta {
	p.attachMetaMu.Lock()
	m := p.attachMeta
	p.attachMeta = kugouAttachMeta{}
	p.attachMetaMu.Unlock()
	return m
}

func newKugouSessionRUM(version, exeVariant string, attachMs int64) *kugouSessionRUM {
	ver := player.SanitizeVersion(version)
	if attachMs < 0 {
		attachMs = 0
	}
	props := rum.Props{
		"player":         PlayerName,
		"action":         "attached",
		"player_version": ver,
		"attach_ms":      attachMs,
	}
	if exeVariant != "" {
		props["exe_variant"] = exeVariant
	}
	rum.Track(rum.CatPlayer, "player_session", props)
	return &kugouSessionRUM{
		version:    ver,
		exeVariant: exeVariant,
		startedAt:  time.Now(),
		compatSent: make(map[string]bool),
		endDone:    make(chan struct{}),
	}
}

func (s *kugouSessionRUM) onCompat(signal, detailCode string) {
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

func (s *kugouSessionRUM) finishTrackEmit(p *kugouPendingTrack, hook func()) {
	defer s.trackEmitWG.Done()
	if hook != nil {
		hook()
	}
	emitKugouTrackRecognized(p)
}

func (s *kugouSessionRUM) beginTrack(gen uint64) {
	if s == nil {
		return
	}
	var flush *kugouPendingTrack
	var hook func()
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	if s.pending != nil && s.pending.lyricReady && !s.pending.emitted {
		cp := *s.pending
		s.pending.emitted = true
		s.tracksPlayed++
		s.trackEmitWG.Add(1)
		hook = s.testBeforeTrackEmit
		flush = &cp
	}
	s.pending = &kugouPendingTrack{
		gen:       gen,
		startedAt: time.Now(),
		coverWait: true,
		coverOut:  player.CoverOutcome{Action: player.CoverActionNone},
	}
	s.mu.Unlock()

	if flush != nil {
		s.finishTrackEmit(flush, hook)
	}
}

func (s *kugouSessionRUM) discardTrack(gen uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.pending != nil && s.pending.gen == gen && !s.pending.lyricReady {
		s.pending = nil
	}
	s.mu.Unlock()
}

func (s *kugouSessionRUM) onCoverOutcome(gen uint64, out player.CoverOutcome) {
	if s == nil {
		return
	}
	var readyToEmit *kugouPendingTrack
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

func (s *kugouSessionRUM) onTrackLyricSettled(gen uint64, lyricSource string, lines []klyric.Line, pureHint string, lyricMs int64, waitCover bool) {
	if s == nil {
		return
	}
	var readyToEmit *kugouPendingTrack
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
		p.isPureMusic = pureHint != ""
		p.lyricFormat, p.hasVerbatim, p.hasTrans, p.hasRoma, p.hasRomaDetailed = kugouLyricCapabilities(lines)
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

func (s *kugouSessionRUM) onLyricUpdated(reason string, offsetMs int, includeOffset bool) {
	if s == nil || reason == "" {
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

	props := rum.Props{
		"player":            PlayerName,
		"reason":            reason,
		"since_announce_ms": sinceMs,
	}
	if includeOffset {
		props["offset_ms"] = offsetMs
	}
	rum.Track(rum.CatLyricTrack, "track_lyric_updated", props)
}

func (s *kugouSessionRUM) end(disconnectCause string) {
	if s == nil {
		return
	}
	var flush *kugouPendingTrack
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
	if s.pending != nil && s.pending.lyricReady && !s.pending.emitted {
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
		emitKugouTrackRecognized(flush)
	}
	if detachHook != nil {
		detachHook()
	}
	props := rum.Props{
		"player":           PlayerName,
		"action":           "detached",
		"player_version":   ver,
		"disconnect_cause": disconnectCause,
		"duration_sec":     durSec,
		"tracks_played":    tracks,
	}
	if variant != "" {
		props["exe_variant"] = variant
	}
	rum.Track(rum.CatPlayer, "player_session", props)
}

func emitKugouTrackRecognized(p *kugouPendingTrack) {
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

func emitKugouLocalAwaitSample(durMs int64, verdict string) {
	if durMs < 0 {
		durMs = 0
	}
	rum.Track(rum.CatDiagFine, "scan_perf_sample", rum.Props{
		"player":      PlayerName,
		"scan_kind":   "kugou_local_await",
		"duration_ms": durMs,
		"verdict":     verdict,
	})
}
