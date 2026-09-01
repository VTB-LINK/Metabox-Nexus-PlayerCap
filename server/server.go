package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"Metabox-Nexus-PlayerCap/logger"
	"Metabox-Nexus-PlayerCap/player"

	"github.com/gorilla/websocket"
)

var serverLog = logger.New("Server")

// --- 订阅器 ---

// subscriber 表示一个 WS 或 SSE 订阅者
type subscriber struct {
	player     string   // "" = root（跟随 activePlayer），"wesing" = 只收该 player
	eventTypes []string // nil/empty = 全部事件，否则按类型过滤
	ch         chan WSEvent
	isWS       bool   // true=WS, false=SSE（用于统计）
	addr       string // 远程地址（用于 service-status）
}

func (sub *subscriber) matchesType(evtType string) bool {
	if len(sub.eventTypes) == 0 {
		return true
	}
	for _, t := range sub.eventTypes {
		if t == evtType {
			return true
		}
	}
	return false
}

// --- 声明式路由 ---

type routeDef struct {
	suffix   string
	kind     string                        // "http", "ws", "sse"
	httpH    func(string) http.HandlerFunc // http 用
	sseTypes []string                      // sse 用：订阅哪些事件类型
}

// PlayerState 每个播放器在服务端的缓存状态
type PlayerState struct {
	Status      *StatusMessage
	SongInfo    *SongInfoUpdate
	LyricUpdate *LyricUpdate
	AllLyrics   []LyricItem
	Title       string
	Duration    float32
	Position    float32 // 整曲实时播放位置（秒）；喂 all_lyrics 的 position
	Progress    float32

	// per-player 通道无活跃自动隐藏（issue #47）。IdleHidden 是叠加在缓存之上的「视图隐藏」标志，
	// 只影响 per-player 传输（/{player}/*），绝不影响根路径（§3.4）；缓存本身照常保持新鲜（§2.7）。
	// 播放器一恢复，UpdatePlayerState 即解除隐藏：HTTP/重连立刻取回缓存；已在线的 WS/SSE 则由恢复后的
	// 实时事件自然复原（下一条 lyric_update 到达前可能仍空白）。
	LastEventAt time.Time // 最近一次非内部事件的时刻，供无活跃巡检计时
	IdleHidden  bool      // 该 per-player 通道当前是否处于无活跃隐藏态
}

// ServiceInfo 服务配置信息
type ServiceInfo struct {
	Version           string
	Addr              string
	Sources           []string
	Endpoints         *OrderedMap
	PlayerSupport     []string
	Config            *OrderedMap
	ConfigOverwritten []string
}

// Server 统一 HTTP+WS+SSE 服务器
type Server struct {
	upgrader    websocket.Upgrader
	serviceInfo *ServiceInfo
	mu          sync.RWMutex

	// 每个播放器的独立状态缓存
	playerStates map[string]*PlayerState

	// 当前活跃播放器（由 Router 设置）
	activePlayer string

	// 统一订阅器系统（替代旧的 clients/broadcastCh/SSE channels）
	subscribers map[*subscriber]struct{}
	subMu       sync.Mutex

	// 网易云特效镜像帧分发（独立于 JSON 事件通道）
	effectHub *effectHub

	// per-player 通道无活跃自动隐藏阈值解析器（秒，0=关），由 main 注入 cfg.GetPlayerIdleHide。
	// 受 s.mu 保护；nil=未注入=全关（本地/测试零副作用）。
	idleHideResolver func(playerName string) int
}

// NewServer 创建统一服务器
func NewServer(playerNames []string) *Server {
	states := make(map[string]*PlayerState)
	for _, name := range playerNames {
		states[name] = &PlayerState{}
	}

	return &Server{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		playerStates: states,
		subscribers:  make(map[*subscriber]struct{}),
		serviceInfo:  &ServiceInfo{},
		effectHub:    newEffectHub(),
	}
}

// --- 订阅器管理 ---

func (s *Server) subscribe(playerName string, eventTypes []string, isWS bool, addr string) *subscriber {
	sub := &subscriber{
		player:     playerName,
		eventTypes: eventTypes,
		ch:         make(chan WSEvent, 64),
		isWS:       isWS,
		addr:       addr,
	}
	s.subMu.Lock()
	s.subscribers[sub] = struct{}{}
	s.subMu.Unlock()
	return sub
}

func (s *Server) unsubscribe(sub *subscriber) {
	s.subMu.Lock()
	delete(s.subscribers, sub)
	s.subMu.Unlock()
}

