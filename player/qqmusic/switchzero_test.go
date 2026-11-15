package qqmusic

// 本文件只测换歌前进度归零的暂扣（#202，doc/design/qqmusic-switch-zero-design.md）：判定函数 startsHold 与
// 暂扣状态 switchHold，经 sessionreplay_test.go 的复述回放设计 §7.3 的各组情形（S 组：暂扣与放行；F 组：补取
// 的重读；C 组：认领的重读），并以源码扫描钉住 runSession 里的接线形状（§7.4）。
//
// 变异在一份分离的工作区副本里逐条注入、跑完即还原，结果记在各用例的注释里（均实测）。「复述里」指只改
// sessionreplay_test.go 的复述（runSession 里对应的写法由文件末的源码扫描守形状）；未注明的改的是 qqmusic.go。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"strings"
	"testing"
	"time"
)

// tick 是回放的拍长：录音推断的约 31ms（设计 §1.2）。
const tick = 31

func zeroSongs() map[string]replaySong {
	return map[string]replaySong{
		"A": {durMs: 192000, lyrics: coastLyrics()},    // 第 0 行 0s、第 1 行 4.8s，每 4.8s 一行
		"B": {durMs: 281000, lyrics: utaLyrics()},      // 第 0 行 0s、第 1 行 6.87s
		"C": {durMs: 195000, lyrics: zhijiangLyrics()}, // 第 0 行 0s
		"N": {durMs: 200000},                           // 无歌词
		"P": {durMs: 192000, pending: true},            // 认领时歌词未到，由登记的补取送来
	}
}

// seg 是读数序列的一段：自 from 起（含）歌名为 name，读数为 base；moving 为真时随时间每毫秒前进 1。
type seg struct {
	from   int
	name   string
	base   uint32
	moving bool
}

func segs(ss ...seg) func(int) (string, uint32) {
	return func(ms int) (string, uint32) {
		s := ss[0]
		for _, x := range ss {
			if x.from <= ms {
				s = x
			}
		}
		if s.moving {
			return s.name, s.base + uint32(ms-s.from)
		}
		return s.name, s.base
	}
}

// playTo 是从会话开始就随时间前进、在 T 的前一拍（T − tick）读到 at 的一段。
func playTo(t *testing.T, name string, at uint32, T int) seg {
	t.Helper()
	if int(at) < T-tick {
		t.Fatalf("playTo(%q, %d, %d)：会话开始时读数为负，换一个更小的 T", name, at, T)
	}
	return seg{0, name, at - uint32(T-tick), true}
}

// firstTick 是不早于 ms 的第一拍。
func firstTick(ms int) int { return (ms + tick - 1) / tick * tick }

// evFrom 返回不早于 ms 那一拍发出的事件。
func evFrom(r *replay, ms int) []replayEvent {
	var out []replayEvent
	for _, e := range r.evs {
		if e.ms >= ms {
			out = append(out, e)
		}
	}
	return out
}

func expectFrom(t *testing.T, r *replay, ms int, want string) []replayEvent {
	t.Helper()
	evs := evFrom(r, ms)
	if got := shapeOf(evs); got != want {
		t.Fatalf("%dms 起的事件序列\n  得到 %q\n  期望 %q\n明细：%v", ms, got, want, evs)
	}
	return evs
}

// expectAt 核对一个事件的类型、歌、拍与位置；pos 为负时不查位置。
func expectAt(t *testing.T, e replayEvent, typ, song string, ms int, pos float32) {
	t.Helper()
	if e.typ != typ || e.song != song || e.ms != ms || (pos >= 0 && !approx(e.pos, pos)) {
		t.Errorf("事件 %v，期望 %s[%s] 在 %dms、位置 %.3f", e, typ, song, ms, pos)
	}
}

// ---------------------------------------------------------------- 单元

// TestStartsHold 是暂扣的触发判据（§7.2 U1）：startsHold 本身与 switchHold.step 的首次调用各跑一遍。
//
// 变异自证（均实测）：
//   - step 的触发恒假（不暂扣）：应暂扣的四例在 step 一侧红。
//   - 带宽改为无穷大：「带宽边界」「带外回跳」「带外回跳（中段）」三例红。
//   - 带宽比较改为含等号：「带宽边界」红。
//   - 触发不要求构成回跳（ms < prevMs 即可）：「恰好不构成回跳」两例、「回退不足 2s」两例红。
//   - seekBack 改为 `ms+2000 <= prevMs`：「恰好不构成回跳」两例红。
func TestStartsHold(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name     string
		prev, ms uint32
		want     bool
	}{
		{"录音的归零", 80000, 0, true},
		{"带内上沿", 80000, 999, true},
		{"带宽边界（严格小于）", 80000, 1000, false},
		{"带外回跳", 80000, 1500, false},
		{"带外回跳（中段）", 80000, 30000, false},
		{"恰好构成回跳", 2001, 0, true},
		{"恰好构成回跳、带内上沿", 3000, 999, true},
		{"恰好不构成回跳（同 :398）", 2000, 0, false},
		{"恰好不构成回跳、带内上沿", 2999, 999, false},
		{"回退不足 2s", 1500, 200, false},
		{"回退不足 2s、落到 0", 500, 0, false},
		{"无变化", 0, 0, false},
		{"前进", 1000, 1500, false},
		{"前进（中段）", 80000, 120000, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := startsHold(c.prev, c.ms); got != c.want {
				t.Errorf("startsHold(%d, %d) = %v，期望 %v", c.prev, c.ms, got, c.want)
			}
			var h switchHold
			held, released := h.step(c.prev, c.ms, t0)
			if held != c.want || released {
				t.Errorf("首次 step(%d, %d) = (%v, %v)，期望 (%v, false)", c.prev, c.ms, held, released, c.want)
			}
			if h.since.IsZero() == c.want {
				t.Errorf("首次 step(%d, %d) 之后暂扣起点 %v，期望%s", c.prev, c.ms, h.since, map[bool]string{true: "已记下", false: "为空"}[c.want])
			}
		})
	}
}

// TestSwitchHoldTiming 是暂扣的时长与状态（§7.2 U2）。
//
// 变异自证（均实测）：期满比较改为严格大于（`<=`）、放行后不清空暂扣、drop 不清空、暂扣时长改为 300ms、step 的
// 触发恒假，各自红（分别在「恰为 400ms 放行」「放行后同一读数不再暂扣」「drop 之后」「399ms 仍暂扣」「开始暂扣」处）。
func TestSwitchHoldTiming(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	step := func(h *switchHold, prev, ms uint32, now int, wantHeld, wantReleased bool, what string) {
		t.Helper()
		held, released := h.step(prev, ms, at(now))
		if held != wantHeld || released != wantReleased {
			t.Errorf("%s：step(%d, %d, +%dms) = (%v, %v)，期望 (%v, %v)", what, prev, ms, now, held, released, wantHeld, wantReleased)
		}
	}

	var h switchHold
	step(&h, 80000, 0, 0, true, false, "开始暂扣")
	step(&h, 80000, 50000, 200, true, false, "暂扣中不再看读数")
	step(&h, 80000, 0, 399, true, false, "399ms 仍暂扣")
	step(&h, 80000, 0, 400, false, true, "恰为 400ms 放行")
	step(&h, 0, 0, 431, false, false, "放行后同一读数不再暂扣")

	// drop 之后再遇回跳，从新的那一拍重新计时。
	step(&h, 80000, 0, 1000, true, false, "再次开始暂扣")
	h.drop(at(1100))
	if !h.since.IsZero() {
		t.Fatalf("drop 之后暂扣起点 %v，期望为空", h.since)
	}
	step(&h, 80000, 0, 1150, true, false, "drop 之后重新计时")
	step(&h, 80000, 0, 1549, true, false, "drop 之后重新计时：399ms 仍暂扣")
	step(&h, 80000, 0, 1550, false, true, "drop 之后重新计时：400ms 放行")

	// 没有暂扣时 drop 不做任何事。
	var h2 switchHold
	h2.drop(at(0))
	step(&h2, 80000, 999, 0, true, false, "空 drop 之后照常开始暂扣")
}

