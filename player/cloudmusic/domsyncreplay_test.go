package cloudmusic

// 本文件只测对表、未告知量与时钟三件合在一起的效果：在假时间线上按 runSession 一拍之内的顺序
// （起走 / 冻结 → 行匹配 → seek 检测 → 对表 → 本拍补发的歌词 → 状态变化 → 行号变化的 lyric_update →
// 通知检查 → 位置采样）复述一遍，再用一个只在带位置的对外事件上落锚的模拟前端看结果。
//
// 一拍里有两个时刻，同 runSession：与 DOM 读数成对的读写配 got；对外事件与位置采样的位置取发出那一刻的
// 时钟读数（posNow），模拟前端也按发出的时刻落锚。两者之差是本拍的阻塞时长（tickOpt.blockMs），没有
// 阻塞的拍上相同。
//
// **回放不经 runSession 本身**：轮询循环要真 CDP 连接，没有离线夹具，下面的 replay.tick 是照着它抄的。
// 它守得住三件合起来对不对，以及「按这个顺序、取这些读数」得到的结论；runSession 是否真的这样接线
// 守不住，那部分靠读代码与真机。注释里写「注入在回放里」的变异，改的是 replay.tick，不是 runSession。
// 带位置的事件是否都经 emitPos 发出另有 AST 门禁（player/cloudmusicanchor_lint_test.go）。
//
// 时间一律由用例给定，单位毫秒，自 syncT0 起算。p 是界面进度对应的连续位置；落后量 = p − 位置。

