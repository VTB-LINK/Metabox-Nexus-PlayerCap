package pos

// This file tests the #135 output-state read: parsing out_kw_ds's globals and
// the device object into an OutputState, telling the device apart by its
// vtable RVA, and the best-effort read sequence. Field offsets are written out
// here independently of lag.go so a wrong offset there is caught.

import (
	"encoding/binary"
	"math"
	"testing"
)

const (
	tOutBase        = 0x6f000000 // out_kw_ds.dll base used in these tests
	tOutStateRVA    = 0x1a87c    // globals block start
	tOutStateSize   = 0x98
	tEffectRatioRVA = 0x1a014 // effect-rate multiplier, float32

	// Block field RVAs (absolute, out_kw_ds relative).
	tfA87c     = 0x1a87c
	tfStaging  = 0x1a888
	tfEffectOn = 0x1a88c
	tfDevPtr   = 0x1a894
	tfBase     = 0x1a8a4
	tfBps      = 0x1a8c8
	tfRing     = 0x1a900
	tRingFed   = 0xc

	// Device object fields.
	tDevSize     = 0x40
	tDevVtable   = 0x0
	tDevBlkAlign = 0x1c
	tDevHdrW     = 0x38
	tDevHdrD     = 0x3c

	// Device vtable RVAs.
	tVtblDS      = 0x17000
	tVtblWaveOut = 0x173c0
	tVtblWASAPI  = 0x17360
)

// outBlock builds a globals block; devPtr and ring are absolute addresses.
func outBlock(base, bps, staging, a87c, effectOn, devPtr, ring uint32) []byte {
	b := make([]byte, tOutStateSize)
	put := func(rva int, v uint32) { binary.LittleEndian.PutUint32(b[rva-tOutStateRVA:], v) }
	put(tfBase, base)
	put(tfBps, bps)
	put(tfStaging, staging)
	put(tfA87c, a87c)
	put(tfEffectOn, effectOn)
	put(tfDevPtr, devPtr)
	put(tfRing, ring)
	return b
}

// devObject builds a device object whose vtable sits at outBase+rva.
func devObject(outBase uintptr, rva uint32, blkAlign uint16, hdrW, hdrD uint32) []byte {
	d := make([]byte, tDevSize)
	binary.LittleEndian.PutUint32(d[tDevVtable:], uint32(outBase)+rva)
	binary.LittleEndian.PutUint16(d[tDevBlkAlign:], blkAlign)
	binary.LittleEndian.PutUint32(d[tDevHdrW:], hdrW)
	binary.LittleEndian.PutUint32(d[tDevHdrD:], hdrD)
	return d
}

// Each vtable RVA maps to its device; any other RVA is DeviceUnknown. All of
// them read a valid device pointer, so Valid stays true (no transient evidence
// is only signalled by Valid=false, which these do not set).
func TestEvalOutputStateDeviceKind(t *testing.T) {
	const devPtr = 0x02000000
	for _, tc := range []struct {
		name string
		rva  uint32
		want DeviceKind
	}{
		{"DirectSound", tVtblDS, DeviceDirectSound},
		{"WaveOut", tVtblWaveOut, DeviceWaveOut},
		{"WASAPI", tVtblWASAPI, DeviceWASAPI},
		{"unknown low", 0x1000, DeviceUnknown},
		{"unknown high", 0x40000, DeviceUnknown},
		{"one below DS", tVtblDS - 1, DeviceUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := outBlock(6000, 176400, 0, 0, 0, devPtr, 0)
			dev := devObject(tOutBase, tc.rva, 0, 0, 0)
			s := evalOutputState(block, dev, tOutBase, 0)
			if !s.Valid {
				t.Fatalf("Valid=false, want true")
			}
			if s.Device != tc.want {
				t.Errorf("Device=%d, want %d", s.Device, tc.want)
			}
		})
	}
}

// A vtable below the module base cannot be an RVA: DeviceUnknown, still valid.
func TestEvalOutputStateVtableBelowBase(t *testing.T) {
	block := outBlock(0, 176400, 0, 0, 0, 0x02000000, 0)
	dev := make([]byte, tDevSize)
	binary.LittleEndian.PutUint32(dev[tDevVtable:], uint32(tOutBase)-4)
	s := evalOutputState(block, dev, tOutBase, 0)
	if !s.Valid || s.Device != DeviceUnknown {
		t.Fatalf("got Valid=%v Device=%d, want true DeviceUnknown", s.Valid, s.Device)
	}
}

// WaveOut's header counts and nBlockAlign come out of the device object.
func TestEvalOutputStateWaveOutFields(t *testing.T) {
	block := outBlock(6000, 176400, 0, 0, 0, 0x02000000, 0)
	dev := devObject(tOutBase, tVtblWaveOut, 4, 11, 7)
	s := evalOutputState(block, dev, tOutBase, 0)
	if !s.Valid || s.Device != DeviceWaveOut {
		t.Fatalf("got Valid=%v Device=%d", s.Valid, s.Device)
	}
	if s.BlkAlign != 4 || s.HdrW != 11 || s.HdrD != 7 {
		t.Errorf("BlkAlign=%d HdrW=%d HdrD=%d, want 4 11 7", s.BlkAlign, s.HdrW, s.HdrD)
	}
}

