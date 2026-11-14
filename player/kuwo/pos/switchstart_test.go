package pos

import (
	"slices"
	"strconv"
	"testing"
	"time"
)

// This file tests only the song-switch anchor of the #135 transient filter
// (#198, doc/design/kuwo-switch-start-transient-design.md §5.1 A-3, A-4, A-5):
// the output-segment tracking fed on every tick, including the ticks with no core
// sample, and its use on a song's opening segment. The ticks are synthetic, after
// the recorded shapes of design §3.

const rC = "MUSIC_3"

// swTick is a core sample (rid set) or an output-only observation (rid empty,
// the Track entry: no-song, bridge settling or a failed core read).
type swTick struct {
	ms   int64
	rid  string
	core int32
	out  OutputState
	play bool
	ival time.Duration
}

// noPf marks an output-only tick in the returned positions.
const noPf = int32(-1 << 30)

// driveSwitch runs the ticks and returns, per tick, P_f (noPf for an
// output-only tick) and TransientResync.
func driveSwitch(t *testing.T, f *TransientFilter, ticks ...swTick) ([]int32, []bool) {
	t.Helper()
	base := time.Unix(100, 0)
	var pfs []int32
	var res []bool
	for i, tk := range ticks {
		at := base.Add(time.Duration(tk.ms) * time.Millisecond)
		if tk.rid == "" {
			f.Track(tk.out, at)
			pfs, res = append(pfs, noPf), append(res, false)
			continue
		}
		iv := tk.ival
		if iv == 0 {
			iv = 50 * time.Millisecond
		}
		s := Sample{RID: tk.rid, PositionMs: tk.core, CoreMs: tk.core, DurationMs: 600000, ObservedAt: at}
		o, r := f.Apply(s, tk.out, tk.play, iv)
		if o.CoreMs != tk.core {
			t.Fatalf("tick %d: CoreMs changed: got %d want %d", i, o.CoreMs, tk.core)
		}
		pfs, res = append(pfs, o.PositionMs), append(res, r)
	}
	return pfs, res
}

// at returns the index of the tick at ms.
func tickAt(ticks []swTick, ms int64) int {
	for i, tk := range ticks {
		if tk.ms == ms {
			return i
		}
	}
	panic("no tick at that time")
}

// prevSong is song A playing on its own segment until ms (its last core sample).
func prevSong(untilMs int64) []swTick {
	var ticks []swTick
	for ms := untilMs - 100; ms <= untilMs; ms += 50 {
		ticks = append(ticks, swTick{ms: ms, rid: rA, core: int32(150000 + ms), out: dsOut(0, 150000+ms), play: true})
	}
	return ticks
}

// fedSeries is output-only ticks on segment base 0 at the given (ms, fed ms).
func fedSeries(pairs ...int64) []swTick {
	var ticks []swTick
	for i := 0; i+1 < len(pairs); i += 2 {
		ticks = append(ticks, swTick{ms: pairs[i], out: dsOut(0, pairs[i+1])})
	}
	return ticks
}

// Design §7 item 1 (H1, the ws/23 愿与愁 shape): song B's output opens and
// crosses the Play threshold while the bridge reports no song; the first core
// sample of B is already at the 1515 ms stall. The tracked crossing anchors it:
// the public value walks from the anchor and never shows 1515, and the change
// tick sets no TransientResync.
// 变异自证：A-4 不查跟踪（照旧降级），t=400 发布 1515（实测）；RID 变化拍也置 resync，t=400 resync 为真（实测）。
func TestSwitchStartTrackedAnchorNoSong(t *testing.T) {
	var f TransientFilter
	ticks := prevSong(-100)
	ticks = append(ticks, fedSeries(0, 150000, 50, 0, 100, 300, 150, 600, 200, 900, 250, 1600, 300, 1600, 350, 1650)...)
	ticks = append(ticks, swTick{ms: 400, rid: rB, core: 1515, out: dsOut(0, 1700), play: true})
	// The stall lasts until the sound (from t=306) reaches 1515, then 1:1.
	for ms := int64(450); ms <= 2100; ms += 50 {
		core := int32(1515)
		if ms-306 > 1515 {
			core = int32(ms - 306)
		}
		ticks = append(ticks, swTick{ms: ms, rid: rB, core: core, out: dsOut(0, 1700+ms), play: true})
	}
	pfs, res := driveSwitch(t, &f, ticks...)
	first := tickAt(ticks, 400)
	if pfs[first] != 94 {
		t.Fatalf("change tick: P_f = %d, want 94 (anchored at the tracked crossing t=200, L 106)", pfs[first])
	}
	if res[first] {
		t.Fatal("change tick set TransientResync")
	}
	for i := first; i < len(ticks); i++ {
		if pfs[i] >= 1515 && ticks[i].core == 1515 {
			t.Fatalf("t=%d published the stall value %d", ticks[i].ms, pfs[i])
		}
		if want := int32(ticks[i].ms - 306); ticks[i].core == 1515 && pfs[i] != want {
			t.Fatalf("t=%d: P_f = %d, want %d on the anchored line", ticks[i].ms, pfs[i], want)
		}
	}
	if pfs[len(pfs)-1] != ticks[len(ticks)-1].core {
		t.Fatalf("not disengaged after the core caught up: P_f %d, core %d", pfs[len(pfs)-1], ticks[len(ticks)-1].core)
	}
	if n := countTrue(res); n != 0 {
		t.Fatalf("%d TransientResync, want 0", n)
	}
	if _, ok := f.TakeOpenPass(); ok {
		t.Fatal("an anchored opening reported a passthrough")
	}
}

