package kgdb

// 本文件只放读取器测试的共用夹具：读入 testdata 下的明文库、按页切明文的取页器、记录取页的取页器、
// 在明文库上定位页与单元格的辅助函数，以及带超时的整表扫描。本身不含测试。
// 定位辅助只用于构造反例，假定输入是 gen_fixtures.py 生成的结构完好的库。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// plainPager 直接按页切明文字节，不加密；每次 Page 返回新副本。
type plainPager []byte

func (p plainPager) PageCount() uint32 { return uint32(len(p) / PageSize) }

func (p plainPager) Page(n uint32) ([]byte, error) {
	if n == 0 || n > p.PageCount() {
		return nil, fmt.Errorf("plainPager: page %d out of range", n)
	}
	return bytes.Clone(page(p, n)), nil
}

// recordingPager 按顺序记下每次取页的页号。
type recordingPager struct {
	Pager
	pages []uint32
}

func (r *recordingPager) Page(n uint32) ([]byte, error) {
	r.pages = append(r.pages, n)
	return r.Pager.Page(n)
}

func loadFixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func mustOpen(t *testing.T, p Pager) *DB {
	t.Helper()
	db, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

// rootOf 返回表在 sqlite_master 中登记的根页号。
func rootOf(t *testing.T, b []byte, name string) uint32 {
	t.Helper()
	for _, tb := range mustOpen(t, plainPager(b)).tables {
		if tb.name == name {
			return uint32(tb.root)
		}
	}
	t.Fatalf("table %s not in sqlite_master", name)
	return 0
}

func hdrOffset(n uint32) int {
	if n == 1 {
		return 100
	}
	return 0
}

func isInterior(b []byte, n uint32) bool { return page(b, n)[hdrOffset(n)] == 0x05 }

// ptrArray 返回第 n 页单元格指针数组的起点与单元格数。
func ptrArray(b []byte, n uint32) (start, cells int) {
	pg, h := page(b, n), hdrOffset(n)
	start = h + 8
	if isInterior(b, n) {
		start = h + 12
	}
	return start, int(binary.BigEndian.Uint16(pg[h+3:]))
}

// cellOffsets 返回第 n 页各单元格的页内偏移。
func cellOffsets(b []byte, n uint32) []int {
	start, cells := ptrArray(b, n)
	offs := make([]int, cells)
	for i := range offs {
		offs[i] = int(binary.BigEndian.Uint16(page(b, n)[start+2*i:]))
	}
	return offs
}

func setCellPtr(b []byte, n uint32, i int, off uint16) {
	start, _ := ptrArray(b, n)
	binary.BigEndian.PutUint16(page(b, n)[start+2*i:], off)
}

// children 返回内部页 n 的子页号：各单元格的左子页，最后是最右指针。
func children(b []byte, n uint32) []uint32 {
	var out []uint32
	for _, off := range cellOffsets(b, n) {
		out = append(out, binary.BigEndian.Uint32(page(b, n)[off:]))
	}
	return append(out, binary.BigEndian.Uint32(page(b, n)[hdrOffset(n)+8:]))
}

// setChild 改内部页 n 的第 i 个子指针；i 等于单元格数时改最右指针。
func setChild(b []byte, n uint32, i int, child uint32) {
	offs := cellOffsets(b, n)
	at := hdrOffset(n) + 8
	if i < len(offs) {
		at = offs[i]
	}
	binary.BigEndian.PutUint32(page(b, n)[at:], child)
}

// leafPages 按遍历顺序返回以 root 为根的表 b-tree 的叶页号。
func leafPages(b []byte, root uint32) []uint32 {
	if !isInterior(b, root) {
		return []uint32{root}
	}
	var out []uint32
	for _, c := range children(b, root) {
		out = append(out, leafPages(b, c)...)
	}
	return out
}

// leafCell 返回叶页 n 第 i 个单元格从起点到页尾的字节，以及其中记录载荷的起点。
func leafCell(b []byte, n uint32, i int) (cell []byte, payloadAt int) {
	cell = page(b, n)[cellOffsets(b, n)[i]:]
	_, n1 := readVarint(cell)
	_, n2 := readVarint(cell[n1:])
	return cell, n1 + n2
}

// overflowHead 返回 table 中第一条溢出记录的首个溢出页号。
func overflowHead(t *testing.T, b []byte, table string) uint32 {
	t.Helper()
	for _, lp := range leafPages(b, rootOf(t, b, table)) {
		for i := range cellOffsets(b, lp) {
			cell, at := leafCell(b, lp, i)
			size, _ := readVarint(cell)
			if local := localPayload(size); local < size {
				return binary.BigEndian.Uint32(cell[at+int(local):])
			}
		}
	}
	t.Fatalf("no overflow record in %s", table)
	return 0
}

// scanAll 以表的全部列扫描 table，返回 Columns 或 Scan 的错误与读到的行数；10 秒内不返回即判挂起。
// 扫描在另一个 goroutine 里进行，panic 会使整个测试进程失败。
func scanAll(t *testing.T, db *DB, table string) (rows int, err error) {
	t.Helper()
	type result struct {
		rows int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		cols, err := db.Columns(table)
		if err == nil {
			err = db.Scan(table, cols, func([]Value) bool { r.rows++; return true })
		}
		r.err = err
		done <- r
	}()
	select {
	case r := <-done:
		return r.rows, r.err
	case <-time.After(10 * time.Second):
		t.Fatalf("scan of %s did not return within 10s", table)
		return 0, nil
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}
