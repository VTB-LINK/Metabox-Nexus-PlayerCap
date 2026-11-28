package manager

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"Metabox-Nexus-PlayerCap/audiocap/frame"
	"Metabox-Nexus-PlayerCap/rum"
)

const (
	// asioSuspectDuration 是播放中连续出现 SessionKnown && !SessionActive 触发疑似 ASIO/独占上报的时长门槛。
	asioSuspectDuration = 5 * time.Second

	// defaultSummaryGrace 是 stopCapture / 正常 teardownHelper 后等待 helper 经 stderr 泵送达 capture_summary 的短暂宽限期。
	defaultSummaryGrace = 300 * time.Millisecond
)

type sessionKey struct {
	helperGen uint64
	sessionID uint64
}

type captureSessionRUM struct {
	helperGen       uint64
	sessionID       uint64
	player          string
	startedAt       time.Time
	endedAt         time.Time
	ttffMs          atomic.Int64
	jobObjectOK     bool
	normEnabled     bool
	normFrames      uint64
	cappedFrames    uint64
	discontinuities uint64
	framesDropped   uint64
	summaryReceived bool
	hasFinalSummary bool
	timer           *time.Timer
}

// asioTrackState 为不可变结构体，经 playerHotState.asioState 以 CAS 原子发布，绝不原地修改。
type asioTrackState struct {
	inactiveSince time.Time
	reported      bool
}

type playerHotState struct {
	currentSess atomic.Pointer[captureSessionRUM]
	frameSeq    atomic.Uint64
	asioState   atomic.Pointer[asioTrackState]
}

type wasapiErrRecord struct {
	errKey   string
	frameSeq uint64
}

// rumTracker 维护音频采集会话指标、疑似 ASIO/独占边沿去重与 helper 结构化诊断消费。
// 零值可用；onFrame 热路径全程零锁等待（不获取 mu），全部 rum.Track 均在锁外执行，避免任何锁序倒挂。
type rumTracker struct {
	nextSessionID    atomic.Uint64
	hot              sync.Map // player string -> *playerHotState
	mu               sync.Mutex
	currentHelperGen uint64
	sessions         map[string]*captureSessionRUM
	pendingStop      map[sessionKey]*captureSessionRUM
	lastWasapiErr    map[string]wasapiErrRecord
	summaryGrace     time.Duration
}

func (t *rumTracker) hotState(player string) *playerHotState {
	if v, ok := t.hot.Load(player); ok {
		return v.(*playerHotState)
	}
	st := &playerHotState{}
	actual, _ := t.hot.LoadOrStore(player, st)
	return actual.(*playerHotState)
}

func (t *rumTracker) allocSessionID() uint64 {
	return t.nextSessionID.Add(1)
}

func (t *rumTracker) grace() time.Duration {
	if t.summaryGrace > 0 {
		return t.summaryGrace
	}
	return defaultSummaryGrace
}

func (t *rumTracker) onCaptureStart(helperGen, sessionID uint64, player string, now time.Time, jobOK, normEnabled bool) {
	if player == "" {
		return
	}
	var toEmit []*captureSessionRUM

	t.mu.Lock()
	if helperGen > t.currentHelperGen {
		t.currentHelperGen = helperGen
	}
	if t.sessions == nil {
		t.sessions = make(map[string]*captureSessionRUM)
	}
	if prev := t.sessions[player]; prev != nil {
		prev.endedAt = now
		toEmit = append(toEmit, prev)
	}
	sess := &captureSessionRUM{
		helperGen:   helperGen,
		sessionID:   sessionID,
		player:      player,
		startedAt:   now,
		jobObjectOK: jobOK,
		normEnabled: normEnabled,
	}
	sess.ttffMs.Store(-1)
	t.sessions[player] = sess
	hot := t.hotState(player)
	hot.currentSess.Store(sess)
	hot.asioState.Store(nil)
	t.mu.Unlock()

	for _, s := range toEmit {
		emitCaptureSessionEnd(s)
	}
}

