package kgdb

import (
	"errors"
	"slices"
	"testing"
)

// 本文件只测按名字查找失败与不支持的表：没有的表（ErrNoTable）、没有的列（ErrNoColumn）、
// WITHOUT ROWID 表与虚拟表（ErrUnsupportedTable）、sqlite_master 里的根页号不合法（ErrCorrupt）。出错时一律不回调。

// wantUnsupportedTable 断言 err 是 ErrUnsupportedTable，且不是整库不支持或结构异常：调用方据此只跳过这张表。
func wantUnsupportedTable(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrUnsupportedTable) || errors.Is(err, ErrUnsupportedDB) || errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrUnsupportedTable only", err)
	}
}

func TestScanErrors(t *testing.T) {
	db := mustOpen(t, plainPager(loadFixture(t, "basic.db")))
	cases := []struct {
		table string
		cols  []string
		want  error
	}{
		{"NoSuchTable", []string{"a"}, ErrNoTable},
		{"ix", []string{"MusicFileHash"}, ErrNoTable}, // 索引不是表
		{"LocalFile", []string{"SongID", "NoSuchColumn"}, ErrNoColumn},
		{"LocalFile", []string{"Quoted Table"}, ErrNoColumn},
		{"Quoted Table", []string{"col"}, ErrNoColumn},     // 列名不做前缀匹配
		{"Quoted Table", []string{`"col a"`}, ErrNoColumn}, // 调用方给的是去引号后的列名
		{"wr", []string{"k"}, ErrUnsupportedTable},
		{"wr", []string{"NoSuchColumn"}, ErrUnsupportedTable}, // 先判表的种类，再判列
		{"vt", []string{"a"}, ErrUnsupportedTable},
		{"vt", []string{"NoSuchColumn"}, ErrUnsupportedTable},
		{"vt_idx", []string{"term"}, ErrUnsupportedTable},
		{"vt_config", []string{"k"}, ErrUnsupportedTable},
	}
	for _, c := range cases {
		called := false
		err := db.Scan(c.table, c.cols, func([]Value) bool { called = true; return true })
		if c.want == ErrUnsupportedTable {
			wantUnsupportedTable(t, err)
		} else {
			wantErr(t, err, c.want)
		}
		if called {
			t.Errorf("Scan(%q, %q) called fn despite error", c.table, c.cols)
		}
	}
}

func TestColumnsUnknownTable(t *testing.T) {
	db := mustOpen(t, plainPager(loadFixture(t, "basic.db")))
	for _, name := range []string{"NoSuchTable", "ix", "", "sqlite_master"} {
		cols, err := db.Columns(name)
		wantErr(t, err, ErrNoTable)
		if cols != nil {
			t.Errorf("Columns(%q) = %q, want nil", name, cols)
		}
	}
}

// 夹具里的 WITHOUT ROWID 表与虚拟表：Columns 也报不支持；同属 fts5、但是普通 rowid 表的影子表照常可读。
func TestUnsupportedTableKindsInFixture(t *testing.T) {
	db := mustOpen(t, plainPager(loadFixture(t, "basic.db")))
	for _, name := range []string{"wr", "vt", "VT", "vt_idx", "vt_config"} {
		cols, err := db.Columns(name)
		wantUnsupportedTable(t, err)
		if cols != nil {
			t.Errorf("Columns(%q) = %q, want nil", name, cols)
		}
	}
	var rows [][]string
	err := db.Scan("vt_content", []string{"id", "c0", "c1"}, func(row []Value) bool {
		rows = append(rows, []string{row[0].String(), row[1].String(), row[2].String()})
		return true
	})
	if err != nil || len(rows) != 1 || !slices.Equal(rows[0], []string{"1", "alpha", "beta"}) {
		t.Fatalf("Scan(vt_content) = %q, err %v; want one row [1 alpha beta]", rows, err)
	}
	for _, name := range []string{"vt_data", "vt_docsize"} {
		if _, err := scanAll(t, db, name); err != nil {
			t.Errorf("Scan(%s): %v", name, err)
		}
	}
}

// 虚拟表的两种判据各自成立：根页号为 0（建表语句是普通的、甚至解析不了），或建表语句以 CREATE VIRTUAL TABLE 开头
// （根页号非 0）。名字叫 virtual 的普通表不算。
func TestUnsupportedTableKindsByRootOrSQL(t *testing.T) {
	p := plainPager(loadFixture(t, "basic.db"))
	for _, tb := range []table{
		{name: "z", root: 0, sql: "CREATE TABLE z(a)"},
		{name: "z", root: 0, sql: "CREATE VIRTUAL TABLE z USING m('unclosed"},
		{name: "z", root: 2, sql: "CREATE VIRTUAL TABLE z USING fts5(a)"},
		{name: "z", root: 2, sql: "  create virtual table z using rtree(a, b, c)"},
	} {
		db := &DB{p: p, tables: []table{tb}}
		_, err := db.Columns("z")
		wantUnsupportedTable(t, err)
		err = db.Scan("z", []string{"a"}, func([]Value) bool { t.Errorf("%+v: fn called", tb); return true })
		wantUnsupportedTable(t, err)
	}

	db := &DB{p: p, tables: []table{{name: "virtual", root: 2, sql: "CREATE TABLE virtual(a)"}}}
	if cols, err := db.Columns("virtual"); err != nil || !slices.Equal(cols, []string{"a"}) {
		t.Fatalf("Columns(virtual) = %q, %v; want [a]", cols, err)
	}
}

// 根页号来自库内容：越界、为负，或超出 uint32（截断后会落在合法页号上）都判结构异常（为 0 见上，算虚拟表）。
func TestScanRejectsBadRootPage(t *testing.T) {
	p := plainPager(loadFixture(t, "basic.db"))
	for _, root := range []int64{-1, int64(p.PageCount()) + 1, 1<<32 + 2} {
		db := &DB{p: p, tables: []table{{name: "t", root: root, sql: "CREATE TABLE t(a)"}}}
		err := db.Scan("t", []string{"a"}, func([]Value) bool { return true })
		wantErr(t, err, ErrCorrupt)
	}
}

// 建表语句解析不了：Columns 与 Scan 都判结构异常。
func TestUnparsableCreateTable(t *testing.T) {
	db := &DB{p: plainPager(loadFixture(t, "basic.db")), tables: []table{{name: "t", root: 2, sql: "CREATE TABLE t"}}}
	_, err := db.Columns("t")
	wantErr(t, err, ErrCorrupt)
	err = db.Scan("t", nil, func([]Value) bool { return true })
	wantErr(t, err, ErrCorrupt)
}