// Design §7 item 2 (the ws/25 Middle Of The Night shape): the first core sample
// of B is at the second ladder step, 600, and the next at 1515; the crossing came
// 50 ms before the first sample. Neither ladder value is published.
// 变异自证：A-4 不查跟踪，t=400 发布 600（实测）。
func TestSwitchStartTrackedAnchorAt600(t *testing.T) {
	var f TransientFilter
	ticks := prevSong(-100)
	ticks = append(ticks, fedSeries(0, 150000, 200, 0, 250, 300, 300, 600, 350, 900)...)
	ticks = append(ticks,
		swTick{ms: 400, rid: rB, core: 600, out: dsOut(0, 1200), play: true},
		swTick{ms: 450, rid: rB, core: 1515, out: dsOut(0, 1700), play: true},
		swTick{ms: 500, rid: rB, core: 1515, out: dsOut(0, 1750), play: true},
	)
	pfs, _ := driveSwitch(t, &f, ticks...)
	want := []int32{0, 0, 44} // cand = t - 350 - 106, floored at 0
	for i, w := range want {
		if got := pfs[tickAt(ticks, 400+int64(i)*50)]; got != w {
			t.Fatalf("t=%d: P_f = %d, want %d", 400+i*50, got, w)
		}
	}
}

// Design §7 item 3 (counterexample, freshness): the crossing counts only if the
// observations either side of it are at most anchorMaxGap apart, as for the
// per-tick anchor. Three failed output reads leave 200 ms: no tracked crossing,
// the opening degrades as before and reports why; one failed read leaves 100 ms:
// tracked.
// 变异自证：跟踪不查实际间隔，三次读失败一例以 t=350 锚定、P_f 不为 1515（实测）。
func TestSwitchStartTrackedCrossingFreshness(t *testing.T) {
	invalid := OutputState{}
	t.Run("gap 200 ms", func(t *testing.T) {
		var f TransientFilter
		ticks := prevSong(-100)
		ticks = append(ticks, fedSeries(0, 150000, 50, 0, 100, 300, 150, 600)...)
		ticks = append(ticks, swTick{ms: 200, out: invalid}, swTick{ms: 250, out: invalid}, swTick{ms: 300, out: invalid})
		ticks = append(ticks, fedSeries(350, 900)...)
		ticks = append(ticks, swTick{ms: 400, rid: rB, core: 1515, out: dsOut(0, 1600), play: true})
		pfs, _ := driveSwitch(t, &f, ticks...)
		if got := pfs[len(pfs)-1]; got != 1515 {
			t.Fatalf("stale crossing anchored: P_f = %d, want 1515 passthrough", got)
		}
		note, ok := f.TakeOpenPass()
		if !ok || note.Reason != OpenPassNoCrossing || note.Base != 0 {
			t.Fatalf("passthrough note = %+v, %v; want no crossing, base 0", note, ok)
		}
	})
	t.Run("gap 100 ms", func(t *testing.T) {
		var f TransientFilter
		ticks := prevSong(-100)
		ticks = append(ticks, fedSeries(0, 150000, 50, 0, 100, 300, 150, 600)...)
		ticks = append(ticks, swTick{ms: 200, out: invalid})
		ticks = append(ticks, fedSeries(250, 900)...)
		ticks = append(ticks, swTick{ms: 400, rid: rB, core: 1515, out: dsOut(0, 1600), play: true})
		pfs, _ := driveSwitch(t, &f, ticks...)
		if got := pfs[len(pfs)-1]; got != 44 { // 400 - 250 - 106
			t.Fatalf("crossing across one failed read: P_f = %d, want 44", got)
		}
	})
}

