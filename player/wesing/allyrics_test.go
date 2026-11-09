package wesing

// 本文件只测宣告的 all_lyrics 确认入队（设计 §4.4「all_lyrics 确认入队」、§6）：宣告时没入队（事件通道满）
// 就每拍按本拍的位置与时长再试，入队了把当前行重发；尚未到首行时不发 lyric_update。当前行同样入队之后才记为
// 已发（代码审 F3）。两种模式共用。
//
// 通道满用容量为 0 的事件通道模拟（TryEmit 恒失败），腾出空间就换成有容量的通道。

import (
	"fmt"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/wesing/lyric"
)

const annTitle = "红日 - 李克勤"

func annLines(first float32) []lyric.LyricLine {
	return []lyric.LyricLine{
		{Index: 0, Time: first, Text: "第一行"},
		{Index: 1, Time: first + 2, Text: "第二行"},
		{Index: 2, Time: first + 9, Text: "第三行"},
	}
}

// annSetup 宣告一首歌：position 是宣告时的位置；room 为假时事件通道是满的。
func annSetup(lines []lyric.LyricLine, position, duration float32, room bool) (*WesingPlayer, *allLyricsAnnounce) {
	p := newTestPlayer()
	p.EventCh = make(chan player.Event)
	if room {
		p.EventCh = make(chan player.Event, 64)
	}
	ann := &allLyricsAnnounce{title: annTitle, items: toLyricItems(lines, 0)}
	ann.send(p.TryEmit, position, duration)
	return p, ann
}

// describe 把事件列成「类型@位置」的序列，供断言先后与位置。
func describe(evts []player.Event) []string {
	var out []string
	for _, e := range evts {
		switch d := e.Data.(type) {
		case *player.AllLyricsData:
			out = append(out, fmt.Sprintf("all_lyrics@%.1f/%.0f", d.Position, d.Duration))
		case *player.LyricUpdate:
			out = append(out, fmt.Sprintf("lyric_update#%d@%.1f", d.Index, d.Position))
		case *player.PlaybackTimeInfo:
			out = append(out, fmt.Sprintf("%s@%.1f", e.Type, d.Position))
		default:
			out = append(out, e.Type)
		}
	}
	return out
}

// 首次失败、第 3 拍成功：all_lyrics 只成功发出一次，带第 3 拍的位置；成功之后当前行重发（前两拍推出的
// 当前行没入队，all_lyrics 又会清掉服务端缓存的 lyric_update）。
//
// 变异自证（实测）：宣告改回不确认（发出即记为已发），第 3 拍不再补发而红；send 不看是否已发过，第 4 拍又发
// 一次 all_lyrics 而红。入队成功后的复位由 TestAllLyricsQueuedAfterCurrentLine 守：这里前两拍的当前行同样没入队，
// 复位与否结果相同。
func TestAllLyricsRetriedUntilQueued(t *testing.T) {
	lines := annLines(1)
	p, ann := annSetup(lines, 1.5, 200, false)
	idx := -1
	idx = p.lyricTail(ann, lines, idx, 1.6, 0, 200)
	idx = p.lyricTail(ann, lines, idx, 1.7, 0, 200)
	p.EventCh = make(chan player.Event, 64) // 通道腾出空间
	idx = p.lyricTail(ann, lines, idx, 1.8, 0, 200)
	idx = p.lyricTail(ann, lines, idx, 1.9, 0, 200)
	got := fmt.Sprint(describe(drainEvents(p)))
	if want := fmt.Sprint([]string{"all_lyrics@1.8/200", "lyric_update#0@1.8"}); got != want {
		t.Fatalf("事件 %s，want %s", got, want)
	}
	if idx != 0 {
		t.Errorf("lastLineIdx = %d，want 0", idx)
	}
}

// 当前行先入队、宣告后入队：某一拍 all_lyrics 没入队，紧随的当前行却赶上通道腾出位置而入队；下一拍 all_lyrics
// 入队，会清掉服务端缓存的当前行，所以当前行要再发一次。
//
// 变异自证（实测）：入队成功后不复位 lastLineIdx，当前行没有重发而红。
func TestAllLyricsQueuedAfterCurrentLine(t *testing.T) {
	lines := annLines(1)
	p, ann := annSetup(lines, 1.5, 200, false)
	p.EventCh = make(chan player.Event, 64)
	idx := p.lyricTail(ann, lines, 0, 1.8, 0, 200) // 当前行（第 0 行）已在上一拍入队
	if got, want := fmt.Sprint(describe(drainEvents(p))), fmt.Sprint([]string{"all_lyrics@1.8/200", "lyric_update#0@1.8"}); got != want {
		t.Errorf("事件 %s，want %s", got, want)
	}
	if idx != 0 {
		t.Errorf("lastLineIdx = %d，want 0", idx)
	}
}

