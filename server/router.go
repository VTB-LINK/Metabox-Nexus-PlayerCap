package server

import (
	"sort"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/telemetry"
)

var routerLog = logger.New("Router")

// playerState 单个播放器的路由状态（prior 和 normal 组共用）
type playerState struct {
	status    string    // 归一化状态："playing", "loading", "paused", "idle"
	activated bool      // 自上次 idle 以来是否进入过 playing/loading
	loadingAt time.Time // loading 开始时间（per-player 超时）
	pausedAt  time.Time // paused 开始时间（per-player activated 清除）
	activeAt  time.Time // 本轮**进入**活跃态（playing/loading）的时刻，用于「最后播放者优先」
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

	// 切换后抑制重复事件：基于内容哈希比对，FullState 已推送的相同内容不再重复推给根订阅者
	switchSkipPlayer string
	switchSkipHashes map[string]uint64 // 事件类型 → 内容 FNV 哈希
	switchSkipAt     time.Time         // 切换发生时间，仅用于安全超时清理（10s）
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
			defer telemetry.Guard()
			for evt := range p.Events() {
				merged <- evt
			}
		}(p)
	}

	go r.watchExpire()

	for evt := range merged {
		r.processEvent(evt)
	}
}

// processEvent 执行单条播放器事件的完整路由管线（缓存更新 → 路由评估与活跃计数刷新 → idle_wake 上报 → 切歌去重 → 广播）。
func (r *Router) processEvent(evt player.Event) {
	// 1. 始终更新该播放器的状态缓存（供 per-player API 使用）；idle_wake 延迟到路由更新活跃组计数之后上报。
	wokeIdle := r.server.updatePlayerState(evt)

	// 2. 更新路由逻辑（决定谁是 activePlayer，可能触发切换广播，并刷新活跃组计数）
	switched := r.updateRouting(evt)
	if wokeIdle {
		r.server.emitIdleWake(evt.PlayerName)
	}

	// 3. 抑制切换后的重复事件（基于内容哈希比对，相同内容才抑制）
	if !switched {
		r.mu.Lock()
		if r.switchSkipPlayer == evt.PlayerName && r.switchSkipHashes != nil {
			if oldHash, ok := r.switchSkipHashes[evt.Type]; ok {
				newHash := hashEventData(evt.Type, evt.Data)
				delete(r.switchSkipHashes, evt.Type)
				if newHash == oldHash {
					routerLog.Detail("suppress duplicate event [%s] (player=%s, hash=%x)", evt.Type, evt.PlayerName, newHash)
					switched = true
				} else {
					routerLog.Detail("pass new-content event [%s] (player=%s, old=%x, new=%x)", evt.Type, evt.PlayerName, oldHash, newHash)
				}
				if len(r.switchSkipHashes) == 0 {
					r.switchSkipPlayer = ""
					r.switchSkipHashes = nil
				}
			}
		} else if r.switchSkipPlayer != "" && time.Since(r.switchSkipAt) >= 10*time.Second {
			// 安全超时清理（正常流程中哈希会被逐条消费，此处仅防残留）
			r.switchSkipPlayer = ""
			r.switchSkipHashes = nil
		}
		r.mu.Unlock()
	}

	// 4. 通知所有匹配的订阅者
	//    - 根订阅者：仅推送 activePlayer 事件；switched=true 时跳过（FullState 已推送）
	//    - Per-player 订阅者：始终推送对应播放器事件
	r.server.NotifySubscribers(evt, switched)
}

func countActivePlayers(states map[string]*playerState) int {
	n := 0
	for _, ps := range states {
		if ps.status == "playing" || ps.status == "loading" {
			n++
		}
	}
	return n
}

func (r *Router) syncActiveGroupCounts() (priorCnt, normalCnt int) {
	priorCnt = countActivePlayers(r.priorStates)
	normalCnt = countActivePlayers(r.normalStates)
	if r.server != nil {
		r.server.setActiveGroupCounts(priorCnt, normalCnt)
	}
	return priorCnt, normalCnt
}

func (r *Router) recordTransition(pending *[]routerTransitionEvent, kind, fromPlayer, toPlayer, reason string) int {
	priorCnt, normalCnt := r.syncActiveGroupCounts()
	ev := routerTransitionEvent{
		kind:              kind,
		fromPlayer:        fromPlayer,
		toPlayer:          toPlayer,
		reason:            reason,
		activePriorCount:  priorCnt,
		activeNormalCount: normalCnt,
	}
	if pending != nil {
		*pending = append(*pending, ev)
		return len(*pending) - 1
	}
	emitRouterTransition(ev)
	return -1
}

