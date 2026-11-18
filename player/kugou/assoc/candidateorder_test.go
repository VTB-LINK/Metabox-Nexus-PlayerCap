package assoc

// 本文件只测 collect 给出的候选顺序：关联一级在缓存一级之前，各级内按 UpdateTime 降序。

import (
	"slices"
	"testing"
)

func mustCollect(t *testing.T, db tables, hash string) []candidate {
	t.Helper()
	return mustResolve(t, db, hash).cands
}

// mustResolve 返回 collect 的完整结局（含 keyed、playedAt）。
func mustResolve(t *testing.T, db tables, hash string) collected {
	t.Helper()
	got, err := collect(db, hash)
	if err != nil {
		t.Fatalf("collect(%q): %v", hash, err)
	}
	return got
}

func wantCandidates(t *testing.T, got, want []candidate) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("候选 = %+v\nwant %+v", got, want)
	}
}

// 缓存那一行在表里排在前面、UpdateTime 也更大，关联仍在前：两级不按时间混排。
// 关联指向的那一行 SongHash 也等于播放 hash，只按关联计一次，不再作为缓存出现。
func TestCollectManualBeforeCache(t *testing.T) {
	h, k := hex32("aa", 1), hex32("bb", 1)
	db := newDB(
		songTable("LocalFile", songRow{h, k}),
		lyricInfo(
			lyricRow{hex32("cc", 1), "cache.lrc", h, 200},
			lyricRow{k, `D:\关联.krc`, h, 100},
		),
	)
	wantCandidates(t, mustCollect(t, db, h), []candidate{
		{`D:\关联.krc`, SourceAssociation, 100},
		{"cache.lrc", SourceCache, 200},
	})
}

// 两个 LyricKey 分在两张表、在 LyricInfo 里按时间升序排列：UpdateTime 大者在前。
// 时间介于两者之间的缓存仍排在两条关联之后。
func TestCollectLatestAssociationFirst(t *testing.T) {
	h, k1, k2 := hex32("aa", 1), hex32("bb", 1), hex32("bb", 2)
	db := newDB(
		songTable("LocalFile", songRow{h, k1}),
		songTable("RecentlyPlayedItem", songRow{h, k2}),
		lyricInfo(
			lyricRow{k1, `D:\old.lrc`, "", 100},
			lyricRow{hex32("cc", 1), "cache.lrc", h, 200},
			lyricRow{k2, `D:\new.lrc`, "", 300},
		),
	)
	wantCandidates(t, mustCollect(t, db, h), []candidate{
		{`D:\new.lrc`, SourceAssociation, 300},
		{`D:\old.lrc`, SourceAssociation, 100},
		{"cache.lrc", SourceCache, 200},
	})
}
