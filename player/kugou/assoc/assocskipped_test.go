package assoc

// 本文件只测结果里区分「同一次播放中关联失效」的两项（设计 §6.5 第 4 步）：
//   - AssocSkipped（关联未取用）：第 1 步得出了关联键、而结果不是关联（键在 LyricInfo 里没有行、文件暂缺、文件不可用）
//     时为真；第 1 步没有关联键（含超过 64 个而不用关联一级）、或结果是关联时为假。
//   - PlayedAt：正在播放的队列行 P 的 LastPlayTime，没有 P（不在队列表、队列表缺该列或全为 NULL）为 0；P 带不带关联都带。
//
// 经 collect 与 pick 走完整的解析，库形状复用 queueorder_test.go 与 candidatecap_test.go。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

func TestAssocSkippedAndPlayedAt(t *testing.T) {
	const lyricDir = `C:\KuGou\Lyric`
	x3CachePath := lyricDir + `\` + x3Cache
	wumingzhiCachePath := lyricDir + `\` + wumingzhiCache
	x3Played := lastPlay(clock(9, 4, 23, 0)).Int          // #20146
	wumingzhiPlayed := lastPlay(clock(14, 25, 14, 0)).Int // #20144
	overlongA04 := newDB(
		playedTable("PlayQueueAssistItem_0", playedRow{x3Hash, keyA04, x3Audio, lastPlay(clock(9, 4, 23, 0))}),
		lyricInfo(lyricRow{keyA04, overlong(`D:\`, maxPathBytes+1), zeroKey, 1}, x3CacheRow),
	)
	otherKey, otherPath := hex32("dd", 1), `D:\Lyrics\无名指-QQNT.krc`
	otherFile := wumingzhiDB(otherKey, lyricRow{otherKey, otherPath, "", updated(clock(7, 0, 0, 0))})
	h, k, song := hex32("aa", 1), hex32("bb", 1), `D:\Music\Song.flac`
	nullQueue := newDB(playedTable("PlayQueueAssistItem_0", playedRow{h, k, song, kgdb.Value{}}), lyricInfo(lyricRow{k, `D:\k.krc`, "", 1}))
	cases := []struct {
		name     string
		db       *fakeTables
		hash     string
		files    map[string]any
		kind     Kind
		src      Source
		retry    bool
		skipped  bool
		playedAt int64
	}{
		{"P 键悬空、有缓存", x3DB(keyDangling, x3CacheRow), x3Hash, map[string]any{x3CachePath: testLRC},
			KindFound, SourceCache, false, true, x3Played},
		{"P 键悬空、无缓存", x3DB(keyDangling), x3Hash, nil, KindNone, 0, false, true, x3Played},
		{"P 带关联、文件暂缺", x3DB(keyA04, x3CacheRow), x3Hash, map[string]any{x3CachePath: testLRC},
			KindFound, SourceCache, true, true, x3Played},
		{"P 带关联、文件暂缺、无缓存", x3DB(keyA04), x3Hash, nil, KindNone, 0, true, true, x3Played},
		{"P 带关联、文件零行", x3DB(keyA04, x3CacheRow), x3Hash, map[string]any{pathA04: "[ti:x]\n", x3CachePath: testLRC},
			KindFound, SourceCache, false, true, x3Played},
		{"P 带关联、文件超限", x3DB(keyA04, x3CacheRow), x3Hash,
			map[string]any{pathA04: klyric.ErrLyricTooLarge, x3CachePath: testLRC}, KindFound, SourceCache, false, true, x3Played},
		{"P 带关联、LyricPath 超长", overlongA04, x3Hash, map[string]any{x3CachePath: testLRC},
			KindFound, SourceCache, false, true, x3Played},
		{"结果是关联", x3DB(keyA04, x3CacheRow), x3Hash, map[string]any{pathA04: testLRC, x3CachePath: testLRC},
			KindFound, SourceAssociation, false, false, x3Played},
		{"P 不带关联、只有缓存", wumingzhiDB(zeroKey), wumingzhiHash, map[string]any{wumingzhiCachePath: testLRC},
			KindFound, SourceCache, false, false, wumingzhiPlayed},
		{"1甲、其他文件的关联取到", otherFile, wumingzhiHash, map[string]any{otherPath: testLRC, wumingzhiCachePath: testLRC},
			KindFound, SourceAssociation, false, false, wumingzhiPlayed},
		{"1甲、其他文件暂缺", otherFile, wumingzhiHash, map[string]any{wumingzhiCachePath: testLRC},
			KindFound, SourceCache, true, true, wumingzhiPlayed},
		{"没有 P、没有关联键", newDB(lyricInfo(x3CacheRow)), x3Hash, map[string]any{x3CachePath: testLRC},
			KindFound, SourceCache, false, false, 0},
		{"没有关联键、也没有缓存", newDB(lyricInfo()), x3Hash, nil, KindNone, 0, false, false, 0},
		{"队列表 LastPlayTime 全为 NULL", nullQueue, h, map[string]any{`D:\k.krc`: testLRC},
			KindFound, SourceAssociation, false, false, 0},
		// 队列表是 songTable，没有 LastPlayTime 列：没有 P。
		{"关联键 65 个、不用关联一级", manyKeysDB(h, 65, 0), h, map[string]any{lyricDir + `\cache2.lrc`: testLRC},
			KindFound, SourceCache, false, false, 0},
		// 反例：恰 64 个时用关联一级，关联文件全部暂缺，标记为真。
		{"关联键 64 个、文件都暂缺", manyKeysDB(h, 64, 0), h, map[string]any{lyricDir + `\cache2.lrc`: testLRC},
			KindFound, SourceCache, true, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var r Result
			captureLog(t, func() { r = pick(mustResolve(t, c.db, c.hash), lyricDir, (&fakeRead{files: c.files}).fn) })
			if r.Kind != c.kind || r.Source != c.src || r.Retry != c.retry || r.AssocSkipped != c.skipped || r.PlayedAt != c.playedAt {
				t.Fatalf("结果 Kind %d、Source %d、Retry %v、AssocSkipped %v、PlayedAt %d；want %d、%d、%v、%v、%d",
					r.Kind, r.Source, r.Retry, r.AssocSkipped, r.PlayedAt, c.kind, c.src, c.retry, c.skipped, c.playedAt)
			}
		})
	}
}

// 读取者发出的结果带上两项；没有走到 pick 的暂时失败两项都为零值。
func TestReaderSendsAssocSkippedAndPlayedAt(t *testing.T) {
	for name, c := range map[string]struct {
		col     collected
		skipped bool
	}{
		"关联暂缺、取缓存": {collected{cands: []candidate{assocCand, cacheCand}, keyed: true, playedAt: 300}, true},
		"只有缓存":     {collected{cands: []candidate{cacheCand}, playedAt: 300}, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeDeps{colFn: func(int, string) (collected, error) { return c.col, nil }}
			runReader(t, f, func(t *testing.T, r *Reader) {
				r.SetHash("A")
				res := latest(t, r)
				if res.Kind != KindFound || res.Source != SourceCache || res.AssocSkipped != c.skipped || res.PlayedAt != 300 {
					t.Fatalf("结果 %+v，want 缓存、AssocSkipped=%v、PlayedAt=300", res, c.skipped)
				}
			})
		})
	}
	t.Run("暂时失败", func(t *testing.T) {
		f := &fakeDeps{colFn: func(int, string) (collected, error) {
			return collected{keyed: true, playedAt: 300}, errTransientStub
		}}
		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash("A")
			if res := latest(t, r); res.Kind != KindTransient || res.AssocSkipped || res.PlayedAt != 0 {
				t.Fatalf("结果 %+v，want KindTransient、两项为零值", res)
			}
		})
	})
}
