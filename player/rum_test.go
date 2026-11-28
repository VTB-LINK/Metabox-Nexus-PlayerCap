package player

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/rum"
)

// TestBaseEmitterDropCountsInRUM 验证 BaseEmitter 事件通道满丢包计入 rum.DrainDropped() 的 player_event_ch。
func TestBaseEmitterDropCountsInRUM(t *testing.T) {
	// 清空前置残留丢包账单
	_, _ = rum.DrainDropped()

	b := NewBaseEmitter("testplayer")
	for i := 0; i < cap(b.EventCh); i++ {
		if !b.TryEmit(EventLyricUpdate, i) {
			t.Fatalf("第 %d 条事件应成功入队", i)
		}
	}
	// 再连续发送 3 条，均应非阻塞丢弃
	if b.TryEmit(EventLyricUpdate, "drop1") {
		t.Fatal("通道满时 TryEmit 应返回 false")
	}
	b.Emit(EventLyricUpdate, "drop2")
	b.Emit(EventStatusUpdate, &StatusInfo{Status: "playing", Detail: "Secret Song"})

	_, byScope := rum.DrainDropped()
	if got := byScope["player_event_ch"]; got != 3 {
		t.Fatalf("player_event_ch 丢包计数 = %d，期望 3（全部账单 %v）", got, byScope)
	}
	// Drain 后应归零
	_, after := rum.DrainDropped()
	if after["player_event_ch"] != 0 {
		t.Fatalf("DrainDropped 后应清零，实得 %v", after)
	}
}

// TestTracksRecognizedAtomicCounter 验证 NoteTrackRecognized 与 TracksRecognizedTotal 并发原子计数准确。
func TestTracksRecognizedAtomicCounter(t *testing.T) {
	before := TracksRecognizedTotal()
	const workers = 4
	const perWorker = 25
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				NoteTrackRecognized()
			}
		}()
	}
	wg.Wait()
	if got := TracksRecognizedTotal() - before; got != workers*perWorker {
		t.Fatalf("TracksRecognizedTotal 增量 = %d，期望 %d", got, workers*perWorker)
	}
}

// TestCoverDataURIWithOutcomeClassification 验证 800x800 透传、非 800 缩放、非法字节与各类规范化失败回退枚举。
func TestCoverDataURIWithOutcomeClassification(t *testing.T) {
	t.Run("空字节返回 none", func(t *testing.T) {
		uri, out := CoverDataURIWithOutcome("Test", nil, "image/jpeg")
		if uri != "" || out.Action != CoverActionNone || out.FallbackReason != "" {
			t.Fatalf("got uri=%q out=%+v", uri, out)
		}
	})

	t.Run("800x800 原图走 passthrough_800", func(t *testing.T) {
		raw := photoJPEG(t, 800, 800)
		uri, out := CoverDataURIWithOutcome("Test", raw, "image/png")
		if uri == "" || out.Action != CoverActionPassthrough800 || out.FallbackReason != "" {
			t.Fatalf("got len(uri)=%d out=%+v", len(uri), out)
		}
	})

	t.Run("非 800 原图走 resized_800", func(t *testing.T) {
		raw := photoPNG(t, 400, 300)
		uri, out := CoverDataURIWithOutcome("Test", raw, "image/png")
		if uri == "" || out.Action != CoverActionResized800 || out.FallbackReason != "" {
			t.Fatalf("got len(uri)=%d out=%+v", len(uri), out)
		}
	})

	t.Run("非法字节走 fallback_raw 与 decode_config_err", func(t *testing.T) {
		uri, out := CoverDataURIWithOutcome("Test", []byte("not-a-valid-image"), "image/jpeg")
		if uri == "" || out.Action != CoverActionFallbackRaw || out.FallbackReason != "decode_config_err" {
			t.Fatalf("got len(uri)=%d out=%+v", len(uri), out)
		}
	})

	t.Run("超尺寸走 dimension_out_of_range", func(t *testing.T) {
		raw := withSOFSize(t, photoJPEG(t, 16, 16), 2000, 2000)
		uri, out := CoverDataURIWithOutcome("Test", raw, "image/jpeg")
		if uri == "" || out.Action != CoverActionFallbackRaw || out.FallbackReason != "dimension_out_of_range" {
			t.Fatalf("got len(uri)=%d out=%+v", len(uri), out)
		}
	})

	t.Run("CMYK 走 cmyk_jpeg", func(t *testing.T) {
		raw := progressiveCMYKHead(32, 32)
		uri, out := CoverDataURIWithOutcome("Test", raw, "image/jpeg")
		if uri == "" || out.Action != CoverActionFallbackRaw || out.FallbackReason != "cmyk_jpeg" {
			t.Fatalf("got len(uri)=%d out=%+v", len(uri), out)
		}
	})

	t.Run("渐进扫描超限走 scan_budget_exceeded", func(t *testing.T) {
		raw := progressiveGray(32, 32, maxJPEGScans+2, 0)
		uri, out := CoverDataURIWithOutcome("Test", raw, "image/jpeg")
		if uri == "" || out.Action != CoverActionFallbackRaw || out.FallbackReason != "scan_budget_exceeded" {
			t.Fatalf("got len(uri)=%d out=%+v", len(uri), out)
		}
	})
}