// internalEvents 只驱动服务端内部状态、不属于对外 API 的事件类型。
//
// clear_song_data 的唯一职责是清空 playerState 缓存（UpdatePlayerState 的
// EventClearSongData 分支），好让后续连入的客户端不会从 buildInitEvents 拿到上一首的残留。
// 它从不是给客户端看的——载荷恒为 nil（而非本项目「空数据一律 {}」的约定）、两份对外文档
// 零记载、lyric_page.html 不处理、接口项目里也没有它。
//
// 它此前会被推给 per-player 订阅者，是 router.Run 对每个事件无差别调用本函数的副作用；
// 根订阅者则恰好被 evt.PlayerName != active 挡掉——因为它只在「本播放器已不活跃」时发出，
// 那正是根过滤成立的条件。即根订阅者从来收不到它，纯属巧合而非设计。
//
// 溯源：65d9e80 把 WesingCap 时代的 Server.ClearSongData() **方法**（清缓存 + 广播空数据）
// 重构成**事件**，职责未变但从此要过 NotifySubscribers——内部信号于是泄漏到了公开 API 表面。
// 同期的文档原话「服务端不会主动发送清空歌词的消息」在重构前为真，之后成假且无人订正。
var internalEvents = map[string]bool{
	player.EventClearSongData: true,
}

// NotifySubscribers 向匹配的订阅者推送事件
// skipRoot=true 时跳过根订阅者（播放器切换后避免与 FullState 重复）
func (s *Server) NotifySubscribers(evt player.Event, skipRoot bool) {
	// 内部事件不外发。调用方（router.Run）在此之前已调过 UpdatePlayerState，
	// 缓存清空照常发生——这里拦掉的只是「广播」这一步。
	if internalEvents[evt.Type] {
		return
	}

	wsEvt := WSEvent{Type: evt.Type, Player: evt.PlayerName, Data: evt.Data}

	// 「空数据一律 {}，绝不 null」（openapi.yaml:10 的全局约定）在**唯一出口**执行。
	//
	// nil 的 interface{} 会被 encoding/json 编码成 null。指望每个 Emit 调用点各自记得
	// 传 struct{}{} 是靠不住的——lyric_idle 就是这么破的（wesing 两处传 nil，实测发出
	// data:null，而文档三处声称「始终为 {}」），而且**没有任何症状**：前端只看 type，
	// 没人发现。这类分叉只能在出口收口。
	//
	// 播放器侧同样传 struct{}{}（wesing.go:146/:263），双保险：源头对了，这里也兜着。
	if wsEvt.Data == nil {
		wsEvt.Data = struct{}{}
	}

	s.mu.RLock()
	active := s.activePlayer
	s.mu.RUnlock()

	s.subMu.Lock()
	defer s.subMu.Unlock()

	for sub := range s.subscribers {
		if sub.player == "" {
			// 根订阅者：仅推送活跃播放器的事件
			if skipRoot || evt.PlayerName != active {
				continue
			}
		} else if sub.player != evt.PlayerName {
			// Per-player 订阅者：仅推送对应播放器的事件
			continue
		}
		if !sub.matchesType(evt.Type) {
			continue
		}
		select {
		case sub.ch <- wsEvt:
		default:
		}
	}
}

// NotifySubscribersFullState 向根订阅者推送指定播放器的完整缓存
// 在 activePlayer 切换时调用，先发送 player_switch 事件再推缓存
// 返回实际发送过的事件类型→内容哈希（供 switchSkip 内容比对使用）
func (s *Server) NotifySubscribersFullState(oldPlayer, newPlayer string) map[string]uint64 {
	switchEvt := WSEvent{
		Type:   player.EventPlayerSwitch,
		Player: newPlayer,
		Data:   &player.PlayerSwitchInfo{From: oldPlayer, To: newPlayer},
	}

	events := s.buildInitEvents(newPlayer, false) // 根切换：不看 per-player 隐藏态（§3.4）

	// 收集实际包含的事件类型及其内容哈希
	sentHashes := make(map[string]uint64, len(events))
	for _, evt := range events {
		sentHashes[evt.Type] = hashEventData(evt.Type, evt.Data)
	}

	s.subMu.Lock()
	defer s.subMu.Unlock()
	for sub := range s.subscribers {
		if sub.player != "" {
			continue // 仅推送给根订阅者
		}
		// 先推 player_switch
		if sub.matchesType(switchEvt.Type) {
			select {
			case sub.ch <- switchEvt:
			default:
			}
		}
		// 再推完整缓存
		for _, evt := range events {
			if !sub.matchesType(evt.Type) {
				continue
			}
			select {
			case sub.ch <- evt:
			default:
			}
		}
	}
	return sentHashes
}

