package pos

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"time"
)

var (
	ErrUnsupportedCore = errors.New("kuwo: unsupported playback core")
	ErrSongChanged     = errors.New("kuwo: playback core song changed")
	ErrInvalidSample   = errors.New("kuwo: invalid playback core sample")
	// ErrClockStalled means a song played long enough that a live core would
	// have advanced, yet the position never moved: a behavioural check beyond
	// the static structure match, in case the position field has moved elsewhere.
	ErrClockStalled = errors.New("kuwo: playback core position stalled while playing")
)

const (
	coreSize     = 0x2ac
	coreRID      = 0x84
	coreDuration = 0xa8
	coreBackend  = 0x188
	corePosition = 0x2a0
)

// Sample is a read-only snapshot of PlayerCore's playback position, in milliseconds.
// This is a client clock, not a measurement of sound-card or OBS output latency.
// The core reader returns the position PlayerCore reports, which after a seek
// leads the sound by the seek lag; LagCorrector.Apply deducts that lag
// (doc/design/kuwo-seek-lag-design.md). The corrected position is the public
// one: positioned events, EmitPositionSample and lyric matching all use it,
// without offset.
type Sample struct {
	RID        string
	PositionMs int32
	DurationMs int32
	ObservedAt time.Time
	// LagShiftMs is how far, in milliseconds, this sample's position moved
	// beyond the core position's own movement because the seek-lag correction
	// changed on this tick: the deduction applied to the previous sample
	// (core position minus output position, clamp included) minus the one
	// applied to this sample. Positive when the correction shrinks, negative
	// when it grows; 0 on a song change and on a session's first sample. The
	// core reader leaves it 0; the corrector fills it.
	LagShiftMs int32
	// LagAppliedMs is the deduction applied to this sample, in milliseconds:
	// the core position minus PositionMs, clamp included, so PositionMs +
	// LagAppliedMs is the core position. The core reader leaves it 0; the
	// corrector fills it.
	LagAppliedMs int32
	// LagAdopted is whether a segment's confirmed value became the e_cur in
	// effect for the first time on this tick: at most once per segment, the
	// opening segment and a session's first one included, never on a raise, and
	// not on a tick on which the mismatch rule zeroes it. The core reader leaves
	// it false; the corrector fills it.
	LagAdopted bool
	// CoreMs is the raw core position before the #135 transient filter and the
	// #131 correction, so that the three consumers that must see it (design
	// §3.2: the mismatch rule, the stall guard, and LagAppliedMs's reference)
	// read the kernel's own value rather than the filtered PositionMs. The core
	// reader sets it to PositionMs; the filter and corrector leave it.
	CoreMs int32
	// TransientResync is set by the #135 transient filter on the tick DirectSound
	// starts playing, so the session can notify the front-end model (design
	// §3.6). The core reader leaves it false.
	TransientResync bool
	// TransientEngaged is set by the #135 transient filter on the one tick a
	// segment first enters filtering, so the corrected source logs the engage once
	// (design §3.8). It is distinct from TransientResync, which marks only the
	// DirectSound Play-issued tick. The core reader leaves it false.
	TransientEngaged bool
}

type memoryRead func(uintptr, int) ([]byte, error)

