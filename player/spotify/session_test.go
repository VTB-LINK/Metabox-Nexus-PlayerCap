package spotify

// 本文件测 runSession 里抽出来的几处判定（runSession 本身没有离线夹具，见 AGENTS §4）：
// 歌词补取的节奏（lyricsRetry）、时钟迁移到对外事件的翻译（clockEmits）、时长晚到（durationArrived）。
// 异步取词的状态机与取词结果的落地判定在 lyricsjob_test.go。不碰 Spotify、不碰 CDP。

import (
	"math"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

// tries 从 from 起每 step 调一次 due，直到 until（含），返回触发补取的时刻（相对 from 的秒数）。
func tries(r *lyricsRetry, from time.Time, step, until time.Duration) []float64 {
	var got []float64
	for d := time.Duration(0); d <= until; d += step {
		if r.due(from.Add(d)) {
			got = append(got, d.Seconds())
		}
	}
	return got
}

func sameSeconds(t *testing.T, label string, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：补取时刻 %v，期望 %v", label, got, want)
	}
	for i := range got {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Fatalf("%s：补取时刻 %v，期望 %v", label, got, want)
		}
	}
}

// 第一首：前 90 秒 5 秒一次，到 90 秒那一次降频，之后 30 秒一次。
// 变异自证：due 里降频的判据改成 `>`（90 秒那一次不降频）→ 本例红（实测）。
func TestLyricsRetryCadenceWithinOneSong(t *testing.T) {
	var r lyricsRetry
	r.start("a", t0)
	got := tries(&r, t0, 100*time.Millisecond, 185*time.Second)
	want := []float64{5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55, 60, 65, 70, 75, 80, 85, 90, 120, 150, 180}
	sameSeconds(t, "第一首", got, want)
}

// F5：上一首等过 90 秒降到 30 秒之后换歌，下一首必须从 5 秒一次重新起步（真机曾是 +29.8s 才第一次补取）。
// 变异自证：start 里去掉 `r.every = lyricsRetryInterval` → 本例红（第一首从零值节奏起步、第二首沿用 30s，实测）。
func TestLyricsRetryCadenceResetsOnSongChange(t *testing.T) {
	var r lyricsRetry
	r.start("a", t0)
	tries(&r, t0, time.Second, 200*time.Second) // 第一首等到慢节奏
	if r.every != lyricsRetrySlow {
		t.Fatalf("前置条件：第一首应已降到慢节奏，得到 %v", r.every)
	}

	change := t0.Add(200 * time.Second)
	r.stop()
	r.start("b", change)
	sameSeconds(t, "换歌后的第二首", tries(&r, change, 100*time.Millisecond, 31*time.Second), []float64{5, 10, 15, 20, 25, 30})
}

// 反例：没开始、已停、同一时刻重复问、时间倒退、换到有词的歌（只 stop 不 start）都不补取。
func TestLyricsRetryNotDue(t *testing.T) {
	var zero lyricsRetry
	if zero.due(t0.Add(time.Hour)) {
		t.Fatal("零值（从未 start）不应补取")
	}

	var r lyricsRetry
	r.start("a", t0)
	if r.due(t0) {
		t.Fatal("start 那一刻计为第一次尝试，不应立刻再补取")
	}
	if r.due(t0.Add(4999 * time.Millisecond)) {
		t.Fatal("不足 5 秒不应补取")
	}
	if !r.due(t0.Add(5 * time.Second)) {
		t.Fatal("满 5 秒应补取")
	}
	if r.due(t0.Add(5 * time.Second)) {
		t.Fatal("同一时刻重复问只应补取一次")
	}
	if r.due(t0.Add(2 * time.Second)) {
		t.Fatal("时间倒退不应补取")
	}

	// 拿到了词 / 平台只有 UNSYNCED：stop 之后无论多久都不再补取，重复 stop 无害。
	r.stop()
	r.stop()
	if r.due(t0.Add(time.Hour)) {
		t.Fatal("stop 之后不应补取")
	}
	if r.trackID != "" {
		t.Fatalf("stop 之后 trackID 应为空，得到 %q", r.trackID)
	}
}

// 同一首歌重新 start（例如换走又换回）：等待起点重算，不沿用上一次的 since。
func TestLyricsRetryRestartResetsSince(t *testing.T) {
	var r lyricsRetry
	r.start("a", t0)
	tries(&r, t0, time.Second, 100*time.Second)
	again := t0.Add(100 * time.Second)
	r.start("a", again)
	// 若 since 没重算，again+5s 这一次就会因「已等过 90 秒」降频，下一次落在 +35s。
	sameSeconds(t, "重新 start", tries(&r, again, time.Second, 20*time.Second), []float64{5, 10, 15, 20})
}