// buildInitEvents 构建指定播放器的完整缓存事件列表
// playerName="" 时使用 activePlayer
func (s *Server) buildInitEvents(playerName string, honorHidden bool) []WSEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if playerName == "" && s.activePlayer == "" {
		return []WSEvent{{
			Type:   player.EventPlayerClear,
			Player: "",
			Data:   struct{}{},
		}}
	}

	// per-player 通道处于无活跃隐藏态：per-player 连接重连时给出与根清除一致的「已清空」初始态，
	// 而不是缓存里停留的末行歌词。
	// honorHidden 必须由调用方区分意图：per-player 连接的初始化(handleWS/handleSSE)传 true；而
	// NotifySubscribersFullState 用**非空的新 activePlayer 名**调本函数、结果推给**根订阅者**，必须传
	// false——否则根切到一个恰处于 per-player 隐藏态的播放器时会被清空，让 per-player 的视图标志泄漏进
	// 根路径(违反 §3.4)。
	if honorHidden && playerName != "" {
		if ps, ok := s.playerStates[playerName]; ok && ps != nil && ps.IdleHidden {
			return []WSEvent{{
				Type:   player.EventPlayerClear,
				Player: playerName,
				Data:   struct{}{},
			}}
		}
	}

	target := playerName
	if target == "" {
		target = s.activePlayer
	}
	if target == "" {
		return nil
	}
	ps, ok := s.playerStates[target]
	if !ok || ps == nil {
		return nil
	}

	var events []WSEvent
	if ps.Status != nil {
		events = append(events, WSEvent{Type: "status_update", Player: target, Data: ps.Status})
	}
	if ps.SongInfo != nil {
		events = append(events, WSEvent{Type: "song_info_update", Player: target, Data: ps.SongInfo})
	}
	if ps.AllLyrics != nil {
		events = append(events, WSEvent{Type: "all_lyrics", Player: target, Data: &AllLyrics{
			Title:    ps.Title,
			Lyrics:   ps.AllLyrics,
			Duration: ps.Duration,
			Position: ps.Position,
			Progress: ps.Progress,
			Count:    len(ps.AllLyrics),
		}})
	}
	if ps.LyricUpdate != nil {
		events = append(events, WSEvent{Type: "lyric_update", Player: target, Data: ps.LyricUpdate})
	}
	return events
}

// --- 服务配置 ---

// SetServiceInfo 设置服务信息
func (s *Server) SetServiceInfo(info *ServiceInfo) {
	s.mu.Lock()
	s.serviceInfo = info
	s.mu.Unlock()
}

// SetActivePlayer 由 Router 调用，更新当前活跃播放器
func (s *Server) SetActivePlayer(name string) {
	s.mu.Lock()
	changed := s.activePlayer != name
	s.activePlayer = name
	s.mu.Unlock()
	if changed {
		// 通知特效订阅者：网易云是否仍是活跃输出（驱动前端淡入/淡出）
		s.BroadcastEffectStatus()
	}
}

// SetIdleHideResolver 注入 per-player 通道无活跃自动隐藏阈值解析器（秒，0=关）。
// Server 刻意不持有 config（见 §3.4 边界），由 main 在启动时把 cfg.GetPlayerIdleHide 喂进来，
// 与 effect 策略等单值注入同一思路。
func (s *Server) SetIdleHideResolver(fn func(playerName string) int) {
	s.mu.Lock()
	s.idleHideResolver = fn
	s.mu.Unlock()
}

// NotifySubscribersClear 向根订阅者推送活跃播放器清除通知
// 发送 player_switch(to="") + player_clear 双重事件
func (s *Server) NotifySubscribersClear(oldPlayer string) {
	switchEvt := WSEvent{
		Type:   player.EventPlayerSwitch,
		Player: "",
		Data:   &player.PlayerSwitchInfo{From: oldPlayer, To: ""},
	}
	clearEvt := WSEvent{
		Type:   player.EventPlayerClear,
		Player: "",
		Data:   struct{}{},
	}

	s.subMu.Lock()
	defer s.subMu.Unlock()
	for sub := range s.subscribers {
		if sub.player != "" {
			continue // 仅推送给根订阅者
		}
		if sub.matchesType(switchEvt.Type) {
			select {
			case sub.ch <- switchEvt:
			default:
			}
		}
		if sub.matchesType(clearEvt.Type) {
			select {
			case sub.ch <- clearEvt:
			default:
			}
		}
	}
}

// watchIdleHide 每秒巡检各 per-player 通道的无活跃隐藏（issue #47）。它只影响 per-player 视图
// （/{player}/* 的 HTTP 快照、WS/SSE 重连初始态、以及 live 推送），绝不读或改 activePlayer /
// skipRoot / 根路径（§3.4）；IdleHidden 只是叠加在缓存之上的一层「视图隐藏」，缓存保持新鲜（§2.7）。
func (s *Server) watchIdleHide() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.sweepIdleHide(time.Now())
	}
}

// idleClearTarget 是一次巡检里需要清除的 per-player 通道及其停下那一刻的整曲进度。
type idleClearTarget struct {
	name     string
	progress float32
}