// Design §7 items 4 and 10 (counterexamples): output-only observations never
// reach the state machine, and the tracking gives no ownership. The existing
// in-song and cross-song sequences give the same P_f and TransientResync tick for
// tick with output-only ticks interleaved, and with a previous song before them.
// A seek whose crossing falls on a failed core read (only the output observed)
// still degrades, as before: the per-tick anchor needs two core ticks at most
// 150 ms apart.
// 变异自证：只观测拍更新状态机的越阈记录，seek-to-zero、paused-seek-to-start 等例与对照逐拍不同，seek-with-failed-read 一例在 t=200 锚定、置 resync（实测）。
// 「跟踪到越阈即视为开播归属」这一变异，本用例的逐拍对照两边同受影响、不变红；由既有的
// TestTransientCrossSongNoResyncWhileUnowned 变红（t=150 发出一条 resync，实测）。
func TestSwitchStartTrackingLeavesStateMachine(t *testing.T) {
	seqs := map[string][]tick{
		"ds-seek": {
			{ms: 0, rid: rA, core: 10000, out: dsOut(10000, 10000), play: true},
			{ms: 50, rid: rA, core: 30313, out: dsOut(30000, 313), play: true},
			{ms: 100, rid: rA, core: 30600, out: dsOut(30000, 600), play: true},
			{ms: 150, rid: rA, core: 30914, out: dsOut(30000, 914), play: true},
			{ms: 200, rid: rA, core: 30914, out: dsOut(30000, 1800), play: true},
			{ms: 1100, rid: rA, core: 30914, out: dsOut(30000, 2800), play: true},
			{ms: 1150, rid: rA, core: 30964, out: dsOut(30000, 2850), play: true},
		},
		"seek-to-zero": {
			{ms: 0, rid: rA, core: 20000, out: dsOut(20000, 20000), play: true},
			{ms: 50, rid: rA, core: 313, out: dsOut(0, 313), play: true},
			{ms: 100, rid: rA, core: 600, out: dsOut(0, 600), play: true},
			{ms: 150, rid: rA, core: 914, out: dsOut(0, 914), play: true},
			{ms: 200, rid: rA, core: 914, out: dsOut(0, 1800), play: true},
		},
		"paused-seek-to-start": {
			{ms: 0, rid: rA, core: 10000, out: dsOut(10000, 10000), play: true},
			{ms: 50, rid: rA, core: 10000, out: dsOut(0, 0), play: false},
			{ms: 4000, rid: rA, core: 10000, out: dsOut(0, 0), play: false},
			{ms: 4050, rid: rA, core: 313, out: dsOut(0, 313), play: true},
			{ms: 4100, rid: rA, core: 600, out: dsOut(0, 600), play: true},
			{ms: 4150, rid: rA, core: 914, out: dsOut(0, 914), play: true},
		},
		"waveout-seek": {
			{ms: 0, rid: rA, core: 10000, out: woOut(10000, 0, 0, 0, 0), play: true},
			{ms: 50, rid: rA, core: 30182, out: woOut(30000, 7*tfBlock, 7, 0, 0), play: true},
			{ms: 100, rid: rA, core: 30287, out: woOut(30000, 11*tfBlock, 11, 2, 0), play: true},
		},
		"cross-song-far": {
			{ms: 0, rid: rA, core: 50000, out: dsOut(30000, 50000), play: true},
			{ms: 50, rid: rA, core: 50000, out: dsOut(0, 0), play: true},
			{ms: 100, rid: rB, core: 0, out: dsOut(0, 0), play: true},
		},
		"cross-song-within-2s": {
			{ms: 0, rid: rA, core: 950, out: dsOut(30000, 950), play: true},
			{ms: 50, rid: rA, core: 1000, out: dsOut(0, 0), play: true},
		},
		"cross-song-no-resync": {
			{ms: 0, rid: rA, core: 950, out: dsOut(30000, 950), play: true},
			{ms: 50, rid: rA, core: 1000, out: dsOut(0, 313), play: true},
			{ms: 100, rid: rA, core: 1050, out: dsOut(0, 600), play: true},
			{ms: 150, rid: rA, core: 1100, out: dsOut(0, 800), play: true},
		},
		"independent-raw-history": {
			{ms: 0, rid: rA, core: 950, out: dsOut(5000, 950), play: true},
			{ms: 50, rid: rA, core: 1000, out: dsOut(0, 0), play: true},
		},
		"delayed-ownership": {
			{ms: 0, rid: rA, core: 30000, out: dsOut(30000, 30000), play: true},
			{ms: 50, rid: rA, core: 30050, out: dsOut(0, 0), play: true},
			{ms: 100, rid: rA, core: 313, out: dsOut(0, 313), play: true},
			{ms: 150, rid: rA, core: 600, out: dsOut(0, 600), play: true},
			{ms: 200, rid: rA, core: 914, out: dsOut(0, 914), play: true},
		},
	}
	for name, seq := range seqs {
		t.Run(name, func(t *testing.T) {
			var plain TransientFilter
			wantPf, wantRes := drive(t, &plain, seq...)
			variants := map[string][]swTick{}
			// Output-only ticks between core ticks, showing the next tick's output.
			var inter []swTick
			for i, tk := range seq {
				if i > 0 {
					inter = append(inter, swTick{ms: (seq[i-1].ms + tk.ms) / 2, out: tk.out})
				}
				inter = append(inter, swTick(tk))
			}
			variants["interleaved"] = inter
			// A previous song C before the sequence, so its first tick is a real
			// song change; then the interleaved ticks again.
			before := []swTick{{ms: seq[0].ms - 100, rid: rC, core: 90000, out: dsOut(80000, 90000), play: true}}
			variants["after a previous song"] = append(before, inter...)
			for vname, v := range variants {
				var f TransientFilter
				pfs, res := driveSwitch(t, &f, v...)
				var gotPf []int32
				var gotRes []bool
				for i, tk := range v {
					if tk.rid != "" && tk.rid != rC {
						gotPf, gotRes = append(gotPf, pfs[i]), append(gotRes, res[i])
					}
				}
				if !slices.Equal(gotPf, wantPf) || !slices.Equal(gotRes, wantRes) {
					t.Fatalf("%s: P_f %v resync %v, want %v %v", vname, gotPf, gotRes, wantPf, wantRes)
				}
			}
		})
	}
	t.Run("seek-with-failed-read", func(t *testing.T) {
		var f TransientFilter
		pfs, res := driveSwitch(t, &f,
			swTick{ms: -50, rid: rA, core: 10000, out: dsOut(10000, 10000), play: true},
			swTick{ms: 0, rid: rA, core: 30313, out: dsOut(30000, 500), play: true}, // seek, engaged at the target
			swTick{ms: 100, out: dsOut(30000, 700)},                                 // failed core read: output only
			swTick{ms: 200, rid: rA, core: 30914, out: dsOut(30000, 900), play: true},
		)
		if pfs[1] != 30000 {
			t.Fatalf("seek tick: P_f = %d, want 30000", pfs[1])
		}
		if pfs[3] != 30914 || res[3] {
			t.Fatalf("crossing 200 ms after the previous core tick: P_f %d resync %v, want 30914 passthrough, no resync", pfs[3], res[3])
		}
		if _, ok := f.TakeOpenPass(); ok {
			t.Fatal("an in-song seek reported an opening passthrough")
		}
	})
}

