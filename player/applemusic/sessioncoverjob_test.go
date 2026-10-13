package applemusic

// 本文件测封面的异步缩放（issue #110）：缩放在飞期间轮询照常、至多一次在飞、对不上当前曲目的结果丢弃、
// 通道里有残留时不认错也不挂住、会话不再收时 goroutine 照样退出。缩放经 coverDataURI seam 换成桩，
// 由用例决定它何时做完。runSession 里会话结束时关闭 coverEnd 的那一句依赖真进程，靠人守。

import (
	"slices"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

// gatedCover 是 coverDataURI 的桩：每次调用记下输入，等 release 放行一次才返回 "uri:"+输入，并记下同一时刻
// 在跑的最大个数。
type gatedCover struct {
	release chan struct{}

	mu      sync.Mutex
	calls   []string
	running int
	peak    int
}

// useGatedCover 换上 gatedCover，并让 harness 的 run 不再等缩放（由用例用 finish 放行）。用例结束时还原
// seam、放走还挡着的调用。
func useGatedCover(t *testing.T, h *harness) *gatedCover {
	g := &gatedCover{release: make(chan struct{})}
	orig := coverDataURI
	coverDataURI = func(_ string, b []byte, _ string) string {
		g.mu.Lock()
		g.calls = append(g.calls, string(b))
		g.running++
		g.peak = max(g.peak, g.running)
		g.mu.Unlock()
		<-g.release
		g.mu.Lock()
		g.running--
		g.mu.Unlock()
		return "uri:" + string(b)
	}
	h.manualCover = true
	t.Cleanup(func() {
		coverDataURI = orig
		close(g.release)
	})
	return g
}

// inputs 等到会话已起的各次缩放都进了桩（goroutine 起了未必已被调度到），返回各次的输入；peakRunning 返回
// 同一时刻在跑的最大个数。
func (g *gatedCover) inputs(h *harness) []string {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		g.mu.Lock()
		got := len(g.calls)
		g.mu.Unlock()
		if got >= int(h.s.coverSeq) {
			break
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

func (g *gatedCover) peakRunning() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

// finish 放行在跑的那一次，并等它把结果送进通道：下一轮 tick 落地。
func (g *gatedCover) finish(h *harness) {
	h.t.Helper()
	select {
	case g.release <- struct{}{}:
	case <-time.After(5 * time.Second):
		h.t.Fatal("没有在跑的缩放可放行")
	}
	select {
	case r := <-h.s.coverCh:
		h.s.coverCh <- r
	case <-time.After(5 * time.Second):
		h.t.Fatal("放行后 5s 内没有送回结果")
	}
}

// announceNow 推进到宣告出现，limit 内没出现即失败。
func announceNow(h *harness, what string) {
	h.t.Helper()
	if waitAnnounce(h, 5*time.Second) < 0 {
		h.t.Fatalf("前置：5s 内应宣告%s", what)
	}
}

// 换歌连按：A 的缩放在飞时连切 B、C。在飞的只有 A 一次，B、C 宣告时都不带图；A 做完落地时对不上 C、丢掉，
// 随即起 C 的，B 的那张从不缩放；最后落地的是 C 自己的图，A、B 的图从没发出去。
//
// 变异自证：startCover 去掉 `s.coverFly` 的判断（在飞时照起），A 在飞期间又起了多次而红；pollCover 落地
// 不比哈希（去掉 `r.hash == s.song.coverHash`），A 的图补到 C 上而红；pollCover 收到结果后不清 coverFly，
// C 一直不起而红。
func TestSessionCoverSkipBurst(t *testing.T) {
	h := newHarness(t)
	g := useGatedCover(t, h)
	h.f.songs = []mem.NowPlaying{storeOf("A", "专辑A", "图A")}
	h.setTwin(200, 10, true)
	announceNow(h, " A")
	skipTo(h, storeOf("B", "专辑B", "图B"), 210)
	announceNow(h, " B")
	skipTo(h, storeOf("C", "专辑C", "图C"), 220)
	announceNow(h, " C")
	h.run(time.Second)
	if got := g.inputs(h); !slices.Equal(got, []string{"图A"}) {
		t.Fatalf("A 的缩放在飞时不应再起，got 各次输入 %v", got)
	}

	g.finish(h) // A 做完：落地时当前曲目是 C，丢掉，随即起 C 的
	h.run(step)
	if got := g.inputs(h); !slices.Equal(got, []string{"图A", "图C"}) {
		t.Fatalf("A 的结果落地后应只起最新的 C，got 各次输入 %v", got)
	}
	g.finish(h)
	h.run(time.Second)

	if got := infoSeq(h.evs); !slices.Equal(got, []string{"A", "B", "C", "C+图"}) {
		t.Fatalf("A、B、C 都应不带图宣告，只给 C 补图，got %v", got)
	}
	if si := songInfos(h.evs); si[3].CoverBase64 != "uri:图C" {
		t.Fatalf("补到 C 上的应是 C 自己的图，got %q", si[3].CoverBase64)
	}
	if n := g.peakRunning(); n != 1 {
		t.Fatalf("同一时刻至多一次缩放在跑，got %d", n)
	}
}

// 同一首歌在缩放途中换了图（宣告时存储里还是旧图、随后写进新图）：在飞的旧图做完时丢掉，接着缩放新图，
// 只补发新图一条。
//
// 变异自证：pollCover 落地不比哈希，旧图先补发一条、新图再补一条而红。
func TestSessionCoverChangedWhileInFlight(t *testing.T) {
	h := newHarness(t)
	g := useGatedCover(t, h)
	h.f.songs = []mem.NowPlaying{storeOf("换图", "专辑", "旧图")}
	h.setTwin(200, 10, true)
	announceNow(h, "")
	h.f.songs = []mem.NowPlaying{storeOf("换图", "专辑", "新图")} // 存储重建、盯梢发现
	h.run(time.Second)
	if got := g.inputs(h); !slices.Equal(got, []string{"旧图"}) {
		t.Fatalf("旧图在飞时不应再起，got 各次输入 %v", got)
	}

	g.finish(h)
	h.run(step)
	if got := g.inputs(h); !slices.Equal(got, []string{"旧图", "新图"}) {
		t.Fatalf("旧图做完后应接着缩放新图，got 各次输入 %v", got)
	}
	g.finish(h)
	h.run(time.Second)
	si := songInfos(h.evs)
	if got := infoSeq(h.evs); !slices.Equal(got, []string{"换图", "换图+图"}) || si[1].CoverBase64 != "uri:新图" {
		t.Fatalf("应只补发新图一条，got %v（末条 %q）", got, si[len(si)-1].CoverBase64)
	}
}

// 上一次缩放的图被新歌当场用上之后，在飞的另一张才做完：当前曲目的图得照它自己的字节重新缩放，不能拿在飞
// 那张的字节去顶。P 的图已缩放过；A 换了图、缩放在飞；B 又用回 P 的图，宣告时当场带上；A 的结果落地后丢掉，
// 重新缩放的输入是 P 的图，B 的封面始终是 P 的，不多发。
//
// 变异自证：setCover 改成只在没缩放过时才记下原始字节（当场用上的那条路不记），重新缩放的输入成了 A 的图而红；
// putCover 去掉「同一 URI 不补发」，重新缩放落地时给 B 多发一条而红。
func TestSessionCoverReuseThenStaleResult(t *testing.T) {
	h := newHarness(t)
	g := useGatedCover(t, h)
	h.f.songs = []mem.NowPlaying{storeOf("P", "专辑P", "图P")}
	h.setTwin(200, 10, true)
	announceNow(h, " P")
	g.finish(h)
	h.run(step)
	if got := infoSeq(h.evs); !slices.Equal(got, []string{"P", "P+图"}) {
		t.Fatalf("前置：P 应补上自己的图，got %v", got)
	}

	skipTo(h, storeOf("A", "专辑A", "图A"), 210)
	announceNow(h, " A")
	from := h.mark()
	skipTo(h, storeOf("B", "专辑P", "图P"), 220)
	announceNow(h, " B")
	g.finish(h) // A 的做完：对不上 B，丢掉；B 要的 P 图已被挤出「上一次」，重新缩放
	h.run(step)
	g.finish(h)
	h.run(time.Second)

	if got := g.inputs(h); !slices.Equal(got, []string{"图P", "图A", "图P"}) {
		t.Fatalf("重新缩放的应是 B 自己的图 P，got 各次输入 %v", got)
	}
	si := songInfos(h.since(from))
	if len(si) != 1 || si[0].Name != "B" || si[0].CoverBase64 != "uri:图P" {
		t.Fatalf("B 应随宣告带着 P 的图、只发一条，got %v", infoSeq(h.since(from)))
	}
}

// 结果通道里有一条不属于在飞那次的残留（按构造不会有，防将来改坏）：收到时丢掉、不算在飞的那次做完，不因它
// 再起一次、也不把它落地；在飞的那次照常送进来、落地。
//
// 变异自证：pollCover 不比序号（去掉 `r.seq != s.coverSeq` 的判断），残留被当成在飞那次的结果、在飞被清掉而红。
func TestSessionCoverResidueIgnored(t *testing.T) {
	h := newHarness(t)
	g := useGatedCover(t, h)
	h.f.songs = []mem.NowPlaying{storeOf("残留", "专辑", "图")}
	h.setTwin(200, 10, true)
	announceNow(h, "")
	h.s.coverCh <- coverResult{seq: h.s.coverSeq - 1, hash: h.s.song.coverHash, uri: "残留的结果"}
	h.run(time.Second)
	if got := g.inputs(h); !slices.Equal(got, []string{"图"}) || !h.s.coverFly || len(h.s.coverCh) != 0 {
		t.Fatalf("残留应被收走丢掉、在飞的照旧，got 各次输入 %v、在飞=%v、通道里 %d 条", got, h.s.coverFly, len(h.s.coverCh))
	}
	g.finish(h)
	h.run(time.Second)
	si := songInfos(h.evs)
	if got := infoSeq(h.evs); !slices.Equal(got, []string{"残留", "残留+图"}) || si[1].CoverBase64 != "uri:图" {
		t.Fatalf("应只落地在飞那次的结果，got %v（末条 %q）", got, si[len(si)-1].CoverBase64)
	}
	if n := g.peakRunning(); n != 1 {
		t.Fatalf("同一时刻至多一次缩放在跑，got %d", n)
	}
}

// 会话不再收（进程退出、Stop：不再 tick）时缩放还在飞：做完照样送得进通道（容量 1、此时是空的），goroutine
// 退出、不挂住。不关 coverEnd：关了之后 select 两路都就绪、随机挑，结果可能不送，本例就看不出通道有没有缓冲；
// 「有残留时等 end」由 TestRunCoverJobNeverHangs 守。
//
// 变异自证：coverCh 改成不带缓冲（`make(chan coverResult)`），送不进去、5s 内通道仍空而红。
func TestSessionCoverJobExitsAfterSessionEnds(t *testing.T) {
	h := newHarness(t)
	g := useGatedCover(t, h)
	h.f.songs = []mem.NowPlaying{storeOf("会话结束", "专辑", "图")}
	h.setTwin(200, 10, true)
	announceNow(h, "")
	if !h.s.coverFly {
		t.Fatal("前置：缩放应在飞")
	}
	g.release <- struct{}{} // 此后不再 tick
	deadline := time.Now().Add(5 * time.Second)
	for len(h.s.coverCh) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(h.s.coverCh) != 1 {
		t.Fatal("会话不再收时，做完的结果应照样送进通道（goroutine 不挂住）")
	}
}

// runCoverJob 送结果：通道空着时当场送进去；通道里有残留时等着被收走，等到会话结束（end 关闭）就退出，不挂住。
//
// 变异自证：runCoverJob 的 select 去掉 `case <-end`（只剩送结果），「有残留」一例 end 关闭后仍不退出而红；
// 改成 `select { case out <- r: default: }`（送不进就丢），「有残留」一例没等 end 关闭就退出而红。
func TestRunCoverJobNeverHangs(t *testing.T) {
	norm := func(_ string, b []byte, _ string) string { return "uri:" + string(b) }
	run := func(out chan coverResult, end chan struct{}) chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			runCoverJob(norm, coverResult{seq: 7, hash: 9}, []byte("图"), "image/jpeg", out, end)
		}()
		return done
	}

	t.Run("通道空着", func(t *testing.T) {
		out, end := make(chan coverResult, 1), make(chan struct{})
		select {
		case <-run(out, end):
		case <-time.After(5 * time.Second):
			t.Fatal("通道空着时应当场送进去并退出")
		}
		if r := <-out; r != (coverResult{seq: 7, hash: 9, uri: "uri:图"}) {
			t.Fatalf("送回的结果不对，got %+v", r)
		}
	})
	t.Run("有残留", func(t *testing.T) {
		out, end := make(chan coverResult, 1), make(chan struct{})
		out <- coverResult{seq: 1}
		done := run(out, end)
		select {
		case <-done:
			t.Fatal("会话还在时应等残留被收走，不应丢掉结果就退出")
		case <-time.After(100 * time.Millisecond):
		}
		close(end)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("会话结束后应退出，不挂住")
		}
		if r := <-out; r.seq != 1 {
			t.Fatalf("通道里应仍是那条残留，got %+v", r)
		}
	})
}

