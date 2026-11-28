package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/config"
	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/rum"

	"github.com/gorilla/websocket"
)

type capturedRUMBatch struct {
	Batch []struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	} `json:"batch"`
}

type capturedRUMEvent struct {
	Event      string
	Properties map[string]any
}

type serverRUMCollector struct {
	srv     *httptest.Server
	mu      sync.Mutex
	rawBody []string
	events  []capturedRUMEvent
}

func setupServerRUMCollector(t *testing.T) *serverRUMCollector {
	t.Helper()
	col := &serverRUMCollector{}
	col.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{\"enabled\":true}"}}`)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			body, _ := io.ReadAll(r.Body)
			var parsed capturedRUMBatch
			_ = json.Unmarshal(body, &parsed)
			col.mu.Lock()
			col.rawBody = append(col.rawBody, string(body))
			for _, ev := range parsed.Batch {
				col.events = append(col.events, capturedRUMEvent{
					Event:      ev.Event,
					Properties: ev.Properties,
				})
			}
			col.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	restore := rum.OverrideCredentialsForTest("phc_server_test", col.srv.URL)
	t.Cleanup(func() {
		restore()
		col.srv.Close()
	})
	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		HTTPClient: col.srv.Client(),
	})
	rum.Start()
	return col
}

func (c *serverRUMCollector) snapshot() []capturedRUMEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedRUMEvent, len(c.events))
	copy(out, c.events)
	return out
}

func (c *serverRUMCollector) rawJSON() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.rawBody, "\n")
}

func TestClassifyClientSurfaceRedactsLocalAndQuery(t *testing.T) {
	col := setupServerRUMCollector(t)

	cases := []struct {
		name    string
		origin  string
		referer string
		ua      string
		want    string
	}{
		{
			name:   "在线编辑器根路径",
			origin: "https://playercap.cfg.vtb.tools/",
			ua:     "Mozilla/5.0 Chrome/120.0",
			want:   "online_editor",
		},
		{
			name:    "在线v3带查询参数Referer",
			origin:  "https://playercap.cfg.vtb.tools",
			referer: "https://playercap.cfg.vtb.tools/v3/lyric_display.html?title=secret",
			ua:      "Mozilla/5.0 Chrome/120.0",
			want:    "online_v3",
		},
		{
			name:   "在线v3带查询参数Origin",
			origin: "https://playercap.cfg.vtb.tools/v3/lyric_display.html?title=secret",
			want:   "online_v3",
		},
		{
			name:    "本地文件URI含用户名与查询串",
			origin:  "file:///C:/Users/Alice/Desktop/lyric_page.html?text=secret",
			referer: "file:///C:/Users/Alice/Desktop/lyric_page.html?text=secret",
			ua:      "Mozilla/5.0 Chrome/120.0",
			want:    "local_file",
		},
		{
			name:   "浏览器本地文件null Origin",
			origin: "null",
			ua:     "Mozilla/5.0 Chrome/120.0",
			want:   "local_file",
		},
		{
			name: "OBS浏览器源UA",
			ua:   "Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 OBS/30.0.2 Chrome/103.0",
			want: "obs_browser",
		},
		{
			name:   "普通外部浏览器",
			origin: "http://127.0.0.1:3000",
			ua:     "Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/124.0",
			want:   "browser_other",
		},
		{
			name: "自定义脚本客户端",
			ua:   "curl/8.5.0",
			want: "custom_client",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyClientSurface(tc.origin, tc.referer, tc.ua)
			if got != tc.want {
				t.Fatalf("classifyClientSurface(%q, %q, %q) = %q，期望 %q", tc.origin, tc.referer, tc.ua, got, tc.want)
			}
		})
	}

	// 经真实 HTTP 端点与 rum 序列化出网，核对零原始路径/用户名/查询参数泄露。
	srv := NewServer([]string{"wesing"})
	mux := http.NewServeMux()
	mux.HandleFunc("/song_info", srv.handleSongInfo(""))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req1, _ := http.NewRequest(http.MethodGet, ts.URL+"/song_info", nil)
	req1.Header.Set("Origin", "file:///C:/Users/Alice/Desktop/lyric_page.html?text=secret")
	req1.Header.Set("Referer", "file:///C:/Users/Alice/Desktop/lyric_page.html?text=secret")
	resp1, err := ts.Client().Do(req1)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp1.Body.Close()

	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/song_info", nil)
	req2.Header.Set("Origin", "https://playercap.cfg.vtb.tools")
	req2.Header.Set("Referer", "https://playercap.cfg.vtb.tools/v3/lyric_display.html?title=secret")
	resp2, err := ts.Client().Do(req2)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp2.Body.Close()

	srv.FlushRUM()
	rum.Flush(2 * time.Second)

	raw := col.rawJSON()
	if raw == "" {
		t.Fatal("未收到任何 /batch/ 请求")
	}
	for _, forbidden := range []string{"Alice", "secret", "lyric_page.html", "title=", "text=", "file://"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("出网 JSON 泄露了违禁文本 %q：\n%s", forbidden, raw)
		}
	}
}

func TestRouterTransitionEvents(t *testing.T) {
	col := setupServerRUMCollector(t)

	cfg := config.DefaultConfig()
	cfg.PriorPlayer = []string{"wesing"}
	cfg.PriorPlayerExpire = 10

	srv := NewServer([]string{"wesing", "cloudmusicv3"})
	srv.SetIdleHideResolver(func(string) int { return 5 })
	router := NewRouter(&cfg, srv, []string{"wesing", "cloudmusicv3"})

	sendStatus := func(p, st string) {
		evt := player.Event{
			Type:       player.EventStatusUpdate,
			PlayerName: p,
			Data:       &player.StatusInfo{Status: st},
		}
		srv.UpdatePlayerState(evt)
		router.updateRouting(evt)
	}

	// 1. 普通播放器开始播放 → switch (normal_switch)
	sendStatus("cloudmusicv3", "playing")
	// 2. 优先播放器开始播放 → switch (prior_preempt)
	sendStatus("wesing", "playing")
	// 3. 优先播放器暂停 → enter_holding
	sendStatus("wesing", "paused")
	// 4. 优先组暂停保持超时 → expire_timeout (group_paused_timeout) + switch (normal_fallback)
	router.mu.Lock()
	router.priorGroupPauseAt = time.Now().Add(-15 * time.Second)
	router.mu.Unlock()
	router.sweepExpire(time.Now())
	// 5. 普通播放器进入停止 → clear_all (true_stop)
	sendStatus("cloudmusicv3", "waiting_song")

	// 6. idle_hide 隐藏与唤醒
	srv.mu.Lock()
	srv.playerStates["cloudmusicv3"].LastEventAt = time.Now().Add(-10 * time.Second)
	srv.mu.Unlock()
	srv.sweepIdleHide(time.Now())
	sendStatus("cloudmusicv3", "playing") // 唤醒 idle_wake + switch (normal_switch)

	rum.Flush(2 * time.Second)

	snap := srv.StatsSnapshot()
	if snap.RouteSwitches != 4 {
		t.Errorf("StatsSnapshot().RouteSwitches = %d，期望 4（normal启动、prior抢占、normal回落、唤醒再播）", snap.RouteSwitches)
	}

	var transitions []capturedRUMEvent
	for _, ev := range col.snapshot() {
		if ev.Event == "router_transition" {
			transitions = append(transitions, ev)
		}
	}

	hasKindReason := func(kind, from, to, reason string) bool {
		for _, ev := range transitions {
			if ev.Properties["kind"] == kind &&
				(from == "*" || ev.Properties["from_player"] == from) &&
				(to == "*" || ev.Properties["to_player"] == to) &&
				(reason == "*" || ev.Properties["reason"] == reason) {
				return true
			}
		}
		return false
	}

	checks := []struct {
		desc   string
		kind   string
		from   string
		to     string
		reason string
	}{
		{"普通启动切换", "switch", "", "cloudmusicv3", "normal_switch"},
		{"优先抢占切换", "switch", "cloudmusicv3", "wesing", "prior_preempt"},
		{"优先进入暂停保持", "enter_holding", "wesing", "wesing", "*"},
		{"优先组超时释放", "expire_timeout", "wesing", "cloudmusicv3", "group_paused_timeout"},
		{"回落普通播放器", "switch", "wesing", "cloudmusicv3", "normal_fallback"},
		{"全员清空", "clear_all", "cloudmusicv3", "", "true_stop"},
		{"无活跃静默隐藏", "idle_hide", "cloudmusicv3", "", "*"},
		{"无活跃唤醒", "idle_wake", "", "cloudmusicv3", "*"},
	}
	for _, c := range checks {
		if !hasKindReason(c.kind, c.from, c.to, c.reason) {
			t.Errorf("缺少路由跃迁 %s（kind=%q from=%q to=%q reason=%q），实际事件：%#v",
				c.desc, c.kind, c.from, c.to, c.reason, transitions)
		}
	}

	// 核对必要整数计数键均存在
	for _, ev := range transitions {
		if _, ok := ev.Properties["active_prior_count"]; !ok {
			t.Errorf("router_transition 缺少 active_prior_count：%#v", ev.Properties)
		}
		if _, ok := ev.Properties["active_normal_count"]; !ok {
			t.Errorf("router_transition 缺少 active_normal_count：%#v", ev.Properties)
		}
	}
}

func TestSlowConsumerDropCountersAndClientSession(t *testing.T) {
	col := setupServerRUMCollector(t)
	_, _ = rum.DrainDropped()

	srv := NewServer([]string{"wesing", "cloudmusicv3"})
	srv.SetActivePlayer("wesing")

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.handleWS(""))
	mux.HandleFunc("/audio-ws", srv.handleAudioWS(""))
	mux.HandleFunc("/cloudmusicv3/effect-ws", srv.handleEffectWS)
	mux.HandleFunc("/cloudmusicv3/effect-ingest", srv.handleEffectIngest)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	dial := func(path string, headers http.Header) *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial(wsURL+path, headers)
		if err != nil {
			t.Fatalf("dial %s 失败: %v", path, err)
		}
		return conn
	}

	wsHdr := http.Header{"User-Agent": []string{"Mozilla/5.0 OBS/30.0"}}
	wsConn := dial("/ws", wsHdr)
	defer wsConn.Close()
	audioHdr := http.Header{"Origin": []string{"https://playercap.cfg.vtb.tools"}}
	audioConn := dial("/audio-ws", audioHdr)
	defer audioConn.Close()
	effectHdr := http.Header{
		"Origin":  []string{"https://playercap.cfg.vtb.tools"},
		"Referer": []string{"https://playercap.cfg.vtb.tools/v3/effect.html?secret=1"},
	}
	effectConn := dial("/cloudmusicv3/effect-ws", effectHdr)
	defer effectConn.Close()

	// 等待三个服务端 handler 完成 Upgrade 后的订阅登记再检查峰值
	regDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(regDeadline) {
		if srv.ClientCount() == 1 && len(srv.AudioClientAddrs()) == 1 && len(srv.EffectClientAddrs()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 验证三类峰值只读快照（未 Reset 前重复调用值不变）
	snap1 := srv.StatsSnapshot()
	snap2 := srv.StatsSnapshot()
	if snap1.PeakWSClients != 1 || snap1.PeakAudioClients != 1 || snap1.PeakEffectClients != 1 {
		t.Fatalf("连接峰值异常：%+v", snap1)
	}
	if snap2 != snap1 {
		t.Fatalf("StatsSnapshot 必须为纯只读快照，连续两次返回不同：%+v vs %+v", snap1, snap2)
	}

	// 验证 effect-ingest 错误 token 触发 403 并计数
	resp403, err := ts.Client().Get(ts.URL + "/cloudmusicv3/effect-ingest?t=wrong")
	if err != nil {
		t.Fatalf("请求 effect-ingest 失败: %v", err)
	}
	resp403.Body.Close()
	if resp403.StatusCode != http.StatusForbidden {
		t.Fatalf("期望 403，实际 %d", resp403.StatusCode)
	}
	if got := srv.DrainEffectIngest403(); got != 1 {
		t.Errorf("DrainEffectIngest403() = %d，期望 1", got)
	}
	if got := srv.DrainEffectIngest403(); got != 0 {
		t.Errorf("二次 DrainEffectIngest403() = %d，期望 0", got)
	}

	// 直接往慢消费者满通道触发丢包：同时测真实 WS/Audio/Effect 的满丢包路径
	// 先建各自处于满态的慢订阅者，保证确定性满丢（不依赖 TCP 缓冲区大小）
	slowWS := srv.subscribeWithSession("", nil, true, "slow-ws", "ws_root", "obs_browser")
	for i := 0; i < cap(slowWS.ch); i++ {
		slowWS.ch <- WSEvent{Type: "status_update", Player: "wesing", Data: struct{}{}}
	}
	srv.NotifySubscribers(player.Event{
		Type:       player.EventStatusUpdate,
		PlayerName: "wesing",
		Data:       &player.StatusInfo{Status: "playing"},
	}, false)
	srv.unsubscribe(slowWS)
	slowWS.session.emitEnded(slowWS.dropped.Load(), time.Now())

	slowAudio := &audioSub{
		ch:      make(chan []byte, 8),
		player:  "",
		addr:    "slow-audio",
		session: newClientSessionMeta("audio_ws_root", "", "online_editor", time.Now()),
	}
	for i := 0; i < cap(slowAudio.ch); i++ {
		slowAudio.ch <- []byte{1}
	}
	srv.audioHub.addSub(slowAudio)
	srv.audioHub.BroadcastAudioFrame("wesing", []byte{2})
	srv.audioHub.removeSub(slowAudio)
	slowAudio.session.emitEnded(slowAudio.dropped.Load(), time.Now())

	slowEffect := &effectSub{
		ch:      make(chan effectMsg, 8),
		addr:    "slow-effect",
		session: newClientSessionMeta("effect_ws", cloudmusicPlayerName, "online_v3", time.Now()),
	}
	for i := 0; i < cap(slowEffect.ch); i++ {
		slowEffect.ch <- effectMsg{mt: websocket.BinaryMessage, data: []byte{1}}
	}
	srv.effectHub.mu.Lock()
	srv.effectHub.subs[slowEffect] = struct{}{}
	srv.effectHub.mu.Unlock()
	srv.BroadcastEffectFrame([]byte{2})
	srv.effectHub.mu.Lock()
	delete(srv.effectHub.subs, slowEffect)
	srv.effectHub.mu.Unlock()
	slowEffect.session.emitEnded(slowEffect.dropped.Load(), time.Now())

	// 关闭真实连接，验证 ResetPeakClients 随当前在连数回落
	wsConn.Close()
	audioConn.Close()
	effectConn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.ClientCount() == 0 && len(srv.AudioClientAddrs()) == 0 && len(srv.EffectClientAddrs()) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 断开前只读快照仍保留峰值；TakeHeartbeatStatsSnapshot 原子取出峰值并回落为当前在连数 0
	if st := srv.StatsSnapshot(); st.PeakWSClients < 1 || st.PeakAudioClients < 1 || st.PeakEffectClients < 1 {
		t.Errorf("TakeHeartbeatStatsSnapshot 前只读快照周期峰值不应丢失：%+v", st)
	}
	taken := srv.TakeHeartbeatStatsSnapshot()
	if taken.PeakWSClients < 1 || taken.PeakAudioClients < 1 || taken.PeakEffectClients < 1 {
		t.Errorf("TakeHeartbeatStatsSnapshot 应返回本周期峰值：%+v", taken)
	}
	if st := srv.StatsSnapshot(); st.PeakWSClients != 0 || st.PeakAudioClients != 0 || st.PeakEffectClients != 0 {
		t.Errorf("全部断开并 TakeHeartbeatStatsSnapshot 后周期峰值应回落为 0，实得 %+v", st)
	}

	_, droppedByScope := rum.DrainDropped()
	for _, scope := range []string{"ws_sub", "audio_sub", "effect_sub"} {
		if droppedByScope[scope] == 0 {
			t.Errorf("rum.DrainDropped()[%q] = 0，期望 > 0，实得 %#v", scope, droppedByScope)
		}
	}

	rum.Flush(2 * time.Second)

	events := col.snapshot()
	var sawConnected, sawEndedWS, sawEndedAudio, sawEndedEffect bool
	for _, ev := range events {
		if ev.Event != "client_session" {
			continue
		}
		action := ev.Properties["action"]
		ch := ev.Properties["channel"]
		surface := ev.Properties["client_surface"]
		if action == "connected" && ch == "ws_root" && surface == "obs_browser" {
			sawConnected = true
		}
		if action == "ended" {
			dropped, _ := ev.Properties["dropped_frames_or_events"].(float64)
			if _, hasDur := ev.Properties["duration_sec"]; !hasDur {
				t.Errorf("client_session(ended) 缺少 duration_sec：%#v", ev.Properties)
			}
			if ch == "ws_root" && surface == "obs_browser" && dropped >= 1 {
				sawEndedWS = true
			}
			if ch == "audio_ws_root" && surface == "online_editor" && dropped >= 1 {
				sawEndedAudio = true
			}
			if ch == "effect_ws" && surface == "online_v3" && dropped >= 1 {
				sawEndedEffect = true
			}
		}
	}
	if !sawConnected {
		t.Errorf("未收到 ws_root 首连 client_session(action=connected)，事件：%#v", events)
	}
	if !sawEndedWS || !sawEndedAudio || !sawEndedEffect {
		t.Errorf("缺少带丢包数的 client_session(ended)：ws=%v audio=%v effect=%v，事件：%#v",
			sawEndedWS, sawEndedAudio, sawEndedEffect, events)
	}
}

func TestHTTPSnapshotSummaryAggregation(t *testing.T) {
	col := setupServerRUMCollector(t)

	srv := NewServer([]string{"wesing"})
	srv.SetServiceInfo(&ServiceInfo{PlayerSupport: []string{"wesing"}})

	// 0 请求时 FlushRUM 不应发 http_snapshot_summary
	srv.FlushRUM()
	rum.Flush(2 * time.Second)
	for _, ev := range col.snapshot() {
		if ev.Event == "http_snapshot_summary" {
			t.Fatalf("0 请求时不应发送 http_snapshot_summary：%#v", ev)
		}
	}

	// 将 wesing 设为无活跃隐藏态以触发 idle_hidden_hits
	srv.mu.Lock()
	srv.playerStates["wesing"].IdleHidden = true
	srv.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("/all_lyrics", srv.handleAllLyrics(""))
	mux.HandleFunc("/wesing/lyric_update", srv.handleLyricUpdate("wesing"))
	mux.HandleFunc("/song_info", srv.handleSongInfo(""))
	mux.HandleFunc("/status_update", srv.handleStatusUpdate(""))
	mux.HandleFunc("/service-status", srv.handleServiceStatus)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	doReq := func(path string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Header.Set("User-Agent", "Python-urllib/3.11")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("GET %s 失败: %v", path, err)
		}
		resp.Body.Close()
	}

	doReq("/all_lyrics")
	doReq("/wesing/lyric_update")
	doReq("/wesing/lyric_update")
	doReq("/song_info")
	doReq("/status_update")
	doReq("/service-status")

	srv.FlushRUM()
	rum.Flush(2 * time.Second)

	var summaries []capturedRUMEvent
	for _, ev := range col.snapshot() {
		if ev.Event == "http_snapshot_summary" {
			summaries = append(summaries, ev)
		}
	}
	if len(summaries) != 1 {
		t.Fatalf("期望收到 1 条 http_snapshot_summary，实际 %d 条：%#v", len(summaries), summaries)
	}
	props := summaries[0].Properties
	if props["client_surface"] != "custom_client" {
		t.Errorf("client_surface = %#v，期望 custom_client", props["client_surface"])
	}
	if props["idle_hidden_hits"] != float64(2) {
		t.Errorf("idle_hidden_hits = %#v，期望 2", props["idle_hidden_hits"])
	}
	counts, _ := props["counts_by_route"].(map[string]any)
	wantCounts := map[string]float64{
		"all_lyrics":     1,
		"lyric_update":   2,
		"song_info":      1,
		"status_update":  1,
		"service_status": 1,
	}
	for k, want := range wantCounts {
		if counts[k] != want {
			t.Errorf("counts_by_route[%q] = %#v，期望 %v（全部：%#v）", k, counts[k], want, counts)
		}
	}

	// 二次 FlushRUM（已清零）不应再发
	srv.FlushRUM()
	rum.Flush(2 * time.Second)
	countAfter := 0
	for _, ev := range col.snapshot() {
		if ev.Event == "http_snapshot_summary" {
			countAfter++
		}
	}
	if countAfter != 1 {
		t.Errorf("清零后二次 FlushRUM 不应再发 http_snapshot_summary，实得总数 %d", countAfter)
	}
}

func TestFlushRUMSettlesActiveClientsOnce(t *testing.T) {
	col := setupServerRUMCollector(t)

	srv := NewServer([]string{"wesing"})
	mux := http.NewServeMux()
	mux.HandleFunc("/wesing/ws", srv.handleWS("wesing"))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	hdr := http.Header{"Origin": []string{"https://playercap.cfg.vtb.tools/"}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL+"/wesing/ws", hdr)
	if err != nil {
		t.Fatalf("dial /wesing/ws 失败: %v", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && srv.ClientCount() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.ClientCount() != 1 {
		t.Fatalf("前置：期望 1 个 WS 连接，实际 %d", srv.ClientCount())
	}

	// 在连接仍在线时调用 FlushRUM，应结算 1 条 ended
	srv.FlushRUM()
	// 随后断开连接并再次 FlushRUM，均不得重复发送 ended
	conn.Close()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && srv.ClientCount() > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	srv.FlushRUM()

	rum.Flush(2 * time.Second)

	endedCount := 0
	for _, ev := range col.snapshot() {
		if ev.Event == "client_session" && ev.Properties["action"] == "ended" && ev.Properties["channel"] == "ws_player" {
			endedCount++
			if ev.Properties["target_player"] != "wesing" || ev.Properties["client_surface"] != "online_editor" {
				t.Errorf("ws_player 结算属性异常：%#v", ev.Properties)
			}
		}
	}
	if endedCount != 1 {
		t.Fatalf("同一连接在 FlushRUM + 断开后仅应结算 1 次 ended，实际 %d 次", endedCount)
	}
}

// TestTakeHeartbeatStatsSnapshotAtomicPeakReset 覆盖 R4：并发连接升降期间原子取峰并回落，不丢失窗口内新峰值。
func TestTakeHeartbeatStatsSnapshotAtomicPeakReset(t *testing.T) {
	srv := NewServer([]string{"wesing"})

	sub1 := srv.subscribe("", nil, true, "c1")
	sub2 := srv.subscribe("", nil, true, "c2")
	srv.unsubscribe(sub2) // 当前 1，峰值 2

	// 只读快照不重置
	if s1 := srv.StatsSnapshot(); s1.PeakWSClients != 2 {
		t.Fatalf("只读 StatsSnapshot PeakWSClients = %d，期望 2", s1.PeakWSClients)
	}
	// 原子取峰返回 2 并回落为当前连接数 1
	if taken := srv.TakeHeartbeatStatsSnapshot(); taken.PeakWSClients != 2 {
		t.Fatalf("TakeHeartbeatStatsSnapshot PeakWSClients = %d，期望 2", taken.PeakWSClients)
	}
	if after := srv.StatsSnapshot(); after.PeakWSClients != 1 {
		t.Fatalf("回落后 PeakWSClients = %d，期望当前连接数 1", after.PeakWSClients)
	}

	// 并发窗口：持续短连接升降 + 反复 TakeHeartbeatStatsSnapshot，期间任一周期内的新峰必被某次 Take 或最终快照观测到
	var maxObserved uint64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			a := srv.subscribe("", nil, true, "burst-a")
			b := srv.subscribe("", nil, true, "burst-b")
			srv.unsubscribe(b)
			srv.unsubscribe(a)
		}
		close(done)
	}()
	for {
		select {
		case <-done:
			goto finished
		default:
			if p := srv.TakeHeartbeatStatsSnapshot().PeakWSClients; p > maxObserved {
				maxObserved = p
			}
		}
	}
finished:
	wg.Wait()
	if p := srv.TakeHeartbeatStatsSnapshot().PeakWSClients; p > maxObserved {
		maxObserved = p
	}
	srv.unsubscribe(sub1)
	if maxObserved < 2 {
		t.Fatalf("并发短连接期间应捕获到 >=2 的峰值，实得 %d", maxObserved)
	}
}

// TestHTTPSnapshotSummaryConcurrentNoTornIdleHidden 覆盖 R5：
// 并发高频 recordHTTPSnapshot(..., idleHidden=true) 与 flushHTTPSnapshotSummary()，
// 断言每条产出的 http_snapshot_summary 均满足 sum(counts_by_route) >= idle_hidden_hits 且不存在空 counts 配非零 idle_hidden_hits。
func TestHTTPSnapshotSummaryConcurrentNoTornIdleHidden(t *testing.T) {
	col := setupServerRUMCollector(t)
	srv := NewServer([]string{"wesing"})

	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/wesing/lyric_update", nil)
	req.Header.Set("User-Agent", "Python-urllib/3.11")

	const maxConcurrentFlushes = 30

	var wg sync.WaitGroup
	ready := make(chan struct{}, 1)
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				srv.recordHTTPSnapshot(req, httpRouteLyricUpdate, true)
				if j == 0 {
					select {
					case ready <- struct{}{}:
					default:
					}
				}
				if j%4 == 0 {
					runtime.Gosched()
				}
			}
		}()
	}
	var flushWG sync.WaitGroup
	flushWG.Add(1)
	go func() {
		defer flushWG.Done()
		select {
		case <-ready:
		case <-stop:
			return
		}
		for flushes := 0; flushes < maxConcurrentFlushes; flushes++ {
			select {
			case <-stop:
				return
			default:
			}
			srv.flushHTTPSnapshotSummary()
			runtime.Gosched()
		}
	}()
	wg.Wait()
	close(stop)
	flushWG.Wait()
	// 收尾冲刷剩余计数；并发最多 30 次 + 收尾 1 次，总事件数 <= 31 < 50，确保单次 rum.Flush 收齐全部 800 条总账。
	srv.flushHTTPSnapshotSummary()
	rum.Flush(2 * time.Second)

	var totalReq, totalHidden float64
	for _, ev := range col.snapshot() {
		if ev.Event != "http_snapshot_summary" {
			continue
		}
		counts, _ := ev.Properties["counts_by_route"].(map[string]any)
		hidden, _ := ev.Properties["idle_hidden_hits"].(float64)
		var sum float64
		for _, v := range counts {
			n, _ := v.(float64)
			sum += n
		}
		if sum < hidden || (hidden > 0 && len(counts) == 0) {
			t.Fatalf("检测到撕裂汇总：counts_by_route=%#v idle_hidden_hits=%v", counts, hidden)
		}
		totalReq += sum
		totalHidden += hidden
	}
	if totalReq != 800 || totalHidden != 800 {
		t.Fatalf("并发总账不一致：totalReq=%v totalHidden=%v，期望均 800", totalReq, totalHidden)
	}
}

// TestRouterRunIdleWakeReportsUpdatedActiveCount 覆盖 R6：
// 按真实 Router.Run 处理流水线（processEvent），唯一普通播放器处于 IdleHidden=true 后收到 playing，
// 断言 idle_wake 的 active_normal_count 为更新后的 1 而非旧值 0。
func TestRouterRunIdleWakeReportsUpdatedActiveCount(t *testing.T) {
	col := setupServerRUMCollector(t)

	cfg := config.DefaultConfig()
	srv := NewServer([]string{"cloudmusicv3"})
	srv.SetIdleHideResolver(func(string) int { return 5 })
	router := NewRouter(&cfg, srv, []string{"cloudmusicv3"})

	// 先置为非活跃并触发无活跃隐藏
	router.processEvent(player.Event{
		Type:       player.EventStatusUpdate,
		PlayerName: "cloudmusicv3",
		Data:       &player.StatusInfo{Status: "waiting_song"},
	})
	srv.mu.Lock()
	srv.playerStates["cloudmusicv3"].LastEventAt = time.Now().Add(-10 * time.Second)
	srv.mu.Unlock()
	srv.sweepIdleHide(time.Now())

	// 经真实 Router 流程唤醒并进入 playing
	router.processEvent(player.Event{
		Type:       player.EventStatusUpdate,
		PlayerName: "cloudmusicv3",
		Data:       &player.StatusInfo{Status: "playing"},
	})

	rum.Flush(2 * time.Second)

	var wakeEv map[string]any
	for _, ev := range col.snapshot() {
		if ev.Event == "router_transition" && ev.Properties["kind"] == "idle_wake" {
			wakeEv = ev.Properties
		}
	}
	if wakeEv == nil {
		t.Fatalf("未收到 idle_wake 事件，全部事件：%#v", col.snapshot())
	}
	if wakeEv["active_normal_count"] != float64(1) {
		t.Errorf("idle_wake 的 active_normal_count = %#v，期望更新后的 1（全部属性：%#v）", wakeEv["active_normal_count"], wakeEv)
	}
}

// TestFlushRUMSettlesPostFlushRegisteredClients 覆盖 R7 残余：
// 用 FlushRUM() + rum.Shutdown() 真实退出序列验证：
// 1) FlushRUM() 之前登记的连接成对产出 connected + ended；
// 2) FlushRUM() 之后（含 rum.Shutdown() 前后）再登记的 WS / Audio / Effect 连接既不产生孤儿 connected 也不漏 ended。
func TestFlushRUMSettlesPostFlushRegisteredClients(t *testing.T) {
	col := setupServerRUMCollector(t)

	srv := NewServer([]string{"wesing", "cloudmusicv3"})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.handleWS(""))
	mux.HandleFunc("/audio-ws", srv.handleAudioWS(""))
	mux.HandleFunc("/cloudmusicv3/effect-ws", srv.handleEffectWS)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	// 1. FlushRUM 之前建立 1 条 ws_root（online_editor），应成对发出 connected + ended
	preConn, _, err := websocket.DefaultDialer.Dial(wsURL+"/ws", http.Header{
		"Origin": []string{"https://playercap.cfg.vtb.tools/"},
	})
	if err != nil {
		t.Fatalf("dial pre /ws 失败: %v", err)
	}
	defer preConn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && srv.ClientCount() < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	// 2. 执行真实退出序列：FlushRUM() → 紧接 post-flush 新连入（保持在线）→ rum.Shutdown()
	srv.FlushRUM()

	postWS, _, err := websocket.DefaultDialer.Dial(wsURL+"/ws", http.Header{
		"User-Agent": []string{"Mozilla/5.0 OBS/30.0"},
	})
	if err != nil {
		t.Fatalf("dial post /ws 失败: %v", err)
	}
	defer postWS.Close()
	postAudio, _, err := websocket.DefaultDialer.Dial(wsURL+"/audio-ws", http.Header{
		"Origin": []string{"https://playercap.cfg.vtb.tools/"},
	})
	if err != nil {
		t.Fatalf("dial post /audio-ws 失败: %v", err)
	}
	defer postAudio.Close()
	postEff, _, err := websocket.DefaultDialer.Dial(wsURL+"/cloudmusicv3/effect-ws", http.Header{
		"Origin":  []string{"https://playercap.cfg.vtb.tools"},
		"Referer": []string{"https://playercap.cfg.vtb.tools/v3/effect.html"},
	})
	if err != nil {
		t.Fatalf("dial post /cloudmusicv3/effect-ws 失败: %v", err)
	}
	defer postEff.Close()

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.ClientCount() >= 2 && len(srv.AudioClientAddrs()) >= 1 && len(srv.EffectClientAddrs()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	rum.Shutdown(2 * time.Second)

	// 随后断开连接，同样不能 panic（session==nil 安全）
	preConn.Close()
	postWS.Close()
	postAudio.Close()
	postEff.Close()

	var preConnected, preEnded int
	var postOrphanConnected, postEnded int
	for _, ev := range col.snapshot() {
		if ev.Event != "client_session" {
			continue
		}
		ch, _ := ev.Properties["channel"].(string)
		surf, _ := ev.Properties["client_surface"].(string)
		act, _ := ev.Properties["action"].(string)
		if ch == "ws_root" && surf == "online_editor" {
			if act == "connected" {
				preConnected++
			} else if act == "ended" {
				preEnded++
			}
			continue
		}
		if act == "connected" {
			postOrphanConnected++
		} else if act == "ended" {
			postEnded++
		}
	}
	if preConnected != 1 || preEnded != 1 {
		t.Fatalf("FlushRUM 前连接的会话应成对出现 connected=1 ended=1，实际 connected=%d ended=%d（全部：%#v）",
			preConnected, preEnded, col.snapshot())
	}
	if postOrphanConnected != 0 || postEnded != 0 {
		t.Fatalf("FlushRUM 后登记的连接不应发出孤儿 connected 或残留 ended，实际 connected=%d ended=%d（全部：%#v）",
			postOrphanConnected, postEnded, col.snapshot())
	}
}

// TestFlushRUMWaitsForInFlightDisconnectEmitEnded 回归测试：
// 固定交错：断连处理抢先 CAS 进入 emitEnded 后在 testBeforeEmit 暂停，并发 FlushRUM() 必须等待其完成
// rum.Track 后才返回，随后 rum.Shutdown() 绝不丢失 ended 且零重复。
func TestFlushRUMWaitsForInFlightDisconnectEmitEnded(t *testing.T) {
	col := setupServerRUMCollector(t)

	srv := NewServer([]string{"wesing"})
	sub := srv.subscribeWithSession("wesing", nil, true, "127.0.0.1:10001", "ws_player", "obs_browser")

	inEmit := make(chan struct{})
	releaseEmit := make(chan struct{})
	sub.session.testBeforeEmit = func() {
		close(inEmit)
		<-releaseEmit
	}

	discDone := make(chan struct{})
	go func() {
		defer close(discDone)
		srv.unsubscribe(sub)
		srv.endClientSession(sub.session, sub.dropped.Load(), time.Now())
	}()
	<-inEmit

	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		srv.FlushRUM()
	}()

	select {
	case <-flushDone:
		t.Fatal("断连路径尚未完成 emitEnded 的 rum.Track，FlushRUM 不得提前返回")
	case <-time.After(40 * time.Millisecond):
	}

	close(releaseEmit)
	select {
	case <-flushDone:
	case <-time.After(2 * time.Second):
		t.Fatal("释放屏障后 FlushRUM 超时未返回")
	}
	<-discDone

	rum.Shutdown(2 * time.Second)

	var endedCnt int
	for _, ev := range col.snapshot() {
		if ev.Event == "client_session" && ev.Properties["action"] == "ended" && ev.Properties["channel"] == "ws_player" {
			endedCnt++
		}
	}
	if endedCnt != 1 {
		t.Fatalf("期望 FlushRUM 等待完成后恰好收到 1 条 ended，实得 %d（全部：%#v）", endedCnt, col.snapshot())
	}
}

// TestFlushRUMSettlesUnsubscribedPendingDisconnectSession 回归测试：
// 固定交错：先 unsubscribe（模拟断开流程等待写协程 <-done 期间），再调用 FlushRUM() + rum.Shutdown()，
// 断言该会话仍被 FlushRUM() 结清 ended，且后续断连流程继续结算绝不重复发送。
func TestFlushRUMSettlesUnsubscribedPendingDisconnectSession(t *testing.T) {
	col := setupServerRUMCollector(t)

	srv := NewServer([]string{"wesing", "cloudmusicv3"})
	wsSub := srv.subscribeWithSession("wesing", nil, true, "127.0.0.1:10002", "ws_player", "online_editor")
	wsSub.dropped.Store(2)

	audioSubItem := &audioSub{ch: make(chan []byte, 8), player: "wesing", addr: "127.0.0.1:10003"}
	audioSubItem.session = srv.registerClientSession(
		newClientSessionMeta("audio_ws_player", "wesing", "online_v3", time.Now()),
		audioSubItem.dropped.Load,
	)
	audioSubItem.dropped.Store(3)
	srv.audioHub.addSub(audioSubItem)

	// 模拟断开流程已从各自订阅表摘除，尚未执行 endClientSession（正等待 <-done）
	srv.unsubscribe(wsSub)
	srv.audioHub.removeSub(audioSubItem)

	// 在此窗口内进程退出执行 FlushRUM + rum.Shutdown
	srv.FlushRUM()
	rum.Shutdown(2 * time.Second)

	// 后续断连流程恢复继续结算，应安全幂等且绝不重复发
	srv.endClientSession(wsSub.session, wsSub.dropped.Load(), time.Now())
	srv.endClientSession(audioSubItem.session, audioSubItem.dropped.Load(), time.Now())

	var wsEnded, audioEnded int
	for _, ev := range col.snapshot() {
		if ev.Event != "client_session" || ev.Properties["action"] != "ended" {
			continue
		}
		switch ev.Properties["channel"] {
		case "ws_player":
			wsEnded++
			if ev.Properties["dropped_frames_or_events"] != float64(2) {
				t.Errorf("ws_player dropped_frames_or_events = %#v，期望 2", ev.Properties["dropped_frames_or_events"])
			}
		case "audio_ws_player":
			audioEnded++
			if ev.Properties["dropped_frames_or_events"] != float64(3) {
				t.Errorf("audio_ws_player dropped_frames_or_events = %#v，期望 3", ev.Properties["dropped_frames_or_events"])
			}
		}
	}
	if wsEnded != 1 || audioEnded != 1 {
		t.Fatalf("已 unsubscribe 但未结算的会话必须被 FlushRUM 结清：wsEnded=%d audioEnded=%d（全部：%#v）",
			wsEnded, audioEnded, col.snapshot())
	}
}
