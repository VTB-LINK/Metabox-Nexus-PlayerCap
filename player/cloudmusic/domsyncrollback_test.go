package cloudmusic

// 本文件只测对表回拨与其它环节的衔接：回拨之后行号不回退、各处对外位置都在回拨后的时间线上，
// 以及对表与 seek 检测的先后。夹具与它的边界见 domsyncreplay_test.go 的文件头：回放不经 runSession
// 本身，注释里「注入在回放里」的变异改的是 replay 的对应步骤。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// rollbackAtBoundary 回放「校准恰逢行边界」的时间线（设计审查给出）到回拨的那一拍为止：seek 之后一直
// 在播，p(ms) = ms，时钟也是 p（落后 0）；offset 0.5s。上一拍 sent=950、got=955、秒数 0；本拍 sent=1050、
// got=1055、秒数 1：u = 0.105，区间上界 0.805，时钟 1.055 回拨到 0.805，行匹配下限记 1.055。
func rollbackAtBoundary(lines []float32) *replay {
	r := newReplay(0)
	r.offset, r.lines = 0.5, lines
	r.tick(850, 855, 0, true)
	r.tick(950, 955, 0, true)
	r.tick(1050, 1055, 1, true)
	return r
}

// eventsAt 返回某一拍（按 got）发出的事件，不含位置采样。
func (r *replay) eventsAt(ms int) (out []replayEvent) {
	for _, e := range r.events {
		if e.ms == ms && e.cause != "sample" {
			out = append(out, e)
		}
	}
	return out
}

// lineEvents 返回各条 lyric_update 的行号。
func (r *replay) lineEvents() (out []int) {
	for _, e := range r.events {
		if e.cause == "line" {
			out = append(out, e.line)
		}
	}
	return out
}

// TestReplayRollbackAtLineBoundary 校准恰逢行边界：下一行的显示边界在 1.50，回拨的那一拍行匹配时间
// 1.555 已选中新行。这一拍对外只有那条 lyric_update，位置是回拨后的 0.805，不另发 playback_resume；
// 其后各拍行号不回退。
//
// 变异自证（注入在回放里，均实测）：行匹配不取下限（matchLine 取 at），本例红（下一拍退回第 0 行，再过
// 一拍又推第 1 行）；lyric_update 的位置取 lineAt，本例红（位置 1.055，前端被锚回回拨前的时间线）；
// 通知检查挪到 lyric_update 之前，本例红（同一拍先发了一条 playback_resume）。
// 注入在时钟：回拨不记下限、lineAt 不取下限，本例均红；注入在 untold：anchored 不清零，本例红（同一拍
// lyric_update 之后又发了一条 playback_resume）（均实测）。
func TestReplayRollbackAtLineBoundary(t *testing.T) {
	r := rollbackAtBoundary([]float32{0.2, 1.5, 30.0})

	if len(r.adjusts) != 1 || r.adjusts[0].ms != 1055 || !inRange(r.adjusts[0].delta, -0.25, -0.25) {
		t.Fatalf("应在 1055ms 回拨 0.25s，实得 %v", r.adjusts)
	}
	evs := r.eventsAt(1055)
	if len(evs) != 1 || evs[0].typ != player.EventLyricUpdate || evs[0].line != 1 {
		t.Fatalf("回拨的那一拍对外应只有第 1 行的 lyric_update，实得 %v", evs)
	}
	if !inRange(evs[0].pos, 0.805, 0.805) {
		t.Errorf("lyric_update 的位置 %.3f，应为回拨后的 0.805", evs[0].pos)
	}
	// 前端锚在回拨后的时间线上：落后量 = 1.055 − 0.805 = 0.25，在 [L−u, L] 之内。
	if lag := float32(1.055) - r.frontPos(1055); !inRange(lag, domLagSec-0.105, domLagSec) {
		t.Errorf("前端落后 %.3fs，不在 [L−u, L] 之内", lag)
	}

	timeline{}.play(r, 1150, 6000, replayPoll, 5)
	if got := r.lineEvents(); len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("行号应只往前走（0、1），实得 %v", got)
	}
	if n := r.count("sync") + r.count("seek"); n != 0 {
		t.Errorf("未告知量已由 lyric_update 带到，不该再发 playback_resume，实得 %v", r.events)
	}
}

