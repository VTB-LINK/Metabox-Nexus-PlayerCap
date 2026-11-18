package assoc

// 本文件只测 LyricInfo 取不到时 collect 的错误：缺表、缺必需的列、表的种类不支持判为库不支持
// （kgdb.ErrUnsupportedDB），LyricInfo 的其余错误原样返回。

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
)

// withoutColumn 返回去掉 col 一列的 LyricInfo（行里的值一并去掉）。
func withoutColumn(col string, rows ...lyricRow) *fakeTable {
	t := lyricInfo(rows...)
	i := slices.Index(t.cols, col)
	t.cols = slices.Delete(slices.Clone(t.cols), i, i+1)
	for j, r := range t.rows {
		t.rows[j] = slices.Delete(r, i, i+1)
	}
	return t
}

func TestCollectSchemaErrors(t *testing.T) {
	h := hex32("aa", 1)
	row := lyricRow{hex32("cc", 1), "cache.lrc", h, 1}
	songs := songTable("LocalFile", songRow{h, hex32("bb", 1)})
	unsupported := func() *fakeTable {
		li := lyricInfo(row)
		li.columnsErr = fmt.Errorf("%w: table LyricInfo is WITHOUT ROWID", kgdb.ErrUnsupportedTable)
		li.scanErr = li.columnsErr
		return li
	}
	cases := []struct {
		name string
		db   *fakeTables
	}{
		{"缺 LyricInfo", newDB(songs)},
		{"缺 Key 列", newDB(songs, withoutColumn("Key", row))},
		{"缺 LyricPath 列", newDB(songs, withoutColumn("LyricPath", row))},
		{"缺 SongHash 列", newDB(songs, withoutColumn("SongHash", row))},
		{"缺 UpdateTime 列", newDB(songs, withoutColumn("UpdateTime", row))},
		{"LyricInfo 种类不支持", newDB(songs, unsupported())},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := collect(c.db, h)
			if !errors.Is(err, kgdb.ErrUnsupportedDB) || got.cands != nil {
				t.Fatalf("collect = (%+v, %v)，want (nil, ErrUnsupportedDB)", got, err)
			}
		})
	}
}

// 反例：缺的是用不到的列（LyricType）不算失败；LyricInfo 结构异常、读失败原样返回，不判为库不支持。
// LyricInfo 的 Columns 结构异常同样原样返回，不像其他表那样跳过（裁定 K 只针对 LyricInfo 以外的表；
// 内存表此时 Scan 照常成功，跳过即得到候选而无错误）。
func TestCollectLyricInfoOtherErrors(t *testing.T) {
	h := hex32("aa", 1)
	row := lyricRow{hex32("cc", 1), "cache.lrc", h, 1}
	wantCandidates(t, mustCollect(t, newDB(withoutColumn("LyricType", row)), h), []candidate{{"cache.lrc", SourceCache, 1}})

	corrupt := fmt.Errorf("%w: invalid record header", kgdb.ErrCorrupt)
	readFail := errors.New("read failed")
	cases := []struct {
		name string
		set  func(*fakeTable)
		want error
	}{
		{"Scan 结构异常", func(t *fakeTable) { t.scanErr = corrupt }, kgdb.ErrCorrupt},
		{"Scan 读失败", func(t *fakeTable) { t.scanErr = readFail }, readFail},
		{"Columns 结构异常", func(t *fakeTable) { t.columnsErr = corrupt }, kgdb.ErrCorrupt},
	}
	for _, c := range cases {
		li := lyricInfo(row)
		c.set(li)
		got, err := collect(newDB(li), h)
		if !errors.Is(err, c.want) || errors.Is(err, kgdb.ErrUnsupportedDB) || got.cands != nil {
			t.Fatalf("%s：collect = (%+v, %v)，want (nil, %v)", c.name, got, err, c.want)
		}
	}
}