import (
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

func msAt(ms int) time.Time { return syncT0.Add(time.Duration(ms) * time.Millisecond) }

// replayEvent 是回放里发出的一条事件。
type replayEvent struct {
	ms    int    // 发出它的那一拍的 got
	at    int    // 发出的时刻：got 加上那一拍的阻塞时长
	typ   string // 事件类型
	cause string // seek / late（本拍补发的歌词）/ state / line / sync（由校准触发的通知）/ sample
	pos   float32
	line  int // lyric_update 的行号，其余为 -1
}

// replayFlip 是秒数变化的那一拍的读数「DOM 秒数 − 时钟」，与真机探针打印的是同一个量。
type replayFlip struct {
	ms            int
	before, after float32 // 对表前、对表后
}

type replayAdjust struct {
	ms    int
	delta float32
}

type replay struct {
	clock   baseRealClock
	dom     domSync
	pending untold

	offset      float32   // 歌词 offset（秒）
	lines       []float32 // 各行的显示起点（秒），升序；空即无歌词
	lastLine    int
	lastPlaying bool
	lastSec     int
	songAt      time.Time // 换歌的时刻
	lastSample  time.Time
	block       int // 当前这一拍读到 DOM 之后、发出事件之前被阻塞的时长（毫秒）

	events  []replayEvent
	flips   []replayFlip
	adjusts []replayAdjust

	// 模拟前端：只在带位置的对外事件上落锚，其间按墙钟推进；position_sample 不可见。
	feAnchor  float32
	feAt      time.Time
	fePlaying bool
}

// newReplay 起一段回放：0 时刻在播，时钟读数为 clockSec，前端与后端同一条线，距换歌已久。
func newReplay(clockSec float32) *replay {
	return &replay{
		clock:    baseRealClock{BaseRealTime: clockSec, AnchorTime: msAt(0), Playing: true},
		lastLine: -1, lastPlaying: true, lastSec: -1, songAt: msAt(-60_000),
		feAnchor: clockSec, feAt: msAt(0), fePlaying: true,
	}
}

// emit 记下一条事件。ms 是那一拍的 got；事件在 got 加上本拍阻塞时长的时刻发出，模拟前端按这个时刻落锚。
func (r *replay) emit(ms int, typ, cause string, pos float32, line int) {
	at := ms + r.block
	r.events = append(r.events, replayEvent{ms, at, typ, cause, pos, line})
	if typ == player.EventPositionSample {
		return // 内部事件：不广播，不清未告知量
	}
	r.pending.anchored()
	r.feAnchor, r.feAt, r.fePlaying = pos, msAt(at), typ != player.EventPlaybackPause
}

// posNow 对外事件与位置采样的位置：发出那一刻的时钟读数（runSession 的 posNow）。
func (r *replay) posNow(gotMs int) float32 { return r.clock.at(msAt(gotMs + r.block)) }

// tickOpt 是一拍里不常有的两样，零值即没有。
type tickOpt struct {
	// blockMs 是读到 DOM 之后、发出事件之前被阻塞的时长（毫秒）：runSession 在「Redux lyrics appeared
	// later」分支里读翻译歌词的那次同步 CDP 调用，排在本拍所有对外事件之前。
	blockMs int
	// late 为真即本拍补发歌词（「Redux lyrics appeared later」或超时兜底）。lines 是补到的各行显示起点，
	// 就地生效、本拍的行匹配就用；为空即只有提示语或超时兜底，all_lyrics 之后另发 index:-1 的
	// lyric_update。
	late  bool
	lines []float32
}

// tick 复述 runSession 的一拍，各步的先后与它相同。sec 是这一拍读到的 DomTimeSec，playing 是读到的
// 播放状态。
func (r *replay) tick(sentMs, gotMs, sec int, playing bool) {
	r.tickWith(sentMs, gotMs, sec, playing, tickOpt{})
}

// tickWith 同 tick，另带这一拍的阻塞与补发歌词。
func (r *replay) tickWith(sentMs, gotMs, sec int, playing bool, o tickOpt) {
	now := msAt(gotMs)
	settled := now.Sub(r.songAt) > 500*time.Millisecond
	seeked := false
	r.block = o.blockMs
	if o.late {
		r.lines = o.lines // 状态就地更新；事件推迟到对表之后
	}

	r.clock.setPlaying(playing, now)
	lineIdx := r.matchLine(now)
	seeked = r.detectSeek(gotMs, sec, settled)
	r.syncClock(sentMs, gotMs, sec, settled, seeked)
	if o.late {
		r.lateLyrics(gotMs)
	}
	r.stateChange(gotMs, playing, seeked)
	r.lineChange(gotMs, playing, lineIdx)
	r.notice(gotMs, playing)
	r.sample(gotMs, playing)
}

// matchLine 行匹配：取 lineAt。
func (r *replay) matchLine(now time.Time) int {
	currentTime := r.clock.lineAt(now) + r.offset
	for i := len(r.lines) - 1; i >= 0; i-- {
		if currentTime >= r.lines[i] {
			return i
		}
	}
	return -1
}

// detectSeek seek 检测：与 DOM 比的是 got 时刻的时钟读数，经 jump 置位。
func (r *replay) detectSeek(gotMs, sec int, settled bool) (seeked bool) {
	now := msAt(gotMs)
	if sec >= 0 && r.clock.Playing && settled {
		diff := float32(sec) - r.clock.at(now)
		threshold := float32(1.5)
		if diff < 0 {
			threshold = 1.0
		}
		if diff > threshold || -diff > threshold {
			r.clock.jump(float32(sec), now)
			r.emit(gotMs, player.EventPlaybackResume, "seek", r.posNow(gotMs), -1)
			seeked = true
		}
	}
	return seeked
}

// syncClock 对表。
func (r *replay) syncClock(sentMs, gotMs, sec int, settled, seeked bool) {
	sent, now := msAt(sentMs), msAt(gotMs)
	before := r.clock.at(now)
	synced := r.dom.observe(domSample{
		sent: sent, got: now, sec: sec, playing: r.clock.Playing, sameSong: settled, seeked: seeked,
	}, before)
	if d := synced - before; d != 0 {
		r.clock.adjust(synced, now)
		r.pending.add(d)
		r.adjusts = append(r.adjusts, replayAdjust{gotMs, d})
	}
	if sec != r.lastSec && r.lastSec >= 0 {
		r.flips = append(r.flips, replayFlip{gotMs, float32(sec) - before, float32(sec) - r.clock.at(now)})
	}
	r.lastSec = sec
}

// lateLyrics 本拍补发的歌词：all_lyrics，没有歌词行时另发 index:-1 的 lyric_update。排在对表之后，
// 位置是校准后的读数。
func (r *replay) lateLyrics(gotMs int) {
	pos := r.posNow(gotMs)
	r.emit(gotMs, player.EventAllLyrics, "late", pos, -1)
	if len(r.lines) == 0 {
		r.emit(gotMs, player.EventLyricUpdate, "late", pos, -1)
	}
}

// stateChange 状态变化：playback_pause / playback_resume；同拍判了 seek 的不重复发 playback_resume。
func (r *replay) stateChange(gotMs int, playing, seeked bool) {
	if playing == r.lastPlaying {
		return
	}
	r.lastPlaying = playing
	if !playing {
		r.emit(gotMs, player.EventPlaybackPause, "state", r.posNow(gotMs), -1)
	} else if !seeked {
		r.emit(gotMs, player.EventPlaybackResume, "state", r.posNow(gotMs), -1)
	}
}

// lineChange 行号变化的 lyric_update。
func (r *replay) lineChange(gotMs int, playing bool, lineIdx int) {
	if playing && lineIdx >= 0 && lineIdx != r.lastLine {
		r.lastLine = lineIdx
		r.emit(gotMs, player.EventLyricUpdate, "line", r.posNow(gotMs), lineIdx)
	}
}

// notice 通知检查：由校准触发的 playback_resume。限频按 got 计，位置取发出那一刻的读数。去掉 `playing`
// 是等价变异（实测不红）：暂停那一拍的 playback_pause 已把未告知量清零，暂停中又不对表。
func (r *replay) notice(gotMs int, playing bool) {
	if playing && r.pending.due(msAt(gotMs)) {
		r.emit(gotMs, player.EventPlaybackResume, "sync", r.posNow(gotMs), -1)
	}
}

// sample 位置采样（内部事件）：时刻与位置成对取在发出那一刻。
func (r *replay) sample(gotMs int, playing bool) {
	if t := msAt(gotMs + r.block); playing && t.Sub(r.lastSample) >= player.PositionSampleInterval {
		r.lastSample = t
		r.emit(gotMs, player.EventPositionSample, "sample", r.clock.at(t), -1)
	}
}

// frontPos 返回模拟前端在 ms 时刻认为的位置。
func (r *replay) frontPos(ms int) float32 {
	return player.LivePosition(r.feAnchor, r.feAt, msAt(ms), r.fePlaying, 0)
}

// count 数出某个起因的事件条数。
func (r *replay) count(cause string) int {
	n := 0
	for _, e := range r.events {
		if e.cause == cause {
			n++
		}
	}
	return n
}

// totalAdjust 返回各次校准改动量的代数和与绝对值之和。
func (r *replay) totalAdjust() (sum, abs float32) {
	for _, a := range r.adjusts {
		sum += a.delta
		abs += max(a.delta, -a.delta)
	}
	return sum, abs
}

// adjustsAfter 返回 ms 之后的各次校准。改动量不到 lagTol 的不算：时钟恰在区间边界上时，float32 的舍入
// 会让它与边界相差百万分之几秒，夹取随之改动同样大小的量。
func (r *replay) adjustsAfter(ms int) (out []replayAdjust) {
	for _, a := range r.adjusts {
		if a.ms > ms && max(a.delta, -a.delta) > lagTol {
			out = append(out, a)
		}
	}
	return out
}

// timeline 是一段在播的假时间线：界面进度 p(ms) = p0 + ms（毫秒）；秒数在 p 过整秒 k 之后 jitter(k) 毫秒
// 才跳到 k（jitter 为 nil 即准时）。页面在每拍的 sent 读 DOM。
type timeline struct {
	p0     int
	jitter func(k int) int
}

func (tl timeline) p(ms int) float32 { return float32(tl.p0+ms) / 1000 }

func (tl timeline) flipAt(k int) int {
	at := k*1000 - tl.p0
	if tl.jitter != nil {
		at += tl.jitter(k)
	}
	return at
}

func (tl timeline) sec(ms int) int {
	k := (tl.p0+ms)/1000 + 1
	for tl.flipAt(k) > ms {
		k--
	}
	return k
}

// play 从 from 到 to（毫秒，不含 to）每 poll 毫秒一拍地回放这段时间线，往返耗时 rtt 毫秒。
func (tl timeline) play(r *replay, from, to, poll, rtt int) {
	for ms := from; ms < to; ms += poll {
		r.tick(ms, ms+rtt, tl.sec(ms), true)
	}
}

const (
	replayPoll = 100 // 默认轮询间隔（毫秒）
	replayRTT  = 2   // Extract 往返耗时（毫秒）
	// replayU 是默认轮询下的区间宽度 u = w = 轮询间隔 + 往返耗时（秒）。
	replayU = float32(replayPoll+replayRTT) / 1000
	lagTol  = 0.002 // 落后量与读数的比较容差（秒）
)

// inRange 报告 v 是否在 [lo, hi] 之内（各放宽 lagTol）。
func inRange(v, lo, hi float32) bool { return v >= lo-lagTol && v <= hi+lagTol }

// TestReplayMeasuredSongs 把 2026-10-09 两次真机实测的各首歌（设计 §4.1）代进去：校准前的读数取自实测，
// 断言校准量、校准后的读数与事件。读数 = 秒数变化那一拍的「DOM 秒数 − 时钟」。默认轮询下的规则是：
// 读数大于 300ms 时向前拨到 300ms，小于 300ms − u 时回拨到 300ms − u，其间不动。
//
// 相位的建模：只有一档读数的歌，跳秒落在两拍中间（看见时已过 42ms）；两档的歌，跳秒贴着拍点，
// 奇数秒在拍点前 1ms（看见时已过 3ms，读数较大）、偶数秒在拍点后 1ms（看见时已过 101ms），两档相差
// 98ms（实测为 79～101ms）。
func TestReplayMeasuredSongs(t *testing.T) {
	twoPhase := func(k int) int {
		if k%2 == 0 {
			return 2
		}
		return 0
	}
	cases := []struct {
		name      string
		readingMs int  // 实测的校准前读数（两档的取较大一档）
		twoPhase  bool // 实测出现两档读数
		wantMs    int  // 期望的校准量合计（向前为正）
		wantSync  int  // 期望由校准触发的通知条数
	}{
		// 常态、读数不超过 300 的 7 首（260 有两首）：不动。
		{"常态 297", 297, false, 0, 0},
		{"常态 260", 260, false, 0, 0},
		{"常态 256", 256, false, 0, 0},
		{"常态 286", 286, false, 0, 0},
		{"常态 248", 248, false, 0, 0},
		{"常态 232", 232, false, 0, 0},
		// 常态、只有一档且超过 300 的 2 首，与分支 696ms 的一首（316～331）：首个跳秒向前拨到 300。
		{"常态 356", 356, false, 56, 0},
		{"常态 316", 316, false, 16, 0},
		{"分支偏慢 331", 331, false, 31, 0},
		// 常态、两档的 4 首：合计向前 99、9、43、59ms，不到通知门槛。
		{"两档 320/399", 399, true, 99, 0},
		{"两档 210/309", 309, true, 9, 0},
		{"两档 243/343", 343, true, 43, 0},
		{"两档 258/359", 359, true, 59, 0},
		// 暂停着换歌、起播延迟 451ms 的一首（99～115）：回拨到 300 − u，约 0.09s，不到门槛。
		{"起播延迟长 115", 115, false, 115 - (300 - replayPoll - replayRTT), 0},
		{"起播延迟长 99", 99, false, 99 - (300 - replayPoll - replayRTT), 0},
		// 暂停后恢复的 3 首：首个跳秒向前 0.43～0.48s，各发一条通知；遇另一档再向前约 0.1s，不到门槛。
		{"暂停恢复 742/842", 842, true, 542, 1},
		{"暂停恢复 729/821", 821, true, 521, 1},
		{"暂停恢复 776/875", 875, true, 575, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// p 在 1000ms 过 11.000。单档：拍点在 …940、1040，看见跳秒时已过 40 + 往返 2 = 42ms；
			// 两档：拍点在 …1001、1101，奇数秒看见时已过 1 + 2 = 3ms，偶数秒晚 2ms 才跳，下一拍才看见。
			tl, seen, first := timeline{p0: 10_000}, 42, 40
			if c.twoPhase {
				tl.jitter, seen, first = twoPhase, 3, 1
			}
			lag0 := float32(c.readingMs+seen) / 1000 // 读数 = 落后量 − 跳秒到 got 的间隔
			r := newReplay(tl.p(0) - lag0)
			tl.play(r, first, 40_000, replayPoll, replayRTT)

			sum, abs := r.totalAdjust()
			if want := float32(c.wantMs) / 1000; !inRange(sum, want, want) {
				t.Errorf("校准量合计 %.3fs，期望 %.3fs（各次：%v）", sum, want, r.adjusts)
			}
			if !inRange(abs, max(sum, -sum), max(sum, -sum)) {
				t.Errorf("校准有来回：代数和 %.3fs，绝对值之和 %.3fs（%v）", sum, abs, r.adjusts)
			}
			if late := r.adjustsAfter(3000); len(late) > 0 {
				t.Errorf("两档相位各见过一次之后不该再动，实得 %v", late)
			}
			for _, f := range r.flips {
				if f.ms > 3000 && !inRange(f.after, domLagSec-replayU, domLagSec) {
					t.Errorf("%dms 处的读数 %.3fs 不在 [%.3f, %.3f] 之内", f.ms, f.after, domLagSec-replayU, domLagSec)
				}
			}
			if got := r.count("sync"); got != c.wantSync {
				t.Errorf("由校准触发的通知 %d 条，期望 %d 条（事件：%v）", got, c.wantSync, r.events)
			}
			if n := r.count("seek"); n != 0 {
				t.Errorf("不该判 seek，实得 %d 次", n)
			}
			// 校准后的落后量：后端在 [L−u, L+u] 之内，从落后一侧夹回的在 [L, L+u]；前端与后端相差不到门槛。
			// 读时钟一律取不早于最后一拍的时刻，与 runSession 的用法一致（下同）。
			end := 40_000
			back := tl.p(end) - r.clock.at(msAt(end))
			lo := float32(domLagSec)
			if c.wantMs <= 0 {
				lo = domLagSec - replayU
			}
			if !inRange(back, lo, domLagSec+replayU) {
				t.Errorf("校准后后端落后 %.3fs，不在 [%.3f, %.3f] 之内", back, lo, domLagSec+replayU)
			}
			if diff := r.frontPos(end) - r.clock.at(msAt(end)); diff >= syncNotifySec || diff <= -syncNotifySec {
				t.Errorf("前端与后端相差 %.3fs，应不到 %.1fs", diff, syncNotifySec)
			}
		})
	}
}

