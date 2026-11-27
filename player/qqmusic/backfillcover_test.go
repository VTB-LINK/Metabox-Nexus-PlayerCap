package qqmusic

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

func drainSongInfoEvents(t *testing.T, p *QQMusicPlayer) []*player.SongInfo {
	t.Helper()
	var out []*player.SongInfo
	for {
		select {
		case evt := <-p.Events():
			if evt.Type == player.EventSongInfoUpdate {
				info, ok := evt.Data.(*player.SongInfo)
				if !ok || info == nil {
					t.Fatalf("song_info_update 载荷不是 *player.SongInfo: %T", evt.Data)
				}
				out = append(out, info)
			}
		default:
			return out
		}
	}
}

// TestBackfillCover 验证补取在发出新元数据之前先作废认领时的封面任务（#229，Q6）：
//   - 认领封面任务在补取期间下载完成时，因代次已过期不得回写覆盖补取发出的歌名/歌手；
//   - 对照子例将 NewSongGen 挪到旧封面任务完成之后，证实测试确实能观测到该竞态；
//   - url 为空时 startCover 不起任务、不调 download、立刻关闭 done 且不发事件。
//
// 变异自证（隔离副本实测，全部可编译）：
//   - startCover 改经 Emit 而非 EmitForGen 回写 → 开、关两例均红；
//   - 对照子例将 NewSongGen 挪回旧封面完成之后 → 确能观测到 singer == "" 的旧封面回写覆盖补取新值。
func TestBackfillCover(t *testing.T) {
	t.Run("开/认领封面迟到不盖回补取补上的歌手", func(t *testing.T) {
		t.Cleanup(func() { player.SetSongInfoKeepFirst(true) })
		player.SetSongInfoKeepFirst(true)

		p := New(0, 50)
		started1 := make(chan struct{})
		release1 := make(chan struct{})
		ops1 := qqCoverOps{download: func(url string) string {
			close(started1)
			<-release1
			return "data:image/jpeg;base64,OLD"
		}}
		ops2 := qqCoverOps{download: func(url string) string {
			return "data:image/jpeg;base64,NEW"
		}}

		// 认领：歌手为空，起封面任务
		g1 := p.NewSongGen()
		done1 := p.startCover(ops1, g1, "https://example.com/u1.jpg", "A", "", "A - ")
		<-started1

		// 补取：先作废认领封面任务，再发补取的歌名/歌手（歌手补上 X，标题保持认领时的）
		g2 := p.NewSongGen()
		bfName, bfSinger := backfillNames("A", "", "A", "", "A (Live)", "X")
		if bfName != "A" || bfSinger != "X" {
			t.Fatalf("前置 backfillNames = (%q, %q), 期望 (\"A\", \"X\")", bfName, bfSinger)
		}
		p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
			Name: bfName, Singer: bfSinger, Title: "A - ",
		})

		// 模拟阻塞在 fetchCoverURL 期间认领封面任务下载完成
		close(release1)
		<-done1

		// 补取起新的封面任务
		done2 := p.startCover(ops2, g2, "https://example.com/u2.jpg", bfName, bfSinger, "A - ")
		<-done2

		got := drainSongInfoEvents(t, p)
		if len(got) != 2 {
			t.Fatalf("收到 %d 条 song_info_update，期望 2 条（补取元数据 + 补取封面）: %+v", len(got), got)
		}
		if got[0].Name != "A" || got[0].Singer != "X" || got[0].Title != "A - " || got[0].CoverBase64 != "" {
			t.Fatalf("第 1 条应为补取发出的不带封面元数据，实得 %+v", got[0])
		}
		for i, info := range got[1:] {
			if info.Singer == "" {
				t.Fatalf("补取 A / X 之后第 %d 条出现认领时的空歌手回写: %+v", i+1, info)
			}
		}
		last := got[len(got)-1]
		if last.Name != "A" || last.Singer != "X" || last.Title != "A - " ||
			last.Cover != "https://example.com/u2.jpg" || last.CoverBase64 != "data:image/jpeg;base64,NEW" {
			t.Fatalf("最后一条应为 A / X 带新封面，实得 %+v", last)
		}
	})

	t.Run("关/认领封面迟到不盖回补取的歌词标签写法", func(t *testing.T) {
		t.Cleanup(func() { player.SetSongInfoKeepFirst(true) })
		player.SetSongInfoKeepFirst(false)

		p := New(0, 50)
		started1 := make(chan struct{})
		release1 := make(chan struct{})
		ops1 := qqCoverOps{download: func(url string) string {
			close(started1)
			<-release1
			return "data:image/jpeg;base64,OLD"
		}}
		ops2 := qqCoverOps{download: func(url string) string {
			return "data:image/jpeg;base64,NEW"
		}}

		// 认领：内存写法，起封面任务
		g1 := p.NewSongGen()
		done1 := p.startCover(ops1, g1, "https://example.com/u1.jpg", "晚风", "陈婧霏", "晚风 - 陈婧霏")
		<-started1

		// 补取：开关关闭，先作废认领封面任务，再发歌词标签写法（标题仍为认领时的）
		g2 := p.NewSongGen()
		bfName, bfSinger := backfillNames("晚风", "陈婧霏", "晚风", "陈婧霏", "晚风 (Live)", "陈婧霏、X")
		if bfName != "晚风 (Live)" || bfSinger != "陈婧霏、X" {
			t.Fatalf("前置 backfillNames = (%q, %q), 期望 (\"晚风 (Live)\", \"陈婧霏、X\")", bfName, bfSinger)
		}
		p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
			Name: bfName, Singer: bfSinger, Title: "晚风 - 陈婧霏",
		})

		close(release1)
		<-done1

		done2 := p.startCover(ops2, g2, "https://example.com/u2.jpg", bfName, bfSinger, "晚风 - 陈婧霏")
		<-done2

		got := drainSongInfoEvents(t, p)
		if len(got) != 2 {
			t.Fatalf("收到 %d 条 song_info_update，期望 2 条: %+v", len(got), got)
		}
		for i, info := range got[1:] {
			if info.Name == "晚风" && info.Singer == "陈婧霏" {
				t.Fatalf("补取歌词标签写法之后第 %d 条出现认领写法的回写: %+v", i+1, info)
			}
		}
		last := got[len(got)-1]
		if last.Name != "晚风 (Live)" || last.Singer != "陈婧霏、X" || last.Title != "晚风 - 陈婧霏" ||
			last.Cover != "https://example.com/u2.jpg" || last.CoverBase64 != "data:image/jpeg;base64,NEW" {
			t.Fatalf("最后一条应为歌词标签写法带新封面，实得 %+v", last)
		}
	})

	t.Run("对照/NewSongGen挪到旧封面完成之后确能观测到旧值盖回竞态", func(t *testing.T) {
		t.Cleanup(func() { player.SetSongInfoKeepFirst(true) })
		player.SetSongInfoKeepFirst(true)

		p := New(0, 50)
		started1 := make(chan struct{})
		release1 := make(chan struct{})
		ops1 := qqCoverOps{download: func(url string) string {
			close(started1)
			<-release1
			return "data:image/jpeg;base64,OLD"
		}}
		ops2 := qqCoverOps{download: func(url string) string {
			return "data:image/jpeg;base64,NEW"
		}}

		g1 := p.NewSongGen()
		done1 := p.startCover(ops1, g1, "https://example.com/u1.jpg", "A", "", "A - ")
		<-started1

		// 复现旧次序：先发补取元数据，阻塞期间旧封面回写，之后才 NewSongGen
		p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
			Name: "A", Singer: "X", Title: "A - ",
		})
		close(release1)
		<-done1

		g2 := p.NewSongGen()
		done2 := p.startCover(ops2, g2, "https://example.com/u2.jpg", "A", "X", "A - ")
		<-done2

		got := drainSongInfoEvents(t, p)
		sawOverwrite := false
		for _, info := range got[1:] {
			if info.Name == "A" && info.Singer == "" && info.CoverBase64 == "data:image/jpeg;base64,OLD" {
				sawOverwrite = true
				break
			}
		}
		if !sawOverwrite {
			t.Fatalf("对照组本应观测到补取之后出现 singer == \"\" 的旧封面回写，实际事件: %+v", got)
		}
	})

	t.Run("空URL不起任务不调用download且立即关闭done", func(t *testing.T) {
		p := New(0, 50)
		called := false
		ops := qqCoverOps{download: func(url string) string {
			called = true
			return "data:image/jpeg;base64,SHOULD_NOT_RUN"
		}}

		g := p.NewSongGen()
		done := p.startCover(ops, g, "", "A", "X", "A - X")
		select {
		case <-done:
		default:
			t.Fatal("url 为空时 startCover 应立即关闭返回的 done 通道")
		}
		if called {
			t.Fatal("url 为空时不应调用 ops.download")
		}
		if got := drainSongInfoEvents(t, p); len(got) != 0 {
			t.Fatalf("url 为空时不应发出任何 song_info_update，实得 %+v", got)
		}
	})

	t.Run("下载返回空串时不发事件", func(t *testing.T) {
		p := New(0, 50)
		ops := qqCoverOps{download: func(url string) string { return "" }}

		g := p.NewSongGen()
		done := p.startCover(ops, g, "https://example.com/empty.jpg", "A", "X", "A - X")
		<-done
		if got := drainSongInfoEvents(t, p); len(got) != 0 {
			t.Fatalf("下载返回空串时不应发出 song_info_update，实得 %+v", got)
		}
	})
}

