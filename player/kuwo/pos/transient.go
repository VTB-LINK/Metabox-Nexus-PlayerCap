package pos

import (
	"math"
	"time"
)

// This file is build-constraint free so the #135 open/seek output-transient
// filter can be tested off Windows. It consumes the raw core position C_raw and
// the per-tick output state (Task 1's OutputState) and returns the filtered
// position P_f plus whether DirectSound's "Play has been issued" tick fell on
// this sample. Design: doc/design/kuwo-output-transient-design.md (§3.2, §3.3,
// §3.4, §3.5, §3.6, §3.7).
//
// After Open or Flush, out_kw_ds GetOutputTime reports fed (bytes handed to the
// device) as "already played" until the device starts consuming, so the core
// position leads the sound by whole blocks before stalling, then recovers 1:1.
// The filter caps the core position at a candidate line that tracks the real
// sound: for DirectSound a time anchor (method A), for WaveOut the device's
// header counts (method B). WASAPI has no transient and is never filtered.

// Package variables so tests can adjust the thresholds; initial values from the
// design §3.4 / plan Task 2 (本机 Win10/Realtek 实测中位，样本外 run10/11 一致).
var (
	transientTol                = 50 * time.Millisecond  // band and stop tolerance
	transientMaxOvershoot int64 = 2000                   // band upper margin above base, ms
	transientAnchorMaxGap       = 150 * time.Millisecond // longest poll that still anchors DS
	transientFallback           = 8 * time.Second        // disengage backstop from t_det
	transientFedPlayMs    int64 = 750                    // DS "Play issued": fed first reaches this, ms
	transientLDSOpen            = 106 * time.Millisecond // DS fixed delay L on open (RID change)
	transientLDSSeek            = 44 * time.Millisecond  // DS fixed delay L on an in-song seek
)

// transientBlockFrames is the PCM frame count per fed block / WaveOut header
// (nBlockAlign * 1152 bytes), from out_kw_ds's buffering (findings §2.3).
const transientBlockFrames = 1152

// transientPhase is the state of the current output segment (design §3.3):
// pending (detected, not engaged), filtering (engaged), done (disengaged,
// passthrough for the rest of the segment).
type transientPhase int

const (
	phasePending transientPhase = iota
	phaseFiltering
	phaseDone
)

// TransientFilter corrects the open/seek output transient on the raw core
// position (design §3.3). The zero value is ready to use. It is not safe for
// concurrent use; its lifetime matches the kernel reader (one playback
// session), so a new session starts from the zero value.
type TransientFilter struct {
	// Own raw core history for the attribution jump test (design §3.2): the
	// filter must not read or advance the session's CoreClock, which consumes
	// the corrected position and runs after the filter.
	havePrev   bool
	prevCoreMs int32
	prevAt     time.Time
	prevRID    string

	// Current output segment = (device, base, whether fed fell). RID change
	// also resets it.
	segActive bool
	device    DeviceKind
	base      uint32
	segFed    uint32 // last valid tick's fed in this segment, for fed-fall detection
	phase     transientPhase

	// owned: the segment belongs to the current kernel song. Monotonic
	// false->true; set once per segment and kept until the next reset. Its
	// source tells DS open from seek for L.
	owned     bool
	ownedOpen bool

	// degraded: a device-specific failure (unknown vtable, WASAPI, WaveOut
	// field invalid) disables filtering for the rest of the segment.
	degraded bool
	// dsDegraded: DS cannot anchor (long poll, or fed already over threshold on
	// the first filterable tick) so it passes through for the rest of the segment.
	dsDegraded bool

	// DS method-A anchor.
	playSeen     bool      // "Play has been issued" seen this segment
	tSeen        time.Time // tick fed first crossed the threshold
	lastJumpAt   time.Time // observed time of the last in-segment kernel step jump
	seenValid    bool      // a valid filterable sample has been seen this segment
	prevFedBelow bool      // last valid sample's fed was below the threshold
	prevValidAt  time.Time // observation time of the last valid filterable sample, for the anchor's actual-gap check

	// t_det and the P_f floor.
	tDet   time.Time
	havePf bool
	lastPf int64
}