// sweepIdleHide 把「无活跃达阈值」的 per-player 通道标记为隐藏，并向其 live 订阅者推一次清除。
// 只有开启了自动隐藏（阈值 > 0）、曾经活跃过、当前不在播放、且静默达阈值的通道才会被隐藏。
func (s *Server) sweepIdleHide(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn := s.idleHideResolver // 纯配置读闭包（cfg.GetPlayerIdleHide），不回锁 s.mu，可在锁内直接调
	var toClear []idleClearTarget
	if fn != nil {
		for name, ps := range s.playerStates {
			if ps == nil || ps.IdleHidden || ps.LastEventAt.IsZero() {
				continue // 已隐藏、或从未出过事件的通道不处理（后者保持其自然待机态）
			}
			thr := fn(name)
			if thr <= 0 {
				continue // 该通道未开启自动隐藏
			}
			status := "offline"
			if ps.Status != nil {
				status = ps.Status.Status
			}
			if status == "playing" {
				continue // 正在放（含长间奏）不隐，对齐 issue #47「不播放时才隐藏」
			}
			if now.Sub(ps.LastEventAt) >= time.Duration(thr)*time.Second {
				ps.IdleHidden = true
				// 与根路径「清除活跃播放器」日志对齐：清屏是可见输出变化，需留现场证据（§1.2）。
				serverLog.Info("per-player [%s] 无活跃达 %ds，推送清屏隐藏", name, thr)
				// 带上停下那一刻的整曲进度：SSE 清行的 lyric_update(index:-1) 需要它，
				// 且它与最后一条 all_lyrics.progress 一致、不制造自相矛盾的事件（见 player/lyricupdate_lint_test.go）。
				toClear = append(toClear, idleClearTarget{name: name, progress: ps.Progress})
			}
		}
	}
	// 仍持 s.mu 时推送清除：使「置 IdleHidden=true + 推 clear」对 UpdatePlayerState 原子。否则解锁后、
	// 推送前播放器恰好恢复（UpdatePlayerState 置 IdleHidden=false 并经 NotifySubscribers 推新歌词）会造成
	// sub.ch=[歌词, clear]，迟到的 clear 把已恢复画面清成空白。发送均 select-default 非阻塞、无网络 I/O；
	// 锁序 s.mu→subMu 与既有 NotifySubscribers* 一致，无死锁。
	for _, tc := range toClear {
		s.notifyPerPlayerIdleClear(tc.name, tc.progress)
	}
}

// notifyPerPlayerIdleClear 向某 per-player 通道的 live 订阅者推一次「清除」，按传输/类型过滤裁剪，
// 使 WS/SSE/HTTP 三种传输的可见结果与根路径的清除一致：
//   - WS / 全类型订阅者：一条 player_clear（Player=name）即整体清屏（歌词+封面+标题）。
//   - 类型过滤的 SSE：player_clear 会被自身过滤挡掉，改发 in-band——lyric_update-SSE 发
//     lyric_update(index:-1)（文档已定义的「无歌词」清行），song_info-SSE 发空 song_info_update。
//
// 只投递给 sub.player==name 的订阅者，不碰根订阅者、不读 activePlayer/skipRoot（§3.4）；全部非阻塞（§2.1）。
func (s *Server) notifyPerPlayerIdleClear(name string, progress float32) {
	clearEvt := WSEvent{Type: player.EventPlayerClear, Player: name, Data: struct{}{}}
	// index:-1 是文档已定义的「无歌词」清行形式；Progress 带上停下那一刻的整曲进度，
	// 既满足 lyricupdate 门禁、又与最后一条 all_lyrics.progress 一致（不糊零值假契约）。
	lyricClear := WSEvent{Type: player.EventLyricUpdate, Player: name, Data: &LyricUpdate{Index: -1, Progress: progress}}
	songClear := WSEvent{Type: player.EventSongInfoUpdate, Player: name, Data: struct{}{}}

	s.subMu.Lock()
	defer s.subMu.Unlock()
	for sub := range s.subscribers {
		if sub.player != name {
			continue
		}
		if sub.matchesType(player.EventPlayerClear) {
			// WS / 全类型：一条 player_clear 即整体清屏
			select {
			case sub.ch <- clearEvt:
			default:
			}
			continue
		}
		// 类型过滤的 SSE：按其过滤类型发对应的 in-band 清除
		if sub.matchesType(player.EventLyricUpdate) {
			select {
			case sub.ch <- lyricClear:
			default:
			}
		}
		if sub.matchesType(player.EventSongInfoUpdate) {
			select {
			case sub.ch <- songClear:
			default:
			}
		}
	}
}

// getPlayerState 获取/创建播放器状态
func (s *Server) getPlayerState(playerName string) *PlayerState {
	if ps, ok := s.playerStates[playerName]; ok {
		return ps
	}
	ps := &PlayerState{}
	s.playerStates[playerName] = ps
	return ps
}