// Design §7 item 5 (counterexample, the session's first song): a filter that saw
// no previous song never takes the tracked crossing, even one observed on failed
// core reads before its first sample (connected at a song start), and reports no
// opening passthrough; the ws/23 Silence shape (connected at 1515 with the stall
// over) passes through tick for tick.
// 变异自证：条件 4 不要求上一首存在，第一例以 t=100 锚定、P_f 不为 1515（实测）；开播段无越阈时以首拍为锚（方案 B），Silence 一例首拍发布 0（实测）。
func TestSwitchStartFirstSongNotTracked(t *testing.T) {
	t.Run("crossing before the first sample", func(t *testing.T) {
		var f TransientFilter
		ticks := fedSeries(0, 0, 50, 300, 100, 900)
		ticks = append(ticks, swTick{ms: 150, rid: rA, core: 1515, out: dsOut(0, 1600), play: true})
		pfs, _ := driveSwitch(t, &f, ticks...)
		if pfs[3] != 1515 {
			t.Fatalf("first song: P_f = %d, want 1515 passthrough", pfs[3])
		}
		if _, ok := f.TakeOpenPass(); ok {
			t.Fatal("the session's first song reported an opening passthrough")
		}
	})
	t.Run("silence connected after the stall", func(t *testing.T) {
		var f TransientFilter
		ticks := []swTick{
			{ms: 0, rid: rA, core: 1515, out: dsOut(0, 1700), play: true},
			{ms: 50, rid: rA, core: 1565, out: dsOut(0, 1750), play: true},
			{ms: 100, rid: rA, core: 1615, out: dsOut(0, 1800), play: true},
		}
		pfs, res := driveSwitch(t, &f, ticks...)
		for i, tk := range ticks {
			if pfs[i] != tk.core || res[i] {
				t.Fatalf("t=%d: P_f %d resync %v, want %d passthrough", tk.ms, pfs[i], res[i], tk.core)
			}
		}
	})
}