// ---------------------------------------------------------------- 回放：暂扣与放行（S 组）

// TestReplaySwitchZeroAbsorbed：录音里的 8 次归零（§7.3 S1）。上一首在 80s，T 读数归零、歌名未变，间隔之后歌名
// 换成 B（读数 0）。期望：从 T 到认领 B 之间上一首没有任何事件，B 在间隔之后的第一拍认领、第 0 行同拍。上一首
// 取有歌词的 A 与无歌词的 N 各一组。
//
// 变异自证（均实测），以下各条都使 14 个子例全红：step 的触发恒假（不暂扣：上一首在 T 发 resume(0)，有歌词时另有
// 第 0 行）；drop 不清空（认领之后仍在暂扣，B 的第 0 行推到放行拍）；复述里暂扣判定挪到换歌分支之前（B 的认领推到
// 放行拍）；复述里认领不调 drop。
func TestReplaySwitchZeroAbsorbed(t *testing.T) {
	const T = 3100
	for _, old := range []string{"A", "N"} {
		for _, gap := range []int{31, 51, 60, 61, 62, 105, 249} {
			t.Run(fmt.Sprintf("%s 间隔 %dms", old, gap), func(t *testing.T) {
				r := newReplay(-0.4, zeroSongs())
				r.run(0, T+gap+600, tick, segs(playTo(t, old, 80000, T), seg{T, old, 0, false}, seg{T + gap, "B", 0, false}))
				claim := firstTick(T + gap)
				evs := expectFrom(t, r, T, "all lu0")
				expectAt(t, evs[0], "all_lyrics", "B", claim, 0)
				expectAt(t, evs[1], "lyric_update", "B", claim, -1)
			})
		}
	}
}

// TestReplaySwitchZeroNameAtReleaseTick：歌名恰在放行那一拍才换（§7.3 S2，拍长 50ms，T 在拍上）。换歌分支在暂扣
// 判定之前，认领先发生、暂扣丢弃；对照：歌名晚一拍，放行拍补发上一首的回跳。
//
// 变异自证（均实测）：期满比较改为严格大于（`<=`），对照例红（放行推到 T + 450，与认领同拍，回跳不再发出）；
// 暂扣时长改为 300ms，红（T + 300 即放行补发）；step 的触发恒假，红。
func TestReplaySwitchZeroNameAtReleaseTick(t *testing.T) {
	const T = 5000
	base := seg{0, "A", 80000 - (T - 50), true}

	r := newReplay(-0.4, zeroSongs())
	r.run(0, T+800, 50, segs(base, seg{T, "A", 0, false}, seg{T + 400, "B", 0, false}))
	evs := expectFrom(t, r, T, "all lu0")
	expectAt(t, evs[0], "all_lyrics", "B", T+400, 0)

	r = newReplay(-0.4, zeroSongs())
	r.run(0, T+800, 50, segs(base, seg{T, "A", 0, false}, seg{T + 450, "B", 0, false}))
	evs = expectFrom(t, r, T, "resume lu0 all lu0")
	expectAt(t, evs[0], "playback_resume", "A", T+400, 0)
	expectAt(t, evs[1], "lyric_update", "A", T+400, -1)
	expectAt(t, evs[2], "all_lyrics", "B", T+450, 0)
}

// TestReplaySwitchZeroConfirmWindow：400ms 的覆盖（§7.3 S3）。间隔 390ms 时歌名在放行拍（T + 403）读到，认领；
// 410ms 时放行拍补发上一首的回跳，下一拍认领。
//
// 变异自证（均实测）：暂扣时长改为 300ms，两例都红（T + 310 即补发；390 一例即「400ms 覆盖到 390ms」这一条）；
// step 的触发恒假，两例都红。
func TestReplaySwitchZeroConfirmWindow(t *testing.T) {
	const T = 3100
	cases := []struct {
		gap  int
		want string
	}{{390, "all lu0"}, {410, "resume lu0 all lu0"}}
	for _, c := range cases {
		t.Run(fmt.Sprintf("间隔 %dms", c.gap), func(t *testing.T) {
			r := newReplay(-0.4, zeroSongs())
			r.run(0, T+1000, tick, segs(playTo(t, "A", 80000, T), seg{T, "A", 0, false}, seg{T + c.gap, "B", 0, false}))
			evs := expectFrom(t, r, T, c.want)
			if c.gap == 410 {
				expectAt(t, evs[0], "playback_resume", "A", T+403, 0)
				expectAt(t, evs[2], "all_lyrics", "B", T+434, 0)
			} else {
				expectAt(t, evs[0], "all_lyrics", "B", T+403, 0)
			}
		})
	}
}

// TestReplaySwitchZeroDragToStart：播放中拖回开头（§7.3 S4）。A 在 80s，T 拖到 0.3s，停 1200ms 后首步 +1068，
// 其后随时间走。期望：恰一条 resume(0.300)，在放行拍（T + 403），随后第 0 行；起步早于放行拍 + 1s，不判暂停。
// 现行在 T 发 resume，T + 1s 后另有一对暂停与恢复（#172）。
//
// 变异自证（均实测）：step 的触发恒假，本例红（resume 在 T，随后另有一对暂停与恢复）；暂扣时长改为 300ms，红；
// 复述里放行拍不复位行号，红（resume 之后没有第 0 行）；复述里暂扣的拍照样更新读数与锚点，红（放行拍读数等于
// 上一次读数，不发 resume）。
func TestReplaySwitchZeroDragToStart(t *testing.T) {
	const T = 3100
	r := newReplay(-0.4, zeroSongs())
	r.run(0, T+3000, tick, segs(playTo(t, "A", 80000, T), seg{T, "A", 300, false}, seg{T + 1200, "A", 1368, true}))
	evs := expectFrom(t, r, T, "resume lu0")
	expectAt(t, evs[0], "playback_resume", "A", T+403, 0.3)
	expectAt(t, evs[1], "lyric_update", "A", T+403, -1)
}