// UpdatePlayerState 仅更新播放器状态缓存（不广播，始终调用）
func (s *Server) UpdatePlayerState(evt player.Event) {
	s.mu.Lock()
	ps := s.getPlayerState(evt.PlayerName)

	// 任何对外真实事件都视为该 per-player 通道的一次活跃：刷新计时锚点、解除无活跃隐藏态。
	// 内部事件（clear_song_data）是「停止/清缓存」信号、不算活跃，不重置计时（否则会推迟隐藏）。
	if !internalEvents[evt.Type] {
		ps.LastEventAt = time.Now()
		ps.IdleHidden = false
	}

	switch evt.Type {
	case player.EventStatusUpdate:
		if msg, ok := evt.Data.(*player.StatusInfo); ok {
			ps.Status = &StatusMessage{Status: msg.Status, Detail: msg.Detail}
		}
	case player.EventSongInfoUpdate:
		if msg, ok := evt.Data.(*player.SongInfo); ok {
			ps.SongInfo = &SongInfoUpdate{
				Name: msg.Name, Singer: msg.Singer, Title: msg.Title,
				Cover: msg.Cover, CoverBase64: msg.CoverBase64,
			}
		}
	case player.EventLyricUpdate:
		if msg, ok := evt.Data.(*player.LyricUpdate); ok {
			ps.LyricUpdate = &LyricUpdate{
				Index: msg.Index, Text: msg.Text, SubText: msg.SubText, RomaText: msg.RomaText,
				Timestamp: msg.Timestamp, PlayTime: msg.PlayTime,
				Position: msg.Position, Progress: msg.Progress, TextDetailed: msg.TextDetailed,
			}
			// 同步更新 Position 与 Progress 缓存，保持 FullState 时效性。
			//
			// **两个必须一起更新**：它们是同一时刻的两个投影（position 是整曲实时位置的绝对
			// 秒数、progress 是它除以时长），漏掉任何一个都会让 buildInitEvents / FullState /
			// HTTP 拼出一个自相矛盾的 all_lyrics——此前只更新位置缓存、Progress 从切歌那一刻起
			// 永远停在 0（实测 280 次 HTTP 采样零例外：位置=134.86 而 progress=0，本应 0.5756）。
			// 症状：切播放器时进度条归零，而时间在走。
			//
			// 存 msg.Position（整曲实时位置）而非 msg.PlayTime（本行播出时间）：后者在逐字引子
			// 行上会早于行时间戳数秒，拿它当 all_lyrics 的整曲位置会自相矛盾。position 无歧义。
			ps.Position = msg.Position
			ps.Progress = msg.Progress
		}
	case player.EventAllLyrics:
		if msg, ok := evt.Data.(*player.AllLyricsData); ok {
			ps.AllLyrics = msg.Lyrics
			ps.Title = msg.Title
			ps.Duration = msg.Duration
			ps.Position = msg.Position
			ps.Progress = msg.Progress
			ps.LyricUpdate = nil // 新歌词到达，旧 lyric_update 已失效；避免中途连入的客户端收到上一首的歌词行
		}
	case player.EventClearSongData:
		ps.SongInfo = nil
		ps.LyricUpdate = nil
		ps.AllLyrics = nil
		ps.Title = ""
		ps.Duration = 0
		ps.Position = 0
		ps.Progress = 0
	}
	s.mu.Unlock()
}

// GetPlayerStatus 获取播放器状态
func (s *Server) GetPlayerStatus(playerName string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ps, ok := s.playerStates[playerName]; ok && ps.Status != nil {
		return ps.Status.Status
	}
	return "offline"
}

// --- 启动 & 路由注册 ---