// Design §7 item 6 (counterexample): a crossing belongs to its tracked segment.
// After it the output falls again (a new segment) and the next observation is
// more than anchorMaxGap later, already over the threshold: no crossing for the
// segment B opens on, so it degrades.
// 变异自证：新跟踪段不清 crossAt，沿用前一段 t=200 的越阈锚定（实测）。
func TestSwitchStartCrossingBelongsToSegment(t *testing.T) {
	var f TransientFilter
	ticks := prevSong(-100)
	ticks = append(ticks, fedSeries(0, 150000, 50, 0, 100, 300, 150, 600, 200, 900, 250, 1600, 300, 0)...)
	ticks = append(ticks, swTick{ms: 500, rid: rB, core: 1515, out: dsOut(0, 1600), play: true})
	pfs, _ := driveSwitch(t, &f, ticks...)
	if got := pfs[len(pfs)-1]; got != 1515 {
		t.Fatalf("the earlier segment's crossing was used: P_f = %d, want 1515", got)
	}
	if note, ok := f.TakeOpenPass(); !ok || note.Reason != OpenPassNoCrossing {
		t.Fatalf("passthrough note = %+v, %v; want no crossing", note, ok)
	}
}

// Design §7 item 7 (counterexample, condition 4): song A was switched away 300 ms
// after its Play was issued (crossing at t=150, A's core sampled until t=450);
// at t=500 the core reports B while the output is still A's segment, the core
// position slot at 0, 600 or 1515. A's crossing is not taken for B: B's first
// tick passes through as before and reports why. When B's own segment opens at
// t=550 the existing ownership rules (a step back to the ladder is a jump) take
// over and the ladder is filtered.
// 变异自证：去掉条件 4，600、1515 两例在 t=500 以 A 的越阈锚定，P_f = 244（实测）；不等高于起点即报告，0 一例记下降级（实测）；
// 未报告的原因不随段重置清除，0 一例在 B 的段过滤到 50 ms 以上时误报（实测）。
func TestSwitchStartPreviousSongCrossingNotTaken(t *testing.T) {
	for _, first := range []int32{0, 600, 1515} {
		t.Run(strconv.Itoa(int(first)), func(t *testing.T) {
			var f TransientFilter
			ticks := []swTick{
				{ms: -50, rid: rC, core: 90000, out: dsOut(80000, 90000), play: true}, // a song before A
				{ms: 0, rid: rA, core: 0, out: dsOut(0, 0), play: true},
				{ms: 50, rid: rA, core: 313, out: dsOut(0, 313), play: true},
				{ms: 100, rid: rA, core: 600, out: dsOut(0, 600), play: true},
				{ms: 150, rid: rA, core: 1515, out: dsOut(0, 914), play: true}, // A's Play issued
			}
			for ms := int64(200); ms <= 450; ms += 50 {
				ticks = append(ticks, swTick{ms: ms, rid: rA, core: 1515, out: dsOut(0, 1800), play: true})
			}
			next := first
			if first != 0 {
				next = 313
			}
			ticks = append(ticks,
				swTick{ms: 500, rid: rB, core: first, out: dsOut(0, 1850), play: true}, // A's segment still
				swTick{ms: 550, rid: rB, core: next, out: dsOut(0, 0), play: true},     // B's segment opens
				swTick{ms: 600, rid: rB, core: 313, out: dsOut(0, 313), play: true},
				swTick{ms: 650, rid: rB, core: 600, out: dsOut(0, 600), play: true},
				swTick{ms: 700, rid: rB, core: 1515, out: dsOut(0, 914), play: true},
				swTick{ms: 750, rid: rB, core: 1515, out: dsOut(0, 1800), play: true},
				swTick{ms: 800, rid: rB, core: 1515, out: dsOut(0, 1850), play: true},
				swTick{ms: 850, rid: rB, core: 1515, out: dsOut(0, 1900), play: true},
				swTick{ms: 900, rid: rB, core: 1515, out: dsOut(0, 1950), play: true},
			)
			pfs, _ := driveSwitch(t, &f, ticks...)
			change := tickAt(ticks, 500)
			if pfs[change] != first {
				t.Fatalf("B's first tick: P_f = %d, want %d passthrough (A's crossing not taken)", pfs[change], first)
			}
			note, ok := f.TakeOpenPass()
			if first == 0 {
				// The core is at the base: nothing wrong goes out, nothing is logged.
				// B's own segment is then owned only by the existing rules.
				if ok {
					t.Fatalf("passthrough at the base noted: %+v", note)
				}
				return
			}
			if !ok || note.Reason != OpenPassCrossingNotAfter || !note.CrossAt.Equal(time.Unix(100, 0).Add(150*time.Millisecond)) || !note.LastSongAt.Equal(time.Unix(100, 0).Add(450*time.Millisecond)) {
				t.Fatalf("passthrough note = %+v, %v; want crossing at t=150 not after t=450", note, ok)
			}
			for i := change + 1; i < len(ticks); i++ {
				if pfs[i] >= 600 {
					t.Fatalf("t=%d: B's ladder value %d published", ticks[i].ms, pfs[i])
				}
			}
		})
	}
}

