package effect

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/cloudmusic/park"
	"Metabox-Nexus-PlayerCap/rum"
)

type effectRUMBatch struct {
	Batch []struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	} `json:"batch"`
}

type effectRUMEvent struct {
	Event string
	Props map[string]any
}

type fakeEffectSink struct {
	strategy    string
	ingest403   uint64
	subscribers bool
}

func (f *fakeEffectSink) HasEffectSubscribers() bool  { return f.subscribers }
func (f *fakeEffectSink) BroadcastEffectFrame([]byte) {}
func (f *fakeEffectSink) EffectCaptureParams() (int, float64, bool, bool, uint64) {
	return 88, 1, false, false, 1
}
func (f *fakeEffectSink) SetEffectShowing(bool)               {}
func (f *fakeEffectSink) EffectStrategy() string              { return f.strategy }
func (f *fakeEffectSink) TakeManualParkCmd() int              { return -1 }
func (f *fakeEffectSink) SetEffectIngestHandler(func([]byte)) {}
func (f *fakeEffectSink) EffectIngestWSURL() string           { return "ws://127.0.0.1:8765/ingest" }
func (f *fakeEffectSink) DrainEffectIngest403() uint64        { v := f.ingest403; f.ingest403 = 0; return v }

func startEffectRUMCollector(t *testing.T) func() (string, []effectRUMEvent) {
	t.Helper()
	var mu sync.Mutex
	var raw []string
	var evs []effectRUMEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/flags"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"featureFlags":{"playercap-rum-config":true},"featureFlagPayloads":{"playercap-rum-config":"{\"enabled\":true}"}}`)
		case strings.HasPrefix(r.URL.Path, "/batch"):
			body, _ := io.ReadAll(r.Body)
			var parsed effectRUMBatch
			_ = json.Unmarshal(body, &parsed)
			mu.Lock()
			raw = append(raw, string(body))
			for _, ev := range parsed.Batch {
				evs = append(evs, effectRUMEvent{Event: ev.Event, Props: ev.Properties})
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	restore := rum.OverrideCredentialsForTest("phc_test_effect", srv.URL)
	t.Cleanup(restore)
	rum.Init(rum.Options{
		IsRelease:  true,
		AppVersion: "4.0.0",
		DistinctID: "0123456789abcdef0123456789abcdef",
		HTTPClient: srv.Client(),
	})
	rum.Start()
	t.Cleanup(func() { rum.Shutdown(time.Second) })

	return func() (string, []effectRUMEvent) {
		rum.Flush(2 * time.Second)
		mu.Lock()
		defer mu.Unlock()
		out := make([]effectRUMEvent, len(evs))
		copy(out, evs)
		return strings.Join(raw, "\n"), out
	}
}

// TestEffectMirrorSessionRUMAndReadOnlyStatsJS 验证 effect_mirror_session 指标聚合、
// 源 canvas 统计表达式纯只读（绝不改 width/height/style，AGENTS.md §0.2）与 FlushRUM 幂等完成屏障。
func TestEffectMirrorSessionRUMAndReadOnlyStatsJS(t *testing.T) {
	flush := startEffectRUMCollector(t)

	// 1. 静态断言：采集 JS 表达式绝不对源 canvas 的 width/height/style 做赋值写入
	for _, forbidden := range []string{"c.width=", "c.height=", "c.style", ".setAttribute"} {
		if strings.Contains(strings.ReplaceAll(effectStatsExprJS, " ", ""), forbidden) {
			t.Fatalf("effectStatsExprJS 违反 AGENTS.md §0.2 禁止写源 canvas：%q", forbidden)
		}
	}

	sink := &fakeEffectSink{strategy: "park", ingest403: 3, subscribers: true}
	cap := New(sink, func() bool { return true })
	park.MarkOrphanedRestoredForTest(true)
	t.Cleanup(func() { park.MarkOrphanedRestoredForTest(false) })

	sess := cap.registerEffectSessionRUM("purelayer", nil)

	// 模拟 400ms 探活只读快照采集与泊车
	sess.onShowingChange(true, time.Now().Add(-2*time.Second))
	sess.onPark()
	sess.onPurelayerStats(2560, 1440, 90, 10, 4)

	// 注入完成屏障测试：并发两次 FlushRUM 均等待首次结算完毕再返回且只发 1 条事件
	inEmit := make(chan struct{})
	releaseEmit := make(chan struct{})
	sess.mu.Lock()
	sess.testBeforeEmit = func() {
		close(inEmit)
		<-releaseEmit
	}
	sess.mu.Unlock()

	done1 := make(chan struct{})
	done2 := make(chan struct{})
	go func() { cap.FlushRUM(); close(done1) }()
	<-inEmit
	go func() { cap.FlushRUM(); close(done2) }()

	select {
	case <-done2:
		t.Fatal("并发 FlushRUM 在首次结算未完成前提前返回，违背完成屏障")
	case <-time.After(40 * time.Millisecond):
	}
	close(releaseEmit)
	<-done1
	<-done2

	raw, evs := flush()
	if strings.Contains(raw, "ws://127.0.0.1") || strings.Contains(raw, "orpheus://") {
		t.Errorf("effect_mirror_session 不应泄露内部 URL：%s", raw)
	}

	var count int
	for _, ev := range evs {
		if ev.Event != "effect_mirror_session" {
			continue
		}
		count++
		if ev.Props["mode"] != "purelayer" ||
			ev.Props["strategy"] != "park" ||
			ev.Props["park_count"] != float64(1) ||
			ev.Props["orphaned_restored"] != true ||
			ev.Props["canvas_w"] != float64(2560) ||
			ev.Props["canvas_h"] != float64(1440) ||
			ev.Props["downsampled_1920"] != true ||
			ev.Props["stale_dropped_frames"] != float64(4) ||
			ev.Props["ingest_forbidden_403"] != float64(3) ||
			ev.Props["sync_fallback_ratio"] != float64(0.1) {
			t.Errorf("effect_mirror_session 属性异常：%#v", ev.Props)
		}
	}
	if count != 1 {
		t.Fatalf("期望 1 条 effect_mirror_session，实得 %d 条", count)
	}
}

// TestEffectMirrorSessionFPSUnifiedScope 回归验证：显示 10s（广播 300 帧）后隐藏 90s（页面统计增至 3000 帧），
// effective_fps 须按广播帧数/有效显示时长同口径算出约 30.0（而非 300.0），且 sync_fallback_ratio 准确。
func TestEffectMirrorSessionFPSUnifiedScope(t *testing.T) {
	flush := startEffectRUMCollector(t)

	sink := &fakeEffectSink{strategy: "park", subscribers: true}
	cap := New(sink, func() bool { return false })
	sess := cap.registerEffectSessionRUM("purelayer", func(s *effectSessionRUM) {
		now := time.Now()
		s.startedAt = now.Add(-100 * time.Second)
		s.showing = false
		s.showingSince = time.Time{}
		s.showingActiveDur = 10 * time.Second
		s.broadcastFrames = 300
	})
	sess.onPurelayerStats(1920, 1080, 2700, 300, 2)

	cap.FlushRUM()

	_, evs := flush()
	var got map[string]any
	for _, ev := range evs {
		if ev.Event == "effect_mirror_session" {
			got = ev.Props
		}
	}
	if got == nil {
		t.Fatal("未收到 effect_mirror_session 事件")
	}
	fps, _ := got["effective_fps"].(float64)
	if fps < 29.5 || fps > 30.5 {
		t.Fatalf("effective_fps = %v，期望约 30.0（不得误算为 300.0）；props=%#v", fps, got)
	}
	if got["sync_fallback_ratio"] != float64(0.1) {
		t.Fatalf("sync_fallback_ratio = %#v，期望 0.1", got["sync_fallback_ratio"])
	}
}

type counting403Sink struct {
	fakeEffectSink
	val   atomic.Uint64
	calls atomic.Int64
}

func (s *counting403Sink) DrainEffectIngest403() uint64 {
	s.calls.Add(1)
	return s.val.Swap(0)
}

// TestEffectMirrorSession403DrainRace 回归验证：defer 与 FlushRUM 并发竞争进入 end 时，
// 仅唯一胜出方调用 1 次 DrainEffectIngest403()，竞争失败方绝不提前消费 403 计数导致清零丢失。
func TestEffectMirrorSession403DrainRace(t *testing.T) {
	flush := startEffectRUMCollector(t)

	sink := &counting403Sink{fakeEffectSink: fakeEffectSink{strategy: "park", subscribers: true}}
	sink.val.Store(3)

	cap := New(sink, func() bool { return true })
	sess := cap.registerEffectSessionRUM("purelayer", nil)

	// 让模拟 defer 的首次 end 抢到结算权后先卡在 testBeforeEmit，再并发进入 FlushRUM（竞争失败方）
	inWinner := make(chan struct{})
	releaseWinner := make(chan struct{})
	sess.mu.Lock()
	sess.testBeforeEmit = func() {
		close(inWinner)
		<-releaseWinner
	}
	sess.mu.Unlock()

	winnerDone := make(chan struct{})
	go func() {
		defer close(winnerDone)
		cap.endEffectSessionRUM(sess)
	}()
	<-inWinner

	loserDone := make(chan struct{})
	go func() {
		defer close(loserDone)
		cap.FlushRUM()
	}()
	// 给竞争失败方足够时间执行到 <-done，若它在进入 end 前误调 drainIngest403 会在此刻把 3 消费掉
	time.Sleep(40 * time.Millisecond)
	close(releaseWinner)
	<-winnerDone
	<-loserDone

	_, evs := flush()
	if gotCalls := sink.calls.Load(); gotCalls != 1 {
		t.Fatalf("DrainEffectIngest403 被调用了 %d 次，期望恰好 1 次", gotCalls)
	}
	var count int
	for _, ev := range evs {
		if ev.Event != "effect_mirror_session" {
			continue
		}
		count++
		if ev.Props["ingest_forbidden_403"] != float64(3) {
			t.Fatalf("ingest_forbidden_403 = %#v，期望 3（不得因并发竞争被清零丢失）", ev.Props["ingest_forbidden_403"])
		}
	}
	if count != 1 {
		t.Fatalf("期望 1 条 effect_mirror_session，实得 %d 条", count)
	}
}

// TestEffectRUMGateRejectsPostFlushAndWaitsInFlightRegister 回归测试：
// 1) FlushRUM 置位后拒绝新特效会话登记，nil 会话方法全部安全且零孤儿事件；
// 2) 登记阶段暂停时并发 FlushRUM 必须等待登记初始化完成后再结算产出完整 effect_mirror_session。
func TestEffectRUMGateRejectsPostFlushAndWaitsInFlightRegister(t *testing.T) {
	t.Run("Flush后拒绝新登记且nil安全", func(t *testing.T) {
		flush := startEffectRUMCollector(t)

		sink := &fakeEffectSink{strategy: "park", ingest403: 2, subscribers: true}
		cap := New(sink, func() bool { return true })
		cap.FlushRUM()

		sess := cap.registerEffectSessionRUM("purelayer", func(s *effectSessionRUM) {
			s.initPurelayerBaseline(1920, 1080, 10, 1, 0)
		})
		if sess != nil {
			t.Fatalf("FlushRUM 后 registerEffectSessionRUM 应返回 nil，实得 %#v", sess)
		}
		sess.onShowingChange(true, time.Now())
		sess.onPark()
		sess.onBroadcastFrame()
		sess.onPurelayerStats(1920, 1080, 20, 2, 1)
		cap.endEffectSessionRUM(sess)

		_, evs := flush()
		for _, ev := range evs {
			if ev.Event == "effect_mirror_session" {
				t.Fatalf("FlushRUM 后拒绝的会话不应发出 effect_mirror_session：%#v", ev)
			}
		}
	})

	t.Run("登记中并发Flush等待初始化完成后完整结算", func(t *testing.T) {
		flush := startEffectRUMCollector(t)

		sink := &fakeEffectSink{strategy: "park", ingest403: 1, subscribers: true}
		cap := New(sink, func() bool { return true })
		inAttach := make(chan struct{})
		releaseAttach := make(chan struct{})
		cap.testBeforeAttach = func() {
			close(inAttach)
			<-releaseAttach
		}

		regDone := make(chan *effectSessionRUM, 1)
		go func() {
			regDone <- cap.registerEffectSessionRUM("purelayer", func(s *effectSessionRUM) {
				s.initPurelayerBaseline(2560, 1440, 5, 1, 0)
			})
		}()
		<-inAttach

		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			cap.FlushRUM()
		}()
		select {
		case <-flushDone:
			t.Fatal("会话登记屏障尚未释放，FlushRUM 不得提前返回")
		case <-time.After(40 * time.Millisecond):
		}

		close(releaseAttach)
		sess := <-regDone
		if sess == nil {
			t.Fatal("前置：并发 Flush 前已占位的 registerEffectSessionRUM 应返回非 nil 会话")
		}
		<-flushDone
		cap.endEffectSessionRUM(sess)

		_, evs := flush()
		var count int
		for _, ev := range evs {
			if ev.Event != "effect_mirror_session" {
				continue
			}
			count++
			if ev.Props["canvas_w"] != float64(2560) || ev.Props["canvas_h"] != float64(1440) || ev.Props["ingest_forbidden_403"] != float64(1) {
				t.Errorf("effect_mirror_session 属性异常：%#v", ev.Props)
			}
		}
		if count != 1 {
			t.Fatalf("期望恰好 1 条 effect_mirror_session，实得 %d 条", count)
		}
	})
}
