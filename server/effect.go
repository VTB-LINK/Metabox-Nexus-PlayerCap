package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// --- 网易云特效镜像帧分发 ---
//
// 与 JSON 事件通道分离的专用通道：捕获器把网易云特效 canvas 的 JPEG 帧
// 通过 BroadcastEffectFrame 推进来，本 Hub 以二进制 WS 消息广播给所有
// /cloudmusicv3/effect-ws 订阅者。高频帧采用「满则丢」策略，永远只发最新帧，
// 避免慢客户端造成积压延迟。

// cloudmusicPlayerName 网易云播放器名（与 player/cloudmusic.PlayerName 一致）。
// 此处硬编码避免 server→cloudmusic 的反向依赖。
const cloudmusicPlayerName = "cloudmusicv3"

// effectMsg 一条待发往 effect-ws 的消息：mt 为 websocket.TextMessage(状态) 或 BinaryMessage(帧)。
type effectMsg struct {
	mt   int
	data []byte
}

type effectSub struct {
	ch chan effectMsg
}

type effectHub struct {
	mu   sync.Mutex
	subs map[*effectSub]struct{}

	// 截帧/注入参数（由前端连接时通过 query 设置，捕获器按需读取并在变更时重启）
	quality         int     // JPEG 质量 1-100
	scale           float64 // 相对网易云原生物理分辨率的缩放，0-1（默认 1 = 原生，即跟随 Windows 缩放）
	headerClickable bool    // 双击隐藏顶栏后是否保留其点击/拖动能力（默认 true）
	footerClickable bool    // 双击隐藏底栏后是否保留其点击能力（默认 false）
	gen             uint64  // 参数变更代号，捕获器据此判断是否需要重启
	showing         bool    // 网易云是否正打开歌曲详情页（特效在显示），由捕获器轮询写入

	strategy      string // 最小化/迷你时策略："fadeout"(默认) | "park"
	manualParkCmd int    // 一次性手动命令：-1=无，1=park，0=unpark（捕获器取走后复位）

	// 纯层捕获（page→Go 帧回传）：
	// 注入网易云页面的抓帧脚本经 /cloudmusicv3/effect-ingest 把特效 canvas 的 JPEG 字节推进来，
	// 由捕获器注册的 ingestHandler 做帧门控后广播给 effect-ws 订阅者。
	ingestHandler func([]byte) // 捕获器注册：处理一帧 ingest 进来的 JPEG（含门控）
	ingestPort    string       // 服务监听端口，供拼接注入脚本里的 page→Go WS 地址
}

func newEffectHub() *effectHub {
	return &effectHub{
		subs:            make(map[*effectSub]struct{}),
		quality:         95,  // 纯层直读 canvas 原生分辨率，细线条易出 JPEG 色度色块；q95 基本消除（本机带宽无忧）
		scale:           0.7, // 仅 screencast 兜底用；纯层不缩放（直读原生分辨率）
		headerClickable: true,
		footerClickable: false,
		strategy:        "fadeout",
		manualParkCmd:   -1,
	}
}

// EffectStrategy 返回最小化/迷你时的策略（fadeout | park），供捕获器读取。
func (s *Server) EffectStrategy() string {
	h := s.effectHub
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.strategy
}

// SetDefaultEffectStrategy 由 main.go 用 config.yml 的 cloudmusicv3-effect-strategy 设置最小化策略。
// 改为 config 静态读取（而非运行时端点动态切换），因为 /service-status 是静态快照、反映不了动态变更。
func (s *Server) SetDefaultEffectStrategy(strategy string) {
	if strategy != "park" && strategy != "fadeout" {
		return
	}
	h := s.effectHub
	h.mu.Lock()
	h.strategy = strategy
	h.mu.Unlock()
}

// TakeManualParkCmd 取走一次性手动 park 命令（-1 无 / 1 park / 0 unpark），取走即复位。
func (s *Server) TakeManualParkCmd() int {
	h := s.effectHub
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.manualParkCmd
	h.manualParkCmd = -1
	return c
}

// handleEffectControl 运行时控制：切换策略 / 手动 park / unpark。
//
//	GET /cloudmusicv3/effect-control?strategy=park|fadeout
//	GET /cloudmusicv3/effect-control?park=1   手动泊车
//	GET /cloudmusicv3/effect-control?park=0   手动还原
func (s *Server) handleEffectControl(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h := s.effectHub
	h.mu.Lock()
	if v := q.Get("strategy"); v == "park" || v == "fadeout" {
		h.strategy = v
	}
	if v := q.Get("park"); v == "1" {
		h.manualParkCmd = 1
	} else if v == "0" {
		h.manualParkCmd = 0
	}
	resp := fmt.Sprintf("strategy=%s manualParkCmd=%d", h.strategy, h.manualParkCmd)
	h.mu.Unlock()
	serverLog.Info("网易云特效控制: %s", resp)
	w.Write([]byte(resp + "\n"))
}

