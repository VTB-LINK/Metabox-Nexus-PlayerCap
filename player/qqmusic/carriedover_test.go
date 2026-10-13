package qqmusic

// 本文件测 progressCarriedOver（换歌时读到的进度是否还是上一首的），并经 sessionreplay_test.go 的复述
// 回放 issue #96 的换歌序列与它的反例。
//
// QQ 音乐换歌时显示结构里的歌名与时长先换成新歌的，进度晚一些才从上一首换过来。旧写法在换歌那一拍
// 就认领，all_lyrics 带上一首的进度，首条 lyric_update 落在新歌中间的某一行，进度归零时又判一次回跳。
//
// 变异均在本 worktree 里用编辑注入、跑完即还原，结果记在各用例的注释里（均实测）。

import "testing"

// 变异自证：判据恒为假，「读数仍随上一首往前走」「读数等于上一首最后一次读数」「门槛之上 1ms」「上一首
// 几乎没走、新歌读数超过门槛」「上一首在门槛以内、新歌读数越过门槛」五例红；`ms >= prevMs` 改成
// `ms > prevMs`，「读数等于上一首最后一次读数」红；去掉门槛，「门槛以内」「恰在门槛」「上一首几乎没走、
// 新歌读数也在 0」三例红；门槛的 `>` 改成 `>=`，「恰在门槛」红；去掉 `prevName != ""`，「会话里第一首」红。
func TestProgressCarriedOver(t *testing.T) {
	cases := []struct {
		name     string
		prevName string
		prevMs   uint32
		ms       uint32
		want     bool
	}{
		{"会话里第一首：没有上一首可比，读数再大也直接认领", "", 0, 119000, false},
		{"读数仍随上一首往前走（录音的情形）", "Change of Coast", 119390, 119420, true},
		{"读数等于上一首最后一次读数（上一首暂停着）", "Change of Coast", 60000, 60000, true},
		{"读数比上一首最后一次读数少 1ms：已换过来", "Change of Coast", 119390, 119389, false},
		{"读数归零：已换过来", "Change of Coast", 119390, 150, false},
		{"新歌从上次的位置续播、续播点早于上一首：已换过来", "Change of Coast", 119390, 60000, false},
		{"门槛以内：不必等", "Change of Coast", 500, 1500, false},
		{"恰在门槛：不必等", "Change of Coast", 1000, 2000, false},
		{"门槛之上 1ms", "Change of Coast", 1000, 2001, true},
		{"上一首几乎没走、新歌读数也在 0", "Change of Coast", 0, 0, false},
		// 新歌真从 5 秒处续播时也走这一支，要等满等窗才认领：已知的代价，见 progressCarriedOver 的注释。
		{"上一首几乎没走、新歌读数超过门槛", "Change of Coast", 0, 5000, true},
		{"上一首在门槛以内、新歌读数越过门槛", "Change of Coast", 1500, 2500, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := progressCarriedOver(c.prevName, c.prevMs, c.ms); got != c.want {
				t.Fatalf("progressCarriedOver(%q, %d, %d) = %v，期望 %v", c.prevName, c.prevMs, c.ms, got, c.want)
			}
		})
	}
}

// 回放用的三首歌取自录音 01-qqmusic，已知的行起点照录音（lyric_update 的 timestamp），其余补成递增。
func coastLyrics() []lyricLine { // 《Change of Coast》：第 22 行 107.153s
	ts := make([]float32, 24)
	for i := range 22 {
		ts[i] = float32(i) * 4.8
	}
	ts[22], ts[23] = 107.153, 125
	return linesAt(ts...)
}

func utaLyrics() []lyricLine { // 《歌に形はないけれど》：44 行，第 21 行 114.798s
	ts := make([]float32, 44)
	ts[1] = 6.87
	for i := 2; i <= 18; i++ {
		ts[i] = 6.87 + float32(i-1)*5.3
	}
	copy(ts[19:24], []float32{102.699, 109.988, 114.798, 120.717, 124.897})
	copy(ts[24:27], []float32{140, 150, 160})
	copy(ts[27:31], []float32{163.796, 170.836, 173.595, 178.645})
	for i := 31; i < 44; i++ {
		ts[i] = 178.645 + float32(i-30)*7
	}
	return linesAt(ts...)
}

