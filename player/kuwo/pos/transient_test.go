package pos

import (
	"testing"
	"time"
)

// Synthetic coverage of the #135 transient filter against design §4. Every
// counterexample is a constructed Sample/OutputState sequence (no CSV fixture);
// representative WaveOut magnitudes are taken from findings §6.4 run11 and
// written inline. Each decision has a case that turns red if the rule is
// removed; the mutation list is in the task report.

const tbps = 176400 // 44.1 kHz, 16-bit, stereo: bytes per second

// tfBytes converts a fed duration in ms to the byte count the filter reads.
func tfBytes(ms int64) uint32 { return uint32(ms * tbps / 1000) }

const tfBlock = 4 * 1152 // one WaveOut header / fed block, blkAlign 4

func dsOut(base uint32, fedMs int64) OutputState {
	return OutputState{Valid: true, Device: DeviceDirectSound, Base: base, Bps: tbps, Fed: tfBytes(fedMs)}
}

func woOut(base uint32, fed uint32, hdrW, hdrD uint32, staging uint32) OutputState {
	return OutputState{Valid: true, Device: DeviceWaveOut, Base: base, Bps: tbps, Fed: fed, HdrW: hdrW, HdrD: hdrD, BlkAlign: 4, Staging: staging}
}

type tick struct {
	ms   int64
	rid  string
	core int32
	out  OutputState
	play bool
	ival time.Duration
}

func drive(t *testing.T, f *TransientFilter, ticks ...tick) ([]int32, []bool) {
	t.Helper()
	base := time.Unix(100, 0)
	var pfs []int32
	var res []bool
	for i, tk := range ticks {
		iv := tk.ival
		if iv == 0 {
			iv = 50 * time.Millisecond
		}
		s := Sample{RID: tk.rid, PositionMs: tk.core, DurationMs: 600000, ObservedAt: base.Add(time.Duration(tk.ms) * time.Millisecond)}
		s.CoreMs = s.PositionMs
		o, r := f.Apply(s, tk.out, tk.play, iv)
		if o.CoreMs != tk.core {
			t.Fatalf("tick %d: CoreMs changed: got %d want %d", i, o.CoreMs, tk.core)
		}
		pfs = append(pfs, o.PositionMs)
		res = append(res, r)
	}
	return pfs, res
}

func countTrue(b []bool) int {
	n := 0
	for _, v := range b {
		if v {
			n++
		}
	}
	return n
}

const rA = "MUSIC_250413912"
const rB = "MUSIC_28649137"

// --- DS method A -----------------------------------------------------------

// Open (base 0, RID change): the public value walks from 0, never the +1515
// multi-report; resync fires exactly once, on the "Play issued" tick.
func TestTransientDSOpen(t *testing.T) {
	var f TransientFilter
	var ticks []tick
	// t0..t150: 0 / 313 / 600 / 1515 ladder; fed reaches 750 ms at t150.
	ladder := []struct {
		ms    int64
		core  int32
		fedMs int64
	}{
		{0, 0, 0}, {50, 313, 313}, {100, 600, 600}, {150, 1515, 914},
	}
	for _, l := range ladder {
		ticks = append(ticks, tick{ms: l.ms, rid: rA, core: l.core, out: dsOut(0, l.fedMs), play: true})
	}
	// Stall at 1515 while the candidate line ramps from the anchor.
	for ms := int64(200); ms <= 1700; ms += 50 {
		ticks = append(ticks, tick{ms: ms, rid: rA, core: 1515, out: dsOut(0, 1800), play: true})
	}
	pfs, res := drive(t, &f, ticks...)
	if countTrue(res) != 1 {
		t.Fatalf("resync count = %d, want 1", countTrue(res))
	}
	if !res[3] {
		t.Fatalf("resync not on the Play-issued tick (t150)")
	}
	for i, pf := range pfs {
		if pf >= 1515 {
			t.Fatalf("tick %d published the multi-report value %d", i, pf)
		}
		if pf < 0 {
			t.Fatalf("tick %d negative P_f %d", i, pf)
		}
	}
	// The ramp must be climbing by the end of the stall (not stuck at 0).
	if pfs[len(pfs)-1] < 1000 {
		t.Fatalf("candidate line did not ramp: last P_f = %d", pfs[len(pfs)-1])
	}
}

// Seek (base = target): P_f is pinned to the target before Play, then rides the
// ramp; the core catching the uncapped line disengages it.
func TestTransientDSSeek(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 5000, out: dsOut(5000, 5000), play: true}, // steady, establishes RID history
		{ms: 50, rid: rA, core: 30313, out: dsOut(30000, 313), play: true},
		{ms: 100, rid: rA, core: 30600, out: dsOut(30000, 600), play: true},
		{ms: 150, rid: rA, core: 30914, out: dsOut(30000, 914), play: true}, // Play issued
		{ms: 200, rid: rA, core: 30914, out: dsOut(30000, 1800), play: true},
	}
	pfs, res := drive(t, &f, ticks...)
	// Before Play the target is published, not +313/+600.
	if pfs[1] != 30000 || pfs[2] != 30000 {
		t.Fatalf("pre-Play P_f = %d/%d, want 30000/30000", pfs[1], pfs[2])
	}
	if !res[3] {
		t.Fatalf("resync not on the Play-issued tick")
	}
	if pfs[3] > 30000 {
		t.Fatalf("P_f %d exceeded base on the Play tick", pfs[3])
	}
}

