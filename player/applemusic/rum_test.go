package applemusic

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
	"Metabox-Nexus-PlayerCap/rum"
)

type amRUMBatch struct {
	Batch []struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	} `json:"batch"`
}

type amRUMEvent struct {
	Event string
	Props map[string]any
}

type amRUMCollector struct {
	srv    *httptest.Server
	mu     sync.Mutex
	raw    []string
	events []amRUMEvent
}

func startAMRUMCollector(t *testing.T) *amRUMCollector {
	t.Helper()
	c := &amRUMCollector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{\"enabled\":true}"}}`)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			body, _ := io.ReadAll(r.Body)
			var parsed amRUMBatch
			_ = json.Unmarshal(body, &parsed)
			c.mu.Lock()
			c.raw = append(c.raw, string(body))
			for _, ev := range parsed.Batch {
				c.events = append(c.events, amRUMEvent{Event: ev.Event, Props: ev.Properties})
			}
			c.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(c.srv.Close)

	restore := rum.OverrideCredentialsForTest("phc_test_applemusic", c.srv.URL)
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

func (c *amRUMCollector) flushAndRead() (string, []amRUMEvent) {
	rum.Flush(2 * time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	evs := make([]amRUMEvent, len(c.events))
	copy(evs, c.events)
	return strings.Join(c.raw, "\n"), evs
}

func assertAMNoSecrets(t *testing.T, raw string) {
	t.Helper()
	for _, secret := range []string{
		"Secret Apple Song",
		"Alice Apple Artist",
		"Secret Album Name",
		"Secret TTML Word",
		"Secret Translation Text",
		"Late Panel Secret Lyric",
		"C:\\Users\\Alice\\Music\\apple_secret.m4a",
		"Alice",
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("出网 RUM JSON 泄露了敏感信息 %q：\n%s", secret, raw)
		}
	}
}

func makeTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestAMRUMFullSessionTracksLatePanelCompatAndScans 驱动真实 session 验证 Apple Music 全链路 RUM 埋点与零隐私泄露：
// - player_session（attached/detached、tracks_played）
// - player_blocked（no_admin、metadata_unavailable_10s 及恢复）
// - player_compat_signal（twin_lost、twin_refound、twin_candidates_truncated、loading_grace_cleared_6s）
// - track_recognized（逐字+翻译+800x800 封面、以及 15s 判无词）+ TracksRecognizedTotal
// - track_lyric_updated（15s 判无词后晚开面板 #247 late_panel_opened）
// - playback_clock_guard（300ms jumpHold 闪现拦截与稳定放行）
// - scan_perf_sample（am_full_scan）
func TestAMRUMFullSessionTracksLatePanelCompatAndScans(t *testing.T) {
	col := startAMRUMCollector(t)

	// 1. no_admin 阻断与恢复边沿
	var bt amBlockTracker
	bt.onBlocked(player.ReasonNoAdmin)
	bt.onBlocked(player.ReasonNoAdmin) // 去重
	bt.onRecovered()

	beforeTotal := player.TracksRecognizedTotal()

	// 2. 启动带 RUM 统计的会话
	h := newHarness(t)
	h.s.startSessionRUM(14)

	// 构造真实 200x200 JPEG 封面与带敏感文本的 Word+Trans TTML
	cover200 := makeTestJPEG(t, 200, 200)
	song1 := mem.NowPlaying{
		Title:     "Secret Apple Song",
		Artist:    "Alice Apple Artist",
		Album:     "Secret Album Name",
		Full:      true,
		Addr:      0x200000,
		Sig:       []byte("store:secret-song-1"),
		Cover:     cover200,
		CoverMIME: "image/jpeg",
	}
	wordTTML := `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" itunes:timing="Word">` +
		`<head><metadata><iTunesMetadata xmlns="http://music.apple.com/lyric-ttml-internal">` +
		`<translations><translation type="subtitle" xml:lang="zh-Hans"><text for="L1">Secret Translation Text</text></translation></translations>` +
		`</iTunesMetadata></metadata></head>` +
		`<body dur="200.000"><div><p begin="1.000" end="5.000" itunes:key="L1"><span begin="1.000" end="2.500">Secret </span><span begin="2.500" end="4.500">TTML Word</span></p></div></body></tt>`

	h.f.songs = []mem.NowPlaying{song1}
	h.f.docs = []mem.TTMLDoc{docAt(wordTTML, 0x300000)}
	h.setTwin(200, 1, true)
	h.run(4 * time.Second) // 发现 twin、宣告第 1 曲、缩放封面并结算 track_recognized

	// 触发 twin_candidates_truncated 兼容信号
	h.s.setCandidates(mem.Snapshot{Twins: []uintptr{0x9000}, TwinsTruncated: true}, h.now)

	// 触发 twin_lost（0.9s 读失败合成暂停）与 twin_refound（同曲同长找回）
	h.play = false
	delete(h.f.twins, realTwin)
	h.run(1200 * time.Millisecond) // 触发 dropTwin -> twin_lost
	// 换一个地址出现同曲同长 twin，先推进 2.5s 让 twinScan 扫入候选表，再每秒前进 1 格判活 -> twin_refound
	const refoundTwin uintptr = 0x1800
	h.f.twins[refoundTwin] = mem.Twin{Dur: 200, Pos: 10}
	h.run(2500 * time.Millisecond)
	for pos := 11.0; pos <= 14.0; pos++ {
		h.f.twins[refoundTwin] = mem.Twin{Dur: 200, Pos: pos}
		h.run(1000 * time.Millisecond)
	}

	// 触发 6s 慢加载清屏（loading_grace_cleared_6s）
	h.f.twins[refoundTwin] = mem.Twin{Dur: 0, Pos: 0}
	h.run(6500 * time.Millisecond)

	// 触发 10s metadata_unavailable_10s 阻断，然后提供第 2 首标题恢复 -> 15s 判无词 -> 晚开面板救回 (#247)
	h.f.songs = nil
	h.f.docs = nil
	h.f.twins[refoundTwin] = mem.Twin{Dur: 180, Pos: 0}
	h.run(10500 * time.Millisecond) // 触发 metadata_unavailable_10s

	song2 := mem.NowPlaying{
		Title:  `C:\Users\Alice\Music\apple_secret.m4a`,
		Artist: "Alice Apple Artist",
		Album:  "Secret Album Name",
		Full:   true,
		Addr:   0x210000,
		Sig:    []byte("store:secret-song-2"),
	}
	h.f.songs = []mem.NowPlaying{song2}
	h.run(16 * time.Second) // 恢复 metadata_unavailable_10s -> 宣告第 2 首 -> 15s 判无词并结算 track_recognized(none)

	// 15s 判无词后晚开面板，TTML 出现 -> 触发 track_lyric_updated{reason: "late_panel_opened"}
	lateTTML := `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" itunes:timing="Line"><body dur="180.000"><div><p begin="2.000" end="5.000" itunes:key="L1">Late Panel Secret Lyric</p></div></body></tt>`
	h.f.docs = []mem.TTMLDoc{docAt(lateTTML, 0x310000)}
	h.run(16 * time.Second) // 晚到退避扫到并救回

	// 结束会话
	h.s.endSessionRUM("process_exited")

	if delta := player.TracksRecognizedTotal() - beforeTotal; delta != 2 {
		t.Fatalf("TracksRecognizedTotal 增量 = %d，期望 2", delta)
	}

	// 3. 300ms jumpHold：闪现拦截 (intercepted) 与 300ms 稳定放行 (released)
	var clk clock
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clk.reset(50, 50.5, t0, true, 240)
	// 闪现目标 120 持续 100ms 后弹回 51 -> intercepted
	clk.observe(120, t0.Add(50*time.Millisecond))
	clk.observe(120, t0.Add(150*time.Millisecond))
	clk.observe(51, t0.Add(200*time.Millisecond))
	// 稳定目标 160 持续 >=300ms -> released
	clk.observe(160, t0.Add(300*time.Millisecond))
	clk.observe(160, t0.Add(450*time.Millisecond))
	if ev := clk.observe(160, t0.Add(650*time.Millisecond)); ev != evSeek {
		t.Fatalf("期望 300ms 后确认 evSeek，实得 %v", ev)
	}

	raw, evs := col.flushAndRead()
	assertAMNoSecrets(t, raw)

	blockedReasons := map[string]int{}
	recoveredReasons := map[string]int{}
	compatSignals := map[string]int{}
	guardOutcomes := map[string]int{}
	var tracks []map[string]any
	var lateUpdates []map[string]any
	var scans int
	var attached, detached map[string]any

	for _, ev := range evs {
		if ev.Props["player"] != PlayerName {
			continue
		}
		switch ev.Event {
		case "player_session":
			if ev.Props["action"] == "attached" {
				attached = ev.Props
			} else if ev.Props["action"] == "detached" {
				detached = ev.Props
			}
		case "player_blocked":
			r, _ := ev.Props["reason"].(string)
			if ev.Props["phase"] == "blocked" {
				blockedReasons[r]++
			} else if ev.Props["phase"] == "recovered" {
				recoveredReasons[r]++
			}
		case "player_compat_signal":
			sig, _ := ev.Props["signal"].(string)
			compatSignals[sig]++
		case "track_recognized":
			tracks = append(tracks, ev.Props)
		case "track_lyric_updated":
			lateUpdates = append(lateUpdates, ev.Props)
		case "playback_clock_guard":
			if ev.Props["guard_kind"] == "jump_hold_300ms" {
				o, _ := ev.Props["outcome"].(string)
				guardOutcomes[o]++
			}
		case "scan_perf_sample":
			if ev.Props["scan_kind"] == "am_full_scan" {
				scans++
			}
		}
	}

	if attached == nil || detached == nil || detached["tracks_played"] != float64(2) {
		t.Errorf("player_session 异常：attached=%#v detached=%#v", attached, detached)
	}
	if blockedReasons["no_admin"] != 1 || recoveredReasons["no_admin"] != 1 ||
		blockedReasons["metadata_unavailable_10s"] != 1 || recoveredReasons["metadata_unavailable_10s"] != 1 {
		t.Errorf("player_blocked 边沿异常：blocked=%v recovered=%v", blockedReasons, recoveredReasons)
	}
	for _, sig := range []string{"twin_lost", "twin_refound", "twin_candidates_truncated", "loading_grace_cleared_6s"} {
		if compatSignals[sig] != 1 {
			t.Errorf("player_compat_signal[%s] = %d，期望 1（实得 %v）", sig, compatSignals[sig], compatSignals)
		}
	}
	if len(tracks) != 2 {
		t.Fatalf("track_recognized 条数 = %d，期望 2（%#v）", len(tracks), tracks)
	}
	t1 := tracks[0]
	if t1["clock_mode"] != "memory_twin" ||
		t1["lyric_source"] != "memory_ttml" ||
		t1["lyric_format"] != "ttml" ||
		t1["line_count"] != float64(1) ||
		t1["has_verbatim"] != true ||
		t1["has_trans"] != true ||
		t1["has_roma"] != false ||
		t1["has_roma_detailed"] != false ||
		t1["has_furigana"] != false ||
		t1["is_pure_music"] != false ||
		t1["cover_action"] != player.CoverActionResized800 {
		t.Errorf("第 1 曲 track_recognized 属性异常：%#v", t1)
	}
	t2 := tracks[1]
	if t2["lyric_source"] != "none" || t2["lyric_format"] != "none" || t2["line_count"] != float64(0) {
		t.Errorf("第 2 曲无词 track_recognized 属性异常：%#v", t2)
	}
	if len(lateUpdates) != 1 || lateUpdates[0]["reason"] != "late_panel_opened" {
		t.Errorf("晚开面板 track_lyric_updated 异常：%#v", lateUpdates)
	}
	if guardOutcomes["intercepted"] != 1 || guardOutcomes["released"] != 1 {
		t.Errorf("jump_hold_300ms outcome 异常：%v", guardOutcomes)
	}
	if scans == 0 {
		t.Error("未收到 am_full_scan 的 scan_perf_sample 样本")
	}
}

