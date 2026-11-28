package kuwo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kuwo/cdp"
	"Metabox-Nexus-PlayerCap/player/kuwo/cover"
	"Metabox-Nexus-PlayerCap/player/kuwo/lyric"
	"Metabox-Nexus-PlayerCap/player/kuwo/pos"
	"Metabox-Nexus-PlayerCap/player/kuwo/watchdog"
	"Metabox-Nexus-PlayerCap/rum"
)

type kuwoRUMBatch struct {
	Batch []struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	} `json:"batch"`
}

type kuwoRUMEvent struct {
	Event string
	Props map[string]any
}

type kuwoRUMCollector struct {
	srv    *httptest.Server
	mu     sync.Mutex
	raw    []string
	events []kuwoRUMEvent
}

func startKuwoRUMCollector(t *testing.T) *kuwoRUMCollector {
	t.Helper()
	c := &kuwoRUMCollector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{\"enabled\":true}"}}`)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			body, _ := io.ReadAll(r.Body)
			var parsed kuwoRUMBatch
			_ = json.Unmarshal(body, &parsed)
			c.mu.Lock()
			c.raw = append(c.raw, string(body))
			for _, ev := range parsed.Batch {
				c.events = append(c.events, kuwoRUMEvent{Event: ev.Event, Props: ev.Properties})
			}
			c.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(c.srv.Close)

	restore := rum.OverrideCredentialsForTest("phc_test_kuwo", c.srv.URL)
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

func (c *kuwoRUMCollector) flushAndRead() (string, []kuwoRUMEvent) {
	rum.Flush(2 * time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	evs := make([]kuwoRUMEvent, len(c.events))
	copy(evs, c.events)
	return strings.Join(c.raw, "\n"), evs
}

func assertKuwoNoSecrets(t *testing.T, raw string) {
	t.Helper()
	for _, secret := range []string{
		"Secret Kuwo Song",
		"Alice Kuwo Singer",
		"Secret Kuwo Lyric",
		"C:\\Users\\Alice",
		"Alice",
		"artistpic.kwcdn.kuwo.cn",
		"秘密渠道",
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("出网 RUM JSON 泄露了敏感信息 %q：\n%s", secret, raw)
		}
	}
}

// TestKuwoChannelSuffixSanitization 重点验证野生渠道后缀白名单正则校验（^[a-zA-Z0-9_-]{1,16}$）。
func TestKuwoChannelSuffixSanitization(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"W1", "W1"},
		{"UG1", "UG1"},
		{"custom_01", "custom_01"},
		{"a-b_C9", "a-b_C9"},
		{"1234567890123456", "1234567890123456"}, // 恰 16 字符
		{"12345678901234567", "other"},           // 超 16 字符
		{`C:\Users\Alice`, "other"},              // 含路径
		{"W1 beta", "other"},                     // 含空格
		{"秘密渠道", "other"},                        // 含中文
		{"", "other"},                            // 空串
		{"../W1", "other"},                       // 含点/斜杠
	}
	for _, tc := range cases {
		if got := watchdog.SanitizeChannelSuffix(tc.in); got != tc.want {
			t.Errorf("SanitizeChannelSuffix(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
	if got := extractKuwoChannelSuffix("9.6.0.0_UG1"); got != "UG1" {
		t.Errorf("extractKuwoChannelSuffix(9.6.0.0_UG1) = %q，期望 UG1", got)
	}
	if got := extractKuwoChannelSuffix(`9.6.0.0_C:\Users\Alice`); got != "other" {
		t.Errorf("extractKuwoChannelSuffix(path) = %q，期望 other", got)
	}
}

// TestKuwoRUMBlockedZeroPathAndRecovery 验证 store_install 零路径、多开/不支持核阻断与 onNotRunning 清零。
func TestKuwoRUMBlockedZeroPathAndRecovery(t *testing.T) {
	col := startKuwoRUMCollector(t)

	now := time.Date(2026, 11, 27, 13, 0, 0, 0, time.UTC)
	bt := kuwoBlockTracker{now: func() time.Time { return now }}

	bt.onBlocked("store_install")
	bt.onBlocked("store_install") // 同因去重
	now = now.Add(45 * time.Second)
	bt.onNotRunning() // 静默清零，不发 recovered

	now = now.Add(5 * time.Second)
	bt.onBlocked("kuwo_unsupported_core")
	now = now.Add(9 * time.Second)
	bt.onRecovered()

	raw, evs := col.flushAndRead()
	assertKuwoNoSecrets(t, raw)

	var storeCnt, coreBlocked, coreRec int
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
		case ev.Props["reason"] == "kuwo_unsupported_core" && ev.Props["phase"] == "blocked":
			coreBlocked++
		case ev.Props["reason"] == "kuwo_unsupported_core" && ev.Props["phase"] == "recovered":
			coreRec++
			if ev.Props["blocked_sec"] != float64(9) {
				t.Errorf("kuwo_unsupported_core blocked_sec 应为 9，实得 %#v", ev.Props["blocked_sec"])
			}
		}
	}
	if storeCnt != 1 || coreBlocked != 1 || coreRec != 1 {
		t.Fatalf("阻断事件计数异常：store=%d coreBlocked=%d coreRec=%d", storeCnt, coreBlocked, coreRec)
	}
}

// TestKuwoRUMBilingualTrackDriftGuardAndFlushBarrier 验证双语 ml:1.0 标记、漂移停用与暂扣守卫、
// 退出 FlushRUM 完成屏障与零敏感泄露。
func TestKuwoRUMBilingualTrackDriftGuardAndFlushBarrier(t *testing.T) {
	col := startKuwoRUMCollector(t)

	// 用真实 lyric.ParseLRC 解析双语 ml:1.0 逐字歌词（带 [kuwo:054] 与下一行时间戳译文行）
	const lrcx = "[ml:1.0]\n[kuwo:054]\n" +
		"[00:01.000]<400,-400>Secret Kuwo Lyric\n" +
		"[00:02.000]<0,0>翻译行\n" +
		"[00:02.000]\n"
	parsed := lyric.ParseLRC(lrcx)
	items := toLyricLines(parsed, 0)

	beforeTotal := player.TracksRecognizedTotal()
	p := New(0, 50)
	var _ player.RUMFlusher = p

	sess := p.startSessionRUM("9.6.0.0_UG1", 18)

	// 1. 双语逐字曲目 + 封面 + supplement_decision
	sess.beginTrack(1, "ds")
	sess.noteSupplement(1, "kept")
	sess.onTrackLyricSettled(1, "online", items, "", 85, true)
	sess.onCoverOutcome(1, cover.Result{
		URL:     "http://artistpic.kwcdn.kuwo.cn/secret.jpg",
		DataURI: "data:image/jpeg;base64,AAA",
		Outcome: player.CoverOutcome{Action: player.CoverActionResized800, DurationMs: 40},
	}, nil)

	// 2. 时钟守卫：300ms 暂扣、瞬态暂扣、漂移停用
	emitKuwoClockGuard("switch_hold_300ms", "intercepted", 0, 180)
	emitKuwoClockGuard("ds_waveout_transient", "intercepted", 0, 0)
	emitKuwoClockGuard("lag_drift_disabled", "drift_disabled", 25, 0)

	// 3. 兼容信号：未知插件关闭校准 + 20s 卡死
	sess.onCompat("unknown_audio_plugin_lag_off", "")
	sess.onCompat("clock_stalled_20s", "")

	// 4. 退出 FlushRUM 完成屏障交错验证
	inTrack := make(chan struct{})
	releaseTrack := make(chan struct{})
	sess.mu.Lock()
	sess.testBeforeTrackEmit = func() {
		close(inTrack)
		<-releaseTrack
	}
	sess.mu.Unlock()

	sess.beginTrack(2, "wasapi")
	go sess.onTrackLyricSettled(2, "none", nil, "", 30, false)
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
	assertKuwoNoSecrets(t, raw)

	if player.TracksRecognizedTotal() < beforeTotal+2 {
		t.Fatalf("TracksRecognizedTotal 应递增 2：before=%d after=%d", beforeTotal, player.TracksRecognizedTotal())
	}

	var sawBilingual, sawDrift, sawHold, sawTransient, sawLagOff, sawStalled, sawDetached int
	for _, ev := range evs {
		switch ev.Event {
		case "track_recognized":
			if ev.Props["lyric_source"] == "online" {
				sawBilingual++
				if ev.Props["clock_mode"] != "ds" ||
					ev.Props["lyric_format"] != "lrcx" ||
					ev.Props["has_verbatim"] != true ||
					ev.Props["has_trans"] != true ||
					ev.Props["has_roma"] != false ||
					ev.Props["has_furigana"] != false ||
					ev.Props["supplement_decision"] != "kept" ||
					ev.Props["cover_action"] != player.CoverActionResized800 {
					t.Errorf("双语 track_recognized 属性异常：%#v", ev.Props)
				}
			}
		case "playback_clock_guard":
			switch ev.Props["guard_kind"] {
			case "lag_drift_disabled":
				if ev.Props["outcome"] == "drift_disabled" {
					sawDrift++
				}
			case "switch_hold_300ms":
				sawHold++
			case "ds_waveout_transient":
				sawTransient++
			}
		case "player_compat_signal":
			switch ev.Props["signal"] {
			case "unknown_audio_plugin_lag_off":
				sawLagOff++
			case "clock_stalled_20s":
				sawStalled++
			}
		case "player_session":
			if ev.Props["action"] == "detached" {
				sawDetached++
				if ev.Props["exe_variant"] != "UG1" || ev.Props["tracks_played"] != float64(2) || ev.Props["disconnect_cause"] != "stopped" {
					t.Errorf("detached 属性异常：%#v", ev.Props)
				}
			}
		}
	}
	if sawBilingual != 1 || sawDrift != 1 || sawHold != 1 || sawTransient != 1 || sawLagOff != 1 || sawStalled != 1 || sawDetached != 1 {
		t.Fatalf("事件计数异常：bilingual=%d drift=%d hold=%d transient=%d lagOff=%d stalled=%d detached=%d",
			sawBilingual, sawDrift, sawHold, sawTransient, sawLagOff, sawStalled, sawDetached)
	}
	_ = pos.DeviceDirectSound
}

type kuwoStubCDP struct {
	build string
	snap  cdp.Snapshot
}

func (c *kuwoStubCDP) Snapshot(ctx context.Context) (cdp.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return cdp.Snapshot{}, err
	}
	return c.snap, nil
}
func (c *kuwoStubCDP) Build() string { return c.build }
func (c *kuwoStubCDP) Close() error  { return nil }

type kuwoStubPosReader struct {
	onRead func()
	posMs  int32
}

func (r *kuwoStubPosReader) Read(rid string, _ bool) (pos.Sample, error) {
	r.posMs += 50
	if r.onRead != nil {
		r.onRead()
	}
	return pos.Sample{
		RID:        pos.RIDWithPrefix(rid),
		PositionMs: r.posMs,
		CoreMs:     r.posMs,
		DurationMs: 200000,
		ObservedAt: time.Now(),
	}, nil
}
func (r *kuwoStubPosReader) Close() {}

// TestKuwoRUMUnsupportedCoreNotPrematurelyRecovered 回归验证：CDP 连通但 openPosition 连续两轮返回
// pos.ErrUnsupportedCore 时，全过程只发 1 次 blocked、0 次 recovered；直到核心打开并读到有效样本后才发 1 次 recovered。
func TestKuwoRUMUnsupportedCoreNotPrematurelyRecovered(t *testing.T) {
	col := startKuwoRUMCollector(t)

	origEnsure, origFind, origConnect, origOpen := ensureDebugMode, findMainPID, connectCDP, openPosition
	origTransient := transientTolerance
	t.Cleanup(func() {
		ensureDebugMode, findMainPID, connectCDP, openPosition = origEnsure, origFind, origConnect, origOpen
		transientTolerance = origTransient
	})
	transientTolerance = 10 * time.Millisecond

	ensureDebugMode = func(context.Context) error { return nil }
	findMainPID = func() (int, error) { return 4242, nil }
	connectCDP = func(context.Context) (cdpSession, error) {
		return &kuwoStubCDP{
			build: "9.6.0.0_W1",
			snap:  cdp.Snapshot{RID: "MUSIC_99", Title: "Secret Kuwo Song", Artist: "Alice Kuwo Singer", Status: 1},
		}, nil
	}

	p := New(0, 50)
	var bt kuwoBlockTracker

	// 第 1、2 轮：CDP 连通，但 openPosition 持续返回 ErrUnsupportedCore
	openPosition = func(int, string, *bool, int) (positionSource, error) {
		return nil, pos.ErrUnsupportedCore
	}
	for round := 1; round <= 2; round++ {
		err := p.connectSessionWithBT(context.Background(), &bt)
		if !errors.Is(err, pos.ErrUnsupportedCore) {
			t.Fatalf("第 %d 轮期望返回 pos.ErrUnsupportedCore，实得 %v", round, err)
		}
		bt.onBlocked("kuwo_unsupported_core")
	}

	// 前两轮之后断言：仅 1 次 blocked、0 次 recovered
	rawMid, evsMid := col.flushAndRead()
	assertKuwoNoSecrets(t, rawMid)
	var midBlocked, midRecovered int
	for _, ev := range evsMid {
		if ev.Event == "player_blocked" && ev.Props["reason"] == "kuwo_unsupported_core" {
			switch ev.Props["phase"] {
			case "blocked":
				midBlocked++
			case "recovered":
				midRecovered++
			}
		}
	}
	if midBlocked != 1 || midRecovered != 0 {
		t.Fatalf("两轮 ErrUnsupportedCore 后应恰有 1 次 blocked、0 次 recovered，实得 blocked=%d recovered=%d", midBlocked, midRecovered)
	}

	// 第 3 轮：openPosition 成功并读到有效样本后立即取消上下文退出会话
	ctx3, cancel3 := context.WithCancel(context.Background())
	var readOnce atomic.Bool
	openPosition = func(int, string, *bool, int) (positionSource, error) {
		return &kuwoStubPosReader{
			onRead: func() {
				if readOnce.CompareAndSwap(false, true) {
					go func() {
						time.Sleep(20 * time.Millisecond)
						cancel3()
					}()
				}
			},
		}, nil
	}
	_ = p.connectSessionWithBT(ctx3, &bt)

	rawEnd, evsEnd := col.flushAndRead()
	assertKuwoNoSecrets(t, rawEnd)
	var endBlocked, endRecovered int
	for _, ev := range evsEnd {
		if ev.Event == "player_blocked" && ev.Props["reason"] == "kuwo_unsupported_core" {
			switch ev.Props["phase"] {
			case "blocked":
				endBlocked++
			case "recovered":
				endRecovered++
			}
		}
	}
	if endBlocked != 1 || endRecovered != 1 {
		t.Fatalf("核心恢复并读到有效样本后应恰有 1 次 blocked、1 次 recovered，实得 blocked=%d recovered=%d", endBlocked, endRecovered)
	}
}
