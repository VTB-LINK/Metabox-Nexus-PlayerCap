package assoc

// 本文件测磁盘模式的取消关联哨兵、代次锁存与偏移优先级（#250/#251，设计 §6.4.3、§6.5）：
//   - 右键菜单取消关联哨兵（419721cb...）在当首与切走重播均产出 KindUnassociated；
//   - 关联弹窗取消关联哨兵（a9695a2f...）仅在同一次播放从非弹窗键跃迁时产出 KindUnassociated，
//     切走重播（PlayedAt 变大）解除锁存并回落返回 SourceCache；
//   - 非 P 历史表中的任一哨兵键既不误判 KindUnassociated 也不误置 AssocSkipped；
//   - 文件 [offset:] 优先于 LyricInfo.RealOffset（含 [offset:0] 防双重叠加），无文件标签回落 RealOffset，
//     两者均钳位在 [-600_000, +600_000]，缺 RealOffset 列时兼容返回 0。

import (
	"bytes"
	"compress/zlib"
	"slices"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
)

const (
	testMenuSentinel   = "419721cb5e7bf802fd14b272f5d5322e"
	testDialogSentinel = "a9695a2f209c200cd9d393fa58d5f915"
)

var testAssocKRCKey = [16]byte{0x40, 0x47, 0x61, 0x77, 0x5E, 0x32, 0x74, 0x47, 0x51, 0x36, 0x31, 0x2D, 0xCE, 0xD2, 0x6E, 0x69}

func encodeTestKRC(t *testing.T, plain string) []byte {
	t.Helper()
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if _, err := zw.Write([]byte(plain)); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	out := append([]byte("krc1"), z.Bytes()...)
	for i := range out[4:] {
		out[4+i] ^= testAssocKRCKey[i%16]
	}
	return out
}

// lyricRowWithOffset 返回带 RealOffset 值的 LyricInfo 表（含全部实测列）。
func lyricInfoWithOffsets(rows ...struct {
	row        lyricRow
	realOffset int64
}) *fakeTable {
	t := &fakeTable{name: "LyricInfo", cols: lyricInfoCols}
	for _, r := range rows {
		t.rows = append(t.rows, []kgdb.Value{
			text(r.row.key), integer(0), integer(r.row.updateTime), text(r.row.path),
			{}, {}, text(r.row.songHash), integer(0), integer(r.realOffset), integer(0),
			integer(0), integer(0), {}, text(""), integer(0), text(""), integer(0),
		})
	}
	return t
}

