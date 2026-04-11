package server

import (
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
)

var routerLog = logger.New("Router")

// playerState 单个播放器的路由状态（prior 和 normal 组共用）
type playerState struct {
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
	cfg     *config.Config
	server  *Server
	players []player.Player
	mu      sync.RWMutex

	activePlayer string // 根路径当前输出的播放器

	// 优先组
	priorStates       map[string]*playerState // 每个优先播放器的独立状态
	priorLastPlaying  string                  // 优先组内最后播放的播放器
	priorGroupPaused  bool                    // 优先组是否全员非活跃
	priorGroupPauseAt time.Time               // 优先组级非活跃起始时间

	// 普通组
	normalStates       map[string]*playerState // 每个普通播放器的独立状态
	normalLastPlaying  string                  // 普通组内最后播放的播放器
	normalGroupPaused  bool                    // 普通组是否全员非活跃
	normalGroupPauseAt time.Time               // 普通组级非活跃起始时间

	// 切换后抑制重复事件：FullState 已推送的事件类型不再通过 NotifySubscribers 重复推给根订阅者
	switchSkipPlayer string
	switchSkipTypes  map[string]struct{}
}

// NewRouter 创建路由器
func NewRouter(cfg *config.Config, srv *Server, playerNames []string) *Router {
	ps := make(map[string]*playerState, len(cfg.PriorPlayer))
	for _, name := range cfg.PriorPlayer {
		ps[name] = &playerState{status: "idle"}
	}
	ns := make(map[string]*playerState)
	for _, name := range playerNames {
		if !cfg.IsPriorPlayer(name) {
			ns[name] = &playerState{status: "idle"}
		}
	}
	return &Router{
		cfg:          cfg,
		server:       srv,
		priorStates:  ps,
		normalStates: ns,
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

	go r.watchExpire()

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
		// 不抑制 status_update：状态变更是关键事件，loading→playing 极快时会被错误吞掉
		delete(sentTypes, player.EventStatusUpdate)
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

// updatePlayerState 更新播放器组状态（prior 或 normal 通用）
// 调用前必须持有 r.mu 写锁
func (r *Router) updatePlayerState(ps *playerState, normalized, playerName, groupLabel string, lastPlaying *string) {
	switch normalized {
	case "playing":
		if ps.status != "playing" {
			routerLog.Info("%s播放器 [%s] → playing", groupLabel, playerName)
		}
		ps.status = "playing"
		ps.activated = true
		*lastPlaying = playerName

	case "loading":
		if ps.status != "loading" {
			ps.loadingAt = time.Now()
			routerLog.Info("%s播放器 [%s] → loading", groupLabel, playerName)
		}
		ps.status = "loading"
		ps.activated = true
		*lastPlaying = playerName

	case "paused":
		if ps.status != "paused" {
			ps.pausedAt = time.Now()
			routerLog.Info("%s播放器 [%s] → paused", groupLabel, playerName)
		}
		ps.status = "paused"
		// activated 保持不变

	case "idle":
		if ps.status != "idle" {
			routerLog.Info("%s播放器 [%s] → idle", groupLabel, playerName)
		}
		ps.status = "idle"
		ps.activated = false
	}
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
			normalized := normalizeStatus(msg.Status)
			if isPrior {
				ps := r.priorStates[evt.PlayerName]
				r.updatePlayerState(ps, normalized, evt.PlayerName, "优先", &r.priorLastPlaying)
				switched = r.evaluatePriorGroup()
			} else {
				ns := r.normalStates[evt.PlayerName]
				if ns != nil {
					r.updatePlayerState(ns, normalized, evt.PlayerName, "普通", &r.normalLastPlaying)
				}
				if !r.priorGroupBlocking() {
					switched = r.evaluateNormalGroup()
				}
			}
		}

	default:
		if !isPrior && !r.priorGroupBlocking() && r.activePlayer == "" {
			switched = r.evaluateNormalGroup()
		}
	}

	return switched
}

// evaluateGroup 评估播放器组整体状态（prior/normal 通用）
// 返回 activeNames, hasHolding
func evaluateGroup(states map[string]*playerState) (activeNames []string, hasHolding bool) {
	for name, ps := range states {
		switch ps.status {
		case "playing", "loading":
			activeNames = append(activeNames, name)
		case "paused":
			if ps.activated {
				hasHolding = true
			}
		}
	}
	return
}

// evaluatePriorGroup 评估优先组整体状态，决定 activePlayer 切换
// 调用前必须持有 r.mu 写锁
func (r *Router) evaluatePriorGroup() bool {
	activeNames, hasHolding := evaluateGroup(r.priorStates)

	// 1. 有 active 的优先播放器 → 切到组内 priorLastPlaying（若仍 active），否则取第一个
	if len(activeNames) > 0 {
		target := activeNames[0]
		for _, n := range activeNames {
			if n == r.priorLastPlaying {
				target = n
				break
			}
		}
		r.priorGroupPaused = false
		return r.switchTo(target)
	}

	// 2. 无 active，有 holding → 启动组级倒计时（若尚未启动）
	if hasHolding {
		if !r.priorGroupPaused {
			r.priorGroupPaused = true
			r.priorGroupPauseAt = time.Now()
			routerLog.Info("优先组全员非活跃，存在 holding，开始组级倒计时 %ds", r.cfg.PriorPlayerExpire)
		}
		return false
	}

	// 3. 全员 inert → 立即释放，切到普通组
	r.priorGroupPaused = false
	// 强制所有 normal 组 holding 为 inert（prior 播放期间暂停的 normal 播放器不应被切入）
	for _, ns := range r.normalStates {
		if ns.status == "paused" && ns.activated {
			ns.activated = false
		}
	}
	r.normalGroupPaused = false
	routerLog.Info("优先组全员 inert，释放到普通组")
	return r.evaluateNormalGroup()
}

