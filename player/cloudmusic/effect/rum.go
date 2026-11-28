package effect

import (
	"encoding/json"
	"math"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/player/cloudmusic/park"
	"Metabox-Nexus-PlayerCap/rum"
)

// effectStatsExprJS 仅作只读采集（AGENTS.md §0.2 铁律：严禁写入源 canvas 的 width/height/style）。
// 复用 purelayerSession 既有的 400ms 探活拍读取 __mbxCapStats 与源 canvas 当前宽高，绝不每帧上报。
const effectStatsExprJS = `(function(){var c=document.querySelector('#lyric-effect-canvas-id'),s=window.__mbxCapStats;return JSON.stringify({w:c?(c.width|0):0,h:c?(c.height|0):0,b:s?(s.blob|0):0,y:s?(s.sync|0):0,t:s?(s.stale|0):0});})()`

type ingest403Drainer interface {
	DrainEffectIngest403() uint64
}

type purelayerStatsPayload struct {
	W int   `json:"w"`
	H int   `json:"h"`
	B int64 `json:"b"`
	Y int64 `json:"y"`
	T int64 `json:"t"`
}

func parseEffectStatsEval(raw json.RawMessage) (purelayerStatsPayload, bool) {
	if len(raw) == 0 {
		return purelayerStatsPayload{}, false
	}
	var outer struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil || outer.Result.Value == "" {
		return purelayerStatsPayload{}, false
	}
	var st purelayerStatsPayload
	if err := json.Unmarshal([]byte(outer.Result.Value), &st); err != nil {
		return purelayerStatsPayload{}, false
	}
	return st, true
}

// effectSessionRUM 汇总单次特效镜像会话的 RUM 指标；所有可变字段受 mu 保护，
// 与退出路径 FlushRUM 并发安全且带完成屏障（endDone），持锁期间绝不调用 rum.Track。
type effectSessionRUM struct {
	mu               sync.Mutex
	mode             string
	strategy         string
	startedAt        time.Time
	showing          bool
	showingSince     time.Time
	showingActiveDur time.Duration
	parkCount        int
	orphanedRestored bool
	canvasW          int
	canvasH          int

	// 纯层统计基线与会话内单调增量（兼容页面脚本跨会话保留 __mbxCapStats 或重载归零）
	statsInitialized bool
	prevBlob         int64
	prevSync         int64
	prevStale        int64
	deltaBlob        int64
	deltaSync        int64
	deltaStale       int64
	broadcastFrames  int64

	ended          bool
	endDone        chan struct{}
	testBeforeEmit func() // 仅供单测注入完成屏障验证，生产恒为 nil
}

func (c *Capturer) drainIngest403() uint64 {
	if d, ok := c.sink.(ingest403Drainer); ok {
		return d.DrainEffectIngest403()
	}
	return 0
}

// FlushRUM 在主进程退出前同步结算活动特效镜像会话的 effect_mirror_session 事件（并发安全、幂等且带完成屏障）。
func (c *Capturer) FlushRUM() {
	c.rumGate.Flush(func(s *effectSessionRUM) {
		s.end(c.drainIngest403)
	})
}

func (c *Capturer) registerEffectSessionRUM(mode string, initFn func(*effectSessionRUM)) *effectSessionRUM {
	// 锁外采集只读外部状态（避免持 rumGate.mu 时跨越 sink 锁序）。
	now := time.Now()
	showing := c.isShowing()
	strategy := c.sink.EffectStrategy()

	sess, ok := c.rumGate.Register(
		func() *effectSessionRUM {
			s := &effectSessionRUM{
				mode:             mode,
				strategy:         strategy,
				startedAt:        now,
				showing:          showing,
				orphanedRestored: park.TakeOrphanedRestored(),
				endDone:          make(chan struct{}),
			}
			if showing {
				s.showingSince = now
			}
			if initFn != nil {
				initFn(s)
			}
			return s
		},
		func(*effectSessionRUM) {
			if c.testBeforeAttach != nil {
				c.testBeforeAttach()
			}
		},
	)
	if !ok {
		return nil
	}
	return sess
}

func (c *Capturer) endEffectSessionRUM(s *effectSessionRUM) {
	if s == nil {
		return
	}
	c.rumGate.End(s, func(sess *effectSessionRUM) {
		sess.end(c.drainIngest403)
	})
}

func (c *Capturer) startEffectSessionRUM(mode string) *effectSessionRUM {
	now := time.Now()
	showing := c.isShowing()
	s := &effectSessionRUM{
		mode:             mode,
		strategy:         c.sink.EffectStrategy(),
		startedAt:        now,
		showing:          showing,
		orphanedRestored: park.TakeOrphanedRestored(),
		endDone:          make(chan struct{}),
	}
	if showing {
		s.showingSince = now
	}
	return s
}

func (s *effectSessionRUM) onShowingChange(v bool, now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.ended && s.showing != v {
		if s.showing && !s.showingSince.IsZero() {
			if d := now.Sub(s.showingSince); d > 0 {
				s.showingActiveDur += d
			}
			s.showingSince = time.Time{}
		}
		s.showing = v
		if v {
			s.showingSince = now
		}
	}
	s.mu.Unlock()
}

