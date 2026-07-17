package kugou

// 本文件只测 runCoverFetch（封面获取 goroutine）的出口不变量：拿到封面 URL 就必须发出
// song_info_update（b64 成功与否都发），换歌后不得补发。不测提权/patch，那些在别处。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

// deadCoverURL 指向一个必然连不上的地址：TCP 端口 1 上没有服务，连接立即被拒。
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
// 这里用一个可阻塞的 httptest 服务把时序钉死：等下载真正发起（此时 URL 已从 channel
// 取走、顶部 select 已过）再 cancel，然后放行下载，逼它走到末尾的检查。
func TestRunCoverFetchRespectsCancel(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte{0xFF, 0xD8, 0xFF, 0xD9}) //nolint // 极小的假 jpeg，够 base64 出非空串
	}))
	defer srv.Close()

	p := New(0, 500)
	ch := make(chan string, 1)
	ch <- srv.URL + "/cover.jpg"

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
	close(release) // 放行下载，让 FetchCoverBase64 正常拿到 b64
	<-done

	if si := recvSongInfo(t, p, 300*time.Millisecond); si != nil {
		t.Errorf("下载途中换歌后不得再发 song_info_update，实得 %q —— 上一首会盖住当前歌", si.Title)
	}
}

// TestCoverBudgetsSane 守住预算之间的关系：早期等待必须短于总预算，否则「先发不含封面的
// 信息」那条路永远走不到；总预算必须为正，否则 b64 永远没有下载窗口。
func TestCoverBudgetsSane(t *testing.T) {
	if coverEarlyWait <= 0 || coverTotalBudget <= 0 || coverAPIFallbackTimeout <= 0 {
		t.Fatal("三个预算都必须为正")
	}
	if coverEarlyWait >= coverTotalBudget {
		t.Errorf("coverEarlyWait(%v) 必须短于 coverTotalBudget(%v)，否则超时分支拿不到剩余预算",
			coverEarlyWait, coverTotalBudget)
	}
}