// evaluateNormalGroup 评估普通组整体状态，决定 activePlayer 切换
// 调用前必须持有 r.mu 写锁
func (r *Router) evaluateNormalGroup() bool {
	activeNames, hasHolding := evaluateGroup(r.normalStates)

	// 1. 有 active 的普通播放器 → 切到组内 normalLastPlaying（若仍 active），否则取第一个
	if len(activeNames) > 0 {
		target := activeNames[0]
		for _, n := range activeNames {
			if n == r.normalLastPlaying {
				target = n
				break
			}
		}
		r.normalGroupPaused = false
		return r.switchTo(target)
	}

	// 2. 无 active，有 holding → 启动组级倒计时（若尚未启动）
	if hasHolding {
		if !r.normalGroupPaused {
			r.normalGroupPaused = true
			r.normalGroupPauseAt = time.Now()
			routerLog.Info("普通组全员非活跃，存在 holding，开始组级倒计时 %ds", r.cfg.PriorPlayerExpire)
		}
		return false
	}

	// 3. 全员 inert → 清除活跃播放器
	r.normalGroupPaused = false
	return r.clearActivePlayer()
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

// clearActivePlayer 清除活跃播放器并通知前端
// 调用前必须持有 r.mu 写锁
func (r *Router) clearActivePlayer() bool {
	old := r.activePlayer
	if old == "" {
		return false
	}
	r.activePlayer = ""
	r.server.SetActivePlayer("")
	routerLog.Info("清除活跃播放器: [%s] → (空)", old)
	r.server.NotifySubscribersClear(old)
	// 清除切换抑制（不再有 activePlayer）
	r.switchSkipPlayer = ""
	r.switchSkipTypes = nil
	return true
}

// expireGroupPlayers 检查播放器组的 per-player 超时
// 返回 true 表示有状态变化
func expireGroupPlayers(states map[string]*playerState, expire time.Duration, expireSec int, groupLabel string) bool {
	changed := false
	for name, ps := range states {
		// Loading 超时 → 强制 idle
		if ps.status == "loading" && time.Since(ps.loadingAt) >= expire {
			routerLog.Warn("%s播放器 [%s] loading 超时 (%ds)，标记为 idle", groupLabel, name, expireSec)
			ps.status = "idle"
			ps.activated = false
			changed = true
		}
		// Paused activated 超时 → 静默清除 activated（变 inert）
		if ps.status == "paused" && ps.activated && time.Since(ps.pausedAt) >= expire {
			routerLog.Info("%s播放器 [%s] 暂停超时 (%ds)，清除 activated", groupLabel, name, expireSec)
			ps.activated = false
			changed = true
		}
	}
	return changed
}

// forceGroupInert 强制清除组内所有 holding 的 activated
func forceGroupInert(states map[string]*playerState) {
	for _, ps := range states {
		if ps.status == "paused" && ps.activated {
			ps.activated = false
		}
	}
}

// watchExpire 监控所有播放器超时（prior + normal 的 per-player + 组级）
func (r *Router) watchExpire() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		r.mu.Lock()
		if r.cfg.PriorPlayerExpire > 0 {
			expire := time.Duration(r.cfg.PriorPlayerExpire) * time.Second

			// === 优先组 per-player 超时 ===
			if expireGroupPlayers(r.priorStates, expire, r.cfg.PriorPlayerExpire, "优先") {
				r.evaluatePriorGroup()
			}

			// 优先组组级超时
			if r.priorGroupPaused && time.Since(r.priorGroupPauseAt) >= expire {
				routerLog.Warn("优先组暂停超时 (%ds)", r.cfg.PriorPlayerExpire)
				forceGroupInert(r.priorStates)
				r.priorGroupPaused = false
				// 强制 normal 组 holding 为 inert（与 evaluatePriorGroup 全员 inert 路径一致）
				forceGroupInert(r.normalStates)
				r.normalGroupPaused = false
				r.evaluateNormalGroup()
			}

			// === 普通组 per-player 超时（仅在 prior 不阻挡时评估切换）===
			if expireGroupPlayers(r.normalStates, expire, r.cfg.PriorPlayerExpire, "普通") {
				if !r.priorGroupBlocking() {
					r.evaluateNormalGroup()
				}
			}

			// 普通组组级超时（仅在 prior 不阻挡时生效）
			if r.normalGroupPaused && time.Since(r.normalGroupPauseAt) >= expire {
				if !r.priorGroupBlocking() {
					routerLog.Warn("普通组暂停超时 (%ds)", r.cfg.PriorPlayerExpire)
					forceGroupInert(r.normalStates)
					r.normalGroupPaused = false
					r.clearActivePlayer()
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