func (t *rumTracker) onFrame(player string, flags uint32, now time.Time, playingFn func(string) bool) {
	if player == "" {
		return
	}
	hot := t.hotState(player)
	// 推进帧序号供 wasapi_activate 边沿去重识别「期间激活成功过」，避免 onFrame 触碰 t.mu。
	hot.frameSeq.Add(1)

	if sess := hot.currentSess.Load(); sess != nil && sess.ttffMs.Load() < 0 {
		ms := now.Sub(sess.startedAt).Milliseconds()
		if ms < 0 {
			ms = 0
		}
		sess.ttffMs.CompareAndSwap(-1, ms)
	}

	if !rum.Enabled() {
		return
	}

	playing := playingFn != nil && playingFn(player)
	known := (flags & frame.FlagSessionKnown) != 0
	active := (flags & frame.FlagSessionActive) != 0

	for {
		prev := hot.asioState.Load()
		switch {
		case !playing || active:
			// 离开播放态或检测到有效 WASAPI 渲染会话：完整复位连续静默期与边沿去重。
			if prev == nil {
				return
			}
			if hot.asioState.CompareAndSwap(prev, nil) {
				return
			}
		case !known:
			// 探测结果暂不可用（如短暂过期）：中断连续计时但不清除已报边沿，防抖动刷屏。
			if prev == nil || prev.inactiveSince.IsZero() {
				return
			}
			next := &asioTrackState{reported: prev.reported}
			if hot.asioState.CompareAndSwap(prev, next) {
				return
			}
		default:
			// playing && SessionKnown && !SessionActive
			if prev == nil || prev.inactiveSince.IsZero() {
				reported := prev != nil && prev.reported
				next := &asioTrackState{inactiveSince: now, reported: reported}
				if hot.asioState.CompareAndSwap(prev, next) {
					return
				}
			} else if !prev.reported && now.Sub(prev.inactiveSince) >= asioSuspectDuration {
				next := &asioTrackState{inactiveSince: prev.inactiveSince, reported: true}
				if hot.asioState.CompareAndSwap(prev, next) {
					emitAudioSignalState(player, "suspected_asio_or_exclusive", "", "")
					return
				}
			} else {
				return
			}
		}
	}
}

// queuePendingStopLocked 在已持 t.mu 期间把未收齐最终摘要的会话放入 pendingStop 并挂短宽限定时器。
func (t *rumTracker) queuePendingStopLocked(sess *captureSessionRUM) *captureSessionRUM {
	if t.pendingStop == nil {
		t.pendingStop = make(map[sessionKey]*captureSessionRUM)
	}
	key := sessionKey{helperGen: sess.helperGen, sessionID: sess.sessionID}
	var displaced *captureSessionRUM
	if old := t.pendingStop[key]; old != nil {
		if old.timer != nil {
			old.timer.Stop()
			old.timer = nil
		}
		displaced = old
	}
	t.pendingStop[key] = sess
	target := sess
	sess.timer = time.AfterFunc(t.grace(), func() {
		defer func() {
			_ = recover()
		}()
		t.flushPendingSession(key, target)
	})
	return displaced
}

func (t *rumTracker) onCaptureStop(player string, now time.Time, waitSummary bool) {
	if player == "" {
		return
	}
	var toEmit []*captureSessionRUM

	t.mu.Lock()
	sess := t.sessions[player]
	delete(t.sessions, player)
	if v, ok := t.hot.Load(player); ok {
		hot := v.(*playerHotState)
		hot.asioState.Store(nil)
		if sess != nil {
			hot.currentSess.CompareAndSwap(sess, nil)
		}
	}
	if sess != nil {
		sess.endedAt = now
		if waitSummary && !sess.summaryReceived {
			if displaced := t.queuePendingStopLocked(sess); displaced != nil {
				toEmit = append(toEmit, displaced)
			}
		} else {
			toEmit = append(toEmit, sess)
		}
	}
	t.mu.Unlock()

	for _, s := range toEmit {
		emitCaptureSessionEnd(s)
	}
}

func (t *rumTracker) flushPendingSession(key sessionKey, target *captureSessionRUM) {
	var emit *captureSessionRUM
	t.mu.Lock()
	if cur := t.pendingStop[key]; cur == target {
		delete(t.pendingStop, key)
		cur.timer = nil
		emit = cur
	}
	t.mu.Unlock()
	if emit != nil {
		emitCaptureSessionEnd(emit)
	}
}

