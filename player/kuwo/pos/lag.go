package pos

import (
	"bytes"
	"crypto/sha256"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"time"
)

// This file is build-constraint free so the build admission, the per-sample
// read sequence, the seek-lag evaluation and the corrector can be tested off
// Windows. Only the process handle, Toolhelp and file calls (lag_windows.go)
// are Windows-specific. Design: doc/design/kuwo-seek-lag-design.md (§1.3,
// §3.1, §3.2, §3.3, §3.6).
//
// After a seek in_ffaudio flushes the output to the target but decodes from
// the nearest earlier seek point without trimming, so the core position leads
// the sound by e = W - P - D + S: W is out_kw_ds's written time, P and D the
// pts and duration of the last packet taken, S the format start time.

// Thresholds are variables so tests can adjust them.
var (
	lagMinEUs int64 = -200000 // lowest accepted e, µs
	lagMaxEUs int64 = 2000000 // highest accepted e, µs
	lagMaxDUs int64 = 500000  // longest accepted packet duration, µs

	lagConfirmTolUs     int64 = 1000                   // adjacent valid samples this close confirm a value, µs
	lagFailAfter              = 500 * time.Millisecond // no valid sample for this long: the segment fails
	lagUnconfirmedAfter       = 500 * time.Millisecond // a segment unconfirmed this long: no correction
	lagDriftCount             = 3                      // consecutive samples outside the drift band that disable the song
	lagDriftMarginUs    int64 = 5000                   // drift band margin beyond the sample's D, µs
)

// lagTimerSamples is how many samples both timers require after their
// reference sample besides the elapsed time, so that under a long poll
// interval one stray invalid sample does not zero the correction and a new
// segment's second sample can still confirm it.
const lagTimerSamples = 2

const (
	lagMinBps     = 1000    // byte rate lower bound, bytes per second
	lagMaxBps     = 1536000 // byte rate upper bound, bytes per second
	lagMaxStreams = 64      // nb_streams upper bound
	lagMinTbRatio = 1000    // time base den/num lower bound: at least 1 ms precision
	lagStartTolUs = 1000    // AVStream and AVFormatContext start_time may differ by this much, µs
)

// Decoder context layout (in_ffaudio 9.6.0.0_UG1). The AVPacket copy starts
// at +0x68.
const (
	lagCtxSize   = 0x100
	lagCtxFormat = 0x0  // AVFormatContext*
	lagCtxPTS    = 0x70 // int64, pts of the last packet taken
	lagCtxStream = 0x88 // int32, stream_index of that packet
	lagCtxDur    = 0x98 // int64, duration of that packet
	lagCtxHeld   = 0xf8 // int32, non-zero while that packet is not used up
)

// avNoPTS is FFmpeg's AV_NOPTS_VALUE.
const avNoPTS = int64(-1 << 63)

// Build admission (design §3.1): the SHA-256 of the executable sections of
// in_ffaudio.dll and out_kw_ds.dll. Equal code means equal global RVAs and
// decoder context layout, so the offsets in this file are constants. Both
// hashes must come from one row; a new build is a new row.
type lagBuild struct{ Name, InHash, OutHash string }

var lagBuilds = []lagBuild{{"9.6.0.0_UG1",
	"aaaf07c46259aa24941611a15ee3fb8d7a85fc9675053958d8268c38756ad9d8",
	"2e9ff2f5457e7540279ea4815316874c81e323c34960fd5d9f346827dc9c3677"}}

// ErrLagUnsupported means the correction stays off for a reason other than a
// system call failure: a module is missing, unknown, or differs from its file.
var ErrLagUnsupported = errors.New("kuwo: seek-lag correction not available for this build")

const (
	lagFileLimit  = 4 << 20 // module file size upper bound, bytes
	lagHeaderSize = 0x1000  // in-memory PE header bytes read at open
)

