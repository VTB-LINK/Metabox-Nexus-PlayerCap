package kugou

import (
	"context"
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
	"Metabox-Nexus-PlayerCap/player/kugou/assoc"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
	"Metabox-Nexus-PlayerCap/player/kugou/watchdog"
	"Metabox-Nexus-PlayerCap/rum"
)

type kugouRUMBatch struct {
	Batch []struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	} `json:"batch"`
}

type kugouRUMEvent struct {
	Event string
	Props map[string]any
}

type kugouRUMCollector struct {
	srv    *httptest.Server
	mu     sync.Mutex
	raw    []string
	events []kugouRUMEvent
}

func startKugouRUMCollector(t *testing.T) *kugouRUMCollector {
	t.Helper()
	c := &kugouRUMCollector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{\"enabled\":true}"}}`)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			body, _ := io.ReadAll(r.Body)
			var parsed kugouRUMBatch
			_ = json.Unmarshal(body, &parsed)
			c.mu.Lock()
			c.raw = append(c.raw, string(body))
			for _, ev := range parsed.Batch {
				c.events = append(c.events, kugouRUMEvent{Event: ev.Event, Props: ev.Properties})
			}
			c.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(c.srv.Close)

	restore := rum.OverrideCredentialsForTest("phc_test_kugou", c.srv.URL)
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

func (c *kugouRUMCollector) flushAndRead() (string, []kugouRUMEvent) {
	rum.Flush(2 * time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	evs := make([]kugouRUMEvent, len(c.events))
	copy(evs, c.events)
	return strings.Join(c.raw, "\n"), evs
}

func assertKugouNoSecrets(t *testing.T, raw string) {
	t.Helper()
	for _, secret := range []string{
		"Secret Kugou Song",
		"Alice Kugou Singer",
		"Secret Kugou Lyric Line",
		"C:\\Users\\Alice\\AppData\\KuGou.exe",
		"Alice",
		"kugou_secret_cover.jpg",
		"imge.kugou.com",
		"SUPER_SECRET_STATUS_RAW",
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("出网 RUM JSON 泄露了敏感信息 %q：\n%s", secret, raw)
		}
	}
}

type stubKugouLocal struct {
	ch chan assoc.Result
}

func (s *stubKugouLocal) SetHash(string)               {}
func (s *stubKugouLocal) Results() <-chan assoc.Result { return s.ch }

// TestKugouRUMBlockedNoPathAndLibcefMismatch 验证 store_install 零路径、libcef_mismatch 带 1..9 指纹位、
// 以及 onNotRunning 静默清空阻断计时避免跨未运行间隔误去重。
func TestKugouRUMBlockedNoPathAndLibcefMismatch(t *testing.T) {
	col := startKugouRUMCollector(t)

	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	bt := kugouBlockTracker{now: func() time.Time { return now }}

	// 1. store_install：绝不带 exe_path
	bt.onBlocked("store_install", "", 0)
	bt.onBlocked("store_install", "", 0) // 同因去重

	// 2. 进程未运行时静默清零（不发 recovered）
	now = now.Add(60 * time.Second)
	bt.onNotRunning()

	// 3. 再次检测到同因 store_install，因已清零应重新发 blocked 且从新时刻算 blocked_sec
	now = now.Add(10 * time.Second)
	bt.onBlocked("store_install", "", 0)
	now = now.Add(5 * time.Second)
	bt.onRecovered()

	// 4. libcef_mismatch 带 1..9 指纹位与脱敏版本号
	bt.onBlocked("libcef_mismatch", "20.2.0.12345", 5)
	now = now.Add(12 * time.Second)
	bt.onRecovered()

	// 5. VersionBlockError 映射校验
	vbe := &watchdog.VersionBlockError{
		Version:          "20.3.1.999",
		MismatchIdx:      7,
		IsLibcefMismatch: true,
		Err:              watchdog.ErrVersionUnsupported,
	}
	if !errors.Is(vbe, watchdog.ErrVersionUnsupported) {
		t.Fatal("VersionBlockError 必须包装 ErrVersionUnsupported")
	}
	reason, ver, idx := classifyKugouVersionBlock(vbe)
	if reason != "libcef_mismatch" || ver != "20.3.1.999" || idx != 7 {
		t.Fatalf("classifyKugouVersionBlock = (%q, %q, %d)，期望 (libcef_mismatch, 20.3.1.999, 7)", reason, ver, idx)
	}

	raw, evs := col.flushAndRead()
	assertKugouNoSecrets(t, raw)

	var storeBlocked, storeRecovered, libcefBlocked int
	for _, ev := range evs {
		if ev.Event != "player_blocked" {
			continue
		}
		if _, hasExePath := ev.Props["exe_path"]; hasExePath {
			t.Errorf("player_blocked 严禁携带 exe_path：%#v", ev.Props)
		}
		if ev.Props["reason"] == "store_install" && ev.Props["phase"] == "blocked" {
			storeBlocked++
		}
		if ev.Props["reason"] == "store_install" && ev.Props["phase"] == "recovered" {
			storeRecovered++
			if ev.Props["blocked_sec"] != float64(5) {
				t.Errorf("onNotRunning 清零后 store_install blocked_sec 应为 5，实得 %#v", ev.Props["blocked_sec"])
			}
		}
		if ev.Props["reason"] == "libcef_mismatch" && ev.Props["phase"] == "blocked" {
			libcefBlocked++
			if ev.Props["mismatch_idx"] != float64(5) || ev.Props["player_version"] != "20.2.0.12345" {
				t.Errorf("libcef_mismatch 属性异常：%#v", ev.Props)
			}
		}
	}
	if storeBlocked != 2 || storeRecovered != 1 || libcefBlocked != 1 {
		t.Fatalf("阻断事件计数异常：storeBlocked=%d storeRecovered=%d libcefBlocked=%d", storeBlocked, storeRecovered, libcefBlocked)
	}
}

// TestKugouRUMSessionTracksUpdatesAndPrivacy 验证会话 attached/detached、多源 track_recognized、
// 曲中换词/偏移/取消关联 track_lyric_updated、未知 play_status 脱敏信号与 300ms scan_perf_sample。
func TestKugouRUMSessionTracksUpdatesAndPrivacy(t *testing.T) {
	col := startKugouRUMCollector(t)

	origFetchCover := fetchCoverBase64
	origFetchLyrics := fetchLyrics
	origRunning := isKuGouRunning
	t.Cleanup(func() {
		fetchCoverBase64 = origFetchCover
		fetchLyrics = origFetchLyrics
		isKuGouRunning = origRunning
	})

	fetchCoverBase64 = func(_, _ string, _ time.Duration) string {
		return "data:image/jpeg;base64,AAAA"
	}
	fetchLyrics = func(hash string, _ int, _, _ string) ([]klyric.Line, error) {
		return nil, nil
	}
	isKuGouRunning = func() bool { return true }

	localStub := &stubKugouLocal{ch: make(chan assoc.Result, 8)}
	// 第 1 首（内存关联 KRC，含逐字+翻译+逐字音译）
	localStub.ch <- assoc.Result{
		Hash:       "HASH_SONG_1",
		Kind:       assoc.KindFound,
		Source:     assoc.SourceAssociation,
		FromMemory: true,
		Lines: []klyric.Line{
			{
				Index: 0, Time: 1.0, Text: "Secret Kugou Lyric Line",
				SubText:  "翻译行",
				RomaText: "luo ma yin",
				Detailed: player.LyricTextDetailed{
					Words: []player.LyricTextDetailedWord{{Text: "Secret", Timestamp: 1.0}},
				},
				RomaDetailed: player.LyricTextDetailed{
					Words: []player.LyricTextDetailedWord{{Text: "luo", Timestamp: 1.0}},
				},
			},
		},
	}

	var calls int
	src := &funcPlayInfoSource{
		fn: func() (*cdp.PlayInfo, error) {
			calls++
			switch calls {
			case 1, 2:
				return &cdp.PlayInfo{
					Hash:       "HASH_SONG_1",
					Filename:   "Alice Kugou Singer - Secret Kugou Song",
					Cover:      "https://imge.kugou.com/stdmusic/120/kugou_secret_cover.jpg",
					PlayStatus: "playing",
					Progress:   "10000000",
					Duration:   "180000",
				}, nil
			case 3:
				// 注入曲中调偏移与未知 play_status
				localStub.ch <- assoc.Result{
					Hash:          "HASH_SONG_1",
					Kind:          assoc.KindFound,
					Source:        assoc.SourceAssociation,
					FromMemory:    true,
					LyricOffsetMs: 500,
					Lines: []klyric.Line{
						{
							Index: 0, Time: 1.0, Text: "Secret Kugou Lyric Line",
							SubText:  "翻译行",
							RomaText: "luo ma yin",
							Detailed: player.LyricTextDetailed{
								Words: []player.LyricTextDetailedWord{{Text: "Secret", Timestamp: 1.0}},
							},
							RomaDetailed: player.LyricTextDetailed{
								Words: []player.LyricTextDetailedWord{{Text: "luo", Timestamp: 1.0}},
							},
						},
					},
				}
				return &cdp.PlayInfo{
					Hash:       "HASH_SONG_1",
					Filename:   "Alice Kugou Singer - Secret Kugou Song",
					PlayStatus: "SUPER_SECRET_STATUS_RAW",
					Progress:   "20000000",
					Duration:   "180000",
				}, nil
			case 4:
				// 注入曲中取消关联
				localStub.ch <- assoc.Result{
					Hash:       "HASH_SONG_1",
					Kind:       assoc.KindUnassociated,
					FromMemory: true,
				}
				return &cdp.PlayInfo{
					Hash:       "HASH_SONG_1",
					Filename:   "Alice Kugou Singer - Secret Kugou Song",
					PlayStatus: "playing",
					Progress:   "30000000",
					Duration:   "180000",
				}, nil
			default:
				return nil, errors.New("cdp closed")
			}
		},
	}

	beforeTotal := player.TracksRecognizedTotal()
	p := New(0, 10)
	var _ player.RUMFlusher = p
	p.setSessionAttachMeta(42, "20.1.22.27795", watchdog.ExeVariant(`C:\Users\Alice\AppData\HD_KuGou.exe`))
	p.runSession(src, func(context.Context) localLyrics { return localStub })

	raw, evs := col.flushAndRead()
	assertKugouNoSecrets(t, raw)

	if player.TracksRecognizedTotal() < beforeTotal+1 {
		t.Fatalf("player.NoteTrackRecognized 未递增：before=%d after=%d", beforeTotal, player.TracksRecognizedTotal())
	}

	var sawAttached, sawDetached, sawTrack, sawScan, sawOffsetUpd, sawUnassocUpd, sawUnknownStatus bool
	for _, ev := range evs {
		switch ev.Event {
		case "player_session":
			if ev.Props["action"] == "attached" {
				sawAttached = true
				if ev.Props["exe_variant"] != "HD_KuGou.exe" || ev.Props["player_version"] != "20.1.22.27795" {
					t.Errorf("attached 属性异常：%#v", ev.Props)
				}
			}
			if ev.Props["action"] == "detached" {
				sawDetached = true
				if ev.Props["disconnect_cause"] != "cdp_closed" || ev.Props["tracks_played"] != float64(1) {
					t.Errorf("detached 属性异常：%#v", ev.Props)
				}
			}
		case "track_recognized":
			sawTrack = true
			if ev.Props["lyric_source"] != "local_assoc_mem" ||
				ev.Props["lyric_format"] != "krc" ||
				ev.Props["has_verbatim"] != true ||
				ev.Props["has_trans"] != true ||
				ev.Props["has_roma"] != true ||
				ev.Props["has_roma_detailed"] != true ||
				ev.Props["has_furigana"] != false {
				t.Errorf("track_recognized 能力字段异常：%#v", ev.Props)
			}
		case "scan_perf_sample":
			if ev.Props["scan_kind"] == "kugou_local_await" && ev.Props["verdict"] == "found" {
				sawScan = true
			}
		case "track_lyric_updated":
			switch ev.Props["reason"] {
			case "offset_changed":
				sawOffsetUpd = true
				if ev.Props["offset_ms"] != float64(500) {
					t.Errorf("offset_changed offset_ms 异常：%#v", ev.Props)
				}
			case "user_unassociated":
				sawUnassocUpd = true
			}
		case "player_compat_signal":
			if ev.Props["signal"] == "unknown_play_status" {
				sawUnknownStatus = true
				if dc, _ := ev.Props["detail_code"].(string); strings.Contains(dc, "SECRET") {
					t.Errorf("unknown_play_status 泄露了原文：%q", dc)
				}
			}
		}
	}
	if !sawAttached || !sawDetached || !sawTrack || !sawScan || !sawOffsetUpd || !sawUnassocUpd || !sawUnknownStatus {
		t.Fatalf("缺失预期事件：attached=%v detached=%v track=%v scan=%v offset=%v unassoc=%v unknownStatus=%v",
			sawAttached, sawDetached, sawTrack, sawScan, sawOffsetUpd, sawUnassocUpd, sawUnknownStatus)
	}
}

// TestKugouRUMFlushBarrierConcurrency 验证退出 FlushRUM 的完成屏障、幂等性与并发安全。
func TestKugouRUMFlushBarrierConcurrency(t *testing.T) {
	col := startKugouRUMCollector(t)

	p := New(0, 10)
	sess := p.startSessionRUM("20.1.22.27795", "KuGou.exe", 10)
	inTrack := make(chan struct{})
	releaseTrack := make(chan struct{})
	sess.mu.Lock()
	sess.testBeforeTrackEmit = func() {
		close(inTrack)
		<-releaseTrack
	}
	sess.mu.Unlock()

	sess.beginTrack(1)
	go sess.onTrackLyricSettled(1, "online", []klyric.Line{{Text: "line"}}, "", 50, false)
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

	// 重复 FlushRUM 必须幂等
	p.FlushRUM()
	sess.end("cdp_closed")

	_, evs := col.flushAndRead()
	var tracks, detaches int
	for _, ev := range evs {
		if ev.Event == "track_recognized" {
			tracks++
		}
		if ev.Event == "player_session" && ev.Props["action"] == "detached" {
			detaches++
			if ev.Props["disconnect_cause"] != "stopped" {
				t.Errorf("FlushRUM 应报 disconnect_cause=stopped，实得 %#v", ev.Props["disconnect_cause"])
			}
		}
	}
	if tracks != 1 || detaches != 1 {
		t.Fatalf("期望 1 条 track_recognized 与 1 条 detached，实得 tracks=%d detaches=%d", tracks, detaches)
	}
}

type funcPlayInfoSource struct {
	fn func() (*cdp.PlayInfo, error)
}

func (f *funcPlayInfoSource) GetPlayInfo() (*cdp.PlayInfo, error) { return f.fn() }
func (f *funcPlayInfoSource) IsClosed() bool                      { return false }