// TestFetchCoverBase64ContextWithOutcomeNoURLLeak 验证网络封面下载各分支结果枚举且零 URL 泄露。
func TestFetchCoverBase64ContextWithOutcomeNoURLLeak(t *testing.T) {
	origClient := newCoverClient
	t.Cleanup(func() { newCoverClient = origClient })

	imgBytes := photoJPEG(t, 200, 200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok-secret-token":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(imgBytes)
		case "/404-secret-token":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	newCoverClient = func(timeout time.Duration) *http.Client {
		c := srv.Client()
		c.Timeout = timeout
		return c
	}

	// 空 URL -> none
	if uri, out := FetchCoverBase64ContextWithOutcome(context.Background(), "Test", "", time.Second); uri != "" || out.Action != CoverActionNone || out.FallbackReason != "" {
		t.Fatalf("空 URL got uri=%q out=%+v", uri, out)
	}

	// 非法 scheme -> err + invalid_scheme，不含原始 URL
	badURL := "file:///C:/Users/Alice/secret_cover.jpg"
	if uri, out := FetchCoverBase64ContextWithOutcome(context.Background(), "Test", badURL, time.Second); uri != "" || out.Action != CoverActionErr || out.FallbackReason != "invalid_scheme" {
		t.Fatalf("非法 scheme got uri=%q out=%+v", uri, out)
	} else if strings.Contains(out.FallbackReason, "Alice") || strings.Contains(out.FallbackReason, "file://") {
		t.Fatalf("FallbackReason 泄露 URL：%q", out.FallbackReason)
	}

	// 非 200 -> err + non_200，不含原始 URL
	notFoundURL := srv.URL + "/404-secret-token"
	if uri, out := FetchCoverBase64ContextWithOutcome(context.Background(), "Test", notFoundURL, time.Second); uri != "" || out.Action != CoverActionErr || out.FallbackReason != "non_200" {
		t.Fatalf("404 got uri=%q out=%+v", uri, out)
	} else if strings.Contains(out.FallbackReason, "secret") || strings.Contains(out.FallbackReason, srv.URL) {
		t.Fatalf("FallbackReason 泄露 URL：%q", out.FallbackReason)
	}

	// 取消 ctx -> none
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if uri, out := FetchCoverBase64ContextWithOutcome(ctx, "Test", srv.URL+"/ok-secret-token", time.Second); uri != "" || out.Action != CoverActionNone {
		t.Fatalf("已取消 ctx got uri=%q out=%+v", uri, out)
	}

	// 正常下载 200x200 -> resized_800
	if uri, out := FetchCoverBase64ContextWithOutcome(context.Background(), "Test", srv.URL+"/ok-secret-token", time.Second); uri == "" || out.Action != CoverActionResized800 || out.FallbackReason != "" {
		t.Fatalf("正常下载 got len(uri)=%d out=%+v", len(uri), out)
	}
}

// TestSupplementDecisionAndCombine 验证 SupplementDecision 与 SupplementField 语义一致及决策归类。
func TestSupplementDecisionAndCombine(t *testing.T) {
	orig := songInfoCorrect.Load()
	t.Cleanup(func() { songInfoCorrect.Store(orig) })

	// 默认开（song-info-keep-first = true）
	SetSongInfoKeepFirst(true)
	casesKeep := []struct {
		announced, read string
		wantVal         string
		wantDec         string
	}{
		{"A", "B", "A", "kept"},
		{"", "B", "B", "filled_empty"},
		{"A", "A", "A", "unchanged"},
		{"A", "", "A", "unchanged"},
		{"", "", "", "unchanged"},
	}
	for _, tc := range casesKeep {
		val, dec := SupplementDecision(tc.announced, tc.read)
		if val != tc.wantVal || dec != tc.wantDec || val != SupplementField(tc.announced, tc.read) {
			t.Errorf("keep=true (%q,%q) got (%q,%q) want (%q,%q)", tc.announced, tc.read, val, dec, tc.wantVal, tc.wantDec)
		}
	}

	// 开关关（song-info-keep-first = false）
	SetSongInfoKeepFirst(false)
	casesCorrect := []struct {
		announced, read string
		wantVal         string
		wantDec         string
	}{
		{"A", "B", "B", "overwritten"},
		{"", "B", "B", "filled_empty"},
		{"A", "A", "A", "unchanged"},
		{"A", "", "", "overwritten"},
	}
	for _, tc := range casesCorrect {
		val, dec := SupplementDecision(tc.announced, tc.read)
		if val != tc.wantVal || dec != tc.wantDec || val != SupplementField(tc.announced, tc.read) {
			t.Errorf("keep=false (%q,%q) got (%q,%q) want (%q,%q)", tc.announced, tc.read, val, dec, tc.wantVal, tc.wantDec)
		}
	}

	if got := CombineSupplementDecisions("unchanged", "filled_empty"); got != "filled_empty" {
		t.Errorf("Combine(unchanged, filled_empty) = %q", got)
	}
	if got := CombineSupplementDecisions("kept", "overwritten"); got != "overwritten" {
		t.Errorf("Combine(kept, overwritten) = %q", got)
	}
	if got := CombineSupplementDecisions("kept", "unchanged"); got != "kept" {
		t.Errorf("Combine(kept, unchanged) = %q", got)
	}
	if got := CombineSupplementDecisions("unchanged", "unchanged"); got != "unchanged" {
		t.Errorf("Combine(unchanged, unchanged) = %q", got)
	}
}

type fakeGateSession struct {
	mu        sync.Mutex
	ended     bool
	endDone   chan struct{}
	onEnd     func()
	beforeEnd func()
}

func (f *fakeGateSession) end() {
	f.mu.Lock()
	if f.ended {
		done := f.endDone
		f.mu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	if f.endDone == nil {
		f.endDone = make(chan struct{})
	}
	done := f.endDone
	f.ended = true
	before := f.beforeEnd
	onEnd := f.onEnd
	f.mu.Unlock()

	defer close(done)
	if before != nil {
		before()
	}
	if onEnd != nil {
		onEnd()
	}
}

// TestSessionRUMGateRejectsPostFlushAndWaitsAttachEndBarriers 验证通用会话退出闸门的三种精确交错：
// 1) Flush 置位后拒绝新登记（makeSess 与 onAttached 均不执行）；
// 2) Register 锁外发 attached 前暂停时，并发 Flush 等待 attachDone 与 endDone 严格按 attached→detached 成对结算；
// 3) End 抢先进入结算但尚未完成时，并发 Flush 等待其 endDone 完成后才返回。
func TestSessionRUMGateRejectsPostFlushAndWaitsAttachEndBarriers(t *testing.T) {
	t.Run("Flush后拒绝新登记", func(t *testing.T) {
		var gate SessionRUMGate[*fakeGateSession]
		gate.Flush(func(*fakeGateSession) {
			t.Fatal("无活动会话时 Flush 不应调用回调")
		})
		makeCalled := false
		attachCalled := false
		sess, ok := gate.Register(
			func() *fakeGateSession {
				makeCalled = true
				return &fakeGateSession{endDone: make(chan struct{})}
			},
			func(*fakeGateSession) {
				attachCalled = true
			},
		)
		if ok || sess != nil || makeCalled || attachCalled {
			t.Fatalf("Flush 后 Register 应直接拒绝：ok=%v sess=%v make=%v attach=%v", ok, sess, makeCalled, attachCalled)
		}
	})

	t.Run("Register发attached前暂停_并发Flush等待attachDone与endDone成对产出", func(t *testing.T) {
		var gate SessionRUMGate[*fakeGateSession]
		var orderMu sync.Mutex
		var order []string
		record := func(ev string) {
			orderMu.Lock()
			order = append(order, ev)
			orderMu.Unlock()
		}

		inAttach := make(chan struct{})
		releaseAttach := make(chan struct{})
		inDetach := make(chan struct{})
		releaseDetach := make(chan struct{})

		regDone := make(chan *fakeGateSession, 1)
		go func() {
			s, ok := gate.Register(
				func() *fakeGateSession {
					return &fakeGateSession{
						endDone: make(chan struct{}),
						beforeEnd: func() {
							close(inDetach)
							<-releaseDetach
						},
						onEnd: func() {
							record("detached")
						},
					}
				},
				func(*fakeGateSession) {
					close(inAttach)
					<-releaseAttach
					record("attached")
				},
			)
			if !ok {
				regDone <- nil
				return
			}
			regDone <- s
		}()
		<-inAttach

		flush1Done := make(chan struct{})
		go func() {
			defer close(flush1Done)
			gate.Flush(func(s *fakeGateSession) { s.end() })
		}()

		select {
		case <-flush1Done:
			t.Fatal("attached 尚未完成入队，Flush 不得提前返回")
		case <-inDetach:
			t.Fatal("attached 尚未完成入队，Flush 不得提前进入 detached")
		case <-time.After(35 * time.Millisecond):
		}

		// 放行 attached，Flush 应随即进入 detached 并暂停在 inDetach
		close(releaseAttach)
		sess := <-regDone
		if sess == nil {
			t.Fatal("前置：Register 应成功")
		}
		<-inDetach

		// 此时并发第二次 Flush 也必须等待同一个 endDone 屏障
		flush2Done := make(chan struct{})
		go func() {
			defer close(flush2Done)
			gate.Flush(func(s *fakeGateSession) { s.end() })
		}()
		select {
		case <-flush2Done:
			t.Fatal("首次 Flush 尚未完成 detached，并发第二次 Flush 不得提前返回")
		case <-time.After(35 * time.Millisecond):
		}

		close(releaseDetach)
		<-flush1Done
		<-flush2Done

		// 后续 End 必须幂等不重复产出 detached
		gate.End(sess, func(s *fakeGateSession) { s.end() })

		orderMu.Lock()
		got := append([]string(nil), order...)
		orderMu.Unlock()
		if len(got) != 2 || got[0] != "attached" || got[1] != "detached" {
			t.Fatalf("期望严格成对顺序 [attached detached]，实得 %v", got)
		}
	})

	t.Run("End在途结算时并发Flush等待endDone完成", func(t *testing.T) {
		var gate SessionRUMGate[*fakeGateSession]
		inDetach := make(chan struct{})
		releaseDetach := make(chan struct{})
		var detachedCount int
		var mu sync.Mutex

		sess, ok := gate.Register(
			func() *fakeGateSession {
				return &fakeGateSession{
					endDone: make(chan struct{}),
					beforeEnd: func() {
						close(inDetach)
						<-releaseDetach
					},
					onEnd: func() {
						mu.Lock()
						detachedCount++
						mu.Unlock()
					},
				}
			},
			nil,
		)
		if !ok || sess == nil {
			t.Fatal("Register 应成功")
		}

		endDone := make(chan struct{})
		go func() {
			defer close(endDone)
			gate.End(sess, func(s *fakeGateSession) { s.end() })
		}()
		<-inDetach

		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			gate.Flush(func(s *fakeGateSession) { s.end() })
		}()
		select {
		case <-flushDone:
			t.Fatal("End 尚未完成结算，并发 Flush 不得提前返回")
		case <-time.After(35 * time.Millisecond):
		}

		close(releaseDetach)
		<-flushDone
		<-endDone

		mu.Lock()
		cnt := detachedCount
		mu.Unlock()
		if cnt != 1 {
			t.Fatalf("期望 detached 仅产出 1 次，实得 %d", cnt)
		}
	})
}
