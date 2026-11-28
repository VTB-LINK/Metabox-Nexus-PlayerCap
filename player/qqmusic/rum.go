package qqmusic

import (
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/qqmusic/assoc"
	"Metabox-Nexus-PlayerCap/rum"
)

// qqBlockTracker 跟踪 QQ 音乐受阻态（no_admin / cdp_port_not_ready）的边沿与恢复耗时。
type qqBlockTracker struct {
	reason string
	since  time.Time
	now    func() time.Time
}

func (b *qqBlockTracker) nowTime() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *qqBlockTracker) onBlocked(reason string) {
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

func (b *qqBlockTracker) onNotRunning() {
	b.reason = ""
	b.since = time.Time{}
}

func (b *qqBlockTracker) onRecovered() {
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

type qqPendingTrack struct {
	gen                uint64
	startedAt          time.Time
	clockMode          string
	idSource           string
	lyricSource        string
	lyricFormat        string
	lineCount          int
	hasVerbatim        bool
	hasTrans           bool
	hasRoma            bool
	hasRomaDetailed    bool
	hasFurigana        bool
	isPureMusic        bool
	lyricMs            int64
	supplementDecision string

	lyricReady bool
	coverGen   uint64
	coverWait  bool
	coverReady bool
	coverOut   player.CoverOutcome
	emitted    bool
}

// qqSessionRUM 管理单次 QQ 音乐会话的 RUM 生命周期、兼容信号去重、曲目首轮结算与曲中刷新上报。
// 所有共享状态均受 mu 保护，供轮询线程与异步 startCover goroutine 并发安全交互。
type qqSessionRUM struct {
	mu           sync.Mutex
	version      string
	startedAt    time.Time
	tracksPlayed int
	compatSent   map[string]bool
	pending      *qqPendingTrack
	lastFallback string
	ended        bool
	endDone      chan struct{}
	trackEmitWG  sync.WaitGroup

	// testBeforeTrackEmit / testBeforeDetachEmit 仅供单测注入并发交错屏障，生产恒为 nil。
	testBeforeTrackEmit  func()
	testBeforeDetachEmit func()
}

// FlushRUM 在主进程退出前同步结算活动 QQ 音乐会话的待报曲目与 detached 事件（并发安全且幂等）。
func (p *QQMusicPlayer) FlushRUM() {
	p.rumGate.Flush(func(s *qqSessionRUM) {
		s.end("stopped")
	})
}

func (p *QQMusicPlayer) startSessionRUM(version, fallbackTo string, attachMs int64) *qqSessionRUM {
	ver := player.SanitizeVersion(version)
	if attachMs < 0 {
		attachMs = 0
	}
	sess, ok := p.rumGate.Register(
		func() *qqSessionRUM {
			return &qqSessionRUM{
				version:    ver,
				startedAt:  time.Now(),
				compatSent: make(map[string]bool),
				endDone:    make(chan struct{}),
			}
		},
		func(*qqSessionRUM) {
			if p.testBeforeAttach != nil {
				p.testBeforeAttach()
			}
			rum.Track(rum.CatPlayer, "player_session", rum.Props{
				"player":         PlayerName,
				"action":         "attached",
				"player_version": ver,
				"attach_ms":      attachMs,
			})
			if fallbackTo != "" {
				emitQQVersionFallbackRUM(version, fallbackTo)
			}
		},
	)
	if !ok {
		return nil
	}
	return sess
}

func (p *QQMusicPlayer) endSessionRUM(s *qqSessionRUM, fallbackCause string) {
	if s == nil {
		return
	}
	p.rumGate.End(s, func(sess *qqSessionRUM) {
		sess.endWithStopCh(p.StopCh, fallbackCause)
	})
}

func newQQSessionRUM(version, fallbackTo string, attachMs int64) *qqSessionRUM {
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
	s := &qqSessionRUM{
		version:    ver,
		startedAt:  time.Now(),
		compatSent: make(map[string]bool),
		endDone:    make(chan struct{}),
	}
	if fallbackTo != "" {
		emitQQVersionFallbackRUM(version, fallbackTo)
	}
	return s
}

func emitQQVersionFallbackRUM(version, fallbackTo string) {
	if fallbackTo == "" {
		return
	}
	ver := player.SanitizeVersion(version)
	signal := "nearest_version_fallback"
	mode := "nearest_semver"
	if ver == "" || ver == "unknown" {
		signal = "unknown_version_fallback"
		mode = "latest_known"
	}
	rum.TrackOnce("qqmusic."+signal+"."+ver, rum.CatPlayer, "player_compat_signal", rum.Props{
		"player":         PlayerName,
		"signal":         signal,
		"player_version": ver,
		"fallback_mode":  mode,
	})
}

func (s *qqSessionRUM) emitCompat(signal, detailCode string) {
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

func (s *qqSessionRUM) onPlayStateMismatch(tripped string) {
	s.emitCompat("play_state_mismatch", tripped)
}

func qqClockMode(mem *QQMusicMem, judgeBad bool, poll time.Duration) string {
	if mem != nil && !judgeBad && mem.playStateAddr() != 0 && poll <= time.Second {
		return "native_state"
	}
	return "stall"
}

func qqIDSource(songID uint32, songMid string, fromHeap bool) string {
	switch {
	case songID != 0:
		return "song_id"
	case songMid != "" && fromHeap:
		return "heap_song_mid"
	case songMid != "":
		return "song_mid"
	default:
		return "none"
	}
}

func (s *qqSessionRUM) finishTrackEmit(p *qqPendingTrack, hook func()) {
	defer s.trackEmitWG.Done()
	if hook != nil {
		hook()
	}
	emitQQTrackRecognized(p)
}

func (s *qqSessionRUM) beginTrack(gen uint64, clockMode, idSource string) {
	if s == nil {
		return
	}
	var flush *qqPendingTrack
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
	s.pending = &qqPendingTrack{
		gen:       gen,
		startedAt: time.Now(),
		clockMode: clockMode,
		idSource:  idSource,
		coverOut:  player.CoverOutcome{Action: player.CoverActionNone},
	}
	s.lastFallback = ""
	s.mu.Unlock()

	if flush != nil {
		s.finishTrackEmit(flush, hook)
	}
}

func (s *qqSessionRUM) updateIDSource(gen uint64, idSource string) {
	if s == nil || idSource == "" {
		return
	}
	s.mu.Lock()
	if s.pending != nil && s.pending.gen == gen && !s.pending.emitted {
		s.pending.idSource = idSource
	}
	s.mu.Unlock()
}

func (s *qqSessionRUM) noteSupplement(prevName, prevSinger, readName, readSinger string) {
	if s == nil {
		return
	}
	_, nDec := player.SupplementDecision(prevName, readName)
	_, sDec := player.SupplementDecision(prevSinger, readSinger)
	dec := player.CombineSupplementDecisions(nDec, sDec)
	s.mu.Lock()
	if s.pending != nil && !s.pending.emitted {
		s.pending.supplementDecision = player.CombineSupplementDecisions(s.pending.supplementDecision, dec)
	}
	s.mu.Unlock()
}

func (s *qqSessionRUM) bindCoverGen(trackGen, coverGen uint64, willFetch bool) {
	if s == nil {
		return
	}
	var readyToEmit *qqPendingTrack
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

func (s *qqSessionRUM) onCoverOutcome(coverGen uint64, out player.CoverOutcome) {
	if s == nil {
		return
	}
	var readyToEmit *qqPendingTrack
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

func (s *qqSessionRUM) onTrackLyricSettled(gen uint64, src lyricSource, lines []lyricLine, fetchErr bool, pureHint string, waitPendingCover bool, lyricMs int64) {
	if s == nil {
		return
	}
	var readyToEmit *qqPendingTrack
	var hook func()
	s.mu.Lock()
	p := s.pending
	if !s.ended && p != nil && p.gen == gen && !p.emitted && !p.lyricReady {
		if lyricMs <= 0 {
			lyricMs = time.Since(p.startedAt).Milliseconds()
		}
		if lyricMs < 0 {
			lyricMs = 0
		}
		p.lyricMs = lyricMs
		p.lineCount = len(lines)
		p.isPureMusic = pureHint != ""
		p.lyricSource = qqLyricSourceName(src, lines, fetchErr, pureHint)
		p.lyricFormat, p.hasVerbatim, p.hasTrans, p.hasRoma, p.hasRomaDetailed, p.hasFurigana = qqLyricCapabilities(lines)
		p.lyricReady = true
		if waitPendingCover {
			p.coverWait = true
			p.coverReady = false
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

func qqLyricSourceName(src lyricSource, lines []lyricLine, fetchErr bool, pureHint string) string {
	switch src {
	case srcLocal, srcAssoc:
		return "local_assoc_db"
	case srcOnline:
		if fetchErr && len(lines) == 0 {
			return "err"
		}
		if len(lines) > 0 || pureHint != "" {
			return "online"
		}
		return "none"
	default:
		if fetchErr {
			return "err"
		}
		return "none"
	}
}

func qqLyricCapabilities(lines []lyricLine) (format string, hasVerbatim, hasTrans, hasRoma, hasRomaDetailed, hasFurigana bool) {
	if len(lines) == 0 {
		return "none", false, false, false, false, false
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
		if len(l.Furigana) > 0 {
			hasFurigana = true
		}
	}
	if hasVerbatim {
		format = "qrc"
	} else {
		format = "lrc"
	}
	return format, hasVerbatim, hasTrans, hasRoma, hasRomaDetailed, hasFurigana
}

func emitQQTrackRecognized(p *qqPendingTrack) {
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
		"clock_mode":            p.clockMode,
		"id_source":             p.idSource,
		"lyric_source":          lyricSrc,
		"lyric_format":          lyricFmt,
		"line_count":            p.lineCount,
		"has_verbatim":          p.hasVerbatim,
		"has_trans":             p.hasTrans,
		"has_roma":              p.hasRoma,
		"has_roma_detailed":     p.hasRomaDetailed,
		"has_furigana":          p.hasFurigana,
		"is_pure_music":         p.isPureMusic,
		"cover_action":          coverAction,
		"cover_fallback_reason": p.coverOut.FallbackReason,
		"supplement_decision":   p.supplementDecision,
		"lyric_ms":              p.lyricMs,
		"cover_ms":              p.coverOut.DurationMs,
	})
}

func (s *qqSessionRUM) notePendingFallback(triggered bool, reason string) {
	if s == nil || !triggered || reason == "" {
		return
	}
	s.mu.Lock()
	s.lastFallback = reason
	s.mu.Unlock()
}

func (s *qqSessionRUM) onReopen(byLoading bool, st *lyricState) {
	if s == nil {
		return
	}
	s.mu.Lock()
	reason := "reopen_r1"
	if !byLoading {
		if s.lastFallback != "" {
			reason = s.lastFallback
		} else {
			reason = "reopen_r2"
		}
		s.lastFallback = ""
	}
	var sinceMs int64
	if s.pending != nil && !s.pending.startedAt.IsZero() {
		sinceMs = time.Since(s.pending.startedAt).Milliseconds()
		if sinceMs < 0 {
			sinceMs = 0
		}
	}
	s.mu.Unlock()

	shiftMs := 0
	if st != nil {
		shiftMs = st.shown.lyricOffsetMs
	}
	rum.Track(rum.CatLyricTrack, "track_lyric_updated", rum.Props{
		"player":            PlayerName,
		"reason":            reason,
		"since_announce_ms": sinceMs,
		"offset_ms":         shiftMs,
	})
}

func (s *qqSessionRUM) onDrainChanged(gen uint64, prevSrc lyricSource, prevShiftMs int, prevContent [32]byte, prevUndecided, expired bool, st *lyricState, lines []lyricLine) {
	if s == nil || st == nil {
		return
	}
	curSrc := st.source()
	curShiftMs := st.shown.lyricOffsetMs
	curContent := st.shown.content

	var sinceMs int64
	var needInitialSettle bool
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	if s.pending != nil {
		if !s.pending.startedAt.IsZero() {
			sinceMs = time.Since(s.pending.startedAt).Milliseconds()
			if sinceMs < 0 {
				sinceMs = 0
			}
		}
		if s.pending.gen == gen && !s.pending.emitted && !s.pending.lyricReady {
			needInitialSettle = true
		}
	}
	s.mu.Unlock()

	if prevUndecided {
		s.onTrackLyricSettled(gen, curSrc, lines, false, st.hint(), false, sinceMs)
		reason := "local_window_resolved"
		if expired {
			reason = "local_window_timeout_1s"
		}
		rum.Track(rum.CatLyricTrack, "track_lyric_updated", rum.Props{
			"player":            PlayerName,
			"reason":            reason,
			"since_announce_ms": sinceMs,
			"offset_ms":         curShiftMs,
		})
		return
	}

	if needInitialSettle {
		// 无在线编号的本地歌按 srcNone 初始化（prevUndecided 为假），首次由关联读取者落定有效本地/关联歌词时
		// 直接完成首轮识曲结算，不误发曲中 assoc_changed。
		if !st.undecided() && curSrc != srcNone &&
			(len(lines) > 0 || st.hint() != "" || curSrc == srcLocal || curSrc == srcAssoc) {
			s.onTrackLyricSettled(gen, curSrc, lines, false, st.hint(), false, sinceMs)
		}
		// 首轮尚未满足落定条件（如补取窗口中收到空 srcOnline，或仍处未决/srcNone）时直接返回，不误发曲中更新。
		return
	}

	reason := "assoc_changed"
	if curSrc == prevSrc && curContent == prevContent && curShiftMs != prevShiftMs {
		reason = "offset_changed"
	}
	rum.Track(rum.CatLyricTrack, "track_lyric_updated", rum.Props{
		"player":            PlayerName,
		"reason":            reason,
		"since_announce_ms": sinceMs,
		"offset_ms":         curShiftMs,
	})
}

func (s *qqSessionRUM) endWithStopCh(stopCh <-chan struct{}, fallbackCause string) {
	cause := fallbackCause
	select {
	case <-stopCh:
		cause = "stopped"
	default:
	}
	s.end(cause)
}

func (s *qqSessionRUM) end(disconnectCause string) {
	if s == nil {
		return
	}
	var flush *qqPendingTrack
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
		emitQQTrackRecognized(flush)
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

func emitQQQRCDecryptFailed(part string) {
	rum.TrackOnce("qqmusic.qrc_decrypt_failed."+part, rum.CatPlayer, "player_compat_signal", rum.Props{
		"player":      PlayerName,
		"signal":      "qrc_decrypt_failed",
		"detail_code": part,
	})
}

func emitQQFuriganaSignal(signal string) {
	rum.TrackOnce("qqmusic."+signal, rum.CatPlayer, "player_compat_signal", rum.Props{
		"player": PlayerName,
		"signal": signal,
	})
}

func emitQQAssocCompat(version, signal string) {
	ver := player.SanitizeVersion(version)
	rum.TrackOnce("qqmusic."+signal+"."+ver, rum.CatPlayer, "player_compat_signal", rum.Props{
		"player":         PlayerName,
		"signal":         signal,
		"player_version": ver,
	})
}

func emitQQSwitchHoldGuard(outcome string, heldMs int64) {
	if heldMs < 0 {
		heldMs = 0
	}
	rum.Track(rum.CatDiagFine, "playback_clock_guard", rum.Props{
		"player":     PlayerName,
		"guard_kind": "switch_hold_400ms",
		"outcome":    outcome,
		"held_ms":    heldMs,
	})
}

func qqRecordVerdictCode(v assoc.RecordVerdict) string {
	switch v {
	case assoc.RecordFound:
		return "found"
	case assoc.RecordNone:
		return "none"
	case assoc.RecordNotFound:
		return "not_found"
	case assoc.RecordAmbiguous:
		return "ambiguous"
	case assoc.RecordPending:
		return "pending"
	case assoc.RecordIncomplete:
		return "incomplete"
	default:
		return "other"
	}
}

func emitQQSongRecordScanSample(st recordScanStats, verdict assoc.RecordVerdict) {
	durMs := st.elapsed.Milliseconds()
	if durMs < 0 {
		durMs = 0
	}
	rum.Track(rum.CatDiagFine, "scan_perf_sample", rum.Props{
		"player":           PlayerName,
		"scan_kind":        "qq_song_record",
		"duration_ms":      durMs,
		"bytes_scanned_kb": st.read / 1024,
		"capped":           st.capped,
		"verdict":          qqRecordVerdictCode(verdict),
	})
}

func emitQQRMLocalScanSample(durMs int64, verdict string) {
	if durMs < 0 {
		durMs = 0
	}
	rum.Track(rum.CatDiagFine, "scan_perf_sample", rum.Props{
		"player":      PlayerName,
		"scan_kind":   "qq_rm_local",
		"duration_ms": durMs,
		"verdict":     verdict,
	})
}
