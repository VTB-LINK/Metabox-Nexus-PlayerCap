package cloudmusic

// 本文件只测由校准触发的通知（playback_resume）在回放里何时发出、带什么位置，以及模拟前端据此落在
// 哪条时间线上：暂停恢复之后、分两步到位时的顺延、顺延期间被 lyric_update 清零、跳秒时刻不稳时的条数。
// 夹具与它的边界见 domsyncreplay_test.go 的文件头：回放不经 runSession 本身。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// syncEvents 返回回放里由校准触发的通知。
func (r *replay) syncEvents() (out []replayEvent) {
	for _, e := range r.events {
		if e.cause == "sync" {
			out = append(out, e)
		}
	}
	return out
}

// TestReplayResumeWithoutLyricUpdate 暂停后恢复、其后迟迟没有 lyric_update（无歌词的歌、长间奏）：
// 恢复后 1.2s 之内后端与前端的落后量都回到 [L, L+u]。
//
// 时间线：p(ms) = 29.6s + ms，落后 0.33s。5040ms 那一拍读到暂停，时钟冻结；界面进度再走 0.5s 才停
// （实测如此，其间在 5400ms 由 34 跳到 35）。8040ms 恢复，界面进度立即起走，此时落后 0.83s。p 在 8900ms
// 过 36.000，8940ms 那一拍看见。
//
// 变异自证（均实测）：due 恒返回 false（从不发通知），两例均红（前端停在落后 0.83s）；usable 去掉
// `playing`、observe 里不在播的拍不顶掉上一拍，两例均红（暂停期间的那次跳秒被拿来对表）。注入在回放里：
// 对表按在播恒真（不看时钟在不在走），两例均红。
func TestReplayResumeWithoutLyricUpdate(t *testing.T) {
	const p0, pauseAt, fade, resumeAt = 29_600, 5040, 500, 8040
	pMs := func(ms int) int {
		switch {
		case ms < pauseAt+fade:
			return p0 + ms
		case ms < resumeAt:
			return p0 + pauseAt + fade
		}
		return p0 + pauseAt + fade + (ms - resumeAt)
	}
	p := func(ms int) float32 { return float32(pMs(ms)) / 1000 }

	cases := []struct {
		name  string
		lines []float32
	}{
		{"无歌词的歌", nil},
		// 当前行自 20.0s 起，下一行在 60.0s：恢复后 20 多秒没有 lyric_update。
		{"长间奏", []float32{20.0, 60.0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newReplay(p(0) - 0.33)
			r.offset, r.lines = 0.5, c.lines
			for ms := 40; ms < 12_000; ms += replayPoll {
				r.tick(ms, ms+replayRTT, pMs(ms)/1000, ms < pauseAt || ms >= resumeAt)
				got := ms + replayRTT
				switch ms {
				case 5940: // 暂停中：时钟不跟界面进度的那 0.5s，也不对表
					if pos, want := r.clock.at(msAt(got)), p(pauseAt+replayRTT)-0.33; !inRange(pos, want, want) {
						t.Errorf("暂停中时钟读数 %.3f，应冻结在读到暂停那一拍的 %.3f", pos, want)
					}
				case 8840: // 恢复之后、第一个跳秒之前
					if lag := p(got) - r.frontPos(got); !inRange(lag, 0.82, 0.84) {
						t.Errorf("校准之前前端落后 %.3fs，应约为 0.83s", lag)
					}
				case resumeAt + 1200: // 恢复后 1.2s
					if lag := p(got) - r.clock.at(msAt(got)); !inRange(lag, domLagSec, domLagSec+replayU) {
						t.Errorf("恢复后 1.2s 后端落后 %.3fs，不在 [L, L+u] 之内", lag)
					}
					if lag := p(got) - r.frontPos(got); !inRange(lag, domLagSec, domLagSec+replayU) {
						t.Errorf("恢复后 1.2s 前端落后 %.3fs，不在 [L, L+u] 之内（事件：%v）", lag, r.events)
					}
				}
			}

			if adj := r.adjustsAfter(0); len(adj) != 1 || adj[0].ms != 8942 {
				t.Fatalf("应只在恢复之后的第一个跳秒（8942ms）校准一次，实得 %v", r.adjusts)
			}
			sync := r.syncEvents()
			if len(sync) != 1 {
				t.Fatalf("由校准触发的通知应为 1 条，实得 %v", sync)
			}
			// 通知的位置就是那一拍对表之后的时钟读数：p(8942) − (L + 看见跳秒时已过的 42ms)。
			if want := p(8942) - domLagSec - 0.042; sync[0].ms != 8942 || !inRange(sync[0].pos, want, want) {
				t.Errorf("通知应在 8942ms、位置 %.3f，实得 %+v", want, sync[0])
			}
			if r.count("state") != 2 || r.count("seek") != 0 {
				t.Errorf("应有暂停、恢复各一条且不判 seek，实得 %v", r.events)
			}
		})
	}
}

