package spotify

// 本文件测 runSession 里抽出来的几处判定（runSession 本身没有离线夹具，见 AGENTS §4）：
// 歌词补取的节奏、上限与落地后的状态变化（lyricsRetry）、暂停时不补取（retryDue 的播放态）、
// 时钟迁移到对外事件的翻译（clockEmits）、时长晚到（durationArrived）、歌名选择器失效的告警（untitledWatch）。
// 异步取词的状态机与取词结果的落地判定在 lyricsjob_test.go。不碰 Spotify、不碰 CDP。

import (
	"math"
	"slices"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/spotify/lyric"
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

// 第一首：前 18 次（90 秒）5 秒一次，之后 30 秒一次。
// 变异自证：due 里降频的判据改成 `>`（第 18 次之后多一次 5 秒）→ 本例与 TestLyricsRetryCap、TestRetryDuePaused 红（实测）。
func TestLyricsRetryCadenceWithinOneSong(t *testing.T) {
	var r lyricsRetry
	r.start("a", t0)
	got := tries(&r, t0, 100*time.Millisecond, 185*time.Second)
	want := []float64{5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55, 60, 65, 70, 75, 80, 85, 90, 120, 150, 180}
	sameSeconds(t, "第一首", got, want)
}

// F5：上一首等过 90 秒降到 30 秒之后换歌，下一首必须从 5 秒一次重新起步（真机曾是 +29.8s 才第一次补取）。
// 对应 2026-10-13 真机日志 03:59:01 换到有歌词的曲目、随即加载完成：补取状态随换歌复位。
// 变异自证：start 里去掉 `r.tries = 0` → 本例红（第二首沿用 30s，实测）。
func TestLyricsRetryCadenceResetsOnSongChange(t *testing.T) {
	var r lyricsRetry
	r.start("a", t0)
	tries(&r, t0, time.Second, 200*time.Second) // 第一首等到慢节奏
	if r.tries < lyricsFastTries {
		t.Fatalf("前置条件：第一首应已降到慢节奏，得到 %d 次", r.tries)
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

// 同一首歌重新 start（例如换走又换回）：次数从零重算，不沿用上一次的。
func TestLyricsRetryRestartResets(t *testing.T) {
	var r lyricsRetry
	r.start("a", t0)
	tries(&r, t0, time.Second, 100*time.Second)
	again := t0.Add(100 * time.Second)
	r.start("a", again)
	// 若次数没重算（已补 18 次），again 之后第一次就是 30 秒一次。
	sameSeconds(t, "重新 start", tries(&r, again, time.Second, 20*time.Second), []float64{5, 10, 15, 20})
}

// capSchedule 是一首歌从不间断播放、每次补取都没拿到时的全部补取时刻（秒）：5..90 每 5 秒一次，120..600 每 30 秒一次。
func capSchedule() []float64 {
	var want []float64
	for s := 5; s <= 90; s += 5 {
		want = append(want, float64(s))
	}
	for s := 120; s <= 600; s += 30 {
		want = append(want, float64(s))
	}
	return want
}

// 补取有终点（issue #111）：一直取不到时共补 lyricsMaxTries 次、约 10 分钟，之后无论多久都不再补；换歌复位。
// 变异自证（均实测）：due 不查上限 → 「一小时内」与 TestRetryDuePaused 的「播放暂停交替」红（补到换歌为止）；
// 上限比较改成 `>` → 同两例红（多补一次）；start 不清次数 → 「换歌复位」红。
func TestLyricsRetryCap(t *testing.T) {
	want := capSchedule()
	if len(want) != lyricsMaxTries || lyricsFastTries != 18 {
		t.Fatalf("前置条件：时刻表 %d 项、上限 %d、快节奏 %d 次", len(want), lyricsMaxTries, lyricsFastTries)
	}
	var r lyricsRetry
	r.start("a", t0)
	sameSeconds(t, "一小时内", tries(&r, t0, 100*time.Millisecond, time.Hour), want)
	if r.tries != lyricsMaxTries || r.trackID == "" {
		t.Fatalf("到顶后 tries=%d trackID=%q，期望 %d 且仍在等（由 land 停下）", r.tries, r.trackID, lyricsMaxTries)
	}

	// 换歌复位：从 5 秒一次重新起步，上限重新算。
	change := t0.Add(2 * time.Hour)
	r.stop()
	r.start("b", change)
	if r.tries != 0 {
		t.Fatalf("换歌后 tries=%d，期望 0", r.tries)
	}
	sameSeconds(t, "换歌复位", tries(&r, change, 100*time.Millisecond, time.Hour), want)
}

// land：结果落地后怎么动补取状态。401 / 403 在上限之内照常补取，最后一次也没拿到即停并报「到顶」一次；
// 到顶之前拿到、或平台答复 404 / UNSYNCED，当场停而不报到顶。驱动方式照 runSession：每拍 retryDue 为真即起一次，
// 结果立刻落地（settleLyrics → land）。
// 对应 2026-10-13 真机日志 03:33:09–03:43:10：页面内取词约 10 分钟回 401（token 过期），之后又回 404。
// 变异自证（均实测）：land 不查上限 → 「一直 401」「401 与 403 交替」红（due 到顶后不再补，但不报到顶、一直挂着）；
// 到顶判定不看是否仍在等 → 「第 35 次拿到」红（报了到顶）；stopRetry 时不停 → 「转成 404」「第 35 次拿到」「UNSYNCED」三例红。
func TestLyricsRetryLand(t *testing.T) {
	synced := mustParse(t, syncedBody)
	unsynced := mustParse(t, unsyncedBody)
	fail401 := lyricsResult{lyricsJob: lyricsJob{retry: true, trackID: "a"}, diag: "page-side lyrics fetch returned HTTP 401"}
	fail403 := lyricsResult{lyricsJob: lyricsJob{retry: true, trackID: "a"}, diag: "page-side lyrics fetch returned HTTP 403"}
	got404 := lyricsResult{lyricsJob: lyricsJob{retry: true, trackID: "a"}, diag: "page-side lyrics fetch returned HTTP 404", notFound: true}
	gotLines := lyricsResult{lyricsJob: lyricsJob{retry: true, trackID: "a"}, lines: lyric.Parse(synced), resp: synced}
	gotUnsynced := lyricsResult{lyricsJob: lyricsJob{retry: true, trackID: "a"}, resp: unsynced, diag: "x"}

	cases := []struct {
		name      string
		result    func(n int) lyricsResult // 第 n 次补取（从 1 起）的结果
		wantTries int
		wantGave  bool
	}{
		{"一直 401：补满上限后停、报到顶", func(int) lyricsResult { return fail401 }, lyricsMaxTries, true},
		{"401 与 403 交替：同上", func(n int) lyricsResult {
			if n%2 == 0 {
				return fail403
			}
			return fail401
		}, lyricsMaxTries, true},
		{"401 一段之后转成 404（第 25 次起）：当场停、不报到顶", func(n int) lyricsResult {
			if n >= 25 {
				return got404
			}
			return fail401
		}, 25, false},
		{"第 35 次拿到：不报到顶", func(n int) lyricsResult {
			if n == lyricsMaxTries {
				return gotLines
			}
			return fail401
		}, lyricsMaxTries, false},
		{"第 3 次 UNSYNCED：当场停", func(n int) lyricsResult {
			if n == 3 {
				return gotUnsynced
			}
			return fail401
		}, 3, false},
	}
	for _, tc := range cases {
		var j lyricsJobs
		var r lyricsRetry
		// 换歌那次：页面 401、有 URI → 开始补取。
		started, gave := r.land(settleLyrics(lyricsResult{lyricsJob: lyricsJob{trackID: "a"}, diag: "HTTP 401"}), "a", t0)
		if !started || gave {
			t.Fatalf("%s：换歌那次 401 应开始补取，得到 started=%v gaveUp=%v", tc.name, started, gave)
		}
		gaveUps := 0
		for d := time.Duration(0); d <= time.Hour; d += 100 * time.Millisecond {
			now := t0.Add(d)
			if !j.retryDue(&r, true, now) {
				continue
			}
			res := tc.result(r.tries)
			if _, g := r.land(settleLyrics(res), res.trackID, now); g {
				gaveUps++
			}
		}
		if r.tries != tc.wantTries || (gaveUps == 1) != tc.wantGave || gaveUps > 1 || r.trackID != "" {
			t.Errorf("%s：补了 %d 次、报到顶 %d 次、trackID=%q，期望 %d 次、到顶=%v、已停", tc.name, r.tries, gaveUps, r.trackID, tc.wantTries, tc.wantGave)
		}
	}

	// 换歌那次就拿到 / 404 / 没有 URI：不开始，也不报到顶。
	for _, res := range []lyricsResult{
		{lyricsJob: lyricsJob{trackID: "a"}, lines: lyric.Parse(synced), resp: synced},
		{lyricsJob: lyricsJob{trackID: "a"}, notFound: true, diag: "HTTP 404"},
		{lyricsJob: lyricsJob{}},
	} {
		var r lyricsRetry
		if started, gave := r.land(settleLyrics(res), res.trackID, t0); started || gave || r.trackID != "" {
			t.Errorf("换歌那次 %+v：started=%v gaveUp=%v trackID=%q，期望都不", res, started, gave, r.trackID)
		}
	}
}

// 暂停时不补取，也不问 due（不耗次数、不动节奏）；恢复后按暂停前的次数继续，降频与上限都只按补取次数算。
// 对应 2026-10-13 真机日志 03:01:32–03:59:00：暂停近一小时，期间每 30 秒补取一次。
// 变异自证（均实测）：retryDue 不看播放态 → 「暂停期间」红；先问 due 再看播放态（`r.due(now) && playing`）→
// 「暂停期间 tries / lastTry 不应变」红；节奏改回按距 start 的时间降频 → 「恢复后」红（暂停一小时后第一次就是 30 秒节奏）。
func TestRetryDuePaused(t *testing.T) {
	var j lyricsJobs
	var r lyricsRetry
	r.start("a", t0)

	// run 从 from 起每 100ms 一拍跑到 until（含），返回触发补取的时刻（相对 from 的秒数）。
	run := func(from time.Time, until time.Duration, playing bool) []float64 {
		var got []float64
		for d := time.Duration(0); d <= until; d += 100 * time.Millisecond {
			if j.retryDue(&r, playing, from.Add(d)) {
				got = append(got, d.Seconds())
			}
		}
		return got
	}

	// 播放 17 秒：5、10、15 三次。
	sameSeconds(t, "暂停前", run(t0, 17*time.Second, true), []float64{5, 10, 15})
	triesBefore, lastBefore := r.tries, r.lastTry

	// 暂停一小时：一次都不补，次数与上次补取的时刻都不动。
	pause := t0.Add(17 * time.Second)
	if got := run(pause, time.Hour, false); len(got) != 0 {
		t.Fatalf("暂停期间不应补取，得到 %v", got)
	}
	if r.tries != triesBefore || !r.lastTry.Equal(lastBefore) {
		t.Fatalf("暂停期间 tries / lastTry 不应变：%d→%d，%v→%v", triesBefore, r.tries, lastBefore, r.lastTry)
	}

	// 恢复：距上次补取早已超过间隔，恢复那一拍即补；之后仍是 5 秒一次，直到累计 18 次才降到 30 秒。
	resume := pause.Add(time.Hour)
	got := run(resume, 120*time.Second, true)
	want := []float64{0}
	for s := 5; len(want) < lyricsFastTries-triesBefore; s += 5 {
		want = append(want, float64(s))
	}
	last := want[len(want)-1]
	for s := last + 30; s <= 120; s += 30 {
		want = append(want, s)
	}
	sameSeconds(t, "恢复后", got, want)

	// 在飞时同样不问 due（原有约束，暂停与在飞叠加时亦然）。
	_, gen := j.start()
	before := r.tries
	if j.retryDue(&r, true, resume.Add(time.Hour)) || j.retryDue(&r, false, resume.Add(time.Hour)) || r.tries != before {
		t.Fatal("在飞时不应补取、不应耗次数")
	}
	j.accept(gen)

	// 暂停与播放交替（每 3 秒切换一次）：补取只发生在播放的拍上，总数仍止于上限。
	var r2 lyricsRetry
	r2.start("b", t0)
	var j2 lyricsJobs
	n := 0
	for d := time.Duration(0); d <= 3*time.Hour; d += 100 * time.Millisecond {
		playing := (d/(3*time.Second))%2 == 0
		if j2.retryDue(&r2, playing, t0.Add(d)) {
			if !playing {
				t.Fatalf("+%v 暂停中补取了", d)
			}
			n++
		}
	}
	if n != lyricsMaxTries || r2.tries != lyricsMaxTries {
		t.Fatalf("播放暂停交替：补了 %d 次（tries=%d），期望 %d", n, r2.tries, lyricsMaxTries)
	}
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

// untitledWatch：时长大于 0 而歌名持续为空满 untitledWarnAfter 才告警，每个会话一次。
// 变异自证（均实测）：去掉歌名非空时的复位 → 「读得到歌名」「换歌空档」「第 50 拍起为空」红；
// 去掉时长判断 → 「没有曲目」两例与「时长每 9 拍掉成 0」红；去掉 warned → 「一直为空」「恢复又失效」「第 50 拍起为空」红
// （每拍都报）；`<` 改成 `<=` → 同这三例红（晚一拍）。
func TestUntitledWatch(t *testing.T) {
	if untitledWarnAfter != 10*time.Second {
		t.Fatalf("前置条件：用例按 10 秒写，untitledWarnAfter = %v", untitledWarnAfter)
	}
	const dur = 200000.0
	// 每秒一拍，跑 120 拍，第 i 拍的歌名与时长由 at(i) 给出；返回告警的拍号。
	run := func(at func(i int) (string, float64)) []int {
		var w untitledWatch
		var got []int
		for i := 0; i < 120; i++ {
			title, d := at(i)
			if w.observe(title, d, t0.Add(time.Duration(i)*time.Second)) {
				got = append(got, i)
			}
		}
		return got
	}
	cases := []struct {
		name string
		at   func(i int) (string, float64)
		want []int
	}{
		{"一直为空：第 10 拍报一次", func(int) (string, float64) { return "", dur }, []int{10}},
		{"读得到歌名：不报", func(int) (string, float64) { return "歌名", dur }, nil},
		{"没有曲目（时长 0）：不报", func(int) (string, float64) { return "", 0 }, nil},
		{"没有曲目（时长非法）：不报", func(int) (string, float64) { return "", math.NaN() }, nil},
		{"换歌空档（每 9 拍有一拍读到歌名）：不报", func(i int) (string, float64) {
			if i%9 == 8 {
				return "歌名", dur
			}
			return "", dur
		}, nil},
		{"时长每 9 拍掉成 0 一拍：重新计时、不报", func(i int) (string, float64) {
			if i%9 == 8 {
				return "", 0
			}
			return "", dur
		}, nil},
		{"报过之后恢复又失效：本会话不再报", func(i int) (string, float64) {
			if i >= 30 && i < 40 {
				return "歌名", dur
			}
			return "", dur
		}, []int{10}},
		{"第 50 拍起为空：第 60 拍报", func(i int) (string, float64) {
			if i < 50 {
				return "歌名", dur
			}
			return "", dur
		}, []int{60}},
	}
	for _, c := range cases {
		if got := run(c.at); !slices.Equal(got, c.want) {
			t.Errorf("%s：告警于第 %v 拍，期望 %v", c.name, got, c.want)
		}
	}
}