// TestReplayAlternatingPhaseConverges 跳秒贴着拍点、相位在约 0 与约 95ms 之间交替 120 秒（实测有 4 首歌
// 如此）：总调整量不超过初始的 |落后量 − L|，收敛后不再调整，不发通知。
//
// 变异自证（注入在 observe）：夹到区间中点，本例红（每个跳秒来回校约 0.05s）；每跳落锚到下界，本例红
// （均实测）。
func TestReplayAlternatingPhaseConverges(t *testing.T) {
	// 拍点在 …1001、1101。奇数秒准时跳，看见时已过 3ms；偶数秒晚 6ms 才跳，下一拍才看见，已过 97ms。
	tl := timeline{p0: 10_000, jitter: func(k int) int {
		if k%2 == 0 {
			return 6
		}
		return 0
	}}
	for _, lag0 := range []float32{0.30, 0.36, 0.45, 0.25, 0.21} {
		r := newReplay(tl.p(0) - lag0)
		tl.play(r, 1, 120_000, replayPoll, replayRTT)

		_, abs := r.totalAdjust()
		if limit := max(lag0-domLagSec, domLagSec-lag0); abs > limit+lagTol {
			t.Errorf("初始落后 %.2fs：总调整量 %.3fs，超过初始偏差 %.3fs（%v）", lag0, abs, limit, r.adjusts)
		}
		if late := r.adjustsAfter(3000); len(late) > 0 {
			t.Errorf("初始落后 %.2fs：两档相位各见过一次之后不该再动，实得 %v", lag0, late)
		}
		if n := r.count("sync"); n != 0 {
			t.Errorf("初始落后 %.2fs：不该发通知，实得 %d 条", lag0, n)
		}
		if back := tl.p(120_000) - r.clock.at(msAt(120_000)); !inRange(back, domLagSec-replayU, domLagSec+replayU) {
			t.Errorf("初始落后 %.2fs：收敛后落后 %.3fs，不在 L±u 之内", lag0, back)
		}
	}
}