// onHelperEnd 结算指定 helperGen（0 表示全部，如主服务 BeginShutdown）的活动捕获会话。
// waitSummary 为真（正常拆除）时，尚未收齐最终摘要的会话转入短宽限异步等待（不阻塞 Run 等 stderr 泵），
// 超时未到再按 stats_incomplete=true 结算；为假（异常退出或关停）时立即结算全部活动与待结算会话。
func (t *rumTracker) onHelperEnd(helperGen uint64, now time.Time, waitSummary bool) {
	var toEmit []*captureSessionRUM

	t.mu.Lock()
	if !waitSummary {
		for k, s := range t.pendingStop {
			if helperGen != 0 && k.helperGen != helperGen {
				continue
			}
			if s.timer != nil {
				s.timer.Stop()
				s.timer = nil
			}
			delete(t.pendingStop, k)
			toEmit = append(toEmit, s)
		}
	}
	for p, s := range t.sessions {
		if helperGen != 0 && s.helperGen != helperGen {
			continue
		}
		delete(t.sessions, p)
		if v, ok := t.hot.Load(p); ok {
			hot := v.(*playerHotState)
			hot.asioState.Store(nil)
			hot.currentSess.CompareAndSwap(s, nil)
		}
		s.endedAt = now
		if waitSummary && !s.summaryReceived {
			if displaced := t.queuePendingStopLocked(s); displaced != nil {
				toEmit = append(toEmit, displaced)
			}
		} else {
			toEmit = append(toEmit, s)
		}
	}
	if helperGen == 0 {
		t.hot.Range(func(_, v any) bool {
			v.(*playerHotState).asioState.Store(nil)
			return true
		})
	}
	t.mu.Unlock()

	for _, s := range toEmit {
		emitCaptureSessionEnd(s)
	}
}

func (t *rumTracker) handleDiagLine(line string) {
	t.handleDiagLineForHelper(0, line)
}

func (t *rumTracker) handleDiagLineForHelper(helperGen uint64, line string) {
	body, ok := strings.CutPrefix(line, rumDiagPrefix)
	if !ok {
		return
	}
	var kind, player, signalState, stage, hresult string
	var sid, normFrames, cappedFrames, discontinuities, framesDropped uint64
	var incomplete bool

	for _, tok := range strings.Fields(body) {
		k, v, hasEq := strings.Cut(tok, "=")
		if !hasEq || v == "" {
			continue
		}
		switch k {
		case "kind":
			kind = v
		case "player":
			player = v
		case "sid":
			if n, err := strconv.ParseUint(v, 10, 64); err == nil {
				sid = n
			}
		case "signal_state":
			signalState = v
		case "activate_stage":
			stage = v
		case "hresult":
			hresult = v
		case "norm_frames":
			if n, err := strconv.ParseUint(v, 10, 64); err == nil {
				normFrames = n
			}
		case "capped_frames":
			if n, err := strconv.ParseUint(v, 10, 64); err == nil {
				cappedFrames = n
			}
		case "discontinuities":
			if n, err := strconv.ParseUint(v, 10, 64); err == nil {
				discontinuities = n
			}
		case "frames_dropped":
			if n, err := strconv.ParseUint(v, 10, 64); err == nil {
				framesDropped = n
			}
		case "incomplete":
			incomplete = v == "1" || v == "true"
		}
	}

	if !isSafeDiagToken(player) {
		return
	}

	switch kind {
	case "capture_stats":
		t.mu.Lock()
		if sess := t.sessions[player]; sess != nil && sess.helperGen == helperGen && sess.sessionID == sid {
			sess.normFrames = normFrames
			sess.cappedFrames = cappedFrames
			sess.discontinuities = discontinuities
			sess.framesDropped = framesDropped
		}
		t.mu.Unlock()

	case "capture_summary":
		key := sessionKey{helperGen: helperGen, sessionID: sid}
		var emit *captureSessionRUM
		t.mu.Lock()
		if ps := t.pendingStop[key]; ps != nil && ps.player == player {
			if ps.timer != nil {
				ps.timer.Stop()
				ps.timer = nil
			}
			delete(t.pendingStop, key)
			ps.normFrames = normFrames
			ps.cappedFrames = cappedFrames
			ps.discontinuities = discontinuities
			ps.framesDropped = framesDropped
			ps.summaryReceived = true
			if !incomplete {
				ps.hasFinalSummary = true
			}
			emit = ps
		} else if sess := t.sessions[player]; sess != nil && sess.helperGen == helperGen && sess.sessionID == sid {
			sess.normFrames = normFrames
			sess.cappedFrames = cappedFrames
			sess.discontinuities = discontinuities
			sess.framesDropped = framesDropped
			sess.summaryReceived = true
			if !incomplete {
				sess.hasFinalSummary = true
			}
		}
		t.mu.Unlock()
		if emit != nil {
			emitCaptureSessionEnd(emit)
		}

	case "wasapi_activate":
		if signalState != "wasapi_activate_failed" && signalState != "wasapi_wait_timeout_2s" {
			return
		}
		if !isSafeDiagToken(stage) {
			stage = "unknown"
		}
		if !isSafeDiagToken(hresult) {
			hresult = ""
		}
		edgeKey := signalState + "|" + stage + "|" + hresult
		shouldEmit := false
		t.mu.Lock()
		if helperGen >= t.currentHelperGen {
			var curSeq uint64
			if v, ok := t.hot.Load(player); ok {
				curSeq = v.(*playerHotState).frameSeq.Load()
			}
			if t.lastWasapiErr == nil {
				t.lastWasapiErr = make(map[string]wasapiErrRecord)
			}
			prev, hasPrev := t.lastWasapiErr[player]
			// 首次出现、错误边沿变化、或期间收到有效帧（frameSeq 推进）视为已恢复，允许重新上报。
			if !hasPrev || prev.errKey != edgeKey || prev.frameSeq != curSeq {
				t.lastWasapiErr[player] = wasapiErrRecord{errKey: edgeKey, frameSeq: curSeq}
				shouldEmit = true
			}
		}
		t.mu.Unlock()
		if shouldEmit {
			emitAudioSignalState(player, signalState, hresult, stage)
		}
	}
}

