package player

// 本文件只测 BaseEmitter 的代次守卫（NewSongGen / InvalidateSongGen / EmitForGen）：异步
// 回写（封面等）回来时若已换歌或会话已结束，必须被丢弃，否则上一首的标题+封面会盖在
// 当前歌上、或复活一首已经停掉的歌。

import (
	"sync"
	"testing"
)

// drain 取出通道里当前所有事件（非阻塞）。
func drain(b *BaseEmitter) []Event {
	var out []Event
	for {
		select {
		case e := <-b.EventCh:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestEmitForGenDropsStaleWriteback 钉死代次守卫：过期代次的异步回写必须被丢弃。
//
// 真实场景：封面靠 goroutine 异步下载（cloudmusic/qqmusic 5s，wesing 最长约 10s），
// 取完才补发 song_info_update。若下载期间换了歌，迟到的回写会把上一首的标题+封面盖在
// 当前歌曲上——而 song_info_update 只在换歌分支发一次，轮询循环不会重发，**整首歌都
// 不自愈**。go vet 看不见这类缺陷：裸 go func 没有 CancelFunc 可丢。
//
// 变异自证：去掉 EmitForGen 里的代次比较即红。
func TestEmitForGenDropsStaleWriteback(t *testing.T) {
	b := NewBaseEmitter("test")

	genA := b.NewSongGen() // 歌 A 开始，spawn 封面 goroutine 捕获 genA
	genB := b.NewSongGen() // 用户切到歌 B

	if genA == genB {
		t.Fatalf("换歌后代次必须递增：genA=%d genB=%d", genA, genB)
	}

	// 歌 A 的封面 goroutine 现在才下载完 —— 必须被丢弃
	if b.EmitForGen(genA, EventSongInfoUpdate, &SongInfo{Title: "A"}) {
		t.Error("过期代次（上一首歌）的回写应被丢弃，实际发出了")
	}
	// 歌 B 的回写 —— 必须发出
	if !b.EmitForGen(genB, EventSongInfoUpdate, &SongInfo{Title: "B"}) {
		t.Error("当前代次的回写应发出，实际被丢弃了")
	}

	evts := drain(&b)
	if len(evts) != 1 {
		t.Fatalf("应恰好收到 1 个事件（只有 B），实得 %d 个", len(evts))
	}
	if got := evts[0].Data.(*SongInfo).Title; got != "B" {
		t.Errorf("收到的应是当前歌 B，实得 %q —— 上一首把当前歌盖掉了", got)
	}
}

// TestEmitForGenAllowsCurrent 反方向：没换歌时回写必须照常发出。
//
// 没有这条，「EmitForGen 永远返回 false」这种把封面功能整个废掉的假修复能通过上面那条。
func TestEmitForGenAllowsCurrent(t *testing.T) {
	b := NewBaseEmitter("test")
	gen := b.NewSongGen()

	if !b.EmitForGen(gen, EventSongInfoUpdate, &SongInfo{Title: "A"}) {
		t.Fatal("未换歌时回写应发出")
	}
	evts := drain(&b)
	if len(evts) != 1 || evts[0].Data.(*SongInfo).Title != "A" {
		t.Errorf("应收到 A 的封面回写，实得 %+v", evts)
	}
}

// TestInvalidateSongGenDropsInFlight 钉死会话结束时的作废：进程退出后，最后一首歌仍在飞的
// 封面回写必须被丢弃，不能复活已清空的 SongInfo。
//
// 缺陷背景：代次原本只在换歌（NewSongGen）时前进，会话结束不前进——于是迟到的封面回写
// 拿到的 gen 仍等于当前 gen，EmitForGen 放行，把已 ClearSongData 的歌曲信息重新贴回缓存，
// 之后每个新连接经 buildInitEvents 都拿到这份复活数据，OBS 一直显示已停掉的歌。
//
// 变异自证：把 InvalidateSongGen 改成空实现（不递增）即红。
func TestInvalidateSongGenDropsInFlight(t *testing.T) {
	b := NewBaseEmitter("test")

	gen := b.NewSongGen() // 歌 A 开始，封面 goroutine 捕获 gen
	b.InvalidateSongGen() // 会话结束（进程退出），作废

	// 歌 A 的封面 goroutine 现在才下载完 —— 必须被丢弃
	if b.EmitForGen(gen, EventSongInfoUpdate, &SongInfo{Title: "A"}) {
		t.Error("会话结束后，在飞的封面回写应被作废——否则复活一首已经停掉的歌")
	}
	if n := len(drain(&b)); n != 0 {
		t.Errorf("作废后不应有事件发出，实得 %d 个", n)
	}
}

// TestNewSongGenMonotonic 并发换歌下代次严格唯一——两个 goroutine 不得拿到同一代次，
// 否则上一首的回写会被误认为当前。
func TestNewSongGenMonotonic(t *testing.T) {
	b := NewBaseEmitter("test")
	const n = 200
	seen := make([]uint64, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seen[i] = b.NewSongGen()
		}(i)
	}
	wg.Wait()

	uniq := make(map[uint64]bool, n)
	for _, g := range seen {
		if uniq[g] {
			t.Fatalf("代次 %d 被分配了两次——并发下不唯一", g)
		}
		uniq[g] = true
	}
	if len(uniq) != n {
		t.Errorf("应有 %d 个不同代次，实得 %d", n, len(uniq))
	}
}