// setEffectParams 由前端连接时调用，更新截帧/注入参数（变更则递增 gen）。
func (s *Server) setEffectParams(quality int, scale float64, headerClickable, footerClickable bool) {
	if quality < 1 {
		quality = 1
	} else if quality > 100 {
		quality = 100
	}
	if scale < 0.1 {
		scale = 0.1
	} else if scale > 1 {
		scale = 1
	}
	h := s.effectHub
	h.mu.Lock()
	if quality != h.quality || scale != h.scale || headerClickable != h.headerClickable || footerClickable != h.footerClickable {
		h.quality = quality
		h.scale = scale
		h.headerClickable = headerClickable
		h.footerClickable = footerClickable
		h.gen++
		serverLog.Info("网易云特效参数更新: quality=%d scale=%.2f headerClickable=%v footerClickable=%v (gen=%d)",
			quality, scale, headerClickable, footerClickable, h.gen)
	}
	h.mu.Unlock()
}

// EffectCaptureParams 供捕获器读取当前截帧/注入参数与变更代号。
func (s *Server) EffectCaptureParams() (quality int, scale float64, headerClickable, footerClickable bool, gen uint64) {
	h := s.effectHub
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.quality, h.scale, h.headerClickable, h.footerClickable, h.gen
}

// HasEffectSubscribers 是否有客户端在看特效镜像（捕获器据此按需启停 screencast）
func (s *Server) HasEffectSubscribers() bool {
	h := s.effectHub
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0
}

// BroadcastEffectFrame 向所有特效订阅者广播一帧 JPEG（满则丢最新优先）
func (s *Server) BroadcastEffectFrame(jpeg []byte) {
	msg := effectMsg{mt: websocket.BinaryMessage, data: jpeg}
	h := s.effectHub
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		select {
		case sub.ch <- msg:
		default:
			// 通道已满：丢弃旧消息，写入最新帧，保证低延迟
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- msg:
			default:
			}
		}
	}
}

// effectStatusJSON 构造当前状态消息：网易云是否为活跃输出 + 是否正显示特效详情页。
func (s *Server) effectStatusJSON() []byte {
	s.mu.RLock()
	cmActive := s.activePlayer == cloudmusicPlayerName
	s.mu.RUnlock()
	h := s.effectHub
	h.mu.Lock()
	showing := h.showing
	h.mu.Unlock()
	return []byte(fmt.Sprintf(`{"type":"status","cmActive":%t,"showing":%t}`, cmActive, showing))
}

// SetEffectShowing 由捕获器轮询写入：网易云是否正打开歌曲详情页（特效在显示）。变更则广播。
func (s *Server) SetEffectShowing(showing bool) {
	h := s.effectHub
	h.mu.Lock()
	changed := h.showing != showing
	h.showing = showing
	h.mu.Unlock()
	if changed {
		s.BroadcastEffectStatus()
	}
}

// BroadcastEffectStatus 向所有特效订阅者广播状态（文本消息，不丢弃）。
func (s *Server) BroadcastEffectStatus() {
	msg := effectMsg{mt: websocket.TextMessage, data: s.effectStatusJSON()}
	h := s.effectHub
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		select {
		case sub.ch <- msg:
		default:
			// 满则腾一格再发，确保状态不丢
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- msg:
			default:
			}
		}
	}
}

// SetEffectIngestHandler 由捕获器注册：接收一帧从网易云页面 ingest 进来的 JPEG（捕获器内部做门控+广播）。
func (s *Server) SetEffectIngestHandler(fn func([]byte)) {
	h := s.effectHub
	h.mu.Lock()
	h.ingestHandler = fn
	h.mu.Unlock()
}

// EffectIngestWSURL 返回注入脚本用的 page→Go 帧回传地址（本机回环）。
func (s *Server) EffectIngestWSURL() string {
	h := s.effectHub
	h.mu.Lock()
	port := h.ingestPort
	h.mu.Unlock()
	if port == "" {
		port = "8765"
	}
	return "ws://127.0.0.1:" + port + "/cloudmusicv3/effect-ingest"
}