// TestReplaySwitchZeroLoopToStart：单曲循环回到开头（§7.3 S5，录音的形状：停 2072ms，首步 1648）。期望：放行拍
// resume(0) 与第 0 行；距放行拍超过 1s 的第一拍判暂停（#172 的误判照旧、晚约 0.4s）；首步那一拍判恢复。
//
// 变异自证（均实测）：step 的触发恒假，本例红（resume 在 T、pause 在 T + 1023）；暂扣时长改为 300ms、复述里放行拍
// 不复位行号、复述里暂扣的拍照样更新读数与锚点，各自红。
func TestReplaySwitchZeroLoopToStart(t *testing.T) {
	const T = 3100
	r := newReplay(-0.4, zeroSongs())
	r.run(0, T+2500, tick, segs(playTo(t, "A", 188000, T), seg{T, "A", 0, false}, seg{T + 2077, "A", 1648, true}))
	evs := expectFrom(t, r, T, "resume lu0 pause resume")
	expectAt(t, evs[0], "playback_resume", "A", T+403, 0)
	expectAt(t, evs[2], "playback_pause", "A", T+1426, 0)
	expectAt(t, evs[3], "playback_resume", "A", T+2077, 1.648)
}

// TestReplaySwitchZeroBand：带宽（§7.3 S6）。A 在 80s，T 拖到 X。1000、1500、30000 不暂扣，当拍发；999 在放行拍。
//
// 变异自证（均实测）：带宽改为无穷大，1000、1500、30000 三例红；带宽比较改为含等号，1000 一例红；step 的触发恒假、
// 暂扣时长改为 300ms，999 一例红。
func TestReplaySwitchZeroBand(t *testing.T) {
	const T = 3100
	cases := []struct {
		to   uint32
		want int
	}{{1000, T}, {999, T + 403}, {1500, T}, {30000, T}}
	for _, c := range cases {
		t.Run(fmt.Sprintf("拖到 %dms", c.to), func(t *testing.T) {
			r := newReplay(-0.4, zeroSongs())
			r.run(0, T+900, tick, segs(playTo(t, "A", 80000, T), seg{T, "A", c.to, false}))
			expectAt(t, r.find(t, "playback_resume", 0), "playback_resume", "A", c.want, float32(c.to)/1000)
		})
	}
}

// TestReplaySwitchZeroShortBack：同一首回退不足 2s 落在带内不暂扣、不发事件（同现行）；恰好构成回跳的照常暂扣
// （§7.3 S7）。
//
// 变异自证（均实测）：触发不要求构成回跳，前两例红（放行拍补发 resume）；seekBack 改为 `<=`，2000 一例红；step 的
// 触发恒假、暂扣时长改为 300ms，2001 一例红。
func TestReplaySwitchZeroShortBack(t *testing.T) {
	cases := []struct {
		name     string
		from, to uint32
		T        int
		want     string
	}{
		{"1500 → 200", 1500, 200, 1519, ""},
		{"2000 → 0", 2000, 0, 2015, ""},
		{"2001 → 0", 2001, 0, 2015, "resume lu0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newReplay(-0.4, zeroSongs())
			r.run(0, c.T+1000, tick, segs(playTo(t, "A", c.from, c.T), seg{c.T, "A", c.to, false}))
			evs := expectFrom(t, r, c.T, c.want)
			if c.want != "" {
				expectAt(t, evs[0], "playback_resume", "A", c.T+403, 0)
			}
		})
	}
}

// TestReplaySwitchZeroDragWhilePaused：暂停中拖回开头（§7.3 S8）。期望：放行拍 resume(0.300) 判恢复、第 0 行；
// 读数不动，距放行拍超过 1s 再判暂停。
//
// 变异自证（均实测）：step 的触发恒假，本例红（resume 在 T）；暂扣时长改为 300ms、复述里放行拍不复位行号，各自红。
func TestReplaySwitchZeroDragWhilePaused(t *testing.T) {
	const T = 2015
	r := newReplay(-0.4, zeroSongs())
	r.run(0, T+1500, tick, segs(seg{0, "A", 50000, false}, seg{T, "A", 300, false}))
	if e := r.find(t, "playback_pause", 0); e.ms >= T {
		t.Fatalf("拖动之前应已判暂停：%v", e)
	}
	evs := expectFrom(t, r, T, "resume lu0 pause")
	expectAt(t, evs[0], "playback_resume", "A", T+403, 0.3)
	expectAt(t, evs[2], "playback_pause", "A", T+1426, 0.3)
}

// TestReplaySwitchZeroWhilePaused：上一首已判暂停时换歌、读数先归零（§7.3 S9）。期望：上一首没有事件，认领 B。
//
// 变异自证（均实测）：step 的触发恒假，本例红（A 的 resume(0) 与第 0 行）；复述里暂扣判定挪到换歌分支之前，红。
func TestReplaySwitchZeroWhilePaused(t *testing.T) {
	const T = 2015
	r := newReplay(-0.4, zeroSongs())
	r.run(0, T+600, tick, segs(seg{0, "A", 50000, false}, seg{T, "A", 0, false}, seg{T + 61, "B", 0, false}))
	evs := expectFrom(t, r, T, "all lu0")
	expectAt(t, evs[0], "all_lyrics", "B", T+62, 0)
}

// TestReplaySwitchZeroReleaseRule：放行拍的判据（§7.3 S10，设计审第 1 轮 R2）。A 在播，T − 31 读数 80000；T 归零；
// T + 186 读数变为 X。放行拍（T + 403）读数低于 80000 即按回跳补发；不低于交给前跳判据（经过时间 434ms，门槛
// 80000 + 434 + 2000 = 82434），接着走不发事件。
//
// 变异自证（均实测，前五条改的是复述里放行拍的判据；runSession 里同一行由源码扫描守形状）：
//   - 放行低于暂扣前读数时不强制（重新比较 2s 门槛）：「低于 1ms」「暂扣中起步」两例红。
//   - 放行一律按回跳补发（第 1 版的 `seekDetected := released`）：「接着走」「恰为前跳门槛」两例红。
//   - 强制的判据改为 `<=`：等价、不红。读数等于暂扣前读数时不进变化分支，「停住」照绿。
//   - 前跳门槛 `>` 改为 `>=`：「恰为前跳门槛」红。
//   - 补发位置取暂扣起点的读数：「拖到 0.5s」「低于 1ms」「暂扣中起步」三例红。
//   - 复述里放行拍不复位行号：「拖到 0.5s」「低于 1ms」「暂扣中起步」三例红。
//   - step 的触发恒假：全部红；暂扣时长改为 300ms：除「停住」「接着走」外全部红。
func TestReplaySwitchZeroReleaseRule(t *testing.T) {
	const T = 3100
	cases := []struct {
		name  string
		after []seg
		want  string
		pos   float32 // 放行拍 resume 的位置
	}{
		{"拖到 120s（前跳）", []seg{{T + 186, "A", 120000, false}}, "resume lu22", 120},
		{"拖到 0.5s", []seg{{T + 186, "A", 500, false}}, "resume lu0", 0.5},
		{"回到 80000 停住", []seg{{T + 186, "A", 80000, false}, {T + 434, "A", 80031, true}}, "", 0},
		{"回到 80000 接着走（R2 的反例）", []seg{{T + 186, "A", 80000, true}}, "", 0},
		// 不发跳转；位置正常推进到第 17 行（81.6s 起），照常换行。
		{"恰为前跳门槛 82434", []seg{{T + 186, "A", 82434, false}}, "lu17", 0},
		{"前跳门槛外 1ms：82435", []seg{{T + 186, "A", 82435, false}}, "resume lu17", 82.435},
		{"低于暂扣前读数 1ms：79999", []seg{{T + 186, "A", 79999, false}}, "resume lu16", 79.999},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newReplay(-0.4, zeroSongs())
			ss := append([]seg{playTo(t, "A", 80000, T), {T, "A", 0, false}}, c.after...)
			r.run(0, T+1000, tick, segs(ss...))
			evs := expectFrom(t, r, T, c.want)
			if strings.HasPrefix(c.want, "resume") {
				expectAt(t, evs[0], "playback_resume", "A", T+403, c.pos)
				expectAt(t, evs[1], "lyric_update", "A", T+403, -1)
			} else if c.want != "" {
				expectAt(t, evs[0], "lyric_update", "A", T+403, -1)
			}
		})
	}

	// 暂扣中起步：A 在 2.3s 归零，T + 310 读数 400。400 + 2000 不低于 2300，重新比较不构成回跳；按放行规则补发。
	t.Run("暂扣中起步：2300 → 0 → 400", func(t *testing.T) {
		const T = 2325
		r := newReplay(-0.4, zeroSongs())
		r.run(0, T+900, tick, segs(playTo(t, "A", 2300, T), seg{T, "A", 0, false}, seg{T + 310, "A", 400, false}))
		evs := expectFrom(t, r, T, "resume lu0")
		expectAt(t, evs[0], "playback_resume", "A", T+403, 0.4)
	})
}