// Apply returns core with PositionMs set to the filtered position P_f (CoreMs
// unchanged) and whether DS's "Play issued" tick is this sample. playing is
// meta.Status == 1; interval is the effective poll interval, used for the DS
// anchor gap. On any lack of evidence it passes the core position through
// (P_f = C_raw), so behaviour never differs from #131 alone (design §3.3).
func (f *TransientFilter) Apply(core Sample, out OutputState, playing bool, interval time.Duration) (Sample, bool) {
	cRaw := int64(core.PositionMs)
	now := core.ObservedAt

	// Jump test against the filter's own raw history (design §3.3 attribution).
	jump := false
	advanced := false
	if f.havePrev && core.RID == f.prevRID && !now.Before(f.prevAt) {
		step := cRaw - int64(f.prevCoreMs)
		elapsed := now.Sub(f.prevAt).Milliseconds()
		jump = step < 0 || step > elapsed+250
		advanced = step > 0
	}
	ridChanged := core.RID != f.prevRID

	// Segment detection and reset. RID change always resets; base change, a fed
	// fall or a device change reset only when the output state is valid.
	reset := ridChanged
	if out.Valid && isTransientDevice(out.Device) {
		if !f.segActive || out.Device != f.device || out.Base != f.base || out.Fed < f.segFed {
			reset = true
		}
	}
	if reset {
		f.resetSegment(out, ridChanged)
	}

	resync := false
	pf := cRaw // default passthrough

	// Whether this tick is the one the segment first enters filtering, for the
	// engage log (design §3.8). Detected as the pending→filtering transition
	// across the switch, so the many filter() return paths need not carry it.
	prevPhase := f.phase

	switch {
	case !out.Valid:
		// A read failed this tick (global block, device, or ring/fed): this tick
		// passes through the un-capped C_raw. If the segment is already filtering,
		// degrade it for the rest of the segment: it has published a capped P_f, so
		// re-capping on recovery would step the public position back off the
		// passed-through multi-report value and CoreClock would emit an extra
		// playback_resume (design §3.3 degrade table). pending/done have published
		// no capped value, so this-tick passthrough is enough; the segment state is
		// kept and a new segment (valid, base/fed/RID change) re-enables.
		if f.phase == phaseFiltering {
			f.degraded = true
		}
	case f.degraded:
		// A device-specific failure earlier this segment: stay passed through.
	case out.Device == DeviceWASAPI, out.Device == DeviceUnknown:
		// WASAPI has no transient; an unknown vtable is unrecognised. Off for the
		// rest of the segment (design §3.3 degrade table).
		f.degraded = true
	case out.Device == DeviceWaveOut && (out.BlkAlign == 0 || out.HdrD > out.HdrW):
		// WaveOut field invalid or out of range: off for the segment, do not fall
		// back to A (design §3.3).
		f.degraded = true
	default:
		pf, resync = f.filter(core, out, playing, interval, cRaw, now, jump, advanced, ridChanged)
	}

	// Update the filter's own raw history every tick, including passthrough.
	f.havePrev, f.prevCoreMs, f.prevAt, f.prevRID = true, core.PositionMs, now, core.RID
	// Record this segment's fed at the end of the tick, for the next tick's
	// fed-fall reset. Updated for every valid transient-device tick including a
	// degraded one, so that a Flush (fed→0) under the same base still starts a new
	// segment and clears the degrade (reset detection above used the old value).
	if out.Valid && isTransientDevice(out.Device) {
		f.segFed = out.Fed
	}

	out2 := core
	out2.PositionMs = int32(pf)
	out2.TransientResync = resync
	out2.TransientEngaged = prevPhase == phasePending && f.phase == phaseFiltering
	return out2, resync
}