// handleEffectIngest 接收网易云注入脚本推来的二进制 JPEG 帧，交给捕获器注册的 handler。
// 单一生产者（注入脚本只开一条），故不做并发保护；handler 缺失则丢弃。
func (s *Server) handleEffectIngest(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		serverLog.Warn("网易云特效Ingest Upgrade 失败: %v", err)
		return
	}
	defer conn.Close()
	serverLog.Info("网易云特效Ingest 连接: %s", conn.RemoteAddr())
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt != websocket.BinaryMessage || len(data) == 0 {
			continue
		}
		h := s.effectHub
		h.mu.Lock()
		fn := h.ingestHandler
		h.mu.Unlock()
		if fn != nil {
			fn(data)
		}
	}
	serverLog.Info("网易云特效Ingest 断开: %s", conn.RemoteAddr())
}

// parseBoolParam 解析 "1"/"0"/"true"/"false"/"yes"/"no"，无法识别时返回 def。
func parseBoolParam(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func (s *Server) handleEffectWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		serverLog.Warn("网易云特效WS Upgrade 失败: %v", err)
		return
	}

	// 读取本次连接的截帧/注入偏好（缺省则沿用当前值）。
	//
	// **必须在 Upgrade 成功之后**：这些参数是 effectHub 级的全局单例，无 per-connection
	// 状态可回滚，而 Upgrade 失败是直接 return 的。放在它之前 → 任何一个连不上的请求
	// （主播在地址栏敲 URL 探活、curl 试端口）都会把直播画质当场改掉，只能重启服务恢复。
	//
	// **但必须仍排在下面 h.subs[sub] 登记之前**，别挪到那之后：捕获器唯一的感知入口是
	// HasEffectSubscribers()，session 开头才读 EffectCaptureParams 并锁定 startGen。排在
	// 登记之前 → gen++ 严格 happens-before 订阅者可见，零窗口；排在之后 → 捕获器可能以
	// 旧参数启动 session，而 effect.go 的 400ms 热更只重写 quality/fps，header/footer 的
	// 注入只在会话启动时做一次 —— 本次连接请求的 header/footer 会静默失效。
	//
	// **也别为「原子」把它并进那个临界区**：setEffectParams 内部自取 h.mu（:135），
	// Go 的 sync.Mutex 不可重入 → 持锁自死锁，handler 会抱着 h.mu 永久卡住，连带冻死
	// BroadcastEffectFrame / HasEffectSubscribers / EffectCaptureParams / handleEffectIngest
	// 整个特效子系统。上面的顺序已提供同等保证，原子性无收益。
	q := r.URL.Query()
	curQ, curS, curHC, curFC, _ := s.EffectCaptureParams()
	quality, scale, headerClickable, footerClickable := curQ, curS, curHC, curFC
	if v := q.Get("quality"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			quality = n
		}
	}
	if v := q.Get("scale"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			scale = f
		}
	}
	if v := q.Get("header_clickable"); v != "" {
		headerClickable = parseBoolParam(v, headerClickable)
	}
	if v := q.Get("footer_clickable"); v != "" {
		footerClickable = parseBoolParam(v, footerClickable)
	}
	s.setEffectParams(quality, scale, headerClickable, footerClickable)

	sub := &effectSub{ch: make(chan effectMsg, 8)}
	h := s.effectHub
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	count := len(h.subs)
	h.mu.Unlock()
	serverLog.Info("网易云特效WS 连接: %s (total: %d)", conn.RemoteAddr(), count)

	// 连接即发一次当前状态（网易云是否活跃输出）
	sub.ch <- effectMsg{mt: websocket.TextMessage, data: s.effectStatusJSON()}

	// 写失败必须显式关连接，理由同 server.go 的 handleWS：写失败不蕴含连接已断，
	// 只 return 会让读循环永远阻塞、sub 永远留在 h.subs 里（截帧管线因此继续为
	// 一个收不到帧的僵尸供帧）。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range sub.ch {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(msg.mt, msg.data); err != nil {
				serverLog.Warn("网易云特效WS 写入失败，关闭连接: %s: %v", conn.RemoteAddr(), err)
				conn.Close() // 解阻塞读循环，交由下面既有的清理路径注销
				return
			}
		}
	}()

	// Read loop：保活 + 断开检测（也消费客户端可能发来的控制消息）
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}

	h.mu.Lock()
	delete(h.subs, sub)
	count = len(h.subs)
	h.mu.Unlock()
	close(sub.ch)
	conn.Close()
	<-done
	serverLog.Info("网易云特效WS 断开: %s (total: %d)", conn.RemoteAddr(), count)
}