// Seek to 0:00 with the RID unchanged: the filter's own jump test owns the
// segment (contrast the cross-song residual case).
func TestTransientDSSeekToZero(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 60000, out: dsOut(60000, 60000), play: true},
		{ms: 50, rid: rA, core: 313, out: dsOut(0, 313), play: true}, // jumped back to 0
		{ms: 100, rid: rA, core: 600, out: dsOut(0, 600), play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[1] != 0 || pfs[2] != 0 {
		t.Fatalf("seek-to-zero P_f = %d/%d, want 0/0", pfs[1], pfs[2])
	}
}

// 135-136: the stop test compares the uncapped candidate line, not the capped
// P_f. If it used P_f the first multi-report tick would disengage and leak the
// ladder.
func TestTransientDSStopUsesCand(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 0, out: dsOut(0, 0), play: true},
		{ms: 50, rid: rA, core: 313, out: dsOut(0, 313), play: true},
		{ms: 100, rid: rA, core: 600, out: dsOut(0, 600), play: true},
		{ms: 150, rid: rA, core: 1515, out: dsOut(0, 914), play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	// Every multi-report tick stays filtered at the candidate line, far below
	// the core value: proves no premature disengage.
	for i := 1; i < len(pfs); i++ {
		if pfs[i] >= int32(ticks[i].core) {
			t.Fatalf("tick %d published core %d (premature disengage)", i, pfs[i])
		}
	}
}

// 135-145: slow decode. Play is late (t=400); before Play cand = base, so P_f
// never exceeds base + max(0, t - t_seen - L). A t_det+K prediction would break
// the bound.
func TestTransientDSSlowDecodeCapsBase(t *testing.T) {
	transientLDSSeek = 44 * time.Millisecond
	var f TransientFilter
	base := int64(30000)
	ticks := []tick{{ms: 0, rid: rA, core: 10000, out: dsOut(10000, 10000), play: true}}
	// Seek at t50, Play delayed: fed stays < 750 ms until t400.
	stalls := []struct {
		ms    int64
		core  int32
		fedMs int64
	}{
		{50, 30313, 313}, {100, 30600, 600}, {150, 30914, 700}, {200, 30914, 700},
		{250, 30914, 720}, {300, 30914, 730}, {350, 30914, 740}, {400, 30914, 914},
	}
	for _, s := range stalls {
		ticks = append(ticks, tick{ms: s.ms, rid: rA, core: s.core, out: dsOut(uint32(base), s.fedMs), play: true})
	}
	// Ramp after Play.
	for ms := int64(450); ms <= 900; ms += 50 {
		ticks = append(ticks, tick{ms: ms, rid: rA, core: 30914, out: dsOut(uint32(base), 1800), play: true})
	}
	pfs, res := drive(t, &f, ticks...)
	tSeen := int64(400)
	L := int64(44)
	resyncAt := -1
	for i, r := range res {
		if r {
			resyncAt = i
		}
	}
	if resyncAt < 0 || ticks[resyncAt].ms != 400 {
		t.Fatalf("Play not detected at t=400 (resync at index %d)", resyncAt)
	}
	for i, pf := range pfs {
		tms := ticks[i].ms
		bound := base
		if d := tms - tSeen - L; d > 0 {
			bound = base + d
		}
		if int64(pf) > bound {
			t.Fatalf("tick %d (t=%d): P_f %d exceeds slow-decode bound %d", i, tms, pf, bound)
		}
	}
}

// 135-late-play-anchor: a long effective poll (> anchorMaxGap) degrades the DS
// segment to passthrough; a fresh 50 ms poll on the same shape filters.
func TestTransientDSLongPollDegrade(t *testing.T) {
	long := []tick{
		{ms: 0, rid: rA, core: 0, out: dsOut(0, 0), play: true, ival: time.Second},
		{ms: 1000, rid: rA, core: 1515, out: dsOut(0, 1800), play: true, ival: time.Second},
		{ms: 2000, rid: rA, core: 3000, out: dsOut(0, 3000), play: true, ival: time.Second},
	}
	var f1 TransientFilter
	pfs, res := drive(t, &f1, long...)
	for i, pf := range pfs {
		if pf != int32(long[i].core) {
			t.Fatalf("long poll tick %d filtered (%d != core %d)", i, pf, long[i].core)
		}
	}
	if countTrue(res) != 0 {
		t.Fatalf("long poll produced %d resync", countTrue(res))
	}
	// First filterable tick already at/over the fed threshold → degrade too.
	early := []tick{
		{ms: 0, rid: rB, core: 0, out: dsOut(0, 0), play: true},
		{ms: 50, rid: rB, core: 1515, out: dsOut(0, 900), play: true}, // fed already ≥ 750 at first multi-report...
	}
	// Prepend so the first filterable tick (t0) has fed ≥ 750.
	early[0].out = dsOut(0, 800)
	early[0].core = 1515
	var f2 TransientFilter
	pfs2, res2 := drive(t, &f2, early...)
	for i, pf := range pfs2 {
		if pf != int32(early[i].core) {
			t.Fatalf("stale-crossing tick %d filtered (%d != core %d)", i, pf, early[i].core)
		}
	}
	if countTrue(res2) != 0 {
		t.Fatalf("stale crossing produced resync")
	}
}

