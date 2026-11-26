package assoc

// 本文件只测 collect 认哪些行：播放 hash 与库里的 hash 怎样算相等，哪些 LyricKey 不算关联。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
)

// 空串、32 个 0、只有空白、更短的全 0、NULL 的 LyricKey 都不算关联。LyricInfo 里恰有 Key 为空串与
// 全 0 的行：误把这些 LyricKey 当关联，就会把那两行取成关联候选。
func TestCollectIgnoresZeroAndEmptyKeys(t *testing.T) {
	h := hex32("aa", 1)
	songs := songTable("LocalFile",
		songRow{h, ""},
		songRow{h, zeroKey},
		songRow{h, "   "},
		songRow{h, "0000"},
	)
	songs.rows = append(songs.rows, []kgdb.Value{text(`D:\x.mp3`), {}, text(h), integer(1)}) // LyricKey 为 NULL
	db := newDB(songs, lyricInfo(
		lyricRow{"", `D:\empty.lrc`, hex32("dd", 1), 1},
		lyricRow{zeroKey, `D:\zero.lrc`, hex32("dd", 2), 2},
		lyricRow{"0000", `D:\short.lrc`, hex32("dd", 3), 3},
	))
	if got := mustCollect(t, db, h); len(got) != 0 {
		t.Fatalf("候选 = %+v，want 无", got)
	}
}

// 查询的 hash 为大写且两端带空白，库里为小写（LyricKey 也有大小写之差）：关联与缓存都命中。
// 库里的 hash 带空白、为大写同样命中。前 31 位相同的另一个 hash 不命中。
func TestCollectHashCaseAndSpace(t *testing.T) {
	h, k := sampleWumingzhi, keyWumingzhi
	near := h[:31] + "9" // 只差最后一位
	db := newDB(
		songTable("LocalFile", songRow{h, "1685C8E7F0A14B2C9D3E4F5A6B7C8D9E"}, songRow{near, hex32("ee", 1)}),
		lyricInfo(
			lyricRow{k, `D:\assoc.krc`, "", 300},
			lyricRow{hex32("cc", 1), "lower.lrc", h, 200},
			lyricRow{hex32("cc", 2), "upper.lrc", " 9D6E3B30C1F04A5E8B7D2C9A6F3E1D08\t", 100},
			lyricRow{hex32("cc", 3), "near.lrc", near, 400},
			lyricRow{hex32("ee", 1), `D:\near.krc`, "", 500},
		),
	)
	wantCandidates(t, mustCollect(t, db, " 9D6E3B30C1F04A5E8B7D2C9A6F3E1D08 \t"), []candidate{
		{Path: `D:\assoc.krc`, Source: SourceAssociation, UpdateTime: 300},
		{Path: "lower.lrc", Source: SourceCache, UpdateTime: 200},
		{Path: "upper.lrc", Source: SourceCache, UpdateTime: 100},
	})
}

// 播放 hash 为空串、只有空白（Review Focus：getPlayInfo 给出空 hash）或全 0（换歌过渡态，真机 X1）：
// 没有候选、不报错、不扫任何表。库里 MusicFileHash 与 SongHash 为同样的空或全 0 的行（手动关联的
// LyricInfo 行 SongHash 即全 0）若被当成相等，就会产生候选。
func TestCollectEmptyHash(t *testing.T) {
	for _, h := range []string{"", "  \t", zeroKey, " " + zeroKey + "\t", "0000"} {
		db := newDB(
			songTable("LocalFile", songRow{"", hex32("bb", 1)}, songRow{zeroKey, hex32("bb", 2)}),
			lyricInfo(
				lyricRow{hex32("bb", 1), `D:\assoc.krc`, "", 1},
				lyricRow{hex32("bb", 2), `D:\assoc0.krc`, zeroKey, 1},
				lyricRow{hex32("cc", 1), "cache.lrc", "", 2},
				lyricRow{hex32("cc", 2), `E:\manual.lrc`, zeroKey, 3},
			),
		)
		got, err := collect(db, h)
		if err != nil || len(got.cands) != 0 || got.keyed || got.playedAt != 0 || len(db.scanned) != 0 {
			t.Fatalf("collect(%q) = (%+v, %v)，扫了 %v；want 无候选、无错误、不扫表", h, got, err, db.scanned)
		}
	}
}