// TestReplaySwitchZeroNoPauseWhileHeld：暂扣的拍不判暂停（§7.3 S11）。A 停在 50s 已 837ms（未判暂停），T 归零：暂扣中
// 距 A 最后一次变化超过 1s，不判暂停。换歌则认领 B；歌名不变则放行补发，再停走超过 1s 才判暂停。
//
// 变异自证（均实测）：复述里暂扣的拍照常判暂停，两例都红（T + 186 发 pause(50)）；step 的触发恒假，两例都红（T 发
// resume(0)）；复述里暂扣判定挪到换歌分支之前，换歌一例红。
func TestReplaySwitchZeroNoPauseWhileHeld(t *testing.T) {
	const T = 3100
	stalled := []seg{{0, "A", 50000 - (T - 837), true}, {T - 806, "A", 50000, false}, {T, "A", 0, false}}

	t.Run("换歌", func(t *testing.T) {
		r := newReplay(-0.4, zeroSongs())
		r.run(0, T+600, tick, segs(append(stalled, seg{T + 249, "B", 0, false})...))
		evs := expectFrom(t, r, T, "all lu0")
		expectAt(t, evs[0], "all_lyrics", "B", T+279, 0)
	})
	t.Run("歌名不变", func(t *testing.T) {
		r := newReplay(-0.4, zeroSongs())
		r.run(0, T+1500, tick, segs(stalled...))
		evs := expectFrom(t, r, T, "resume lu0 pause")
		expectAt(t, evs[0], "playback_resume", "A", T+403, 0)
		expectAt(t, evs[2], "playback_pause", "A", T+1426, 0)
	})
}

// TestReplaySwitchZeroRapidSwitches：快速连续换歌（§7.3 S12）。A 归零换 B；B 停在 0 时换 N（读数 0，不是回跳）；
// N 放到约 3s 归零、60ms 后换回 B。期望全程没有 resume。
//
// 变异自证（均实测）：step 的触发恒假，本例红（A 与 N 各一条 resume(0)）；drop 不清空、复述里认领不调 drop、复述里
// 暂扣判定挪到换歌分支之前，各自红。
func TestReplaySwitchZeroRapidSwitches(t *testing.T) {
	const T = 3100
	const T2 = T + 3751
	r := newReplay(-0.4, zeroSongs())
	r.run(0, T2+400, tick, segs(playTo(t, "A", 80000, T), seg{T, "A", 0, false}, seg{T + 61, "B", 0, false},
		seg{T + 700, "N", 0, false}, seg{T + 744, "N", 0, true}, seg{T2, "N", 0, false}, seg{T2 + 60, "B", 0, false}))
	evs := expectFrom(t, r, T, "all lu0 all lu-1 all lu0")
	expectAt(t, evs[0], "all_lyrics", "B", T+62, 0)
	expectAt(t, evs[2], "all_lyrics", "N", T+713, 0)
	expectAt(t, evs[4], "all_lyrics", "B", T2+62, 0)
}

// TestReplaySwitchZeroResumeMidway：归零后新歌从 5s 续播（§7.3 S13c）。暂扣期间 lastMemProgress 保持上一首的读数，
// progressCarriedOver 以它比较，B 当拍认领。现行回跳那拍已把它改成 0，要等满 2s 才认领。
//
// 变异自证（均实测）：step 的触发恒假，本例红（B 等满 2s 才认领）；复述里暂扣的拍照样更新读数与锚点，红（同上）；
// drop 不清空、复述里认领不调 drop、复述里暂扣判定挪到换歌分支之前，各自红。
func TestReplaySwitchZeroResumeMidway(t *testing.T) {
	const T = 3100
	r := newReplay(-0.4, zeroSongs())
	r.run(0, T+400, tick, segs(playTo(t, "A", 80000, T), seg{T, "A", 0, false}, seg{T + 62, "B", 5000, true}))
	evs := expectFrom(t, r, T, "all lu0")
	expectAt(t, evs[0], "all_lyrics", "B", T+62, 5)
}

// TestReplaySwitchZeroShortPrev：上一首不足 2s 时换歌不是回跳、不暂扣；2.5s 时暂扣并随认领丢弃（§7.3 S14）。两例都
// 没有 resume。
//
// 变异自证（均实测）：step 的触发恒假，2.5s 一例红（A 的 resume(0) 与第 0 行）；复述里暂扣判定挪到换歌分支之前，
// 2.5s 一例红。
func TestReplaySwitchZeroShortPrev(t *testing.T) {
	cases := []struct {
		from uint32
		T    int
	}{{1500, 1519}, {2500, 2511}}
	for _, c := range cases {
		t.Run(fmt.Sprintf("上一首在 %dms", c.from), func(t *testing.T) {
			r := newReplay(-0.4, zeroSongs())
			r.run(0, c.T+600, tick, segs(playTo(t, "A", c.from, c.T), seg{c.T, "A", 0, false}, seg{c.T + 61, "B", 0, false}))
			evs := expectFrom(t, r, c.T, "all lu0")
			expectAt(t, evs[0], "all_lyrics", "B", c.T+62, 0)
		})
	}
}

// TestReplaySwitchZeroClearedAfterRelease：放行之后暂扣已清（§7.3 S15）。S4 之后稳定播放，T + 3007 往回拖 1.5s
// （不足 2s，不是回跳，不发事件），再拖到 100s：当拍发 resume。全程两条 resume，没有 pause。
//
// 变异自证（均实测）：放行后不清空暂扣，本例红（之后每拍都算放行，往回 1.5s 那一拍被强制补发 resume）；step 的触发
// 恒假、暂扣时长改为 300ms、复述里暂扣的拍照样更新读数与锚点，各自红。
func TestReplaySwitchZeroClearedAfterRelease(t *testing.T) {
	const T = 3100
	const T3 = T + 5022
	r := newReplay(-0.4, zeroSongs())
	r.run(0, T3+300, tick, segs(playTo(t, "A", 80000, T), seg{T, "A", 300, false}, seg{T + 1200, "A", 1368, true},
		seg{T + 3007, "A", 1675, true}, seg{T3, "A", 100000, true}))
	var resumes []replayEvent
	for _, e := range evFrom(r, T) {
		switch e.typ {
		case "playback_resume":
			resumes = append(resumes, e)
		case "playback_pause":
			t.Errorf("不应判暂停：%v", e)
		}
	}
	if len(resumes) != 2 {
		t.Fatalf("resume %d 条，期望 2；明细：%v", len(resumes), evFrom(r, T))
	}
	expectAt(t, resumes[0], "playback_resume", "A", T+403, 0.3)
	expectAt(t, resumes[1], "playback_resume", "A", T3, 100)
}