// 135-pause-passthrough: a pause mid-filter freezes the candidate line and P_f,
// emits no resync, and does not switch to passthrough (which would republish the
// multi-report).
func TestTransientDSPauseFreeze(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 30000, out: dsOut(30000, 30000), play: true},
		{ms: 50, rid: rA, core: 30313, out: dsOut(30000, 313), play: true},
		{ms: 100, rid: rA, core: 30600, out: dsOut(30000, 600), play: true},
		{ms: 150, rid: rA, core: 30914, out: dsOut(30000, 914), play: true}, // Play issued, filtering
		{ms: 200, rid: rA, core: 30914, out: dsOut(30000, 1800), play: true},
		// Pause: core would read a frozen multi-report value.
		{ms: 250, rid: rA, core: 30914, out: dsOut(30000, 1800), play: false},
		{ms: 2000, rid: rA, core: 30914, out: dsOut(30000, 1800), play: false},
	}
	pfs, res := drive(t, &f, ticks...)
	frozen := pfs[4]
	if pfs[5] != frozen || pfs[6] != frozen {
		t.Fatalf("pause did not freeze P_f: %d then %d/%d", frozen, pfs[5], pfs[6])
	}
	if frozen >= 30914 {
		t.Fatalf("pause published the multi-report value %d", frozen)
	}
	if res[5] || res[6] {
		t.Fatalf("pause ticks emitted resync")
	}
}

// 135-120: Play is judged from fed ≥ 750 ms (not the a87c window), so coarse
// polling that overshoots a87c still detects it — see TestTransientDSOpen /
// TestTransientDSSeek for the positive fresh-crossing case. Here a run of
// invalid reads interrupts the filtering segment; per the whole-branch final
// review (135-device-degrade) an invalid read while filtering degrades the
// segment, so the recovery tick passes the core through rather than re-anchoring
// Play and re-capping below the already-exposed overshoot. No resync, no re-pin.
func TestTransientDSPlayByFed(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 0, out: dsOut(0, 0), play: true},
		{ms: 50, rid: rA, core: 313, out: dsOut(0, 313), play: true}, // engage filtering (pre-Play, capped to 0)
		// invalid read (global block failure) for two ticks: segment degrades
		{ms: 100, rid: rA, core: 600, out: OutputState{}, play: true},
		{ms: 150, rid: rA, core: 1515, out: OutputState{}, play: true},
		// fed recovers already ≥ 750, but the segment is degraded: passthrough
		{ms: 200, rid: rA, core: 1515, out: dsOut(0, 914), play: true},
	}
	pfs, res := drive(t, &f, ticks...)
	if countTrue(res) != 0 {
		t.Fatalf("degraded segment set a resync across invalid reads: %v", res)
	}
	if pfs[2] != 600 || pfs[3] != 1515 || pfs[4] != 1515 {
		t.Fatalf("invalid-read/recovery not passthrough after degrade: %v", pfs)
	}
}

// Pause during the stall after Play: t_seen shifts by the pause so the ramp
// resumes where it froze, not ahead of the sound.
func TestTransientDSPauseDuringStall(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 0, out: dsOut(0, 0), play: true},
		{ms: 50, rid: rA, core: 1515, out: dsOut(0, 914), play: true}, // Play issued
		{ms: 100, rid: rA, core: 1515, out: dsOut(0, 1800), play: true},
		{ms: 150, rid: rA, core: 1515, out: dsOut(0, 1800), play: false}, // pause
		{ms: 2000, rid: rA, core: 1515, out: dsOut(0, 1800), play: false},
		{ms: 2050, rid: rA, core: 1515, out: dsOut(0, 1800), play: true}, // resume
	}
	pfs, _ := drive(t, &f, ticks...)
	resumed := pfs[5]
	frozen := pfs[3]
	// After a ~1.9 s pause the ramp must not have jumped far ahead: resume value
	// close to the frozen one (anchor shifted by the pause).
	if resumed-frozen > 300 {
		t.Fatalf("ramp ran ahead across pause: frozen %d resumed %d", frozen, resumed)
	}
}

// New Flush while still filtering starts a new segment and resets t_det.
func TestTransientDSReseekDuringFilter(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 0, out: dsOut(0, 0), play: true},
		{ms: 50, rid: rA, core: 313, out: dsOut(0, 313), play: true}, // filtering seg 1
		{ms: 100, rid: rA, core: 40313, out: dsOut(40000, 313), play: true}, // new base: seg 2
		{ms: 150, rid: rA, core: 40600, out: dsOut(40000, 600), play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[2] != 40000 {
		t.Fatalf("re-seek engage P_f = %d, want 40000 (new base)", pfs[2])
	}
}

