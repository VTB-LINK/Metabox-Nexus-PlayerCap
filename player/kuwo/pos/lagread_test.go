package pos

// This file tests only the per-sample read of the seek-lag fields: read order
// and sizes, pointer checks before each dereference, incomplete reads, and
// that nothing carries over between samples (design §3.2, §3.6). Memory is a
// fake 32-bit address space; the field offsets are written out here
// independently of lag.go.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

const (
	flIn      = 0x6e000000 // in_ffaudio.dll base
	flOut     = 0x6f000000 // out_kw_ds.dll base
	flBlock   = flOut + 0x1a8a4
	flRunning = flIn + 0xa044
	flCtxVar  = flIn + 0xa4f4
	flRing    = 0x06120a00
	flCtx     = 0x05a32e40
	flFmt     = 0x05b00020
	flStreams = 0x05b10000
	flStream  = 0x05b20000
)

type lagRead struct {
	addr uintptr
	n    int
}

// fakeLagMem holds disjoint readable regions and logs every read. failN and
// shortN make the read with that index fail or return one byte short; after
// runs once the read with index afterN has returned.
type fakeLagMem struct {
	regions map[uintptr][]byte
	log     []lagRead
	failN   int
	shortN  int
	afterN  int
	after   func(m *fakeLagMem)
}

func (m *fakeLagMem) read(addr uintptr, n int) ([]byte, error) {
	i := len(m.log)
	m.log = append(m.log, lagRead{addr, n})
	if m.after != nil && i == m.afterN {
		defer m.after(m)
	}
	if i == m.failN {
		return nil, errFakeRead
	}
	for start, b := range m.regions {
		if addr >= start && addr+uintptr(n) <= start+uintptr(len(b)) {
			out := make([]byte, n)
			copy(out, b[addr-start:])
			if i == m.shortN {
				out = out[:n-1]
			}
			return out, nil
		}
	}
	return nil, errFakeRead
}

// at returns the bytes at addr inside a region.
func (m *fakeLagMem) at(addr uintptr) []byte {
	for start, b := range m.regions {
		if addr >= start && addr < start+uintptr(len(b)) {
			return b[addr-start:]
		}
	}
	panic(fmt.Sprintf("no region at %#x", addr))
}

func (m *fakeLagMem) u32(addr uintptr, v uint32) { binary.LittleEndian.PutUint32(m.at(addr), v) }
func (m *fakeLagMem) u64(addr uintptr, v uint64) { binary.LittleEndian.PutUint64(m.at(addr), v) }

// region adds a readable region at addr, copying size bytes from src if any.
func (m *fakeLagMem) region(addr uintptr, size int, src uintptr) {
	b := make([]byte, size)
	if src != 0 {
		copy(b, m.at(src))
	}
	m.regions[addr] = b
}

func (m *fakeLagMem) readsAt(addr uintptr) bool {
	for _, r := range m.log {
		if r.addr == addr {
			return true
		}
	}
	return false
}

// newFakeLag lays out the run-9 MP3 read after a seek (lageval_test.go
// mp3Run9): e = 16894 µs. Regions around each object are larger than the
// fields read, so a missing check reads instead of failing.
func newFakeLag() *fakeLagMem {
	m := &fakeLagMem{regions: map[uintptr][]byte{}, failN: -1, shortN: -1, afterN: -1}
	m.region(flBlock, 0x60, 0)
	m.region(flRing, 0x10, 0)
	m.region(flIn+0xa000, 0x600, 0)
	m.region(flCtx, 0x200, 0)
	m.region(flFmt, 0x430, 0)
	m.region(flStreams-0x100, 0x300, 0)
	m.region(flStream, 0x100, 0)
	m.u32(flBlock+0x0, 6000)    // base, ms
	m.u32(flBlock+0x24, 176400) // byte rate
	m.u32(flBlock+0x5c, flRing)
	m.u32(flRing+0x8, 368640) // written
	m.u32(flRunning, 1)
	m.u32(flCtxVar, flCtx)
	m.u32(flCtx+0x0, flFmt)
	m.u64(flCtx+0x70, 113909760) // pts
	m.u32(flCtx+0x88, 0)         // stream_index
	m.u64(flCtx+0x98, 368640)    // duration
	m.u32(flFmt+0x18, 1)         // nb_streams
	m.u32(flFmt+0x1c, flStreams)
	m.u64(flFmt+0x420, 25057) // start_time, µs
	m.u32(flStreams, flStream)
	m.u32(flStream+0x0, 0)         // index
	m.u32(flStream+0x28, 1)        // time base num
	m.u32(flStream+0x2c, 14112000) // time base den
	m.u64(flStream+0x30, 353600)   // start_time
	return m
}