// 缩放慢（桩里睡 1s，远慢于真机最坏的约 170ms）也不挡轮询：缩放在飞期间照常推歌词行，几秒的轮询转眼跑完；
// 做完后下一轮补上封面。
//
// 变异自证：startCover 里把 `go runCoverJob(...)` 改成直接调用（在轮询 goroutine 里同步做），轮询被挡住 1s
// 以上而红。
func TestSessionCoverInFlightKeepsTicking(t *testing.T) {
	h := newHarness(t)
	h.manualCover = true
	orig := coverDataURI
	t.Cleanup(func() { coverDataURI = orig })
	coverDataURI = func(_ string, b []byte, _ string) string {
		time.Sleep(time.Second)
		return "uri:" + string(b)
	}
	h.f.songs = []mem.NowPlaying{storeOf("慢封面", "专辑", "图")}
	h.f.docs = []mem.TTMLDoc{docAt(ttmlAt(200.4, 1, 2, 3), 0xA000)}
	h.setTwin(200, 0, true)
	h.s.twin = realTwin // 直接从已确认的位置结构起步
	start := time.Now()
	h.run(4 * time.Second)
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("缩放在飞时轮询不应被挡住，4s 的轮询花了 %v", d)
	}
	if !h.s.coverFly {
		t.Fatal("前置：缩放应仍在飞")
	}
	if got := lineIdxs(h.evs); !slices.Equal(got, []int{0, 1, 2}) {
		t.Fatalf("缩放在飞期间应照常推歌词行，got %v", got)
	}

	h.manualCover = false
	h.run(2 * step) // 第一轮等它做完，第二轮落地
	if got := infoSeq(h.evs); !slices.Equal(got, []string{"慢封面", "慢封面+图"}) {
		t.Fatalf("做完后应补上封面，got %v", got)
	}
}