// 8 s backstop: Play never issues (fed held below the threshold) so the
// candidate line stays pinned at base and P_f is pinned to base while the core
// is frozen at a banded multi-report value. Without the backstop P_f would stay
// at base forever; the 8 s fallback disengages and passes the core through. A
// frozen core at the top of a short ladder would not distinguish the backstop,
// because once the ramp overtakes it min(C_raw, cand) already yields the core;
// pinning cand at base keeps C_raw above it for the whole 8 s.
func TestTransientDSFallback(t *testing.T) {
	transientFallback = 8 * time.Second
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 0, out: dsOut(0, 0), play: true},
		{ms: 50, rid: rA, core: 1900, out: dsOut(0, 300), play: true}, // engage; fed < 750, no Play
	}
	// Core frozen at 1900 (in band), fed held below the Play threshold: cand stays
	// at base, P_f pinned at 0 until the 8 s backstop fires.
	for ms := int64(100); ms <= 8100; ms += 500 {
		ticks = append(ticks, tick{ms: ms, rid: rA, core: 1900, out: dsOut(0, 700), play: true})
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[2] != 0 {
		t.Fatalf("pre-fallback P_f = %d, want 0 (pinned at base)", pfs[2])
	}
	if pfs[len(pfs)-1] != 1900 {
		t.Fatalf("fallback did not pass core through after 8 s: last P_f %d", pfs[len(pfs)-1])
	}
}

// owned established by base > maxOvershoot: an in-song seek to a high base whose
// output arrives before the kernel jump, with the RID unchanged and the raw step
// too small to be a jump. Only the base > maxOvershoot branch can own the
// segment, so P_f is pinned to the target instead of passing the stale core
// through.
func TestTransientOwnedByHighBase(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		// Steady at 36 s, establishes the RID history (its fed ≥ 750 degrades this
		// segment, but the seek below resets it).
		{ms: 0, rid: rA, core: 36000, out: dsOut(36000, 36000), play: true},
		// Seek to 35 s: output base is already 35000 and fed flushed, but the core
		// still reads 36050 (kernel not yet jumped). Step 50 is not a jump and the
		// RID is unchanged, so only base 35000 > maxOvershoot owns the segment.
		{ms: 50, rid: rA, core: 36050, out: dsOut(35000, 0), play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[1] != 35000 {
		t.Fatalf("high-base ownership: P_f = %d, want 35000 (pinned to target)", pfs[1])
	}
}

// band upper bound: once owned, a multi-report beyond maxOvershoot (2000 ms) is
// not a recognised transient and must pass through, not be squashed to base.
func TestTransientBandUpperBound(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 10000, out: dsOut(10000, 10000), play: true}, // establishes history
		// Seek to 20 s; the core reads 25000 — a 5000 ms overshoot, beyond
		// maxOvershoot. owned is set (jump and base both qualify) but the band
		// fails, so the implausible value passes through.
		{ms: 50, rid: rA, core: 25000, out: dsOut(20000, 0), play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[1] != 25000 {
		t.Fatalf("band upper bound: P_f = %d, want 25000 (passthrough)", pfs[1])
	}
}

// L too small / too large: either way P_f stays within [0, C_raw].
func TestTransientDSLBounds(t *testing.T) {
	for _, L := range []time.Duration{0, 300 * time.Millisecond} {
		transientLDSOpen = L
		var f TransientFilter
		var ticks []tick
		ticks = append(ticks, tick{ms: 0, rid: rA, core: 0, out: dsOut(0, 0), play: true})
		ticks = append(ticks, tick{ms: 50, rid: rA, core: 1515, out: dsOut(0, 914), play: true})
		for ms := int64(100); ms <= 1700; ms += 50 {
			ticks = append(ticks, tick{ms: ms, rid: rA, core: 1515, out: dsOut(0, 1800), play: true})
		}
		pfs, _ := drive(t, &f, ticks...)
		for i, pf := range pfs {
			if pf < 0 || int64(pf) > int64(ticks[i].core) {
				t.Fatalf("L=%v tick %d: P_f %d outside [0, %d]", L, i, pf, ticks[i].core)
			}
		}
	}
	transientLDSOpen = 106 * time.Millisecond
}

// --- WaveOut method B ------------------------------------------------------

// Per-tick cand from header counts (run11 magnitudes): before any header is
// done played = 0 (cand = base); as hdr_d grows, played tracks it.
func TestTransientWaveOutPerTick(t *testing.T) {
	var f TransientFilter
	// base 0; fed grows by whole blocks, hdr_w tracks fed, hdr_d lags (device
	// not yet consuming), then hdr_d catches up.
	ticks := []tick{
		{ms: 0, rid: rA, core: 0, out: woOut(0, 0, 0, 0, 0), play: true},
		{ms: 50, rid: rA, core: 52, out: woOut(0, 2*tfBlock, 2, 0, 0), play: true}, // played 0 → cand 0
		{ms: 100, rid: rA, core: 313, out: woOut(0, 12*tfBlock, 12, 0, 0), play: true}, // played 0 → cand 0
		{ms: 150, rid: rA, core: 313, out: woOut(0, 12*tfBlock, 12, 3, 0), play: true}, // played 3 blocks → 78
		{ms: 200, rid: rA, core: 313, out: woOut(0, 12*tfBlock, 12, 6, 0), play: true}, // played 6 → 156
	}
	pfs, res := drive(t, &f, ticks...)
	want := []int32{0, 0, 0, 78, 156}
	for i := range want {
		if pfs[i] != want[i] {
			t.Fatalf("WaveOut tick %d: P_f %d want %d", i, pfs[i], want[i])
		}
	}
	if countTrue(res) != 0 {
		t.Fatalf("WaveOut set TransientResync")
	}
}

// Extra header before sound (+26 ms): fed holds one header more than hdr_w
// counts, so cand is one block high but stays ≤ C_raw.
func TestTransientWaveOutExtraHeader(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 0, out: woOut(0, 0, 0, 0, 0), play: true},
		// hdr_w=3, hdr_d=0, but fed already holds 4 blocks (callback filled ahead
		// of inc[+0x38]). queue=3 blocks, played=1 block=26 ms.
		{ms: 50, rid: rA, core: 104, out: woOut(0, 4*tfBlock, 3, 0, 0), play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[1] != 26 {
		t.Fatalf("extra-header cand = %d, want 26", pfs[1])
	}
	if int64(pfs[1]) > int64(ticks[1].core) {
		t.Fatalf("extra-header cand %d exceeded core %d", pfs[1], ticks[1].core)
	}
}

// Effect ratio: scaled only when EffectOn and ratio != 1.0 (and > 0); ratio ==
// 1.0 (the run11 const case) is a no-op.
func TestTransientWaveOutEffectScale(t *testing.T) {
	// queue bytes before scale: (hdr_w-hdr_d)=4 blocks + staging 0.
	raw := woOut(0, 20*tfBlock, 10, 6, 0)
	// ratio 1.0, EffectOn: no scale → played = 20-4 = 16 blocks.
	on1 := raw
	on1.EffectOn, on1.EffectRatio = true, 1.0
	// ratio 0.5, EffectOn: queue halved → played = 20-2 = 18 blocks (larger cand).
	half := raw
	half.EffectOn, half.EffectRatio = true, 0.5
	var fa, fb, fc TransientFilter
	// C_raw = the multi-report value (base + fed), within the band and above
	// both candidate lines so min() does not mask the scale difference.
	const cRaw = 522 // 20 blocks of fed, in ms
	open := tick{ms: 0, rid: rA, core: 0, out: woOut(0, 0, 0, 0, 0), play: true}
	pa, _ := drive(t, &fa, open, tick{ms: 50, rid: rA, core: cRaw, out: raw, play: true})
	pb, _ := drive(t, &fb, open, tick{ms: 50, rid: rA, core: cRaw, out: on1, play: true})
	pc, _ := drive(t, &fc, open, tick{ms: 50, rid: rA, core: cRaw, out: half, play: true})
	if pa[1] != pb[1] {
		t.Fatalf("ratio 1.0 changed cand: off %d on %d", pa[1], pb[1])
	}
	if pc[1] <= pb[1] {
		t.Fatalf("ratio 0.5 did not raise played: %d vs %d", pc[1], pb[1])
	}
}

// WaveOut field invalid (hdr_d > hdr_w, or blkAlign 0): degrade for the segment,
// do not fall back to A.
func TestTransientWaveOutFieldInvalid(t *testing.T) {
	for _, bad := range []OutputState{
		woOut(0, 10*tfBlock, 3, 5, 0), // hdr_d > hdr_w
		{Valid: true, Device: DeviceWaveOut, Base: 0, Bps: tbps, Fed: 10 * tfBlock, HdrW: 10, HdrD: 2, BlkAlign: 0},
	} {
		var f TransientFilter
		ticks := []tick{
			{ms: 0, rid: rA, core: 0, out: bad, play: true},
			{ms: 50, rid: rA, core: 500, out: bad, play: true},
		}
		pfs, _ := drive(t, &f, ticks...)
		for i, pf := range pfs {
			if pf != int32(ticks[i].core) {
				t.Fatalf("bad WaveOut field: tick %d filtered (%d != %d)", i, pf, ticks[i].core)
			}
		}
	}
}

// --- attribution & song change --------------------------------------------

// 135-114: cross-song window, old song far from the new base. band fails, so the
// old position is passed through, not squashed toward 0.
func TestTransientCrossSongFarFromBase(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 50000, out: dsOut(30000, 50000), play: true},
		{ms: 50, rid: rA, core: 50000, out: dsOut(0, 0), play: true}, // output opened new song, core still old
		{ms: 100, rid: rB, core: 0, out: dsOut(0, 0), play: true},    // core switches
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[1] != 50000 {
		t.Fatalf("cross-song far: P_f = %d, want 50000 passthrough", pfs[1])
	}
}