// Design §7 item 8 (the cross-song window, review round 2): (a) B's segment
// opens on A's last core tick and fills slowly, crossing during the no-song
// window, 100 ms before B's first sample: taken (H2'a). (b) B's segment crosses
// on A's core ticks (H2'b): not taken, also when the crossing is on A's last tick.
// 变异自证：条件 4 改为比较跟踪段的首次观测时刻（上一版），(a) 发布 1515（实测）；条件 4 用「不早于」，(b) 同拍一例被采用、P_f 44（实测）；去掉条件 4，(b) 两例都被采用、P_f 94 与 44（实测）。
func TestSwitchStartCrossSongWindow(t *testing.T) {
	t.Run("a slow fill across the no-song window", func(t *testing.T) {
		var f TransientFilter
		ticks := []swTick{
			{ms: 0, rid: rA, core: 119950, out: dsOut(0, 120000), play: true},
			{ms: 50, rid: rA, core: 120000, out: dsOut(0, 313), play: true}, // A's last sample; B's segment open
		}
		ticks = append(ticks, fedSeries(100, 600)...)
		for ms := int64(150); ms <= 550; ms += 50 {
			ticks = append(ticks, fedSeries(ms, 700)...)
		}
		ticks = append(ticks, fedSeries(600, 914, 650, 1500)...)
		ticks = append(ticks, swTick{ms: 700, rid: rB, core: 1515, out: dsOut(0, 1600), play: true})
		pfs, res := driveSwitch(t, &f, ticks...)
		if got := pfs[len(pfs)-1]; got != 0 {
			t.Fatalf("slow fill: P_f = %d, want 0 (anchored at t=600)", got)
		}
		if res[len(res)-1] {
			t.Fatal("change tick set TransientResync")
		}
	})
	for _, c := range []struct {
		name    string
		lastA   int64
		crossMs int64
	}{{"crossing before the last sample", 200, 150}, {"crossing on the last sample", 150, 150}} {
		t.Run("b "+c.name, func(t *testing.T) {
			var f TransientFilter
			ticks := []swTick{
				{ms: 0, rid: rA, core: 1000, out: dsOut(30000, 1000), play: true},
				{ms: 50, rid: rA, core: 1050, out: dsOut(0, 313), play: true},
				{ms: 100, rid: rA, core: 1100, out: dsOut(0, 600), play: true},
			}
			for ms := int64(150); ms <= c.lastA; ms += 50 {
				fed := int64(800)
				if ms > c.crossMs {
					fed = 1000
				}
				ticks = append(ticks, swTick{ms: ms, rid: rA, core: int32(1000 + ms), out: dsOut(0, fed), play: true})
			}
			ticks = append(ticks, fedSeries(c.lastA+50, 1500, c.lastA+100, 1550)...)
			ticks = append(ticks, swTick{ms: c.lastA + 150, rid: rB, core: 1515, out: dsOut(0, 1600), play: true})
			pfs, _ := driveSwitch(t, &f, ticks...)
			if got := pfs[len(pfs)-1]; got != 1515 {
				t.Fatalf("crossing at t=%d with A sampled until t=%d: P_f = %d, want 1515", c.crossMs, c.lastA, got)
			}
			note, ok := f.TakeOpenPass()
			if !ok || note.Reason != OpenPassCrossingNotAfter {
				t.Fatalf("passthrough note = %+v, %v; want crossing not after the last sample", note, ok)
			}
		})
	}
}