func zhijiangLyrics() []lyricLine { // 《枝江》：40 行，第 37 行 182.455s
	ts := make([]float32, 40)
	for i := range 37 {
		ts[i] = float32(i) * 175 / 36
	}
	ts[37], ts[38], ts[39] = 182.455, 186, 190
	return linesAt(ts...)
}

func recordingSongs() map[string]replaySong {
	return map[string]replaySong{
		"Change of Coast": {durMs: 192000, lyrics: coastLyrics()},
		"歌に形はないけれど":       {durMs: 281000, lyrics: utaLyrics()},
		"枝江":              {durMs: 195000, lyrics: zhijiangLyrics()},
		"纯音乐":             {durMs: 200000},
	}
}

// switchAt 给出「先放 from、在 switchMs 换成 to」的读数：换歌前 from 自 fromMs 起与时间同步走；换歌后
// 的前 staleTicks 拍读数仍随上一首走，之后自 toMs 起与时间同步走。每拍 30ms，同录音。
func switchAt(from string, fromMs uint32, switchMs int, to string, staleTicks int, toMs uint32) func(int) (string, uint32) {
	resetAt := switchMs + staleTicks*30
	return func(ms int) (string, uint32) {
		switch {
		case ms < switchMs:
			return from, fromMs + uint32(ms)
		case ms < resetAt:
			return to, fromMs + uint32(ms)
		default:
			return to, toMs + uint32(ms-resetAt)
		}
	}
}

// TestReplayIssue96SongChange 回放录音里的两次换歌（offset -400ms，每 30ms 一拍）。换歌那一拍读到的进度
// 仍随上一首走，下一拍归零。录音里的事件是「all_lyrics 带上一首的进度 → lyric_update 落在新歌的第 21
// （第 37）行 → playback_resume → index 0」；修正后换歌那一拍不认领，下一拍以新歌的进度认领，只发
// all_lyrics 与 index 0。
//
// 变异自证：progressCarriedOver 恒为假，两例均红，得到 `all lu22 all lu21 resume lu0` 与
// `all lu30 all lu37 resume lu0`，即录音里的序列；注入在回放里——换歌分支不等（去掉那道判断），同样红。
func TestReplayIssue96SongChange(t *testing.T) {
	cases := []struct {
		name    string
		from    string
		fromMs  uint32
		to      string
		toMs    uint32
		shape   string
		wantPos float32
	}{
		{"录音 94879：Change of Coast 在 119.4s 换到歌に形はないけれど", "Change of Coast", 119000, "歌に形はないけれど", 150, "all lu22 all lu0", 0.15},
		{"录音 140692：歌に形はないけれど在 183.8s 换到枝江", "歌に形はないけれど", 183400, "枝江", 0, "all lu30 all lu0", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newReplay(-0.4, recordingSongs())
			r.run(0, 1200, 30, switchAt(c.from, c.fromMs, 420, c.to, 1, c.toMs))
			r.expectShape(t, c.shape)
			if e := r.find(t, "all_lyrics", 1); e.song != c.to || !approx(e.pos, c.wantPos) {
				t.Errorf("新歌的 all_lyrics：%v，期望 %s 的进度 %.3f", e, c.to, c.wantPos)
			}
		})
	}
}

// TestReplayFirstSongMidway 会话里第一首：开始监听时歌已放到 119s，没有上一首可比，当拍认领。
//
// 变异自证：去掉 `prevName != ""`，本例红（等满 2s 才认领，前 300ms 里一个事件都没有）。
func TestReplayFirstSongMidway(t *testing.T) {
	r := newReplay(-0.4, recordingSongs())
	r.run(0, 300, 30, func(int) (string, uint32) { return "歌に形はないけれど", 119000 })
	r.expectShape(t, "all lu21")
}

// TestReplayOldSongPausedThenSwitch 上一首暂停着（计时器停在 60s）时换歌：换歌后的几拍读数仍是 60s，
// 等它归零再认领；认领不发 playback_resume。
//
// 变异自证：`ms >= prevMs` 改成 `ms > prevMs`，本例红（`all lu12 pause all lu11 resume lu0`）；
// progressCarriedOver 恒为假，同样红。
func TestReplayOldSongPausedThenSwitch(t *testing.T) {
	r := newReplay(-0.4, recordingSongs())
	r.run(0, 2700, 30, func(ms int) (string, uint32) {
		switch {
		case ms < 2010:
			return "Change of Coast", 60000
		case ms < 2340:
			return "歌に形はないけれど", 60000
		default:
			return "歌に形はないけれど", uint32(ms - 2340)
		}
	})
	r.expectShape(t, "all lu12 pause all lu0")
}