// The remaining scalar fields are parsed from the block.
func TestEvalOutputStateFields(t *testing.T) {
	block := outBlock(31296, 176400, 441, 3, 0, 0x02000000, 0)
	dev := devObject(tOutBase, tVtblDS, 0, 0, 0)
	s := evalOutputState(block, dev, tOutBase, 0)
	if !s.Valid {
		t.Fatalf("Valid=false")
	}
	if s.Base != 31296 || s.Bps != 176400 || s.Staging != 441 || s.A87c != 3 {
		t.Errorf("Base=%d Bps=%d Staging=%d A87c=%d, want 31296 176400 441 3", s.Base, s.Bps, s.Staging, s.A87c)
	}
}

// A zero byte rate leaves no usable state: Valid is false. The counterexample
// keeps everything else and only raises bps above zero to get Valid=true.
func TestEvalOutputStateBpsZero(t *testing.T) {
	dev := devObject(tOutBase, tVtblDS, 0, 0, 0)
	if s := evalOutputState(outBlock(0, 0, 0, 0, 0, 0x02000000, 0), dev, tOutBase, 0); s.Valid {
		t.Errorf("bps=0: Valid=true, want false")
	}
	if s := evalOutputState(outBlock(0, 1, 0, 0, 0, 0x02000000, 0), dev, tOutBase, 0); !s.Valid {
		t.Errorf("bps=1: Valid=false, want true")
	}
}

// An out-of-range or missing device pointer means the device object cannot be
// read: Valid is false regardless of everything else.
func TestEvalOutputStateDevicePtr(t *testing.T) {
	dev := devObject(tOutBase, tVtblDS, 0, 0, 0)
	for _, tc := range []struct {
		name   string
		devPtr uint32
		dev    []byte
		valid  bool
	}{
		{"zero", 0, dev, false},
		{"below 0x10000", 0xfffc, dev, false},
		{"at 0xFFFF0000", 0xFFFF0000, dev, false},
		{"misaligned", 0x02000002, dev, false},
		{"valid but dev missing", 0x02000000, nil, false},
		{"valid but dev short", 0x02000000, dev[:tDevSize-1], false},
		{"valid", 0x02000000, dev, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := evalOutputState(outBlock(6000, 176400, 0, 0, 0, tc.devPtr, 0), tc.dev, tOutBase, 0)
			if s.Valid != tc.valid {
				t.Errorf("Valid=%v, want %v", s.Valid, tc.valid)
			}
		})
	}
}

// The effect flag comes from the block, the ratio from the caller's argument.
func TestEvalOutputStateEffect(t *testing.T) {
	dev := devObject(tOutBase, tVtblDS, 0, 0, 0)
	on := evalOutputState(outBlock(6000, 176400, 0, 0, 1, 0x02000000, 0), dev, tOutBase, 1.5)
	if !on.EffectOn || on.EffectRatio != 1.5 {
		t.Errorf("effect on: EffectOn=%v EffectRatio=%v, want true 1.5", on.EffectOn, on.EffectRatio)
	}
	off := evalOutputState(outBlock(6000, 176400, 0, 0, 0, 0x02000000, 0), dev, tOutBase, 2.0)
	if off.EffectOn {
		t.Errorf("effect off: EffectOn=true, want false")
	}
}

// A block shorter than the fields it holds is unusable.
func TestEvalOutputStateShortBlock(t *testing.T) {
	dev := devObject(tOutBase, tVtblDS, 0, 0, 0)
	if s := evalOutputState(make([]byte, tOutStateSize-1), dev, tOutBase, 0); s.Valid {
		t.Errorf("short block: Valid=true, want false")
	}
}

// readOutputState reads the block, the effect ratio, the device object and the
// ring's fed counter, in that order, and assembles an OutputState.
func TestReadOutputStateSequence(t *testing.T) {
	const devAddr, ringAddr = 0x02000000, 0x03000000
	m := &fakeLagMem{regions: map[uintptr][]byte{}, failN: -1, shortN: -1, afterN: -1}
	m.regions[flOut+tOutStateRVA] = outBlock(6000, 176400, 441, 3, 1, devAddr, ringAddr)
	ratio := make([]byte, 4)
	binary.LittleEndian.PutUint32(ratio, math.Float32bits(1.25))
	m.regions[flOut+tEffectRatioRVA] = ratio
	m.regions[devAddr] = devObject(flOut, tVtblWaveOut, 4, 11, 7)
	ring := make([]byte, 0x10)
	binary.LittleEndian.PutUint32(ring[tRingFed:], 352800)
	m.regions[ringAddr] = ring

	s := readOutputState(m.read, flOut)
	if !s.Valid || s.Device != DeviceWaveOut {
		t.Fatalf("got Valid=%v Device=%d", s.Valid, s.Device)
	}
	want := OutputState{
		Valid: true, Device: DeviceWaveOut, Base: 6000, Bps: 176400, Fed: 352800,
		Staging: 441, A87c: 3, EffectRatio: 1.25, EffectOn: true, HdrW: 11, HdrD: 7, BlkAlign: 4,
	}
	if s != want {
		t.Errorf("got %+v\nwant %+v", s, want)
	}
}

