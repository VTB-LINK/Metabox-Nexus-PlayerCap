package kugou

// 本文件只测 runCoverFetch（封面获取 goroutine）的出口不变量：拿到封面 URL 就必须发出
// song_info_update（b64 成功与否都发），b64 为空时补取一次、取到再补发（#194），换歌后不得补发。
// 不测提权/patch，那些在别处。

import (
	"context"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

// deadCoverURL 指向一个必然连不上的地址：既是回环（SSRF 拨号校验直接拒绝），端口 1 上也无服务。
// 用它让 FetchCoverBase64 快速返回空串，模拟「封面 URL 有效但 b64 拿不到」。
const deadCoverURL = "http://127.0.0.1:1/cover.jpg"

func recvSongInfo(t *testing.T, p *KuGouPlayer, within time.Duration) *player.SongInfo {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case evt := <-p.EventCh:
			if evt.Type != player.EventSongInfoUpdate {
				continue
			}
			si, ok := evt.Data.(*player.SongInfo)
			if !ok {
				t.Fatalf("song_info_update 的 Data 类型不对: %T", evt.Data)
			}
			return si
		case <-deadline:
			return nil
		}
	}
}

// TestRunCoverFetchEmitsWhenBase64Fails 钉死本模块最重要的不变量：
// **拿到封面 URL 后，无论 b64 成功与否都必须发出 song_info_update。**
//
// 为什么这条最要命：换歌时封面 URL 会被预置进缓冲 channel，所以常见路径上第一个 select
// 立即命中 URL，跳过「先发不含封面的信息」那整个分支——此前一次 song_info_update 都没发
// 过。此时若为 b64 失败而 return，OBS 上整首歌都停在上一首的标题和封面，而轮询循环只在
// 换歌时发 song_info_update，不会自愈。
//
// b64 预算（coverTotalBudget=800ms）是实测调优的 perf 值，缩短它的前提正是本不变量成立。
//
// 变异自证：把末尾的 Emit 换回 `if b64 == "" { return }` 即红。
func TestRunCoverFetchEmitsWhenBase64Fails(t *testing.T) {
	p := New(0, 500)
	ch := make(chan string, 1)
	ch <- deadCoverURL // 模拟换歌时预置封面 URL —— 第一个 select 立即命中

	p.runCoverFetch(context.Background(), ch, "歌名", "歌手", "歌名 - 歌手", "hash123")

	si := recvSongInfo(t, p, 2*time.Second)
	if si == nil {
		t.Fatal("b64 下载失败时也必须发出 song_info_update —— 否则 OBS 整首歌停在上一首")
	}
	if si.Title != "歌名 - 歌手" {
		t.Errorf("标题应为「歌名 - 歌手」，实得 %q", si.Title)
	}
	if si.Cover != deadCoverURL {
		t.Errorf("b64 拿不到时仍应带上封面 URL 供前端自行加载，实得 %q", si.Cover)
	}
	if si.CoverBase64 != "" {
		t.Errorf("连不上的地址不该产出 base64，实得 %d 字节", len(si.CoverBase64))
	}
}

// TestRunCoverFetchEmitsWithoutCoverURL 覆盖另一条路径：始终拿不到封面 URL 时，
// 不含封面的歌曲信息必须先发出去，不能被封面拖住。
//
// 这里不给 channel 喂任何值，让它走 coverEarlyWait 超时分支。API 兜底会真的发 HTTP
// 请求，但那发生在第一次 Emit 之后，不影响本断言。
func TestRunCoverFetchEmitsWithoutCoverURL(t *testing.T) {
	p := New(0, 500)
	ch := make(chan string) // 永不送达

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.runCoverFetch(ctx, ch, "歌名", "歌手", "歌名 - 歌手", "hash123")

	si := recvSongInfo(t, p, 3*time.Second)
	if si == nil {
		t.Fatal("拿不到封面 URL 时，也必须先发出不含封面的歌曲信息")
	}
	if si.Title != "歌名 - 歌手" {
		t.Errorf("标题应为「歌名 - 歌手」，实得 %q", si.Title)
	}
	if si.CoverBase64 != "" {
		t.Errorf("首发不该带 base64，实得 %d 字节", len(si.CoverBase64))
	}
}