// TestAMFlushRUMSettlesPendingAndIdempotent 验证 AppleMusicPlayer 实现 player.RUMFlusher，
// 退出冲刷时与轮询状态无数据竞争、先结清待报曲目再发出单次 detached(disconnect_cause="stopped")。
func TestAMFlushRUMSettlesPendingAndIdempotent(t *testing.T) {
	var _ player.RUMFlusher = (*AppleMusicPlayer)(nil)
	col := startAMRUMCollector(t)
	before := player.TracksRecognizedTotal()

	h := newHarness(t)
	h.s.startSessionRUM(12)

	// 宣告一首无歌词（尚在 15s 宽限期未结算）的曲目
	h.f.songs = []mem.NowPlaying{{
		Title:  "Secret Pending AM Track",
		Artist: "Alice AM Singer",
		Album:  "Secret Album",
		Full:   true,
		Addr:   0x200000,
		Sig:    []byte("pending-sig"),
	}}
	h.setTwin(200, 1, true)
	h.run(4 * time.Second)
	if !h.s.announced {
		t.Fatal("前置：曲目应已宣告")
	}
	if player.TracksRecognizedTotal()-before != 0 {
		t.Fatalf("前置：15s 宽限期内曲目尚未结算，实得增量 %d", player.TracksRecognizedTotal()-before)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.p.FlushRUM()
		}()
	}
	wg.Wait()
	// 后续 runSession defer 退出不得重复上报
	h.s.endSessionRUM("process_exited")
	h.p.FlushRUM()

	if delta := player.TracksRecognizedTotal() - before; delta != 1 {
		t.Fatalf("FlushRUM 应结清 1 首待报曲目，实得增量 %d", delta)
	}

	raw, evs := col.flushAndRead()
	assertAMNoSecrets(t, raw)

	var trackIdx, detachedIdx = -1, -1
	var detachedCnt int
	var detachedProps map[string]any
	for i, ev := range evs {
		if ev.Event == "track_recognized" {
			trackIdx = i
		}
		if ev.Event == "player_session" && ev.Props["action"] == "detached" {
			detachedCnt++
			detachedIdx = i
			detachedProps = ev.Props
		}
	}
	if detachedCnt != 1 || trackIdx < 0 || trackIdx > detachedIdx {
		t.Fatalf("FlushRUM 应先发 track_recognized(%d) 再发唯一 detached(%d, cnt=%d)", trackIdx, detachedIdx, detachedCnt)
	}
	if detachedProps["disconnect_cause"] != "stopped" || detachedProps["tracks_played"] != float64(1) {
		t.Errorf("detached 属性异常：%#v", detachedProps)
	}
}

