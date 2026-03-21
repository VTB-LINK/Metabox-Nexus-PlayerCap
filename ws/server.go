package ws

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSEvent WebSocket/SSE 统一事件包装器
type WSEvent struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// LyricUpdate 歌词更新消息
type LyricUpdate struct {
	LineIndex int     `json:"line_index"`
	Text      string  `json:"text"`
	Timestamp float32 `json:"timestamp"`
	PlayTime  float32 `json:"play_time"`
	Progress  float32 `json:"progress"`
}

// AllLyrics 完整歌词列表
type AllLyrics struct {
	SongTitle string      `json:"song_title,omitempty"`
	Duration  float32     `json:"duration"`
	PlayTime  float32     `json:"play_time"`
	Lyrics    []LyricItem `json:"lyrics"`
	Count     int         `json:"count"`
}

// LyricItem 歌词列表中的单项
type LyricItem struct {
	Index int     `json:"index"`
	Time  float32 `json:"time"`
	Text  string  `json:"text"`
}

// StatusMessage 状态消息
type StatusMessage struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// SongInfoUpdate 歌曲信息更新消息
type SongInfoUpdate struct {
	Name   string `json:"name,omitempty"`
	Singer string `json:"singer,omitempty"`
	Title  string `json:"title,omitempty"`
}

// ServiceInfo 服务配置信息
type ServiceInfo struct {
	Version   string   // 服务版本
	Addr      string   // 服务监听地址
	Offset    int      // 时间偏移
	Poll      int      // 轮询间隔
	Sources   []string // 配置来源列表
	Endpoints map[string]string // 所有接口地址
}

// Server WebSocket 广播服务器
type Server struct {
	clients         map[*websocket.Conn]bool
	mu              sync.RWMutex
	upgrader        websocket.Upgrader
	allLyrics       []LyricItem      // 缓存的完整歌词列表
	lastStatus      *StatusMessage   // 缓存的最新状态
	lastSongInfo    *SongInfoUpdate  // 缓存的最新歌曲信息
	lastLyricUpdate *LyricUpdate     // 缓存的最新歌词更新
	lastDuration    float32          // 缓存的歌曲时长 (from Dev)
	lastPlayTime    float32          // 缓存的最新播放时间（BroadcastAllLyrics 时写入）
	lyricUpdateChan chan *LyricUpdate // SSE 歌词更新通道
	songInfoChan    chan *SongInfoUpdate // SSE 歌曲信息通道
	serviceInfo     *ServiceInfo     // 服务配置信息
}

// NewServer 创建 WebSocket 服务器
func NewServer() *Server {
	s := &Server{
		clients: make(map[*websocket.Conn]bool),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		lyricUpdateChan: make(chan *LyricUpdate, 100),
		songInfoChan:    make(chan *SongInfoUpdate, 100),
		serviceInfo: &ServiceInfo{
			Version: "2.0.0",
		},
	}
	return s
}

// SetServiceInfo 设置服务配置信息
func (s *Server) SetServiceInfo(info *ServiceInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serviceInfo = info
}

// SetLyrics 设置歌词缓存
func (s *Server) SetLyrics(lyrics []LyricItem) {
	s.allLyrics = lyrics
}

// emptyData 空数据占位符，序列化为 {}
var emptyData = struct{}{}

// ClearSongData 清空歌曲相关缓存并广播空数据（歌曲信息、歌词列表、当前歌词）
func (s *Server) ClearSongData() {
	s.mu.Lock()
	s.lastSongInfo = nil
	s.allLyrics = nil
	s.lastLyricUpdate = nil
	s.lastDuration = 0
	s.lastPlayTime = 0
	s.mu.Unlock()
	s.Broadcast(WSEvent{Type: "song_info_update", Data: emptyData})
	s.Broadcast(WSEvent{Type: "all_lyrics", Data: emptyData})
	// 发送给 SSE 订阅者
	select {
	case s.songInfoChan <- nil:
	default:
	}
}

// SetStatus 设置并广播状态
func (s *Server) SetStatus(status, detail string) {
	msg := StatusMessage{Status: status, Detail: detail}
	s.mu.Lock()
	s.lastStatus = &msg
	s.mu.Unlock()
	s.Broadcast(WSEvent{Type: "status_update", Data: &msg})
}