// TestRunCoverFetchRespectsCancel 钉死「下载途中换歌 → 不得补发」：上一首的封面
// goroutine 必须停手，否则它的标题+封面会盖在当前歌上。
//
// **取消的时机必须落在下载途中**，不能在调用前就取消：那样顶部 select 里 `<-ch`（缓冲
// 有值）与 `<-ctx.Done()`（已关闭）同时就绪，Go 会随机挑一个，挑中 ctx.Done 就在顶部
// return 了——测试照样绿，却根本没走到 Emit 前的那道检查。实测确认过：那种写法下把末尾
// 的 ctx 检查整个删掉，测试依然 PASS，是个装饰品。
//
// 这里用一个可阻塞的下载桩把时序钉死：等下载真正发起（此时 URL 已从 channel 取走、顶部
// select 已过）再 cancel，然后放行下载，逼它走到末尾的检查。桩替换 fetchCoverBase64 seam
// 而非起真实服务——封面下载现带 SSRF 拨号校验会拒回环地址，httptest 的 127.0.0.1 mock 已不可用。
func TestRunCoverFetchRespectsCancel(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	orig := fetchCoverBase64
	defer func() { fetchCoverBase64 = orig }()
	fetchCoverBase64 = func(_, _ string, _ time.Duration) string {
		close(entered)
		<-release
		return "data:image/jpeg;base64,/9j/2Q==" // 非空 b64，模拟正常拿到
	}

	p := New(0, 500)
	ch := make(chan string, 1)
	ch <- "http://cover.example/cover.jpg"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.runCoverFetch(ctx, ch, "上一首", "歌手", "上一首 - 歌手", "hash123")
	}()

	select {
	case <-entered: // 下载已发起：顶部 select 必然走的是 <-ch 分支
	case <-time.After(2 * time.Second):
		t.Fatal("封面下载未在 2s 内发起")
	}
	cancel()       // 下载途中换歌
	close(release) // 放行下载，让桩正常返回 b64
	<-done

	if si := recvSongInfo(t, p, 300*time.Millisecond); si != nil {
		t.Errorf("下载途中换歌后不得再发 song_info_update，实得 %q —— 上一首会盖住当前歌", si.Title)
	}
}

// fetchCall 记一次对 fetchCoverBase64 桩的调用。
type fetchCall struct {
	url     string
	timeout time.Duration
}

// stubFetch 把 fetchCoverBase64 换成按次序返回 results 的桩（用完之后返回空串），返回调用记录。
// runCoverFetch 同步跑完之后再读记录，不另加锁。
func stubFetch(t *testing.T, results ...string) *[]fetchCall {
	t.Helper()
	orig := fetchCoverBase64
	t.Cleanup(func() { fetchCoverBase64 = orig })
	var calls []fetchCall
	fetchCoverBase64 = func(_, u string, timeout time.Duration) string {
		calls = append(calls, fetchCall{u, timeout})
		if len(calls) <= len(results) {
			return results[len(calls)-1]
		}
		return ""
	}
	return &calls
}

// drainSongInfos 收下 within 内到达的全部 song_info_update。
func drainSongInfos(t *testing.T, p *KuGouPlayer, within time.Duration) []*player.SongInfo {
	t.Helper()
	var out []*player.SongInfo
	for si := recvSongInfo(t, p, within); si != nil; si = recvSongInfo(t, p, within) {
		out = append(out, si)
	}
	return out
}

const (
	retryCoverURL = "http://imge.kugou.com/stdmusic/800/20250101/20250101071447378136.jpg"
	retryCoverB64 = "data:image/jpeg;base64,/9j/2Q=="
)

// TestRunCoverFetchRetriesAfterBase64Fails（#194）：b64 头一次没取到时，先照发带 URL 的一条，再以 coverRetryTimeout
// 下载同一个 URL，取到就补发带 b64 的一条，两条的 cover 相同。修正前这里直接结束，整首歌没有图。
//
// 变异自证（2026-11-14，隔离克隆，实测；本例与其后四例合守）：
//   - 去掉补取：本例、RetryFailsEmitsOnce、LateURLRetries、RetryRespectsCancel 红；
//   - 补取挪到首发之前（两次下载之后才发一条）：本例、LateURLRetries、RetryRespectsCancel 红；
//   - 补取之后不查取消：RetryRespectsCancel 红；
//   - 补取失败也补发一条：RetryFailsEmitsOnce 红；
//   - 补取预算改用 coverTotalBudget：本例红；
//   - 补取两次：RetryFailsEmitsOnce 红。
//
// 补取之前的取消检查去掉后各例仍绿（实测，跑 3 遍）：它只省一次下载，补发前另有一道检查，对外不可见，靠人守。
func TestRunCoverFetchRetriesAfterBase64Fails(t *testing.T) {
	calls := stubFetch(t, "", retryCoverB64)
	p := New(0, 500)
	ch := make(chan string, 1)
	ch <- retryCoverURL
	p.runCoverFetch(context.Background(), ch, "歌名", "歌手", "歌名 - 歌手", "hash123")

	got := drainSongInfos(t, p, 200*time.Millisecond)
	if len(got) != 2 {
		t.Fatalf("应发两条 song_info_update（先带 URL、再带图），实得 %d 条", len(got))
	}
	if got[0].Cover != retryCoverURL || got[0].CoverBase64 != "" {
		t.Errorf("第一条应只带 URL：cover=%q b64 长 %d", got[0].Cover, len(got[0].CoverBase64))
	}
	if got[1].Cover != retryCoverURL || got[1].CoverBase64 != retryCoverB64 || got[1].Title != "歌名 - 歌手" {
		t.Errorf("补发的一条应带同一个 URL 与 b64：%+v", *got[1])
	}
	if len(*calls) != 2 || (*calls)[1].url != retryCoverURL || (*calls)[1].timeout != coverRetryTimeout {
		t.Errorf("应以 coverRetryTimeout 补取同一个 URL 一次，实际调用 %+v", *calls)
	}
}

