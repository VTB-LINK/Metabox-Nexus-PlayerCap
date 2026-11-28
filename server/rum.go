package server

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"Metabox-Nexus-PlayerCap/rum"
)

const (
	clientSurfaceOnlineEditor = "online_editor"
	clientSurfaceOnlineV3     = "online_v3"
	clientSurfaceLocalFile    = "local_file"
	clientSurfaceOBSBrowser   = "obs_browser"
	clientSurfaceBrowserOther = "browser_other"
	clientSurfaceCustom       = "custom_client"

	// httpSummaryInterval 为旧式 HTTP 快照轮询接口的定时汇总上报间隔（复用 watchIdleHide 节拍，不新增协程）。
	httpSummaryInterval = 10 * time.Minute
)

const (
	surfaceIdxOnlineEditor = iota
	surfaceIdxOnlineV3
	surfaceIdxLocalFile
	surfaceIdxOBSBrowser
	surfaceIdxBrowserOther
	surfaceIdxCustomClient
	surfaceIdxCount
)

var surfaceIndexNames = [surfaceIdxCount]string{
	surfaceIdxOnlineEditor: clientSurfaceOnlineEditor,
	surfaceIdxOnlineV3:     clientSurfaceOnlineV3,
	surfaceIdxLocalFile:    clientSurfaceLocalFile,
	surfaceIdxOBSBrowser:   clientSurfaceOBSBrowser,
	surfaceIdxBrowserOther: clientSurfaceBrowserOther,
	surfaceIdxCustomClient: clientSurfaceCustom,
}

const (
	httpRouteAllLyrics = iota
	httpRouteLyricUpdate
	httpRouteSongInfo
	httpRouteStatusUpdate
	httpRouteServiceStatus
	httpRouteCount
)

var httpRouteNames = [httpRouteCount]string{
	httpRouteAllLyrics:     "all_lyrics",
	httpRouteLyricUpdate:   "lyric_update",
	httpRouteSongInfo:      "song_info",
	httpRouteStatusUpdate:  "status_update",
	httpRouteServiceStatus: "service_status",
}

// StatsSnapshot 为导出给 main.go 心跳与退出事件的只读运行期统计快照。
type StatsSnapshot struct {
	RouteSwitches     uint64
	PeakWSClients     uint64
	PeakAudioClients  uint64
	PeakEffectClients uint64
}

// httpSnapshotStats 采用轻量叶子锁聚合旧式 HTTP 快照请求与无活跃隐藏命中，保证单次请求的路由计数与
// idle_hidden_hits 原子更新，并与定时/退出冲刷严格原子切换，杜绝空 counts 配非零 idle_hidden_hits 的撕裂汇总。
type httpSnapshotStats struct {
	mu             sync.Mutex
	counts         [surfaceIdxCount][httpRouteCount]uint64
	idleHiddenHits [surfaceIdxCount]uint64
}

func surfaceToIndex(surface string) int {
	switch surface {
	case clientSurfaceOnlineEditor:
		return surfaceIdxOnlineEditor
	case clientSurfaceOnlineV3:
		return surfaceIdxOnlineV3
	case clientSurfaceLocalFile:
		return surfaceIdxLocalFile
	case clientSurfaceOBSBrowser:
		return surfaceIdxOBSBrowser
	case clientSurfaceBrowserOther:
		return surfaceIdxBrowserOther
	default:
		return surfaceIdxCustomClient
	}
}

