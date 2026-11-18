package kgdb

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
)

// 本文件只测结构异常的库：对 basic.db 的副本改字节后，Scan 返回 ErrCorrupt，不 panic、不挂起。

func wantCorruptScan(t *testing.T, b []byte, table string) {
	t.Helper()
	_, err := scanAll(t, mustOpen(t, plainPager(b)), table)
	wantErr(t, err, ErrCorrupt)
}

func localFileRoot(t *testing.T, b []byte) uint32 {
	t.Helper()
	root := rootOf(t, b, "LocalFile")
	if !isInterior(b, root) {
		t.Fatalf("fixture: LocalFile root %d is not an interior page", root)
	}
	return root
}

func TestCorruptInteriorCycle(t *testing.T) {
	for _, which := range []string{"first cell", "right pointer"} {
		t.Run(which, func(t *testing.T) {
			b := loadFixture(t, "basic.db")
			root := localFileRoot(t, b)
			i := 0
			if which == "right pointer" {
				i = len(cellOffsets(b, root))
			}
			setChild(b, root, i, root)
			wantCorruptScan(t, b, "LocalFile")
		})
	}
}

func TestCorruptOverflowCycle(t *testing.T) {
	b := loadFixture(t, "basic.db")
	head := overflowHead(t, b, "LyricInfo")
	binary.BigEndian.PutUint32(page(b, head), head)
	wantCorruptScan(t, b, "LyricInfo")
}

// 溢出链在载荷读满之前就结束（下一页为 0）。
func TestCorruptOverflowChainTooShort(t *testing.T) {
	b := loadFixture(t, "basic.db")
	binary.BigEndian.PutUint32(page(b, overflowHead(t, b, "LyricInfo")), 0)
	wantCorruptScan(t, b, "LyricInfo")
}

func TestCorruptCellPointerOutOfPage(t *testing.T) {
	for _, c := range []struct {
		name string
		off  uint16
	}{
		{"page end", PageSize},
		{"far past page end", 0xFFFF},
		{"inside page header", 0},
		{"inside cell pointer array", 9},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := loadFixture(t, "basic.db")
			setCellPtr(b, leafPages(b, localFileRoot(t, b))[0], 0, c.off)
			wantCorruptScan(t, b, "LocalFile")
		})
	}
	// 指针指进指针数组自身，且那里的字节恰好拼成一条合法记录：只有起点检查能拦下。
	t.Run("well-formed cell inside pointer array", func(t *testing.T) {
		b := loadFixture(t, "basic.db")
		pg := page(b, rootOf(t, b, "Quoted Table"))
		binary.BigEndian.PutUint16(pg[3:], 1)                        // 1 个单元格，指针数组占 [8,10)
		binary.BigEndian.PutUint16(pg[8:], 9)                        // 起点 9：载荷长度即指针低字节 0x09
		copy(pg[10:], []byte{0x01, 0x02, 0x00, 0, 0, 0, 0, 0, 0, 0}) // rowid 1；记录头 2 字节、一列 NULL；补足 9 字节
		wantCorruptScan(t, b, "Quoted Table")
	})
	t.Run("interior cell", func(t *testing.T) {
		b := loadFixture(t, "basic.db")
		setCellPtr(b, localFileRoot(t, b), 0, PageSize-2) // 子页号要 4 字节，页内只剩 2 字节
		wantCorruptScan(t, b, "LocalFile")
	})
}

func TestCorruptPageNumberOutOfRange(t *testing.T) {
	count := plainPager(loadFixture(t, "basic.db")).PageCount()
	for _, child := range []uint32{0, count + 1, 0xFFFFFFFF} {
		b := loadFixture(t, "basic.db")
		setChild(b, localFileRoot(t, b), 0, child)
		wantCorruptScan(t, b, "LocalFile")
	}
	b := loadFixture(t, "basic.db")
	binary.BigEndian.PutUint32(page(b, overflowHead(t, b, "LyricInfo")), count+1)
	wantCorruptScan(t, b, "LyricInfo")
}

func TestCorruptBadPageType(t *testing.T) {
	for _, c := range []struct {
		name string
		leaf bool
		typ  byte
	}{
		{"root is index leaf", false, 0x0A},
		{"root is zero", false, 0x00},
		{"leaf is index interior", true, 0x02},
		{"leaf is freelist-like garbage", true, 0xFF},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := loadFixture(t, "basic.db")
			n := localFileRoot(t, b)
			if c.leaf {
				n = leafPages(b, n)[0]
			}
			page(b, n)[0] = c.typ
			wantCorruptScan(t, b, "LocalFile")
		})
	}
}

func TestCorruptCellCountOverflowsPage(t *testing.T) {
	b := loadFixture(t, "basic.db")
	leaf := leafPages(b, localFileRoot(t, b))[0]
	binary.BigEndian.PutUint16(page(b, leaf)[3:], (PageSize-8)/2+1)
	wantCorruptScan(t, b, "LocalFile")
}