// 反例：头一次就取到 b64 时只发一条、不补取。
func TestRunCoverFetchNoRetryWhenBase64Succeeds(t *testing.T) {
	calls := stubFetch(t, retryCoverB64)
	p := New(0, 500)
	ch := make(chan string, 1)
	ch <- retryCoverURL
	p.runCoverFetch(context.Background(), ch, "歌名", "歌手", "歌名 - 歌手", "hash123")

	got := drainSongInfos(t, p, 200*time.Millisecond)
	if len(got) != 1 || got[0].CoverBase64 != retryCoverB64 {
		t.Fatalf("取到 b64 时应只发一条带图的，实得 %d 条", len(got))
	}
	if len(*calls) != 1 {
		t.Errorf("取到 b64 后不应再下载，实际下载 %d 次", len(*calls))
	}
}

// 反例：补取也失败时只有带 URL 的那一条，不发 b64 为空的第二条，也不再取第三次。
func TestRunCoverFetchRetryFailsEmitsOnce(t *testing.T) {
	calls := stubFetch(t, "", "")
	p := New(0, 500)
	ch := make(chan string, 1)
	ch <- retryCoverURL
	p.runCoverFetch(context.Background(), ch, "歌名", "歌手", "歌名 - 歌手", "hash123")

	got := drainSongInfos(t, p, 200*time.Millisecond)
	if len(got) != 1 || got[0].Cover != retryCoverURL || got[0].CoverBase64 != "" {
		t.Fatalf("补取失败时应只有带 URL 的一条，实得 %d 条", len(got))
	}
	if len(*calls) != 2 {
		t.Errorf("应只补取一次（共下载 2 次），实际 %d 次", len(*calls))
	}
}

// 封面地址晚到（先发了不带封面的一条）之后下载失败，同样补取：依次是不带封面、只带 URL、带图三条。
// URL 在第一条发出之后才送进 channel，走的是 coverEarlyWait 超时之后的分支。
func TestRunCoverFetchLateURLRetries(t *testing.T) {
	calls := stubFetch(t, "", retryCoverB64)
	p := New(0, 500)
	ch := make(chan string) // 不预置，模拟地址晚到
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.runCoverFetch(context.Background(), ch, "歌名", "歌手", "歌名 - 歌手", "hash123")
	}()

	first := recvSongInfo(t, p, 2*time.Second)
	if first == nil || first.Cover != "" {
		t.Fatalf("地址未到时应先发不带封面的一条，实得 %+v", first)
	}
	ch <- retryCoverURL
	<-done

	rest := drainSongInfos(t, p, 200*time.Millisecond)
	if len(rest) != 2 || rest[0].CoverBase64 != "" || rest[1].CoverBase64 != retryCoverB64 {
		t.Fatalf("地址晚到、下载失败后应再发只带 URL 的一条与补取到的带图一条，实得 %d 条", len(rest))
	}
	if len(*calls) != 2 {
		t.Errorf("应下载 2 次，实际 %d 次", len(*calls))
	}
}

// 补取途中换歌：带 URL 的一条要先于补取发出（补取不推迟歌曲信息），补取回来之后不得再发——那时已是下一首。
// 桩在第二次调用时阻塞，取消落在补取途中，时序不靠睡眠。
func TestRunCoverFetchRetryRespectsCancel(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	orig := fetchCoverBase64
	defer func() { fetchCoverBase64 = orig }()
	n := 0
	fetchCoverBase64 = func(_, _ string, _ time.Duration) string {
		n++
		if n == 1 {
			return ""
		}
		close(entered)
		<-release
		return retryCoverB64
	}

	p := New(0, 500)
	ch := make(chan string, 1)
	ch <- retryCoverURL
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.runCoverFetch(ctx, ch, "上一首", "歌手", "上一首 - 歌手", "hash123")
	}()

	if si := recvSongInfo(t, p, 2*time.Second); si == nil || si.Cover != retryCoverURL {
		t.Fatalf("补取开始之前应已发出带 URL 的一条，实得 %+v", si)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("补取未在 2s 内发起")
	}
	cancel()
	close(release)
	<-done

	if si := recvSongInfo(t, p, 300*time.Millisecond); si != nil {
		t.Errorf("补取途中换歌后不得再发 song_info_update，实得 %+v", *si)
	}
}

// TestCoverBudgetsSane 守住预算之间的关系：早期等待必须短于总预算，否则「先发不含封面的
// 信息」那条路永远走不到；总预算必须为正，否则 b64 永远没有下载窗口。
func TestCoverBudgetsSane(t *testing.T) {
	if coverEarlyWait <= 0 || coverTotalBudget <= 0 || coverAPIFallbackTimeout <= 0 || coverRetryTimeout <= 0 {
		t.Fatal("四个预算都必须为正")
	}
	if coverEarlyWait >= coverTotalBudget {
		t.Errorf("coverEarlyWait(%v) 必须短于 coverTotalBudget(%v)，否则超时分支拿不到剩余预算",
			coverEarlyWait, coverTotalBudget)
	}
}