// clockEmits：一拍的时钟迁移 → 对外事件序列。
// 变异自证：去掉 evSeek 分支里「!playing 时补发暂停」→「seek 与暂停同拍」红（实测）；
// 补发的暂停改带 livePos → 同一例红（实测；端到端那例仍绿：seek 之后时钟位置本就等于读数）；
// seek 且仍在播放时多发一条 resume → 本例与 TestClockEmitsAtMostOneResume 红（实测）。
func TestClockEmits(t *testing.T) {
	const seekPos, livePos = 60, 12.5
	resume := player.EventPlaybackResume
	pause := player.EventPlaybackPause
	cases := []struct {
		name    string
		ev      clockEvent
		playing bool
		want    []clockEmit
	}{
		{"纯 seek（播放中跳）只发 resume、不改状态", evSeek, true, []clockEmit{{resume, seekPos, ""}}},
		{"seek 与暂停同拍：resume 再 pause，都带跳变后的位置", evSeek, false, []clockEmit{{resume, seekPos, ""}, {pause, seekPos, "paused"}}},
		{"纯恢复", evResume, true, []clockEmit{{resume, livePos, "playing"}}},
		{"纯暂停", evPause, false, []clockEmit{{pause, livePos, "paused"}}},
		{"无事件（播放中）", evNone, true, nil},
		{"无事件（暂停中）", evNone, false, nil},
	}
	for _, c := range cases {
		got := clockEmits(c.ev, c.playing, seekPos, livePos)
		if len(got) != len(c.want) {
			t.Errorf("%s：得到 %+v，期望 %+v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s：第 %d 条得到 %+v，期望 %+v", c.name, i, got[i], c.want[i])
			}
		}
	}
}

// 不变量：任何输入下 playback_resume 一拍至多一条（AGENTS §5.1），且 pause 永远排在 resume 之后。
func TestClockEmitsAtMostOneResume(t *testing.T) {
	for _, ev := range []clockEvent{evNone, evResume, evPause, evSeek} {
		for _, playing := range []bool{true, false} {
			resumes, sawPause := 0, false
			for _, e := range clockEmits(ev, playing, 1, 2) {
				switch e.event {
				case player.EventPlaybackResume:
					resumes++
					if sawPause {
						t.Errorf("ev=%v playing=%v：resume 排在 pause 之后", ev, playing)
					}
				case player.EventPlaybackPause:
					sawPause = true
				default:
					t.Errorf("ev=%v playing=%v：意外的事件 %q", ev, playing, e.event)
				}
			}
			if resumes > 1 {
				t.Errorf("ev=%v playing=%v：发了 %d 条 playback_resume", ev, playing, resumes)
			}
		}
	}
}

// F1 端到端（时钟 + 翻译）：播放中同一拍里既跳位又暂停，时钟只报 evSeek、下一拍不再报暂停，
// 所以暂停只能在这一拍补发；位置是跳变后的读数。
func TestSeekAndPauseInOneTickEmitsPause(t *testing.T) {
	var c clock
	c.observe(10, true, 300, t0)
	at := t0.Add(time.Second)
	ev := c.observe(60, false, 300, at)
	if ev != evSeek {
		t.Fatalf("前置条件：播放中跳位应判 evSeek，得到 %v", ev)
	}
	got := clockEmits(ev, false, 60, float32(c.liveAt(at)))
	if len(got) != 2 || got[1].event != player.EventPlaybackPause || got[1].pos != 60 || got[1].status != "paused" {
		t.Fatalf("seek 与暂停同拍应补发 pause@60，得到 %+v", got)
	}
	if ev := c.observe(60, false, 300, at.Add(200*time.Millisecond)); ev != evNone {
		t.Fatalf("下一拍时钟不会再报暂停（这正是要在 seek 那一拍补发的原因），得到 %v", ev)
	}
}

// durationArrived：只在从未取到（≤0）且这一拍读到有效时长时补；不追后续变化。
// 变异自证：去掉 `current <= 0` → 「已有时长」两例红；`read > 0` 改成 `read >= 0` → 「仍是 0」红（均实测）。
func TestDurationArrived(t *testing.T) {
	cases := []struct {
		current, read float32
		want          bool
	}{
		{0, 200, true},
		{-1, 200, true},                 // 非法的旧值同样视为没取到
		{0, 0, false},                   // 仍是 0
		{0, -5, false},                  // 非法读数
		{0, float32(math.NaN()), false}, // NaN
		{180, 200, false},               // 已有时长：不追变化
		{180, 0, false},                 // 已有时长、这一拍读到 0
		{200, 200, false},               // 补过之后再问一次
	}
	for _, c := range cases {
		if got := durationArrived(c.current, c.read); got != c.want {
			t.Errorf("durationArrived(%v, %v) = %v，期望 %v", c.current, c.read, got, c.want)
		}
	}
}
