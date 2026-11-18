package kgdb

import (
	"slices"
	"testing"
)

// 本文件只测从 sqlite_master 读出的表与列：Tables 的取舍与顺序、Columns 对 CREATE TABLE 文本的解析。

// 不支持的表种类（WITHOUT ROWID 的 wr、vt_idx、vt_config，虚拟表 vt）照常列出；fts5 的影子表也是表。
func TestOpenBasicTables(t *testing.T) {
	got := mustOpen(t, plainPager(loadFixture(t, "basic.db"))).Tables()
	want := []string{"LyricInfo", "LocalFile", "Quoted Table", "wr",
		"vt", "vt_data", "vt_idx", "vt_content", "vt_docsize", "vt_config", "d", "a"}
	if !slices.Equal(got, want) {
		t.Fatalf("Tables() = %q, want %q", got, want)
	}
	// 索引（含自动索引）不是表。
	for _, name := range []string{"ix", "sqlite_autoindex_LyricInfo_1", "sqlite_autoindex_Quoted Table_1"} {
		if slices.Contains(got, name) {
			t.Errorf("Tables() contains index %q", name)
		}
	}
}

func TestTablesReturnsCopy(t *testing.T) {
	db := mustOpen(t, plainPager(loadFixture(t, "basic.db")))
	db.Tables()[0] = "changed"
	if got := db.Tables()[0]; got != "LyricInfo" {
		t.Fatalf("Tables()[0] = %q after caller modified a previous result", got)
	}
}

func TestColumnsParsesConstraintsAndQuotes(t *testing.T) {
	db := mustOpen(t, plainPager(loadFixture(t, "basic.db")))
	cases := map[string][]string{
		"Quoted Table": {"col a", "col b", "col c", "d"},
		"LyricInfo": {"Key", "KRCID", "UpdateTime", "LyricPath", "SingerNames", "SongName", "SongHash", "Duration",
			"RealOffset", "RecommendOffset", "LastCheckTime", "LyricType", "ExternalInfo", "AISongID", "Expire",
			"VocabID", "VocabCheckTime"},
		// ALTER TABLE ADD COLUMN 之后 SQLite 改写了建表语句。
		"LocalFile": {"SongID", "TitleName", "MusicFileHash", "LyricKey", "Pad", "Extra"},
		// 表名用单引号：CREATE TABLE 'vt_content'(id INTEGER PRIMARY KEY, c0, c1)。
		"vt_content": {"id", "c0", "c1"},
	}
	for table, want := range cases {
		got, err := db.Columns(table)
		if err != nil {
			t.Errorf("Columns(%q): %v", table, err)
			continue
		}
		if !slices.Equal(got, want) {
			t.Errorf("Columns(%q) = %q, want %q", table, got, want)
		}
	}
}