func isSafeDiagToken(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_' || c == '-' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}

func waitExitedTimeout(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	default:
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

func emitCaptureSessionEnd(sess *captureSessionRUM) {
	if sess == nil {
		return
	}
	durSec := int64(sess.endedAt.Sub(sess.startedAt).Seconds())
	if durSec < 0 {
		durSec = 0
	}
	ratio := 0.0
	if sess.normFrames > 0 {
		ratio = float64(sess.cappedFrames) / float64(sess.normFrames)
		if ratio < 0 {
			ratio = 0
		} else if ratio > 1 {
			ratio = 1
		}
	}
	rum.Track(rum.CatAudioEffect, "audio_capture_session", rum.Props{
		"event_kind":          "session_end",
		"player":              sess.player,
		"ttff_ms":             sess.ttffMs.Load(),
		"duration_sec":        durSec,
		"job_object_ok":       sess.jobObjectOK,
		"norm_enabled":        sess.normEnabled,
		"gain_capped_ratio":   ratio,
		"discontinuity_count": sess.discontinuities,
		"frames_dropped":      sess.framesDropped,
		"stats_incomplete":    !sess.hasFinalSummary,
	})
}

func (t *rumTracker) emitHelperAbnormalExit(exitCode uint32, quickFails int, jobOK bool) {
	rum.Track(rum.CatAudioEffect, "audio_capture_session", rum.Props{
		"event_kind":    "helper_abnormal_exit",
		"player":        "",
		"exit_code":     exitCodeString(exitCode),
		"quick_fails":   quickFails,
		"job_object_ok": jobOK,
	})
}

func (t *rumTracker) emitGaveUp(player string, exitCode uint32, hasExit bool, quickFails int, jobOK bool) {
	codeStr := ""
	if hasExit {
		codeStr = exitCodeString(exitCode)
	}
	rum.Track(rum.CatAudioEffect, "audio_capture_session", rum.Props{
		"event_kind":    "gave_up",
		"player":        player,
		"exit_code":     codeStr,
		"quick_fails":   quickFails,
		"job_object_ok": jobOK,
	})
}

func emitAudioSignalState(player, signalState, hresult, stage string) {
	rum.Track(rum.CatAudioEffect, "audio_signal_state", rum.Props{
		"player":         player,
		"signal_state":   signalState,
		"hresult":        hresult,
		"activate_stage": stage,
	})
}