// F4 的反例：宣告时位置 1 s、没入队；重试期间在 2 s 判为暂停，暂停事件先入队，随后的重试成功。发出的
// all_lyrics 位置须为 2 s：服务端收到 all_lyrics 会无条件覆盖缓存的位置，沿用宣告时的 1 s 会把已入队的
// 暂停位置覆盖回去。暂停事件在这里由用例代 pollLyrics 发出（pollLyrics 的重试排在本拍暂停判定之后）。
//
// 变异自证（实测）：send 沿用第一次的位置，发出 1.0 而红；沿用第一次的时长，「重试按当前时长」一例发出 180 而红。
func TestAllLyricsRetryUsesThisBeatPosition(t *testing.T) {
	lines := annLines(5)
	p, ann := annSetup(lines, 1, 200, false)
	p.lyricTail(ann, lines, -1, 1.5, 0, 200) // 仍是满的
	p.EventCh = make(chan player.Event, 64)
	p.Emit(player.EventPlaybackPause, &player.PlaybackTimeInfo{Position: 2, Progress: player.ClampProgress(2, 200)})
	p.lyricTail(ann, lines, -1, 2, 0, 200)
	evts := drainEvents(p)
	if got, want := fmt.Sprint(describe(evts)), fmt.Sprint([]string{"playback_pause@2.0", "all_lyrics@2.0/200"}); got != want {
		t.Fatalf("事件 %s，want %s", got, want)
	}
	d := evts[1].Data.(*player.AllLyricsData)
	if d.Progress != player.ClampProgress(2, 200) || d.Title != annTitle || d.Count != len(lines) || len(d.Lyrics) != len(lines) {
		t.Errorf("all_lyrics 内容不对：%+v", d)
	}

	t.Run("重试按当前时长", func(t *testing.T) {
		p, ann := annSetup(lines, 1, 180, false)
		p.EventCh = make(chan player.Event, 64)
		p.lyricTail(ann, lines, -1, 2, 0, 181.4) // 宣告之后时长被纠正
		if got, want := fmt.Sprint(describe(drainEvents(p))), "[all_lyrics@2.0/181]"; got != want {
			t.Errorf("事件 %s，want %s", got, want)
		}
	})
}

// 首行在 10 s、成功时位置 2 s：all_lyrics 发出，没有当前行，不发 lyric_update。
//
// 变异自证（实测）：send 沿用第一次的位置，发出 1.0 而红；发出即记为已发，没有补发而红。
func TestAllLyricsRetryBeforeFirstLine(t *testing.T) {
	lines := annLines(10)
	p, ann := annSetup(lines, 1, 200, false)
	p.EventCh = make(chan player.Event, 64)
	if idx := p.lyricTail(ann, lines, -1, 2, 0, 200); idx != -1 {
		t.Errorf("lastLineIdx = %d，want -1", idx)
	}
	if got, want := fmt.Sprint(describe(drainEvents(p))), "[all_lyrics@2.0/200]"; got != want {
		t.Errorf("事件 %s，want %s", got, want)
	}
}

// 宣告即入队：之后各拍不再发 all_lyrics，也不复位当前行。
//
// 变异自证（实测）：send 不看是否已发过，每拍都发一次 all_lyrics、当前行跟着重发而红。
func TestAllLyricsQueuedAtAnnouncement(t *testing.T) {
	lines := annLines(1)
	p, ann := annSetup(lines, 1.5, 200, true)
	idx := -1
	for _, tm := range []float32{1.6, 1.7, 3.1, 3.2} {
		idx = p.lyricTail(ann, lines, idx, tm, 0, 200)
	}
	got := fmt.Sprint(describe(drainEvents(p)))
	if want := fmt.Sprint([]string{"all_lyrics@1.5/200", "lyric_update#0@1.6", "lyric_update#1@3.1"}); got != want {
		t.Errorf("事件 %s，want %s", got, want)
	}
}

// F3 的反例（代码审）：通道只剩一个槽位，补发的 all_lyrics 入队、紧随的当前行没入队；当前行不记为已发，
// 下一拍通道有空位时按那一拍的位置补发，之后不重复发送。
//
// 变异自证（实测）：当前行改回先记行号、再发（不看 TryEmit 的结果），第二拍没有补发而红。
func TestCurrentLineRetriedUntilQueued(t *testing.T) {
	lines := annLines(1)
	p, ann := annSetup(lines, 1.5, 200, false)
	p.EventCh = make(chan player.Event, 1) // 只剩一个槽位
	idx := p.lyricTail(ann, lines, -1, 1.8, 0, 200)
	if got, want := fmt.Sprint(describe(drainEvents(p))), "[all_lyrics@1.8/200]"; got != want {
		t.Fatalf("第一拍事件 %s，want %s", got, want)
	}
	if idx != -1 {
		t.Errorf("当前行没入队，lastLineIdx = %d，want -1", idx)
	}
	idx = p.lyricTail(ann, lines, idx, 1.9, 0, 200)
	if got, want := fmt.Sprint(describe(drainEvents(p))), "[lyric_update#0@1.9]"; got != want {
		t.Errorf("第二拍事件 %s，want %s", got, want)
	}
	idx = p.lyricTail(ann, lines, idx, 2.0, 0, 200)
	if got := describe(drainEvents(p)); len(got) != 0 || idx != 0 {
		t.Errorf("第三拍事件 %v、lastLineIdx = %d，want 无事件、0", got, idx)
	}
}

// 一直失败到本首结束：每拍立即返回，不阻塞轮询，也不记为已发出。
//
// 变异自证（实测）：发出即记为已发（不看 TryEmit 的结果），「不该记为已发出」红。
func TestAllLyricsRetryNeverBlocks(t *testing.T) {
	lines := annLines(1)
	p, ann := annSetup(lines, 1, 200, false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		idx := -1
		for i := range 2000 {
			idx = p.lyricTail(ann, lines, idx, 1+float32(i)*0.03, 0, 200)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("通道一直满时 lyricTail 阻塞了")
	}
	if ann.sent {
		t.Error("通道一直满，不该记为已发出")
	}
}