// TestReplayWideThenNarrowWindow u 由宽变窄（设计审查给出的时间线）：第一次校准恰逢 Extract 返回得晚，
// 区间宽，落后量只收到 L+u 以内；约 1 秒后的跳秒再校一次，这第二笔达到门槛却不足 5 秒，顺延到满 5 秒发出。
//
// 一直在播，p(ms) = 10.9s + ms，时钟落后 0.88s。上一拍 sent=0、got=5、秒数 10；本拍 sent=100 读到 11、
// got=320：u = 0.32，时钟 10.34 → 10.70，发通知，此后落后 0.52s。sent=1100 读到 12、got=1105：u = 0.105，
// 时钟 11.485 → 11.70，未告知量 0.215s，顺延。
//
// 变异自证（均实测）：顺延写成丢弃（间隔未到时清零）、每次检查都记时刻，本例红（第二条通知没有了，
// 前端一直落后 0.52s）；去掉限频，本例红（第二条在 1105ms 就发了）。注入在回放里：position_sample 也清
// 未告知量，本例红（5005ms 的采样把顺延的那一笔清掉了）。untold.add 取绝对值累加不影响本例（两笔同向）。
func TestReplayWideThenNarrowWindow(t *testing.T) {
	tl := timeline{p0: 10_900}
	r := newReplay(tl.p(0) - 0.88)
	r.tick(0, 5, tl.sec(0), true)
	r.tick(100, 320, tl.sec(100), true)
	tl.play(r, 400, 3000, replayPoll, 5)

	if adj := r.adjustsAfter(0); len(adj) != 2 || !inRange(adj[0].delta, 0.36, 0.36) || !inRange(adj[1].delta, 0.215, 0.215) ||
		adj[0].ms != 320 || adj[1].ms != 1105 {
		t.Fatalf("应在 320ms 向前 0.36s、1105ms 向前 0.215s，实得 %v", r.adjusts)
	}
	if lag := tl.p(3000) - r.clock.at(msAt(3000)); !inRange(lag, domLagSec, domLagSec+0.105) {
		t.Errorf("第二次校准之后后端落后 %.3fs，不在 [L, L+u] 之内", lag)
	}
	// 顺延期间：前端仍在第一条通知的时间线上，比后端晚 0.215s。
	if diff := r.clock.at(msAt(3000)) - r.frontPos(3000); !inRange(diff, 0.215, 0.215) {
		t.Errorf("顺延期间前端与后端相差 %.3fs，应为 0.215s", diff)
	}
	if sync := r.syncEvents(); len(sync) != 1 || sync[0].ms != 320 || !inRange(sync[0].pos, 10.70, 10.70) {
		t.Fatalf("3 秒之内应只有 320ms 的一条通知（位置 10.70），实得 %v", sync)
	}

	tl.play(r, 3000, 9000, replayPoll, 5)
	sync := r.syncEvents()
	if len(sync) != 2 {
		t.Fatalf("两条通知都应发出，实得 %v", sync)
	}
	// 距第一条满 5 秒之后的第一拍：got=5405（5305 − 320 还不足 5 秒）。位置是那一刻的时钟读数：
	// 1105ms 拨到 11.70，其后走了 4.3 秒。
	if sync[1].ms != 5405 || !inRange(sync[1].pos, 16.00, 16.00) {
		t.Errorf("第二条通知应在 5405ms、位置 16.00，实得 %+v", sync[1])
	}
	if lag := tl.p(9000) - r.frontPos(9000); !inRange(lag, domLagSec, domLagSec+0.105) {
		t.Errorf("第二条通知之后前端落后 %.3fs，不在 [L, L+u] 之内", lag)
	}
}

