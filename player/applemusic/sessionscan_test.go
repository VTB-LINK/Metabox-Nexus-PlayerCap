package applemusic

// 本文件只测全扫途中的事：扫描的那零点几秒里照常读位置，扫完按那一刻的状态用这份快照。

import (
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/applemusic/mem"
)

// 开播落在全扫途中（换歌本身会触发全扫，2026-10-08 真机见过时长变正晚了 0.29s 才被看见）：位置从途中
// 读到它的那一刻起算，不是扫完才起算；这份快照扫完就用来宣告新歌，不必再等一次全扫（真机上那要多等
// 约 0.5s）。
//
// 变异自证：把 maybeScan 里传给 Scan 的 s.scanTick 换成 nil（扫描途中不读位置），开播要等扫完才看见，
// 位置比该有的少 0.2s（0.1 对 0.3）而红；把扫完后重新算 songReady 的那一行删掉（沿用扫描前「还在加载」
// 的判断），新歌要再扫一次才宣告而红。
func TestSessionSongStartSeenDuringScan(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("A")
	h.setTwin(200, 10, true)
	h.run(5 * time.Second)

	h.setTwin(0, 0, false) // 切歌：加载中
	h.run(500 * time.Millisecond)
	from, scans := h.mark(), h.f.scans
	var startedAt time.Time
	h.f.midScan = func(during func()) {
		h.f.midScan = nil
		h.now = h.now.Add(100 * time.Millisecond)
		h.setTwin(260, 0, true) // 扫到一半，新歌开播
		startedAt = h.now
		if during != nil {
			during()
		}
		h.now = h.now.Add(200 * time.Millisecond) // 扫描的后半程
	}
	h.f.songs = np("B") // 存储换了 → 盯梢发现 → 全扫
	for i := 0; i < 10 && startedAt.IsZero(); i++ {
		h.run(step)
	}
	if startedAt.IsZero() {
		t.Fatal("存储换了之后 1s 内应有一次全扫")
	}
	// run 的最后一步在全扫返回之后又推进了一个 step。
	want := h.now.Sub(startedAt).Seconds()
	if got := h.s.clk.live(h.now); h.s.song.dur != 260 || !near(got, want) {
		t.Fatalf("新歌时长=%.0f 位置=%.3f，want 260 与 %.3f（从途中看到开播的那一刻起算）", h.s.song.dur, got, want)
	}
	if si := songInfos(h.since(from)); len(si) != 1 || si[0].Name != "B" || h.f.scans != scans+1 {
		t.Fatalf("横跨开播的那一次全扫就该宣告 B，got %d 条 song_info_update、全扫 %d 次", len(si), h.f.scans-scans)
	}
}