// Start 启动 HTTP 服务；readyCh 非 nil 时，端口监听成功后会 close(readyCh) 通知调用方
func (s *Server) Start(addr string, readyCh chan struct{}) error {
	mux := http.NewServeMux()

	// 声明式路由表：新增播放器零代码注册
	routes := []routeDef{
		{"/ws", "ws", nil, nil},
		{"/all_lyrics", "http", s.handleAllLyrics, nil},
		{"/lyric_update", "http", s.handleLyricUpdate, nil},
		{"/status_update", "http", s.handleStatusUpdate, nil},
		{"/song_info", "http", s.handleSongInfo, nil},
		{"/lyric_update-SSE", "sse", nil, []string{"lyric_update"}},
		{"/song_info-SSE", "sse", nil, []string{"song_info_update"}},
	}

	registerRoutes := func(prefix, playerName string) {
		for _, rd := range routes {
			path := prefix + rd.suffix
			switch rd.kind {
			case "ws":
				mux.HandleFunc(path, s.handleWS(playerName))
			case "http":
				mux.HandleFunc(path, rd.httpH(playerName))
			case "sse":
				mux.HandleFunc(path, s.handleSSE(playerName, rd.sseTypes...))
			}
		}
	}

	// 根路径
	registerRoutes("", "")

	// 播放器专属路径
	for name := range s.playerStates {
		registerRoutes("/"+name, name)
	}

	// 仅根路径的内部端点
	mux.HandleFunc("/health-check", s.handleHealthCheck)
	mux.HandleFunc("/service-status", s.handleServiceStatus)

	// 网易云特效镜像帧通道（二进制 JPEG 帧）—— 归于 cloudmusicv3 播放器命名空间
	mux.HandleFunc("/cloudmusicv3/effect-ws", s.handleEffectWS)
	// 纯层捕获帧回传：网易云注入脚本把特效 canvas 的 JPEG 推到这里
	mux.HandleFunc("/cloudmusicv3/effect-ingest", s.handleEffectIngest)
	// 网易云特效运行时控制（策略切换 / 手动 park）—— 已关闭：策略改为 config.yml 静态读取
	// （动态切换无法反映到静态的 /service-status）。保留 handleEffectControl 代码以便将来按需重新启用。
	// mux.HandleFunc("/cloudmusicv3/effect-control", s.handleEffectControl)

	// 记录监听端口，供注入脚本拼接 page→Go 回传地址
	if _, port, perr := net.SplitHostPort(addr); perr == nil && port != "" {
		s.effectHub.mu.Lock()
		s.effectHub.ingestPort = port
		s.effectHub.mu.Unlock()
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	serverLog.Info("服务启动: %s", addr)
	if readyCh != nil {
		close(readyCh)
	}
	// per-player 通道无活跃自动隐藏的巡检（读 SetIdleHideResolver 注入的阈值；未注入即全空转）
	go s.watchIdleHide()
	return http.Serve(ln, corsMiddleware(mux))
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == "OPTIONS" {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- WebSocket ---

func (s *Server) handleWS(playerName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := s.upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverLog.Warn("WS Upgrade error: %v", err)
			return
		}

		sub := s.subscribe(playerName, nil, true, conn.RemoteAddr().String())
		serverLog.Info("WS 连接: %s player=%q (total: %d)", conn.RemoteAddr(), playerName, s.wsCount())

		// 发送初始缓存状态
		for _, evt := range s.buildInitEvents(playerName, true) {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			conn.WriteJSON(evt)
		}

		// 写入 goroutine：从订阅通道读取并发送到 WebSocket。
		//
		// 写失败必须显式关连接：**写失败不蕴含连接已断**。写超时（客户端不读）与 JSON
		// 编码失败（WriteJSON 先 NextWriter 再 Encode，NaN/Inf 在编码阶段就失败，连接完好）
		// 都会让这里出错而 TCP 仍然活着。此时只 return 的话，下面的读循环会永远阻塞在
		// ReadMessage —— overlay 客户端从不发消息 —— sub 便永远留在 s.subscribers 里，
		// wsCount() 仍把它算作在线，而它再也收不到任何事件。
		done := make(chan struct{})
		go func() {
			defer close(done)
			for evt := range sub.ch {
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteJSON(evt); err != nil {
					serverLog.Warn("WS 写入失败，关闭连接: %s player=%q: %v", conn.RemoteAddr(), playerName, err)
					conn.Close() // 解阻塞读循环，交由下面既有的清理路径注销
					return
				}
			}
		}()

		// Read loop (keep alive, detect disconnect)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}

		// 清理：停止接收 → 关闭通道 → 关闭连接 → 等待写入完成
		s.unsubscribe(sub)
		close(sub.ch)
		conn.Close()
		<-done
		serverLog.Info("WS 断开: %s player=%q (total: %d)", conn.RemoteAddr(), playerName, s.wsCount())
	}
}

// --- HTTP Handlers ---

func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, HTTPResponse{
		Code:   0,
		Msg:    "success",
		Player: "internal",
		Data:   map[string]interface{}{"now_time": time.Now().Format(time.RFC3339)},
	})
}

func (s *Server) handleServiceStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	info := s.serviceInfo

	playerStatus := NewOrderedMap()
	playerRunning := make([]string, 0)
	for _, name := range info.PlayerSupport {
		ps := s.playerStates[name]
		st := "offline"
		if ps != nil && ps.Status != nil {
			st = ps.Status.Status
		}
		playerStatus.Set(name, st)
		if st != "offline" && st != "standby" && st != "waiting_process" {
			playerRunning = append(playerRunning, name)
		}
	}
	s.mu.RUnlock()

	clients := s.ClientAddrs()

	data := struct {
		Version           string      `json:"version"`
		Addr              string      `json:"addr"`
		NowTime           string      `json:"now_time"`
		ConfigSources     []string    `json:"config_sources"`
		Config            *OrderedMap `json:"config"`
		ConfigOverwritten []string    `json:"config_overwritten"`
		PlayerSupport     []string    `json:"player_support"`
		PlayerRunning     []string    `json:"player_running"`
		PlayerStatus      *OrderedMap `json:"player_status"`
		Endpoints         *OrderedMap `json:"endpoints"`
		ClientCount       int         `json:"client_count"`
		WSConnected       interface{} `json:"ws_connected"`
	}{
		Version:           info.Version,
		Addr:              info.Addr,
		NowTime:           time.Now().Format(time.RFC3339),
		ConfigSources:     info.Sources,
		Config:            info.Config,
		ConfigOverwritten: info.ConfigOverwritten,
		PlayerSupport:     info.PlayerSupport,
		PlayerRunning:     playerRunning,
		PlayerStatus:      playerStatus,
		Endpoints:         info.Endpoints,
		ClientCount:       len(clients),
		WSConnected: map[string]interface{}{
			"connected": len(clients) > 0,
			"clients":   clients,
		},
	}

	writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: "internal", Data: data})
}