// TestReplayRollbackAwayFromLineBoundary 同一时间线，行边界不在回拨的那一拍：这一拍末尾发由校准触发的
// playback_resume，位置是回拨后的 0.805。
//
// 变异自证（均实测）：注入在回放里——通知的位置取 lineAt，本例红（位置 1.055）；注入在 untold——
// 只判向前（回拨不通知）、due 恒返回 false，本例红。
func TestReplayRollbackAwayFromLineBoundary(t *testing.T) {
	r := rollbackAtBoundary([]float32{0.2, 30.0})

	evs := r.eventsAt(1055)
	if len(evs) != 1 || evs[0].typ != player.EventPlaybackResume || evs[0].cause != "sync" {
		t.Fatalf("回拨的那一拍应只发一条由校准触发的 playback_resume，实得 %v", evs)
	}
	if !inRange(evs[0].pos, 0.805, 0.805) {
		t.Errorf("通知的位置 %.3f，应为回拨后的 0.805", evs[0].pos)
	}

	timeline{}.play(r, 1150, 6000, replayPoll, 5)
	if got := r.lineEvents(); len(got) != 1 || got[0] != 0 {
		t.Errorf("没有换行，不该有新的 lyric_update，实得 %v", got)
	}
	if n := r.count("sync"); n != 1 {
		t.Errorf("通知应只有一条，实得 %d 条", n)
	}
}

// TestReplayPauseBeforeClockPassesFloor 回拨之后、时钟走过下限之前暂停：playback_pause 的位置是时钟读数
// （回拨后的时间线），恢复后行号不回退。
//
// 回拨后下一拍（got=1155）读到暂停：时钟 0.905，下限 1.055。暂停到 5050ms 恢复，其间与恢复后的头几拍
// 时钟仍低于下限。
//
// 变异自证（均实测）：注入在时钟——冻结取 lineAt，本例红（playback_pause 的位置 1.055）；冻结时清掉下限，
// 本例红（恢复后退回第 0 行）。注入在回放里——playback_pause 的位置取 lineAt，本例红；行匹配不取下限，
// 本例红。
func TestReplayPauseBeforeClockPassesFloor(t *testing.T) {
	r := rollbackAtBoundary([]float32{0.2, 1.5, 30.0})
	for ms := 1150; ms < 5050; ms += replayPoll {
		r.tick(ms, ms+5, 1, false)
	}

	evs := r.eventsAt(1155)
	if len(evs) != 1 || evs[0].typ != player.EventPlaybackPause {
		t.Fatalf("1155ms 应只发 playback_pause，实得 %v", evs)
	}
	if !inRange(evs[0].pos, 0.905, 0.905) {
		t.Errorf("playback_pause 的位置 %.3f，应为时钟读数 0.905（不是行匹配下限 1.055）", evs[0].pos)
	}
	if got := r.frontPos(4000); !inRange(got, 0.905, 0.905) {
		t.Errorf("暂停中前端位置 %.3f，应停在 0.905", got)
	}

	// 恢复：界面进度从 1.65 起走（暂停后又走了 0.5s），秒数在 5400ms 跳到 2。
	tl := timeline{p0: 1650 - 5050}
	for ms := 5050; ms < 9000; ms += replayPoll {
		r.tick(ms, ms+5, tl.sec(ms), true)
	}
	if evs := r.eventsAt(5055); len(evs) != 1 || evs[0].typ != player.EventPlaybackResume || !inRange(evs[0].pos, 0.905, 0.905) {
		t.Errorf("恢复那一拍应只发 playback_resume、位置 0.905，实得 %v", evs)
	}
	if got := r.lineEvents(); len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("恢复后行号不该回退（应为 0、1），实得 %v", got)
	}
}

// TestReplaySampleBeforeClockPassesFloor 回拨之后、时钟走过下限之前到点的 position_sample：位置是时钟
// 读数。服务端按它外推连接补发与 HTTP 快照的位置，取下限会把缓存锚回回拨前的时间线。
//
// 变异自证（注入在回放里，实测）：position_sample 的位置取 lineAt，本例红（位置 1.055）。
func TestReplaySampleBeforeClockPassesFloor(t *testing.T) {
	r := rollbackAtBoundary([]float32{0.2, 1.5, 30.0})
	r.lastSample = msAt(1155).Add(-player.PositionSampleInterval) // 下一拍恰好到点
	r.tick(1150, 1155, 1, true)

	last := r.events[len(r.events)-1]
	if last.typ != player.EventPositionSample || last.ms != 1155 {
		t.Fatalf("1155ms 应发出 position_sample，实得 %v", r.events)
	}
	if !inRange(last.pos, 0.905, 0.905) {
		t.Errorf("position_sample 的位置 %.3f，应为时钟读数 0.905（不是行匹配下限 1.055）", last.pos)
	}
	// 它是内部事件：前端看不到，仍在 lyric_update 落下的锚点上。
	if !inRange(r.feAnchor, 0.805, 0.805) || r.feAt != msAt(1055) {
		t.Errorf("position_sample 不该动前端的锚点，实得 %.3f @ %v", r.feAnchor, r.feAt.Sub(syncT0))
	}
}