// Seek-lag fields of 9.6.0.0_UG1, pinned by the build fingerprint. RVAs are
// relative to the module base.
const (
	lagOutBlockRVA  = 0x1a8a4 // out_kw_ds globals block
	lagOutBlockSize = 0x60
	lagOutBase      = 0x0    // in the block: output base, ms
	lagOutBps       = 0x24   // in the block: byte rate (RVA 0x1a8c8)
	lagOutRing      = 0x5c   // in the block: ring buffer pointer (RVA 0x1a900)
	lagRingWritten  = 0x8    // ring buffer: bytes written
	lagInRunningRVA = 0xa044 // in_ffaudio decoder thread running flag
	lagInCtxRVA     = 0xa4f4 // in_ffaudio decoder context pointer
	lagFmtStreams   = 0x18   // AVFormatContext: nb_streams, then streams
	lagFmtStart     = 0x420  // AVFormatContext: start_time, µs
	lagStreamSize   = 0x38   // AVStream bytes read
	lagStIndex      = 0x0    // AVStream: index
	lagStTbNum      = 0x28   // AVStream: time base num, then den
	lagStStart      = 0x30   // AVStream: start_time
)

// Output transient fields (#135, design §3.1), pinned by the same build
// fingerprint as the seek-lag fields. The globals block starts 0x28 bytes
// before lagOutBlockRVA and is 0x98 bytes; it is read separately from the
// seek-lag block (readOutputState), so the #131 sample read is unchanged.
// Offsets inside the block are relative to lagOutStateRVA.
const (
	lagOutStateRVA  = 0x1a87c // output globals block start for #135
	lagOutStateSize = 0x98
	lagStA87c       = 0x00 // 0x1a87c: blocks written since Flush/Open
	lagStStaging    = 0x0c // 0x1a888: bytes taken but not yet written to the device
	lagStEffectOn   = 0x10 // 0x1a88c: effect chain enabled flag
	lagStDevPtr     = 0x18 // 0x1a894: device object pointer
	lagStBase       = 0x28 // 0x1a8a4: output base, ms
	lagStBps        = 0x4c // 0x1a8c8: byte rate
	lagStRing       = 0x84 // 0x1a900: ring buffer pointer
	lagRingFed      = 0xc  // ring buffer: bytes fed to the device side

	lagEffectRatioRVA = 0x1a014 // effect-rate multiplier, float32, in .data

	lagDevSize     = 0x40 // device object bytes read
	lagDevVtable   = 0x0  // first dword: vtable pointer
	lagDevBlkAlign = 0x1c // WORD nBlockAlign
	lagDevHdrW     = 0x38 // WaveOut headers written
	lagDevHdrD     = 0x3c // WaveOut headers done

	// Device object vtable RVAs (relative to the out_kw_ds module base).
	lagVtblDS      = 0x17000
	lagVtblWaveOut = 0x173c0
	lagVtblWASAPI  = 0x17360
)

// DeviceKind is the out_kw_ds output device behind GetBufferedBytes, told
// apart by its object's vtable RVA (design §1.2). Only DirectSound and WaveOut
// show the open/seek transient #135 corrects; WASAPI does not.
type DeviceKind int

const (
	DeviceUnknown DeviceKind = iota
	DeviceDirectSound
	DeviceWaveOut
	DeviceWASAPI
)

// OutputState is one read of out_kw_ds's output state for the #135 transient
// filter (design §3.1). Valid is false when there is no transient evidence
// this tick: the byte rate is zero, or the device pointer is out of range or
// its object could not be read. The filter passes the core position through on
// a false Valid, so a failed read never changes behaviour.
type OutputState struct {
	Valid       bool
	Device      DeviceKind
	Base        uint32  // output base, ms (0x1a8a4)
	Bps         uint32  // byte rate (0x1a8c8)
	Fed         uint32  // bytes fed to the device side (ring +0xc)
	Staging     uint32  // bytes taken but not yet written to the device (0x1a888)
	A87c        uint32  // blocks written since Flush/Open (0x1a87c)
	EffectRatio float32 // effect-rate multiplier (0x1a014)
	EffectOn    bool    // effect chain enabled (0x1a88c)
	HdrW        uint32  // WaveOut headers written (device +0x38)
	HdrD        uint32  // WaveOut headers done (device +0x3c)
	BlkAlign    uint16  // nBlockAlign (device +0x1c)
}