// SetSongInfo 发送歌曲信息更新
func (s *Server) SetSongInfo(name, singer, title string) {
	msg := SongInfoUpdate{Name: name, Singer: singer, Title: title}
	s.mu.Lock()
	s.lastSongInfo = &msg
	s.mu.Unlock()
	s.Broadcast(WSEvent{Type: "song_info_update", Data: &msg})
	// 发送给 SSE 订阅者
	select {
	case s.songInfoChan <- &msg:
	default:
	}
}

// SetDuration 设置歌曲总时长缓存
func (s *Server) SetDuration(d float32) {
	s.mu.Lock()
	s.lastDuration = d
	s.mu.Unlock()
}

// BroadcastLyricUpdate 广播歌词更新
func (s *Server) BroadcastLyricUpdate(update *LyricUpdate) {
	s.mu.Lock()
	s.lastLyricUpdate = update
	s.mu.Unlock()
	s.Broadcast(WSEvent{Type: "lyric_update", Data: update})
	// 发送给 SSE 订阅者
	select {
	case s.lyricUpdateChan <- update:
	default:
	}
}

// BroadcastLyricNull 广播空的歌词更新（用于清空歌词状态）
func (s *Server) BroadcastLyricNull() {
	s.mu.Lock()
	s.lastLyricUpdate = nil
	s.mu.Unlock()
	s.Broadcast(WSEvent{Type: "lyric_update", Data: emptyData})
	// 发送给 SSE 订阅者
	select {
	case s.lyricUpdateChan <- nil:
	default:
	}
}

// BroadcastAllLyrics 广播完整歌词列表（含 Duration 和当前 PlayTime）
func (s *Server) BroadcastAllLyrics(songTitle string, lyrics []LyricItem, playTime float32) {
	s.mu.Lock()
	s.lastPlayTime = playTime
	duration := s.lastDuration
	s.mu.Unlock()
	s.Broadcast(WSEvent{Type: "all_lyrics", Data: AllLyrics{SongTitle: songTitle, Duration: duration, PlayTime: playTime, Lyrics: lyrics, Count: len(lyrics)}})
}

// BroadcastIdle 广播空闲消息（歌曲结束）
func (s *Server) BroadcastIdle() {
	s.Broadcast(WSEvent{Type: "lyric_idle", Data: emptyData})
}

// BroadcastPause 广播暂停消息（play_time 停止推进）
func (s *Server) BroadcastPause(playTime float32) {
	s.Broadcast(WSEvent{Type: "playback_pause", Data: map[string]interface{}{"play_time": playTime}})
}

// BroadcastResume 广播恢复播放消息
func (s *Server) BroadcastResume(playTime float32) {
	s.Broadcast(WSEvent{Type: "playback_resume", Data: map[string]interface{}{"play_time": playTime}})
}

// handleWS 处理 WebSocket 连接
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[!] WebSocket 升级失败: %v", err)
		return
	}

	s.mu.Lock()
	s.clients[conn] = true
	s.mu.Unlock()

	fmt.Printf("[+] 客户端连接: %s\n", conn.RemoteAddr())

	// 发送当前状态（始终发送，无数据时 data 为 {}）
	{
		var d interface{} = emptyData
		if s.lastStatus != nil {
			d = s.lastStatus
		}
		data, _ := json.Marshal(WSEvent{Type: "status_update", Data: d})
		conn.WriteMessage(websocket.TextMessage, data)
	}

	// 发送当前歌曲信息（始终发送，无数据时 data 为 {}）
	{
		var d interface{} = emptyData
		if s.lastSongInfo != nil {
			d = s.lastSongInfo
		}
		data, _ := json.Marshal(WSEvent{Type: "song_info_update", Data: d})
		conn.WriteMessage(websocket.TextMessage, data)
	}

	// 发送最新歌词（始终发送，无数据时 data 为 {}）
	{
		var d interface{} = emptyData
		if s.lastLyricUpdate != nil {
			d = s.lastLyricUpdate
		}
		data, _ := json.Marshal(WSEvent{Type: "lyric_update", Data: d})
		conn.WriteMessage(websocket.TextMessage, data)
	}

	// 发送完整歌词列表（始终发送，无歌词时 data 为 {}）
	if len(s.allLyrics) > 0 {
		songTitle := ""
		if s.lastSongInfo != nil {
			songTitle = s.lastSongInfo.Title
		}
		playTime := s.lastPlayTime
		if s.lastLyricUpdate != nil && s.lastLyricUpdate.PlayTime > playTime {
			playTime = s.lastLyricUpdate.PlayTime
		}
		data, _ := json.Marshal(WSEvent{Type: "all_lyrics", Data: AllLyrics{SongTitle: songTitle, Duration: s.lastDuration, PlayTime: playTime, Lyrics: s.allLyrics, Count: len(s.allLyrics)}})
		conn.WriteMessage(websocket.TextMessage, data)
	} else {
		data, _ := json.Marshal(WSEvent{Type: "all_lyrics", Data: emptyData})
		conn.WriteMessage(websocket.TextMessage, data)
	}

	// 读循环（保持连接）
	defer func() {
		s.mu.Lock()
		delete(s.clients, conn)
		s.mu.Unlock()
		conn.Close()
		fmt.Printf("[-] 客户端断开: %s\n", conn.RemoteAddr())
	}()

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