// TestReplaySeekComparesClockNotFloor seek 检测与 DOM 比的是时钟读数。回拨后下一拍界面进度退回 0：
// 时钟读数 0.905，与 DOM 相差不到回跳门槛 1.0s，不判 seek；拿行匹配下限 1.055 去比就超过门槛了——
// 那是把对表的回拨量算成了偏差。
//
// 变异自证（注入在回放里，实测）：seek 检测拿 lineAt 与 DOM 比，本例红（误报一次回跳）。
func TestReplaySeekComparesClockNotFloor(t *testing.T) {
	r := rollbackAtBoundary([]float32{0.2, 1.5, 30.0})
	r.tick(1150, 1155, 0, true)

	if n := r.count("seek"); n != 0 {
		t.Errorf("时钟只比 DOM 超前 0.905s，不到回跳门槛，不该判 seek（事件：%v）", r.events)
	}
	if got := r.clock.at(msAt(1155)); !inRange(got, 0.905, 0.905) {
		t.Errorf("时钟读数 %.3f，应仍为 0.905", got)
	}
}

// TestReplaySeekBackAfterRollback 回拨之后、时钟走过下限之前回跳：seek 检测经 jump 置位，下限清掉，
// 行号跟着位置回到前面的行。
//
// 回拨后第二拍（got=1255）界面进度退回 0：时钟 1.005，超过回跳门槛，置为 0。下一拍行匹配时间
// 0.1 + 0.5 = 0.6，回到第 0 行。
//
// 变异自证（均实测）：注入在回放里——seek 置位改用 adjust（不清下限），本例红（行匹配仍被下限顶在
// 第 1 行，回跳后不推第 0 行）；注入在时钟——jump 不清下限，本例红。
func TestReplaySeekBackAfterRollback(t *testing.T) {
	r := rollbackAtBoundary([]float32{0.2, 1.5, 30.0})
	r.tick(1150, 1155, 1, true)
	r.tick(1250, 1255, 0, true)
	if evs := r.eventsAt(1255); len(evs) != 1 || evs[0].cause != "seek" || !inRange(evs[0].pos, 0, 0) {
		t.Fatalf("1255ms 应判回跳、位置 0，实得 %v", evs)
	}
	r.tick(1350, 1355, 0, true)
	if got := r.lineEvents(); len(got) != 3 || got[2] != 0 {
		t.Errorf("回跳之后行号应跟着位置回到第 0 行（0、1、0），实得 %v", got)
	}
}

// TestReplayNoSyncRightAfterSongChange 换歌后 500ms 之内不对表（沿用 seek 检测的抑制）：界面进度可能
// 还是上一首的。换歌后 240ms、340ms 两拍秒数由 41 跳到 42（上一首的进度），时钟才走到 0.3 上下。
//
// 变异自证（均实测）：注入在回放里——对表不看换歌的抑制（sameSong 恒传 true），本例红（时钟被拨到
// 41.7）；注入在 observe——usable 去掉 `sameSong`，本例红。
func TestReplayNoSyncRightAfterSongChange(t *testing.T) {
	r := newReplay(0)
	r.songAt = msAt(0)
	r.tick(240, 242, 41, true)
	r.tick(340, 342, 42, true)
	r.tick(440, 442, 42, true)
	if len(r.adjusts) != 0 || r.count("seek") != 0 {
		t.Errorf("换歌后 500ms 之内不该对表也不该判 seek，实得校准 %v、事件 %v", r.adjusts, r.events)
	}
	if got := r.clock.at(msAt(442)); !inRange(got, 0.442, 0.442) {
		t.Errorf("时钟读数 %.3f，应仍按墙钟走到 0.442", got)
	}
}