// evalOutputState parses out_kw_ds's output state. block is lagOutStateSize
// bytes at lagOutStateRVA; dev is lagDevSize bytes of the device object at the
// pointer held in the block, or nil when that pointer is out of range or its
// read failed; outBase is the out_kw_ds module base; effectRatio is the
// separately read multiplier. Fed is filled by the caller. Valid is false when
// the byte rate is zero or the device object is unavailable; an unknown or
// WASAPI vtable is still Valid (the device was read) and the filter decides
// from Device. The device vtable RVA is firstDword(dev) − outBase.
func evalOutputState(block, dev []byte, outBase uintptr, effectRatio float32) OutputState {
	if len(block) < lagOutStateSize {
		return OutputState{}
	}
	bps := getU32(block[lagStBps:])
	if bps == 0 || !lagPtrOK(getU32(block[lagStDevPtr:])) || len(dev) < lagDevSize {
		return OutputState{}
	}
	s := OutputState{
		Valid:       true,
		Base:        getU32(block[lagStBase:]),
		Bps:         bps,
		Staging:     getU32(block[lagStStaging:]),
		A87c:        getU32(block[lagStA87c:]),
		EffectRatio: effectRatio,
		EffectOn:    getU32(block[lagStEffectOn:]) != 0,
		HdrW:        getU32(dev[lagDevHdrW:]),
		HdrD:        getU32(dev[lagDevHdrD:]),
		BlkAlign:    getU16(dev[lagDevBlkAlign:]),
	}
	if vt := getU32(dev[lagDevVtable:]); uintptr(vt) >= outBase {
		switch uintptr(vt) - outBase {
		case lagVtblDS:
			s.Device = DeviceDirectSound
		case lagVtblWaveOut:
			s.Device = DeviceWaveOut
		case lagVtblWASAPI:
			s.Device = DeviceWASAPI
		}
	}
	return s
}

// readOutputState reads out_kw_ds's output state for the #135 filter. It is
// best-effort and never returns an error: any failed read, short read or
// out-of-range pointer yields OutputState{Valid:false}, so the filter passes
// the core position through and the session is never failed (design §3.3). It
// reads the globals block, the effect ratio, the device object (only through a
// checked pointer) and the ring's fed counter, leaving the #131 sample read
// untouched. The ring/fed read is fatal to Valid: fed must not be reported as a
// false zero, which the filter would read as a fed-fall (a new segment) and use
// to re-pin the position (design §3.3 degrade table). The effect ratio is the
// one exception: it is used only for the WaveOut scale (no effect when ≤ 0), so
// a failed ratio read leaves it zero and keeps Valid.
func readOutputState(read memoryRead, out uintptr) OutputState {
	block, err := read(out+lagOutStateRVA, lagOutStateSize)
	if err != nil || len(block) != lagOutStateSize {
		return OutputState{}
	}
	var ratio float32
	if b, err := read(out+lagEffectRatioRVA, 4); err == nil && len(b) == 4 {
		ratio = math.Float32frombits(getU32(b))
	}
	var dev []byte
	if devPtr := getU32(block[lagStDevPtr:]); lagPtrOK(devPtr) {
		if b, err := read(uintptr(devPtr), lagDevSize); err == nil && len(b) == lagDevSize {
			dev = b
		}
	}
	s := evalOutputState(block, dev, out, ratio)
	if s.Valid {
		// A missing fed is not a zero fed: an invalid ring pointer or a failed/short
		// read invalidates the whole output state rather than reporting Fed = 0.
		ring := getU32(block[lagStRing:])
		if !lagPtrOK(ring) {
			return OutputState{}
		}
		b, err := read(uintptr(ring)+lagRingFed, 4)
		if err != nil || len(b) != 4 {
			return OutputState{}
		}
		s.Fed = getU32(b)
	}
	return s
}

