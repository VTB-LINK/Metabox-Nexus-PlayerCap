package sodamusic

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
	"Metabox-Nexus-PlayerCap/player/krc"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
	"Metabox-Nexus-PlayerCap/player/sodamusic/cdp"
	"Metabox-Nexus-PlayerCap/rum"
)

type sodaRUMBatch struct {
	Batch []struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	} `json:"batch"`
}

type sodaRUMEvent struct {
	Event string
	Props map[string]any
}

type sodaRUMCollector struct {
	srv    *httptest.Server
	mu     sync.Mutex
	raw    []string
	events []sodaRUMEvent
}

func startSodaRUMCollector(t *testing.T) *sodaRUMCollector {
	t.Helper()
	c := &sodaRUMCollector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{\"enabled\":true}"}}`)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			body, _ := io.ReadAll(r.Body)
			var parsed sodaRUMBatch
			_ = json.Unmarshal(body, &parsed)
			c.mu.Lock()
			c.raw = append(c.raw, string(body))
			for _, ev := range parsed.Batch {
				c.events = append(c.events, sodaRUMEvent{Event: ev.Event, Props: ev.Properties})
			}
			c.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(c.srv.Close)

	restore := rum.OverrideCredentialsForTest("phc_test_soda", c.srv.URL)
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

func (c *sodaRUMCollector) flushAndRead() (string, []sodaRUMEvent) {
	rum.Flush(2 * time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	evs := make([]sodaRUMEvent, len(c.events))
	copy(evs, c.events)
	return strings.Join(c.raw, "\n"), evs
}

func assertSodaNoSecrets(t *testing.T, raw string) {
	t.Helper()
	for _, secret := range []string{
		"Secret Soda Song",
		"Alice Soda Singer",
		"Secret Soda Lyric",
		"Secret Soda Trans",
		"C:\\Users\\Alice",
		"WindowsApps",
		"p1.music.126.net",
		"sf16-music.byteimg.com",
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("出网 RUM JSON 泄露了敏感信息 %q：\n%s", secret, raw)
		}
	}
}

// TestSodaRUMBlockedZeroPathAndRecovery 验证 store_install 零路径、no_admin 恢复耗时与 onNotRunning 静默清零。
func TestSodaRUMBlockedZeroPathAndRecovery(t *testing.T) {
	col := startSodaRUMCollector(t)

	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	bt := sodaBlockTracker{now: func() time.Time { return now }}

	bt.onBlocked("store_install")
	bt.onBlocked("store_install") // 同因去重
	now = now.Add(40 * time.Second)
	bt.onNotRunning() // 静默清零，不发 recovered

	now = now.Add(5 * time.Second)
	bt.onBlocked("no_admin")
	now = now.Add(11 * time.Second)
	bt.onRecovered()

	raw, evs := col.flushAndRead()
	assertSodaNoSecrets(t, raw)

	var storeCnt, noAdminBlocked, noAdminRec int
	for _, ev := range evs {
		if ev.Event != "player_blocked" {
			continue
		}
		if _, hasExePath := ev.Props["exe_path"]; hasExePath {
			t.Errorf("player_blocked 严禁携带 exe_path：%#v", ev.Props)
		}
		switch {
		case ev.Props["reason"] == "store_install" && ev.Props["phase"] == "blocked":
			storeCnt++
		case ev.Props["reason"] == "no_admin" && ev.Props["phase"] == "blocked":
			noAdminBlocked++
		case ev.Props["reason"] == "no_admin" && ev.Props["phase"] == "recovered":
			noAdminRec++
			if ev.Props["blocked_sec"] != float64(11) {
				t.Errorf("no_admin blocked_sec 应为 11，实得 %#v", ev.Props["blocked_sec"])
			}
		}
	}
	if storeCnt != 1 || noAdminBlocked != 1 || noAdminRec != 1 {
		t.Fatalf("阻断事件计数异常：store=%d noAdminBlocked=%d noAdminRec=%d", storeCnt, noAdminBlocked, noAdminRec)
	}
}

// TestSodaRUMSessionExitCausesThrottleKugouRomaAndFlushBarrier 验证 host_exiting/fail_grace_10s、
// background_throttling_stuck 限频、借酷狗音译（不重复记 NoteTrackRecognized）、纯音乐跳过与 FlushRUM 完成屏障。
func TestSodaRUMSessionExitCausesThrottleKugouRomaAndFlushBarrier(t *testing.T) {
	col := startSodaRUMCollector(t)

	origFetch := fetchKugouLyrics
	defer func() { fetchKugouLyrics = origFetch }()

	beforeTotal := player.TracksRecognizedTotal()
	p := New(0, 200)
	var _ player.RUMFlusher = p

	// 1. 验证 host_exiting 退出原因与 background_throttling_stuck 会话级限频
	sess1 := p.startSessionRUM("2.1.0", 22)
	sess1.onCompat("background_throttling_stuck", "")
	sess1.onCompat("background_throttling_stuck", "") // 重复应被限频

	// 曲目 1：带明文 KRC 逐字 + 翻译，先发 pending 的 track_recognized，再异步借酷狗音译命中 hit_char
	lines := []krc.Line{
		{
			Index:   0,
			Time:    1.0,
			Text:    "Secret Soda Lyric",
			SubText: "Secret Soda Trans",
			Detailed: player.LyricTextDetailed{
				Timestamp: 1.0,
				Duration:  1.0,
				Words: []player.LyricTextDetailedWord{
					{Timestamp: 1.0, Duration: 0.5, Text: "Secret"},
					{Timestamp: 1.5, Duration: 0.5, Text: "Soda"},
				},
			},
		},
	}
	sess1.beginTrack(1, true)
	sess1.onTrackLyricReady(1, "online", lines, "", 15, true, "pending")
	sess1.onCoverOutcome(1, player.CoverOutcome{Action: player.CoverActionResized800, DurationMs: 35})

	// 异步借酷狗音译完成：通过真实 fetchKugouRomaWithRUM 路径验证
	fetchKugouLyrics = func(string, int, string, string) ([]klyric.Line, error) {
		return []klyric.Line{
			{
				Text: "Secret Soda Lyric",
				Detailed: player.LyricTextDetailed{
					Words: []player.LyricTextDetailedWord{
						{Text: "Secret"},
						{Text: "Soda"},
						{Text: "Lyric"},
					},
				},
				RomaWords: []string{"se ", "so ", "ly"},
				RomaText:  "se so ly",
			},
		}, nil
	}
	romaCh := make(chan []krc.Line, 1)
	p.fetchKugouRomaWithRUM(romaCh, append([]krc.Line(nil), lines...), "Secret Soda Song", "Alice Soda Singer", 200000, sess1, 1)
	select {
	case merged := <-romaCh:
		if len(merged) != 1 || merged[0].RomaText == "" {
			t.Fatalf("期望借酷狗音译写回 RomaText，实得 %+v", merged)
		}
	default:
		t.Fatal("fetchKugouRomaWithRUM 应将对齐后的歌词写入 romaCh")
	}

	// 曲目 2：纯音乐（skipped）
	sess1.beginTrack(2, false)
	sess1.onTrackLyricReady(2, "online", nil, "纯音乐，请欣赏", 5, false, "skipped")

	p.endSessionRUM(sess1, "host_exiting")

	// 2. 验证 fail_grace_10s 退出原因与 fetchKugouRomaWithRUM err 分支
	sess2 := p.startSessionRUM("2.1.0", 10)
	sess2.beginTrack(3, false)
	sess2.onTrackLyricReady(3, "online", lines, "", 8, false, "pending")
	fetchKugouLyrics = func(string, int, string, string) ([]klyric.Line, error) {
		return nil, errors.New("kugou network error")
	}
	p.fetchKugouRomaWithRUM(make(chan []krc.Line, 1), append([]krc.Line(nil), lines...), "Secret Soda Song", "Alice Soda Singer", 200000, sess2, 3)
	p.endSessionRUM(sess2, "fail_grace_10s")

	// 3. 验证退出 FlushRUM 完成屏障
	sess3 := p.startSessionRUM("2.1.0", 12)
	inTrack := make(chan struct{})
	releaseTrack := make(chan struct{})
	sess3.mu.Lock()
	sess3.testBeforeTrackEmit = func() {
		close(inTrack)
		<-releaseTrack
	}
	sess3.mu.Unlock()

	sess3.beginTrack(4, false)
	go sess3.onTrackLyricReady(4, "none", nil, "", 4, false, "skipped")
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
	p.FlushRUM() // 幂等

	raw, evs := col.flushAndRead()
	assertSodaNoSecrets(t, raw)

	// 共 4 首曲目（借酷狗音译异步落地绝不能重复递增 NoteTrackRecognized）
	if got := player.TracksRecognizedTotal(); got != beforeTotal+4 {
		t.Fatalf("TracksRecognizedTotal 应恰递增 4：before=%d after=%d", beforeTotal, got)
	}

	var (
		throttleCnt     int
		hostExitCnt     int
		failGraceCnt    int
		stoppedCnt      int
		trackPendingCnt int
		trackSkippedCnt int
		romaHitCharCnt  int
		romaErrCnt      int
	)
	for _, ev := range evs {
		switch ev.Event {
		case "player_compat_signal":
			if ev.Props["signal"] == "background_throttling_stuck" {
				throttleCnt++
			}
		case "player_session":
			if ev.Props["action"] == "detached" {
				switch ev.Props["disconnect_cause"] {
				case "host_exiting":
					hostExitCnt++
					if ev.Props["tracks_played"] != float64(2) {
						t.Errorf("sess1 tracks_played 应为 2，实得 %#v", ev.Props["tracks_played"])
					}
				case "fail_grace_10s":
					failGraceCnt++
				case "stopped":
					stoppedCnt++
				}
			}
		case "track_recognized":
			if ev.Props["clock_mode"] != "cdp_edge" || ev.Props["has_furigana"] != false {
				t.Errorf("track_recognized 基础属性异常：%#v", ev.Props)
			}
			switch ev.Props["soda_kugou_roma_outcome"] {
			case "pending":
				trackPendingCnt++
				if ev.Props["lyric_format"] != "krc" || ev.Props["has_verbatim"] != true || ev.Props["has_trans"] != true {
					t.Errorf("带词曲目属性异常：%#v", ev.Props)
				}
			case "skipped":
				trackSkippedCnt++
			}
		case "track_lyric_updated":
			if ev.Props["reason"] == "kugou_roma_resolved" {
				switch ev.Props["soda_kugou_roma_outcome"] {
				case "hit_char":
					romaHitCharCnt++
					if ev.Props["has_roma"] != true {
						t.Errorf("hit_char 应带 has_roma=true：%#v", ev.Props)
					}
				case "err":
					romaErrCnt++
				}
			}
		}
	}
	if throttleCnt != 1 || hostExitCnt != 1 || failGraceCnt != 1 || stoppedCnt != 1 ||
		trackPendingCnt != 2 || trackSkippedCnt != 2 || romaHitCharCnt != 1 || romaErrCnt != 1 {
		t.Fatalf("事件计数异常：throttle=%d hostExit=%d failGrace=%d stopped=%d pending=%d skipped=%d romaHitChar=%d romaErr=%d",
			throttleCnt, hostExitCnt, failGraceCnt, stoppedCnt, trackPendingCnt, trackSkippedCnt, romaHitCharCnt, romaErrCnt)
	}
	_ = cdp.ErrHostExiting
}