// switchTo 切换 activePlayer 并广播新播放器的完整缓存状态
// 调用前必须持有 r.mu 写锁
// 返回 true 表示发生了实际切换并已推送完整状态
func (r *Router) switchTo(newPlayer string) bool {
	return r.switchToWithReason(newPlayer, "", nil)
}

func (r *Router) switchToWithReason(newPlayer, reason string, pending *[]routerTransitionEvent) bool {
	old := r.activePlayer
	r.activePlayer = newPlayer
	// 同步到 server（供新 WS 连接使用）
	r.server.SetActivePlayer(newPlayer)
	if old != newPlayer && newPlayer != "" {
		if reason == "" {
			newIsPrior := r.cfg.IsPriorPlayer(newPlayer)
			oldIsPrior := old != "" && r.cfg.IsPriorPlayer(old)
			switch {
			case newIsPrior && old != "" && !oldIsPrior:
				reason = "prior_preempt"
			case newIsPrior:
				reason = "prior_switch"
			case oldIsPrior:
				reason = "normal_fallback"
			default:
				reason = "normal_switch"
			}
		}
		r.server.addRouteSwitch()
		r.recordTransition(pending, "switch", old, newPlayer, reason)
		routerLog.Info("player switch: [%s] → [%s], pushing full state", old, newPlayer)
		sentHashes := r.server.NotifySubscribersFullState(old, newPlayer)
		// 不抑制 status_update：状态变更是关键事件，loading→playing 极快时会被错误吞掉
		delete(sentHashes, player.EventStatusUpdate)
		// 仅抑制 FullState 实际发送过的事件类型（缓存为空时不设抑制，避免吞首次数据）
		if len(sentHashes) > 0 {
			r.switchSkipPlayer = newPlayer
			r.switchSkipHashes = sentHashes
			r.switchSkipAt = time.Now()
		} else {
			r.switchSkipPlayer = ""
			r.switchSkipHashes = nil
		}
		return true
	}
	return false
}

// updatePlayerState 更新播放器组状态（prior 或 normal 通用）
// 调用前必须持有 r.mu 写锁
func (r *Router) updatePlayerState(ps *playerState, normalized, playerName, groupLabel string, lastPlaying *string) {
	// 上一状态是否已经算「活跃」（= 已在 evaluateGroup 的 activeNames 里）。
	// activeAt 只在**从非活跃进入活跃**那一刻打，loading→playing 属同一轮活跃内的转换，
	// 不重打 —— 否则它就退化成「谁最后发的事件」而不是「谁最近开始播放」。
	wasActive := ps.status == "playing" || ps.status == "loading"

	switch normalized {
	case "playing":
		if ps.status != "playing" {
			routerLog.Info("%[1]s player [%[2]s] → playing", groupLabel, playerName)
		}
		if !wasActive {
			ps.activeAt = time.Now()
		}
		ps.status = "playing"
		ps.activated = true
		*lastPlaying = playerName

	case "loading":
		if ps.status != "loading" {
			ps.loadingAt = time.Now()
			routerLog.Info("%[1]s player [%[2]s] → loading", groupLabel, playerName)
		}
		if !wasActive {
			ps.activeAt = time.Now()
		}
		ps.status = "loading"
		ps.activated = true
		*lastPlaying = playerName

	case "paused":
		if ps.status != "paused" {
			ps.pausedAt = time.Now()
			routerLog.Info("%[1]s player [%[2]s] → paused", groupLabel, playerName)
		}
		ps.status = "paused"
		// activated 保持不变

	case "idle":
		if ps.status != "idle" {
			routerLog.Info("%[1]s player [%[2]s] → idle", groupLabel, playerName)
		}
		ps.status = "idle"
		ps.activated = false
	}
}