// TestReplaySwitchZeroBlankName：归零后歌名为空串直到期满（§7.3 S16）。放行不要求歌名，期满补发上一首的回跳（同现行，
// 晚 400ms），随后认领 B。
//
// 变异自证（均实测）：复述里期满且歌名为空时继续暂扣，本例红（没有 A 的 resume）；step 的触发恒假、暂扣时长改为
// 300ms、复述里放行拍不复位行号，各自红。
func TestReplaySwitchZeroBlankName(t *testing.T) {
	const T = 3100
	r := newReplay(-0.4, zeroSongs())
	r.run(0, T+900, tick, segs(playTo(t, "A", 80000, T), seg{T, "A", 0, false}, seg{T + 31, "", 0, false}, seg{T + 500, "B", 0, false}))
	evs := expectFrom(t, r, T, "resume lu0 all lu0")
	expectAt(t, evs[0], "playback_resume", "A", T+403, 0)
	expectAt(t, evs[2], "all_lyrics", "B", T+527, 0)
}

// ---------------------------------------------------------------- 回放：取词之后的重读（F、C 组）

// fillRun 先跑到补取那一拍 fillTick（含），跳过取词耗时 fetchMs，再从其后的第一拍跑到 end。
func fillRun(r *replay, fillTick, fetchMs, end int, at func(int) (string, uint32)) {
	r.run(0, fillTick, tick, at)
	r.run(fillTick+fetchMs+tick, end, tick, at)
}

// TestReplayFillRereadZero：补取的取词期间读数归零（§7.3 F1、F2，设计审第 1 轮 R1 的反例）。P 在播，上一拍读数 5000，
// 本拍（T）首读 5031，补取成功，取词 150ms，重读 P/0。期望补取拍不采用重读：按首读发补取结果（位置外推过取词
// 耗时 5.181）与第 1 行；取词之后第一拍（T + 181）读到 0，相对 5031 构成触发，开始暂扣。换歌则认领 B，从读到 P/0
// 到认领 B 之间 P 没有 resume、没有第 0 行；歌名不变则期满（T + 584）补发 resume(0) 与第 0 行。
//
// 变异自证（均实测）：rereadSameSong 去掉 !startsHold，两例都红（补取拍发 all(0)、resume(0)、第 0 行）；step 的触发
// 恒假，两例都红；复述里暂扣判定挪到换歌分支之前，换歌一例红。
func TestReplayFillRereadZero(t *testing.T) {
	const T = 3100
	fill := replayFill{song: "P", from: T, ok: true, lines: coastLyrics(), fetchMs: 150, rereadName: "P", rereadMs: 0}

	t.Run("随后换歌", func(t *testing.T) {
		r := newReplay(-0.4, zeroSongs())
		r.fills = []replayFill{fill}
		fillRun(r, T, 150, T+800, segs(seg{0, "P", 1931, true}, seg{T + 1, "P", 0, false}, seg{T + 250, "B", 0, false}))
		evs := expectFrom(t, r, T, "all lu1 all lu0")
		expectAt(t, evs[0], "all_lyrics", "P", T, 5.181)
		expectAt(t, evs[1], "lyric_update", "P", T, -1)
		expectAt(t, evs[2], "all_lyrics", "B", T+274, 0)
	})
	t.Run("歌名不变", func(t *testing.T) {
		r := newReplay(-0.4, zeroSongs())
		r.fills = []replayFill{fill}
		fillRun(r, T, 150, T+900, segs(seg{0, "P", 1931, true}, seg{T + 1, "P", 0, false}))
		evs := expectFrom(t, r, T, "all lu1 resume lu0")
		expectAt(t, evs[2], "playback_resume", "P", T+584, 0)
		expectAt(t, evs[3], "lyric_update", "P", T+584, -1)
	})
}

// TestReplayFillFailedRereadZero：同上，取词失败（§7.3 F3）。补取拍不发事件，T + 181 开始暂扣；换歌则认领 B；歌名
// 不变则期满只有 resume(0)（尚无歌词行），其后的下一次补取（成功、重读不回退）在其拍发 all_lyrics 与第 0 行。
//
// 变异自证（均实测）：rereadSameSong 去掉 !startsHold，两例都红（补取拍发 resume(0)）；step 的触发恒假，两例都红。
func TestReplayFillFailedRereadZero(t *testing.T) {
	const T = 3100
	failed := replayFill{song: "P", from: T, fetchMs: 150, rereadName: "P", rereadMs: 0}

	t.Run("随后换歌", func(t *testing.T) {
		r := newReplay(-0.4, zeroSongs())
		r.fills = []replayFill{failed}
		fillRun(r, T, 150, T+800, segs(seg{0, "P", 1931, true}, seg{T + 1, "P", 0, false}, seg{T + 250, "B", 0, false}))
		evs := expectFrom(t, r, T, "all lu0")
		expectAt(t, evs[0], "all_lyrics", "B", T+274, 0)
	})
	t.Run("歌名不变，之后补取成功", func(t *testing.T) {
		r := newReplay(-0.4, zeroSongs())
		r.fills = []replayFill{failed,
			{song: "P", from: T + 700, ok: true, lines: coastLyrics(), rereadName: "P", rereadMs: 0}}
		fillRun(r, T, 150, T+900, segs(seg{0, "P", 1931, true}, seg{T + 1, "P", 0, false}))
		evs := expectFrom(t, r, T, "resume all lu0")
		expectAt(t, evs[0], "playback_resume", "P", T+584, 0)
		expectAt(t, evs[1], "all_lyrics", "P", T+708, 0)
	})
}

// TestReplayFillRereadForward：重读正常前进（§7.3 F4）：照现行采用，补取结果的位置是重读的读数 5.100（不是首读的
// 外推值 5.181）。
//
// 变异自证（均实测）：rereadSameSong 改为一律不采用，本例红（位置 5.181）。
func TestReplayFillRereadForward(t *testing.T) {
	const T = 3100
	r := newReplay(-0.4, zeroSongs())
	r.fills = []replayFill{{song: "P", from: T, ok: true, lines: coastLyrics(), fetchMs: 150, rereadName: "P", rereadMs: 5100}}
	fillRun(r, T, 150, T+600, segs(seg{0, "P", 1931, true}, seg{T + 181, "P", 5131, true}))
	evs := expectFrom(t, r, T, "all lu1")
	expectAt(t, evs[0], "all_lyrics", "P", T, 5.1)
}