// execSectionHash returns the lowercase hex SHA-256 of the raw data of every
// IMAGE_SCN_MEM_EXECUTE section, concatenated in section table order.
// Timestamps, resources and signatures do not affect it.
func execSectionHash(file []byte) (string, error) {
	f, err := pe.NewFile(bytes.NewReader(file))
	if err != nil {
		return "", err
	}
	// debug/pe also accepts a COFF object, which has no optional header.
	if _, ok := f.OptionalHeader.(*pe.OptionalHeader32); !ok {
		return "", errors.New("not a PE32 image")
	}
	h := sha256.New()
	exec := false
	for _, s := range f.Sections {
		if s.Characteristics&pe.IMAGE_SCN_MEM_EXECUTE == 0 {
			continue
		}
		b, err := s.Data()
		if err != nil {
			return "", err
		}
		h.Write(b)
		exec = true
	}
	if !exec {
		return "", errors.New("no executable section")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// matchLagBuild returns the name of the build whose row holds both hashes.
func matchLagBuild(inHash, outHash string) (string, bool) {
	for _, b := range lagBuilds {
		if b.InHash == inHash && b.OutHash == outHash {
			return b.Name, true
		}
	}
	return "", false
}

// checkImageHeader rejects a module whose mapped image differs from the file
// that was fingerprinted: mem is the first lagHeaderSize bytes of the module
// in memory. TimeDateStamp and SizeOfImage must equal the file's, and
// SizeOfImage must equal modBaseSize.
func checkImageHeader(mem, file []byte, modBaseSize uint32) error {
	ms, msz, err := peStampSize(mem)
	if err != nil {
		return fmt.Errorf("image in memory: %w", err)
	}
	fs, fsz, err := peStampSize(file)
	if err != nil {
		return fmt.Errorf("file: %w", err)
	}
	if ms != fs || msz != fsz || msz != modBaseSize {
		return fmt.Errorf("image in memory (TimeDateStamp %#x, SizeOfImage %#x, ModBaseSize %#x) does not match the file (TimeDateStamp %#x, SizeOfImage %#x)", ms, msz, modBaseSize, fs, fsz)
	}
	return nil
}

// peStampSize returns TimeDateStamp and SizeOfImage from the headers at the
// start of a PE32 file or mapped image.
func peStampSize(b []byte) (stamp, size uint32, err error) {
	if len(b) < 0x40 || string(b[:2]) != "MZ" {
		return 0, 0, errors.New("not a PE32 image")
	}
	p := uint64(getU32(b[0x3c:]))
	// SizeOfImage ends at the signature + 0x54.
	if p+0x54 > uint64(len(b)) || string(b[p:p+4]) != "PE\x00\x00" || getU16(b[p+0x18:]) != 0x10b {
		return 0, 0, errors.New("not a PE32 image")
	}
	return getU32(b[p+0x8:]), getU32(b[p+0x50:]), nil
}

// readLimited reads r to the end, failing if it holds more than limit bytes.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("file larger than %d bytes", limit)
	}
	return b, nil
}

// readLag reads one sample in the order of design §3.2: out_kw_ds state (G1),
// in_ffaudio's two globals, the decoder context, the stream fields, out_kw_ds
// state again (G2); 11 reads, 0x21c bytes (§3.6). Nothing carries over between
// samples: a song change can complete between two samples on the same heap
// addresses with Written not lower, so no key built from addresses and
// counters tells the two files' time bases apart. Every pointer passes
// lagPtrOK before it is read through and every read must be complete. A
// failed or short read returns the error; a pointer or index failing its
// check gives the invalid sample without error.
func readLag(read memoryRead, in, out uintptr) (LagSample, error) {
	g1, ok, err := readOutGlobals(read, out)
	if err != nil || !ok {
		return LagSample{}, err
	}
	running, err := readU32(read, in+lagInRunningRVA)
	if err != nil {
		return LagSample{}, err
	}
	ctxPtr, err := readU32(read, in+lagInCtxRVA)
	if err != nil || !lagPtrOK(ctxPtr) {
		return LagSample{}, err
	}
	ctx, err := readFull(read, uintptr(ctxPtr), lagCtxSize)
	if err != nil {
		return LagSample{}, err
	}
	st, ok, err := readLagStream(read, getU32(ctx[lagCtxFormat:]), int32(getU32(ctx[lagCtxStream:])))
	if err != nil || !ok {
		return LagSample{}, err
	}
	g2, ok, err := readOutGlobals(read, out)
	if err != nil || !ok {
		return LagSample{}, err
	}
	return evalLag(g1, g2, running, ctxPtr, ctx, st), nil
}