// 135-114 residual: old song inside the first 2 s, new base 0, core advancing
// (no jump, RID unchanged) → owned stays false, no filtering.
func TestTransientCrossSongWithinTwoSeconds(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 950, out: dsOut(30000, 950), play: true},
		{ms: 50, rid: rA, core: 1000, out: dsOut(0, 0), play: true}, // new song opened, core still old, advancing
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[1] != 1000 {
		t.Fatalf("cross-song within 2 s: P_f = %d, want 1000 (owned false)", pfs[1])
	}
}

// A cross-song window that lasts long enough for the new song's DS fed to cross
// the "Play issued" threshold while the kernel RID still lags on the old song:
// owned stays false (no RID change, no jump, base within the band ceiling), so
// the segment passes through AND emits no resync. Play detection is gated on
// owned; without that gate the fed crossing at t150 would set TransientResync on
// the old song and risk an extra playback_resume.
func TestTransientCrossSongNoResyncWhileUnowned(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 950, out: dsOut(30000, 950), play: true},
		{ms: 50, rid: rA, core: 1000, out: dsOut(0, 313), play: true}, // new song opened, core still old, advancing
		{ms: 100, rid: rA, core: 1050, out: dsOut(0, 600), play: true},
		{ms: 150, rid: rA, core: 1100, out: dsOut(0, 800), play: true}, // fed crosses 750 while unowned
	}
	pfs, res := drive(t, &f, ticks...)
	if n := countTrue(res); n != 0 {
		t.Fatalf("unowned cross-song emitted %d resync, want 0", n)
	}
	want := []int32{950, 1000, 1050, 1100}
	for i := range want {
		if pfs[i] != want[i] {
			t.Fatalf("tick %d: P_f = %d, want %d passthrough (owned false)", i, pfs[i], want[i])
		}
	}
}