func (m *fakeLagMem) readLag() (LagSample, error) {
	m.log = nil
	return readLag(m.read, flIn, flOut)
}

// sampleReads is the read sequence of every sample.
var sampleReads = []lagRead{
	{flBlock, 0x60}, {flRing + 8, 4}, // G1
	{flRunning, 4}, {flCtxVar, 4},
	{flCtx, 0x100},
	{flFmt + 0x18, 8}, {flFmt + 0x420, 8}, {flStreams, 4}, {flStream, 0x38},
	{flBlock, 0x60}, {flRing + 8, 4}, // G2
}

func equalReads(a, b []lagRead) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReadLagSequence(t *testing.T) {
	m := newFakeLag()
	s, err := m.readLag()
	if err != nil || !s.Valid || !near(s.EUs, 16894, 2) {
		t.Fatalf("got %+v, %v; want valid with e within 2 µs of 16894", s, err)
	}
	if want := (LagKey{Ctx: flCtx, Ring: flRing, Base: 6000}); s.Key != want || s.Written != 368640 {
		t.Errorf("Key %+v Written %d, want %+v 368640", s.Key, s.Written, want)
	}
	if !equalReads(m.log, sampleReads) {
		t.Fatalf("reads %x\nwant  %x", m.log, sampleReads)
	}
	// §3.6: 11 reads, 0x21c bytes, of which the stream fields are 0x4c.
	total, stream := 0, 0
	for i, r := range sampleReads {
		total += r.n
		if i >= 5 && i < 9 {
			stream += r.n
		}
	}
	if len(sampleReads) != 11 || total != 0x21c || stream != 0x4c {
		t.Errorf("%d reads, %#x bytes, stream fields %#x; want 11, 0x21c, 0x4c", len(sampleReads), total, stream)
	}
}

// Nothing carries over between samples: paused (memory unchanged) or playing,
// every sample reads the stream fields again.
func TestReadLagReadsStreamFieldsEverySample(t *testing.T) {
	m := newFakeLag()
	for i, step := range []struct{ written, pts uint32 }{{0, 0}, {0, 0}, {1764, 141120}} {
		m.u32(flRing+8, 368640+step.written)
		m.u64(flCtx+0x70, 113909760+uint64(step.pts))
		s, err := m.readLag()
		if err != nil || !s.Valid || !near(s.EUs, 16894, 2) {
			t.Fatalf("sample %d: got %+v, %v", i, s, err)
		}
		if !equalReads(m.log, sampleReads) {
			t.Fatalf("sample %d: reads %x\nwant  %x", i, m.log, sampleReads)
		}
	}
}

// A song change completed between two samples (design review counterexample):
// the new file reuses the decoder context, AVFormatContext and ring buffer;
// base and stream_index stay 0, running is 1, Written is 176400 and both
// start_time are 0 in both samples. Old file: 192000 B/s, time base 1/48000,
// P = 43140, D = 960, true e = 0. New file: 176400 B/s, time base 1/44100,
// P = 43076, D = 1024, true e = 0 (1 µs after rounding down). Computed with the
// old time base the second sample would be valid with e ≈ 81251 µs. The second
// case keeps the byte rate at 176400 B/s (old P = 47040), so that a key that
// includes the byte rate cannot tell the files apart either.
func TestReadLagSongChangeBetweenSamples(t *testing.T) {
	for _, tc := range []struct {
		name   string
		oldBps uint32
		oldPts uint64
	}{{"review counterexample", 192000, 43140}, {"same byte rate", 176400, 47040}} {
		t.Run(tc.name, func(t *testing.T) {
			m := newFakeLag()
			file := func(bps, den uint32, pts, dur uint64) {
				m.u32(flBlock+0x24, bps)
				m.u32(flStream+0x2c, den)
				m.u64(flCtx+0x70, pts)
				m.u64(flCtx+0x98, dur)
			}
			m.u32(flBlock+0x0, 0)
			m.u32(flRing+8, 176400)
			m.u64(flFmt+0x420, 0)
			m.u64(flStream+0x30, 0)
			file(tc.oldBps, 48000, tc.oldPts, 960)
			s, err := m.readLag()
			if err != nil || !s.Valid || !near(s.EUs, 0, 2) || s.DUs != 20000 {
				t.Fatalf("old file: got %+v, %v; want valid, e within 2 µs of 0, D 20000 µs", s, err)
			}
			first := s
			file(176400, 44100, 43076, 1024)
			s, err = m.readLag()
			if err != nil || !s.Valid || !near(s.EUs, 0, 2) || s.DUs != 23219 {
				t.Fatalf("new file: got %+v, %v; want valid from time base 1/44100: e within 2 µs of 0, D 23219 µs", s, err)
			}
			// The two samples are indistinguishable by segment key and Written.
			if s.Key != first.Key || s.Written != first.Written {
				t.Fatalf("setup: keys %+v %+v, written %d %d; want equal", first.Key, s.Key, first.Written, s.Written)
			}
		})
	}
}

