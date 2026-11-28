package wesing

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/wesing/lyric"
	"Metabox-Nexus-PlayerCap/player/wesing/proc"
	"Metabox-Nexus-PlayerCap/rum"
)

type wesingRUMBatch struct {
	Batch []struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	} `json:"batch"`
}

type wesingRUMEvent struct {
	Event string
	Props map[string]any
}

type wesingRUMCollector struct {
	srv    *httptest.Server
	mu     sync.Mutex
	raw    []string
	events []wesingRUMEvent
}

func startWesingRUMCollector(t *testing.T) *wesingRUMCollector {
	t.Helper()
	c := &wesingRUMCollector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{\"enabled\":true}"}}`)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			body, _ := io.ReadAll(r.Body)
			var parsed wesingRUMBatch
			_ = json.Unmarshal(body, &parsed)
			c.mu.Lock()
			c.raw = append(c.raw, string(body))
			for _, ev := range parsed.Batch {
				c.events = append(c.events, wesingRUMEvent{Event: ev.Event, Props: ev.Properties})
			}
			c.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(c.srv.Close)

	restore := rum.OverrideCredentialsForTest("phc_test_wesing", c.srv.URL)
	t.Cleanup(restore)

	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		HTTPClient: c.srv.Client(),
	})
	rum.Start()
	return c
}

func (c *wesingRUMCollector) flushAndRead() (string, []wesingRUMEvent) {
	rum.Flush(2 * time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	evs := make([]wesingRUMEvent, len(c.events))
	copy(evs, c.events)
	return strings.Join(c.raw, "\n"), evs
}

func assertWesingNoSecrets(t *testing.T, raw string) {
	t.Helper()
	for _, secret := range []string{
		"Secret WeSing Song",
		"Alice WeSing Singer",
		"Secret Lyric Line Content",
		"C:\\Users\\Alice\\Music\\wesing_secret.mp3",
		"Alice",
		"secret_album_cover.jpg",
		"imgcache.qq.com",
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("出网 RUM JSON 泄露了敏感信息 %q：\n%s", secret, raw)
		}
	}
}

// TestWesingRUMSessionAndBlockedEdge 验证 no_admin 阻断与恢复边沿、player_session attached/detached 成对上报。
func TestWesingRUMSessionAndBlockedEdge(t *testing.T) {
	col := startWesingRUMCollector(t)

	of, oo, oc, oe, og := findProcess, openProc, closeProc, enumModules, getPlayState
	t.Cleanup(func() {
		findProcess, openProc, closeProc, enumModules, getPlayState = of, oo, oc, oe, og
	})

	var mu sync.Mutex
	openCalls := 0
	alive := true

	findProcess = func(string) (uint32, error) {
		mu.Lock()
		defer mu.Unlock()
		if !alive {
			return 0, errors.New("not running")
		}
		return 4242, nil
	}
	openProc = func(pid uint32) (syscall.Handle, error) {
		mu.Lock()
		defer mu.Unlock()
		openCalls++
		if openCalls == 1 {
			return 0, errors.New("access denied")
		}
		return syscall.Handle(0x100), nil
	}
	closeProc = func(syscall.Handle) {}
	enumModules = func(uint32) ([]proc.Module, error) {
		return []proc.Module{{Name: "KSongsLyric.dll", Base: 0x1000, Size: 0x1000}}, nil
	}
	getPlayState = func(uint32) proc.PlayState {
		return proc.PlayState{Phase: proc.PhaseStandby}
	}

	p := New(0, 30)
	// 用较短的循环通过直接在独立 goroutine 启动 Start，或验证 blockTracker + runSession。
	// 直接用辅助状态验证 Start 周期会等 2s sleep，太慢；先验证 blockTracker 边沿与 runSession 真实退出：
	var bt wesingBlockTracker
	bt.onBlocked("no_admin")
	bt.onBlocked("no_admin") // 重复阻断不得刷屏
	bt.onRecovered()
	bt.onRecovered() // 重复恢复不得刷屏

	// 驱动一次真实 runSession：连上后立刻令进程退出
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.runSession(syscall.Handle(0x100), 4242, 0, 30*time.Millisecond, &bt, 12)
	}()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	alive = false
	mu.Unlock()
	p.Stop()
	<-done

	raw, evs := col.flushAndRead()
	assertWesingNoSecrets(t, raw)

	var blockedCount, recoveredCount, attachedCount, detachedCount int
	for _, ev := range evs {
		if ev.Props["player"] != PlayerName {
			continue
		}
		switch ev.Event {
		case "player_blocked":
			switch ev.Props["phase"] {
			case "blocked":
				blockedCount++
				if ev.Props["reason"] != "no_admin" {
					t.Errorf("player_blocked reason = %v，期望 no_admin", ev.Props["reason"])
				}
			case "recovered":
				recoveredCount++
				if ev.Props["reason"] != "no_admin" {
					t.Errorf("player_blocked recovered reason = %v，期望 no_admin", ev.Props["reason"])
				}
			}
		case "player_session":
			switch ev.Props["action"] {
			case "attached":
				attachedCount++
				if ev.Props["attach_ms"] != float64(12) {
					t.Errorf("attach_ms = %v，期望 12", ev.Props["attach_ms"])
				}
			case "detached":
				detachedCount++
				cause, _ := ev.Props["disconnect_cause"].(string)
				if cause != "stopped" && cause != "process_exited" {
					t.Errorf("disconnect_cause = %q，期望 stopped 或 process_exited", cause)
				}
			}
		}
	}
	if blockedCount != 1 || recoveredCount != 1 {
		t.Fatalf("player_blocked 边沿去重异常：blocked=%d recovered=%d", blockedCount, recoveredCount)
	}
	if attachedCount != 1 || detachedCount != 1 {
		t.Fatalf("player_session 成对上报异常：attached=%d detached=%d", attachedCount, detachedCount)
	}
}

// TestWesingRUMTrackCompatClockGuardAndCoverScan 验证 track_recognized 防重宣告虚增、compat 信号去重、
// tickJudge 跳转/最小化外推边沿与 startCoverFetch 补扫全链路事件且零隐私泄露。
func TestWesingRUMTrackCompatClockGuardAndCoverScan(t *testing.T) {
	col := startWesingRUMCollector(t)

	beforeTotal := player.TracksRecognizedTotal()

	// 1. track_recognized 与 compat 去重
	sess := newWesingSessionRUM(15)
	lyrics := []lyric.LyricLine{
		{
			Index:    0,
			Time:     1.0,
			Text:     "Secret Lyric Line Content",
			RomaText: "secret roma line",
			Detailed: player.LyricTextDetailed{
				Timestamp: 1.0,
				Words:     []player.LyricTextDetailedWord{{Timestamp: 1.0, Text: "Secret"}},
			},
		},
	}
	// 首轮宣告应记 1 曲并递增全局计数
	sess.onTrackRecognized("pcm", "pcm_path_mid", lyrics, 24)
	// 自愈重宣告不应重复发 track_recognized
	sess.onCompat("lyric_host_failed", "find_host")
	sess.onCompat("lyric_host_failed", "find_host") // 去重
	sess.onCompat("clock_dead_addr", "")
	sess.onCompat("clock_alt_addr", "")
	sess.onCompat("clock_pcm_rate_off", "rate_out_of_range")
	sess.onCompat("clock_pcm_stalled", "")
	sess.onCompat("songs_graph_corrected", "")
	sess.end("process_exited")

	if delta := player.TracksRecognizedTotal() - beforeTotal; delta != 1 {
		t.Fatalf("TracksRecognizedTotal 增量 = %d，期望 1", delta)
	}

	// 2. playback_clock_guard：前跳、回跳、最小化外推（仅首次边沿报 1 次）
	p := newTestPlayer()
	j := newTickJudge()
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	runTick := func(atMs int, raw float32, min bool) {
		o := j.step(raw, t0.Add(time.Duration(atMs)*time.Millisecond), min, false, 200)
		p.emitTick(o, raw, "Secret WeSing Song - Alice WeSing Singer", 200)
	}
	runTick(0, 10.0, false)
	runTick(100, 10.1, false)
	runTick(200, 25.0, false) // 前跳 >2s
	runTick(300, 5.0, false)  // 回跳 >2s
	runTick(400, 5.1, true)   // 首次进入最小化外推 -> 上报 1 次
	runTick(500, 5.1, true)   // 持续最小化 -> 不得刷屏
	runTick(600, 5.1, true)   // 持续最小化 -> 不得刷屏

	// 3. scan_perf_sample：startCoverFetch 补扫 + SupplementDecision + CoverOutcome
	origKeep := player.SupplementField("A", "B") == "A"
	player.SetSongInfoKeepFirst(true)
	t.Cleanup(func() { player.SetSongInfoKeepFirst(origKeep) })

	coverURL := "https://imgcache.qq.com/music/photo/mid_album_800/v/X/secret_album_cover.jpg"
	ops := coverFetchOps{
		openProc:  func(uint32) (syscall.Handle, error) { return syscall.Handle(0x200), nil },
		closeProc: func(syscall.Handle) {},
		findByMID: func(syscall.Handle, string) (lyric.SongInfo, bool) {
			return lyric.SongInfo{
				Name:   "Secret WeSing Song",
				Singer: "Alice WeSing Singer",
				Mid:    testMID,
			}, true
		},
		findAlbum: func(syscall.Handle, string) string { return testAlbumMID },
		downloadWithOutcome: func(u string) (string, player.CoverOutcome) {
			return "data:image/jpeg;base64,AAAA", player.CoverOutcome{
				Action:     player.CoverActionResized800,
				DurationMs: 42,
			}
		},
		sleep: func(time.Duration) {},
	}
	_ = coverURL
	gen := p.NewSongGen()
	done := p.startCoverFetch(ops, gen, 4242, "Secret WeSing Song", testMID, "Secret WeSing Song", "", "Secret WeSing Song", "", true, true)
	waitDone(t, done)

	raw, evs := col.flushAndRead()
	assertWesingNoSecrets(t, raw)

	var gotTrack map[string]any
	var gotScan map[string]any
	compatSignals := map[string]int{}
	guardKinds := map[string]int{}

	for _, ev := range evs {
		switch ev.Event {
		case "track_recognized":
			gotTrack = ev.Props
		case "player_compat_signal":
			sig, _ := ev.Props["signal"].(string)
			compatSignals[sig]++
		case "playback_clock_guard":
			gk, _ := ev.Props["guard_kind"].(string)
			guardKinds[gk]++
		case "scan_perf_sample":
			if ev.Props["scan_kind"] == "wesing_cover_scan" {
				gotScan = ev.Props
			}
		}
	}

	if gotTrack == nil {
		t.Fatal("未收到 wesing track_recognized 事件")
	}
	if gotTrack["player"] != "wesing" ||
		gotTrack["clock_mode"] != "pcm" ||
		gotTrack["id_source"] != "pcm_path_mid" ||
		gotTrack["lyric_source"] != "memory" ||
		gotTrack["lyric_format"] != "memory" ||
		gotTrack["line_count"] != float64(1) ||
		gotTrack["has_verbatim"] != true ||
		gotTrack["has_trans"] != false ||
		gotTrack["has_roma"] != true ||
		gotTrack["has_roma_detailed"] != false ||
		gotTrack["has_furigana"] != false ||
		gotTrack["is_pure_music"] != false {
		t.Errorf("wesing track_recognized 属性异常：%#v", gotTrack)
	}

	for _, wantSig := range []string{
		"lyric_host_failed",
		"clock_dead_addr",
		"clock_alt_addr",
		"clock_pcm_rate_off",
		"clock_pcm_stalled",
		"songs_graph_corrected",
	} {
		if compatSignals[wantSig] != 1 {
			t.Errorf("player_compat_signal[%s] 次数 = %d，期望 1", wantSig, compatSignals[wantSig])
		}
	}

	if guardKinds["seek_forward_2s"] != 1 || guardKinds["seek_back_2s"] != 1 || guardKinds["minimize_extrapolation"] != 1 {
		t.Errorf("playback_clock_guard 计数异常：%#v，期望各为 1", guardKinds)
	}

	if gotScan == nil {
		t.Fatal("未收到 wesing_cover_scan 的 scan_perf_sample 事件")
	}
	if gotScan["player"] != "wesing" ||
		gotScan["verdict"] != "ok" ||
		gotScan["cover_action"] != player.CoverActionResized800 ||
		gotScan["supplement_decision"] != "filled_empty" ||
		gotScan["cover_ms"] != float64(42) {
		t.Errorf("wesing scan_perf_sample 属性异常：%#v", gotScan)
	}
}

// TestWesingFlushRUMIdempotentAndConcurrent 验证 WesingPlayer 实现 player.RUMFlusher，
// 并发多次 FlushRUM 与后续 runSession defer 退出仅发出 1 条 detached(disconnect_cause="stopped")。
func TestWesingFlushRUMIdempotentAndConcurrent(t *testing.T) {
	var _ player.RUMFlusher = (*WesingPlayer)(nil)
	col := startWesingRUMCollector(t)

	p := New(0, 50)
	sess := p.startSessionRUM(18)
	sess.onTrackRecognized("pcm", "pcm_path_mid", []lyric.LyricLine{{Time: 1, Text: "Secret WeSing Line"}}, 20)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.FlushRUM()
		}()
	}
	wg.Wait()
	// 模拟 runSession 后续退出时 defer 调用 end("process_exited")，不得重复上报
	sess.end("process_exited")
	p.FlushRUM()

	raw, evs := col.flushAndRead()
	assertWesingNoSecrets(t, raw)

	var detached []map[string]any
	for _, ev := range evs {
		if ev.Event == "player_session" && ev.Props["action"] == "detached" {
			detached = append(detached, ev.Props)
		}
	}
	if len(detached) != 1 {
		t.Fatalf("FlushRUM 并发及后续退出应仅产生 1 条 detached，实得 %d：%#v", len(detached), detached)
	}
	if detached[0]["disconnect_cause"] != "stopped" || detached[0]["tracks_played"] != float64(1) {
		t.Errorf("detached 属性异常：%#v", detached[0])
	}
}

// TestWesingBlockTrackerClearsOnNotRunning 验证进程未运行清零阻断计时：
// no_admin 阻断 -> 进程未运行 -> 再次 no_admin 阻断 -> 恢复，产出 2 条 blocked 且 blocked_sec 仅计第二次阻断时长。
func TestWesingBlockTrackerClearsOnNotRunning(t *testing.T) {
	col := startWesingRUMCollector(t)

	cur := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	bt := wesingBlockTracker{now: func() time.Time { return cur }}
	bt.onBlocked(player.ReasonNoAdmin)
	cur = cur.Add(5 * time.Second)
	bt.onNotRunning()
	cur = cur.Add(60 * time.Second)
	bt.onBlocked(player.ReasonNoAdmin)
	cur = cur.Add(3 * time.Second)
	bt.onRecovered()

	_, evs := col.flushAndRead()
	var blockedCnt int
	var recovered []map[string]any
	for _, ev := range evs {
		if ev.Event == "player_blocked" && ev.Props["player"] == PlayerName {
			switch ev.Props["phase"] {
			case "blocked":
				blockedCnt++
			case "recovered":
				recovered = append(recovered, ev.Props)
			}
		}
	}
	if blockedCnt != 2 {
		t.Fatalf("跨未运行间隔再次阻断应产生 2 条 blocked，实得 %d", blockedCnt)
	}
	if len(recovered) != 1 || recovered[0]["blocked_sec"] != float64(3) {
		t.Fatalf("recovered 应仅计第二次阻断 3s，实得 %#v", recovered)
	}
}

// TestWesingFlushRUMWaitsForInFlightTrackAndDetachBarrier 回归 code-t4-r2 Finding 2：
// 并发退出时，FlushRUM 必须等待在途 onTrackRecognized 与竞争方 end 完成入队后再返回。
func TestWesingFlushRUMWaitsForInFlightTrackAndDetachBarrier(t *testing.T) {
	col := startWesingRUMCollector(t)

	t.Run("等待在途track_recognized完成", func(t *testing.T) {
		before := player.TracksRecognizedTotal()
		p := New(0, 50)
		sess := p.startSessionRUM(10)
		inHook := make(chan struct{})
		releaseHook := make(chan struct{})
		sess.mu.Lock()
		sess.testBeforeTrackEmit = func() {
			close(inHook)
			<-releaseHook
		}
		sess.mu.Unlock()

		emitDone := make(chan struct{})
		go func() {
			defer close(emitDone)
			sess.onTrackRecognized("pcm", "pcm_path_mid", []lyric.LyricLine{{Time: 1, Text: "Secret WeSing Line"}}, 12)
		}()
		<-inHook

		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			p.FlushRUM()
		}()
		select {
		case <-flushDone:
			t.Fatal("onTrackRecognized 尚未完成 NoteTrackRecognized/rum.Track，FlushRUM 不得提前返回")
		case <-time.After(30 * time.Millisecond):
		}

		close(releaseHook)
		<-flushDone
		<-emitDone
		if delta := player.TracksRecognizedTotal() - before; delta != 1 {
			t.Fatalf("FlushRUM 返回时 TracksRecognizedTotal 增量应已为 1，实得 %d", delta)
		}
	})

	t.Run("等待竞争方detached完成", func(t *testing.T) {
		p := New(0, 50)
		sess := p.startSessionRUM(10)
		inHook := make(chan struct{})
		releaseHook := make(chan struct{})
		sess.mu.Lock()
		sess.testBeforeDetachEmit = func() {
			close(inHook)
			<-releaseHook
		}
		sess.mu.Unlock()

		endDone := make(chan struct{})
		go func() {
			defer close(endDone)
			p.endSessionRUM(sess, "process_exited")
		}()
		<-inHook

		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			p.FlushRUM()
		}()
		select {
		case <-flushDone:
			t.Fatal("竞争方尚未发出 detached，FlushRUM 不得提前返回")
		case <-time.After(30 * time.Millisecond):
		}

		close(releaseHook)
		<-flushDone
		<-endDone
	})

	raw, evs := col.flushAndRead()
	assertWesingNoSecrets(t, raw)
	var detachedCnt int
	for _, ev := range evs {
		if ev.Event == "player_session" && ev.Props["action"] == "detached" {
			detachedCnt++
		}
	}
	if detachedCnt != 2 {
		t.Fatalf("两个子场景各应产生恰好 1 条 detached（共 2 条），实得 %d", detachedCnt)
	}
}

// TestWesingRUMGateRejectsPostFlushAndWaitsAttach 回归测试：
// FlushRUM 置位后拒绝新登记（零孤儿 attached）；登记发 attached 前暂停时并发 FlushRUM 等待成对结算。
func TestWesingRUMGateRejectsPostFlushAndWaitsAttach(t *testing.T) {
	t.Run("Flush后拒绝新会话", func(t *testing.T) {
		col := startWesingRUMCollector(t)
		p := New(0, 50)
		p.FlushRUM()
		sess := p.startSessionRUM(15)
		if sess != nil {
			t.Fatalf("FlushRUM 后 startSessionRUM 应返回 nil，实得 %#v", sess)
		}
		sess.onCompat("clock_alt_addr", "")
		sess.onTrackRecognized("pcm", "pcm_path_mid", []lyric.LyricLine{{Time: 1, Text: "x"}}, 10)
		p.endSessionRUM(sess, "process_exited")

		_, evs := col.flushAndRead()
		for _, ev := range evs {
			if ev.Event == "player_session" || ev.Event == "track_recognized" {
				t.Fatalf("FlushRUM 后拒绝的会话不应发事件：%#v", ev)
			}
		}
	})

	t.Run("登记中并发Flush等待成对结算", func(t *testing.T) {
		col := startWesingRUMCollector(t)
		p := New(0, 50)
		inAttach := make(chan struct{})
		releaseAttach := make(chan struct{})
		p.testBeforeAttach = func() {
			close(inAttach)
			<-releaseAttach
		}

		regDone := make(chan *wesingSessionRUM, 1)
		go func() {
			regDone <- p.startSessionRUM(15)
		}()
		<-inAttach

		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			p.FlushRUM()
		}()
		select {
		case <-flushDone:
			t.Fatal("attached 尚未完成入队，FlushRUM 不得提前返回")
		case <-time.After(35 * time.Millisecond):
		}

		close(releaseAttach)
		sess := <-regDone
		if sess == nil {
			t.Fatal("前置：并发 Flush 前已占位的 startSessionRUM 应返回非 nil 会话")
		}
		<-flushDone
		p.endSessionRUM(sess, "process_exited")

		_, evs := col.flushAndRead()
		var actions []string
		for _, ev := range evs {
			if ev.Event == "player_session" {
				if act, ok := ev.Props["action"].(string); ok {
					actions = append(actions, act)
				}
			}
		}
		if len(actions) != 2 || actions[0] != "attached" || actions[1] != "detached" {
			t.Fatalf("期望严格成对 [attached detached]，实得 %v", actions)
		}
	})
}