// resolvePlayer 根据路径或 active player 确定用哪个播放器。
//
// 根路径（playerName==""）完全跟随 Router 设定的 activePlayer，**包括 activePlayer==""
// 这个信号本身**：router.go clearActivePlayer 主动置空并推 player_switch(to="") +
// player_clear，语义是「当前没有播放器在放」，不是「还没初始化」。而 normalizeStatus 把
// standby/waiting_process/waiting_song 全归为 idle，router 永不为它们设 activePlayer ——
// 所以「开机没放歌 → activePlayer 为空」是长期稳态，不是启动瞬态。
//
// 此时返回 ""，四个 handler 的 `ps != nil` 守卫落入既有 nil 分支，返回 Data:{}，与
// buildInitEvents 给 WS 的 player_clear 语义一致。
//
// **不要再加「取任意有状态的播放器」的兜底。** 这里曾有两个 `range map` 循环（先找
// Status=="playing"，再找任意 Status != nil），已删，理由：
//   - 它的孪生体 findAnyNonPriorPlayer() 已被 9090f0b 从 router 删除，commit 原话是
//     「歌词冻结 bug 的根本原因」「不检查播放状态」；这两个循环是同型残株。
//   - 它诞生于 Server 还没有 activePlayer 字段的年代（65d9e80，当时它是唯一选址逻辑）；
//     6087be4 叠加 activePlayer 优先分支时没清旧路径，「兜底」是事后追认的标签而非设计。
//   - `range map` 顺序随机：实测同一待机态 400 次调用分布 wesing:260 / qqmusic:53 /
//     cloudmusicv3:49 / kugou:38，连续两次答案不一致 214/400。且四个 handler 各自独立
//     调用本函数，同一秒内 /song_info 与 /status_update 可解析到不同播放器，拼出
//     「A 的状态 + B 的歌名」。
func (s *Server) resolvePlayer(playerName string) string {
	if playerName != "" {
		return playerName
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activePlayer
}

// perPlayerHidden 报告某 per-player 请求当前是否处于无活跃隐藏态（issue #47）。
// 仅对 per-player 请求（requestPlayer != ""）生效；根请求恒返回 false——根路径绝不看 per-player
// 的 IdleHidden（§3.4：per-player 状态不得影响根，哪怕 activePlayer 恰好就是它）。
func (s *Server) perPlayerHidden(requestPlayer string) bool {
	if requestPlayer == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ps, ok := s.playerStates[requestPlayer]
	return ok && ps != nil && ps.IdleHidden
}

// --- HTTP 读取端点：锁内取快照，锁外序列化 ---
//
// 这四个 handler 的既有写法是「RLock → 取 *PlayerState → RUnlock → 读它的字段」，
// 锁只保护了 map 查找，字段读全在锁外，而 UpdatePlayerState(:329) 是在 s.mu.Lock()
// 内写这些字段的 —— 写侧持锁、读侧不持锁，是真实数据竞争，已复现两种后果：
//  1. slice header 撕裂：ps.AllLyrics 拿到「新 ptr + 旧 len」→ BuildLyricsDetailed
//     把分配区外的垃圾当 LyricLine 迭代 → BuildDetailedText 解引用垃圾指针 panic。
//     net/http 的 conn.serve 会 recover handler panic，故**进程不死**；但它只
//     recover + 打日志 + 关连接，**不写任何响应**，且 json 是先整体 marshal 再写、
//     panic 发生在 marshal 阶段，一个字节都没发出去 —— 所以客户端拿到的是**裸 EOF /
//     connection reset，不是 500**（实测：`Get "http://…": EOF`；服务端只有
//     `http: panic serving`）。排障时该找的是连接层错误，不是 5xx 日志。
//  2. 非原子快照：跨过写者的两次赋值即拿到「A 的标题 + B 的歌词」，不需要任何撕裂。
//
// 关于两者的相对频率：实测回退旧写法跑 6 次，**6/6 全死于 (1) 撕裂 panic**，
// race_test.go 里那个「非原子快照」检测器一次都没触发过 —— (1) 先发生，检测器
// 根本没机会看到 (2)。(2) 的存在是从 panic 栈帧（Title len=4 + Count=0x28）推得的，
// 属推断而非直接复现。此前注释把两者的频率写反了。
//
// 两条纪律，别改：
//   - **快照必须在锁内取完**：AllLyrics 的 slice header 与 Title 等字段必须在同一次
//     持锁内一起读出，否则仍是非原子快照。
//   - **绝不在持锁期间 writeJSON**：json.Marshal + 网络写会被慢客户端拖住，持 RLock
//     写网络等于让一个慢下游阻塞 UpdatePlayerState，进而阻塞整个路由器。
//
// 只拷 slice header / 指针而不做深拷贝是安全的：发布出去的数据事实上不可变——
// ps.AllLyrics 只被整体替换（:356 / :366），全仓无 AllLyrics[i] 下标写入；
// ps.Status / ps.SongInfo / ps.LyricUpdate 每次更新都是新分配的结构体。
func (s *Server) handleAllLyrics(playerName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pn := s.resolvePlayer(playerName)
		if s.perPlayerHidden(playerName) {
			writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: struct{}{}})
			return
		}

		s.mu.RLock()
		var data *AllLyrics
		if ps := s.playerStates[pn]; ps != nil && ps.AllLyrics != nil {
			data = &AllLyrics{
				Title:    ps.Title,
				Lyrics:   ps.AllLyrics,
				Duration: ps.Duration,
				Position: ps.Position,
				Progress: ps.Progress,
				Count:    len(ps.AllLyrics),
			}
		}
		s.mu.RUnlock()

		if data == nil {
			writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: struct{}{}})
			return
		}
		writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: data})
	}
}