// 135-raw-clock-dependency: a 50 ms forward step is below the jump threshold on
// the filter's own raw history, so owned stays false even though a corrected
// (session) clock would mis-judge it as a jump.
func TestTransientIndependentRawHistory(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 950, out: dsOut(5000, 950), play: true},
		{ms: 50, rid: rA, core: 1000, out: dsOut(0, 0), play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[1] != 1000 {
		t.Fatalf("raw-history step mis-judged as jump: P_f = %d, want 1000", pfs[1])
	}
}

// 135-pending-ownership: WaveOut open, owned set by the RID change on t0 and
// kept; engage on t50 publishes 0 even though the step from t0 is tiny.
func TestTransientPendingOwnershipKept(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rB, core: 0, out: woOut(0, 0, 0, 0, 0), play: true}, // RID change → open, owned true
		{ms: 50, rid: rB, core: 182, out: woOut(0, 7*tfBlock, 7, 0, 0), play: true},
		{ms: 100, rid: rB, core: 287, out: woOut(0, 11*tfBlock, 11, 0, 0), play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[1] != 0 || pfs[2] != 0 {
		t.Fatalf("pending ownership not kept: P_f = %d/%d, want 0/0", pfs[1], pfs[2])
	}
}

// 135-delayed-ownership: the output segment changes first (t50) with no jump
// evidence, so owned is false (pending, passthrough); the kernel jumps to base
// at t100, owned flips true and engage falls back to the target.
func TestTransientDelayedOwnership(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 30000, out: dsOut(30000, 30000), play: true},
		{ms: 50, rid: rA, core: 30050, out: dsOut(0, 0), play: true},  // new base, fed fell; no jump yet
		{ms: 100, rid: rA, core: 313, out: dsOut(0, 313), play: true}, // kernel jumps to base → owned true
		{ms: 150, rid: rA, core: 600, out: dsOut(0, 600), play: true},
		{ms: 200, rid: rA, core: 914, out: dsOut(0, 914), play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	want := []int32{30000, 30050, 0, 0, 0}
	for i := range want {
		if pfs[i] != want[i] {
			t.Fatalf("delayed ownership tick %d: P_f %d want %d", i, pfs[i], want[i])
		}
	}
}

// engage first tick may fall back below the pending passthrough value (the seek
// landing on its target); it is not floored by the old position.
func TestTransientEngageCanRetreat(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 30000, out: dsOut(30000, 30000), play: true},
		{ms: 50, rid: rA, core: 30050, out: dsOut(0, 0), play: true},  // pending passthrough 30050
		{ms: 100, rid: rA, core: 313, out: dsOut(0, 313), play: true}, // engage, target 0
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[2] != 0 {
		t.Fatalf("engage floored at old position: P_f = %d, want 0", pfs[2])
	}
}

// 135-119: paused seek back to the start. Detection tick is paused
// (passthrough, owned false); the resume tick jumps to base+multi-report and
// owns the segment, so the opening transient is filtered.
func TestTransientPausedSeekToStart(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 10000, out: dsOut(10000, 10000), play: true},
		{ms: 50, rid: rA, core: 10000, out: dsOut(0, 0), play: false}, // flush while paused; core still old
		{ms: 4000, rid: rA, core: 10000, out: dsOut(0, 0), play: false},
		{ms: 4050, rid: rA, core: 313, out: dsOut(0, 313), play: true}, // resume: jump to base
		{ms: 4100, rid: rA, core: 600, out: dsOut(0, 600), play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	// Detection/paused ticks pass through the old value.
	if pfs[1] != 10000 || pfs[2] != 10000 {
		t.Fatalf("paused detection not passthrough: %d/%d", pfs[1], pfs[2])
	}
	// Resume filters the opening transient.
	if pfs[3] != 0 || pfs[4] != 0 {
		t.Fatalf("resume did not filter: P_f = %d/%d, want 0/0", pfs[3], pfs[4])
	}
}

// --- degrade / shutdown ----------------------------------------------------

// WASAPI (and unknown vtable) are never filtered: P_f == C_raw every tick.
func TestTransientWASAPIAndUnknownPassthrough(t *testing.T) {
	for _, dev := range []DeviceKind{DeviceWASAPI, DeviceUnknown} {
		var f TransientFilter
		ticks := []tick{
			{ms: 0, rid: rA, core: 0, out: OutputState{Valid: true, Device: dev, Base: 0, Bps: tbps}, play: true},
			{ms: 50, rid: rA, core: 1515, out: OutputState{Valid: true, Device: dev, Base: 0, Bps: tbps, Fed: tfBytes(914)}, play: true},
			{ms: 100, rid: rA, core: 1515, out: OutputState{Valid: true, Device: dev, Base: 0, Bps: tbps, Fed: tfBytes(1800)}, play: true},
		}
		pfs, res := drive(t, &f, ticks...)
		for i, pf := range pfs {
			if pf != int32(ticks[i].core) {
				t.Fatalf("device %v tick %d filtered (%d != %d)", dev, i, pf, ticks[i].core)
			}
		}
		if countTrue(res) != 0 {
			t.Fatalf("device %v emitted resync", dev)
		}
	}
}