func TestDiskMenuSentinelPersistsAcrossReplay(t *testing.T) {
	const (
		localAudio = `D:\Music\歌手 - 歌.flac`
		cacheFile  = `cache.lrc`
	)
	h := hex32("aa", 1)
	other := hex32("aa", 2)

	var mu sync.Mutex
	curPlayed := int64(1000)

	f := &fakeDeps{
		colFn: func(_ int, hash string) (collected, error) {
			mu.Lock()
			played := curPlayed
			mu.Unlock()
			db := newDB(
				playedTable("PlayQueueAssistItem_0",
					playedRow{h, testMenuSentinel, localAudio, integer(played)},
					playedRow{other, "", `D:\Music\other.flac`, integer(played + 10)},
				),
				lyricInfo(
					lyricRow{hex32("cc", 1), cacheFile, h, 100},
					lyricRow{hex32("cc", 2), "a.lrc", other, 100},
				),
			)
			return collect(db, hash)
		},
		readFn: func(_ time.Duration, path string) ([]byte, error) {
			if path == okConfirmed.LyricDir+`\`+cacheFile || path == cachePath {
				return []byte(testLRC), nil
			}
			return nil, errNotDiskFile
		},
	}

	runReader(t, f, func(t *testing.T, r *Reader) {
		// 当首（PlayedAt = T1）：右键菜单取消关联产出 KindUnassociated，不回落缓存。
		r.SetHash(h)
		res := latest(t, r)
		wantKind(t, res, h, KindUnassociated)
		if res.AssocSkipped || res.PlayedAt != 1000 || !res.LocalFile || res.LyricOffsetMs != 0 {
			t.Fatalf("首播结果 %+v，want KindUnassociated、PlayedAt=1000、LocalFile=true、AssocSkipped=false", res)
		}

		// 切走播放另一首。
		r.SetHash(other)
		wantKind(t, latest(t, r), other, KindFound)

		// 重播（PlayedAt = T2 > T1）：右键菜单取消关联依然保持 KindUnassociated。
		mu.Lock()
		curPlayed = 2000
		mu.Unlock()
		r.SetHash(h)
		res = latest(t, r)
		wantKind(t, res, h, KindUnassociated)
		if res.AssocSkipped || res.PlayedAt != 2000 || !res.LocalFile {
			t.Fatalf("重播结果 %+v，want KindUnassociated、PlayedAt=2000、LocalFile=true、AssocSkipped=false", res)
		}
	})
}

func TestDiskDialogSentinelClearsCurrentPlayAndFallsBackOnReplay(t *testing.T) {
	const (
		localAudio = `D:\Music\歌手 - 歌.flac`
		assocFile  = `D:\assoc.lrc`
		cacheFile  = `cache.lrc`
	)
	h := hex32("aa", 1)
	other := hex32("aa", 2)
	assocKey := hex32("bb", 1)

	for _, tc := range []struct {
		name       string
		initialKey string
		wantSrc    Source
	}{
		{"空键跃迁为弹窗取消", "", SourceCache},
		{"关联键跃迁为弹窗取消", assocKey, SourceAssociation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			pKey := tc.initialKey
			played := int64(1000)
			mtimeSec := int64(1000)

			f := &fakeDeps{
				colFn: func(_ int, hash string) (collected, error) {
					mu.Lock()
					k, p := pKey, played
					mu.Unlock()
					db := newDB(
						playedTable("PlayQueueAssistItem_0",
							playedRow{h, k, localAudio, integer(p)},
							playedRow{other, "", `D:\Music\other.flac`, integer(p + 1)},
						),
						lyricInfo(
							lyricRow{assocKey, assocFile, zeroKey, 200},
							lyricRow{hex32("cc", 1), cacheFile, h, 100},
							lyricRow{hex32("cc", 2), "a.lrc", other, 100},
						),
					)
					return collect(db, hash)
				},
				statFn: func(_ time.Duration) (int64, time.Time, error) {
					mu.Lock()
					sec := mtimeSec
					mu.Unlock()
					return 1 << 20, time.Unix(sec, 0), nil
				},
				readFn: func(_ time.Duration, path string) ([]byte, error) {
					switch path {
					case assocFile, okConfirmed.LyricDir + `\` + cacheFile, cachePath:
						return []byte(testLRC), nil
					}
					return nil, errNotDiskFile
				},
			}

			runReader(t, f, func(t *testing.T, r *Reader) {
				// 初始播放（PlayedAt = T1）：产出初始歌词。
				r.SetHash(h)
				res := latest(t, r)
				wantKind(t, res, h, KindFound)
				if res.Source != tc.wantSrc || res.PlayedAt != 1000 {
					t.Fatalf("初始结果 %+v，want Source=%v、PlayedAt=1000", res, tc.wantSrc)
				}

				// 同一次播放（PlayedAt 仍为 T1）内 P.key 跃迁为弹窗哨兵，且库文件 mtime 更新并稳定 1s。
				mu.Lock()
				pKey = testDialogSentinel
				mtimeSec = 1001
				mu.Unlock()
				time.Sleep(3 * time.Second)
				res = latest(t, r)
				wantKind(t, res, h, KindUnassociated)
				if res.PlayedAt != 1000 || !res.LocalFile {
					t.Fatalf("弹窗取消结果 %+v，want KindUnassociated、PlayedAt=1000、LocalFile=true", res)
				}

				// 切走再重播（PlayedAt = T2 > T1，键仍为弹窗哨兵）：解除锁存并回落缓存歌词。
				r.SetHash(other)
				wantKind(t, latest(t, r), other, KindFound)

				mu.Lock()
				played = 2000
				mu.Unlock()
				r.SetHash(h)
				res = latest(t, r)
				wantKind(t, res, h, KindFound)
				if res.Source != SourceCache || res.PlayedAt != 2000 {
					t.Fatalf("重播结果 %+v，want 回落 SourceCache、PlayedAt=2000", res)
				}
			})
		})
	}

	t.Run("非P历史表含哨兵键不误判也不误置AssocSkipped", func(t *testing.T) {
		for _, sentinel := range []string{testMenuSentinel, testDialogSentinel} {
			// 情形 1：有 P（空键），LocalFile / RecentlyPlayedItem / 较早队列行含哨兵键（不同路径以触发 1甲 扫描）。
			dbWithP := newDB(
				playedTable("LocalFile", playedRow{h, sentinel, `E:\Other\song.flac`, kgdb.Value{}}),
				playedTable("RecentlyPlayedItem", playedRow{h, sentinel, `F:\Other\song.flac`, integer(9999)}),
				playedTable("PlayQueueAssistItem_0",
					playedRow{h, sentinel, `E:\Other\song.flac`, integer(500)},
					playedRow{h, "", localAudio, integer(1000)},
				),
				lyricInfo(lyricRow{hex32("cc", 1), cacheFile, h, 100}),
			)
			col := mustResolve(t, dbWithP, h)
			if col.keyed {
				t.Fatalf("哨兵 %s 在非 P 历史表触发了 keyed=true", sentinel)
			}
			f := &fakeRead{files: map[string]any{okConfirmed.LyricDir + `\` + cacheFile: testLRC}}
			res := pick(col, okConfirmed.LyricDir, f.fn)
			if res.Kind != KindFound || res.Source != SourceCache || res.AssocSkipped {
				t.Fatalf("有 P 时结果 %+v，want KindFound/SourceCache/AssocSkipped=false", res)
			}

			// 情形 2：没有 P，只有历史表含哨兵键。
			dbNoP := newDB(
				songTable("LocalFile", songRow{h, sentinel}),
				songTable("RecentlyPlayedItem", songRow{h, sentinel}),
				lyricInfo(lyricRow{hex32("cc", 1), cacheFile, h, 100}),
			)
			colNoP := mustResolve(t, dbNoP, h)
			if colNoP.keyed {
				t.Fatalf("无 P 时哨兵 %s 在历史表触发了 keyed=true", sentinel)
			}
			resNoP := pick(colNoP, okConfirmed.LyricDir, f.fn)
			if resNoP.Kind != KindFound || resNoP.Source != SourceCache || resNoP.AssocSkipped {
				t.Fatalf("无 P 时结果 %+v，want KindFound/SourceCache/AssocSkipped=false", resNoP)
			}
		}
	})
}

func TestDiskOffsetPriorityNoDoubleCount(t *testing.T) {
	const lyricDir = `C:\KuGou\Lyric`
	h := hex32("aa", 1)
	krcLine := "[1000,2000]<0,500,0>你<500,500,0>好\n"

	krcWith1500 := encodeTestKRC(t, "[offset:1500]\n"+krcLine)
	krcWith0 := encodeTestKRC(t, "[offset:0]\n"+krcLine)
	krcNoOffset := encodeTestKRC(t, krcLine)
	krcOverMax := encodeTestKRC(t, "[offset:900000]\n"+krcLine)

	readBytes := func(data []byte) func(string) ([]byte, error) {
		return func(string) ([]byte, error) { return data, nil }
	}

	t.Run("krc含offset:1500且RealOffset=1500不双重叠加", func(t *testing.T) {
		db := newDB(lyricInfoWithOffsets(struct {
			row        lyricRow
			realOffset int64
		}{lyricRow{hex32("cc", 1), "a.krc", h, 1}, 1500}))
		col := mustResolve(t, db, h)
		r := pick(col, lyricDir, readBytes(krcWith1500))
		if r.Kind != KindFound || r.LyricOffsetMs != 1500 {
			t.Fatalf("结果 Kind=%v LyricOffsetMs=%d，want KindFound / 1500", r.Kind, r.LyricOffsetMs)
		}
	})

	t.Run("krc含offset:0且RealOffset=1500取文件值0", func(t *testing.T) {
		db := newDB(lyricInfoWithOffsets(struct {
			row        lyricRow
			realOffset int64
		}{lyricRow{hex32("cc", 1), "a.krc", h, 1}, 1500}))
		col := mustResolve(t, db, h)
		r := pick(col, lyricDir, readBytes(krcWith0))
		if r.Kind != KindFound || r.LyricOffsetMs != 0 {
			t.Fatalf("结果 Kind=%v LyricOffsetMs=%d，want KindFound / 0", r.Kind, r.LyricOffsetMs)
		}
	})

	t.Run("文件无offset标签且RealOffset=-800取-800", func(t *testing.T) {
		db := newDB(lyricInfoWithOffsets(struct {
			row        lyricRow
			realOffset int64
		}{lyricRow{hex32("cc", 1), "a.krc", h, 1}, -800}))
		col := mustResolve(t, db, h)
		r := pick(col, lyricDir, readBytes(krcNoOffset))
		if r.Kind != KindFound || r.LyricOffsetMs != -800 {
			t.Fatalf("结果 Kind=%v LyricOffsetMs=%d，want KindFound / -800", r.Kind, r.LyricOffsetMs)
		}
	})

	t.Run("超界RealOffset与文件offset均钳位在[-600000,+600000]", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			content []byte
			realOff int64
			want    int
		}{
			{"RealOffset正超界", krcNoOffset, 999_999, 600_000},
			{"RealOffset负超界", krcNoOffset, -999_999, -600_000},
			{"文件offset正超界", krcOverMax, 0, 600_000},
		} {
			t.Run(tc.name, func(t *testing.T) {
				db := newDB(lyricInfoWithOffsets(struct {
					row        lyricRow
					realOffset int64
				}{lyricRow{hex32("cc", 1), "a.krc", h, 1}, tc.realOff}))
				col := mustResolve(t, db, h)
				r := pick(col, lyricDir, readBytes(tc.content))
				if r.Kind != KindFound || r.LyricOffsetMs != tc.want {
					t.Fatalf("结果 Kind=%v LyricOffsetMs=%d，want KindFound / %d", r.Kind, r.LyricOffsetMs, tc.want)
				}
			})
		}
	})

	t.Run("LyricInfo无RealOffset列兼容返回0", func(t *testing.T) {
		li := withoutColumn("RealOffset", lyricRow{hex32("cc", 1), "a.krc", h, 1})
		if slices.Contains(li.cols, "RealOffset") {
			t.Fatal("测试夹具未移除 RealOffset 列")
		}
		col := mustResolve(t, newDB(li), h)
		r := pick(col, lyricDir, readBytes(krcNoOffset))
		if r.Kind != KindFound || r.LyricOffsetMs != 0 {
			t.Fatalf("结果 Kind=%v LyricOffsetMs=%d，want KindFound / 0", r.Kind, r.LyricOffsetMs)
		}
	})
}

func TestReadPlayingRowTwoPassConsistent(t *testing.T) {
	plain := fixturePlain(t, "kugou_like.db")
	m := encMemFS(t, plain)
	p, err := readPlayingRow(t.Context(), testDBPath, sampleTonghua, m.fs())
	if err != nil {
		t.Fatalf("readPlayingRow: %v", err)
	}
	if m.opens != 2 {
		t.Fatalf("opened %d times, want 2 (two passes)", m.opens)
	}
	m.wantClosed(t)
	// fixture 中 PlayQueueAssistItem_0 无 LastPlayTime 列，p 应为零值且不报错。
	if p.played != 0 {
		t.Fatalf("playing = %+v，want played=0", p)
	}
	// 空 hash 或全 0 hash 不开库直接返回零值。
	mZero := encMemFS(t, plain)
	if _, err := readPlayingRow(t.Context(), testDBPath, zeroKey, mZero.fs()); err != nil {
		t.Fatalf("readPlayingRow(zeroKey): %v", err)
	}
	if mZero.opens != 0 {
		t.Fatalf("全 0 hash 开了 %d 次库，want 0", mZero.opens)
	}
}