// readCoreSample rechecks identity around a read. PlayerCore does not offer an
// external atomic snapshot: reject transitions instead of combining two songs.
func readCoreSample(read memoryRead, base uintptr, expectedRID string) (Sample, error) {
	var result Sample
	a, err := read(base, coreSize)
	if err != nil {
		return result, err
	}
	if len(a) != coreSize {
		return result, ErrInvalidSample
	}
	observedAt := time.Now()
	rid, err := coreString(read, a[coreRID:coreRID+24])
	if err != nil {
		return result, err
	}
	if !validRID(rid) {
		return result, ErrInvalidSample
	}
	if expectedRID != "" && rid != RIDWithPrefix(expectedRID) {
		return result, ErrSongChanged
	}
	// Only the native audio backend has been verified. Do not infer that WMP or
	// DirectShow has the same refresh behavior from the shared position slot.
	if getU32(a[coreBackend:]) != 1 {
		return result, ErrUnsupportedCore
	}
	duration := int64(getU32(a[coreDuration:])) * 1000
	position := int64(int32(getU32(a[corePosition:])))
	if duration <= 0 || duration > 24*60*60*1000 || position < 0 || position > duration {
		return result, ErrInvalidSample
	}
	b, err := read(base, coreSize)
	if err != nil {
		return result, err
	}
	if len(b) != coreSize {
		return result, ErrInvalidSample
	}
	if !bytes.Equal(a[coreRID:coreRID+24], b[coreRID:coreRID+24]) ||
		getU32(a[coreDuration:]) != getU32(b[coreDuration:]) ||
		getU32(a[coreBackend:]) != getU32(b[coreBackend:]) {
		return result, ErrSongChanged
	}
	ridAfter, err := coreString(read, b[coreRID:coreRID+24])
	if err != nil {
		return result, err
	}
	if ridAfter != rid {
		return result, ErrSongChanged
	}
	// Backward movement while taking the snapshot is a seek/loop: retry next
	// poll rather than publish the pre-seek value with post-seek metadata.
	endPosition := int64(int32(getU32(b[corePosition:])))
	if endPosition < position || endPosition-position > 250 || time.Since(observedAt) > 50*time.Millisecond {
		return result, ErrSongChanged
	}
	result = Sample{RID: rid, PositionMs: int32(position), DurationMs: int32(duration), ObservedAt: observedAt}
	result.CoreMs = result.PositionMs
	return result, nil
}

// MSVC x86 std::string: 16-byte inline buffer, length at +16, capacity at +20.
// The old probe mistook these last two fields for quality settings.
func coreString(read memoryRead, b []byte) (string, error) {
	if len(b) != 24 {
		return "", ErrInvalidSample
	}
	n, capacity := getU32(b[16:]), getU32(b[20:])
	if n == 0 || n > 64 || capacity < n || capacity > 1<<20 {
		return "", ErrInvalidSample
	}
	var raw []byte
	if capacity < 16 {
		if n > 15 {
			return "", ErrInvalidSample
		}
		raw = b[:n+1]
	} else {
		addr := uintptr(binary.LittleEndian.Uint32(b))
		if addr < 0x10000 || addr > 0xffff0000 {
			return "", ErrInvalidSample
		}
		var err error
		raw, err = read(addr, int(n)+1)
		if err != nil {
			return "", err
		}
	}
	if len(raw) != int(n)+1 || raw[n] != 0 || bytes.IndexByte(raw[:n], 0) >= 0 {
		return "", ErrInvalidSample
	}
	return string(raw[:n]), nil
}

func validRID(s string) bool {
	if !strings.HasPrefix(s, "MUSIC_") || len(s) <= 6 || len(s) > 26 {
		return false
	}
	for _, c := range s[6:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return strings.Trim(s[6:], "0") != ""
}

// CoreClock consumes real progress samples. It deliberately does not extrapolate:
// a repeated sample freezes immediately, rather than running ahead on pause and
// jumping back later. Advancing only describes recent movement, not the native
// paused/buffering state. A future player session should use a verified status
// signal to distinguish those states.
type CoreClock struct {
	last       Sample
	lastChange time.Time
	have       bool
	moved      bool
}

// Observe returns whether the sample indicates a seek or loop within one song.
// A new RID resets the clock, and must be handled as a song change by the caller.
func (c *CoreClock) Observe(s Sample) (seek bool) {
	if s.RID == "" || s.PositionMs < 0 || s.DurationMs <= 0 || s.PositionMs > s.DurationMs || s.ObservedAt.IsZero() {
		return false
	}
	if c.have && s.ObservedAt.Before(c.last.ObservedAt) {
		return false
	}
	if !c.have || c.last.RID != s.RID {
		*c = CoreClock{last: s, lastChange: s.ObservedAt, have: true}
		return false
	}
	step := int64(s.PositionMs) - int64(c.last.PositionMs)
	if step != 0 {
		elapsed := s.ObservedAt.Sub(c.last.ObservedAt).Milliseconds()
		seek = step < 0 || step > elapsed+250
		c.lastChange = s.ObservedAt
		c.moved = true
	}
	c.last = s
	return seek
}

func (c *CoreClock) Position() float32 { return float32(c.last.PositionMs) / 1000 }
func (c *CoreClock) Advancing(now time.Time) bool {
	return c.have && c.moved && !now.Before(c.lastChange) && now.Sub(c.lastChange) <= 350*time.Millisecond
}
func (c *CoreClock) Reset() { *c = CoreClock{} }
