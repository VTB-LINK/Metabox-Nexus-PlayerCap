package wesing

import (
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/wesing/lyric"
	"Metabox-Nexus-PlayerCap/rum"
)

// wesingBlockTracker 跟踪全民K歌权限阻断态边沿，防等待循环刷屏并在后续成功附加时上报恢复耗时。
type wesingBlockTracker struct {
	reason string
	since  time.Time
	now    func() time.Time
}

func (b *wesingBlockTracker) nowTime() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *wesingBlockTracker) onBlocked(reason string) {
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

func (b *wesingBlockTracker) onNotRunning() {
	b.reason = ""
	b.since = time.Time{}
}

func (b *wesingBlockTracker) onRecovered() {
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

// wesingSessionRUM 管理单次全民K歌会话的 RUM 生命周期、兼容退化去重与识曲结算。
// 所有可变字段受 mu 保护，供轮询线程与退出信号侧 FlushRUM 并发安全访问；持锁期间绝不调用 rum.Track。
type wesingSessionRUM struct {
	mu           sync.Mutex
	startedAt    time.Time
	tracksPlayed int
	compatSent   map[string]bool
	ended        bool
	endDone      chan struct{}
	trackEmitWG  sync.WaitGroup

	// testBeforeTrackEmit / testBeforeDetachEmit 仅供单测注入并发交错屏障，生产恒为 nil。
	testBeforeTrackEmit  func()
	testBeforeDetachEmit func()
}

// FlushRUM 在主进程退出前同步结算活动全民K歌会话的 detached 事件（并发安全且幂等）。
func (p *WesingPlayer) FlushRUM() {
	p.rumGate.Flush(func(s *wesingSessionRUM) {
		s.end("stopped")
	})
}

func (p *WesingPlayer) startSessionRUM(attachMs int64) *wesingSessionRUM {
	if attachMs < 0 {
		attachMs = 0
	}
	sess, ok := p.rumGate.Register(
		func() *wesingSessionRUM {
			return &wesingSessionRUM{
				startedAt:  time.Now(),
				compatSent: make(map[string]bool),
				endDone:    make(chan struct{}),
			}
		},
		func(*wesingSessionRUM) {
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
		return nil
	}
	return sess
}

func (p *WesingPlayer) endSessionRUM(s *wesingSessionRUM, cause string) {
	if s == nil {
		return
	}
	p.rumGate.End(s, func(sess *wesingSessionRUM) {
		sess.end(cause)
	})
}

func newWesingSessionRUM(attachMs int64) *wesingSessionRUM {
	if attachMs < 0 {
		attachMs = 0
	}
	rum.Track(rum.CatPlayer, "player_session", rum.Props{
		"player":    PlayerName,
		"action":    "attached",
		"attach_ms": attachMs,
	})
	return &wesingSessionRUM{
		startedAt:  time.Now(),
		compatSent: make(map[string]bool),
		endDone:    make(chan struct{}),
	}
}

func (s *wesingSessionRUM) onCompat(signal, detailCode string) {
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
	s.mu.Unlock()

	props := rum.Props{
		"player": PlayerName,
		"signal": signal,
	}
	if detailCode != "" {
		props["detail_code"] = detailCode
	}
	rum.Track(rum.CatPlayer, "player_compat_signal", props)
}

func (s *wesingSessionRUM) onTrackRecognized(clockMode, idSource string, lyrics []lyric.LyricLine, lyricMs int64) {
	if s == nil {
		return
	}
	if lyricMs < 0 {
		lyricMs = 0
	}
	var hasVerbatim, hasRoma, hasRomaDetailed bool
	for _, l := range lyrics {
		if len(l.Detailed.Words) > 0 {
			hasVerbatim = true
		}
		if l.RomaText != "" {
			hasRoma = true
		}
		if len(l.RomaDetailed.Words) > 0 {
			hasRomaDetailed = true
		}
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.tracksPlayed++
	s.trackEmitWG.Add(1)
	hook := s.testBeforeTrackEmit
	s.mu.Unlock()

	defer s.trackEmitWG.Done()
	if hook != nil {
		hook()
	}
	player.NoteTrackRecognized()
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{
		"player":            PlayerName,
		"clock_mode":        clockMode,
		"id_source":         idSource,
		"lyric_source":      "memory",
		"lyric_format":      "memory",
		"line_count":        len(lyrics),
		"has_verbatim":      hasVerbatim,
		"has_trans":         false,
		"has_roma":          hasRoma,
		"has_roma_detailed": hasRomaDetailed,
		"has_furigana":      false,
		"is_pure_music":     false,
		"lyric_ms":          lyricMs,
	})
}

func (s *wesingSessionRUM) end(disconnectCause string) {
	if s == nil {
		return
	}
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
	tracks := s.tracksPlayed
	durSec := int64(time.Since(s.startedAt).Seconds())
	if durSec < 0 {
		durSec = 0
	}
	hook := s.testBeforeDetachEmit
	s.mu.Unlock()

	defer close(done)
	s.trackEmitWG.Wait()
	if hook != nil {
		hook()
	}
	rum.Track(rum.CatPlayer, "player_session", rum.Props{
		"player":           PlayerName,
		"action":           "detached",
		"disconnect_cause": disconnectCause,
		"duration_sec":     durSec,
		"tracks_played":    tracks,
	})
}

func emitWesingClockGuard(guardKind, outcome string, deltaMs int64) {
	props := rum.Props{
		"player":     PlayerName,
		"guard_kind": guardKind,
		"outcome":    outcome,
	}
	if deltaMs != 0 {
		props["delta_ms"] = deltaMs
	}
	rum.Track(rum.CatDiagFine, "playback_clock_guard", props)
}

func emitWesingCoverScanSample(trigger, verdict string, durationMs int64, coverOut player.CoverOutcome, supplementDecision string) {
	if durationMs < 0 {
		durationMs = 0
	}
	action := coverOut.Action
	if action == "" {
		action = player.CoverActionNone
	}
	props := rum.Props{
		"player":                PlayerName,
		"scan_kind":             "wesing_cover_scan",
		"trigger":               trigger,
		"duration_ms":           durationMs,
		"verdict":               verdict,
		"cover_action":          action,
		"cover_fallback_reason": coverOut.FallbackReason,
		"supplement_decision":   supplementDecision,
		"cover_ms":              coverOut.DurationMs,
	}
	rum.Track(rum.CatDiagFine, "scan_perf_sample", props)
}