// Invalid output state (global block / device read failure): passthrough, and
// filtering resumes if the read recovers within the same segment.
func TestTransientInvalidPassthrough(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 0, out: dsOut(0, 0), play: true},
		{ms: 50, rid: rA, core: 313, out: OutputState{}, play: true}, // read failed
		{ms: 100, rid: rA, core: 600, out: dsOut(0, 600), play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[1] != 313 {
		t.Fatalf("invalid tick not passthrough: P_f = %d, want 313", pfs[1])
	}
	// Recovered tick filters again (owned via the earlier evidence / jump).
	if pfs[2] >= 600 {
		t.Fatalf("filtering did not resume after recovery: P_f = %d", pfs[2])
	}
}

// Passthrough leaves the sample otherwise identical to its input (CoreMs kept,
// handled in drive); this checks PositionMs equals core when not filtering.
func TestTransientUnfilteredEqualsInput(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 12000, out: OutputState{Valid: true, Device: DeviceWASAPI, Bps: tbps}, play: true},
		{ms: 50, rid: rA, core: 12050, out: OutputState{Valid: true, Device: DeviceWASAPI, Bps: tbps}, play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[0] != 12000 || pfs[1] != 12050 {
		t.Fatalf("WASAPI passthrough changed position: %v", pfs)
	}
}

// --- code review fixes -----------------------------------------------------

// Review 1: a pause longer than the 8 s backstop must not disengage. t_det is
// shifted by the pause (not only t_seen), so the resume tick stays filtering and
// keeps the frozen candidate line instead of republishing the multi-report core.
func TestTransientPauseLongerThanFallback(t *testing.T) {
	transientFallback = 8 * time.Second
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 0, out: dsOut(0, 0), play: true},
		{ms: 50, rid: rA, core: 1515, out: dsOut(0, 914), play: true}, // Play issued, engage, P_f 0
		{ms: 100, rid: rA, core: 1515, out: dsOut(0, 1800), play: true},
		// Pause well past 8 s of wall time; core frozen at the multi-report value.
		{ms: 150, rid: rA, core: 1515, out: dsOut(0, 1800), play: false},
		{ms: 4000, rid: rA, core: 1515, out: dsOut(0, 1800), play: false},
		{ms: 8000, rid: rA, core: 1515, out: dsOut(0, 1800), play: false},
		{ms: 10000, rid: rA, core: 1515, out: dsOut(0, 1800), play: false},
		// Resume after a ~9.9 s pause.
		{ms: 10050, rid: rA, core: 1515, out: dsOut(0, 1800), play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[len(pfs)-1] >= 1515 {
		t.Fatalf("pause longer than 8 s republished the multi-report: resume P_f = %d", pfs[len(pfs)-1])
	}
}

// Review 2: fed over the threshold with no fresh crossing (owned settles only
// after the crossing, so the previous valid sample was already over) must
// degrade to passthrough, not leave cand pinned at base. The position must
// follow the core from the degrade tick on, not stay at 0 until the backstop.
func TestTransientPlayCrossingNoFreshAnchorDegrades(t *testing.T) {
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 30000, out: dsOut(30000, 30000), play: true},
		{ms: 50, rid: rA, core: 30050, out: dsOut(0, 600), play: true},  // new base, owned not yet
		{ms: 100, rid: rA, core: 30100, out: dsOut(0, 914), play: true}, // crossing happens here, still unowned
		{ms: 150, rid: rA, core: 1515, out: dsOut(0, 1800), play: true}, // kernel jumps: owned true, but fed already over
		{ms: 200, rid: rA, core: 1700, out: dsOut(0, 1900), play: true},
		{ms: 250, rid: rA, core: 1900, out: dsOut(0, 2000), play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	// From the degrade tick on, P_f passes the core through; it is never pinned at 0.
	for _, i := range []int{3, 4, 5} {
		if pfs[i] != int32(ticks[i].core) {
			t.Fatalf("tick %d not passthrough after degrade: P_f = %d, want %d", i, pfs[i], ticks[i].core)
		}
	}
}