// parseCreateTable 的边界：引号内的括号与逗号、转义的引号、各种表约束、WITHOUT ROWID 的识别，以及解析失败的输入。
func TestParseCreateTable(t *testing.T) {
	cases := []struct {
		sql          string
		cols         []string
		rowid        []bool
		withoutRowid bool
	}{
		{`CREATE TABLE "a(b"(x, "y,z" TEXT DEFAULT 'p,(q', [w)] INT)`, []string{"x", "y,z", "w)"}, []bool{false, false, false}, false},
		{`CREATE TABLE t("a""b" TEXT, ` + "`c``d`" + ` INT, 'e''f')`, []string{`a"b`, "c`d", "e'f"}, []bool{false, false, false}, false},
		{"CREATE TABLE t(a, CONSTRAINT pk PRIMARY KEY(a), FOREIGN KEY(a) REFERENCES u(b), UNIQUE(a), CHECK(a > 0))", []string{"a"}, []bool{false}, false},
		{"CREATE TABLE t(id integer primary key autoincrement, n DECIMAL(10, 2) NOT NULL)", []string{"id", "n"}, []bool{true, false}, false},
		// 主键但不是 INTEGER（char、INT），或 INTEGER 但不是主键：都不是 rowid 的别名。
		{"CREATE TABLE t(a char(32) PRIMARY KEY, b INT PRIMARY KEY, c INTEGER, d INTEGER DEFAULT 'PRIMARY KEY')", []string{"a", "b", "c", "d"}, []bool{false, false, false, false}, false},
		// 列约束 PRIMARY KEY 后紧跟 DESC 不是别名；ASC、无排序关键字、字符串 'DESC' 仍是别名。
		{"CREATE TABLE t(a INTEGER PRIMARY KEY DESC, b integer primary key desc)", []string{"a", "b"}, []bool{false, false}, false},
		{"CREATE TABLE t(a INTEGER NOT NULL CONSTRAINT pk PRIMARY KEY DESC ON CONFLICT FAIL)", []string{"a"}, []bool{false}, false},
		{"CREATE TABLE t(a INTEGER PRIMARY KEY ASC, b INTEGER PRIMARY KEY, c INTEGER PRIMARY KEY DEFAULT 'DESC')", []string{"a", "b", "c"}, []bool{true, true, true}, false},
		{"CREATE TABLE t(k TEXT PRIMARY KEY, v)\nwithout\trowid", []string{"k", "v"}, []bool{false, false}, true},
		// 列名里的 WITHOUT ROWID 不算。
		{`CREATE TABLE t("WITHOUT", "ROWID")`, []string{"WITHOUT", "ROWID"}, []bool{false, false}, false},
	}
	for _, c := range cases {
		def, ok := parseCreateTable(c.sql)
		if !ok {
			t.Errorf("parseCreateTable(%q) failed", c.sql)
			continue
		}
		var cols []string
		var rowid []bool
		for _, col := range def.cols {
			cols = append(cols, col.name)
			rowid = append(rowid, col.rowid)
		}
		if !slices.Equal(cols, c.cols) || !slices.Equal(rowid, c.rowid) || def.withoutRowid != c.withoutRowid {
			t.Errorf("parseCreateTable(%q) = %q %v withoutRowid=%v, want %q %v %v",
				c.sql, cols, rowid, def.withoutRowid, c.cols, c.rowid, c.withoutRowid)
		}
	}

	for _, sql := range []string{
		"",
		"CREATE TABLE t",                 // 没有列表
		"CREATE TABLE t(a, b",            // 括号不闭合
		"CREATE TABLE t(a, 'b)",          // 引号不闭合
		"CREATE TABLE t(a, , b)",         // 空项
		"CREATE TABLE t()",               // 没有列
		"CREATE TABLE t(PRIMARY KEY(a))", // 只有表约束
	} {
		if def, ok := parseCreateTable(sql); ok {
			t.Errorf("parseCreateTable(%q) = %+v, want failure", sql, def)
		}
	}
}

// 虚拟表以 CREATE VIRTUAL TABLE 开头（不分大小写、词间任意空白），参数写法不必能解析；
// 表名或列名里出现 virtual 不算。
func TestParseCreateTableVirtual(t *testing.T) {
	for _, c := range []struct {
		sql     string
		virtual bool
	}{
		{"CREATE VIRTUAL TABLE vt USING fts5(a, b)", true},
		{"create\n\tvirtual  Table x using rtree(id, a, b)", true},
		{"CREATE VIRTUAL TABLE v USING fts4", true},     // 没有括号
		{"CREATE VIRTUAL TABLE v USING m(a, ", true},    // 括号不闭合
		{"CREATE TABLE virtual(a)", false},              // 表名叫 virtual
		{`CREATE TABLE "VIRTUAL TABLE"(a)`, false},      // 带引号的表名
		{"CREATE TABLE t(virtual, \"VIRTUAL\")", false}, // 列名
	} {
		def, ok := parseCreateTable(c.sql)
		if !ok || def.virtual != c.virtual {
			t.Errorf("parseCreateTable(%q): ok=%v virtual=%v, want ok virtual=%v", c.sql, ok, def.virtual, c.virtual)
		}
	}
}