// Every read in readOutputState that can fail, and its effect: the block and
// device reads gate Valid; the ratio and ring reads are best-effort. Each read
// is driven two ways: a hard error (failN) and a short read with nil error
// (shortN, one byte short). The short read is the one that pins the length
// checks: drop `len(b) == 4` on the ratio or fed read and a 3-byte slice makes
// getU32 index out of range; drop `len(b) == lagDevSize` and a short device
// object reads past its end. A plain ReadProcessMemory can return a short slice
// with nil error (the #131 readFull guards the same way), so this path is real.
func TestReadOutputStateReadFailures(t *testing.T) {
	const devAddr, ringAddr = 0x02000000, 0x03000000
	build := func() *fakeLagMem {
		m := &fakeLagMem{regions: map[uintptr][]byte{}, failN: -1, shortN: -1, afterN: -1}
		m.regions[flOut+tOutStateRVA] = outBlock(6000, 176400, 0, 0, 0, devAddr, ringAddr)
		ratio := make([]byte, 4)
		binary.LittleEndian.PutUint32(ratio, math.Float32bits(1.5))
		m.regions[flOut+tEffectRatioRVA] = ratio
		m.regions[devAddr] = devObject(flOut, tVtblDS, 0, 0, 0)
		ring := make([]byte, 0x10)
		binary.LittleEndian.PutUint32(ring[tRingFed:], 1000)
		m.regions[ringAddr] = ring
		return m
	}
	cases := []struct {
		name            string
		n               int // read index: 0 block, 1 ratio, 2 device, 3 ring/fed
		wantValid       bool
		wantRatioZeroed bool
		wantFedZeroed   bool
	}{
		{"block", 0, false, false, false},
		{"ratio", 1, true, true, false},
		{"device", 2, false, false, false},
		{"ring", 3, false, false, false}, // fed must not be a false zero: a failed ring/fed read invalidates the state
	}
	check := func(t *testing.T, s OutputState, wantValid, wantRatioZeroed, wantFedZeroed bool) {
		if s.Valid != wantValid {
			t.Fatalf("Valid=%v, want %v", s.Valid, wantValid)
		}
		if !wantValid {
			return
		}
		if (s.EffectRatio == 0) != wantRatioZeroed {
			t.Errorf("EffectRatio=%v, ratioZeroed want %v", s.EffectRatio, wantRatioZeroed)
		}
		if (s.Fed == 0) != wantFedZeroed {
			t.Errorf("Fed=%v, fedZeroed want %v", s.Fed, wantFedZeroed)
		}
	}
	for _, tc := range cases {
		t.Run("fail/"+tc.name, func(t *testing.T) {
			m := build()
			m.failN = tc.n
			check(t, readOutputState(m.read, flOut), tc.wantValid, tc.wantRatioZeroed, tc.wantFedZeroed)
		})
		t.Run("short/"+tc.name, func(t *testing.T) {
			m := build()
			m.shortN = tc.n
			check(t, readOutputState(m.read, flOut), tc.wantValid, tc.wantRatioZeroed, tc.wantFedZeroed)
		})
	}
	t.Run("all succeed", func(t *testing.T) {
		m := build()
		s := readOutputState(m.read, flOut)
		if !s.Valid || s.EffectRatio == 0 || s.Fed == 0 {
			t.Errorf("got Valid=%v EffectRatio=%v Fed=%v, want valid with nonzero ratio and fed", s.Valid, s.EffectRatio, s.Fed)
		}
	})
}

// An out-of-range device pointer skips the device read and yields Valid=false
// without reading through the pointer.
func TestReadOutputStateBadDevicePtr(t *testing.T) {
	m := &fakeLagMem{regions: map[uintptr][]byte{}, failN: -1, shortN: -1, afterN: -1}
	m.regions[flOut+tOutStateRVA] = outBlock(6000, 176400, 0, 0, 0, 0xfffc, 0)
	m.regions[flOut+tEffectRatioRVA] = make([]byte, 4)
	if s := readOutputState(m.read, flOut); s.Valid {
		t.Errorf("Valid=true, want false")
	}
	if m.readsAt(0xfffc) {
		t.Errorf("read through out-of-range device pointer")
	}
}