// TestReplayConsecutiveSwitchWhileWaiting 等进度换过来的时候又换了一首：中间那首从未认领，不发任何事件；
// 等窗按最后那首重新起算。
//
// 变异自证：progressCarriedOver 恒为假，本例红（中间那首被认领并发出第 21 行）。
func TestReplayConsecutiveSwitchWhileWaiting(t *testing.T) {
	r := newReplay(-0.4, recordingSongs())
	r.run(0, 1200, 30, func(ms int) (string, uint32) {
		switch {
		case ms < 420:
			return "Change of Coast", 119000 + uint32(ms)
		case ms < 480:
			return "歌に形はないけれど", 119000 + uint32(ms)
		case ms < 540:
			return "枝江", 119000 + uint32(ms)
		default:
			return "枝江", 100 + uint32(ms-540)
		}
	})
	r.expectShape(t, "all lu22 all lu0")
	if e := r.find(t, "all_lyrics", 1); e.song != "枝江" || !approx(e.pos, 0.1) {
		t.Errorf("第二条 all_lyrics：%v，期望枝江的进度 0.100", e)
	}
}

// TestReplayQuickSwitchAfterClaim 新歌认领后不到 1 秒又换歌：读数仍随上一首走，但在门槛以内，当拍认领，
// 不等。
//
// 变异自证：去掉门槛，本例红（等到读数归零才认领，第三条 all_lyrics 的进度成了 0.040）。
func TestReplayQuickSwitchAfterClaim(t *testing.T) {
	r := newReplay(-0.4, recordingSongs())
	r.run(0, 1500, 30, func(ms int) (string, uint32) {
		switch {
		case ms < 420:
			return "Change of Coast", 119000 + uint32(ms)
		case ms < 450:
			return "歌に形はないけれど", 119000 + uint32(ms)
		case ms < 1050:
			return "歌に形はないけれど", 150 + uint32(ms-450)
		case ms < 1110:
			return "枝江", 150 + uint32(ms-450)
		default:
			return "枝江", 40 + uint32(ms-1110)
		}
	})
	r.expectShape(t, "all lu22 all lu0 all lu0")
	if e := r.find(t, "all_lyrics", 2); e.song != "枝江" || !approx(e.pos, 0.75) {
		t.Errorf("第三条 all_lyrics：%v，期望枝江、进度 0.750（换歌那一拍的读数）", e)
	}
}

// TestReplaySettleWindowExpires 进度一直没换过来：等满 progressSettleWindow 按当时的读数认领（等待有界），
// 之后进度归零照旧判一次回跳。首个换歌拍在 420ms，等窗到 2420ms，其后第一拍是 2430ms。
//
// 变异自证：注入在回放里——换歌分支改成只要判为没换过来就一直等（不经等窗），本例红（`all lu22 all lu0`，
// 到 3000ms 读数归零才认领）。
func TestReplaySettleWindowExpires(t *testing.T) {
	r := newReplay(-0.4, recordingSongs())
	r.run(0, 3300, 30, func(ms int) (string, uint32) {
		switch {
		case ms < 420:
			return "Change of Coast", 119000 + uint32(ms)
		case ms < 3000:
			return "歌に形はないけれど", 119000 + uint32(ms)
		default:
			return "歌に形はないけれど", 100 + uint32(ms-3000)
		}
	})
	r.expectShape(t, "all lu22 all lu22 resume lu0")
	if e := r.find(t, "all_lyrics", 1); !approx(e.pos, 121.43) {
		t.Errorf("等满之后认领的进度 %.3f，期望 2430ms 那一拍的读数 121.430", e.pos)
	}
}

// TestReplayNewSongResumesBelowOld 新歌从 60s 处续播（低于上一首的 119s）：判为已换过来，当拍认领。
func TestReplayNewSongResumesBelowOld(t *testing.T) {
	r := newReplay(-0.4, recordingSongs())
	r.run(0, 600, 30, switchAt("Change of Coast", 119000, 420, "歌に形はないけれど", 0, 60000))
	r.expectShape(t, "all lu22 all lu11")
	if e := r.find(t, "all_lyrics", 1); !approx(e.pos, 60) {
		t.Errorf("新歌的 all_lyrics 进度 %.3f，期望 60.000", e.pos)
	}
}