func (s *Server) handleLyricUpdate(playerName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pn := s.resolvePlayer(playerName)
		if s.perPlayerHidden(playerName) {
			writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: struct{}{}})
			return
		}

		s.mu.RLock()
		var data *LyricUpdate
		if ps := s.playerStates[pn]; ps != nil {
			data = ps.LyricUpdate
		}
		s.mu.RUnlock()

		if data == nil {
			writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: struct{}{}})
			return
		}
		writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: data})
	}
}

func (s *Server) handleStatusUpdate(playerName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pn := s.resolvePlayer(playerName)
		if s.perPlayerHidden(playerName) {
			writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: struct{}{}})
			return
		}

		s.mu.RLock()
		var data *StatusMessage
		if ps := s.playerStates[pn]; ps != nil {
			data = ps.Status
		}
		s.mu.RUnlock()

		if data == nil {
			writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: struct{}{}})
			return
		}
		writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: data})
	}
}

func (s *Server) handleSongInfo(playerName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pn := s.resolvePlayer(playerName)
		if s.perPlayerHidden(playerName) {
			writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: struct{}{}})
			return
		}

		s.mu.RLock()
		var data *SongInfoUpdate
		if ps := s.playerStates[pn]; ps != nil {
			data = ps.SongInfo
		}
		s.mu.RUnlock()

		if data == nil {
			writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: struct{}{}})
			return
		}
		writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: data})
	}
}

// --- SSE ---

func (s *Server) handleSSE(playerName string, eventTypes ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "SSE not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		sub := s.subscribe(playerName, eventTypes, false, r.RemoteAddr)
		defer s.unsubscribe(sub)
		serverLog.Info("SSE 订阅: path=%s player=%q eventTypes=%v", r.URL.Path, playerName, eventTypes)

		// 发送初始缓存（仅匹配 eventTypes 的事件）
		for _, evt := range s.buildInitEvents(playerName, true) {
			if !sub.matchesType(evt.Type) {
				continue
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		flusher.Flush()

		// 持续推送
		for {
			select {
			case evt := <-sub.ch:
				data, _ := json.Marshal(evt)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
}

// --- 工具函数 ---

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// wsCount 返回 WS 连接数
func (s *Server) wsCount() int {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	count := 0
	for sub := range s.subscribers {
		if sub.isWS {
			count++
		}
	}
	return count
}

// ClientCount 返回 WS 客户端连接数
func (s *Server) ClientCount() int {
	return s.wsCount()
}

// ClientAddrs 返回 WS 客户端地址列表
func (s *Server) ClientAddrs() []string {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	addrs := []string{}
	for sub := range s.subscribers {
		if sub.isWS {
			addrs = append(addrs, sub.addr)
		}
	}
	return addrs
}

func playerFromPath(path string, knownPlayers map[string]*PlayerState) string {
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) >= 1 {
		if _, ok := knownPlayers[parts[0]]; ok {
			return parts[0]
		}
	}
	return ""
}
