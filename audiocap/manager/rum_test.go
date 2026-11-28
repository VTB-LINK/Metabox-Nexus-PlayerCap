package manager

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/audiocap/frame"
	"Metabox-Nexus-PlayerCap/rum"
)

type mgrRUMBatch struct {
	Batch []struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	} `json:"batch"`
}

type mgrRUMEvent struct {
	Event      string
	Properties map[string]any
}

type mgrRUMCollector struct {
	srv     *httptest.Server
	mu      sync.Mutex
	rawBody []string
	events  []mgrRUMEvent
}

func setupManagerRUMCollector(t *testing.T) *mgrRUMCollector {
	t.Helper()
	col := &mgrRUMCollector{}
	col.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{\"enabled\":true}"}}`)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			body, _ := io.ReadAll(r.Body)
			var parsed mgrRUMBatch
			_ = json.Unmarshal(body, &parsed)
			col.mu.Lock()
			col.rawBody = append(col.rawBody, string(body))
			for _, ev := range parsed.Batch {
				col.events = append(col.events, mgrRUMEvent{
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
	restore := rum.OverrideCredentialsForTest("phc_mgr_test", col.srv.URL)
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

func (c *mgrRUMCollector) snapshot() []mgrRUMEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]mgrRUMEvent, len(c.events))
	copy(out, c.events)
	return out
}

func (c *mgrRUMCollector) rawJSON() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.rawBody, "\n")
}

func TestAudioCaptureSessionMetricsAndLifecycleEvents(t *testing.T) {
	col := setupManagerRUMCollector(t)

	var tr rumTracker
	t0 := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)

	// 1. 正常捕获会话：开始 → 120ms 首帧 → 收到 helper 汇总 → 停止
	tr.onCaptureStart(1, 1, "wesing", t0, true, true)
	tr.onFrame("wesing", frame.FlagSessionKnown|frame.FlagSessionActive, t0.Add(120*time.Millisecond), func(string) bool { return true })
	tr.onCaptureStop("wesing", t0.Add(15*time.Second), true)
	// 模拟 helper 在停止后经 stderr 发出最终摘要行
	tr.handleDiagLineForHelper(1, "[mbx-rum] kind=capture_summary player=wesing sid=1 norm_frames=200 capped_frames=50 discontinuities=3 frames_dropped=7")

	// 2. helper 异常退出与放弃事件
	tr.emitHelperAbnormalExit(0xC0000005, 2, true)
	tr.emitGaveUp("qqmusic", 0, false, 5, true)
	tr.emitGaveUp("", 0xC0000005, true, 5, false)

	rum.Flush(2 * time.Second)

	events := col.snapshot()
	var sawSessionEnd, sawAbnormal, sawPlayerGaveUp, sawHelperGaveUp bool
	for _, ev := range events {
		if ev.Event != "audio_capture_session" {
			continue
		}
		switch ev.Properties["event_kind"] {
		case "session_end":
			if ev.Properties["player"] != "wesing" {
				continue
			}
			sawSessionEnd = true
			if ev.Properties["ttff_ms"] != float64(120) {
				t.Errorf("ttff_ms = %#v，期望 120", ev.Properties["ttff_ms"])
			}
			if ev.Properties["duration_sec"] != float64(15) {
				t.Errorf("duration_sec = %#v，期望 15", ev.Properties["duration_sec"])
			}
			if ev.Properties["job_object_ok"] != true || ev.Properties["norm_enabled"] != true {
				t.Errorf("job_object_ok/norm_enabled 异常：%#v", ev.Properties)
			}
			ratio, _ := ev.Properties["gain_capped_ratio"].(float64)
			if math.Abs(ratio-0.25) > 1e-6 {
				t.Errorf("gain_capped_ratio = %v，期望 0.25", ratio)
			}
			if ev.Properties["discontinuity_count"] != float64(3) {
				t.Errorf("discontinuity_count = %#v，期望 3", ev.Properties["discontinuity_count"])
			}
			if ev.Properties["frames_dropped"] != float64(7) {
				t.Errorf("frames_dropped = %#v，期望 7", ev.Properties["frames_dropped"])
			}
			if ev.Properties["stats_incomplete"] != false {
				t.Errorf("收到最终摘要后 stats_incomplete = %#v，期望 false", ev.Properties["stats_incomplete"])
			}
		case "helper_abnormal_exit":
			sawAbnormal = true
			if ev.Properties["exit_code"] != "0xC0000005" || ev.Properties["quick_fails"] != float64(2) || ev.Properties["job_object_ok"] != true {
				t.Errorf("helper_abnormal_exit 属性异常：%#v", ev.Properties)
			}
		case "gave_up":
			if ev.Properties["player"] == "qqmusic" {
				sawPlayerGaveUp = true
				if ev.Properties["quick_fails"] != float64(5) {
					t.Errorf("player gave_up quick_fails = %#v，期望 5", ev.Properties["quick_fails"])
				}
			} else if ev.Properties["player"] == "" {
				sawHelperGaveUp = true
				if ev.Properties["exit_code"] != "0xC0000005" || ev.Properties["quick_fails"] != float64(5) || ev.Properties["job_object_ok"] != false {
					t.Errorf("helper gave_up 属性异常：%#v", ev.Properties)
				}
			}
		}
	}

	if !sawSessionEnd || !sawAbnormal || !sawPlayerGaveUp || !sawHelperGaveUp {
		t.Fatalf("缺少预期 audio_capture_session 事件：end=%v abnormal=%v playerGaveUp=%v helperGaveUp=%v，实际：%#v",
			sawSessionEnd, sawAbnormal, sawPlayerGaveUp, sawHelperGaveUp, events)
	}
}

func TestAudioSignalStateSuspectedASIOEdgeDedup(t *testing.T) {
	col := setupManagerRUMCollector(t)

	var tr rumTracker
	playing := true
	playingFn := func(string) bool { return playing }
	t0 := time.Date(2026, 11, 27, 14, 10, 0, 0, time.UTC)

	tr.onCaptureStart(1, 1, "kugou", t0, true, true)

	// 连续 4.9s SessionKnown && !SessionActive → 不应误报
	tr.onFrame("kugou", frame.FlagSessionKnown, t0, playingFn)
	tr.onFrame("kugou", frame.FlagSessionKnown, t0.Add(4900*time.Millisecond), playingFn)

	rum.Flush(2 * time.Second)
	for _, ev := range col.snapshot() {
		if ev.Event == "audio_signal_state" {
			t.Fatalf("< 5s 不应上报 suspected_asio_or_exclusive：%#v", ev)
		}
	}

	// 满 5.0s 及之后持续 10s → 仅边沿上报 1 次，不刷屏
	tr.onFrame("kugou", frame.FlagSessionKnown, t0.Add(5*time.Second), playingFn)
	tr.onFrame("kugou", frame.FlagSessionKnown, t0.Add(6*time.Second), playingFn)
	tr.onFrame("kugou", frame.FlagSessionKnown, t0.Add(15*time.Second), playingFn)

	// 暂停态下再持续 10s 不应上报
	playing = false
	tr.onFrame("kugou", frame.FlagSessionKnown, t0.Add(16*time.Second), playingFn)
	tr.onFrame("kugou", frame.FlagSessionKnown, t0.Add(26*time.Second), playingFn)

	// 恢复播放且恢复 SessionActive 复位边沿；之后再次连续 5s 无会话才允许发第 2 次
	playing = true
	tr.onFrame("kugou", frame.FlagSessionKnown|frame.FlagSessionActive, t0.Add(27*time.Second), playingFn)
	tr.onFrame("kugou", frame.FlagSessionKnown, t0.Add(28*time.Second), playingFn)
	tr.onFrame("kugou", frame.FlagSessionKnown, t0.Add(33*time.Second), playingFn)

	rum.Flush(2 * time.Second)

	var sigEvents []mgrRUMEvent
	for _, ev := range col.snapshot() {
		if ev.Event == "audio_signal_state" && ev.Properties["signal_state"] == "suspected_asio_or_exclusive" {
			sigEvents = append(sigEvents, ev)
		}
	}
	if len(sigEvents) != 2 {
		t.Fatalf("期望边沿上报 2 次（首段 1 次 + 恢复后第二段 1 次），实际 %d 次：%#v", len(sigEvents), sigEvents)
	}
	if sigEvents[0].Properties["player"] != "kugou" {
		t.Errorf("player = %#v，期望 kugou", sigEvents[0].Properties["player"])
	}
}

type fixedChunksReader struct {
	chunks [][]byte
}

func (r *fixedChunksReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	c := r.chunks[0]
	if len(c) <= len(p) {
		copy(p, c)
		r.chunks = r.chunks[1:]
		return len(c), nil
	}
	copy(p, c[:len(p)])
	r.chunks[0] = c[len(p):]
	return len(p), nil
}

func TestPumpStderrFiltersRUMDiagLinesAndTriggersWASAPISignals(t *testing.T) {
	col := setupManagerRUMCollector(t)

	var tr rumTracker
	var tail tailBuffer
	var out bytes.Buffer

	// 故意将 [mbx-rum] 前缀和正文横跨多个 Read 块边界，且混入正常日志、超长畸形诊断行与 CRLF
	oversizedDiag := "[mbx-rum] kind=wasapi_activate player=evil " + strings.Repeat("x", 16*1024) + "\n"
	reader := &fixedChunksReader{
		chunks: [][]byte{
			[]byte("2026/11/27 [helper] normal line 1\n[mbx-r"),
			[]byte("um] kind=wasapi_activate player=qqmusic signal_state=wasapi_activate_failed activate_stage=initialize hresult=0x88890008\r\n"),
			// 相同边沿重复发送应去重
			[]byte("[mbx-rum] kind=wasapi_activate player=qqmusic signal_state=wasapi_activate_failed activate_stage=initialize hresult=0x88890008\n"),
			[]byte("[helper] bracket normal line\n"),
			[]byte("[mbx-rum] kind=wasapi_activate player=cloudmusicv3 "),
			[]byte("signal_state=wasapi_wait_timeout_2s activate_stage=wait_2s hresult=0x00000102\n"),
			[]byte(oversizedDiag),
			[]byte("2026/11/27 [helper] normal line 2\n"),
		},
	}

	pumpStderrWithHook(reader, &tail, &out, tr.handleDiagLine)

	wantOut := "2026/11/27 [helper] normal line 1\n[helper] bracket normal line\n2026/11/27 [helper] normal line 2\n"
	if got := out.String(); got != wantOut {
		t.Errorf("stderr 转发内容被污染或丢失：\ngot:  %q\nwant: %q", got, wantOut)
	}
	if snap := tail.Snapshot(); strings.Contains(snap, "[mbx-rum]") || strings.Contains(snap, "wasapi_activate") || snap != wantOut {
		t.Errorf("tailBuffer 被 [mbx-rum] 污染或失真：\ngot:  %q\nwant: %q", snap, wantOut)
	}

	rum.Flush(2 * time.Second)

	var gotFailed, gotTimeout int
	for _, ev := range col.snapshot() {
		if ev.Event != "audio_signal_state" {
			continue
		}
		switch ev.Properties["signal_state"] {
		case "wasapi_activate_failed":
			gotFailed++
			if ev.Properties["player"] != "qqmusic" || ev.Properties["activate_stage"] != "initialize" || ev.Properties["hresult"] != "0x88890008" {
				t.Errorf("wasapi_activate_failed 属性异常：%#v", ev.Properties)
			}
		case "wasapi_wait_timeout_2s":
			gotTimeout++
			if ev.Properties["player"] != "cloudmusicv3" || ev.Properties["activate_stage"] != "wait_2s" || ev.Properties["hresult"] != "0x00000102" {
				t.Errorf("wasapi_wait_timeout_2s 属性异常：%#v", ev.Properties)
			}
		default:
			t.Errorf("出现未预期或畸形信号状态：%#v", ev.Properties)
		}
	}
	if gotFailed != 1 || gotTimeout != 1 {
		t.Fatalf("期望 wasapi_activate_failed=1(去重) 与 wasapi_wait_timeout_2s=1，实际 failed=%d timeout=%d", gotFailed, gotTimeout)
	}
}

func TestCaptureSessionGraceFallbackAndHelperEnd(t *testing.T) {
	col := setupManagerRUMCollector(t)

	tr := rumTracker{summaryGrace: 40 * time.Millisecond}
	t0 := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)

	// 场景 1：stop 后未收到 capture_summary，宽限期满由兜底定时器上报 1 次（带 stats_incomplete=true），迟到 summary 不重复上报
	tr.onCaptureStart(1, 1, "spotify", t0, false, true)
	tr.handleDiagLineForHelper(1, "[mbx-rum] kind=capture_stats player=spotify sid=1 norm_frames=100 capped_frames=10 discontinuities=1 frames_dropped=2")
	tr.onCaptureStop("spotify", t0.Add(8*time.Second), true)
	time.Sleep(120 * time.Millisecond)
	tr.handleDiagLineForHelper(1, "[mbx-rum] kind=capture_summary player=spotify sid=1 norm_frames=120 capped_frames=12 discontinuities=2 frames_dropped=3")

	// 场景 2：helper 异常退出时 onHelperEnd(waitSummary=false) 立即结算在途捕获，保留周期 capture_stats 数据并标记 stats_incomplete=true
	tr.onCaptureStart(2, 2, "kuwo", t0, true, false)
	tr.handleDiagLineForHelper(2, "[mbx-rum] kind=capture_stats player=kuwo sid=2 norm_frames=0 capped_frames=0 discontinuities=4 frames_dropped=5")
	tr.onHelperEnd(2, t0.Add(12*time.Second), false)

	rum.Flush(2 * time.Second)

	var spotifyEnds, kuwoEnds int
	for _, ev := range col.snapshot() {
		if ev.Event != "audio_capture_session" || ev.Properties["event_kind"] != "session_end" {
			continue
		}
		switch ev.Properties["player"] {
		case "spotify":
			spotifyEnds++
			if ev.Properties["ttff_ms"] != float64(-1) || ev.Properties["discontinuity_count"] != float64(1) || ev.Properties["frames_dropped"] != float64(2) || ev.Properties["stats_incomplete"] != true {
				t.Errorf("spotify 宽限兜底属性异常：%#v", ev.Properties)
			}
		case "kuwo":
			kuwoEnds++
			if ev.Properties["duration_sec"] != float64(12) || ev.Properties["discontinuity_count"] != float64(4) || ev.Properties["frames_dropped"] != float64(5) || ev.Properties["stats_incomplete"] != true {
				t.Errorf("kuwo helper 结束结算属性异常：%#v", ev.Properties)
			}
		}
	}
	if spotifyEnds != 1 || kuwoEnds != 1 {
		t.Fatalf("期望 spotify=1 与 kuwo=1 次 session_end，实际 spotify=%d kuwo=%d", spotifyEnds, kuwoEnds)
	}
}

// TestTeardownGracefulSummaryAndStatsIncomplete 覆盖 R1：
// 1) 最后订阅离开触发正常 teardown（尚无任何周期 capture_stats），摘要仍在途经 stderr 到达 → 完整非零指标 + stats_incomplete=false；
// 2) 强杀/超时始终无摘要 → stats_incomplete=true。
func TestTeardownGracefulSummaryAndStatsIncomplete(t *testing.T) {
	col := setupManagerRUMCollector(t)

	tr := rumTracker{summaryGrace: 80 * time.Millisecond}
	t0 := time.Date(2026, 11, 27, 15, 30, 0, 0, time.UTC)

	// 场景 A：短会话（无周期 capture_stats）正常拆除，摘要稍后到达
	tr.onCaptureStart(1, 10, "wesing", t0, true, true)
	tr.onFrame("wesing", frame.FlagSessionKnown|frame.FlagSessionActive, t0.Add(50*time.Millisecond), func(string) bool { return true })
	tr.onHelperEnd(1, t0.Add(6*time.Second), true)
	tr.handleDiagLineForHelper(1, "[mbx-rum] kind=capture_summary player=wesing sid=10 norm_frames=100 capped_frames=40 discontinuities=2 frames_dropped=3")

	// 场景 B：正常拆除但 helper 被强杀从未发回摘要，宽限超时后应带 stats_incomplete=true
	tr.onCaptureStart(2, 11, "qqmusic", t0, true, true)
	tr.onHelperEnd(2, t0.Add(4*time.Second), true)
	time.Sleep(150 * time.Millisecond)

	rum.Flush(2 * time.Second)

	var gotWesing, gotQQ map[string]any
	for _, ev := range col.snapshot() {
		if ev.Event != "audio_capture_session" || ev.Properties["event_kind"] != "session_end" {
			continue
		}
		switch ev.Properties["player"] {
		case "wesing":
			gotWesing = ev.Properties
		case "qqmusic":
			gotQQ = ev.Properties
		}
	}
	if gotWesing == nil || gotQQ == nil {
		t.Fatalf("缺少 session_end 事件：wesing=%#v qqmusic=%#v", gotWesing, gotQQ)
	}
	ratio, _ := gotWesing["gain_capped_ratio"].(float64)
	if math.Abs(ratio-0.4) > 1e-6 || gotWesing["discontinuity_count"] != float64(2) || gotWesing["frames_dropped"] != float64(3) || gotWesing["stats_incomplete"] != false {
		t.Errorf("正常拆除在途摘要应完整落账且 stats_incomplete=false：%#v", gotWesing)
	}
	if gotQQ["stats_incomplete"] != true || gotQQ["discontinuity_count"] != float64(0) {
		t.Errorf("无最终摘要会话必须标记 stats_incomplete=true：%#v", gotQQ)
	}
}

// TestStaleSessionSummaryDoesNotContaminateNewSession 覆盖 R2：
// start(wesing, sid=1) → stop(waitSummary=true) → start(wesing, sid=2) → 投递旧 sid=1 与旧 helperGen 摘要（discontinuities=9）→
// 停止新会话 sid=2，断言新会话断流数为 0 且 stats_incomplete=true（不被误置 hasFinalSummary）。
func TestStaleSessionSummaryDoesNotContaminateNewSession(t *testing.T) {
	col := setupManagerRUMCollector(t)

	tr := rumTracker{summaryGrace: 40 * time.Millisecond}
	t0 := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)

	tr.onCaptureStart(1, 1, "wesing", t0, true, true)
	tr.onCaptureStop("wesing", t0.Add(5*time.Second), true)
	// 等旧会话宽限兜底结算完成（同时再构造一个尚未超时的跨 helper 迟到摘要）
	time.Sleep(80 * time.Millisecond)

	// 同名新会话在同 helper 或新 helper 下启动
	tr.onCaptureStart(2, 2, "wesing", t0.Add(6*time.Second), true, true)
	// 迟到的旧 sid=1（同 gen=2 错 sid 与旧 gen=1 旧 sid）均不得污染新会话 sid=2
	tr.handleDiagLineForHelper(2, "[mbx-rum] kind=capture_summary player=wesing sid=1 norm_frames=50 capped_frames=50 discontinuities=9 frames_dropped=8")
	tr.handleDiagLineForHelper(1, "[mbx-rum] kind=capture_summary player=wesing sid=2 norm_frames=50 capped_frames=50 discontinuities=9 frames_dropped=8")
	tr.handleDiagLineForHelper(1, "[mbx-rum] kind=capture_stats player=wesing sid=2 norm_frames=50 capped_frames=50 discontinuities=9 frames_dropped=8")

	// 立即停止新会话（无最终摘要）
	tr.onCaptureStop("wesing", t0.Add(16*time.Second), false)

	rum.Flush(2 * time.Second)

	var newSess map[string]any
	for _, ev := range col.snapshot() {
		if ev.Event == "audio_capture_session" && ev.Properties["event_kind"] == "session_end" &&
			ev.Properties["player"] == "wesing" && ev.Properties["duration_sec"] == float64(10) {
			newSess = ev.Properties
		}
	}
	if newSess == nil {
		t.Fatalf("未找到新会话（duration_sec=10）的 session_end，全部事件：%#v", col.snapshot())
	}
	if newSess["discontinuity_count"] != float64(0) || newSess["frames_dropped"] != float64(0) || newSess["gain_capped_ratio"] != float64(0) || newSess["stats_incomplete"] != true {
		t.Errorf("旧摘要污染了同名新会话：%#v", newSess)
	}
}

// TestCaptureSummaryIncompleteFlagPreservesStatsAndMarksIncomplete 验证 helper 排空超时回传 incomplete=1 时，
// manager 照常更新统计字段、及时停定时器立即结算（不拖延），但保持 stats_incomplete=true；
// 正常排空回传 incomplete=0 时，准确标记 stats_incomplete=false。
func TestCaptureSummaryIncompleteFlagPreservesStatsAndMarksIncomplete(t *testing.T) {
	col := setupManagerRUMCollector(t)

	// 故意把宽限期设长（5s），验证收到 incomplete=1 后立即清定时器结算而非拖延到超时
	tr := rumTracker{summaryGrace: 5 * time.Second}
	t0 := time.Date(2026, 11, 27, 17, 0, 0, 0, time.UTC)

	// 场景 1：stop 后收到 incomplete=1（pendingStop 路径）
	tr.onCaptureStart(1, 101, "wesing", t0, true, true)
	tr.onCaptureStop("wesing", t0.Add(10*time.Second), true)
	tr.handleDiagLineForHelper(1, "[mbx-rum] kind=capture_summary player=wesing sid=101 norm_frames=200 capped_frames=50 discontinuities=3 frames_dropped=7 incomplete=1")

	tr.mu.Lock()
	remPending := len(tr.pendingStop)
	tr.mu.Unlock()
	if remPending != 0 {
		t.Fatalf("收到 incomplete=1 摘要后应立即清除 pendingStop 定时器并结算，剩余 pendingStop=%d", remPending)
	}

	// 场景 2：stop 前就收到 incomplete=1（活跃 sessions 路径），随后 stop(waitSummary=true) 应立即结算且 stats_incomplete=true
	tr.onCaptureStart(1, 102, "qqmusic", t0, true, true)
	tr.handleDiagLineForHelper(1, "[mbx-rum] kind=capture_summary player=qqmusic sid=102 norm_frames=100 capped_frames=20 discontinuities=2 frames_dropped=4 incomplete=1")
	tr.onCaptureStop("qqmusic", t0.Add(8*time.Second), true)

	tr.mu.Lock()
	remPending = len(tr.pendingStop)
	tr.mu.Unlock()
	if remPending != 0 {
		t.Fatalf("已收 incomplete=1 摘要后 stop 不应再挂 pendingStop 定时器拖延，剩余 pendingStop=%d", remPending)
	}

	// 场景 3：正常排空 incomplete=0（pendingStop 路径）→ stats_incomplete=false
	tr.onCaptureStart(1, 103, "kugou", t0, true, true)
	tr.onCaptureStop("kugou", t0.Add(12*time.Second), true)
	tr.handleDiagLineForHelper(1, "[mbx-rum] kind=capture_summary player=kugou sid=103 norm_frames=100 capped_frames=25 discontinuities=1 frames_dropped=2 incomplete=0")

	rum.Flush(2 * time.Second)

	got := map[string]map[string]any{}
	for _, ev := range col.snapshot() {
		if ev.Event == "audio_capture_session" && ev.Properties["event_kind"] == "session_end" {
			if p, ok := ev.Properties["player"].(string); ok {
				got[p] = ev.Properties
			}
		}
	}

	if w := got["wesing"]; w == nil {
		t.Fatal("缺少 wesing 的 session_end")
	} else {
		ratio, _ := w["gain_capped_ratio"].(float64)
		if math.Abs(ratio-0.25) > 1e-6 || w["discontinuity_count"] != float64(3) || w["frames_dropped"] != float64(7) || w["stats_incomplete"] != true {
			t.Errorf("wesing (incomplete=1 pendingStop) 属性异常：%#v", w)
		}
	}
	if q := got["qqmusic"]; q == nil {
		t.Fatal("缺少 qqmusic 的 session_end")
	} else {
		ratio, _ := q["gain_capped_ratio"].(float64)
		if math.Abs(ratio-0.2) > 1e-6 || q["discontinuity_count"] != float64(2) || q["frames_dropped"] != float64(4) || q["stats_incomplete"] != true {
			t.Errorf("qqmusic (incomplete=1 pre-stop) 属性异常：%#v", q)
		}
	}
	if k := got["kugou"]; k == nil {
		t.Fatal("缺少 kugou 的 session_end")
	} else {
		ratio, _ := k["gain_capped_ratio"].(float64)
		if math.Abs(ratio-0.25) > 1e-6 || k["discontinuity_count"] != float64(1) || k["frames_dropped"] != float64(2) || k["stats_incomplete"] != false {
			t.Errorf("kugou (incomplete=0) 属性异常：%#v", k)
		}
	}
}

// TestOnFrameSkipsPlayingQueryAndASIOWhenRUMDisabled 验证 !rum.Enabled() 时 onFrame
// 完全跳过 playingFn 调用与 ASIO 判定，但仍原子更新 frameSeq 与首帧 ttff_ms。
func TestOnFrameSkipsPlayingQueryAndASIOWhenRUMDisabled(t *testing.T) {
	restore := rum.OverrideCredentialsForTest("", "")
	t.Cleanup(restore)
	rum.Init(rum.Options{IsRelease: false})
	if rum.Enabled() {
		t.Fatal("预期当前 RUM 为禁用态")
	}

	var tr rumTracker
	t0 := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
	tr.onCaptureStart(1, 1, "kugou", t0, true, true)

	var playingCalls atomic.Int64
	playingFn := func(string) bool {
		playingCalls.Add(1)
		return true
	}

	tr.onFrame("kugou", frame.FlagSessionKnown, t0.Add(120*time.Millisecond), playingFn)
	tr.onFrame("kugou", frame.FlagSessionKnown, t0.Add(6*time.Second), playingFn)
	tr.onFrame("kugou", frame.FlagSessionKnown, t0.Add(12*time.Second), playingFn)

	if calls := playingCalls.Load(); calls != 0 {
		t.Fatalf("RUM 禁用时 playingFn 被调用 %d 次，期望 0", calls)
	}
	hot := tr.hotState("kugou")
	if got := hot.asioState.Load(); got != nil {
		t.Fatalf("RUM 禁用时不应更新 ASIO 跟踪态，实际 %#v", got)
	}
	if seq := hot.frameSeq.Load(); seq != 3 {
		t.Fatalf("frameSeq = %d，期望 3", seq)
	}
	if sess := hot.currentSess.Load(); sess == nil || sess.ttffMs.Load() != 120 {
		t.Fatalf("首帧 ttff_ms 未正确记录：sess=%#v", sess)
	}
}

// TestOnFrameClearsWasapiErrEdgeCacheWhenRUMEnabled 验证 rum.Enabled()==true 时，
// onFrame 不获取 t.mu 仍能通过帧序列推进清除 lastWasapiErr 边沿缓存：
// 连续同错误先去重，期间收到有效帧后再次发生同错误允许重新上报，再发同错误则再次去重。
func TestOnFrameClearsWasapiErrEdgeCacheWhenRUMEnabled(t *testing.T) {
	col := setupManagerRUMCollector(t)

	var tr rumTracker
	t0 := time.Date(2026, 11, 27, 18, 10, 0, 0, time.UTC)
	diagLine := "[mbx-rum] kind=wasapi_activate player=qqmusic signal_state=wasapi_activate_failed activate_stage=initialize hresult=0x88890008"

	// 1. 首次错误上报，紧接重复错误去重。
	tr.handleDiagLineForHelper(1, diagLine)
	tr.handleDiagLineForHelper(1, diagLine)

	// 2. 收到有效帧推进 frameSeq，等效清除边沿缓存。
	tr.onFrame("qqmusic", frame.FlagSessionKnown|frame.FlagSessionActive, t0, func(string) bool { return true })

	// 3. 再次发生相同错误应重新上报 1 次，紧接再发则再次去重。
	tr.handleDiagLineForHelper(1, diagLine)
	tr.handleDiagLineForHelper(1, diagLine)

	rum.Flush(2 * time.Second)

	count := 0
	for _, ev := range col.snapshot() {
		if ev.Event == "audio_signal_state" &&
			ev.Properties["player"] == "qqmusic" &&
			ev.Properties["signal_state"] == "wasapi_activate_failed" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("期望收到 2 次 wasapi_activate_failed（首报 1 次 + 帧恢复后再报 1 次），实际 %d 次：%#v", count, col.snapshot())
	}
}
