package assoc

// 本文件只测库内字符串的长度上限 maxPathBytes（设计 §6.10）：LyricInfo.LyricPath 与歌曲条目表的 LyricKey
// 超过上限的行跳过，不进候选、不进关联集合，也不计入关联与缓存两级的 64 条上限；恰为上限的照常。

import (
	"maps"
	"strings"
	"testing"
)

// overlong 返回 prefix 后补 a、以 .lrc 结尾、共 n 字节的路径（全为 ASCII，字节数即长度）。
func overlong(prefix string, n int) string {
	return prefix + strings.Repeat("a", n-len(prefix)-len(".lrc")) + ".lrc"
}

// 上限取 Windows 路径的最大长度：32767 个 UTF-16 单元，每单元转成 UTF-8 至多 3 字节。
func TestPathLimitValue(t *testing.T) {
	if maxPathBytes != 32767*3 {
		t.Fatalf("maxPathBytes = %d，want %d", maxPathBytes, 32767*3)
	}
}

// 缓存一级：恰为上限的照常，多 1 字节的跳过；超长那条 UpdateTime 最大也不取，其余照常、次序不变。
func TestCollectSkipsOverlongCachePath(t *testing.T) {
	h := hex32("aa", 1)
	at, over := overlong("", maxPathBytes), overlong("", maxPathBytes+1)
	db := newDB(lyricInfo(
		lyricRow{hex32("cc", 1), at, h, 1},
		lyricRow{hex32("cc", 2), over, h, 9},
		lyricRow{hex32("cc", 3), "b.lrc", h, 2},
	))
	wantCandidates(t, mustCollect(t, db, h), []candidate{{"b.lrc", SourceCache, 2}, {at, SourceCache, 1}})
}

// 关联一级：LyricKey 指向的行 LyricPath 恰为上限的照常，多 1 字节的跳过；其余关联与缓存照常。
func TestCollectSkipsOverlongAssociationPath(t *testing.T) {
	h := hex32("aa", 1)
	k1, k2, k3 := hex32("bb", 1), hex32("bb", 2), hex32("bb", 3)
	at, over := overlong(`D:\`, maxPathBytes), overlong(`D:\`, maxPathBytes+1)
	db := newDB(
		songTable("LocalFile", songRow{h, k1}, songRow{h, k2}, songRow{h, k3}),
		lyricInfo(
			lyricRow{k1, at, "", 1},
			lyricRow{k2, over, "", 9},
			lyricRow{k3, `D:\c.krc`, "", 2},
			lyricRow{hex32("cc", 1), "cache.lrc", h, 5},
		),
	)
	wantCandidates(t, mustCollect(t, db, h), []candidate{
		{`D:\c.krc`, SourceAssociation, 2},
		{at, SourceAssociation, 1},
		{"cache.lrc", SourceCache, 5},
	})
}

// 超长的 LyricKey 不进集合，恰为上限的与正常的照常进；collect 照常取正常键的关联。
func TestAssociationKeysSkipsOverlongKey(t *testing.T) {
	h := hex32("aa", 1)
	normal := hex32("bb", 1)
	atKey := strings.Repeat("b", maxPathBytes)
	overKey := strings.Repeat("b", maxPathBytes+1)
	db := newDB(
		songTable("LocalFile", songRow{h, overKey}, songRow{h, normal}),
		songTable("RecentlyPlayedItem", songRow{h, atKey}),
		lyricInfo(
			lyricRow{overKey, `D:\over.krc`, "", 9},
			lyricRow{normal, `D:\normal.krc`, "", 1},
		),
	)
	keys, _, err := associationKeys(db, h)
	if err != nil {
		t.Fatalf("associationKeys: %v", err)
	}
	if want := map[string]bool{normal: true, atKey: true}; !maps.Equal(keys, want) {
		t.Fatalf("集合有 %d 个键（含超长键：%v），want 正常键与恰为上限的键共 2 个", len(keys), keys[overKey])
	}
	wantCandidates(t, mustCollect(t, db, h), []candidate{{`D:\normal.krc`, SourceAssociation, 1}})
}

// 超长的行不计入两级的 64 条上限：本 hash 有 64 个正常 LyricKey 加 1 个超长的，关联一级照常取 64 条；
// 64 条正常缓存加 1 条超长且最新的缓存，64 条正常的全留。两处都不打「超过上限」的 Detail。
func TestOverlongRowsNotCounted(t *testing.T) {
	h := hex32("aa", 1)
	db := manyKeysDB(h, 64, 0)
	overKey := strings.Repeat("b", maxPathBytes+1)
	db.tables = append(db.tables, songTable("RecentlyPlayedItem", songRow{h, overKey}))
	var got []candidate
	out := captureLog(t, func() { got = mustCollect(t, db, h) })
	if len(got) != 67 || got[0].Source != SourceAssociation || got[63].Source != SourceAssociation {
		t.Fatalf("64 个正常关联加 1 个超长键：候选 %d 条，want 64 条关联在前、3 条缓存在后", len(got))
	}
	if out != "" {
		t.Fatalf("关联：日志 = %q，want 无", out)
	}

	rows := cacheRows(h, 64, func(i int) int { return i })
	rows = append(rows, lyricRow{hex32("cc", 999), overlong("", maxPathBytes+1), h, 999})
	out = captureLog(t, func() { got = mustCollect(t, newDB(lyricInfo(rows...)), h) })
	if len(got) != 64 || got[0].UpdateTime != 63 || got[63].UpdateTime != 0 {
		t.Fatalf("64 条正常缓存加 1 条超长：候选 %d 条（首 %d、末 %d），want 64 条正常缓存", len(got),
			got[0].UpdateTime, got[len(got)-1].UpdateTime)
	}
	if out != "" {
		t.Fatalf("缓存：日志 = %q，want 无", out)
	}
}