// TestAMStaleInFlightCoverDoesNotBlockNextTrackRecognized 回归 Finding 4：
// 歌曲 A 封面缩放在飞时切到无封面但歌词已就绪的歌曲 B，随后放行 A 的封面，歌曲 B 必须完成 track_recognized 结算。
func TestAMStaleInFlightCoverDoesNotBlockNextTrackRecognized(t *testing.T) {
	col := startAMRUMCollector(t)
	before := player.TracksRecognizedTotal()

	h := newHarness(t)
	g := useGatedCover(t, h)
	h.s.startSessionRUM(10)

	ttmlA := `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" itunes:timing="Line"><body dur="210.000"><div><p begin="1.000" end="5.000" itunes:key="L1">Secret AM Lyric A</p></div></body></tt>`
	h.f.songs = []mem.NowPlaying{{
		Title:     "Secret AM Song",
		Artist:    "Alice AM Singer",
		Album:     "Album A",
		Full:      true,
		Addr:      0x200000,
		Cover:     []byte("cover-A-bytes"),
		CoverMIME: "image/jpeg",
		Sig:       []byte("sig-A"),
	}}
	h.f.docs = []mem.TTMLDoc{docAt(ttmlA, 0x400000)}
	h.setTwin(210, 1, true)
	h.run(4 * time.Second)

	if len(g.inputs(h)) != 1 || !h.s.coverFly {
		t.Fatalf("前置：歌曲 A 封面应在飞阻塞，calls=%v coverFly=%v", g.inputs(h), h.s.coverFly)
	}

	// 切到歌曲 B：无封面（Cover=nil），带有效 TTML 歌词
	ttmlB := `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" itunes:timing="Line"><body dur="185.000"><div><p begin="1.000" end="5.000" itunes:key="L1">Secret AM Lyric B</p></div></body></tt>`
	h.f.songs = []mem.NowPlaying{{
		Title:  "Secret AM Song B",
		Artist: "Alice AM Singer",
		Album:  "Album B",
		Full:   true,
		Addr:   0x210000,
		Sig:    []byte("sig-B"),
	}}
	h.f.docs = []mem.TTMLDoc{docAt(ttmlB, 0x410000)}
	h.setTwin(185, 1, true)
	h.run(2 * time.Second)

	// 放行歌曲 A 的在飞封面并推进 tick
	g.finish(h)
	h.run(1 * time.Second)

	// 不结束会话也不再切歌，歌曲 B 此时必须已经结算 track_recognized（加上切歌冲刷的 A 共 2 首）
	if delta := player.TracksRecognizedTotal() - before; delta != 2 {
		t.Fatalf("歌曲 A 封面在飞不应阻塞无封面歌曲 B 结算：TracksRecognizedTotal 增量 = %d，期望 2", delta)
	}

	raw, evs := col.flushAndRead()
	assertAMNoSecrets(t, raw)

	var tracks []map[string]any
	for _, ev := range evs {
		if ev.Event == "track_recognized" && ev.Props["player"] == PlayerName {
			tracks = append(tracks, ev.Props)
		}
	}
	if len(tracks) != 2 {
		t.Fatalf("期望收到 A 与 B 共 2 条 track_recognized，实得 %d：%#v", len(tracks), tracks)
	}
	if tracks[1]["lyric_source"] != "memory_ttml" || tracks[1]["cover_action"] != player.CoverActionNone {
		t.Errorf("歌曲 B track_recognized 属性异常：%#v", tracks[1])
	}
}

