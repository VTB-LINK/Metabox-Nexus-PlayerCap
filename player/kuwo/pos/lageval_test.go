package pos

import (
	"encoding/binary"
	"math"
	"testing"
)

// This file tests only the seek-lag evaluation: field validation and the
// computation of e from raw fields (doc/design/kuwo-seek-lag-design.md §1.3,
// §3.2, §4). Field values of the run-9 cases are taken from that run's dec.csv.

// lagInput is one read as evalLag receives it.
type lagInput struct {
	g1, g2  outGlobals
	running uint32
	ctxPtr  uint32
	ctx     []byte
	st      lagStream
}

func (in lagInput) eval() LagSample {
	return evalLag(in.g1, in.g2, in.running, in.ctxPtr, in.ctx, in.st)
}

// Decoder context offsets are written out here independently of lag.go, so a
// changed offset constant fails these tests.
func (in *lagInput) setFormat(p uint32) { binary.LittleEndian.PutUint32(in.ctx[0x0:], p) }
func (in *lagInput) setPTS(v int64)     { binary.LittleEndian.PutUint64(in.ctx[0x70:], uint64(v)) }
func (in *lagInput) setStream(v int32)  { binary.LittleEndian.PutUint32(in.ctx[0x88:], uint32(v)) }
func (in *lagInput) setDur(v int64)     { binary.LittleEndian.PutUint64(in.ctx[0x98:], uint64(v)) }
func (in *lagInput) setHeld(v uint32)   { binary.LittleEndian.PutUint32(in.ctx[0xf8:], v) }

// setOut applies f to both G1 and G2, keeping them equal.
func (in *lagInput) setOut(f func(*outGlobals)) { f(&in.g1); f(&in.g2) }

func newLagInput(g outGlobals, pts, dur int64, st lagStream) lagInput {
	in := lagInput{g1: g, g2: g, running: 1, ctxPtr: 0x05a32e40, ctx: make([]byte, 0x100), st: st}
	in.setFormat(0x05b00020)
	in.setPTS(pts)
	in.setDur(dur)
	return in
}

// mp3Run9 is the run-9 MP3 read after a seek: W 8089795, P 8071836, D 26122,
// S 25057 µs, so e = 16894 µs.
func mp3Run9() lagInput {
	return newLagInput(outGlobals{Base: 6000, Bps: 176400, Ring: 0x06120a00, Written: 368640}, 113909760, 368640,
		lagStream{NbStreams: 1, TbNum: 1, TbDen: 14112000, StartTs: 353600, FmtStartUs: 25057})
}

func near(got, want, tol int64) bool { return got >= want-tol && got <= want+tol }

func TestEvalLagVorbisRun9(t *testing.T) {
	in := newLagInput(outGlobals{Base: 6000, Bps: 176400, Ring: 0x06120a00, Written: 322048}, 332608, 1024,
		lagStream{NbStreams: 1, TbNum: 1, TbDen: 44100})
	if got := in.eval(); !got.Valid || !near(got.EUs, 260317, 2) {
		t.Fatalf("got %+v, want valid with e within 2 µs of 260317", got)
	}
}

func TestEvalLagMP3Run9(t *testing.T) {
	got := mp3Run9().eval()
	if !got.Valid || !near(got.EUs, 16894, 2) {
		t.Fatalf("got %+v, want valid with e within 2 µs of 16894", got)
	}
	if want := (LagKey{Ctx: 0x05a32e40, Ring: 0x06120a00, Base: 6000}); got.Key != want {
		t.Errorf("Key = %+v, want %+v", got.Key, want)
	}
	if got.Written != 368640 || got.DUs != 26122 {
		t.Errorf("Written = %d, DUs = %d; want 368640, 26122", got.Written, got.DUs)
	}
}

func TestEvalLagMP3StartSegment(t *testing.T) {
	in := mp3Run9()
	in.setOut(func(g *outGlobals) { g.Base, g.Written = 0, 299708 })
	in.setPTS(23961600)
	if got := in.eval(); !got.Valid || !near(got.EUs, 0, 2) {
		t.Fatalf("got %+v, want valid with |e| ≤ 2 µs", got)
	}
}

