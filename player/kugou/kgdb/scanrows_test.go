package kgdb

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// 本文件只测 Scan 对结构完好的库读出的行：按 rowid 的顺序、列的对应、INTEGER PRIMARY KEY 取 rowid（DESC 除外）、
// 短记录补 Null、各序列类型、溢出记录（含本页存放长度的分界）、提前停止，以及读出的值不与读取器的缓冲区共享。

func TestScanLocalFileAllRows(t *testing.T) {
	db := mustOpen(t, plainPager(loadFixture(t, "basic.db")))
	n := 0
	err := db.Scan("LocalFile", []string{"Extra", "SongID", "TitleName", "Pad"}, func(row []Value) bool {
		n++
		if row[1].Kind != Integer || row[1].Int != int64(n) {
			t.Fatalf("row %d: SongID = %+v, want rowid %d", n, row[1], n)
		}
		if got, want := row[2].String(), fmt.Sprintf("Title %03d", n); row[2].Kind != Text || got != want {
			t.Fatalf("row %d: TitleName = %+v, want %q", n, row[2], want)
		}
		if got, want := row[3].String(), strings.Repeat(fmt.Sprintf("%03d|", n), 75); got != want {
			t.Fatalf("row %d: Pad = %q, want %q", n, got, want)
		}
		// 前 600 行写于 ALTER TABLE ADD COLUMN 之前，记录里没有 Extra。
		if n <= 600 && row[0].Kind != Null || n > 600 && (row[0].Kind != Text || row[0].String() != "x") {
			t.Fatalf("row %d: Extra = %+v", n, row[0])
		}
		return true
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if n != 900 {
		t.Fatalf("got %d rows, want 900", n)
	}
}

func pattern(n int, sep string) string {
	var b strings.Builder
	for k := range n {
		fmt.Fprintf(&b, "%04d%s", k, sep)
	}
	return b.String()
}

// 两条溢出记录：第 7 行（ExternalInfo 3000 字符）本页存 M 字节，第 8 行（SingerNames 1800 字符）本页存 K 字节，
// 分支由 gen_fixtures.py 断言。
func TestScanOverflowRecord(t *testing.T) {
	db := mustOpen(t, plainPager(loadFixture(t, "basic.db")))
	seen := 0
	err := db.Scan("LyricInfo", []string{"SongName", "ExternalInfo", "AISongID", "VocabCheckTime", "SingerNames"}, func(row []Value) bool {
		seen++
		name := row[0].String()
		wantExt := Value{}
		if name == "Song 07" {
			wantExt = Value{Kind: Text, Bytes: []byte(pattern(600, ","))}
		}
		if row[1].Kind != wantExt.Kind || row[1].String() != wantExt.String() {
			t.Errorf("%s: ExternalInfo = %q (%d bytes, kind %d)", name, row[1].String(), len(row[1].Bytes), row[1].Kind)
		}
		wantSinger := "Singer" + strings.TrimPrefix(name, "Song")
		if name == "Song 08" {
			wantSinger = pattern(360, ";")
		}
		if row[4].Kind != Text || row[4].String() != wantSinger {
			t.Errorf("%s: SingerNames = %q (%d bytes)", name, row[4].String(), len(row[4].Bytes))
		}
		// 溢出内容之后的列须仍对得上。
		if row[2].Kind != Text || len(row[2].Bytes) != 0 {
			t.Errorf("%s: AISongID = %+v, want empty Text", name, row[2])
		}
		if want := fmt.Sprintf("Song %02d", row[3].Int); row[3].Kind != Integer || name != want {
			t.Errorf("%s: VocabCheckTime = %+v", name, row[3])
		}
		return true
	})
	if err != nil || seen != 20 {
		t.Fatalf("Scan: %d rows, err %v; want 20 rows", seen, err)
	}
}

// localPayload 的分界（U = 1024：X = 989，M = 103）：P ≤ X 全在本页；否则 K = M+((P-M)%1020)，K ≤ X 取 K，不然取 M。
func TestLocalPayload(t *testing.T) {
	for _, c := range []struct{ p, want uint64 }{
		{0, 0}, {1, 1}, {989, 989},
		{990, 103},  // K = 990
		{1123, 103}, // K = 103
		{2000, 980}, // K = 980
		{2009, 989}, // K = 989 = X
		{2010, 103}, // K = 990
		{4<<20 + 1, 103},
	} {
		if got := localPayload(c.p); got != c.want {
			t.Errorf("localPayload(%d) = %d, want %d", c.p, got, c.want)
		}
	}
}

func TestScanSerialTypes(t *testing.T) {
	ints := map[string]int64{
		"int 0": 0, "int 1": 1, "int -1": -1, "int 127": 127, "int 32767": 32767, "int 8388607": 8388607,
		"int 2147483647": 2147483647, "int 140737488355327": 140737488355327,
		"int 4611686018427387904": 4611686018427387904,
	}
	db := mustOpen(t, plainPager(loadFixture(t, "basic.db")))
	seen := map[string]bool{}
	err := db.Scan("Quoted Table", []string{"col a", "col b", "col c", "d"}, func(row []Value) bool {
		label := row[0].String()
		seen[label] = true
		switch want, isInt := ints[label]; {
		case isInt:
			if row[1].Kind != Integer || row[1].Int != want || row[2].Kind != Null || row[3].Kind != Null {
				t.Errorf("%s: row = %+v, want Integer %d", label, row, want)
			}
		case label == "float":
			if row[2].Kind != Float || row[2].Float != 1.5 || row[1].Kind != Null {
				t.Errorf("float: row = %+v, want Float 1.5", row)
			}
		case label == "blob":
			if row[3].Kind != Blob || !bytes.Equal(row[3].Bytes, []byte{0x00, 0xFF, 0x7F, 0x80}) {
				t.Errorf("blob: row = %+v, want Blob 00 FF 7F 80", row)
			}
		case label == "null":
			if row[1].Kind != Null || row[2].Kind != Null || row[3].Kind != Null {
				t.Errorf("null: row = %+v, want all Null", row)
			}
		default:
			t.Errorf("unexpected row %+v", row)
		}
		return true
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(seen) != len(ints)+3 {
		t.Fatalf("saw %d distinct rows, want %d", len(seen), len(ints)+3)
	}
}

// INTEGER PRIMARY KEY DESC 不是 rowid 别名：d 的 id 取记录里存的值（与 rowid 不同，也不按 rowid 的顺序）；
// INTEGER PRIMARY KEY ASC 是别名：a 的 id 取 rowid（记录里存 NULL）。
func TestScanIntegerPrimaryKeyDescAsc(t *testing.T) {
	db := mustOpen(t, plainPager(loadFixture(t, "basic.db")))
	for table, want := range map[string][]string{"d": {"100 x", "50 y"}, "a": {"7 p", "9 q"}} {
		var got []string
		err := db.Scan(table, []string{"id", "v"}, func(row []Value) bool {
			if row[0].Kind != Integer {
				t.Errorf("%s: id = %+v, want Integer", table, row[0])
			}
			got = append(got, row[0].String()+" "+row[1].String())
			return true
		})
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("Scan(%s) = %q, err %v; want %q", table, got, err, want)
		}
	}
}

func TestScanStopsEarly(t *testing.T) {
	db := mustOpen(t, plainPager(loadFixture(t, "basic.db")))
	calls := 0
	err := db.Scan("LocalFile", []string{"SongID"}, func([]Value) bool {
		calls++
		return calls < 3
	})
	if err != nil || calls != 3 {
		t.Fatalf("Scan: %d calls, err %v; want 3 calls, no error", calls, err)
	}
}

// 列可重复、可乱序、大小写不同；空列表照样逐行回调。
func TestScanColumnSelection(t *testing.T) {
	db := mustOpen(t, plainPager(loadFixture(t, "basic.db")))
	first := true
	err := db.Scan("localfile", []string{"titlename", "SONGID", "TitleName"}, func(row []Value) bool {
		if len(row) != 3 || row[0].String() != "Title 001" || row[1].Int64() != 1 || row[2].String() != "Title 001" {
			t.Errorf("row = %+v", row)
		}
		first = false
		return false
	})
	if err != nil || first {
		t.Fatalf("Scan: err %v, called %v", err, !first)
	}
	n := 0
	if err := db.Scan("LocalFile", nil, func(row []Value) bool { n++; return len(row) == 0 }); err != nil || n != 900 {
		t.Fatalf("Scan with no columns: %d rows, err %v; want 900", n, err)
	}
}

// 回调改写读出的字节：同一列取两次的另一份不变（不共用页缓冲区或载荷），之后的行照常读出。
func TestScanValuesAreIndependentCopies(t *testing.T) {
	db := mustOpen(t, plainPager(loadFixture(t, "basic.db")))
	for _, table := range []string{"LocalFile", "LyricInfo"} {
		col := map[string]string{"LocalFile": "Pad", "LyricInfo": "ExternalInfo"}[table]
		var kept []string
		err := db.Scan(table, []string{col, col}, func(row []Value) bool {
			want := row[1].String()
			for i := range row[0].Bytes {
				row[0].Bytes[i] = 'X'
			}
			if row[1].String() != want {
				t.Errorf("%s: writing one copy of %s changed the other", table, col)
			}
			kept = append(kept, want)
			return true
		})
		if err != nil {
			t.Fatalf("Scan(%s): %v", table, err)
		}
		if table == "LocalFile" && kept[899] != strings.Repeat("900|", 75) {
			t.Errorf("last Pad = %q after earlier rows were modified", kept[899])
		}
	}
}