// Design §7 item 9 (review round 1, item 3): B is announced at 0 before its Play;
// the core reads then fail across the crossing, which only the tracking sees. On
// the recovery tick the per-tick anchor fails (200 ms between core ticks) and the
// tracked one is taken; this is not the change tick, so TransientResync is set
// for the session to judge the front end, which has run 300 ms from 0.
// 变异自证：跟踪锚点一律不置 resync，t=300 resync 为假（实测）。
func TestSwitchStartDelayedTrackedAnchorResyncs(t *testing.T) {
	var f TransientFilter
	ticks := []swTick{
		{ms: -300, rid: rA, core: 50000, out: dsOut(40000, 10000), play: true},
		{ms: 0, rid: rB, core: 0, out: dsOut(0, 0), play: true},
		{ms: 50, rid: rB, core: 313, out: dsOut(0, 313), play: true},
		{ms: 100, rid: rB, core: 600, out: dsOut(0, 600), play: true},
		{ms: 150, out: dsOut(0, 700)},
		{ms: 200, out: dsOut(0, 914)},
		{ms: 250, out: dsOut(0, 1600)},
		{ms: 300, rid: rB, core: 1515, out: dsOut(0, 1600), play: true},
	}
	pfs, res := driveSwitch(t, &f, ticks...)
	if pfs[1] != 0 || pfs[2] != 0 || pfs[3] != 0 {
		t.Fatalf("before the crossing: P_f %v, want 0", pfs[1:4])
	}
	if pfs[7] != 0 || !res[7] {
		t.Fatalf("recovery tick: P_f %d resync %v, want 0 and TransientResync", pfs[7], res[7])
	}
	if n := countTrue(res); n != 1 {
		t.Fatalf("%d TransientResync, want 1", n)
	}
}

// Design §7 item 11 (counterexample, long poll): with an effective poll above
// anchorMaxGap the opening degrades even with a tracked crossing, as before, and
// reports the long poll.
// 变异自证：先查跟踪再判长轮询，P_f 不为 1515（实测）。
func TestSwitchStartLongPollDegrades(t *testing.T) {
	var f TransientFilter
	ticks := prevSong(-100)
	ticks = append(ticks, fedSeries(0, 150000, 50, 0, 100, 300, 150, 600, 200, 900, 250, 1600)...)
	ticks = append(ticks, swTick{ms: 400, rid: rB, core: 1515, out: dsOut(0, 1700), play: true, ival: time.Second})
	pfs, _ := driveSwitch(t, &f, ticks...)
	if got := pfs[len(pfs)-1]; got != 1515 {
		t.Fatalf("long poll: P_f = %d, want 1515", got)
	}
	if note, ok := f.TakeOpenPass(); !ok || note.Reason != OpenPassLongPoll {
		t.Fatalf("passthrough note = %+v, %v; want long poll", note, ok)
	}
}