// resetSegment starts a fresh segment. openSrc is whether the reset was caused
// by an RID change (a new song = open); it seeds the owned source for L.
func (f *TransientFilter) resetSegment(out OutputState, openSrc bool) {
	known := out.Valid && isTransientDevice(out.Device)
	*f = TransientFilter{
		havePrev:   f.havePrev,
		prevCoreMs: f.prevCoreMs,
		prevAt:     f.prevAt,
		prevRID:    f.prevRID,
		segActive:  known,
		phase:      phasePending,
	}
	if known {
		f.device = out.Device
		f.base = out.Base
		f.segFed = out.Fed
	}
	// owned is evaluated in filter(); seed the open source so an RID-change
	// reset is classified as an open even though owned is set there.
	f.ownedOpen = openSrc
}

// filter runs the state machine for a filterable (DS or WaveOut, valid,
// non-degraded) tick and returns P_f and the resync flag.
func (f *TransientFilter) filter(core Sample, out OutputState, playing bool, interval time.Duration, cRaw int64, now time.Time, jump, advanced, ridChanged bool) (int64, bool) {
	// Monotonic ownership: set once, kept for the segment (design §3.3). Not
	// required to coincide with a multi-report tick.
	if !f.owned {
		switch {
		case ridChanged:
			f.owned, f.ownedOpen = true, true
		case int64(out.Base) > transientMaxOvershoot:
			f.owned, f.ownedOpen = true, false
		case jump:
			f.owned, f.ownedOpen = true, false
		}
	}

	resync := false
	tolMs := transientTol.Milliseconds()

	// DS "Play issued" detection and the long-poll / stale-crossing degrade,
	// before the candidate line so the anchor applies from this tick on. Gated on
	// owned: the anchor and its resync belong to method A on a segment that owns
	// the current song (design §3.6). Without this gate a cross-song window (new
	// song's DS output fed past the threshold while the kernel RID still lags on
	// the old song, owned false, passing through) would emit a spurious resync and
	// risk an extra playback_resume on the old song.
	if out.Device == DeviceDirectSound && playing && f.owned && !f.playSeen && !f.dsDegraded {
		fedMs := int64(out.Fed) * 1000 / int64(out.Bps)
		switch {
		case interval > transientAnchorMaxGap:
			f.dsDegraded = true // long poll: no fresh anchor
		case fedMs < transientFedPlayMs:
			// Play not issued yet: keep waiting, cand stays at base.
		case f.seenValid && f.prevFedBelow && !f.prevValidAt.IsZero() && now.Sub(f.prevValidAt) <= transientAnchorMaxGap:
			// Fresh crossing: the previous valid sample was below the threshold, this
			// one is over, AND the two valid samples are within anchorMaxGap of each
			// other. The actual gap (not the configured interval) is what bounds the
			// anchor error: output reads failing, core samples skipped or a delayed
			// snapshot can leave two valid samples far apart even at a 50 ms config,
			// and anchoring then would push the already-passed-through value back.
			f.playSeen, f.tSeen, resync = true, now, true
		default:
			// fed is over the threshold but there is no fresh crossing to anchor on:
			// the first filterable sample is already over (crossing before sampling),
			// or owned settled only after the crossing so the previous valid sample
			// was already over. The Play time is unknowable, so degrade to
			// passthrough rather than leave cand pinned at base until the backstop
			// (design §3.3 Play 判据: fed 越阈即二选一、不得悬空).
			f.dsDegraded = true
		}
	}

	// Record this tick's forward kernel step jump before computing cand, so the
	// ramp-start clamp uses the current ladder jump rather than the previous one.
	// filter() is only reached on a valid, non-degraded transient-device tick, so
	// the former segActive/degraded/Valid guards are implied.
	if jump && advanced {
		f.lastJumpAt = now
	}

	cand := f.candidate(out, now)

	// Bookkeeping for the next tick's DS freshness test (this tick is valid).
	if out.Device == DeviceDirectSound {
		f.prevFedBelow = int64(out.Fed)*1000/int64(out.Bps) < transientFedPlayMs
	}
	f.seenValid = true
	f.prevValidAt = now

	if f.dsDegraded {
		return cRaw, resync
	}

	band := int64(out.Base)-tolMs <= cRaw && cRaw <= int64(out.Base)+transientMaxOvershoot

	switch f.phase {
	case phasePending:
		if playing && f.owned && band && cRaw > cand {
			// Engage. The first filtering tick has no prior P_f, so it may fall
			// back to the target (design §3.3 engage floor).
			f.phase = phaseFiltering
			f.tDet = now
			pf := clampMin0(min(cRaw, cand))
			f.lastPf, f.havePf = pf, true
			return pf, resync
		}
		// pending passthrough: paused, unowned, outside the band, or no
		// multi-report yet.
		return cRaw, resync

	case phaseFiltering:
		if !playing {
			// Freeze the candidate line: hold P_f. Shift t_det forward by the pause
			// so the 8 s backstop measures active-transient time, not wall time that
			// includes a pause; otherwise a pause longer than 8 s would disengage on
			// the resume tick and republish the still-multi-report core. Also shift
			// the anchor t_seen (once Play has issued) so the ramp resumes where it
			// froze (design §3.3 启用时机与三态, §3.7).
			if !now.Before(f.prevAt) {
				pause := now.Sub(f.prevAt)
				f.tDet = f.tDet.Add(pause)
				if f.playSeen {
					f.tSeen = f.tSeen.Add(pause)
				}
			}
			return f.lastPf, resync
		}
		// Disengage: the core has caught the uncapped candidate line and advances,
		// or the 8 s backstop fired. Compare cand, never the min-capped P_f.
		if (cRaw <= cand+tolMs && advanced) || now.Sub(f.tDet) >= transientFallback {
			f.phase = phaseDone
			return f.holdDone(cRaw), resync
		}
		pf := max(f.lastPf, clampMin0(min(cRaw, cand)))
		f.lastPf = pf
		return pf, resync

	default: // phaseDone
		return f.holdDone(cRaw), resync
	}
}

