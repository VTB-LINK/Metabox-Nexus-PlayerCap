package kgdb

// 本文件只放取页层测试的共用夹具：手工构造的明文库、按页切片、读取桩与建 Pager 的辅助函数。本身不含测试。

import (
	"bytes"
	"encoding/binary"
	"testing"
)

var sqliteMagic16 = []byte("SQLite format 3\x00")

// plainDB 构造 pages 页明文库。第 1 页按 SQLite 头手工构造：魔数、页大小 0x0400、读写版本 1/1、
// 保留 0、负载比例 64/32/32、偏移 56 文本编码 1、偏移 100 起一个空叶页；其余页填逐页不同的字节。
func plainDB(pages int) []byte {
	b := make([]byte, pages*PageSize)
	copy(b, sqliteMagic16)
	binary.BigEndian.PutUint16(b[16:], PageSize)
	b[18], b[19] = 1, 1
	b[20], b[21], b[22], b[23] = 0, 64, 32, 32
	binary.BigEndian.PutUint32(b[56:], 1)
	b[100] = 0x0D
	binary.BigEndian.PutUint16(b[105:], PageSize) // 空叶页的单元格内容区从页尾开始
	for i := PageSize; i < len(b); i++ {
		b[i] = byte(i % 251)
	}
	return b
}

func page(b []byte, n uint32) []byte { return b[(n-1)*PageSize : n*PageSize] }

// readerFunc 把函数当 io.ReaderAt 用，作各种读取桩。
type readerFunc func(p []byte, off int64) (int, error)

func (f readerFunc) ReadAt(p []byte, off int64) (int, error) { return f(p, off) }

func mustPager(t *testing.T, enc []byte) Pager {
	t.Helper()
	p, err := NewPager(bytes.NewReader(enc), int64(len(enc)))
	if err != nil {
		t.Fatalf("NewPager: %v", err)
	}
	return p
}
