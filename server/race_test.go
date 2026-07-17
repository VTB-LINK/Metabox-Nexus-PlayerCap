package server

// 本文件只测一个失败模式：handleAllLyrics 必须在锁内取完整快照。锁外读会让标题与歌词来自
// 不同的歌，更早的表现是 slice header 撕裂导致 panic。

import (
	"encoding/json"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// TestAllLyricsSnapshotIsAtomic 钉死 handleAllLyrics 必须在锁内取完整快照。
//
// 手法（不依赖 -race，本机无 C 编译器跑不了 -race）：writer 只在两个**合法组合**
// 之间交替 —— 歌A=(Title="AAAA", 1 行)、歌B=(Title="BBBB", 40 行)。任何时刻的
// 合法快照只有这两种。读者若拿到 (AAAA,40) 或 (BBBB,1)，即证明标题与歌词来自
// 不同的歌 —— 读到了非原子快照。
//
// 修复前实测（回退旧写法跑 6 次）：**6/6 全部死于 slice header 撕裂导致的 panic**
// （BuildDetailedText 解引用垃圾指针），下面这个「非原子快照」检测器一次都没触发过
// —— 撕裂先发生，检测器根本没机会跑到。所以本测试实际守住的是「撕裂不再发生」，
// 「非原子快照」是从 panic 栈帧（Title len=4 + Count=0x28）推得的、未直接复现。
// 两者根因相同（锁外读），一并被修复覆盖。
//
// 注：panic 被 net/http 的 conn.serve recover，**进程不死**；但它不写任何响应，
// 客户端拿到的是裸 EOF/connection reset，不是 500。
//
// 这条路径是公开 HTTP API（README「API 一览」），自家 overlay 走 WS 不碰它，
// 但本服务是中间件，下游用不用、多高频，我们无法控制 —— 故按会被高频调用处理。
func TestAllLyricsSnapshotIsAtomic(t *testing.T) {
	s := NewServer([]string{"wesing"})
	s.SetActivePlayer("wesing")

	songA := &player.AllLyricsData{Title: "AAAA", Lyrics: make([]player.LyricLine, 1)}
	songB := &player.AllLyricsData{Title: "BBBB", Lyrics: make([]player.LyricLine, 40)}

	stop := make(chan struct{})
	var writerWg, readerWg sync.WaitGroup

	writerWg.Add(1)
	go func() {
		defer writerWg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.UpdatePlayerState(player.Event{PlayerName: "wesing", Type: player.EventAllLyrics, Data: songA})
			s.UpdatePlayerState(player.Event{PlayerName: "wesing", Type: player.EventAllLyrics, Data: songB})
		}
	}()

	var mismatched, reads int64
	h := s.handleAllLyrics("")
	for range 4 {
		readerWg.Add(1)
		go func() {
			defer readerWg.Done()
			for range 20000 {
				rec := httptest.NewRecorder()
				h(rec, httptest.NewRequest("GET", "/all_lyrics", nil))

				var resp struct {
					Data struct {
						Title string `json:"title"`
						Count int    `json:"count"`
					} `json:"data"`
				}
				if json.Unmarshal(rec.Body.Bytes(), &resp) != nil || resp.Data.Title == "" {
					continue
				}
				atomic.AddInt64(&reads, 1)

				okA := resp.Data.Title == "AAAA" && resp.Data.Count == 1
				okB := resp.Data.Title == "BBBB" && resp.Data.Count == 40
				if !okA && !okB {
					if atomic.AddInt64(&mismatched, 1) <= 3 {
						t.Errorf("非原子快照: Title=%q Count=%d（合法组合只有 AAAA/1 与 BBBB/40）",
							resp.Data.Title, resp.Data.Count)
					}
				}
			}
		}()
	}

	// 先停 writer 再等它 —— 顺序反了会死锁（wg.Wait 等的 writer 只在 stop 后退出）
	readerWg.Wait()
	close(stop)
	writerWg.Wait()

	if mismatched > 0 {
		t.Errorf("共 %d/%d 次读到非原子快照", mismatched, reads)
	}
	t.Logf("有效读取 %d 次，全部为一致快照", reads)
}