// candidate returns the uncapped candidate line cand for this tick.
func (f *TransientFilter) candidate(out OutputState, now time.Time) int64 {
	if out.Device == DeviceWaveOut {
		// Method B: the device header counts give the real played bytes.
		queue := scaleTransient(max(0, int64(out.HdrW)-int64(out.HdrD))*int64(out.BlkAlign)*transientBlockFrames+int64(out.Staging), out)
		played := max(0, int64(out.Fed)-queue)
		return int64(out.Base) + played*1000/int64(out.Bps)
	}
	// Method A (DirectSound). Before "Play issued" the sound has not started, so
	// cand = base. After it, ramp from the anchor time, clamped not to start
	// before the last kernel step jump.
	if !f.playSeen {
		return int64(out.Base)
	}
	l := transientLDSSeek
	if f.ownedOpen {
		l = transientLDSOpen
	}
	start := f.tSeen.Add(l)
	if start.Before(f.lastJumpAt) {
		start = f.lastJumpAt
	}
	return int64(out.Base) + now.Sub(start).Milliseconds()
}

// holdDone returns the passthrough position once disengaged, holding the last
// filtering P_f through a single small backward step until the core catches up
// (design §3.3 stop).
func (f *TransientFilter) holdDone(cRaw int64) int64 {
	if f.havePf && cRaw < f.lastPf {
		return f.lastPf
	}
	return cRaw
}

// scaleTransient mirrors GetOutputTime's effect-rate scaling (0x1000fd41..fd67):
// when the effect chain is on and the multiplier is not 1.0, the queued bytes
// are scaled by it. The disassembly compares the multiplier to the constant at
// 0x100174fc, which is 1.0, so a multiplier of 1.0 (the run11 case) is a no-op.
// ponytail: only the ratio == 1.0 path is verified on real data; a variable-
// speed multiplier (ratio != 1.0) is untested (design §7.8), but min(C_raw, …)
// caps the result either way.
func scaleTransient(x int64, out OutputState) int64 {
	if out.EffectOn && out.EffectRatio > 0 && out.EffectRatio != 1.0 {
		return int64(math.Round(float64(x) * float64(out.EffectRatio)))
	}
	return x
}

// isTransientDevice reports whether the device shows the open/seek transient the
// filter can correct (DirectSound or WaveOut).
func isTransientDevice(d DeviceKind) bool {
	return d == DeviceDirectSound || d == DeviceWaveOut
}

func clampMin0(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