// Design §7 item 12 (counterexample, paused; code review round 1, item 1): B's
// crossing is tracked, then B comes up paused, its first tick passing the stalled
// 1515 through. On resumption the tracked anchor is not taken after that: with a
// short pause (resumed at t=600) its line, 294 ms, lies below the 1515 already
// published, and taking it would step the public position back (a
// playback_resume); with a long one (t=2400) it lies above the core and would not
// engage anyway. Both degrade and pass the core through, as before #198, and the
// passthrough is reported once, as one that already went out above the base.
// (Code review round 2) The same holds when the value above the base went out on
// a tick whose output read failed: B comes up paused at 0, the next paused tick
// passes 1515 through with the output invalid, and the resumption at t=600 on the
// same segment does not take the anchor either.
// 变异自证：采用跟踪锚点不看此前是否已直通高于起点的值，短暂停一例恢复拍 P_f 294（实测）；pending 直通不记下此事，同红（实测）；
// 原因不区分（按「越阈不晚于上一首」记），三例原因断言红（实测）；暂停拍也启用（去掉 pending 的 playing 门槛），暂停拍发布 0（实测）；
// 输出无效拍直通高于起点时不记下此事，输出无效一例恢复拍 P_f 294（实测）。
func TestSwitchStartPausedAfterCrossing(t *testing.T) {
	t.Run("output invalid", func(t *testing.T) {
		var f TransientFilter
		ticks := prevSong(-100)
		ticks = append(ticks, fedSeries(0, 150000, 50, 0, 100, 300, 150, 600, 200, 900, 250, 1600)...)
		ticks = append(ticks,
			swTick{ms: 400, rid: rB, core: 0, out: dsOut(0, 1700), play: false},
			swTick{ms: 450, rid: rB, core: 1515, out: OutputState{}, play: false},
			swTick{ms: 600, rid: rB, core: 1515, out: dsOut(0, 1700), play: true},
			swTick{ms: 650, rid: rB, core: 1515, out: dsOut(0, 1750), play: true},
		)
		pfs, res := driveSwitch(t, &f, ticks...)
		want := []int32{0, 1515, 1515, 1515}
		for i, w := range want {
			j := len(pfs) - len(want) + i
			if pfs[j] != w || res[j] {
				t.Fatalf("t=%d: P_f %d resync %v, want %d, no resync", ticks[j].ms, pfs[j], res[j], w)
			}
		}
		note, ok := f.TakeOpenPass()
		if !ok || note.Reason != OpenPassPassedAbove {
			t.Fatalf("passthrough note = %+v, %v; want passed above the base", note, ok)
		}
	})
	for _, resumeMs := range []int64{600, 2400} {
		t.Run(strconv.Itoa(int(resumeMs)), func(t *testing.T) {
			var f TransientFilter
			ticks := prevSong(-100)
			ticks = append(ticks, fedSeries(0, 150000, 50, 0, 100, 300, 150, 600, 200, 900, 250, 1600)...)
			ticks = append(ticks,
				swTick{ms: 400, rid: rB, core: 1515, out: dsOut(0, 1700), play: false},
				swTick{ms: resumeMs, rid: rB, core: 1515, out: dsOut(0, 1700), play: true},
				swTick{ms: resumeMs + 50, rid: rB, core: 1515, out: dsOut(0, 1750), play: true},
			)
			pfs, res := driveSwitch(t, &f, ticks...)
			for i := len(pfs) - 3; i < len(pfs); i++ {
				if pfs[i] != 1515 || res[i] {
					t.Fatalf("t=%d: P_f %d resync %v, want 1515 passthrough", ticks[i].ms, pfs[i], res[i])
				}
			}
			note, ok := f.TakeOpenPass()
			if !ok || note.Reason != OpenPassPassedAbove || !note.CrossAt.Equal(time.Unix(100, 0).Add(200*time.Millisecond)) {
				t.Fatalf("passthrough note = %+v, %v; want passed above the base, crossing at t=200", note, ok)
			}
		})
	}
}

// Code review round 1, item 2: an opening that degrades while the core is still at
// its base keeps the reason unreported and reports it on the segment's first
// published value above the base. Here a long poll degrades B's first tick
// (core 0); the next tick passes 1515 through and reports the long poll.
// 变异自证：核心在起点时不保留原因（直接丢弃），第二拍取不到记录（实测）；不等高于起点即报告，第一拍就取到（实测）。
func TestSwitchStartDeferredPassReported(t *testing.T) {
	var f TransientFilter
	ticks := prevSong(-100)
	driveSwitch(t, &f, ticks...)
	driveSwitch(t, &f, swTick{ms: 0, rid: rB, core: 0, out: dsOut(0, 0), play: true, ival: time.Second})
	if note, ok := f.TakeOpenPass(); ok {
		t.Fatalf("reported while the core is at the base: %+v", note)
	}
	pfs, _ := driveSwitch(t, &f, swTick{ms: 1000, rid: rB, core: 1515, out: dsOut(0, 1800), play: true, ival: time.Second})
	if pfs[0] != 1515 {
		t.Fatalf("long poll: P_f = %d, want 1515 passthrough", pfs[0])
	}
	if note, ok := f.TakeOpenPass(); !ok || note.Reason != OpenPassLongPoll {
		t.Fatalf("passthrough note = %+v, %v; want the long poll, on the first value above the base", note, ok)
	}
}

// TakeOpenPass reports a passthrough once.
// 变异自证：TakeOpenPass 不清除，第二次仍取到（实测）。
func TestSwitchStartOpenPassTakenOnce(t *testing.T) {
	var f TransientFilter
	ticks := prevSong(-100)
	ticks = append(ticks, swTick{ms: 0, rid: rB, core: 1515, out: dsOut(0, 1700), play: true},
		swTick{ms: 50, rid: rB, core: 1515, out: dsOut(0, 1750), play: true})
	driveSwitch(t, &f, ticks...)
	if _, ok := f.TakeOpenPass(); !ok {
		t.Fatal("no passthrough note")
	}
	if _, ok := f.TakeOpenPass(); ok {
		t.Fatal("passthrough note taken twice")
	}
}