// readOutGlobals reads out_kw_ds's globals block and the ring buffer's written
// bytes; ok is false when the ring buffer pointer fails lagPtrOK.
func readOutGlobals(read memoryRead, out uintptr) (g outGlobals, ok bool, err error) {
	b, err := readFull(read, out+lagOutBlockRVA, lagOutBlockSize)
	if err != nil {
		return outGlobals{}, false, err
	}
	g = outGlobals{Base: getU32(b[lagOutBase:]), Bps: getU32(b[lagOutBps:]), Ring: getU32(b[lagOutRing:])}
	if !lagPtrOK(g.Ring) {
		return outGlobals{}, false, nil
	}
	if g.Written, err = readU32(read, uintptr(g.Ring)+lagRingWritten); err != nil {
		return outGlobals{}, false, err
	}
	return g, true, nil
}

// readLagStream reads the AVFormatContext and AVStream fields for stream
// index si, 0x4c bytes in four reads. ok is false when a pointer fails
// lagPtrOK or si is outside [0, nb_streams) with nb_streams ≤ lagMaxStreams.
func readLagStream(read memoryRead, fmtPtr uint32, si int32) (st lagStream, ok bool, err error) {
	if !lagPtrOK(fmtPtr) {
		return lagStream{}, false, nil
	}
	b, err := readFull(read, uintptr(fmtPtr)+lagFmtStreams, 8)
	if err != nil {
		return lagStream{}, false, err
	}
	nb, streams := getU32(b), getU32(b[4:])
	// The uint32 conversion also rejects a negative index.
	if uint32(si) >= nb || nb > lagMaxStreams || !lagPtrOK(streams) {
		return lagStream{}, false, nil
	}
	start, err := readFull(read, uintptr(fmtPtr)+lagFmtStart, 8)
	if err != nil {
		return lagStream{}, false, err
	}
	stPtr, err := readU32(read, uintptr(streams)+uintptr(si)*4)
	if err != nil || !lagPtrOK(stPtr) {
		return lagStream{}, false, err
	}
	s, err := readFull(read, uintptr(stPtr), lagStreamSize)
	if err != nil {
		return lagStream{}, false, err
	}
	return lagStream{
		NbStreams:  nb,
		Index:      int32(getU32(s[lagStIndex:])),
		TbNum:      int32(getU32(s[lagStTbNum:])),
		TbDen:      int32(getU32(s[lagStTbNum+4:])),
		StartTs:    int64(binary.LittleEndian.Uint64(s[lagStStart:])),
		FmtStartUs: int64(binary.LittleEndian.Uint64(start)),
	}, true, nil
}

// readFull reads n bytes at addr; a short read is ErrInvalidSample.
func readFull(read memoryRead, addr uintptr, n int) ([]byte, error) {
	b, err := read(addr, n)
	if err != nil {
		return nil, err
	}
	if len(b) != n {
		return nil, ErrInvalidSample
	}
	return b, nil
}

func readU32(read memoryRead, addr uintptr) (uint32, error) {
	b, err := readFull(read, addr, 4)
	if err != nil {
		return 0, err
	}
	return getU32(b), nil
}

// LagKey identifies the output segment a sample belongs to: the decoder
// context, the ring buffer and the output base set by Open or Flush. A
// decrease in Written under the same key also starts a new segment (a seek to
// the same whole second leaves the base unchanged).
type LagKey struct{ Ctx, Ring, Base uint32 }

// LagSample is one evaluation of the seek lag. Every field other than Valid is
// zero when the sample is invalid.
type LagSample struct {
	Valid   bool
	Key     LagKey
	Written uint32 // bytes written to the ring buffer (+0x8) in this segment
	EUs     int64  // e: how far the core position leads the sound, µs
	DUs     int64  // duration of the last packet taken, µs
}