// TestReplaySeekTickIsNotSynced 秒数加 1 的那一拍同时超过前跳门槛（时钟落后 1.7s）：seek 检测在前，
// 把时钟置为整秒值并发 playback_resume；对表在后，判了 seek 的这一拍不对表。其后的跳秒把落后量夹回。
//
// p(ms) = 10.9s + ms，拍点 …40、140；p 在 100ms 过 11.000，140ms 那一拍看见（got=142）。
//
// 变异自证（均实测）：注入在回放里——对表挪到 seek 检测之前，本例红（这一拍没有判 seek，改由校准把
// 时钟拨到 10.70）；对表不看 seeked 标志（恒传 false），本例红（刚置成 11.00 的时钟被回拨到 10.802）。
// 注入在 observe——usable 去掉 `!seeked`，本例红。
func TestReplaySeekTickIsNotSynced(t *testing.T) {
	tl := timeline{p0: 10_900}
	r := newReplay(tl.p(0) - 1.7)
	tl.play(r, 40, 240, replayPoll, replayRTT)

	evs := r.eventsAt(142)
	if len(evs) != 1 || evs[0].cause != "seek" || !inRange(evs[0].pos, 11.0, 11.0) {
		t.Fatalf("142ms 应只发 seek 的 playback_resume、位置 11.00，实得 %v", evs)
	}
	if len(r.adjusts) != 0 {
		t.Fatalf("判了 seek 的那一拍不对表，实得 %v", r.adjusts)
	}

	// 置为整秒值之后落后 0.042s（时钟偏早）；下一个跳秒回拨到上界，落后量回到 [L−u, L+u]。
	tl.play(r, 240, 5000, replayPoll, replayRTT)
	if lag := tl.p(4990) - r.clock.at(msAt(4990)); !inRange(lag, domLagSec-replayU, domLagSec+replayU) {
		t.Errorf("其后落后 %.3fs，不在 L±u 之内（校准：%v）", lag, r.adjusts)
	}
	if n := r.count("seek"); n != 1 {
		t.Errorf("seek 应只判一次，实得 %d 次", n)
	}
}

// TestReplayAttachPausedThenPlay 启动时附着、歌暂停，之后开播（实测一例：前跳 0.00 → 16.00，1.06s 后
// 回跳 17.06 → 16.00）：两次 seek 都是现有行为，其后的第一个可信跳秒把落后量夹回 [L, L+u]。
//
// 时间线：时钟为 0、不在走；界面进度停在 16.5。0ms 读到在播，界面进度到 800ms 才起走（起播延迟 0.8s），
// 在 1300ms 过 17.000。
//
// 变异自证（均实测）：注入在回放里——同拍判了 seek 仍发恢复的 playback_resume，本例红；注入在 untold——
// due 恒返回 false，本例红（前端停在回跳落下的锚点上）。
func TestReplayAttachPausedThenPlay(t *testing.T) {
	pMs := func(ms int) int { return 16_500 + max(0, ms-800) }
	r := newReplay(0)
	r.clock = baseRealClock{}
	r.lastPlaying, r.fePlaying = false, false
	for ms := 40; ms < 6000; ms += replayPoll {
		r.tick(ms, ms+replayRTT, pMs(ms)/1000, true)
	}

	var seeks []replayEvent
	for _, e := range r.events {
		if e.cause == "seek" {
			seeks = append(seeks, e)
		}
	}
	if len(seeks) != 2 || !inRange(seeks[0].pos, 16, 16) || !inRange(seeks[1].pos, 16, 16) {
		t.Fatalf("应先前跳到 16.00、再回跳到 16.00，实得 %v", seeks)
	}
	if n := r.count("state"); n != 0 {
		t.Errorf("开播那一拍判了 seek，不该再发恢复的 playback_resume（AGENTS.md §3.2），实得 %d 条", n)
	}
	lag := func(ms int) float32 { return float32(pMs(ms))/1000 - r.clock.at(msAt(ms)) }
	if got := lag(5990); !inRange(got, domLagSec, domLagSec+replayU) {
		t.Errorf("回跳之后的跳秒应把落后量夹回 [L, L+u]，实得 %.3fs（校准：%v）", got, r.adjusts)
	}
	if got := float32(pMs(5990))/1000 - r.frontPos(5990); !inRange(got, domLagSec, domLagSec+replayU) {
		t.Errorf("前端落后 %.3fs，不在 [L, L+u] 之内（事件：%v）", got, r.events)
	}
}