// classifyClientSurface 将客户端 Origin / Referer / User-Agent 白名单归类为 6 个固定枚举，
// 绝不输出任何原始 URL、本地路径或查询参数。
func classifyClientSurface(origin, referer, userAgent string) string {
	var sawEditor, sawFile bool
	for _, raw := range [2]string{origin, referer} {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.EqualFold(trimmed, "null") {
			continue
		}
		u, err := url.Parse(trimmed)
		if err != nil {
			continue
		}
		if strings.EqualFold(u.Scheme, "file") {
			sawFile = true
			continue
		}
		if strings.EqualFold(u.Hostname(), "playercap.cfg.vtb.tools") {
			p := strings.ToLower(u.Path)
			if p == "/v3" || strings.HasPrefix(p, "/v3/") {
				return clientSurfaceOnlineV3
			}
			sawEditor = true
		}
	}
	if sawEditor {
		return clientSurfaceOnlineEditor
	}
	if sawFile {
		return clientSurfaceLocalFile
	}

	uaLower := strings.ToLower(userAgent)
	if strings.Contains(uaLower, "obs") {
		return clientSurfaceOBSBrowser
	}

	if strings.EqualFold(strings.TrimSpace(origin), "null") {
		return clientSurfaceLocalFile
	}

	for _, marker := range []string{"mozilla/", "chrome/", "edg/", "firefox/", "safari/"} {
		if strings.Contains(uaLower, marker) {
			return clientSurfaceBrowserOther
		}
	}
	return clientSurfaceCustom
}

func classifyRequestSurface(r *http.Request) string {
	if r == nil {
		return clientSurfaceCustom
	}
	return classifyClientSurface(r.Header.Get("Origin"), r.Header.Get("Referer"), r.Header.Get("User-Agent"))
}

// routerTransitionEvent 在持有 Router.mu / Server.mu 期间采集快照，解锁后统一发送 RUM。
type routerTransitionEvent struct {
	kind              string
	fromPlayer        string
	toPlayer          string
	reason            string
	activePriorCount  int
	activeNormalCount int
}

func emitRouterTransition(ev routerTransitionEvent) {
	rum.Track(rum.CatRouteClient, "router_transition", rum.Props{
		"kind":                ev.kind,
		"from_player":         ev.fromPlayer,
		"to_player":           ev.toPlayer,
		"reason":              ev.reason,
		"active_prior_count":  ev.activePriorCount,
		"active_normal_count": ev.activeNormalCount,
	})
}

func emitRouterTransitions(list []routerTransitionEvent) {
	for _, ev := range list {
		emitRouterTransition(ev)
	}
}

// clientSessionMeta 记录单个下游订阅连接的脱敏会话元数据，通过 atomic.Bool + 完成屏障 endDone
// 保证断开与退出冲刷严格单次结算且竞争失败方等待 rum.Track 完成；connDone 保证 connected 先于 ended 入队。
type clientSessionMeta struct {
	channel        string
	targetPlayer   string
	clientSurface  string
	connectedAt    time.Time
	writeFailed    atomic.Bool
	ended          atomic.Bool
	connDone       chan struct{}
	endDone        chan struct{}
	testBeforeEmit func() // 仅供单测注入完成屏障交错验证，生产恒为 nil
}

func newClientSessionMeta(channel, targetPlayer, clientSurface string, now time.Time) *clientSessionMeta {
	if channel == "" {
		return nil
	}
	return &clientSessionMeta{
		channel:       channel,
		targetPlayer:  targetPlayer,
		clientSurface: clientSurface,
		connectedAt:   now,
		endDone:       make(chan struct{}),
	}
}

func emitClientConnected(channel, targetPlayer, clientSurface string) {
	if channel == "" {
		return
	}
	rum.TrackOnce("client_first_seen:"+channel+":"+clientSurface, rum.CatRouteClient, "client_session", rum.Props{
		"action":         "connected",
		"channel":        channel,
		"target_player":  targetPlayer,
		"client_surface": clientSurface,
	})
}

func (m *clientSessionMeta) markWriteFailed() {
	if m != nil {
		m.writeFailed.Store(true)
	}
}

