package kgdb

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// 本文件只测 Open 对 sqlite_master 保留量的上限：表数至多 maxTables，表名与建表文本合计至多 maxSchemaBytes；
// 超出判 ErrUnsupportedDB，恰在上限照常打开，不保留的条目（索引）不计。

// putVarint 编码 SQLite varint（只用于 v < 2^56，即 1～8 字节）。
func putVarint(v uint64) []byte {
	b := []byte{byte(v & 0x7F)}
	for v >>= 7; v > 0; v >>= 7 {
		b = append([]byte{byte(v&0x7F) | 0x80}, b...)
	}
	return b
}

// masterRecord 编码 sqlite_master 的一行 (typ, name, NULL, 2, sql)。
func masterRecord(typ, name, sql string) []byte {
	types := [][]byte{
		putVarint(13 + 2*uint64(len(typ))),
		putVarint(13 + 2*uint64(len(name))),
		{0}, // tbl_name 为 NULL
		{1}, // rootpage 为 1 字节整数
		putVarint(13 + 2*uint64(len(sql))),
	}
	hdr := bytes.Join(types, nil)
	rec := append([]byte{byte(1 + len(hdr))}, hdr...) // 记录头都小于 128 字节
	rec = append(rec, typ...)
	rec = append(rec, name...)
	rec = append(rec, 2)
	return append(rec, sql...)
}

// schemaDB 合成只含 sqlite_master 的明文库：第 1 页是内部页（至多 131 个子页），其下每 perLeaf 条
// (typ, names[i], sqls[i]) 占一个叶页；记录超出本页容量的部分写进它自己的溢出页链。
func schemaDB(t *testing.T, typ string, names, sqls []string, perLeaf int) []byte {
	t.Helper()
	var pages [][]byte
	newPage := func() (uint32, []byte) {
		pg := make([]byte, PageSize)
		pages = append(pages, pg)
		return uint32(len(pages)), pg
	}
	_, p1 := newPage()
	copy(p1, plainDB(1)[:100])

	var leaves []uint32
	for i := 0; i < len(names); i += perLeaf {
		n, pg := newPage()
		leaves = append(leaves, n)
		pg[0] = 0x0D
		end, cells := PageSize, 0
		for j := i; j < min(i+perLeaf, len(names)); j++ {
			rec := masterRecord(typ, names[j], sqls[j])
			local := int(localPayload(uint64(len(rec))))
			cell := append(putVarint(uint64(len(rec))), 0x01) // rowid 不参与读取，都写 1
			cell = append(cell, rec[:local]...)
			if local < len(rec) {
				cell = binary.BigEndian.AppendUint32(cell, uint32(len(pages)+1))
				for rest := rec[local:]; len(rest) > 0; {
					on, op := newPage()
					rest = rest[copy(op[4:], rest):]
					if len(rest) > 0 {
						binary.BigEndian.PutUint32(op, on+1)
					}
				}
			}
			if end -= len(cell); end < 8+2*(cells+1) {
				t.Fatalf("leaf %d overflows: %d records", n, cells+1)
			}
			copy(pg[end:], cell)
			binary.BigEndian.PutUint16(pg[8+2*cells:], uint16(end))
			cells++
		}
		binary.BigEndian.PutUint16(pg[3:], uint16(cells))
		binary.BigEndian.PutUint16(pg[5:], uint16(end))
	}

	// 第 1 页：前面的叶页作单元格的左子页（4 字节页号 + 1 字节 rowid），最后一个作最右指针。
	if len(leaves) > 131 {
		t.Fatalf("%d leaves do not fit under page 1", len(leaves))
	}
	p1[100] = 0x05
	end := PageSize
	for i, leaf := range leaves[:len(leaves)-1] {
		end -= 5
		binary.BigEndian.PutUint32(p1[end:], leaf)
		p1[end+4] = 0x01
		binary.BigEndian.PutUint16(p1[112+2*i:], uint16(end))
	}
	binary.BigEndian.PutUint16(p1[103:], uint16(len(leaves)-1))
	binary.BigEndian.PutUint16(p1[105:], uint16(end))
	binary.BigEndian.PutUint32(p1[108:], leaves[len(leaves)-1])
	return bytes.Join(pages, nil)
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestOpenTableCountLimit(t *testing.T) {
	db, err := Open(plainPager(schemaDB(t, "table", repeat("t", maxTables), repeat("", maxTables), 50)))
	if err != nil {
		t.Fatalf("%d tables: %v", maxTables, err)
	}
	if got := len(db.Tables()); got != maxTables {
		t.Fatalf("%d tables: Tables() = %d entries", maxTables, got)
	}
	_, err = Open(plainPager(schemaDB(t, "table", repeat("t", maxTables+1), repeat("", maxTables+1), 50)))
	wantErr(t, err, ErrUnsupportedDB)

	// 索引不保留，不计入表数。
	db, err = Open(plainPager(schemaDB(t, "index", repeat("i", maxTables+1), repeat("", maxTables+1), 50)))
	if err != nil {
		t.Fatalf("%d indexes: %v", maxTables+1, err)
	}
	if got := db.Tables(); len(got) != 0 {
		t.Fatalf("%d indexes: Tables() = %d entries", maxTables+1, len(got))
	}
}

// 128 张表，每张表名 1 字节、建表文本 32767 字节：合计恰为 4 MiB。每条记录约 32 KB，走溢出页链。
func TestOpenSchemaTextLimit(t *testing.T) {
	const n, sqlLen = 128, maxSchemaBytes/128 - 1
	sqls := repeat(strings.Repeat("x", sqlLen), n)
	db, err := Open(plainPager(schemaDB(t, "table", repeat("t", n), sqls, 1)))
	if err != nil {
		t.Fatalf("schema of exactly %d bytes: %v", maxSchemaBytes, err)
	}
	if got := len(db.Tables()); got != n {
		t.Fatalf("schema of exactly %d bytes: Tables() = %d entries", maxSchemaBytes, got)
	}

	sqls[n-1] += "x" // 建表文本多 1 字节
	_, err = Open(plainPager(schemaDB(t, "table", repeat("t", n), sqls, 1)))
	wantErr(t, err, ErrUnsupportedDB)

	sqls[n-1] = sqls[0]
	names := repeat("t", n)
	names[0] = "tt" // 表名多 1 字节，同样计入
	_, err = Open(plainPager(schemaDB(t, "table", names, sqls, 1)))
	wantErr(t, err, ErrUnsupportedDB)
}
