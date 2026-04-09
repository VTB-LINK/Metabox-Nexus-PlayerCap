package server

import (
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
)

var routerLog = logger.New("Router")

// priorState 单个优先播放器的路由状态
type priorState struct {
	status    string    // 归一化状态："playing", "loading", "paused", "idle"
	activated bool      // 自上次 idle 以来是否进入过 playing/loading
	loadingAt time.Time // loading 开始时间（per-player 超时）
	pausedAt  time.Time // paused 开始时间（per-player activated 清除）
}

// normalizeStatus 将播放器上报的 status 归一化为路由决策的 4 种状态
func normalizeStatus(raw string) string {
	switch raw {
	case "playing":
		return "playing"
	case "loading":
		return "loading"
	case "paused":
		return "paused"
	default: // "standby", "waiting_process", "waiting_song", 以及未来任何未知值
		return "idle"
	}
}

// Router 优先级路由器，管理多个播放器的事件路由
type Router struct {
	cfg              *config.Config
	server           *Server
	players          []player.Player
	mu               sync.RWMutex
	activePlayer     string                 // 根路径当前输出的播放器
	lastPlaying      string                 // 普通播放器最后一个触发播放的
	priorStates      map[string]*priorState // 每个优先播放器的独立状态
	priorLastPlaying string                 // 优先组内最后播放的播放器
	groupPaused      bool                   // 优先组是否全员非活跃
	groupPauseAt     time.Time              // 组级非活跃起始时间

	// 切换后抑制重复事件：FullState 已推送的事件类型不再通过 NotifySubscribers 重复推给根订阅者
	switchSkipPlayer string
	switchSkipTypes  map[string]struct{}
}

// NewRouter 创建路由器
func NewRouter(cfg *config.Config, srv *Server) *Router {
	ps := make(map[string]*priorState, len(cfg.PriorPlayer))
	for _, name := range cfg.PriorPlayer {
		ps[name] = &priorState{status: "idle"}
	}
	return &Router{
		cfg:         cfg,
		server:      srv,
		priorStates: ps,
	}
}

// Register 注册播放器
func (r *Router) Register(p player.Player) {
	r.players = append(r.players, p)
}

// Run 启动事件分发循环（阻塞）
func (r *Router) Run() {
	merged := make(chan player.Event, 256)

	for _, p := range r.players {
		go func(p player.Player) {
			for evt := range p.Events() {
				merged <- evt
			}
		}(p)
	}

	go r.watchPriorExpire()

	for evt := range merged {
		// 1. 始终更新该播放器的状态缓存（供 per-player API 使用）
		r.server.UpdatePlayerState(evt)

		// 2. 更新路由逻辑（决定谁是 activePlayer，可能触发切换广播）
		switched := r.updateRouting(evt)

		// 3. 抑制切换后的重复事件（FullState 已推送过的类型）
		if !switched {
			r.mu.Lock()
			if r.switchSkipPlayer == evt.PlayerName {
				if _, ok := r.switchSkipTypes[evt.Type]; ok {
					delete(r.switchSkipTypes, evt.Type)
					switched = true
					if len(r.switchSkipTypes) == 0 {
						r.switchSkipPlayer = ""
					}
				}
			}
			r.mu.Unlock()
		}

		// 4. 通知所有匹配的订阅者
		//    - 根订阅者：仅推送 activePlayer 事件；switched=true 时跳过（FullState 已推送）
		//    - Per-player 订阅者：始终推送对应播放器事件
		r.server.NotifySubscribers(evt, switched)
	}
}

// switchTo 切换 activePlayer 并广播新播放器的完整缓存状态
// 调用前必须持有 r.mu 写锁
// 返回 true 表示发生了实际切换并已推送完整状态
func (r *Router) switchTo(newPlayer string) bool {
	old := r.activePlayer
	r.activePlayer = newPlayer
	// 同步到 server（供新 WS 连接使用）
	r.server.SetActivePlayer(newPlayer)
	if old != newPlayer && newPlayer != "" {
		routerLog.Info("播放器切换: [%s] → [%s]，推送完整状态", old, newPlayer)
		sentTypes := r.server.NotifySubscribersFullState(old, newPlayer)
		// 仅抑制 FullState 实际发送过的事件类型（缓存为空时不设抑制，避免吞首次数据）
		if len(sentTypes) > 0 {
			r.switchSkipPlayer = newPlayer
			r.switchSkipTypes = sentTypes
		} else {
			r.switchSkipPlayer = ""
			r.switchSkipTypes = nil
		}
		return true
	}
	return false
}