func (m *clientSessionMeta) emitEnded(dropped uint64, now time.Time) {
	if m == nil || m.channel == "" {
		return
	}
	if !m.ended.CompareAndSwap(false, true) {
		if m.endDone != nil {
			<-m.endDone
		}
		return
	}
	if m.endDone != nil {
		defer close(m.endDone)
	}
	if m.connDone != nil {
		<-m.connDone
	}
	if m.testBeforeEmit != nil {
		m.testBeforeEmit()
	}
	durSec := int64(now.Sub(m.connectedAt).Seconds())
	if durSec < 0 {
		durSec = 0
	}
	rum.Track(rum.CatRouteClient, "client_session", rum.Props{
		"action":                   "ended",
		"channel":                  m.channel,
		"target_player":            m.targetPlayer,
		"client_surface":           m.clientSurface,
		"duration_sec":             durSec,
		"dropped_frames_or_events": dropped,
		"write_failed":             m.writeFailed.Load(),
	})
}

// registerClientSession 在独立叶子锁 rumSessMu 下检查退出闸门并登记会话，锁外经 connDone 屏障
// 发出首次 connected；若 FlushRUM 已触发则直接返回 nil，杜绝退出后产生孤儿 connected。
func (s *Server) registerClientSession(m *clientSessionMeta, droppedFn func() uint64) *clientSessionMeta {
	if m == nil || m.channel == "" {
		return nil
	}
	connDone := make(chan struct{})
	m.connDone = connDone

	s.rumSessMu.Lock()
	if s.rumFlushed {
		s.rumSessMu.Unlock()
		return nil
	}
	if s.rumActive == nil {
		s.rumActive = make(map[*clientSessionMeta]func() uint64)
	}
	s.rumActive[m] = droppedFn
	s.rumSessMu.Unlock()

	defer close(connDone)
	emitClientConnected(m.channel, m.targetPlayer, m.clientSurface)
	return m
}

// endClientSession 在锁外完成 emitEnded（含完成屏障）后再从 rumActive 移除会话，
// 确保已 unsubscribe 正在等待写协程退出的连接以及在途 emitEnded 均能被 FlushRUM 观测并等齐。
func (s *Server) endClientSession(m *clientSessionMeta, dropped uint64, now time.Time) {
	if m == nil {
		return
	}
	m.emitEnded(dropped, now)
	s.rumSessMu.Lock()
	delete(s.rumActive, m)
	s.rumSessMu.Unlock()
}

func (s *Server) addRouteSwitch() {
	s.routeSwitches.Add(1)
}

func (s *Server) setActiveGroupCounts(prior, normal int) {
	s.activePriorCount.Store(int32(prior))
	s.activeNormalCount.Store(int32(normal))
}

func (s *Server) activeGroupCounts() (prior, normal int) {
	return int(s.activePriorCount.Load()), int(s.activeNormalCount.Load())
}

// StatsSnapshot 返回路由切换累计与三类客户端周期峰值的纯只读快照，不修改或清零周期峰值。
func (s *Server) StatsSnapshot() StatsSnapshot {
	s.subMu.Lock()
	wsPeak := max(s.wsPeak, uint64(len(s.subscribers)))
	s.subMu.Unlock()

	s.audioHub.mu.Lock()
	audioPeak := max(s.audioHub.peakSubs, uint64(len(s.audioHub.subs)))
	s.audioHub.mu.Unlock()

	s.effectHub.mu.Lock()
	effectPeak := max(s.effectHub.peakSubs, uint64(len(s.effectHub.subs)))
	s.effectHub.mu.Unlock()

	return StatsSnapshot{
		RouteSwitches:     s.routeSwitches.Load(),
		PeakWSClients:     wsPeak,
		PeakAudioClients:  audioPeak,
		PeakEffectClients: effectPeak,
	}
}