// Broadcast 向所有客户端广播消息
func (s *Server) Broadcast(msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for conn := range s.clients {
		err := conn.WriteMessage(websocket.TextMessage, data)
		if err != nil {
			conn.Close()
			delete(s.clients, conn)
		}
	}
}

// ClientCount 返回当前连接数
func (s *Server) ClientCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}

// healthCheck HTTP 健康检查接口
func (s *Server) healthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{
		"code": 0,
		"msg":  "success",
		"data": map[string]interface{}{
			"now_time": time.Now().Format("2006-01-02T15:04:05+08:00"),
		},
	}
	json.NewEncoder(w).Encode(resp)
}

// serviceStatus 服务状态接口
func (s *Server) serviceStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.RLock()
	status := "offline"
	if s.lastStatus != nil {
		status = s.lastStatus.Status
	}
	clientCount := len(s.clients)
	// 收集客户端地址列表
	var clientAddrs []string
	for conn := range s.clients {
		clientAddrs = append(clientAddrs, conn.RemoteAddr().String())
	}
	serviceInfo := s.serviceInfo
	s.mu.RUnlock()
	
	// 构建响应数据
	data := map[string]interface{}{
		"status":         status,
		"now_time":       time.Now().Format("2006-01-02T15:04:05+08:00"),
		"client_count":   clientCount,
		"ws_connected": map[string]interface{}{
			"connected": clientCount > 0,
			"clients":   clientAddrs,
		},
	}
	
	// 添加服务配置信息
	if serviceInfo != nil {
		data["version"] = serviceInfo.Version
		data["addr"] = serviceInfo.Addr
		data["config_sources"] = serviceInfo.Sources
		data["config"] = map[string]interface{}{
			"addr":   serviceInfo.Addr,
			"offset": serviceInfo.Offset,
			"poll":   serviceInfo.Poll,
		}
		if len(serviceInfo.Endpoints) > 0 {
			data["endpoints"] = serviceInfo.Endpoints
		}
	}
	
	resp := map[string]interface{}{
		"code": 0,
		"msg":  "success",
		"data": data,
	}
	json.NewEncoder(w).Encode(resp)
}

// handleAllLyrics 返回全部歌词列表
func (s *Server) handleAllLyrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.RLock()
	lyrics := s.allLyrics
	songTitle := ""
	if s.lastSongInfo != nil {
		songTitle = s.lastSongInfo.Title
	}
	duration := s.lastDuration
	s.mu.RUnlock()
	
	var data interface{} = emptyData
	if len(lyrics) > 0 {
		data = map[string]interface{}{
			"song_title": songTitle,
			"duration":   duration,
			"lyrics":     lyrics,
			"count":      len(lyrics),
		}
	}
	resp := map[string]interface{}{
		"code": 0,
		"msg":  "success",
		"data": data,
	}
	json.NewEncoder(w).Encode(resp)
}

// lyricUpdate 返回当前歌词（最新一条）
func (s *Server) lyricUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.RLock()
	lastLyric := s.lastLyricUpdate
	s.mu.RUnlock()
	
	var data interface{} = emptyData
	if lastLyric != nil {
		data = lastLyric
	}
	resp := map[string]interface{}{
		"code": 0,
		"msg":  "success",
		"data": data,
	}
	json.NewEncoder(w).Encode(resp)
}