// TestClaimNewSongInvalidatesPreviousCover 验证认领新歌时先作废上一首封面任务再发宣告事件（#229 终审 P2）：
//   - 认领歌曲 A 启动封面下载后，认领歌曲 B 先调 NewSongGen 再发 B 的宣告事件；A 的下载在 B 宣告后完成、
//     且 B 封面下载失败时，A 的旧封面任务被代次拦截，不得回写覆盖 B；
//   - 对照子例将 B 的 NewSongGen 挪到 B 宣告之后、A 的下载完成之后，证实测试确能观测到 B→A 覆盖。
//
// 变异自证（隔离副本实测，全部可编译）：
//   - startCover 改经 Emit 而非 EmitForGen 回写 → 正例红；
//   - 对照子例将 NewSongGen 挪回 B 宣告之后 → 确能观测到 SongA 的旧封面回写覆盖 SongB。
func TestClaimNewSongInvalidatesPreviousCover(t *testing.T) {
	t.Run("先NewSongGen再宣告B时A的迟到封面不覆盖B", func(t *testing.T) {
		p := New(0, 50)
		startedA := make(chan struct{})
		releaseA := make(chan struct{})
		opsA := qqCoverOps{download: func(url string) string {
			close(startedA)
			<-releaseA
			return "data:image/jpeg;base64,COVER_A"
		}}
		opsB := qqCoverOps{download: func(url string) string {
			return "" // B 封面下载失败且不补取
		}}

		// 认领歌曲 A，启动封面下载
		gA := p.NewSongGen()
		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: "SongA - SingerA"})
		p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
			Name: "SongA", Singer: "SingerA", Title: "SongA - SingerA", Cover: "https://example.com/a.jpg",
		})
		doneA := p.startCover(opsA, gA, "https://example.com/a.jpg", "SongA", "SingerA", "SongA - SingerA")
		<-startedA
		_ = drainSongInfoEvents(t, p) // 仅关注换到 B 之后的事件序列

		// 认领歌曲 B：先作废 A 的封面任务，再发 B 的宣告事件
		gB := p.NewSongGen()
		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: "SongB - SingerB"})
		p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
			Name: "SongB", Singer: "SingerB", Title: "SongB - SingerB", Cover: "https://example.com/b.jpg",
		})

		// A 的封面下载在 B 宣告发出后完成
		close(releaseA)
		<-doneA

		// B 启动封面下载但失败
		doneB := p.startCover(opsB, gB, "https://example.com/b.jpg", "SongB", "SingerB", "SongB - SingerB")
		<-doneB

		got := drainSongInfoEvents(t, p)
		if len(got) != 1 {
			t.Fatalf("换到 B 且 B 封面失败后应仅有 1 条 B 的 song_info_update，实得 %d 条: %+v", len(got), got)
		}
		if got[0].Name != "SongB" || got[0].Singer != "SingerB" || got[0].Title != "SongB - SingerB" {
			t.Fatalf("唯一事件应为 SongB，实得 %+v", got[0])
		}
	})

	t.Run("对照/NewSongGen挪到B宣告之后确能观测到A覆盖B", func(t *testing.T) {
		p := New(0, 50)
		startedA := make(chan struct{})
		releaseA := make(chan struct{})
		opsA := qqCoverOps{download: func(url string) string {
			close(startedA)
			<-releaseA
			return "data:image/jpeg;base64,COVER_A"
		}}
		opsB := qqCoverOps{download: func(url string) string {
			return ""
		}}

		gA := p.NewSongGen()
		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: "SongA - SingerA"})
		p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
			Name: "SongA", Singer: "SingerA", Title: "SongA - SingerA", Cover: "https://example.com/a.jpg",
		})
		doneA := p.startCover(opsA, gA, "https://example.com/a.jpg", "SongA", "SingerA", "SongA - SingerA")
		<-startedA
		_ = drainSongInfoEvents(t, p)

		// 复现旧窗口：先发 B 宣告，其间 A 的封面下载完成并回写，之后才 NewSongGen
		p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "playing", Detail: "SongB - SingerB"})
		p.Emit(player.EventSongInfoUpdate, &player.SongInfo{
			Name: "SongB", Singer: "SingerB", Title: "SongB - SingerB", Cover: "https://example.com/b.jpg",
		})
		close(releaseA)
		<-doneA

		gB := p.NewSongGen()
		doneB := p.startCover(opsB, gB, "https://example.com/b.jpg", "SongB", "SingerB", "SongB - SingerB")
		<-doneB

		got := drainSongInfoEvents(t, p)
		sawOverwrite := false
		for _, info := range got[1:] {
			if info.Name == "SongA" && info.Title == "SongA - SingerA" && info.CoverBase64 == "data:image/jpeg;base64,COVER_A" {
				sawOverwrite = true
				break
			}
		}
		if !sawOverwrite {
			t.Fatalf("对照组本应观测到 B 宣告之后出现 SongA 覆盖回写，实际事件: %+v", got)
		}
	})
}