// TakeHeartbeatStatsSnapshot 在各订阅锁内单步原子读取三类客户端周期峰值并回落为当前活跃连接数，
// 避免只读与重置拆成两步时丢掉窗口内新出现的短暂峰值。仅由每日心跳在确认跨日后调用。
func (s *Server) TakeHeartbeatStatsSnapshot() StatsSnapshot {
	s.subMu.Lock()
	wsCur := uint64(len(s.subscribers))
	wsPeak := max(s.wsPeak, wsCur)
	s.wsPeak = wsCur
	s.subMu.Unlock()

	s.audioHub.mu.Lock()
	audioCur := uint64(len(s.audioHub.subs))
	audioPeak := max(s.audioHub.peakSubs, audioCur)
	s.audioHub.peakSubs = audioCur
	s.audioHub.mu.Unlock()

	s.effectHub.mu.Lock()
	effectCur := uint64(len(s.effectHub.subs))
	effectPeak := max(s.effectHub.peakSubs, effectCur)
	s.effectHub.peakSubs = effectCur
	s.effectHub.mu.Unlock()

	return StatsSnapshot{
		RouteSwitches:     s.routeSwitches.Load(),
		PeakWSClients:     wsPeak,
		PeakAudioClients:  audioPeak,
		PeakEffectClients: effectPeak,
	}
}

// DrainEffectIngest403 原子取出并清零 effect-ingest 403 拒绝计数，供特效镜像会话汇总读取。
func (s *Server) DrainEffectIngest403() uint64 {
	return s.effectHub.ingestForbidden403.Swap(0)
}

func (s *Server) recordHTTPSnapshot(r *http.Request, routeIdx int, idleHidden bool) {
	if routeIdx < 0 || routeIdx >= httpRouteCount {
		return
	}
	sIdx := surfaceToIndex(classifyRequestSurface(r))
	s.httpStats.mu.Lock()
	s.httpStats.counts[sIdx][routeIdx]++
	if idleHidden {
		s.httpStats.idleHiddenHits[sIdx]++
	}
	s.httpStats.mu.Unlock()
}

func (s *Server) flushHTTPSnapshotSummary() {
	s.httpStats.mu.Lock()
	counts := s.httpStats.counts
	hiddenAll := s.httpStats.idleHiddenHits
	s.httpStats.counts = [surfaceIdxCount][httpRouteCount]uint64{}
	s.httpStats.idleHiddenHits = [surfaceIdxCount]uint64{}
	s.httpStats.mu.Unlock()

	for sIdx := 0; sIdx < surfaceIdxCount; sIdx++ {
		var countsByRoute map[string]uint64
		var total uint64
		for rIdx := 0; rIdx < httpRouteCount; rIdx++ {
			if n := counts[sIdx][rIdx]; n > 0 {
				if countsByRoute == nil {
					countsByRoute = make(map[string]uint64, httpRouteCount)
				}
				countsByRoute[httpRouteNames[rIdx]] = n
				total += n
			}
		}
		hidden := hiddenAll[sIdx]
		if total == 0 && hidden == 0 {
			continue
		}
		if countsByRoute == nil {
			countsByRoute = map[string]uint64{}
		}
		rum.Track(rum.CatDiagFine, "http_snapshot_summary", rum.Props{
			"client_surface":   surfaceIndexNames[sIdx],
			"counts_by_route":  countsByRoute,
			"idle_hidden_hits": hidden,
		})
	}
}

type rumActiveEntry struct {
	sess      *clientSessionMeta
	droppedFn func() uint64
}

// FlushRUM 在进程退出前结算全部未结清客户端会话（含仍在连、已 unsubscribe 待写协程退出以及在途 emitEnded）
// 并置位退出结算标志（其后新登记连接不发 connected 也不发 ended），同时冲刷非零旧式 HTTP 轮询汇总；
// 全部 ended 上报及完成屏障等待均在锁外执行。
func (s *Server) FlushRUM() {
	now := time.Now()

	s.rumSessMu.Lock()
	s.rumFlushed = true
	active := make([]rumActiveEntry, 0, len(s.rumActive))
	for sess, droppedFn := range s.rumActive {
		active = append(active, rumActiveEntry{sess: sess, droppedFn: droppedFn})
	}
	s.rumSessMu.Unlock()

	for _, entry := range active {
		var dropped uint64
		if entry.droppedFn != nil {
			dropped = entry.droppedFn()
		}
		s.endClientSession(entry.sess, dropped, now)
	}

	s.flushHTTPSnapshotSummary()
}