// 拖动进度条恰逢全扫：全扫途中约每 20ms 读一次位置，目标值闪现后弹回（真机闪现 10~14ms，或约 220ms）、
// 之后才真正过去。闪现被连着读到多少次都不认：整次拖动只发一条 playback_resume，闪现期间歌词行不跳，
// 只在真正过去后推一次目标位置的那一行。
//
// 变异自证：把 clock.holdJump 末尾的 `now.Sub(pendingAt) >= jumpHold` 改成 true（连着两次即认，
// 旧实现），两例都在闪现时发出 playback_resume、推出第 2 行，弹回时再发一条、推出第 0 行而红；把
// jumpHold 改回 40ms，「闪现约 220ms」一例同样而红。
func TestSessionSeekFlickerDuringScanSingleResume(t *testing.T) {
	for _, tc := range []struct {
		name            string
		scanMs, flashTo int // 这次全扫多长；目标值从第 20ms 那次读数起闪现，到第几毫秒的那次读数为止
	}{
		{"闪现 15~42ms", 100, 40},
		{"闪现约 220ms", 300, 220},
	} {
		h := newHarness(t)
		h.f.songs = np("拖动")
		h.f.docs = []mem.TTMLDoc{{Text: ttmlAt(300.4, 10, 100, 280), Count: 1}}
		h.setTwin(300, 20, true)
		h.run(5 * time.Second)
		if got := lineIdxs(h.evs); len(got) != 1 || got[0] != 0 {
			t.Fatalf("%s 前置：应已推出第 0 行，got %v", tc.name, got)
		}

		from := h.mark()
		scanned := false
		h.f.midScan = func(during func()) {
			h.f.midScan = nil
			scanned = true
			cur := h.f.twins[realTwin]
			for ms := 20; ms <= tc.scanMs; ms += 20 { // 每 20ms 读一次位置
				h.now = h.now.Add(20 * time.Millisecond)
				tw := cur
				if ms <= tc.flashTo {
					tw.Pos = 285 // 闪现
				}
				h.f.twins[realTwin] = tw
				during()
			}
		}
		h.f.songs = []mem.NowPlaying{store("拖动", "换了一张图")} // 存储换了 → 盯梢发现 → 全扫
		for i := 0; i < 10 && !scanned; i++ {
			h.run(step)
		}
		if !scanned {
			t.Fatalf("%s：存储换了之后 1s 内应有一次全扫", tc.name)
		}
		h.run(400 * time.Millisecond)
		if evs := h.since(from); count(evs, player.EventPlaybackResume) != 0 || len(lineIdxs(evs)) != 0 {
			t.Fatalf("%s：闪现不应认成跳转，got playback_resume=%d 歌词行=%v", tc.name, count(evs, player.EventPlaybackResume), lineIdxs(evs))
		}

		h.setTwin(300, 285, true) // 真正过去
		h.run(3 * time.Second)
		evs := h.since(from)
		if rs := resumes(evs); len(rs) != 1 || rs[0].Position < 285 || rs[0].Position > 286 {
			t.Fatalf("%s：一次拖动应只发一条 playback_resume、位置在 285~286，got %d 条 %+v", tc.name, len(rs), rs)
		}
		if got := lineIdxs(evs); len(got) != 1 || got[0] != 2 {
			t.Fatalf("%s：歌词行应只推一次目标位置的第 2 行，got %v", tc.name, got)
		}
		if st := statuses(evs); len(st) != 0 {
			t.Fatalf("%s：播放中拖动不改变播放态，不应发 status_update，got %v", tc.name, st)
		}
	}
}

// 扫到一半切了歌（时长归零）：不能再拿这份快照给上一首配歌词、发事件——扫描开始前「歌已就绪」的
// 判断此刻已经不成立。
//
// 变异自证：把 maybeScan 里扫完后重新算 songReady 的那一行删掉（沿用扫描前的判断），上一首在加载中
// 补发了一条 all_lyrics 而红。
func TestSessionSkipDuringScanNoLyricsForOldSong(t *testing.T) {
	h := newHarness(t)
	h.f.songs = np("A")
	h.setTwin(200, 10, true)
	h.run(5 * time.Second) // A 已宣告、还没有歌词，在按节奏找

	from := h.mark()
	scans := h.f.scans
	h.f.docs = []mem.TTMLDoc{{Text: ttmlDoc(200.4, "A 的歌词"), Count: 1}} // 歌词到了，下一次全扫会扫到
	h.f.midScan = func(during func()) {
		h.f.midScan = nil
		h.setTwin(0, 0, false) // 扫到一半，用户切歌
		if during != nil {
			during()
		}
	}
	for i := 0; i < 30 && h.f.scans == scans; i++ {
		h.run(step)
	}
	if h.f.scans == scans {
		t.Fatal("没有歌词时 3s 内应有一次全扫")
	}
	h.run(time.Second)
	if evs := h.since(from); count(evs, player.EventAllLyrics) != 0 || count(evs, player.EventLyricUpdate) != 0 {
		t.Fatalf("切歌之后不应再给上一首发歌词，got all_lyrics=%d lyric_update=%d",
			count(evs, player.EventAllLyrics), count(evs, player.EventLyricUpdate))
	}
}
