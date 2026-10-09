package server

import (
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// --- 音频波形帧分发 ---
//
// 克隆 effect.go 的 drop-oldest 二进制扇出（effectHub/BroadcastEffectFrame），
// 但要服务两种拓扑：root（/audio-ws，跟随 activePlayer）与 per-player（/<player>/audio-ws，
// 固定那一家）。故在 effect 的「满则丢最旧」之上加 player 键路由 + active 缓存。
//
// payload 为不透明帧字节（helper 侧编码），主服务不解读其内部结构，原样转发。
// 所有发送一律非阻塞 drop-oldest：慢客户端只丢帧、永不回压主服务或 helper 排空循环。

// audioSub 是一个音频帧订阅者。player=="" 为 root（跟随 activePlayer 缓存）；非空为固定那一家。
type audioSub struct {
	ch     chan []byte
	player string
}

// audioHub 按 player 键 fan-out 音频帧。
//
// active 是 activePlayer 的本 hub 缓存，唯一写入者为 onActivePlayerChanged（§3.3 钩子）。
// 路由判定（root 该不该收某家帧）一律读本缓存——**绝不在持 h.mu 时回读 Server 的
// s.activePlayer（s.mu）**，以免形成 h.mu→s.mu 的反向锁序（§3.4）。
type audioHub struct {
	mu     sync.Mutex
	subs   map[*audioSub]struct{}
	active string // 当前 activePlayer 缓存（由 onActivePlayerChanged 写）
}

func newAudioHub() *audioHub {
	return &audioHub{subs: make(map[*audioSub]struct{})}
}

// onActivePlayerChanged 只写 active 缓存（持 mu）。必须非阻塞、不 panic——它由 SetActivePlayer
// 钩子在 router 的 r.mu 写锁主线上调用（Task 9 接线），任何阻塞/panic 都会拖垮主线（§3.4）。
// 只做一次字段写，不遍历 subs、不发送、不回读 s.mu。
func (h *audioHub) onActivePlayerChanged(name string) {
	h.mu.Lock()
	h.active = name
	h.mu.Unlock()
}

// HasSubscribers 判断 name 这一家是否需要被捕获：存在 name 的 per-player sub，
// 或（存在 root sub 且 name == active 缓存）。后者是「只连 root（OBS 主用法）时也要抓
// 当前 active」的关键判据，缺了 root-only 场景永远收不到帧（§3.2）。
func (h *audioHub) HasSubscribers(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	hasRoot := false
	for sub := range h.subs {
		if sub.player == "" {
			hasRoot = true
			continue
		}
		if sub.player == name {
			return true // per-player 命中
		}
	}
	return hasRoot && name == h.active
}

// AnySubscribers 是否有任一订阅者，供捕获管理器给「进程扫描本身」门控（Task 10 按需节能）。
func (h *audioHub) AnySubscribers() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0
}

// HasRootSubscriber 是否存在 root 订阅（/audio-ws）。供捕获管理器决定是否预热「在播集合」：
// root 订阅时要抓所有正在播放的播放器，使切歌选中谁都已是热的、root 无缝接管（§3.1 预热消切换空窗）。
func (h *audioHub) HasRootSubscriber() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if sub.player == "" {
			return true
		}
	}
	return false
}

// ActivePlayer 返回当前 activePlayer 缓存。供捕获管理器判断 root 是否处于「无活跃」态，
// 据以向 root 持续推 player="" 的空闲空帧（让纯 /audio-ws 消费者无需订阅 /ws 也能知道已清空）。
func (h *audioHub) ActivePlayer() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.active
}

// BroadcastAudioFrame 把 payload 全程持 mu、drop-oldest 扇出给命中的订阅者：
// 该 player 的 per-player sub；以及若 player == active 缓存，root sub。
// 照 effect.go BroadcastEffectFrame 的「满则丢最旧一帧再写最新，两步非阻塞」。
func (h *audioHub) BroadcastAudioFrame(player string, payload []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		// 路由：per-player 命中该家，或 root(sub.player=="") 且该家正是 active 缓存。
		if !(sub.player == player || (sub.player == "" && player == h.active)) {
			continue
		}
		select {
		case sub.ch <- payload:
		default:
			// 通道已满：丢弃最旧一帧，再写入最新帧，两步都非阻塞，保证低延迟且永不回压。
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- payload:
			default:
			}
		}
	}
}