// outGlobals is one read of out_kw_ds's output state (G1 or G2).
type outGlobals struct{ Base, Bps, Ring, Written uint32 }

// lagStream holds the format and stream fields for the packet's stream_index.
type lagStream struct {
	NbStreams  uint32 // AVFormatContext +0x18
	Index      int32  // AVStream +0x0
	TbNum      int32  // AVStream +0x28
	TbDen      int32  // AVStream +0x2c
	StartTs    int64  // AVStream +0x30, in the stream time base
	FmtStartUs int64  // AVFormatContext +0x420, µs
}

// lagPtrOK accepts a non-zero, 4-byte aligned pointer in [0x10000, 0xFFFF0000).
func lagPtrOK(p uint32) bool { return p%4 == 0 && p >= 0x10000 && p < 0xFFFF0000 }

// evalLag validates one read and computes e. g1 and g2 are out_kw_ds's state
// read before and after the decoder fields, running is in_ffaudio's decoder
// thread flag, ctx the first lagCtxSize bytes of the decoder context at ctxPtr,
// st the stream fields for the packet's stream_index. A read failing any
// condition gives the zero sample.
func evalLag(g1, g2 outGlobals, running uint32, ctxPtr uint32, ctx []byte, st lagStream) LagSample {
	// Equal reads mean the output was not written, flushed or reopened while
	// the decoder fields were read. Written > 0 means this segment has new
	// data, so the packet in the context belongs to it.
	if g1 != g2 || g1.Written == 0 || g1.Bps < lagMinBps || g1.Bps > lagMaxBps {
		return LagSample{}
	}
	if len(ctx) < lagCtxSize || !lagPtrOK(ctxPtr) || !lagPtrOK(g1.Ring) || !lagPtrOK(getU32(ctx[lagCtxFormat:])) {
		return LagSample{}
	}
	if running != 1 || getU32(ctx[lagCtxHeld:]) != 0 {
		return LagSample{}
	}
	// The uint32 conversion also rejects a negative stream_index.
	si := int32(getU32(ctx[lagCtxStream:]))
	if uint32(si) >= st.NbStreams || st.NbStreams > lagMaxStreams || st.Index != si {
		return LagSample{}
	}
	num, den := st.TbNum, st.TbDen
	// tsToUs rejects num < 1 and den < 1.
	if int64(den) < lagMinTbRatio*int64(num) {
		return LagSample{}
	}
	var s int64
	if st.StartTs != avNoPTS || st.FmtStartUs != avNoPTS {
		startUs, ok := tsToUs(st.StartTs, num, den)
		if !ok || !withinUs(startUs, st.FmtStartUs, lagStartTolUs) {
			return LagSample{}
		}
		s = st.FmtStartUs
	}
	p, okP := tsToUs(int64(binary.LittleEndian.Uint64(ctx[lagCtxPTS:])), num, den)
	d, okD := tsToUs(int64(binary.LittleEndian.Uint64(ctx[lagCtxDur:])), num, den)
	if !okP || !okD || d <= 0 || d > lagMaxDUs {
		return LagSample{}
	}
	w := int64(g1.Base)*1000 + int64(g1.Written)*1000000/int64(g1.Bps)
	// Wrapping int64 arithmetic gives the exact e whenever it fits. Since
	// w < 2^43, d ≤ lagMaxDUs and p, s lie in [-lagStartTolUs, MaxInt64], a
	// wrapped result lies near ±2^63, far outside [lagMinEUs, lagMaxEUs].
	e := w - p - d + s
	if e < lagMinEUs || e > lagMaxEUs {
		return LagSample{}
	}
	return LagSample{Valid: true, Key: LagKey{Ctx: ctxPtr, Ring: g1.Ring, Base: g1.Base}, Written: g1.Written, EUs: e, DUs: d}
}