// TestReplayFillAfterRelease：补取在暂扣中到期（§7.3 F5，代码审第 1 轮 P2 改写）。P 在 80s（歌词未到），T 拖到 0.3s，
// 暂扣；补取登记在 T + 31。暂扣中不补取，放行拍（T + 403）也不补取，当拍发 resume(0.300)；下一拍（T + 434）才取词
// （100ms），重读 P/300 相对 lastMemProgress（已是放行拍的 300）不构成触发，采用，发 all_lyrics(0.300) 与第 0 行。
//
// 变异自证（均实测）：复述里放行拍照常补取，本例红（补取结果在放行拍、排在 resume 之前）；复述里暂扣判定挪到补取块
// 之后，红（暂扣中 T + 31 即补取）；rereadSameSong 改为一律不采用，红（位置 0.400）；放行后不清空暂扣，红（此后每拍
// 都算放行而不补取）；step 的触发恒假、暂扣时长改为 300ms、复述里暂扣的拍更新读数与锚点，各自红。
func TestReplayFillAfterRelease(t *testing.T) {
	const T = 3100
	r := newReplay(-0.4, zeroSongs())
	r.fills = []replayFill{{song: "P", from: T + 31, ok: true, lines: coastLyrics(), fetchMs: 100, rereadName: "P", rereadMs: 300}}
	fillRun(r, T+434, 100, T+900, segs(seg{0, "P", 76931, true}, seg{T, "P", 300, false}))
	evs := expectFrom(t, r, T, "resume all lu0")
	expectAt(t, evs[0], "playback_resume", "P", T+403, 0.3)
	expectAt(t, evs[1], "all_lyrics", "P", T+434, 0.3)
	expectAt(t, evs[2], "lyric_update", "P", T+434, 0.431)
}

// TestReplayReleaseNotBlockedByFill：放行拍即使补取到期也不阻塞（代码审第 1 轮 P2 的反例）。P 在 80s（歌词未到），
// T 拖到 0.3s，暂扣；补取登记在 T + 31、取词耗时 1s。补发的 resume 必须在放行拍（墙钟 T + 403）发出，补取顺延到下一拍
// （T + 434，墙钟 T + 1434 才返回）。取词失败与成功各一例；拖动之后读数从 T + 404 起随时间走，免得叠上 #172 的
// 起步停顿误判暂停。
//
// 变异自证（均实测）：复述里放行拍照常补取，两例都红（resume 的墙钟是 T + 1403：放行拍先取词 1s 才发跳转）；复述里
// 暂扣判定挪到补取块之后，两例都红；放行后不清空暂扣，成功一例红；step 的触发恒假、暂扣时长改为 300ms、复述里暂扣
// 的拍更新读数与锚点，各自红。runSession 里补取块的 `!released` 由文件末的源码扫描钉住。
func TestReplayReleaseNotBlockedByFill(t *testing.T) {
	const T = 3100
	at := segs(seg{0, "P", 76931, true}, seg{T, "P", 300, false}, seg{T + 404, "P", 300, true})
	for _, ok := range []bool{false, true} {
		t.Run(fmt.Sprintf("取词成功=%v", ok), func(t *testing.T) {
			r := newReplay(-0.4, zeroSongs())
			r.fills = []replayFill{{song: "P", from: T + 31, ok: ok, lines: coastLyrics(), fetchMs: 1000, rereadName: "P", rereadMs: 1330}}
			fillRun(r, T+434, 1000, T+1700, at)
			want := "resume"
			if ok {
				want = "resume all lu0"
			}
			evs := expectFrom(t, r, T, want)
			expectAt(t, evs[0], "playback_resume", "P", T+403, 0.3)
			if evs[0].at != T+403 {
				t.Errorf("resume 的墙钟 %dms，期望在放行拍当即发出（%dms）", evs[0].at, T+403)
			}
			if ok {
				expectAt(t, evs[1], "all_lyrics", "P", T+434, 1.33)
				if evs[1].at != T+1434 {
					t.Errorf("补取结果的墙钟 %dms，期望取词 1s 之后（%dms）", evs[1].at, T+1434)
				}
			}
		})
	}
}

// TestReplayFillOutOfBandFirstRead：设计审第 2 轮 R1 的反例（§7.3 F6）。上一次读数 5000，本拍首读 2000（拖到 2s，带外
// 回跳，不暂扣），补取期间归零、重读 P/0，随后换歌。以 lastMemProgress 为参照不采用重读：补取拍按首读依次发补取
// 结果（2.150）、resume(2.000) 与第 1 行（P 的歌词取第 1 行起点 1.0s 的一组，2s 处不在第 0 行）；下一拍读到 0，
// 相对 2000 不构成回跳，只重锚；随后认领 B。P 恰一条 resume，没有 resume(0)，归零之后没有第 0 行。
//
// 变异自证（均实测）：复述里补取处的参照改回本拍首读（第 3 版的写法），本例红（补取拍发 all(0)、resume(0)、第 0 行）；
// rereadSameSong 去掉 !startsHold，同样红；带宽改为无穷大，红（首读 2000 即被暂扣）。
func TestReplayFillOutOfBandFirstRead(t *testing.T) {
	const T = 3100
	r := newReplay(-0.4, zeroSongs())
	r.fills = []replayFill{{song: "P", from: T, ok: true, lines: linesAt(0, 1, 3, 6, 10), fetchMs: 150, rereadName: "P", rereadMs: 0}}
	fillRun(r, T, 150, T+800, segs(seg{0, "P", 1931, true}, seg{T, "P", 2000, false}, seg{T + 1, "P", 0, false}, seg{T + 250, "B", 0, false}))
	evs := expectFrom(t, r, T, "all resume lu1 all lu0")
	expectAt(t, evs[0], "all_lyrics", "P", T, 2.15)
	expectAt(t, evs[1], "playback_resume", "P", T, 2)
	expectAt(t, evs[2], "lyric_update", "P", T, -1)
	expectAt(t, evs[3], "all_lyrics", "B", T+274, 0)
}

// TestReplayFillAfterReleaseContinuing：放行拍接着走，下一拍补取、取词期间换歌归零（§7.3 F7，代码审第 1 轮 P2 改写）。
// P 在 80s（歌词未到），T 归零、暂扣；T + 186 回到 80000 接着走。放行拍（T + 403，读数 80217）不补取、不发跳转，
// lastMemProgress 成为 80217。下一拍（T + 434，首读 80248）补取，重读 P/0 相对 80217 构成触发、不采用：按首读发补取
// 结果（外推 80.348）与第 16 行，不发跳转。取词之后第一拍（T + 565）读到 0，相对 80248 构成触发，开始新的暂扣（断言
// 起点与读数）；T + 627 认领 B，暂扣随之丢弃。P 全程没有 resume，没有第 0 行。
//
// 变异自证（均实测）：rereadSameSong 去掉 !startsHold，本例红（补取拍发 all(0)、resume(0)、第 0 行）；放行后不清空
// 暂扣，红（此后每拍都算放行而不补取）；复述里放行一律按回跳补发，红（放行拍发 resume(80.217)）；复述里放行拍照常
// 补取，红；复述里暂扣判定挪到补取块之后，红；drop 不清空、复述里认领不调 drop、复述里暂扣判定挪到换歌分支之前、
// step 的触发恒假、暂扣时长改为 300ms，各自红。
func TestReplayFillAfterReleaseContinuing(t *testing.T) {
	const T = 3100
	r := newReplay(-0.4, zeroSongs())
	r.fills = []replayFill{{song: "P", from: T + 31, ok: true, lines: coastLyrics(), fetchMs: 100, rereadName: "P", rereadMs: 0}}
	at := segs(seg{0, "P", 76931, true}, seg{T, "P", 0, false}, seg{T + 186, "P", 80000, true},
		seg{T + 435, "P", 0, false}, seg{T + 603, "B", 0, false})
	fillRun(r, T+434, 100, T+596, at)
	if want := r.t0.Add((T + 565) * time.Millisecond); !r.hold.since.Equal(want) || r.hold.fromMs != 80248 || r.hold.toMs != 0 {
		t.Fatalf("T + 596 时的暂扣 %+v，期望起点 T + 565、由 80248 回退到 0", r.hold)
	}
	r.run(T+627, T+900, tick, at)
	evs := expectFrom(t, r, T, "all lu16 all lu0")
	expectAt(t, evs[0], "all_lyrics", "P", T+434, 80.348)
	expectAt(t, evs[1], "lyric_update", "P", T+434, -1)
	expectAt(t, evs[2], "all_lyrics", "B", T+627, 0)
}