// updateRouting 根据事件更新 activePlayer
// 返回 true 表示发生了播放器切换（已推送完整状态，调用方应跳过本条事件的普通广播）
func (r *Router) updateRouting(evt player.Event) bool {
	// 位置采样不参与路由评估：它每隔几秒到达一次，只校准缓存里的位置，不代表播放器状态有变。
	// 放它进来，下面 default 分支会在「无活跃播放器」时拿它重评估普通组。
	//
	// 按事件类型本身判断，不查 server 的 internalEvents 表：另一种内部事件 clear_song_data 至今
	// 照常进入 default 分支，这里不改它。它在该播放器刚报过非活跃状态之后发出，推断那次重评估的
	// 输入与上一次相同、是空操作，但没有穷举各状态组合，不为两者待遇一致而动一条承重路径。
	if evt.Type == player.EventPositionSample {
		return false
	}

	r.mu.Lock()

	isPrior := r.cfg.IsPriorPlayer(evt.PlayerName)
	switched := false
	var pending []routerTransitionEvent

	switch evt.Type {
	case player.EventStatusUpdate:
		if msg, ok := evt.Data.(*player.StatusInfo); ok {
			normalized := normalizeStatus(msg.Status)
			if isPrior {
				ps := r.priorStates[evt.PlayerName]
				r.updatePlayerState(ps, normalized, evt.PlayerName, i18n.T("prior"), &r.priorLastPlaying)
				switched = r.evaluatePriorGroupWith("true_stop", &pending)
			} else {
				ns := r.normalStates[evt.PlayerName]
				if ns != nil {
					r.updatePlayerState(ns, normalized, evt.PlayerName, i18n.T("normal"), &r.normalLastPlaying)
				}
				if !r.priorGroupBlocking() {
					switched = r.evaluateNormalGroupWith(false, "true_stop", &pending)
				}
			}
			r.syncActiveGroupCounts()
		}

	default:
		if !isPrior && !r.priorGroupBlocking() && r.activePlayer == "" {
			switched = r.evaluateNormalGroupWith(false, "true_stop", &pending)
		}
	}

	r.mu.Unlock()
	emitRouterTransitions(pending)
	return switched
}

// evaluateGroup 评估播放器组整体状态（prior/normal 通用）
// 返回 activeNames（**按 activeAt 降序：最近开始播放的在前**）, hasHolding
//
// 排序是承重的，别去掉：两个调用方都写 `target := activeNames[0]` 再让 lastPlaying
// 循环去覆盖它。lastPlaying 失配时（它已不在 activeNames 里 —— 最后播放的那个进程退出了，
// 而另外两个还在放）activeNames[0] 就是最终答案，而 `range map` 的迭代序是**随机**的
// —— 同一份状态每次评估可能给出不同的 target。放大器：进程不在的播放器每 ~2s 重发一次
// waiting_process（qqmusic.go:49 / kugou.go:190 在外层 for 体内），每条 status_update 都会
// 重新评估 → 硬币每 2s 重掷一次 → 结果一变就 switchTo → NotifySubscribersFullState
// 全量重放 → **OBS 根 overlay 硬切**。实测重掷结果改变的概率 0.37，期望硬切间隔 ~5.4s。
//
// 判据用 activeAt（最近开始播放者优先）而不是「配置顺序」：
//   - 这是作者本来就声明的语义 —— 796d69c 的 body 原话「切到 priorLastPlaying（焦点竞争，
//     **最后播放者优先**）」，README 与 config 模板也都写「切换到最后一个普通播放器」。
//     lastPlaying 就是这个判据的标量版，activeAt 是它的自然全序推广 —— 于是
//     「lastPlaying 永不清除会变陈旧」这个失效前提从根上消失：陈旧者不在 activeNames 里，
//     压根不参与比较。
//   - 「配置顺序」则是另一条相竞争的新判据，且 normal 组的那个顺序硬编码在 main.go，
//     用户根本改不了；prior 组的顺序是 config 模板「取消注释」的产物，不是用户的表达。
//
// 遍历本身保持 `range map` 不动：改成「按 order 遍历」会让 order 里漏掉的名字整个从
// activeNames 消失 —— 漏掉唯一活跃者即 len==0 → clearActivePlayer → OBS 空屏，比它要修的
// 问题严重一级。排序只重排已经收集到的名字，不可能丢成员。
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
	sort.SliceStable(activeNames, func(i, j int) bool {
		ai, aj := states[activeNames[i]].activeAt, states[activeNames[j]].activeAt
		if !ai.Equal(aj) {
			return ai.After(aj)
		}
		// 同刻进入活跃（几乎只可能出现在测试里）：按名字定序，保证结果仍是确定的
		return activeNames[i] < activeNames[j]
	})
	return
}

// evaluatePriorGroup 评估优先组整体状态，决定 activePlayer 切换
// 调用前必须持有 r.mu 写锁
func (r *Router) evaluatePriorGroup() bool {
	return r.evaluatePriorGroupWith("true_stop", nil)
}

