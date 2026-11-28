package cloudmusic

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/cdp"
	"Metabox-Nexus-PlayerCap/player/cloudmusic/lyric"
	"Metabox-Nexus-PlayerCap/rum"
)

type cmRUMBatch struct {
	Batch []struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	} `json:"batch"`
}

type cmRUMEvent struct {
	Event string
	Props map[string]any
}

type cmRUMCollector struct {
	srv    *httptest.Server
	mu     sync.Mutex
	raw    []string
	events []cmRUMEvent
}

func startCMRUMCollector(t *testing.T) *cmRUMCollector {
	t.Helper()
	c := &cmRUMCollector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{\"enabled\":true}"}}`)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			body, _ := io.ReadAll(r.Body)
			var parsed cmRUMBatch
			_ = json.Unmarshal(body, &parsed)
			c.mu.Lock()
			c.raw = append(c.raw, string(body))
			for _, ev := range parsed.Batch {
				c.events = append(c.events, cmRUMEvent{Event: ev.Event, Props: ev.Properties})
			}
			c.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(c.srv.Close)

	restore := rum.OverrideCredentialsForTest("phc_test_cloudmusic", c.srv.URL)
	t.Cleanup(restore)

	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		HTTPClient: c.srv.Client(),
	})
	rum.Start()
	t.Cleanup(func() { rum.Shutdown(time.Second) })
	return c
}

func (c *cmRUMCollector) flushAndRead() (string, []cmRUMEvent) {
	rum.Flush(2 * time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	evs := make([]cmRUMEvent, len(c.events))
	copy(evs, c.events)
	return strings.Join(c.raw, "\n"), evs
}

func assertCMNoSecrets(t *testing.T, raw string) {
	t.Helper()
	for _, secret := range []string{
		"Secret NetEase Song",
		"Alice NetEase Singer",
		"你好世界",
		"C:\\Users\\Alice\\AppData\\cloudmusic.exe",
		"Alice",
		"p1.music.126.net",
		"secret_netease_cover.jpg",
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("出网 RUM JSON 泄露了敏感信息 %q：\n%s", secret, raw)
		}
	}
}

// TestCloudMusicRUMBlockedStoreZeroPath 验证 store_install 零路径、v2 version_unsupported、
// 以及 onNotRunning 静默清零不误发 recovered 且不跨未运行间隔累计 blocked_sec。
func TestCloudMusicRUMBlockedStoreZeroPath(t *testing.T) {
	col := startCMRUMCollector(t)

	now := time.Date(2026, 11, 27, 11, 0, 0, 0, time.UTC)
	bt := cmBlockTracker{now: func() time.Time { return now }}

	bt.onBlocked("store_install", "")
	bt.onBlocked("store_install", "") // 同因去重
	now = now.Add(40 * time.Second)
	bt.onNotRunning() // 静默清零，不发 recovered

	now = now.Add(15 * time.Second)
	bt.onBlocked("version_unsupported", "2.10.13")
	now = now.Add(8 * time.Second)
	bt.onRecovered()

	raw, evs := col.flushAndRead()
	assertCMNoSecrets(t, raw)

	var storeBlocks, verBlocks, verRecovered int
	for _, ev := range evs {
		if ev.Event != "player_blocked" {
			continue
		}
		if _, hasExePath := ev.Props["exe_path"]; hasExePath {
			t.Errorf("player_blocked 严禁携带 exe_path：%#v", ev.Props)
		}
		if ev.Props["reason"] == "store_install" && ev.Props["phase"] == "blocked" {
			storeBlocks++
		}
		if ev.Props["reason"] == "version_unsupported" && ev.Props["phase"] == "blocked" {
			verBlocks++
			if ev.Props["player_version"] != "2.10.13" {
				t.Errorf("version_unsupported player_version 异常：%#v", ev.Props)
			}
		}
		if ev.Props["reason"] == "version_unsupported" && ev.Props["phase"] == "recovered" {
			verRecovered++
			if ev.Props["blocked_sec"] != float64(8) {
				t.Errorf("version_unsupported blocked_sec 应为 8，实得 %#v", ev.Props["blocked_sec"])
			}
		}
	}
	if storeBlocks != 1 || verBlocks != 1 || verRecovered != 1 {
		t.Fatalf("阻断事件计数异常：store=%d ver=%d rec=%d", storeBlocks, verBlocks, verRecovered)
	}
}

// TestCloudMusicRUMTrackRomaDetailedDOMSyncAndCompat 验证一字一音逐字音译标记、DOM 200ms 校准、
// Fiber/Redux 失败兼容信号、退出 FlushRUM 完成屏障与零敏感泄露。
func TestCloudMusicRUMTrackRomaDetailedDOMSyncAndCompat(t *testing.T) {
	col := startCMRUMCollector(t)

	// 构造一字一音粤语逐字音译样本（经真实 resolveCDPLyrics 验证 has_roma_detailed=true）
	lrc := "[00:01.00]你好\n"
	tlyric := "[00:01.00]Hello\n"
	romalrc := "[00:01.00]nei5 hou2\n"
	yrc := "[1000,1000](1000,500,0)你(1500,500,0)好\n"
	parsed, ok := resolveCDPLyrics(lrc, tlyric, romalrc, yrc, 0)
	if !ok {
		t.Fatal("resolveCDPLyrics 解析失败")
	}
	extracted := toExtracted(parsed)

	beforeTotal := player.TracksRecognizedTotal()
	p := New(0, 100)
	var _ player.RUMFlusher = p

	sess := p.startSessionRUM("3.1.38", 35)

	// 1. 首轮识曲（含逐字音译）
	sess.beginTrack(1)
	sess.bindCoverGen(1, 10, true)
	sess.onTrackLyricSettled(1, "cdp", extracted, false, 120)
	sess.onCoverOutcome(10, player.CoverOutcome{Action: player.CoverActionResized800, DurationMs: 45})

	// 2. DOM 200ms 校准信号
	emitCMDOMSyncGuard(0.26)

	// 3. Fiber/Redux 提取失败脱敏兼容信号（限频 1 次）
	sess.onCompat("fiber_redux_extract_failed", classifyExtractErrCode(errors.New("null: store not found at C:\\Users\\Alice\\AppData\\cloudmusic.exe")))
	sess.onCompat("fiber_redux_extract_failed", classifyExtractErrCode(errors.New("null: store not found again")))

	// 4. 退出结算完成屏障测试：在途曲目完成后才发 detached
	inTrack := make(chan struct{})
	releaseTrack := make(chan struct{})
	sess.mu.Lock()
	sess.testBeforeTrackEmit = func() {
		close(inTrack)
		<-releaseTrack
	}
	sess.mu.Unlock()

	sess.beginTrack(2)
	sess.bindCoverGen(2, 20, false)
	go sess.onTrackLyricSettled(2, "redux", []cdp.ExtractedLyric{{Text: "line", Time: 1}}, false, 80)
	<-inTrack

	flushDone := make(chan struct{})
	go func() {
		p.FlushRUM()
		close(flushDone)
	}()
	select {
	case <-flushDone:
		t.Fatal("FlushRUM 在途 track_recognized 完成前提前返回，违背完成屏障")
	case <-time.After(40 * time.Millisecond):
	}
	close(releaseTrack)
	select {
	case <-flushDone:
	case <-time.After(2 * time.Second):
		t.Fatal("释放屏障后 FlushRUM 超时未返回")
	}
	// 幂等重复调用
	p.FlushRUM()
	sess.end("cdp_closed")

	raw, evs := col.flushAndRead()
	assertCMNoSecrets(t, raw)

	if player.TracksRecognizedTotal() < beforeTotal+2 {
		t.Fatalf("TracksRecognizedTotal 未按 2 首递增：before=%d after=%d", beforeTotal, player.TracksRecognizedTotal())
	}

	var sawYRC, sawDOMGuard, sawCompat, sawDetached int
	for _, ev := range evs {
		switch ev.Event {
		case "track_recognized":
			if ev.Props["lyric_source"] == "cdp" {
				sawYRC++
				if ev.Props["clock_mode"] != "cdp_dom_sync" ||
					ev.Props["lyric_format"] != "yrc" ||
					ev.Props["has_verbatim"] != true ||
					ev.Props["has_trans"] != true ||
					ev.Props["has_roma"] != true ||
					ev.Props["has_roma_detailed"] != true ||
					ev.Props["has_furigana"] != false ||
					ev.Props["cover_action"] != player.CoverActionResized800 {
					t.Errorf("track_recognized 一字一音能力字段异常：%#v", ev.Props)
				}
			}
		case "playback_clock_guard":
			if ev.Props["guard_kind"] == "dom_sync_200ms" && ev.Props["outcome"] == "calibrated" && ev.Props["delta_ms"] == float64(260) {
				sawDOMGuard++
			}
		case "player_compat_signal":
			if ev.Props["signal"] == "fiber_redux_extract_failed" {
				sawCompat++
				if ev.Props["detail_code"] != "store_not_found" {
					t.Errorf("fiber_redux_extract_failed detail_code 异常：%#v", ev.Props)
				}
			}
		case "player_session":
			if ev.Props["action"] == "detached" {
				sawDetached++
				if ev.Props["tracks_played"] != float64(2) || ev.Props["disconnect_cause"] != "stopped" {
					t.Errorf("detached 属性异常：%#v", ev.Props)
				}
			}
		}
	}
	if sawYRC != 1 || sawDOMGuard != 1 || sawCompat != 1 || sawDetached != 1 {
		t.Fatalf("事件计数异常：yrc=%d domGuard=%d compat=%d detached=%d", sawYRC, sawDOMGuard, sawCompat, sawDetached)
	}

	// 确保 lyric 包被编译检查无回归
	_ = lyric.LyricLine{}
}
