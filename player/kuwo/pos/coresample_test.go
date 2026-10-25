package pos

import (
	"encoding/binary"
	"errors"
	"testing"
)

// These bytes follow the independently observed PlayerCore layout, not the
// constants consumed by readCoreSample, so moving a field breaks the test.
func coreFixture() []byte {
	b := make([]byte, 0x2ac)
	copy(b[0x84:], "MUSIC_568213041")
	binary.LittleEndian.PutUint32(b[0x94:], 15)
	binary.LittleEndian.PutUint32(b[0x98:], 15)
	binary.LittleEndian.PutUint32(b[0xa8:], 154)
	binary.LittleEndian.PutUint32(b[0x188:], 1)
	binary.LittleEndian.PutUint32(b[0x2a0:], 123450)
	return b
}

func TestCoreSampleIdentityAndMilliseconds(t *testing.T) {
	b := coreFixture()
	read := func(uintptr, int) ([]byte, error) { return b, nil }
	s, err := readCoreSample(read, 0, "568213041")
	if err != nil || s.PositionMs != 123450 || s.DurationMs != 154000 || s.RID != "MUSIC_568213041" {
		t.Fatalf("sample=%+v err=%v", s, err)
	}
	if _, err = readCoreSample(read, 0, "658118183"); !errors.Is(err, ErrSongChanged) {
		t.Fatalf("wrong song accepted: %v", err)
	}
	// Cached duration must not survive a fresh read.
	binary.LittleEndian.PutUint32(b[0xa8:], 200)
	s, err = readCoreSample(read, 0, "")
	if err != nil || s.DurationMs != 200000 {
		t.Fatalf("duration not refreshed: %+v %v", s, err)
	}
}

func TestCoreSampleRejectsMalformedAndTransitions(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func([]byte)
		want   error
	}{
		{"negative", func(b []byte) { binary.LittleEndian.PutUint32(b[0x2a0:], 0xffffffff) }, ErrInvalidSample},
		{"past end", func(b []byte) { binary.LittleEndian.PutUint32(b[0x2a0:], 160000) }, ErrInvalidSample},
		{"zero duration", func(b []byte) { binary.LittleEndian.PutUint32(b[0xa8:], 0) }, ErrInvalidSample},
		{"unknown backend", func(b []byte) { binary.LittleEndian.PutUint32(b[0x188:], 2) }, ErrUnsupportedCore},
		{"bad RID", func(b []byte) { b[0x84] = 'X' }, ErrInvalidSample},
		{"unterminated", func(b []byte) { b[0x93] = 'X' }, ErrInvalidSample},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := coreFixture()
			tt.mutate(b)
			_, err := readCoreSample(func(uintptr, int) ([]byte, error) { return b, nil }, 0, "")
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v want %v", err, tt.want)
			}
		})
	}
	for _, change := range []string{"rid", "duration", "loop", "backend"} {
		t.Run("torn "+change, func(t *testing.T) {
			a, b := coreFixture(), coreFixture()
			switch change {
			case "rid":
				b[0x8a] = '9'
			case "duration":
				binary.LittleEndian.PutUint32(b[0xa8:], 200)
			case "loop":
				binary.LittleEndian.PutUint32(b[0x2a0:], 0)
			case "backend":
				binary.LittleEndian.PutUint32(b[0x188:], 2)
			}
			calls := 0
			_, err := readCoreSample(func(uintptr, int) ([]byte, error) {
				calls++
				if calls == 1 {
					return a, nil
				}
				return b, nil
			}, 0, "")
			if !errors.Is(err, ErrSongChanged) {
				t.Fatalf("torn sample accepted: %v", err)
			}
		})
	}
	_, err := readCoreSample(func(uintptr, int) ([]byte, error) { return []byte{1}, nil }, 0, "")
	if !errors.Is(err, ErrInvalidSample) {
		t.Fatal(err)
	}
}

func TestCoreLongRIDUsesPointerAndRechecksContents(t *testing.T) {
	b := coreFixture()
	rid := "MUSIC_1234567890123"
	data := append([]byte(rid), 0)
	binary.LittleEndian.PutUint32(b[0x84:], 0x123000)
	binary.LittleEndian.PutUint32(b[0x94:], uint32(len(rid)))
	binary.LittleEndian.PutUint32(b[0x98:], 31)
	reads := 0
	read := func(addr uintptr, n int) ([]byte, error) {
		if addr == 0x123000 {
			reads++
			return data, nil
		}
		return b, nil
	}
	s, err := readCoreSample(read, 0, rid)
	if err != nil || s.RID != rid || reads != 2 {
		t.Fatalf("%+v %v reads=%d", s, err, reads)
	}
	reads = 0
	_, err = readCoreSample(func(addr uintptr, n int) ([]byte, error) {
		if addr == 0x123000 {
			reads++
			out := append([]byte{}, data...)
			if reads == 2 {
				out[6] = '9'
			}
			return out, nil
		}
		return b, nil
	}, 0, rid)
	if !errors.Is(err, ErrSongChanged) {
		t.Fatalf("changed heap string accepted: %v", err)
	}
}
