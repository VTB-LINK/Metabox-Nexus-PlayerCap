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
	SongTitle   string
	Duration    float32
	PlayTime    float32
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

// NotifySubscribers 向匹配的订阅者推送事件
// skipRoot=true 时跳过根订阅者（播放器切换后避免与 FullState 重复）
func (s *Server) NotifySubscribers(evt player.Event, skipRoot bool) {
	wsEvt := WSEvent{Type: evt.Type, Player: evt.PlayerName, Data: evt.Data}

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

	events := s.buildInitEvents(newPlayer)

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
func (s *Server) buildInitEvents(playerName string) []WSEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()

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
			SongTitle: ps.SongTitle,
			Lyrics:    ps.AllLyrics,
			Duration:  ps.Duration,
			PlayTime:  ps.PlayTime,
			Count:     len(ps.AllLyrics),
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
	s.activePlayer = name
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
				LineIndex: msg.LineIndex, Text: msg.Text,
				Timestamp: msg.Timestamp, PlayTime: msg.PlayTime,
				Progress: msg.Progress,
			}
			// 同步更新 PlayTime 缓存，保持 FullState 时效性
			ps.PlayTime = msg.PlayTime
		}
	case player.EventAllLyrics:
		if msg, ok := evt.Data.(*player.AllLyricsData); ok {
			ps.AllLyrics = msg.Lyrics
			ps.SongTitle = msg.SongTitle
			ps.Duration = msg.Duration
			ps.PlayTime = msg.PlayTime
			ps.LyricUpdate = nil // 新歌词到达，旧 lyric_update 已失效；避免中途连入的客户端收到上一首的歌词行
		}
	case player.EventClearSongData:
		ps.SongInfo = nil
		ps.LyricUpdate = nil
		ps.AllLyrics = nil
		ps.SongTitle = ""
		ps.Duration = 0
		ps.PlayTime = 0
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

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	serverLog.Info("服务启动: %s", addr)
	if readyCh != nil {
		close(readyCh)
	}
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
		for _, evt := range s.buildInitEvents(playerName) {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			conn.WriteJSON(evt)
		}

		// 写入 goroutine：从订阅通道读取并发送到 WebSocket
		done := make(chan struct{})
		go func() {
			defer close(done)
			for evt := range sub.ch {
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteJSON(evt); err != nil {
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

// resolvePlayer 根据路径或 active player 确定用哪个播放器
func (s *Server) resolvePlayer(playerName string) string {
	if playerName != "" {
		return playerName
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	// 根路径优先使用 Router 设定的 activePlayer，与 WS/SSE 行为一致
	if s.activePlayer != "" {
		return s.activePlayer
	}
	// 兜底：无 activePlayer 时取任意有状态的播放器
	for name, ps := range s.playerStates {
		if ps.Status != nil && ps.Status.Status == "playing" {
			return name
		}
	}
	for name, ps := range s.playerStates {
		if ps.Status != nil {
			return name
		}
	}
	return ""
}

func (s *Server) handleAllLyrics(playerName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pn := s.resolvePlayer(playerName)
		s.mu.RLock()
		ps := s.playerStates[pn]
		s.mu.RUnlock()

		if ps == nil || ps.AllLyrics == nil {
			writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: struct{}{}})
			return
		}

		writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: &AllLyrics{
			SongTitle: ps.SongTitle,
			Lyrics:    ps.AllLyrics,
			Duration:  ps.Duration,
			PlayTime:  ps.PlayTime,
			Count:     len(ps.AllLyrics),
		}})
	}
}

func (s *Server) handleLyricUpdate(playerName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pn := s.resolvePlayer(playerName)
		s.mu.RLock()
		ps := s.playerStates[pn]
		s.mu.RUnlock()

		if ps == nil || ps.LyricUpdate == nil {
			writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: struct{}{}})
			return
		}
		writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: ps.LyricUpdate})
	}
}

func (s *Server) handleStatusUpdate(playerName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pn := s.resolvePlayer(playerName)
		s.mu.RLock()
		ps := s.playerStates[pn]
		s.mu.RUnlock()

		if ps == nil || ps.Status == nil {
			writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: struct{}{}})
			return
		}
		writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: ps.Status})
	}
}

func (s *Server) handleSongInfo(playerName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pn := s.resolvePlayer(playerName)
		s.mu.RLock()
		ps := s.playerStates[pn]
		s.mu.RUnlock()

		if ps == nil || ps.SongInfo == nil {
			writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: struct{}{}})
			return
		}
		writeJSON(w, HTTPResponse{Code: 0, Msg: "success", Player: pn, Data: ps.SongInfo})
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
		for _, evt := range s.buildInitEvents(playerName) {
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
