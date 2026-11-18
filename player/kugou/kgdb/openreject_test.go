package kgdb

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// 本文件只测 Open 拒绝的库：头部不合要求（ErrUnsupportedDB），以及没有页、sqlite_master 结构异常（ErrCorrupt）。

func TestOpenRejectsUTF16AndWAL(t *testing.T) {
	for _, name := range []string{"utf16.db", "wal.db"} {
		_, err := Open(plainPager(loadFixture(t, name)))
		wantErr(t, err, ErrUnsupportedDB)
	}
}

func TestOpenRejectsHeaderFields(t *testing.T) {
	cases := []struct {
		name string
		edit func(h []byte)
	}{
		{"magic", func(h []byte) { h[0] = 's' }},
		{"magic terminator", func(h []byte) { h[15] = ' ' }},
		{"page size 512", func(h []byte) { binary.BigEndian.PutUint16(h[16:], 512) }},
		{"page size 4096", func(h []byte) { binary.BigEndian.PutUint16(h[16:], 4096) }},
		{"page size 65536", func(h []byte) { binary.BigEndian.PutUint16(h[16:], 1) }},
		{"write version 2", func(h []byte) { h[18] = 2 }},
		{"read version 2", func(h []byte) { h[19] = 2 }},
		{"reserved bytes 1", func(h []byte) { h[20] = 1 }},
		{"text encoding UTF-16be", func(h []byte) { binary.BigEndian.PutUint32(h[56:], 3) }},
		{"text encoding 0", func(h []byte) { binary.BigEndian.PutUint32(h[56:], 0) }},
	}
	base := loadFixture(t, "basic.db")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := bytes.Clone(base)
			c.edit(b)
			_, err := Open(plainPager(b))
			wantErr(t, err, ErrUnsupportedDB)
		})
	}
}

func TestOpenRejectsEmptyPager(t *testing.T) {
	_, err := Open(plainPager(nil))
	wantErr(t, err, ErrCorrupt)
}

func TestOpenRejectsCorruptSchema(t *testing.T) {
	b := loadFixture(t, "basic.db")
	b[100] = 0x0A // sqlite_master 的根（第 1 页）成了索引叶页
	_, err := Open(plainPager(b))
	wantErr(t, err, ErrCorrupt)
}