// addSub / removeSub 供 Task 9 的 handleAudioWS 登记/注销订阅者（在 mu 下操作 subs）。
// 通道的创建与关闭由 handler 负责（照 effect.go handleEffectWS：removeSub 后再 close(sub.ch)），
// 本 hub 只管 subs 集合本身。
func (h *audioHub) addSub(s *audioSub) {
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
}

func (h *audioHub) removeSub(s *audioSub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// AudioHub 供捕获管理器（Task 10/11）拿到 hub 以扇出帧、读订阅门控。
// 导出方法返回未导出类型合法（vet 干净）；manager 侧用接口接收它，不依赖具体类型。
func (s *Server) AudioHub() *audioHub {
	return s.audioHub
}

// audioRecover 是音频子系统 goroutine 的本地 panic 兜底（§1 主服务侧 panic 隔离的有意豁免）。
// 音频相关 goroutine 一律用它、**绝不挂 telemetry.Guard**——Guard 会 re-panic 杀主进程=直播事故，
// 而音频故障必须被隔离在音频子系统内、不拖垮歌词/特效主线。仅记日志、不 re-panic。
func audioRecover(label string) {
	if r := recover(); r != nil {
		serverLog.Warn("[audio] %s panic 已本地恢复: %v", label, r)
	}
}

// handleAudioWS 是 /audio-ws（playerName==""，root 跟随 activePlayer）与
// /<player>/audio-ws（playerName 固定）的连接处理器工厂，形态照 handleWS/handleEffectWS。
//
// 承重要点：
//   - 共用现有 s.upgrader（绝不改其 CheckOrigin：server.go 的恒真是全部 WS 端点共用的红线）。
//   - writer goroutine 退出（写失败，或 panic 被 audioRecover 恢复）必经 defer 关连接——否则读循环
//     永阻塞、sub 成僵尸（捕获管线继续为一个收不到帧的订阅者供帧）。写失败的理由同 handleWS/handleEffectWS。
//   - 清理顺序：removeSub（停止投递）→ close(sub.ch)（writer 退出）→ conn.Close() → <-done。
//     先摘订阅再关通道，避免 BroadcastAudioFrame 向已关通道发送。writer 先行关过连接时，此处再关只返回错误。
//   - writer goroutine 本地 recover（audioRecover），不挂 Guard。读循环在 handler goroutine 内、
//     由 net/http 自身 recover 兜底（不经 telemetry.Guard），且 conn.ReadMessage 只返回 err 不 panic。
func (s *Server) handleAudioWS(playerName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := s.upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverLog.Warn("音频WS Upgrade 失败: %v", err)
			return
		}

		sub := &audioSub{ch: make(chan []byte, 8), player: playerName}
		s.audioHub.addSub(sub)
		serverLog.Info("音频WS 连接: %s player=%q", conn.RemoteAddr(), playerName)

		// 写入 goroutine：从订阅通道读帧、以二进制消息发出。三条 defer 的执行顺序为 audioRecover →
		// conn.Close → close(done)：
		//   - audioRecover：本 goroutine 由本函数直接 spawn，未 recover 的 panic 会杀主进程，音频故障
		//     绝不允许拖垮主线（§1 豁免）。须被 defer 直接调用，包闭包则 recover 失效。
		//   - conn.Close：写失败或 panic 被恢复后都关连接，解阻塞读循环，交由下面既有清理路径注销订阅
		//     （理由见函数头）。
		//   - close(done)：最后执行，确保即便 panic 被恢复，下面的 <-done 也不会死锁。
		// 三条 defer 须连续写在函数体最前（goroutineguard 门禁只认前导 defer 段）。
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer conn.Close()
			defer audioRecover("audio-ws-writer")
			for payload := range sub.ch {
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
					serverLog.Warn("音频WS 写入失败，关闭连接: %s player=%q: %v", conn.RemoteAddr(), playerName, err)
					return
				}
			}
		}()

		// Read loop：保活 + 断开检测（overlay 只读不发；这里读到 err 即视为断开）。
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}

		// 清理：先摘订阅（停止投递）→ 关通道（writer range 退出）→ 关连接 → 等 writer 退出。
		s.audioHub.removeSub(sub)
		close(sub.ch)
		conn.Close()
		<-done
		serverLog.Info("音频WS 断开: %s player=%q", conn.RemoteAddr(), playerName)
	}
}