func (s *effectSessionRUM) onPark() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.ended {
		s.parkCount++
	}
	s.mu.Unlock()
}

func (s *effectSessionRUM) onBroadcastFrame() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.ended {
		s.broadcastFrames++
	}
	s.mu.Unlock()
}

func (s *effectSessionRUM) setCanvasSize(w, h int) {
	if s == nil || w <= 0 || h <= 0 {
		return
	}
	s.mu.Lock()
	if !s.ended {
		s.canvasW = w
		s.canvasH = h
	}
	s.mu.Unlock()
}

func (s *effectSessionRUM) initPurelayerBaseline(w, h int, blob, syncCnt, stale int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.ended {
		if w > 0 && h > 0 {
			s.canvasW = w
			s.canvasH = h
		}
		s.statsInitialized = true
		s.prevBlob = max(blob, 0)
		s.prevSync = max(syncCnt, 0)
		s.prevStale = max(stale, 0)
	}
	s.mu.Unlock()
}

func (s *effectSessionRUM) onPurelayerStats(w, h int, blob, syncCnt, stale int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.ended {
		if w > 0 && h > 0 {
			s.canvasW = w
			s.canvasH = h
		}
		blob = max(blob, 0)
		syncCnt = max(syncCnt, 0)
		stale = max(stale, 0)
		if !s.statsInitialized {
			s.statsInitialized = true
			s.deltaBlob += blob
			s.deltaSync += syncCnt
			s.deltaStale += stale
		} else {
			if blob >= s.prevBlob {
				s.deltaBlob += blob - s.prevBlob
			} else {
				s.deltaBlob += blob
			}
			if syncCnt >= s.prevSync {
				s.deltaSync += syncCnt - s.prevSync
			} else {
				s.deltaSync += syncCnt
			}
			if stale >= s.prevStale {
				s.deltaStale += stale - s.prevStale
			} else {
				s.deltaStale += stale
			}
		}
		s.prevBlob = blob
		s.prevSync = syncCnt
		s.prevStale = stale
	}
	s.mu.Unlock()
}

func (s *effectSessionRUM) end(drain403 func() uint64) {
	if s == nil {
		return
	}
	now := time.Now()
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
	if s.showing && !s.showingSince.IsZero() {
		if d := now.Sub(s.showingSince); d > 0 {
			s.showingActiveDur += d
		}
		s.showingSince = time.Time{}
	}
	mode := s.mode
	strategy := s.strategy
	durSec := int64(now.Sub(s.startedAt).Seconds())
	if durSec < 0 {
		durSec = 0
	}
	showingSec := int64(s.showingActiveDur.Seconds())
	if showingSec < 0 {
		showingSec = 0
	}
	parkCount := s.parkCount
	orphaned := s.orphanedRestored
	cw, ch := s.canvasW, s.canvasH
	dBlob, dSync, dStale := s.deltaBlob, s.deltaSync, s.deltaStale
	bFrames := s.broadcastFrames
	activeDurSec := s.showingActiveDur.Seconds()
	totalDurSec := now.Sub(s.startedAt).Seconds()
	hook := s.testBeforeEmit
	s.mu.Unlock()

	defer close(done)
	if hook != nil {
		hook()
	}

	var ingest403 uint64
	if drain403 != nil {
		ingest403 = drain403()
	}

	// 统一分子分母口径：优先以门控后实际广播帧数除以有效显示时长；
	// 无广播帧时退回页面整场编码帧数除以整场时长，避免隐藏期间页面继续编码导致帧率虚高。
	var effFPS float64
	if bFrames > 0 && activeDurSec > 0 {
		effFPS = math.Round((float64(bFrames)/activeDurSec)*10) / 10
	} else if encFrames := dBlob + dSync; encFrames > 0 && totalDurSec > 0 {
		effFPS = math.Round((float64(encFrames)/totalDurSec)*10) / 10
	} else if bFrames > 0 && totalDurSec > 0 {
		effFPS = math.Round((float64(bFrames)/totalDurSec)*10) / 10
	}

	var syncRatio float64
	if dBlob+dSync > 0 {
		syncRatio = math.Round((float64(dSync)/float64(dBlob+dSync))*10000) / 10000
	}

	rum.Track(rum.CatAudioEffect, "effect_mirror_session", rum.Props{
		"mode":                 mode,
		"strategy":             strategy,
		"duration_sec":         durSec,
		"showing_active_sec":   showingSec,
		"park_count":           parkCount,
		"orphaned_restored":    orphaned,
		"canvas_w":             cw,
		"canvas_h":             ch,
		"downsampled_1920":     cw > captureOutMaxW,
		"effective_fps":        effFPS,
		"sync_fallback_ratio":  syncRatio,
		"stale_dropped_frames": dStale,
		"ingest_forbidden_403": ingest403,
	})
}
