package assoc

// 本文件只测 collect 扫哪些歌曲条目表：同时有 MusicFileHash 与 LyricKey 列的表都扫、其余不扫，
// 以及找表、扫表时出错的处理（LyricInfo 以外的表取不到列即跳过并打 Detail；已选中的表扫描时
// 种类不支持即跳过，其余错误原样返回）。

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
)

// 关联只在第三张同时有两列的表里：命中。只有其中一列的两张表不扫（它们的行即便 hash 相符也取不到 LyricKey）。
// 列名大小写不同的表照样算歌曲条目表。LyricInfo 在各歌曲条目表之后扫。
func TestCollectScansEverySongTable(t *testing.T) {
	h, k := hex32("aa", 1), hex32("bb", 1)
	onlyHash := &fakeTable{name: "OnlyHash", cols: []string{"MusicFileHash", "Other"}, rows: [][]kgdb.Value{{text(h), text(k)}}}
	onlyKey := &fakeTable{name: "OnlyKey", cols: []string{"Other", "LyricKey"}, rows: [][]kgdb.Value{{text(h), text(k)}}}
	lower := &fakeTable{name: "lowercols", cols: []string{"musicfilehash", "lyrickey"}}
	db := newDB(
		onlyHash,
		songTable("LocalFile", songRow{hex32("aa", 2), hex32("bb", 2)}),
		onlyKey,
		lower,
		songTable("RecentlyPlayedItem"),
		songTable("PlayQueueAssistItem_0", songRow{h, k}),
		lyricInfo(lyricRow{k, `D:\assoc.krc`, "", 1}, lyricRow{hex32("bb", 2), `D:\other.krc`, "", 2}),
	)
	wantCandidates(t, mustCollect(t, db, h), []candidate{{`D:\assoc.krc`, SourceAssociation, 1}})
	want := []string{"LocalFile", "lowercols", "RecentlyPlayedItem", "PlayQueueAssistItem_0", "LyricInfo"}
	if !slices.Equal(db.scanned, want) {
		t.Fatalf("扫过的表 = %v，want %v", db.scanned, want)
	}
}

// 只有 LyricInfo、没有任何歌曲条目表：只得缓存候选，不算失败。
func TestCollectNoSongTables(t *testing.T) {
	h := hex32("aa", 1)
	db := newDB(lyricInfo(lyricRow{hex32("bb", 1), `D:\assoc.krc`, "", 9}, lyricRow{hex32("cc", 1), "cache.lrc", h, 1}))
	wantCandidates(t, mustCollect(t, db, h), []candidate{{"cache.lrc", SourceCache, 1}})
}

// 一张歌曲条目表 Columns 返回 ErrUnsupportedTable（如虚拟表）、一张 Scan 返回它（如 WITHOUT ROWID 表）、
// 另一张命中关联：跳过前两张，关联照常取到。
func TestCollectSkipsUnsupportedSongTable(t *testing.T) {
	h, k := hex32("aa", 1), hex32("bb", 1)
	virtual := songTable("VirtualSongs", songRow{h, hex32("bb", 2)})
	virtual.columnsErr = fmt.Errorf("%w: table VirtualSongs is a virtual table", kgdb.ErrUnsupportedTable)
	noRowid := songTable("NoRowidSongs", songRow{h, hex32("bb", 3)})
	noRowid.scanErr = fmt.Errorf("%w: table NoRowidSongs is WITHOUT ROWID", kgdb.ErrUnsupportedTable)
	db := newDB(virtual, noRowid, songTable("LocalFile", songRow{h, k}), lyricInfo(lyricRow{k, `D:\assoc.krc`, "", 1}))
	wantCandidates(t, mustCollect(t, db, h), []candidate{{`D:\assoc.krc`, SourceAssociation, 1}})
}

// 裁定 K：一张无关表与一张歌曲条目表的建表语句解析不了（Columns 返回 ErrCorrupt），另一张歌曲条目表
// 命中关联：跳过那两张，关联照常取到、无错误，每张跳过的表各打一条带表名的 Detail。
func TestCollectSkipsUnreadableTable(t *testing.T) {
	h, k := hex32("aa", 1), hex32("bb", 1)
	future := &fakeTable{name: "FutureTable", cols: []string{"x"},
		columnsErr: fmt.Errorf("%w: cannot parse CREATE TABLE statement of table FutureTable", kgdb.ErrCorrupt)}
	odd := songTable("OddSongs", songRow{h, hex32("bb", 2)})
	odd.columnsErr = fmt.Errorf("%w: cannot parse CREATE TABLE statement of table OddSongs", kgdb.ErrCorrupt)
	db := newDB(future, odd, songTable("LocalFile", songRow{h, k}), lyricInfo(lyricRow{k, `D:\assoc.krc`, "", 1}))
	var got []candidate
	out := captureLog(t, func() { got = mustCollect(t, db, h) })
	wantCandidates(t, got, []candidate{{`D:\assoc.krc`, SourceAssociation, 1}})
	if strings.Count(out, "\n") != 2 || !strings.Contains(out, "FutureTable") || !strings.Contains(out, "OddSongs") ||
		strings.Count(out, "[KuGou] [Assoc] [+]") != 2 {
		t.Fatalf("日志 = %q，want 两条分别带表名的 Detail", out)
	}
	if slices.Contains(db.scanned, "OddSongs") {
		t.Fatalf("扫过的表 = %v，取不到列的表不该扫", db.scanned)
	}
}

// 已选中的歌曲条目表扫描时出别的错（结构异常、读失败）：原样返回，不当作表结构不认得（ErrUnsupportedDB），不吞掉。
func TestCollectSongTableErrorsReturned(t *testing.T) {
	h := hex32("aa", 1)
	corrupt := fmt.Errorf("%w: page 7 cell 2 is truncated", kgdb.ErrCorrupt)
	readFail := errors.New("read failed")
	cases := []struct {
		name    string
		scanErr error
		want    error
	}{
		{"Scan 结构异常", corrupt, kgdb.ErrCorrupt},
		{"Scan 读失败", readFail, readFail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bad := songTable("LocalFile", songRow{h, hex32("bb", 1)})
			bad.scanErr = c.scanErr
			db := newDB(bad, lyricInfo(lyricRow{hex32("cc", 1), "cache.lrc", h, 1}))
			got, err := collect(db, h)
			if !errors.Is(err, c.want) || errors.Is(err, kgdb.ErrUnsupportedDB) || got.cands != nil {
				t.Fatalf("collect = (%+v, %v)，want (nil, %v)", got, err, c.want)
			}
		})
	}
}