// 记录头长度超出载荷、或小于它自身的 varint：用 "Quoted Table" 第一行（载荷远小于 0x7F）。
func TestCorruptRecordHeaderTooLong(t *testing.T) {
	for _, hdr := range []byte{0x7F, 0x00} {
		b := loadFixture(t, "basic.db")
		cell, at := leafCell(b, rootOf(t, b, "Quoted Table"), 0)
		if size, _ := readVarint(cell); size >= 0x7F {
			t.Fatalf("fixture: first Quoted Table record is %d bytes", size)
		}
		cell[at] = hdr
		wantCorruptScan(t, b, "Quoted Table")
	}
}

func TestCorruptTruncatedVarint(t *testing.T) {
	t.Run("cell header at page end", func(t *testing.T) {
		b := loadFixture(t, "basic.db")
		leaf := leafPages(b, localFileRoot(t, b))[0]
		setCellPtr(b, leaf, 0, PageSize-2)
		page(b, leaf)[PageSize-2], page(b, leaf)[PageSize-1] = 0xFF, 0xFF
		wantCorruptScan(t, b, "LocalFile")
	})
	t.Run("serial type past record header", func(t *testing.T) {
		b := loadFixture(t, "basic.db")
		cell, at := leafCell(b, rootOf(t, b, "Quoted Table"), 0)
		hdr, _ := readVarint(cell[at:])
		cell[at+int(hdr)-1] = 0x81 // 最后一个序列类型的 varint 越过记录头
		wantCorruptScan(t, b, "Quoted Table")
	})
}

// 记录载荷里的序列类型 10、11 是保留值。
func TestCorruptReservedSerialType(t *testing.T) {
	b := loadFixture(t, "basic.db")
	cell, at := leafCell(b, rootOf(t, b, "Quoted Table"), 0)
	cell[at+1] = 10
	wantCorruptScan(t, b, "Quoted Table")
}

// 记录头声明的值长度超出载荷。
func TestCorruptValueBeyondPayload(t *testing.T) {
	b := loadFixture(t, "basic.db")
	cell, at := leafCell(b, rootOf(t, b, "Quoted Table"), 0)
	cell[at+1] = 13 + 2*0x30 // 第 1 列声明为 48 字节的文本，载荷不够
	wantCorruptScan(t, b, "Quoted Table")
}

// 构造从 LocalFile 的根起 k 层的内部页链（每页 0 个单元格，最右指针指向下一页），链尾指向一个叶页。
// 根到叶共 k+1 页：不超过 32 页照常读出该叶页的行，超过即判结构异常。
func TestCorruptDepthLimit(t *testing.T) {
	for _, c := range []struct {
		interior int
		ok       bool
	}{{31, true}, {32, false}, {33, false}} {
		b := loadFixture(t, "basic.db")
		root := localFileRoot(t, b)
		leaves := leafPages(b, root)
		chain := append([]uint32{root}, leaves[1:c.interior]...)
		for i, n := range chain {
			next := leaves[0]
			if i+1 < len(chain) {
				next = chain[i+1]
			}
			pg := page(b, n)
			clear(pg[:12])
			pg[0] = 0x05
			binary.BigEndian.PutUint16(pg[5:], PageSize)
			binary.BigEndian.PutUint32(pg[8:], next)
		}
		rows, err := scanAll(t, mustOpen(t, plainPager(b)), "LocalFile")
		if c.ok {
			if want := len(cellOffsets(b, leaves[0])); err != nil || rows != want {
				t.Errorf("%d interior pages: %d rows, err %v; want %d rows", c.interior, rows, err, want)
			}
			continue
		}
		wantErr(t, err, ErrCorrupt)
	}
}

// 把 LocalFile 第一个叶页第一条记录的载荷长度改为 4 MiB+1（4 字节 varint，覆盖其后的 rowid 与记录头），
// 并在按新长度算出的溢出页号位置写入一个合法页号：判结构异常，且不去读那一页。
func TestCorruptRecordTooLarge(t *testing.T) {
	b := loadFixture(t, "basic.db")
	leaves := leafPages(b, localFileRoot(t, b))
	cell := page(b, leaves[0])[cellOffsets(b, leaves[0])[0]:]
	size := []byte{0x82, 0x80, 0x80, 0x01}
	if v, n := readVarint(size); v != 4<<20+1 || n != 4 {
		t.Fatalf("varint %X = %d (%d bytes)", size, v, n)
	}
	copy(cell, size)
	_, n2 := readVarint(cell[4:])
	trap := leaves[len(leaves)-1]
	binary.BigEndian.PutUint32(cell[4+n2+int(localPayload(4<<20+1)):], trap)

	rp := &recordingPager{Pager: plainPager(b)}
	_, err := scanAll(t, mustOpen(t, rp), "LocalFile")
	wantErr(t, err, ErrCorrupt)
	if slices.Contains(rp.pages, trap) {
		t.Fatalf("read page %d as overflow of an oversized record", trap)
	}
}

// 未改字节的副本照常读完，确认上面各例的错误来自所改的字节。
func TestCorruptBaselineIntact(t *testing.T) {
	b := loadFixture(t, "basic.db")
	db := mustOpen(t, plainPager(bytes.Clone(b)))
	for _, table := range []string{"LocalFile", "LyricInfo", "Quoted Table"} {
		if _, err := scanAll(t, db, table); err != nil {
			t.Errorf("%s: %v", table, err)
		}
	}
}