// TestReplayNoLyricsSong 换到没有歌词的歌：同样等进度换过来；all_lyrics 与 index -1 的 lyric_update 都带
// 新歌的进度。
//
// 变异自证：progressCarriedOver 恒为假，本例红（两条都带 119.42，随后一条 playback_resume）。
func TestReplayNoLyricsSong(t *testing.T) {
	r := newReplay(-0.4, recordingSongs())
	r.run(0, 900, 30, switchAt("Change of Coast", 119000, 420, "纯音乐", 1, 150))
	r.expectShape(t, "all lu22 all lu-1")
	for _, e := range r.evs[2:] {
		if !approx(e.pos, 0.15) {
			t.Errorf("%v：期望新歌的进度 0.150", e)
		}
	}
}

// TestReplayAbandonedWaitThenSwitchAgain 等进度换过来的时候又切回了已认领的那首（B 从未认领）：这次等待作废，
// 过了原来的等窗之后再切到 B，要重新开窗，不能沿用早已过期的登记当拍认领（Codex 审查 R1）。
// 顺序：A 认领 → B（读数仍随 A 走，开窗）→ 切回 A 放到 3s → 再切到 B（读数仍随 A 走）→ B 的进度归零。
//
// 变异自证：注入在回放里——切回已认领的歌时不清等待的登记，本例红（第二次切到 B 按过期的等窗当拍认领，
// all_lyrics 带 A 的进度 122.x，而不是 B 的 0.1）。
func TestReplayAbandonedWaitThenSwitchAgain(t *testing.T) {
	r := newReplay(-0.4, recordingSongs())
	r.run(0, 3300, 30, func(ms int) (string, uint32) {
		switch {
		case ms < 420:
			return "Change of Coast", 119000 + uint32(ms)
		case ms < 450:
			return "歌に形はないけれど", 119000 + uint32(ms)
		case ms < 3000:
			return "Change of Coast", 119000 + uint32(ms)
		case ms < 3030:
			return "歌に形はないけれど", 119000 + uint32(ms)
		default:
			return "歌に形はないけれど", 100 + uint32(ms-3030)
		}
	})
	n := 0
	for _, e := range r.evs {
		if e.typ == "all_lyrics" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("all_lyrics %d 条，期望 2（A、B 各一）；明细：%v", n, r.evs)
	}
	if e := r.find(t, "all_lyrics", 1); e.song != "歌に形はないけれど" || !approx(e.pos, 0.1) {
		t.Errorf("B 的 all_lyrics：%v，期望 B 的进度 0.100；明细：%v", e, r.evs)
	}
}

// TestReplaySameNameAgainAfterWait 等过一次的歌之后又换回来：认领时清掉等待的登记，再次换回来时重新开窗。
// 顺序：Change of Coast → 歌に形はないけれど（等一拍）→ 枝江（当拍认领）→ 歌に形はないけれど（读数仍随
// 枝江走，要等）。
//
// 变异自证：注入在回放里——认领时不清等待的登记，本例红（第二次换回来按上一次早已过期的等窗当拍认领，
// 带着枝江的进度，随后判一次回跳）。
func TestReplaySameNameAgainAfterWait(t *testing.T) {
	r := newReplay(-0.4, recordingSongs())
	r.run(0, 11000, 30, func(ms int) (string, uint32) {
		switch {
		case ms < 420:
			return "Change of Coast", 119000 + uint32(ms)
		case ms < 450:
			return "歌に形はないけれど", 119000 + uint32(ms)
		case ms < 5460:
			return "歌に形はないけれど", 150 + uint32(ms-450)
		case ms < 10470:
			return "枝江", 30 + uint32(ms-5460)
		case ms < 10500:
			return "歌に形はないけれど", 30 + uint32(ms-5460)
		default:
			return "歌に形はないけれど", 20 + uint32(ms-10500)
		}
	})
	r.expectShape(t, "all lu22 all lu0 all lu0 lu1 all lu0")
	if e := r.find(t, "all_lyrics", 3); !approx(e.pos, 0.02) {
		t.Errorf("换回来之后的 all_lyrics 进度 %.3f，期望 0.020", e.pos)
	}
}