// TestEvalLagRejects breaks each condition of design §3.2 in turn. Where the
// change would also move e out of range, another field compensates, so each
// case fails on its own condition only.
func TestEvalLagRejects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*lagInput)
	}{
		{"context read short", func(in *lagInput) { in.ctx = in.ctx[:0xff] }},
		{"context pointer zero", func(in *lagInput) { in.ctxPtr = 0 }},
		{"context pointer unaligned", func(in *lagInput) { in.ctxPtr = 0x05a32e42 }},
		{"ring pointer zero", func(in *lagInput) { in.setOut(func(g *outGlobals) { g.Ring = 0 }) }},
		{"ring pointer unaligned", func(in *lagInput) { in.setOut(func(g *outGlobals) { g.Ring = 0x06120a01 }) }},
		{"format pointer zero", func(in *lagInput) { in.setFormat(0) }},
		{"format pointer unaligned", func(in *lagInput) { in.setFormat(0x05b00022) }},
		{"decoder not running", func(in *lagInput) { in.running = 0 }},
		{"running flag 2", func(in *lagInput) { in.running = 2 }},
		{"packet still held", func(in *lagInput) { in.setHeld(1) }},
		{"base differs", func(in *lagInput) { in.g2.Base++ }},
		{"ring pointer differs", func(in *lagInput) { in.g2.Ring += 4 }},
		{"byte rate differs", func(in *lagInput) { in.g2.Bps++ }},
		// Review counterexample: G1 is read before the decoder takes the next
		// packet (P 5786 ms, D 20 ms), G2 after its data is written. e from G1
		// would be one packet short; TestEvalLagAcceptsBounds shows the same
		// fields with equal G1 and G2 are valid.
		{"written grows during read", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Base, g.Written = 5991, 19200 })
			in.g2.Written = 23040
			in.setPTS(5786 * 14112)
			in.setDur(20 * 14112)
		}},
		{"written shrinks during read", func(in *lagInput) { in.g2.Written -= 4096 }},
		{"nothing written", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Base, g.Written = 8090, 0 })
		}},
		{"byte rate 999", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Bps, g.Written = 999, 2090 })
		}},
		{"byte rate 1536001", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Bps, g.Written = 1536001, 3209926 })
		}},
		{"stream index equals nb_streams", func(in *lagInput) { in.setStream(1); in.st.Index = 1 }},
		{"stream index negative", func(in *lagInput) { in.setStream(-1); in.st.Index = -1 }},
		{"nb_streams 0", func(in *lagInput) { in.st.NbStreams = 0 }},
		{"nb_streams 65", func(in *lagInput) { in.st.NbStreams = 65 }},
		{"stream index mismatch", func(in *lagInput) { in.st.NbStreams = 2; in.st.Index = 1 }},
		{"time base num 0", func(in *lagInput) { in.st.TbNum = 0 }},
		{"time base num negative", func(in *lagInput) { in.st.TbNum = -1 }},
		{"time base den 0", func(in *lagInput) { in.st.TbDen = 0 }},
		{"time base den/num 999", func(in *lagInput) {
			in.st.TbDen, in.st.StartTs = 999, 25
			in.setPTS(8064)
			in.setDur(26)
		}},
		{"start times 1001 µs apart, format later", func(in *lagInput) { in.st.FmtStartUs = 26057 }},
		{"start times 1001 µs apart, format earlier", func(in *lagInput) { in.st.FmtStartUs = 24055 }},
		{"only stream start time unset", func(in *lagInput) { in.st.StartTs, in.st.FmtStartUs = avNoPTS, 0 }},
		{"only format start time unset", func(in *lagInput) { in.st.FmtStartUs = avNoPTS }},
		{"stream start time negative", func(in *lagInput) { in.st.StartTs, in.st.FmtStartUs = -1, 0 }},
		// With 64-bit subtraction the two start times below differ by -900 µs
		// after wrapping, and e wraps into range as well.
		{"start times wrap around int64", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Base, g.Written = 0, 176400 })
			in.st.TbDen = 1000
			in.st.StartTs, in.st.FmtStartUs = 9223372036854775, math.MinInt64+92
			in.setPTS(9223372036854775)
			in.setDur(26)
		}},
		// Segment start (W 10000 µs), so that P read as 0 would give e in range.
		{"pts unset", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Base, g.Written = 0, 1764 })
			in.setPTS(avNoPTS)
		}},
		{"pts negative", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Base, g.Written = 0, 1764 })
			in.setPTS(-1)
		}},
		{"duration 0", func(in *lagInput) { in.setDur(0) }},
		{"duration negative", func(in *lagInput) { in.setDur(-14112) }},
		{"duration 501 ms", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Base = 6475 })
			in.setDur(501 * 14112)
		}},
		{"e -201 ms", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Base = 5782 })
			in.st.FmtStartUs = 25163
		}},
		{"e 2001 ms", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Base = 7985 })
			in.st.FmtStartUs = 24163
		}},
	}
	if got := mp3Run9().eval(); !got.Valid {
		t.Fatalf("baseline invalid: %+v", got)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := mp3Run9()
			c.mut(&in)
			if got := in.eval(); got != (LagSample{}) {
				t.Errorf("got %+v, want the zero (invalid) sample", got)
			}
		})
	}
}