// Each case puts a pointer that fails lagPtrOK (or an index out of bounds)
// over readable memory: the read through it must not happen.
func TestReadLagChecksPointersBeforeReading(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(m *fakeLagMem)
		forbidden uintptr
	}{
		{"ring zero", func(m *fakeLagMem) { m.region(0, 0x10, 0); m.u32(flBlock+0x5c, 0) }, 8},
		{"ring below 0x10000", func(m *fakeLagMem) { m.region(0xfff0, 0x20, 0); m.u32(flBlock+0x5c, 0xfffc) }, 0x10004},
		{"ring at 0xFFFF0000", func(m *fakeLagMem) { m.region(0xffff0000, 0x10, 0); m.u32(flBlock+0x5c, 0xffff0000) }, 0xffff0008},
		{"ring misaligned", func(m *fakeLagMem) { m.u32(flBlock+0x5c, flRing+2) }, flRing + 10},
		{"context zero", func(m *fakeLagMem) { m.region(0, 0x100, 0); m.u32(flCtxVar, 0) }, 0},
		{"context misaligned", func(m *fakeLagMem) { m.u32(flCtxVar, flCtx+2) }, flCtx + 2},
		{"AVFormatContext misaligned", func(m *fakeLagMem) { m.u32(flCtx+0x0, flFmt+2) }, flFmt + 2 + 0x18},
		{"streams misaligned", func(m *fakeLagMem) { m.u32(flFmt+0x1c, flStreams+2) }, flStreams + 2},
		{"AVStream misaligned", func(m *fakeLagMem) { m.u32(flStreams, flStream+2) }, flStream + 2},
		{"stream_index equals nb_streams", func(m *fakeLagMem) { m.u32(flCtx+0x88, 1) }, flStreams + 4},
		{"stream_index negative", func(m *fakeLagMem) { m.u32(flCtx+0x88, 0xffffffff) }, flStreams - 4},
		{"nb_streams zero", func(m *fakeLagMem) { m.u32(flFmt+0x18, 0) }, flStreams},
		{"nb_streams 65", func(m *fakeLagMem) { m.u32(flFmt+0x18, 65); m.u32(flCtx+0x88, 64) }, flStreams + 256},
		// G2 sees a different ring pointer: written into memory after the
		// AVStream read.
		{"G2 ring misaligned", func(m *fakeLagMem) {
			m.afterN = 8
			m.after = func(m *fakeLagMem) { m.u32(flBlock+0x5c, flRing+2) }
		}, flRing + 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newFakeLag()
			tc.mutate(m)
			s, err := m.readLag()
			if err != nil || s.Valid {
				t.Fatalf("got %+v, %v; want invalid without error", s, err)
			}
			if m.readsAt(tc.forbidden) {
				t.Fatalf("read through unchecked pointer at %#x: %x", tc.forbidden, m.log)
			}
		})
	}
}

// A read that fails or comes back short makes the sample invalid and returns
// an error, at every step of the sequence.
func TestReadLagIncompleteReads(t *testing.T) {
	for i, r := range sampleReads {
		for _, short := range []bool{false, true} {
			t.Run(fmt.Sprintf("read %d at %#x short=%v", i, r.addr, short), func(t *testing.T) {
				m := newFakeLag()
				if short {
					m.shortN = i
				} else {
					m.failN = i
				}
				s, err := m.readLag()
				if s.Valid || s != (LagSample{}) {
					t.Fatalf("sample %+v, want the zero sample", s)
				}
				if short && !errors.Is(err, ErrInvalidSample) || !short && !errors.Is(err, errFakeRead) {
					t.Fatalf("err %v", err)
				}
			})
		}
	}
}

// A write between G1 and G2 (design §4, the reviewed counterexample) is caught
// only if G2 is read after the decoder fields.
func TestReadLagWriteDuringRead(t *testing.T) {
	m := newFakeLag()
	m.afterN = 4 // the decoder context read
	m.after = func(m *fakeLagMem) { m.u32(flRing+8, 368640+3840) }
	if s, err := m.readLag(); err != nil || s.Valid {
		t.Fatalf("got %+v, %v; want invalid", s, err)
	}
}