// statusUpdate 返回播放状态
func (s *Server) statusUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.RLock()
	status := s.lastStatus
	s.mu.RUnlock()
	
	var data interface{} = emptyData
	if status != nil {
		data = status
	}
	resp := map[string]interface{}{
		"code": 0,
		"msg":  "success",
		"data": data,
	}
	json.NewEncoder(w).Encode(resp)
}

// songInfo 返回歌曲信息
func (s *Server) songInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.RLock()
	si := s.lastSongInfo
	s.mu.RUnlock()
	
	var data interface{} = emptyData
	if si != nil {
		data = si
	}
	resp := map[string]interface{}{
		"code": 0,
		"msg":  "success",
		"data": data,
	}
	json.NewEncoder(w).Encode(resp)
}

// lyricUpdateSSE SSE 实时歌词推送接口
func (s *Server) lyricUpdateSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	// 发送最新的歌词更新（如果存在，否则发送 {}）
	s.mu.RLock()
	lastLyric := s.lastLyricUpdate
	s.mu.RUnlock()
	
	{
		var d interface{} = emptyData
		if lastLyric != nil {
			d = lastLyric
		}
		event := WSEvent{Type: "lyric_update", Data: d}
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	
	// 监听新的歌词更新
	for {
		select {
		case update := <-s.lyricUpdateChan:
			var d interface{} = emptyData
			if update != nil {
				d = update
			}
			event := WSEvent{Type: "lyric_update", Data: d}
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

// songInfoSSE SSE 实时歌曲信息推送接口
func (s *Server) songInfoSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	// 发送最新的歌曲信息（如果存在，否则发送 {}）
	s.mu.RLock()
	lastSongInfo := s.lastSongInfo
	s.mu.RUnlock()
	
	{
		var d interface{} = emptyData
		if lastSongInfo != nil {
			d = lastSongInfo
		}
		event := WSEvent{Type: "song_info_update", Data: d}
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	
	// 监听新的歌曲信息更新
	for {
		select {
		case update := <-s.songInfoChan:
			var d interface{} = emptyData
			if update != nil {
				d = update
			}
			event := WSEvent{Type: "song_info_update", Data: d}
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

// Start 启动 HTTP/WebSocket 服务
func (s *Server) Start(addr string) error {
	// WebSocket
	http.HandleFunc("/ws", s.handleWS)
	
	// HTTP 接口（静态数据）
	http.HandleFunc("/health-check", s.healthCheck)
	http.HandleFunc("/service-status", s.serviceStatus)
	http.HandleFunc("/all_lyrics", s.handleAllLyrics)
	http.HandleFunc("/lyric_update", s.lyricUpdate)
	http.HandleFunc("/status_update", s.statusUpdate)
	http.HandleFunc("/song_info", s.songInfo)
	
	// SSE 接口（实时推送）
	http.HandleFunc("/lyric_update-SSE", s.lyricUpdateSSE)
	http.HandleFunc("/song_info-SSE", s.songInfoSSE)
	
	fmt.Println("\n========== HTTP/WebSocket 服务接口 ==========")
	fmt.Printf("[*] WebSocket: ws://%s/ws\n", addr)
	fmt.Println("\n--- HTTP 接口 (静态数据) ---")
	fmt.Printf("[*] 健康检查: http://%s/health-check\n", addr)
	fmt.Printf("[*] 服务状态: http://%s/service-status\n", addr)
	fmt.Printf("[*] 完整歌词: http://%s/all_lyrics\n", addr)
	fmt.Printf("[*] 当前歌词: http://%s/lyric_update\n", addr)
	fmt.Printf("[*] 播放状态: http://%s/status_update\n", addr)
	fmt.Printf("[*] 歌曲信息: http://%s/song_info\n", addr)
	fmt.Println("\n--- SSE 接口 (实时推送) ---")
	fmt.Printf("[*] 歌词推送: http://%s/lyric_update-SSE\n", addr)
	fmt.Printf("[*] 歌曲推送: http://%s/song_info-SSE\n", addr)
	fmt.Println("\n[*] 等待客户端连接...\n")
	return http.ListenAndServe(addr, nil)
}