// TestReplayPollIntervals 轮询间隔配大之后区间随之变宽，校准自动变弱（设计 §2.4）：250ms 时夹回
// L±0.25s；1000ms 时每拍秒数加 1，u 约 1s，只纠正落在区间之外的偏差；2000ms 时每拍加 2，没有跳秒，
// 不校准。各例断言校准后的落后量在 [L+δ−u, L+δ] 之内（δ 为看见跳秒时已过的时间），且不比初始更远离 L。
//
// 变异自证（注入在 observe）：允许加 2，2000ms 的两例红（实测）。
func TestReplayPollIntervals(t *testing.T) {
	cases := []struct {
		name     string
		poll     int     // 轮询间隔（毫秒）
		first    int     // 第一拍的时刻；p 在 1000、2000… 过整秒
		lag0     float32 // 初始落后量
		wantLag  float32 // 期望的校准后落后量
		wantSync int
	}{
		// 拍点 …790、1040：看见时已过 42ms，读数 0.808 → 0.300。
		{"250ms：落后 0.85s，夹到 L+δ", 250, 40, 0.85, 0.342, 1},
		// 拍点 …990、1240：看见时已过 242ms，读数 0.608 → 0.300。
		{"250ms：跳秒刚过拍点，夹到 L+δ", 250, 240, 0.85, 0.542, 1},
		// 读数 0.158，在 [0.3−0.252, 0.3] 之内。
		{"250ms：落后 0.20s，在区间内不动", 250, 40, 0.20, 0.20, 0},
		// 看见时已过 102ms，读数 0.748 → 0.300。
		{"1000ms：跳秒后 0.1s 的拍，夹到 L+δ", 1000, 100, 0.85, 0.402, 1},
		// 看见时已过 902ms，读数 −0.052，在 [0.3−1.002, 0.3] 之内：区间盖不住这个偏差，与改动前相同。
		{"1000ms：跳秒后 0.9s 的拍，不动", 1000, 900, 0.85, 0.85, 0},
		{"1000ms：落后 1.45s（不到前跳门槛），夹到 L+δ", 1000, 100, 1.45, 0.402, 1},
		{"2000ms：每拍加 2，不校准", 2000, 100, 0.85, 0.85, 0},
		{"2000ms：换一个相位，同样不校准", 2000, 1100, 0.85, 0.85, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tl := timeline{p0: 10_000}
			r := newReplay(tl.p(0) - c.lag0)
			tl.play(r, c.first, 30_000, c.poll, replayRTT)

			back := tl.p(30_000) - r.clock.at(msAt(30_000))
			if !inRange(back, c.wantLag, c.wantLag) {
				t.Errorf("校准后落后 %.3fs，期望 %.3fs（校准：%v）", back, c.wantLag, r.adjusts)
			}
			if n := len(r.adjustsAfter(0)); n > 1 {
				t.Errorf("相位不变时至多校准一次，实得 %v", r.adjusts)
			}
			if got := r.count("sync"); got != c.wantSync {
				t.Errorf("由校准触发的通知 %d 条，期望 %d 条", got, c.wantSync)
			}
			if n := r.count("seek"); n != 0 {
				t.Errorf("不该判 seek，实得 %d 次", n)
			}
		})
	}
}