// TestReplayClaimReread：认领那一拍取词之后的重读（§7.3 C1～C3）。
//
// 变异自证（均实测）：复述里认领处的重读一律采用，C1 两例红（宣告位置 0，随后 resume(0)）；rereadSameSong 改为一律
// 不采用，C2、C3 红（宣告位置 0.950、80.200）；rereadSameSong 去掉带宽（只要构成回跳即不采用），C3 红；rereadSameSong
// 去掉 !startsHold，C1 两例红；带宽改为无穷大，C3 红；触发不要求构成回跳，C2 红；复述里暂扣判定挪到换歌分支之前，
// C1 换歌一例红。
func TestReplayClaimReread(t *testing.T) {
	t.Run("C1 会话第一首在 80s，取词期间归零，随后换歌", func(t *testing.T) {
		r := newReplay(-0.4, zeroSongs())
		r.claimRereads[0] = replayReread{fetchMs: 200, name: "B", ms: 0}
		fillRun(r, 0, 200, 700, segs(seg{0, "B", 80000, false}, seg{1, "B", 0, false}, seg{300, "C", 0, false}))
		evs := expectFrom(t, r, 0, "all lu14 all lu0")
		expectAt(t, evs[0], "all_lyrics", "B", 0, 80.2)
		expectAt(t, evs[2], "all_lyrics", "C", 324, 0)
	})
	t.Run("C1 变体：歌名不变", func(t *testing.T) {
		r := newReplay(-0.4, zeroSongs())
		r.claimRereads[0] = replayReread{fetchMs: 200, name: "B", ms: 0}
		fillRun(r, 0, 200, 900, segs(seg{0, "B", 80000, false}, seg{1, "B", 0, false}))
		evs := expectFrom(t, r, 0, "all lu14 resume lu0")
		expectAt(t, evs[0], "all_lyrics", "B", 0, 80.2)
		expectAt(t, evs[2], "playback_resume", "B", 634, 0)
	})
	t.Run("C2 认领时 750、重读 40：采用", func(t *testing.T) {
		r := newReplay(-0.4, zeroSongs())
		r.claimRereads[0] = replayReread{fetchMs: 200, name: "B", ms: 40}
		r.run(0, 0, tick, segs(seg{0, "B", 750, false}))
		evs := expectFrom(t, r, 0, "all lu0")
		expectAt(t, evs[0], "all_lyrics", "B", 0, 0.04)
	})
	t.Run("C3 认领时 80s、重读 30s（带外）：采用并当拍发回跳", func(t *testing.T) {
		r := newReplay(-0.4, zeroSongs())
		r.claimRereads[0] = replayReread{fetchMs: 200, name: "B", ms: 30000}
		r.run(0, 0, tick, segs(seg{0, "B", 80000, false}))
		evs := expectFrom(t, r, 0, "all resume lu5")
		expectAt(t, evs[0], "all_lyrics", "B", 0, 30)
		expectAt(t, evs[1], "playback_resume", "B", 0, 30)
	})
}

// ---------------------------------------------------------------- 接线门禁（§7.4）

// switchZeroWiringProblems 按设计 §7.4 检查 src（qqmusic.go 的源码）里 runSession 的接线形状，返回发现的问题。
// runSession 要真进程、没有离线夹具，回放是复述；这里只查形状，不替代读代码。
func switchZeroWiringProblems(src string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "qqmusic.go", src, 0)
	if err != nil {
		return []string{"解析失败: " + err.Error()}
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if d, ok := d.(*ast.FuncDecl); ok && d.Name.Name == "runSession" {
			fn = d
		}
	}
	if fn == nil {
		return []string{"没找到 runSession —— 门禁失效"}
	}
	var loop *ast.ForStmt
	for _, s := range fn.Body.List {
		if l, ok := s.(*ast.ForStmt); ok && l.Cond == nil {
			loop = l
		}
	}
	if loop == nil {
		return []string{"没找到 runSession 的轮询循环 —— 门禁失效"}
	}

	calls := func(n ast.Node, recv, name string) []*ast.CallExpr {
		var out []*ast.CallExpr
		ast.Inspect(n, func(x ast.Node) bool {
			c, ok := x.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := c.Fun.(type) {
			case *ast.SelectorExpr:
				if id, ok := fun.X.(*ast.Ident); ok && id.Name == recv && fun.Sel.Name == name {
					out = append(out, c)
				}
			case *ast.Ident:
				if recv == "" && fun.Name == name {
					out = append(out, c)
				}
			}
			return true
		})
		return out
	}
	isIdent := func(e ast.Expr, name string) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == name
	}

	switchIdx, stepIdx, heldIdx, fillIdx, changeIdx := -1, -1, -1, -1, -1
	for i, s := range loop.Body.List {
		switch s := s.(type) {
		case *ast.IfStmt:
			cond := types.ExprString(s.Cond)
			switch {
			case len(calls(s, "", "progressCarriedOver")) > 0 && switchIdx < 0:
				switchIdx = i
			case isIdent(s.Cond, "held") && len(s.Body.List) == 1:
				if b, ok := s.Body.List[0].(*ast.BranchStmt); ok && b.Tok == token.CONTINUE {
					heldIdx = i
				}
			case strings.Contains(cond, "lyricsPending") && fillIdx < 0:
				fillIdx = i
			case cond == "meta.ProgressMs != lastMemProgress":
				changeIdx = i
			}
		case *ast.AssignStmt:
			for _, c := range calls(s, "hold", "step") {
				if len(c.Args) == 3 && isIdent(c.Args[0], "lastMemProgress") {
					stepIdx = i
				}
			}
		}
	}
	var probs []string
	for name, idx := range map[string]int{"换歌分支": switchIdx, "暂扣判定 hold.step(lastMemProgress, …)": stepIdx,
		"if held { continue }": heldIdx, "补取块": fillIdx, "变化分支": changeIdx} {
		if idx < 0 {
			probs = append(probs, "没找到"+name)
		}
	}
	if len(probs) > 0 {
		return probs
	}
	if !(switchIdx < stepIdx && stepIdx < heldIdx && heldIdx < fillIdx && fillIdx < changeIdx) || heldIdx != stepIdx+1 {
		probs = append(probs, fmt.Sprintf("次序不对：换歌分支 %d、暂扣判定 %d、if held %d、补取块 %d、变化分支 %d",
			switchIdx, stepIdx, heldIdx, fillIdx, changeIdx))
	}

	// 放行拍不补取（代码审第 1 轮 P2）：补取块的条件里有 !released。
	if cond := types.ExprString(loop.Body.List[fillIdx].(*ast.IfStmt).Cond); !strings.Contains(cond, "!released &&") {
		probs = append(probs, "补取块的条件应含 !released（放行拍不补取），实得 "+cond)
	}

	if n := len(calls(loop.Body.List[switchIdx], "hold", "drop")); n != 1 || len(calls(fn, "hold", "drop")) != 1 {
		probs = append(probs, fmt.Sprintf("hold.drop 应恰在换歌分支里调一次，换歌分支里 %d 次", n))
	}

	rereads := calls(fn, "", "rereadSameSong")
	inSwitch := calls(loop.Body.List[switchIdx], "", "rereadSameSong")
	inFill := calls(loop.Body.List[fillIdx], "", "rereadSameSong")
	if len(rereads) != 2 || len(inSwitch) != 1 || len(inFill) != 1 {
		probs = append(probs, fmt.Sprintf("rereadSameSong 应恰有两处（换歌分支、补取块各一），实得 %d（换歌分支 %d、补取块 %d）",
			len(rereads), len(inSwitch), len(inFill)))
	}
	for _, c := range rereads {
		if len(c.Args) != 4 || !isIdent(c.Args[3], "lastMemProgress") {
			probs = append(probs, "rereadSameSong 的参照应为 lastMemProgress："+types.ExprString(c))
		}
	}

	seekInit := ""
	for _, s := range loop.Body.List[changeIdx].(*ast.IfStmt).Body.List {
		if a, ok := s.(*ast.AssignStmt); ok && a.Tok == token.DEFINE && len(a.Lhs) == 1 && isIdent(a.Lhs[0], "seekDetected") {
			seekInit = types.ExprString(a.Rhs[0])
		}
	}
	if seekInit != "released && meta.ProgressMs < lastMemProgress" {
		probs = append(probs, "变化分支里 seekDetected 的初值应为 released && meta.ProgressMs < lastMemProgress，实得 "+seekInit)
	}
	return probs
}

func readQQMusicSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("qqmusic.go")
	if err != nil {
		t.Fatalf("读 qqmusic.go 失败: %v", err)
	}
	return string(b)
}

// TestRunSessionSwitchZeroWiring 钉住 runSession 里 #202 的接线形状（§7.4）：暂扣判定在换歌分支之后、补取与变化
// 分支之前，紧跟 if held { continue }；补取块的条件含 !released（放行拍不补取，代码审第 1 轮 P2）；hold.drop 恰在
// 换歌分支里；两处重读都经 rereadSameSong、参照为 lastMemProgress；变化分支里 seekDetected 以放行与低于暂扣前
// 读数起始。门禁自身的变异见下一条。
func TestRunSessionSwitchZeroWiring(t *testing.T) {
	if probs := switchZeroWiringProblems(readQQMusicSource(t)); len(probs) > 0 {
		t.Fatalf("runSession 的 #202 接线不符：\n  %s", strings.Join(probs, "\n  "))
	}
}

// TestSwitchZeroWiringCatchesKnownHoles 以改写过的源码确认门禁拦得住设计 §7.6 里只能靠它守的接线变异：每种改写都要
// 报出问题。改写找不到原文时自报失效（源码变了，门禁的这条自测要跟着改）。
//
// 变异自证（均实测，改的是上面的 switchZeroWiringProblems）：去掉次序检查，「挪到补取块之后」「挪到换歌分支之前」
// 红；不查重读的参照，两条「改传本拍首读」红；不查 seekDetected 的初值，「改回只看放行」红；不查 hold.drop，
// 「去掉 hold.drop」红；暂扣判定不看第一个实参，「暂扣判定改传本拍首读」红；不查补取块的 !released，「放行拍照常
// 补取」红。
func TestSwitchZeroWiringCatchesKnownHoles(t *testing.T) {
	src := readQQMusicSource(t)
	stepBlock := "\t\theld, released := hold.step(lastMemProgress, meta.ProgressMs, sampleTime)\n\t\tif held {\n\t\t\tcontinue\n\t\t}\n"
	switchHead := "\t\t// --- 切歌检测 ---\n"
	changeHead := "\t\t// --- 快速计时器锚点更新 + seek 检测 ---\n"
	move := func(to string) func(string) string {
		return func(s string) string {
			s = strings.Replace(s, stepBlock, "", 1)
			return strings.Replace(s, to, stepBlock+to, 1)
		}
	}
	replace := func(old, new string, n int) func(string) string {
		return func(s string) string { return strings.Replace(s, old, new, n) }
	}
	rereadArg := "rereadSameSong(fresh, rerr, lastName, lastMemProgress)"
	cases := []struct {
		name   string
		mutate func(string) string
		needle string // 改写依赖的原文
	}{
		{"暂扣判定挪到补取块之后", move(changeHead), stepBlock},
		{"暂扣判定挪到换歌分支之前", move(switchHead), stepBlock},
		{"去掉 hold.drop", replace("hold.drop(sampleTime)", "_ = sampleTime", 1), "hold.drop(sampleTime)"},
		{"认领处的重读改传本拍首读", replace(rereadArg, "rereadSameSong(fresh, rerr, lastName, meta.ProgressMs)", 1), rereadArg},
		{"补取处的重读改传本拍首读", func(s string) string {
			i := strings.LastIndex(s, rereadArg)
			return s[:i] + "rereadSameSong(fresh, rerr, lastName, meta.ProgressMs)" + s[i+len(rereadArg):]
		}, rereadArg},
		{"seekDetected 改回只看放行", replace("seekDetected := released && meta.ProgressMs < lastMemProgress", "seekDetected := released", 1),
			"seekDetected := released && meta.ProgressMs < lastMemProgress"},
		{"暂扣判定改传本拍首读", replace("hold.step(lastMemProgress,", "hold.step(meta.ProgressMs,", 1), "hold.step(lastMemProgress,"},
		{"去掉 if held { continue }", replace("\t\tif held {\n\t\t\tcontinue\n\t\t}\n", "\t\t_ = held\n", 1), "\t\tif held {\n\t\t\tcontinue\n\t\t}\n"},
		{"放行拍照常补取（补取块去掉 !released）", replace("if lyricsPending && !released && meta.Name == lastName", "if lyricsPending && meta.Name == lastName", 1),
			"if lyricsPending && !released && meta.Name == lastName"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(src, c.needle) {
				t.Fatalf("qqmusic.go 里找不到改写依赖的原文 %q —— 本条自测失效", c.needle)
			}
			mutated := c.mutate(src)
			if mutated == src {
				t.Fatalf("改写没有生效 —— 本条自测失效")
			}
			if probs := switchZeroWiringProblems(mutated); len(probs) == 0 {
				t.Fatalf("门禁没有拦下这处改写")
			}
		})
	}
}