// TestAMBlockTrackerClearsOnNotRunning 验证进程未运行清零阻断计时：
// no_admin 阻断 -> 进程未运行 -> 再次 no_admin 阻断 -> 恢复，产出 2 条 blocked 且 blocked_sec 仅计第二次阻断时长。
func TestAMBlockTrackerClearsOnNotRunning(t *testing.T) {
	col := startAMRUMCollector(t)

	cur := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	bt := amBlockTracker{now: func() time.Time { return cur }}
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

// TestAMLatePanelTTMLWhileCoverInFlightUpdatesTrackRecognized 回归 code-t4-r2 Finding 1：
// 封面缩放在飞且 15s 判无词之后，晚到的普通 TTML 歌词须更新尚未发出的待报识曲快照，
// 放行封面后唯一 track_recognized 的 lyric_source 为 memory_ttml、line_count 准确，同时保留 late_panel_opened。
func TestAMLatePanelTTMLWhileCoverInFlightUpdatesTrackRecognized(t *testing.T) {
	col := startAMRUMCollector(t)
	before := player.TracksRecognizedTotal()

	h := newHarness(t)
	g := useGatedCover(t, h)
	h.s.startSessionRUM(10)

	h.f.songs = []mem.NowPlaying{{
		Title:     "Secret Late Cover Song",
		Artist:    "Alice AM Singer",
		Album:     "Secret Album",
		Full:      true,
		Addr:      0x200000,
		Cover:     []byte("slow-cover-bytes"),
		CoverMIME: "image/jpeg",
		Sig:       []byte("sig-late-cover"),
	}}
	h.setTwin(220, 1, true)
	h.run(4 * time.Second)
	if !h.s.announced || len(g.inputs(h)) != 1 || !h.s.coverFly {
		t.Fatalf("前置：曲目应已宣告且封面在飞，announced=%v calls=%v coverFly=%v", h.s.announced, g.inputs(h), h.s.coverFly)
	}

	// 推进超过 15s 宽限期触发 emitVerdict（封面仍在飞，因此 track_recognized 尚未发出）
	h.run(15 * time.Second)
	if !h.s.verdictSent {
		t.Fatal("前置：15s 后 verdictSent 应为 true")
	}
	if delta := player.TracksRecognizedTotal() - before; delta != 0 {
		t.Fatalf("前置：封面仍在飞时不应提前结算，实得增量 %d", delta)
	}

	// 注入晚到的 2 行普通 TTML 歌词并推进至晚到退避扫到应用
	lateTTML := `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" itunes:timing="Line"><body dur="220.000"><div>` +
		`<p begin="1.000" end="4.000" itunes:key="L1">Late Panel Secret Lyric</p>` +
		`<p begin="5.000" end="8.000" itunes:key="L2">Late Panel Line Two</p>` +
		`</div></body></tt>`
	h.f.docs = []mem.TTMLDoc{docAt(lateTTML, 0x420000)}
	h.run(16 * time.Second)
	if len(h.s.song.lyrics) != 2 {
		t.Fatalf("前置：晚到 TTML 应已应用 2 行歌词，实得 %d", len(h.s.song.lyrics))
	}

	// 放行封面完成结算
	g.finish(h)
	h.run(1 * time.Second)

	if delta := player.TracksRecognizedTotal() - before; delta != 1 {
		t.Fatalf("放行封面后应仅结算 1 首，实得增量 %d", delta)
	}

	raw, evs := col.flushAndRead()
	assertAMNoSecrets(t, raw)

	var tracks []map[string]any
	var lateUpdates []map[string]any
	for _, ev := range evs {
		if ev.Props["player"] != PlayerName {
			continue
		}
		switch ev.Event {
		case "track_recognized":
			tracks = append(tracks, ev.Props)
		case "track_lyric_updated":
			if ev.Props["reason"] == "late_panel_opened" {
				lateUpdates = append(lateUpdates, ev.Props)
			}
		}
	}
	if len(tracks) != 1 {
		t.Fatalf("期望 1 条 track_recognized，实得 %d：%#v", len(tracks), tracks)
	}
	if tracks[0]["lyric_source"] != "memory_ttml" || tracks[0]["lyric_format"] != "ttml" || tracks[0]["line_count"] != float64(2) {
		t.Errorf("晚到歌词未同步到待报识曲快照：%#v", tracks[0])
	}
	if len(lateUpdates) != 1 {
		t.Errorf("期望同时收到 1 条 late_panel_opened，实得 %d", len(lateUpdates))
	}
}

// TestAMFlushRUMWaitsForInFlightTrackAndDetachBarrier 回归 code-t4-r2 Finding 2：
// 并发退出时，FlushRUM 必须等待在途 emitPending（NoteTrackRecognized + rum.Track）与竞争方 end 完成 detached 后才返回。
func TestAMFlushRUMWaitsForInFlightTrackAndDetachBarrier(t *testing.T) {
	col := startAMRUMCollector(t)

	// 场景 A：emitPending 在锁外入队前暂停，并发 FlushRUM 必须阻塞直到 NoteTrackRecognized 与 detached 均完成
	t.Run("等待在途track_recognized完成", func(t *testing.T) {
		before := player.TracksRecognizedTotal()
		p := New(0, 50)
		sess := &session{p: p, now: time.Now, rum: newAMSessionRUM(time.Now)}
		r := p.registerSessionRUM(sess, 0)
		r.mu.Lock()
		r.pending = &amPendingTrack{
			lyricSource: "memory_ttml",
			lyricFormat: "ttml",
			lineCount:   1,
			coverOut:    player.CoverOutcome{Action: player.CoverActionNone},
		}
		inHook := make(chan struct{})
		releaseHook := make(chan struct{})
		r.testBeforeTrackEmit = func() {
			close(inHook)
			<-releaseHook
		}
		r.mu.Unlock()

		emitDone := make(chan struct{})
		go func() {
			defer close(emitDone)
			r.emitPending()
		}()
		<-inHook

		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			p.FlushRUM()
		}()
		select {
		case <-flushDone:
			t.Fatal("emitPending 尚未完成 NoteTrackRecognized/rum.Track，FlushRUM 不得提前返回")
		case <-time.After(30 * time.Millisecond):
		}

		close(releaseHook)
		<-flushDone
		<-emitDone
		if delta := player.TracksRecognizedTotal() - before; delta != 1 {
			t.Fatalf("FlushRUM 返回时 TracksRecognizedTotal 增量应已为 1，实得 %d", delta)
		}
	})

	// 场景 B：竞争方 end 在 detached 入队前暂停，并发 FlushRUM 必须等待其完成后再返回且零重复
	t.Run("等待竞争方detached完成", func(t *testing.T) {
		p := New(0, 50)
		sess := &session{p: p, now: time.Now, rum: newAMSessionRUM(time.Now)}
		r := p.registerSessionRUM(sess, 0)
		r.mu.Lock()
		inHook := make(chan struct{})
		releaseHook := make(chan struct{})
		r.testBeforeDetachEmit = func() {
			close(inHook)
			<-releaseHook
		}
		r.mu.Unlock()

		endDone := make(chan struct{})
		go func() {
			defer close(endDone)
			p.endSessionRUM(r, "process_exited")
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

	_, evs := col.flushAndRead()
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

// TestAMRUMGateRejectsPostFlushAndNilSafe 回归测试：
// 1) FlushRUM 置位后新登记被拒绝、s.rum 置 nil 且 session 各 RUM 方法全部 nil 安全不 panic；
// 2) 登记发 attached 前暂停时并发 FlushRUM 等待成对产出 attached + detached。
func TestAMRUMGateRejectsPostFlushAndNilSafe(t *testing.T) {
	t.Run("Flush后拒绝新会话且session方法nil安全", func(t *testing.T) {
		col := startAMRUMCollector(t)
		h := newHarness(t)
		h.p.FlushRUM()

		rumSess := h.p.registerSessionRUM(h.s, 10)
		if rumSess != nil || h.s.rum != nil {
			t.Fatalf("FlushRUM 后 registerSessionRUM 应返回 nil 并清零 s.rum，实得 rumSess=%v s.rum=%v", rumSess, h.s.rum)
		}
		// 逐一调用 session 上所有 RUM 相关方法确认 nil 安全
		h.s.resetTrackRUM(false)
		h.s.emitCompat("twin_lost", 0)
		h.s.onMetadataBlocked(time.Now())
		h.s.onMetadataRecovered(time.Now())
		h.s.emitLatePanelLyricUpdate(time.Now())
		h.s.tryEmitTrackRecognized()
		h.s.flushTrackRecognized()
		h.p.endSessionRUM(rumSess, "process_exited")

		_, evs := col.flushAndRead()
		for _, ev := range evs {
			if ev.Event == "player_session" || ev.Event == "track_recognized" || ev.Event == "player_compat_signal" || ev.Event == "player_blocked" {
				t.Fatalf("FlushRUM 后拒绝的会话不应发事件：%#v", ev)
			}
		}
	})

	t.Run("登记中并发Flush等待成对结算", func(t *testing.T) {
		col := startAMRUMCollector(t)
		h := newHarness(t)
		inAttach := make(chan struct{})
		releaseAttach := make(chan struct{})
		h.p.testBeforeAttach = func() {
			close(inAttach)
			<-releaseAttach
		}

		regDone := make(chan *amSessionRUM, 1)
		go func() {
			regDone <- h.p.registerSessionRUM(h.s, 12)
		}()
		<-inAttach

		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			h.p.FlushRUM()
		}()
		select {
		case <-flushDone:
			t.Fatal("attached 尚未完成入队，FlushRUM 不得提前返回")
		case <-time.After(35 * time.Millisecond):
		}

		close(releaseAttach)
		rumSess := <-regDone
		if rumSess == nil {
			t.Fatal("前置：并发 Flush 前已占位的 registerSessionRUM 应返回非 nil")
		}
		<-flushDone
		h.p.endSessionRUM(rumSess, "process_exited")

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