func (r *Router) evaluatePriorGroupWith(clearReason string, pending *[]routerTransitionEvent) bool {
	activeNames, hasHolding := evaluateGroup(r.priorStates)

	// 1. 有 active 的优先播放器 → 切到组内 priorLastPlaying（若仍 active），否则取最近开始播放的那个
	//    （evaluateGroup 已按 activeAt 降序，故 activeNames[0] 即最近活跃者）
	if len(activeNames) > 0 {
		target := activeNames[0]
		for _, n := range activeNames {
			if n == r.priorLastPlaying {
				target = n
				break
			}
		}
		r.priorGroupPaused = false
		reason := "prior_switch"
		if r.activePlayer != "" && !r.cfg.IsPriorPlayer(r.activePlayer) {
			reason = "prior_preempt"
		}
		return r.switchToWithReason(target, reason, pending)
	}

	// 2. 无 active，有 holding → 启动组级倒计时（若尚未启动）
	if hasHolding {
		if !r.priorGroupPaused {
			r.priorGroupPaused = true
			r.priorGroupPauseAt = time.Now()
			routerLog.Info("prior group all inactive, holding present, starting group-level countdown %ds", r.cfg.PriorPlayerExpire)
			r.recordTransition(pending, "enter_holding", r.activePlayer, r.activePlayer, "prior_holding")
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
	routerLog.Info("prior group all inert, releasing to normal group")
	return r.evaluateNormalGroupWith(true, clearReason, pending)
}

// evaluateNormalGroup 评估普通组整体状态，决定 activePlayer 切换
// 调用前必须持有 r.mu 写锁
func (r *Router) evaluateNormalGroup() bool {
	return r.evaluateNormalGroupWith(false, "true_stop", nil)
}

func (r *Router) evaluateNormalGroupWith(fromPriorRelease bool, clearReason string, pending *[]routerTransitionEvent) bool {
	activeNames, hasHolding := evaluateGroup(r.normalStates)

	// 1. 有 active 的普通播放器 → 切到组内 normalLastPlaying（若仍 active），否则取最近开始播放的那个
	//    （evaluateGroup 已按 activeAt 降序，故 activeNames[0] 即最近活跃者）
	if len(activeNames) > 0 {
		target := activeNames[0]
		for _, n := range activeNames {
			if n == r.normalLastPlaying {
				target = n
				break
			}
		}
		r.normalGroupPaused = false
		reason := "normal_switch"
		if fromPriorRelease || (r.activePlayer != "" && r.cfg.IsPriorPlayer(r.activePlayer)) {
			reason = "normal_fallback"
		}
		return r.switchToWithReason(target, reason, pending)
	}

	// 2. 无 active，有 holding → 启动组级倒计时（若尚未启动）
	if hasHolding {
		if !r.normalGroupPaused {
			r.normalGroupPaused = true
			r.normalGroupPauseAt = time.Now()
			routerLog.Info("normal group all inactive, holding present, starting group-level countdown %ds", r.cfg.PriorPlayerExpire)
			r.recordTransition(pending, "enter_holding", r.activePlayer, r.activePlayer, "normal_holding")
		}
		return false
	}

	// 3. 全员 inert → 清除活跃播放器
	r.normalGroupPaused = false
	return r.clearActivePlayerWithReason(clearReason, pending)
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
	return r.clearActivePlayerWithReason("true_stop", nil)
}

func (r *Router) clearActivePlayerWithReason(reason string, pending *[]routerTransitionEvent) bool {
	old := r.activePlayer
	if old == "" {
		return false
	}
	r.activePlayer = ""
	r.server.SetActivePlayer("")
	r.recordTransition(pending, "clear_all", old, "", reason)
	routerLog.Info("clear active player: [%s] → (none)", old)
	r.server.NotifySubscribersClear(old)
	// 清除切换抑制（不再有 activePlayer）
	r.switchSkipPlayer = ""
	r.switchSkipHashes = nil
	return true
}

type expiredPlayer struct {
	name   string
	reason string
}

// expireGroupPlayersAt 检查播放器组的 per-player 超时，返回触发超时的播放器与原因。
func expireGroupPlayersAt(states map[string]*playerState, now time.Time, expire time.Duration, expireSec int, groupLabel string) []expiredPlayer {
	var expired []expiredPlayer
	for name, ps := range states {
		// Loading 超时 → 强制 idle
		if ps.status == "loading" && now.Sub(ps.loadingAt) >= expire {
			routerLog.Warn("%[1]s player [%[2]s] loading timeout (%[3]ds), marking idle", groupLabel, name, expireSec)
			ps.status = "idle"
			ps.activated = false
			expired = append(expired, expiredPlayer{name: name, reason: "loading_timeout"})
		}
		// Paused activated 超时 → 静默清除 activated（变 inert）
		if ps.status == "paused" && ps.activated && now.Sub(ps.pausedAt) >= expire {
			routerLog.Info("%[1]s player [%[2]s] paused timeout (%[3]ds), clearing activated", groupLabel, name, expireSec)
			ps.activated = false
			expired = append(expired, expiredPlayer{name: name, reason: "paused_timeout"})
		}
	}
	return expired
}

// forceGroupInert 强制清除组内所有 holding 的 activated
func forceGroupInert(states map[string]*playerState) {
	for _, ps := range states {
		if ps.status == "paused" && ps.activated {
			ps.activated = false
		}
	}
}

// sweepExpire 执行一次超时巡检并在释放 r.mu 后上报产生的路由跃迁 RUM 事件。
func (r *Router) sweepExpire(now time.Time) {
	var pending []routerTransitionEvent

	r.mu.Lock()
	if r.cfg.PriorPlayerExpire > 0 {
		expire := time.Duration(r.cfg.PriorPlayerExpire) * time.Second

		// === 优先组 per-player 超时 ===
		if expired := expireGroupPlayersAt(r.priorStates, now, expire, r.cfg.PriorPlayerExpire, i18n.T("prior")); len(expired) > 0 {
			idxs := make([]int, 0, len(expired))
			for _, ex := range expired {
				idxs = append(idxs, r.recordTransition(&pending, "expire_timeout", ex.name, "", ex.reason))
			}
			r.evaluatePriorGroupWith(expired[len(expired)-1].reason, &pending)
			for _, idx := range idxs {
				pending[idx].toPlayer = r.activePlayer
			}
		}

		// 优先组组级超时
		if r.priorGroupPaused && now.Sub(r.priorGroupPauseAt) >= expire {
			routerLog.Warn("prior group paused timeout (%ds)", r.cfg.PriorPlayerExpire)
			oldActive := r.activePlayer
			forceGroupInert(r.priorStates)
			r.priorGroupPaused = false
			// 强制 normal 组 holding 为 inert（与 evaluatePriorGroup 全员 inert 路径一致）
			forceGroupInert(r.normalStates)
			r.normalGroupPaused = false
			idx := r.recordTransition(&pending, "expire_timeout", oldActive, "", "group_paused_timeout")
			r.evaluateNormalGroupWith(true, "group_paused_timeout", &pending)
			pending[idx].toPlayer = r.activePlayer
		}

		// === 普通组 per-player 超时（仅在 prior 不阻挡时评估切换）===
		if expired := expireGroupPlayersAt(r.normalStates, now, expire, r.cfg.PriorPlayerExpire, i18n.T("normal")); len(expired) > 0 {
			idxs := make([]int, 0, len(expired))
			for _, ex := range expired {
				idxs = append(idxs, r.recordTransition(&pending, "expire_timeout", ex.name, "", ex.reason))
			}
			if !r.priorGroupBlocking() {
				r.evaluateNormalGroupWith(false, expired[len(expired)-1].reason, &pending)
			}
			for _, idx := range idxs {
				pending[idx].toPlayer = r.activePlayer
			}
		}

		// 普通组组级超时（仅在 prior 不阻挡时生效）
		if r.normalGroupPaused && now.Sub(r.normalGroupPauseAt) >= expire {
			if !r.priorGroupBlocking() {
				routerLog.Warn("normal group paused timeout (%ds)", r.cfg.PriorPlayerExpire)
				oldActive := r.activePlayer
				forceGroupInert(r.normalStates)
				r.normalGroupPaused = false
				r.recordTransition(&pending, "expire_timeout", oldActive, "", "group_paused_timeout")
				r.clearActivePlayerWithReason("group_paused_timeout", &pending)
			}
		}
		r.syncActiveGroupCounts()
	}
	r.mu.Unlock()

	emitRouterTransitions(pending)
}

// watchExpire 监控所有播放器超时（prior + normal 的 per-player + 组级）
func (r *Router) watchExpire() {
	defer telemetry.Guard()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		r.sweepExpire(time.Now())
	}
}

// GetActivePlayer 获取当前活跃的播放器名
func (r *Router) GetActivePlayer() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.activePlayer
}