// tsToUs converts ts in time base num/den to µs, rounding down, with a 128-bit
// intermediate product. It rejects a negative ts, num < 1, den < 1 and a
// result above MaxInt64.
func tsToUs(ts int64, num, den int32) (int64, bool) {
	if ts < 0 || num < 1 || den < 1 {
		return 0, false
	}
	hi, lo := bits.Mul64(uint64(ts), uint64(num)*1000000)
	if hi >= uint64(den) {
		return 0, false
	}
	q, _ := bits.Div64(hi, lo, uint64(den))
	if q > math.MaxInt64 {
		return 0, false
	}
	return int64(q), true
}

// withinUs reports whether |a-b| ≤ tol (tol ≥ 0) without overflow.
func withinUs(a, b, tol int64) bool {
	if a < b {
		a, b = b, a
	}
	return uint64(a)-uint64(b) <= uint64(tol)
}

// LagNoteKind tells the caller what the corrector did on a tick, for logging.
type LagNoteKind int

const (
	LagNoteNone    LagNoteKind = iota
	LagNoteAdopted             // a confirmed value was adopted or raised, deducting a non-zero amount
	LagNoteDrift               // correction disabled for the rest of the song
)

// LagNote reports an adoption or a drift. EUs is the adopted value, or for a
// drift the sample that completed it; Key is the segment.
type LagNote struct {
	Kind LagNoteKind
	EUs  int64
	Key  LagKey
}

// LagCorrector deducts the confirmed seek lag from core samples (design
// §3.3). The zero value is ready to use; it is not safe for concurrent use.
type LagCorrector struct {
	have     bool
	rid      string
	eUs      int64 // e_cur: the correction in effect, µs
	applied  int32 // deduction applied to the previous output, ms
	disabled bool  // drift seen: no correction until the RID changes
	seg      lagSegment
}

// lagSegment is the current segment: the one the latest valid sample belongs
// to.
type lagSegment struct {
	ok         bool
	key        LagKey
	written    uint32 // Written of the latest valid sample
	eUs, dUs   int64  // e and D of the latest valid sample
	firstAt    time.Time
	lastAt     time.Time // time of the latest valid sample
	sinceFirst int       // samples after the first one
	sinceLast  int       // samples after the latest valid one, all invalid
	// eligible: first seen during this song and not failed since. A song
	// change or a failure clears it; only a new segment sets it again.
	eligible  bool
	confirmed bool
	adopted   bool  // conf has been the e_cur in effect (Sample.LagAdopted reported)
	conf      int64 // confirmed value, µs; never lowered
	ceil      int64 // highest raise: first confirmed value + that pair's D + tolerance
	drift     int   // consecutive valid samples outside the drift band since adoption
}