// TestEvalLagAcceptsBounds pins the inclusive bounds and the special cases
// that must stay valid, with the exact e each one gives.
func TestEvalLagAcceptsBounds(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*lagInput)
		wantE int64
	}{
		{"both start times unset", func(in *lagInput) { in.st.StartTs, in.st.FmtStartUs = avNoPTS, avNoPTS }, 16894 - 25057},
		{"start times 1000 µs apart, format later", func(in *lagInput) { in.st.FmtStartUs = 26056 }, 17893},
		{"start times 1000 µs apart, format earlier", func(in *lagInput) { in.st.FmtStartUs = 24056 }, 15893},
		{"time base den/num 1000", func(in *lagInput) {
			in.st.TbDen, in.st.StartTs = 1000, 25
			in.setPTS(8072)
			in.setDur(26)
		}, 16852},
		{"64 streams, last index", func(in *lagInput) { in.st.NbStreams = 64; in.st.Index = 63; in.setStream(63) }, 16894},
		{"byte rate 1000", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Bps, g.Written = 1000, 2090 })
		}, 17099},
		{"byte rate 1536000", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Bps, g.Written = 1536000, 3209926 })
		}, 16894},
		{"duration 500 ms", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Base = 6474 })
			in.setDur(500 * 14112)
		}, 17016},
		{"e -200 ms", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Base = 5783 })
			in.st.FmtStartUs = 25163
		}, -200000},
		{"e 2000 ms", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Base = 7984 })
			in.st.FmtStartUs = 24163
		}, 2000000},
		{"review counterexample with equal G1 and G2", func(in *lagInput) {
			in.setOut(func(g *outGlobals) { g.Base, g.Written = 5991, 19200 })
			in.setPTS(5786 * 14112)
			in.setDur(20 * 14112)
		}, 318900},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := mp3Run9()
			c.mut(&in)
			if got := in.eval(); !got.Valid || got.EUs != c.wantE {
				t.Errorf("got %+v, want valid with e = %d", got, c.wantE)
			}
		})
	}
}

func TestEvalLagPtrBounds(t *testing.T) {
	for _, c := range []struct {
		p    uint32
		want bool
	}{
		{0, false},
		{0xFFFF, false},
		{0x10000, true},
		{0x10002, false},
		{0xFFFEFFFC, true},
		{0xFFFF0000, false},
		{0xFFFFFFFC, false},
	} {
		if got := lagPtrOK(c.p); got != c.want {
			t.Errorf("lagPtrOK(%#x) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestTsToUsLarge(t *testing.T) {
	if got, ok := tsToUs(86400*14112000, 1, 14112000); !ok || got != 86400000000 {
		t.Errorf("24 h at 1/14112000 = %d, %v; want 86400000000, true", got, ok)
	}
	if got, ok := tsToUs(1<<62, 1, 1); ok {
		t.Errorf("2^62 at 1/1 = %d, true; want overflow", got)
	}
	// The 128-bit quotient fits in 64 bits but exceeds MaxInt64.
	if got, ok := tsToUs(1<<62, 1, 400000); ok {
		t.Errorf("2^62 at 1/400000 = %d, true; want overflow", got)
	}
}

func TestTsToUsExact(t *testing.T) {
	for _, c := range []struct {
		ts       int64
		num, den int32
		want     int64
	}{
		{0, 1, 14112000, 0},
		{353600, 1, 14112000, 25056}, // rounds down from 25056.69
		{3, 2, 1000, 6000},
		{math.MaxInt64 / 1000, 1, 1000, math.MaxInt64 / 1000 * 1000},
	} {
		if got, ok := tsToUs(c.ts, c.num, c.den); !ok || got != c.want {
			t.Errorf("tsToUs(%d, %d, %d) = %d, %v; want %d, true", c.ts, c.num, c.den, got, ok, c.want)
		}
	}
}

func TestTsToUsRejects(t *testing.T) {
	for _, c := range []struct {
		ts       int64
		num, den int32
	}{
		{-1, 1, 1000},
		{avNoPTS, 1, 1000},
		// With den above the high word of the product, a negative ts read as
		// unsigned would give a quotient that fits in int64.
		{-1, 1, 14112000},
		{avNoPTS, 1, 14112000},
		{5, 0, 1000},
		{5, -1, 1000},
		{5, 1, 0},
		{5, 1, -5},
	} {
		if got, ok := tsToUs(c.ts, c.num, c.den); ok {
			t.Errorf("tsToUs(%d, %d, %d) = %d, true; want false", c.ts, c.num, c.den, got)
		}
	}
}