// Review 3: segFed is updated even while degraded, so a Flush (fed→0) under the
// same base starts a new segment and clears the degrade. Otherwise segFed stays
// at the value from the degrading tick and the fed-fall is never detected.
func TestTransientSegFedUpdatesWhileDegraded(t *testing.T) {
	badField := OutputState{Valid: true, Device: DeviceWaveOut, Base: 30000, Bps: tbps, Fed: 0, BlkAlign: 0} // BlkAlign 0 → degrade
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 30000, out: badField, play: true}, // degrade, segFed 0
		// Fields recover but the segment stays degraded (passthrough); fed grows.
		{ms: 50, rid: rA, core: 30050, out: woOut(30000, tfBytes(3000), 20, 20, 0), play: true},
		{ms: 100, rid: rA, core: 30100, out: woOut(30000, tfBytes(3100), 21, 21, 0), play: true},
		// Flush to the same base: fed falls to 0 → new segment, degrade cleared, filters.
		{ms: 150, rid: rA, core: 30313, out: woOut(30000, 0, 0, 0, 0), play: true},
		{ms: 200, rid: rA, core: 30313, out: woOut(30000, 12*tfBlock, 12, 3, 0), play: true},
	}
	pfs, _ := drive(t, &f, ticks...)
	if pfs[1] != 30050 || pfs[2] != 30100 {
		t.Fatalf("degraded ticks not passthrough: %d/%d", pfs[1], pfs[2])
	}
	if pfs[3] != 30000 {
		t.Fatalf("Flush after degrade not recognised as a new segment: P_f = %d, want 30000", pfs[3])
	}
}

// Review 4: a new ladder jump after Play clamps the ramp start to this tick's
// observed time, not the previous jump's, so the candidate restarts at base on
// the jump tick rather than carrying a stale, higher value.
func TestTransientLadderJumpClampSameTick(t *testing.T) {
	transientLDSOpen = 106 * time.Millisecond
	var f TransientFilter
	ticks := []tick{
		{ms: 0, rid: rA, core: 0, out: dsOut(0, 0), play: true},
		{ms: 50, rid: rA, core: 313, out: dsOut(0, 313), play: true},
		{ms: 100, rid: rA, core: 600, out: dsOut(0, 600), play: true},
		{ms: 150, rid: rA, core: 914, out: dsOut(0, 914), play: true}, // Play issued
		{ms: 200, rid: rA, core: 914, out: dsOut(0, 1200), play: true},
		{ms: 250, rid: rA, core: 914, out: dsOut(0, 1200), play: true},
		{ms: 300, rid: rA, core: 1515, out: dsOut(0, 1800), play: true}, // new ladder jump (step 601)
	}
	pfs, _ := drive(t, &f, ticks...)
	// With the jump recorded on this tick, start = t300 and cand = base; a stale
	// lastJumpAt (t150) would ramp cand to base + (300 - 256) = 44.
	if pfs[6] != 0 {
		t.Fatalf("ladder jump not clamped on the same tick: P_f = %d, want 0", pfs[6])
	}
}

// --- whole-branch final review fixes ---------------------------------------

// Final review 135-anchor-age: the DS anchor needs a fresh crossing bounded by
// the ACTUAL gap between the two valid samples, not the configured interval.
// Here the config is 50 ms but a snapshot delay leaves the pre-crossing sample
// (t=50) and the crossing (t=1000) 950 ms apart, beyond anchorMaxGap, so the
// segment degrades to passthrough instead of anchoring late and pinning low.
//
// Mutation (verified): dropping the `now.Sub(prevValidAt) <= anchorMaxGap`
// guard anchors at t=1000, sets TransientResync and pins P_f at 0.
func TestTransientAnchorActualGapDegrades(t *testing.T) {
	var f TransientFilter
	pfs, res := drive(t, &f,
		tick{ms: 0, rid: rA, core: 0, out: dsOut(0, 0), play: true},
		tick{ms: 50, rid: rA, core: 313, out: dsOut(0, 313), play: true},    // engage; prevFedBelow, prevValidAt=50
		tick{ms: 1000, rid: rA, core: 1515, out: dsOut(0, 2000), play: true}, // fed crosses, but 950 ms after the last valid sample
	)
	if res[2] {
		t.Fatalf("stale-gap crossing set TransientResync")
	}
	if pfs[2] != 1515 {
		t.Fatalf("stale-gap crossing: P_f = %d, want 1515 passthrough (degraded)", pfs[2])
	}
}

// Final review 135-device-degrade: a read failure (Valid=false) while the
// segment is filtering degrades the whole segment, so the recovery tick keeps
// passing through the raw core instead of re-capping below the value the
// failed tick already published (which CoreClock would judge a backward seek).
//
// Mutation (verified): removing the `if f.phase == phaseFiltering { degraded }`
// re-caps the recovery tick to 40000, a backward step off the 40600 published
// on the read-fail tick.
func TestTransientReadFailWhileFilteringDegrades(t *testing.T) {
	var f TransientFilter
	pfs, _ := drive(t, &f,
		tick{ms: 0, rid: rA, core: 10000, out: dsOut(10000, 10000), play: true}, // steady, first-tick fed over threshold → that segment degrades
		tick{ms: 50, rid: rA, core: 40313, out: dsOut(40000, 313), play: true},  // seek: new segment, engage filtering, cap to 40000
		tick{ms: 100, rid: rA, core: 40600, out: OutputState{}, play: true},     // read fail while filtering: passthrough 40600, degrade segment
		tick{ms: 150, rid: rA, core: 40600, out: dsOut(40000, 600), play: true}, // recover: stay passthrough, no re-pin
	)
	if pfs[1] != 40000 {
		t.Fatalf("engage tick: P_f = %d, want 40000", pfs[1])
	}
	if pfs[2] != 40600 {
		t.Fatalf("read-fail tick: P_f = %d, want 40600 passthrough", pfs[2])
	}
	if pfs[3] != 40600 {
		t.Fatalf("recovery after read-fail while filtering must stay passthrough: P_f = %d, want 40600", pfs[3])
	}
}