// Apply returns core with the correction deducted, PositionMs =
// clamp(core.PositionMs - e_cur rounded to ms, 0, DurationMs), LagAppliedMs
// set to the deduction applied to this output, LagShiftMs set to the
// deduction applied to the previous output minus this one (0 on a song change
// and on the first sample), and LagAdopted set on the first tick on which the
// current segment's confirmed value is the e_cur in effect. All times are
// core.ObservedAt.
func (c *LagCorrector) Apply(core Sample, lag LagSample) (Sample, LagNote) {
	now := core.ObservedAt
	// The first sample and a song change start without correction. The segment
	// current at a song change was first seen during the previous song (the
	// next song's opening segment may already be decoding) and is never
	// adopted; a segment first seen from this tick on may be.
	fresh := !c.have || core.RID != c.rid
	if fresh {
		c.have, c.rid, c.eUs, c.disabled = true, core.RID, 0, false
		c.seg.eligible = false
	}
	note := c.take(lag, now)
	// Mismatch: within one segment e never exceeds the seek target, which is
	// the output base, and the core position is not below the base. A core
	// position below e_cur therefore belongs to another segment: an old e_cur
	// meeting a jump back to about 0, or a new segment confirmed before the core
	// reports its target. Only this tick goes uncorrected; the segment keeps
	// its confirmed value and eligibility, and its next valid sample sets e_cur
	// again. It compares the raw core CoreMs, not PositionMs: the #135 transient
	// filter may pin PositionMs at the base while the raw core overshoots, and
	// judging the pinned value would misfire (design §3.2).
	if int64(core.CoreMs) < lagMs(c.eUs) {
		c.eUs = 0
	}
	// Timers are judged after the tick's sample is taken, so a sample that
	// confirms or arrives on the deadline tick counts.
	s := &c.seg
	if s.ok && s.sinceLast >= lagTimerSamples && now.Sub(s.lastAt) >= lagFailAfter {
		c.eUs, s.eligible = 0, false
	}
	if s.ok && !s.confirmed && s.sinceFirst >= lagTimerSamples && now.Sub(s.firstAt) >= lagUnconfirmedAfter {
		c.eUs = 0
	}
	// Judged after the mismatch rule and the timers: the confirmed value must
	// be what this output deducts. Once per segment, so a raise is not a first;
	// the value may equal the previous one, in which case nothing else tells
	// the session the front end may be off by a silent seek.
	adopted := s.ok && s.eligible && s.confirmed && !c.disabled && !s.adopted && c.eUs == s.conf
	if adopted {
		s.adopted = true
	}
	out := core
	out.LagAdopted = adopted
	out.PositionMs = int32(min(max(int64(core.PositionMs)-lagMs(c.eUs), 0), int64(core.DurationMs)))
	applied := core.PositionMs - out.PositionMs
	out.LagAppliedMs = applied
	out.LagShiftMs = 0
	if !fresh {
		out.LagShiftMs = c.applied - applied
	}
	c.applied = applied
	return out, note
}

// take records one sample in the current segment, confirming, raising,
// adopting and checking drift.
func (c *LagCorrector) take(lag LagSample, now time.Time) LagNote {
	s := &c.seg
	s.sinceFirst++
	if !lag.Valid {
		s.sinceLast++
		return LagNote{}
	}
	// A new key is a seek or a reopened output; Written going back under the
	// same key is a seek to the same whole second.
	if !s.ok || lag.Key != s.key || lag.Written < s.written {
		*s = lagSegment{ok: true, key: lag.Key, written: lag.Written, eUs: lag.EUs, dUs: lag.DUs, firstAt: now, lastAt: now, eligible: true}
		return LagNote{}
	}
	// Two adjacent valid samples within the tolerance give the larger one: a
	// sample can fall short by one packet but never exceed. Two equal short
	// samples confirm a value one packet low, so a later agreeing pair may
	// raise it by at least the tolerance, up to the first confirmed value plus
	// that pair's D plus the tolerance.
	pairE, pairD := max(s.eUs, lag.EUs), max(s.dUs, lag.DUs)
	agree := withinUs(s.eUs, lag.EUs, lagConfirmTolUs)
	s.written, s.eUs, s.dUs, s.lastAt, s.sinceLast = lag.Written, lag.EUs, lag.DUs, now, 0
	updated := false
	switch {
	case !agree:
	case !s.confirmed:
		s.confirmed, s.conf, s.ceil, updated = true, pairE, pairE+pairD+lagConfirmTolUs, true
	case pairE >= s.conf+lagConfirmTolUs && pairE <= s.ceil:
		s.conf, updated = pairE, true
	}
	if !s.eligible || c.disabled || !s.confirmed {
		return LagNote{}
	}
	// Adopted. Samples outside [conf - (D + margin), conf + D + margin] mean
	// the model does not hold (for example a changed playback rate).
	if withinUs(lag.EUs, s.conf, lag.DUs+lagDriftMarginUs) {
		s.drift = 0
	} else if s.drift++; s.drift >= lagDriftCount {
		c.disabled, c.eUs = true, 0
		return LagNote{Kind: LagNoteDrift, EUs: lag.EUs, Key: s.key}
	}
	c.eUs = s.conf
	if updated && lagMs(s.conf) != 0 {
		return LagNote{Kind: LagNoteAdopted, EUs: s.conf, Key: s.key}
	}
	return LagNote{}
}

// lagMs rounds µs to the nearest ms, halves away from zero.
func lagMs(us int64) int64 { return int64(math.Round(float64(us) / 1000)) }
