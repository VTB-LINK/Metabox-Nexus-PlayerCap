package kgdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb/kgdbtest"
)

// 本文件只测 NewPager 判为不支持格式（ErrUnsupportedDB）的情形：第 1 页完整性校验、头部校验、
// 文件大小、未加密的明文库。

func TestPagerPage1IntegrityFails(t *testing.T) {
	enc := kgdbtest.Encrypt(plainDB(2))
	// [16:24] 是明文头；[8:16] 与 [24:32] 是第一个密文分组，改了解出的头与明文头不符。
	for i := 8; i < 32; i++ {
		bad := bytes.Clone(enc)
		bad[i] ^= 0x01
		_, err := NewPager(bytes.NewReader(bad), int64(len(bad)))
		if !errors.Is(err, ErrUnsupportedDB) {
			t.Errorf("byte %d flipped: err = %v, want ErrUnsupportedDB", i, err)
		}
	}
}

// 加密自洽、但明文头本身不合 pc_kugou_db 的判据：完整性校验能过，头部校验须拦下。
func TestPagerRejectsBadPage1Header(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(b []byte)
		valid bool
	}{
		{"page size 512", func(b []byte) { binary.BigEndian.PutUint16(b[16:], 0x0200) }, true},
		{"page size 32768", func(b []byte) { binary.BigEndian.PutUint16(b[16:], 0x8000) }, true},
		{"page size 256", func(b []byte) { binary.BigEndian.PutUint16(b[16:], 0x0100) }, false},
		{"page size 768", func(b []byte) { binary.BigEndian.PutUint16(b[16:], 0x0300) }, false},
		{"page size 0", func(b []byte) { binary.BigEndian.PutUint16(b[16:], 0) }, false},
		{"page size field 1 (65536)", func(b []byte) { binary.BigEndian.PutUint16(b[16:], 1) }, false},
		{"reserved 1", func(b []byte) { b[20] = 1 }, false},
		{"max payload 65", func(b []byte) { b[21] = 65 }, false},
		{"min payload 31", func(b []byte) { b[22] = 31 }, false},
		{"leaf payload 33", func(b []byte) { b[23] = 33 }, false},
	}
	for _, c := range cases {
		plain := plainDB(2)
		c.edit(plain)
		enc := kgdbtest.Encrypt(plain)
		_, err := NewPager(bytes.NewReader(enc), int64(len(enc)))
		if c.valid && err != nil {
			t.Errorf("%s: err = %v, want nil", c.name, err)
		}
		if !c.valid && !errors.Is(err, ErrUnsupportedDB) {
			t.Errorf("%s: err = %v, want ErrUnsupportedDB", c.name, err)
		}
	}
}

func TestPagerRejectsBadSize(t *testing.T) {
	enc := kgdbtest.Encrypt(plainDB(2))
	// 最后一例的页数超出 uint32，截断后是 1 页，不拦就会把它当单页库读下去。
	for _, size := range []int64{0, 1023, 1025, -1024, (math.MaxUint32 + 2) * PageSize} {
		reads := 0
		r := readerFunc(func(p []byte, off int64) (int, error) {
			reads++
			return bytes.NewReader(enc).ReadAt(p, off)
		})
		_, err := NewPager(r, size)
		if !errors.Is(err, ErrUnsupportedDB) {
			t.Errorf("size %d: err = %v, want ErrUnsupportedDB", size, err)
		}
		if reads != 0 {
			t.Errorf("size %d: %d reads, want 0", size, reads)
		}
	}
}

func TestPagerRejectsPlainSQLite(t *testing.T) {
	plain := plainDB(2)
	_, err := NewPager(bytes.NewReader(plain), int64(len(plain)))
	if !errors.Is(err, ErrUnsupportedDB) {
		t.Fatalf("err = %v, want ErrUnsupportedDB", err)
	}
}