// TestReplayLyricUpdateDuringDeferral 顺延期间遇到 lyric_update：前端据它落到后端的时间线上，未告知量
// 清零，间隔到了也不再补发。时间线同上一例，另有一行歌词在约 3 秒处换行。
//
// 变异自证（均实测）：anchored 不清零，本例红（5405ms 多发一条通知）；去掉限频，本例红。
func TestReplayLyricUpdateDuringDeferral(t *testing.T) {
	tl := timeline{p0: 10_900}
	r := newReplay(tl.p(0) - 0.88)
	r.offset, r.lines = 0.5, []float32{5.0, 14.1} // 时钟走到 13.6 时换到第 1 行，约在 3000ms
	r.tick(0, 5, tl.sec(0), true)
	r.tick(100, 320, tl.sec(100), true)
	tl.play(r, 400, 9000, replayPoll, 5)

	var line1 *replayEvent
	for i, e := range r.events {
		if e.cause == "line" && e.line == 1 {
			line1 = &r.events[i]
		}
	}
	if line1 == nil || line1.ms < 1105 || line1.ms > 5305 {
		t.Fatalf("第 1 行的 lyric_update 应落在顺延期间，实得 %v", r.events)
	}
	if sync := r.syncEvents(); len(sync) != 1 || sync[0].ms != 320 {
		t.Errorf("顺延的那一笔已由 lyric_update 带到，不该补发；应只有 320ms 的一条通知，实得 %v", sync)
	}
	if lag := tl.p(9000) - r.frontPos(9000); !inRange(lag, domLagSec, domLagSec+0.105) {
		t.Errorf("前端落后 %.3fs，不在 [L, L+u] 之内", lag)
	}
}

// TestReplayUnstableFlipsRateLimited 跳秒时刻不稳（无歌词的歌，迟到 0.3s 与准时的跳秒交替 60 秒；窗口
// 不在前台时是否如此没有量过，设计 §2.5）：由校准触发的通知不超过 60s / 5s + 1 = 13 条。
//
// 轮询 100ms 时每个跳秒来回校 0.3 − u = 0.198s，恰在通知门槛之下，只有第一笔（初始落后 0.33s，回拨
// 0.21s）发通知；轮询 50ms 时每个跳秒来回校 0.248s，每一笔都达门槛，条数由限频约束。
//
// 变异自证（均实测）：去掉限频，50ms 一例红（通知 59 条）；每次检查都记时刻，50ms 一例红；untold 只判
// 向前，两例均红；untold.add 取绝对值累加，100ms 一例红。
func TestReplayUnstableFlipsRateLimited(t *testing.T) {
	late := func(k int) int {
		if k%2 == 1 {
			return 300
		}
		return 0
	}
	cases := []struct {
		name     string
		poll     int
		min, max int // 通知条数的范围
	}{
		{"轮询 100ms", 100, 1, 1},
		{"轮询 50ms", 50, 2, 13},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tl := timeline{p0: 10_000, jitter: late}
			r := newReplay(tl.p(0) - 0.33)
			tl.play(r, 40, 60_000, c.poll, replayRTT)

			if n := len(r.adjustsAfter(0)); n < 55 {
				t.Fatalf("每个跳秒都应校准一次（59 个跳秒），实得 %d 次：用例没有打到要测的情形", n)
			}
			sync := r.syncEvents()
			if len(sync) < c.min || len(sync) > c.max {
				t.Errorf("由校准触发的通知 %d 条，应在 %d～%d 条之间", len(sync), c.min, c.max)
			}
			for i := 1; i < len(sync); i++ {
				if gap := sync[i].ms - sync[i-1].ms; gap < int(player.PositionSampleInterval.Milliseconds()) {
					t.Errorf("第 %d、%d 条通知只隔 %dms，不足 5 秒", i, i+1, gap)
				}
			}
			if n := r.count("seek"); n != 0 {
				t.Errorf("不该判 seek，实得 %d 次", n)
			}
		})
	}
}
