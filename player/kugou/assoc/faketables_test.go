package assoc

// 本文件只放 collect 用例共用的内存表 fakeTables 及其构造函数，不含用例。

import (
	"fmt"
	"slices"
	"strings"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
)

// fakeTable 是一张内存表。columnsErr、scanErr 非空时 Columns、Scan 返回它们。
type fakeTable struct {
	name       string
	cols       []string
	rows       [][]kgdb.Value
	columnsErr error
	scanErr    error
}

// fakeTables 以内存表实现 tables，语义同 *kgdb.DB：表名、列名不分大小写；记录的列数少于表定义时
// 缺的列为 Null；没有的表、列分别返回包装 kgdb.ErrNoTable、kgdb.ErrNoColumn 的错误。scanned 按顺序
// 记下 Scan 过的表名（含返回错误的）。
type fakeTables struct {
	tables  []*fakeTable
	scanned []string
}

func (f *fakeTables) Tables() []string {
	names := make([]string, len(f.tables))
	for i, t := range f.tables {
		names[i] = t.name
	}
	return names
}

func (f *fakeTables) find(name string) (*fakeTable, error) {
	for _, t := range f.tables {
		if strings.EqualFold(t.name, name) {
			return t, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", kgdb.ErrNoTable, name)
}

func (f *fakeTables) Columns(name string) ([]string, error) {
	t, err := f.find(name)
	if err != nil {
		return nil, err
	}
	if t.columnsErr != nil {
		return nil, t.columnsErr
	}
	return slices.Clone(t.cols), nil
}

func (f *fakeTables) Scan(name string, cols []string, fn func(row []kgdb.Value) bool) error {
	f.scanned = append(f.scanned, name)
	t, err := f.find(name)
	if err != nil {
		return err
	}
	if t.scanErr != nil {
		return t.scanErr
	}
	idx := make([]int, len(cols))
	for i, c := range cols {
		idx[i] = slices.IndexFunc(t.cols, func(d string) bool { return strings.EqualFold(d, c) })
		if idx[i] < 0 {
			return fmt.Errorf("%w: %s.%s", kgdb.ErrNoColumn, t.name, c)
		}
	}
	for _, r := range t.rows {
		row := make([]kgdb.Value, len(cols))
		for i, j := range idx {
			if j < len(r) {
				row[i] = r[j]
			}
		}
		if !fn(row) {
			return nil
		}
	}
	return nil
}

func text(s string) kgdb.Value { return kgdb.Value{Kind: kgdb.Text, Bytes: []byte(s)} }

func integer(n int64) kgdb.Value { return kgdb.Value{Kind: kgdb.Integer, Int: n} }

// songRow 是歌曲条目表的一行：播放 hash 与 LyricKey。
type songRow struct{ hash, key string }

// songTable 建一张歌曲条目表。两列前后各有一列无关的列，列序不同于 collect 请求的顺序。
func songTable(name string, rows ...songRow) *fakeTable {
	t := &fakeTable{name: name, cols: []string{"FilePath", "LyricKey", "MusicFileHash", "FileSize"}}
	for i, r := range rows {
		t.rows = append(t.rows, []kgdb.Value{text(fmt.Sprintf(`D:\Music\%d.mp3`, i)), text(r.key), text(r.hash), integer(1)})
	}
	return t
}

// playedRow 是带 MusicFilePath 与 LastPlayTime 列的歌曲条目表的一行；played 为零值即该列为 NULL。
type playedRow struct {
	hash, key, path string
	played          kgdb.Value
}

// playedTable 建一张带 MusicFilePath 与 LastPlayTime 列的歌曲条目表（播放队列表、RecentlyPlayedItem、
// LocalFile 等），列序同样不同于 collect 请求的顺序。
func playedTable(name string, rows ...playedRow) *fakeTable {
	t := &fakeTable{name: name, cols: []string{"MusicFilePath", "LastPlayTime", "LyricKey", "MusicFileHash", "FileSize"}}
	for _, r := range rows {
		t.rows = append(t.rows, []kgdb.Value{text(r.path), r.played, text(r.key), text(r.hash), integer(1)})
	}
	return t
}

// lyricRow 是 LyricInfo 的一行。
type lyricRow struct {
	key, path, songHash string
	updateTime          int64
}

// lyricInfoCols 是设计 §2.2 实测的 LyricInfo 列序。
var lyricInfoCols = []string{"Key", "KRCID", "UpdateTime", "LyricPath", "SingerNames", "SongName", "SongHash",
	"Duration", "RealOffset", "RecommendOffset", "LastCheckTime", "LyricType", "ExternalInfo", "AISongID",
	"Expire", "VocabID", "VocabCheckTime"}

func lyricInfo(rows ...lyricRow) *fakeTable {
	t := &fakeTable{name: "LyricInfo", cols: lyricInfoCols}
	for _, r := range rows {
		t.rows = append(t.rows, []kgdb.Value{text(r.key), integer(0), integer(r.updateTime), text(r.path),
			{}, {}, text(r.songHash), integer(0), integer(0), integer(0), integer(0), integer(0), {}, text(""),
			integer(0), text(""), integer(0)})
	}
	return t
}

func newDB(tables ...*fakeTable) *fakeTables { return &fakeTables{tables: tables} }

// hex32 返回以 prefix 开头、其后补零写出 n 的十六进制、共 32 位的串，供测试造 hash 与 LyricKey。
func hex32(prefix string, n int) string { return fmt.Sprintf("%s%0*x", prefix, 32-len(prefix), n) }