// updateRouting 根据事件更新 activePlayer
// 返回 true 表示发生了播放器切换（已推送完整状态，调用方应跳过本条事件的普通广播）
func (r *Router) updateRouting(evt player.Event) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	isPrior := r.cfg.IsPriorPlayer(evt.PlayerName)
	switched := false

	switch evt.Type {
	case player.EventStatusUpdate:
		if msg, ok := evt.Data.(*player.StatusInfo); ok {
			if isPrior {
				normalized := normalizeStatus(msg.Status)
				ps := r.priorStates[evt.PlayerName]

				switch normalized {
				case "playing":
					if ps.status != "playing" {
						routerLog.Info("优先播放器 [%s] → playing", evt.PlayerName)
					}
					ps.status = "playing"
					ps.activated = true
					r.priorLastPlaying = evt.PlayerName

				case "loading":
					if ps.status != "loading" {
						ps.loadingAt = time.Now()
						routerLog.Info("优先播放器 [%s] → loading", evt.PlayerName)
					}
					ps.status = "loading"
					ps.activated = true
					r.priorLastPlaying = evt.PlayerName

				case "paused":
					if ps.status != "paused" {
						ps.pausedAt = time.Now()
						routerLog.Info("优先播放器 [%s] → paused", evt.PlayerName)
					}
					ps.status = "paused"
					// activated 保持不变

				case "idle":
					if ps.status != "idle" {
						routerLog.Info("优先播放器 [%s] → idle (raw=%s)", evt.PlayerName, msg.Status)
					}
					ps.status = "idle"
					ps.activated = false
				}

				switched = r.evaluatePriorGroup()
			} else {
				if msg.Status == "playing" || msg.Status == "loading" {
					r.lastPlaying = evt.PlayerName
					if !r.priorGroupBlocking() {
						switched = r.switchTo(evt.PlayerName)
					}
				}
			}
		}

	default:
		if !isPrior && !r.priorGroupBlocking() && r.activePlayer == "" {
			switched = r.switchTo(evt.PlayerName)
			routerLog.Info("自动激活播放器 [%s]（无活跃播放器）", evt.PlayerName)
		}
	}

	return switched
}

// evaluatePriorGroup 评估优先组整体状态，决定 activePlayer 切换
// 调用前必须持有 r.mu 写锁
func (r *Router) evaluatePriorGroup() bool {
	// 1. 扫描组内状态
	var activeNames []string
	hasHolding := false

	for name, ps := range r.priorStates {
		switch ps.status {
		case "playing", "loading":
			activeNames = append(activeNames, name)
		case "paused":
			if ps.activated {
				hasHolding = true
			}
		}
	}

	// 2. 有 active 的优先播放器 → 切到组内 priorLastPlaying（若仍 active），否则取第一个
	if len(activeNames) > 0 {
		target := activeNames[0]
		for _, n := range activeNames {
			if n == r.priorLastPlaying {
				target = n
				break
			}
		}
		r.groupPaused = false
		return r.switchTo(target)
	}

	// 3. 无 active，有 holding → 启动组级倒计时（若尚未启动）
	if hasHolding {
		if !r.groupPaused {
			r.groupPaused = true
			r.groupPauseAt = time.Now()
			routerLog.Info("优先组全员非活跃，存在 holding，开始组级倒计时 %ds", r.cfg.PriorPlayerExpire)
		}
		return false
	}

	// 4. 全员 inert → 立即释放，切到普通播放器
	r.groupPaused = false
	fallback := r.lastPlaying
	if fallback == "" {
		fallback = r.findAnyNonPriorPlayer()
	}
	if fallback != "" && r.activePlayer != fallback {
		routerLog.Info("优先组全员 inert，切换到普通播放器 [%s]", fallback)
		return r.switchTo(fallback)
	}
	return false
}

// priorGroupBlocking 检查优先组是否阻挡普通播放器切入
// 有任何 active 或 holding 的优先播放器则返回 true
func (r *Router) priorGroupBlocking() bool {
	for _, ps := range r.priorStates {
		if ps.status == "playing" || ps.status == "loading" {
			return true
		}
		if ps.status == "paused" && ps.activated {
			return true
		}
	}
	return false
}

// findAnyNonPriorPlayer 查找任意非优先播放器
func (r *Router) findAnyNonPriorPlayer() string {
	for _, p := range r.players {
		if !r.cfg.IsPriorPlayer(p.Name()) {
			return p.Name()
		}
	}
	return ""
}

// watchPriorExpire 监控优先播放器超时（per-player + 组级）
func (r *Router) watchPriorExpire() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		r.mu.Lock()
		if r.cfg.PriorPlayerExpire > 0 {
			expire := time.Duration(r.cfg.PriorPlayerExpire) * time.Second
			changed := false

			// Per-player 超时检查
			for name, ps := range r.priorStates {
				// Loading 超时 → 强制 idle
				if ps.status == "loading" && time.Since(ps.loadingAt) >= expire {
					routerLog.Warn("优先播放器 [%s] loading 超时 (%ds)，标记为 idle", name, r.cfg.PriorPlayerExpire)
					ps.status = "idle"
					ps.activated = false
					changed = true
				}
				// Paused activated 超时 → 静默清除 activated（变 inert）
				if ps.status == "paused" && ps.activated && time.Since(ps.pausedAt) >= expire {
					routerLog.Info("优先播放器 [%s] 暂停超时 (%ds)，清除 activated", name, r.cfg.PriorPlayerExpire)
					ps.activated = false
					changed = true
				}
			}

			// Per-player 状态变化后重新评估组状态（可能全变 inert → 提前释放）
			if changed {
				r.evaluatePriorGroup()
			}

			// 组级超时
			if r.groupPaused && time.Since(r.groupPauseAt) >= expire {
				routerLog.Warn("优先组暂停超时 (%ds)", r.cfg.PriorPlayerExpire)
				// 强制清除所有 holding 的 activated，确保 priorGroupBlocking 不再阻挡
				for _, ps := range r.priorStates {
					if ps.status == "paused" && ps.activated {
						ps.activated = false
					}
				}
				r.groupPaused = false
				fallback := r.lastPlaying
				if fallback == "" {
					fallback = r.findAnyNonPriorPlayer()
				}
				if fallback != "" && r.activePlayer != fallback {
					routerLog.Info("切换到普通播放器 [%s]", fallback)
					r.switchTo(fallback)
				}
			}
		}
		r.mu.Unlock()
	}
}

// GetActivePlayer 获取当前活跃的播放器名
func (r *Router) GetActivePlayer() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.activePlayer
}
