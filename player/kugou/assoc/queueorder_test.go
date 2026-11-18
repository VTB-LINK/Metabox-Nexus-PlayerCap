package assoc

// 本文件只测关联一级只认正在播放的队列行 P（设计 §6.5 第 1 步）：P 是播放队列表（表名以 PlayQueueAssistItem
// 开头，不分大小写）里与 hash 相符、LastPlayTime 最大且大于 0 的行，并列取先扫到的。P 带关联时只用它；P 不带
// 关联时只用与 P 不是同一文件（MusicFilePath 不分大小写不同、且都不为空）的行的关联；没有 P 时全收、按 UpdateTime。
//
// X3、B01、无名指的路径与时刻取自 #205 真机验收（2026-11-17，本机时区 UTC-7）。LastPlayTime 写成 1601 年起的
// 100 ns 计数，UpdateTime 写成 1601 年起的微秒计数（设计 §2.3）。标「合成」的值不是实测。

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// clock 返回 2026-11-17 hh:mm:ss.mmm（UTC-7）。
func clock(h, m, s, ms int) time.Time {
	return time.Date(2026, 11, 17, h, m, s, ms*int(time.Millisecond), time.FixedZone("UTC-7", -7*3600))
}

const unixFrom1601 = 11644473600 // 1601-01-01 至 1970-01-01 的秒数

// lastPlay 把 t 写成 LastPlayTime 的值：1601-01-01 UTC 起的 100 ns 计数。
func lastPlay(t time.Time) kgdb.Value {
	return integer((t.Unix()+unixFrom1601)*10_000_000 + int64(t.Nanosecond()/100))
}

// updated 把 t 写成 UpdateTime 的值：1601-01-01 UTC 起的微秒计数。
func updated(t time.Time) int64 {
	return (t.Unix()+unixFrom1601)*1_000_000 + int64(t.Nanosecond()/1000)
}

const (
	x3Hash  = "da4ff27d31bdadf9e778485a0feb1360" // 童话镇伴奏
	x3Audio = `E:\Downloads\Lunette\伴奏\1_暗杠 - 童话镇_(Instrumental).wav`
	pathA04 = `E:\Downloads\Lunette\伴奏\A04.lrc`                          // 合成
	pathA11 = `E:\Downloads\Lunette\伴奏\A11.lrc`                          // 合成
	x3Cache = `暗杠 - 童话镇-da4ff27d31bdadf9e778485a0feb1360-1-00000000.krc` // 合成
)

var (
	keyA04, keyA11 = hex32("a", 4), hex32("a", 11) // 合成
	// keyDangling 是关联被拒时酷狗写入的、在 LyricInfo 里没有行的键；前 6 位取自设计 §2.3，其余合成。
	keyDangling = hex32("6c9087", 1)
)

// x3DB 是 X3 的三行（同一路径）：队列 #20142 键悬空、#20146 为 P 带 playingKey、「最近播放」#10893 带 A11
// 且 LastPlayTime 比 P 晚。LyricInfo 有 A04、A11 两行，A11 建得较晚、排在前面；extra 另加在其后。
// playingKey 为 keyA04 即 X3，为 keyDangling 即 B01。
func x3DB(playingKey string, extra ...lyricRow) *fakeTables {
	return newDB(
		playedTable("RecentlyPlayedItem", playedRow{x3Hash, keyA11, x3Audio, lastPlay(clock(9, 8, 33, 0))}), // #10893
		playedTable("PlayQueueAssistItem_0",
			playedRow{x3Hash, keyDangling, x3Audio, lastPlay(clock(8, 53, 16, 0))}, // #20142
			playedRow{x3Hash, playingKey, x3Audio, lastPlay(clock(9, 4, 23, 0))},   // #20146，即 P
		),
		lyricInfo(append([]lyricRow{
			{keyA11, pathA11, zeroKey, updated(clock(9, 8, 23, 0))},
			{keyA04, pathA04, zeroKey, updated(clock(8, 56, 45, 0))},
		}, extra...)...),
	)
}

var x3CacheRow = lyricRow{hex32("cc", 1), x3Cache, x3Hash, updated(clock(9, 10, 0, 0))} // 合成，比两条关联都新

// X3：关联一级只有 P 的 A04；「最近播放」的 A11 建得更晚、LastPlayTime 也更晚，不取。P 带关联时不再扫其余各表。
func TestCollectQueueX3(t *testing.T) {
	db := x3DB(keyA04)
	wantCandidates(t, mustCollect(t, db, x3Hash), []candidate{{pathA04, SourceAssociation, updated(clock(8, 56, 45, 0))}})
	if want := []string{"PlayQueueAssistItem_0", "LyricInfo"}; !slices.Equal(db.scanned, want) {
		t.Fatalf("扫过的表 = %v，want %v", db.scanned, want)
	}
}

// X3 走一遍 pick：取到 A04，只读这一份。
func TestPickQueueX3(t *testing.T) {
	f := &fakeRead{files: map[string]any{pathA04: "[00:01.00]A04\n", pathA11: "[00:01.00]A11\n"}}
	r := pick(mustResolve(t, x3DB(keyA04), x3Hash), "", f.fn)
	if r.Kind != KindFound || r.Source != SourceAssociation || r.Retry || len(r.Lines) != 1 || r.Lines[0].Text != "A04" {
		t.Fatalf("结果 %+v，want A04、SourceAssociation、不待重试", r)
	}
	if !slices.Equal(f.read, []string{pathA04}) {
		t.Fatalf("读了 %q，want 只读 A04", f.read)
	}
}

// B01：P 的键悬空（关联被拒），「最近播放」仍带 A11，该 hash 没有缓存：没有候选，但第 1 步得出了关联键；结果为没有、
// 不待重试、带「关联未取用」，什么也不读。不退到 A11。
func TestCollectQueueB01DanglingPlayingKey(t *testing.T) {
	col := mustResolve(t, x3DB(keyDangling), x3Hash)
	wantCandidates(t, col.cands, nil)
	if !col.keyed {
		t.Fatal("keyed = false，want true（P 的键悬空也算得出了关联键）")
	}
	f := &fakeRead{files: map[string]any{pathA11: "[00:01.00]A11\n"}}
	if r := pick(col, `C:\KuGou\Lyric`, f.fn); r.Kind != KindNone || r.Retry || !r.AssocSkipped || len(f.read) != 0 {
		t.Fatalf("结果 %+v，读了 %q；want KindNone、不待重试、关联未取用、不读", r, f.read)
	}
}

// P 带关联但文件不可用（零行、超限）或暂缺（读出错）：不退到「最近播放」的 A11，走缓存；只有暂缺带 Retry。
func TestPickQueuePlayingFileUnusable(t *testing.T) {
	col := mustResolve(t, x3DB(keyA04, x3CacheRow), x3Hash)
	wantCandidates(t, col.cands, []candidate{
		{pathA04, SourceAssociation, updated(clock(8, 56, 45, 0))},
		{x3Cache, SourceCache, updated(clock(9, 10, 0, 0))},
	})
	const lyricDir = `C:\KuGou\Lyric`
	for name, c := range map[string]struct {
		a04   any
		retry bool
	}{
		"零行": {"[ti:x]\n", false},
		"超限": {klyric.ErrLyricTooLarge, false},
		"暂缺": {os.ErrNotExist, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeRead{files: map[string]any{
				pathA04:                  c.a04,
				pathA11:                  "[00:01.00]A11\n",
				lyricDir + `\` + x3Cache: testLRC,
			}}
			r := pick(col, lyricDir, f.fn)
			if r.Kind != KindFound || r.Source != SourceCache || r.Retry != c.retry {
				t.Fatalf("结果 %+v，want 缓存、Retry=%v", r, c.retry)
			}
			if !slices.Equal(f.read, []string{pathA04, lyricDir + `\` + x3Cache}) {
				t.Fatalf("读了 %q，want 只读 A04 与缓存", f.read)
			}
		})
	}
}

const (
	wumingzhiHash  = "9d6e3b30f3f5d2134d826ab8e7f751b2"
	wumingzhiMusic = `C:\Users\oldking139\Music\1_门尼 - 无名指_(Instrumental).flac`
	wumingzhiQQNT  = `C:\Users\oldking139\Downloads\QQNT-DOWNLOAD\1_门尼 - 无名指_(Instrumental).flac`
	// wumingzhiCache 按设计 §2.3 的缓存文件名格式写成。
	wumingzhiCache = `1_门尼 - 无名指_(Instrumental)-9d6e3b30f3f5d2134d826ab8e7f751b2-565331094-00000000.lrc`
)

// wumingzhiDB 是无名指的各行：LocalFile #27925（QQNT 那份，带 qqntKey）、#27927（Music 那份，带 krc 关联）；
// 队列 #20134（Music，带 krc 关联）、#20144（Music，不带关联，即 P）；「最近播放」（Music，不带关联）；
// LyricInfo 有 krc 关联一行与该 hash 的缓存一行。extra 另加在 LyricInfo 末尾。
func wumingzhiDB(qqntKey string, extra ...lyricRow) *fakeTables {
	return newDB(
		playedTable("LocalFile",
			playedRow{wumingzhiHash, qqntKey, wumingzhiQQNT, kgdb.Value{}},       // #27925
			playedRow{wumingzhiHash, keyWumingzhi, wumingzhiMusic, kgdb.Value{}}, // #27927
		),
		playedTable("RecentlyPlayedItem", playedRow{wumingzhiHash, zeroKey, wumingzhiMusic, lastPlay(clock(14, 25, 14, 3))}), // 时刻合成
		playedTable("PlayQueueAssistItem_0",
			playedRow{wumingzhiHash, keyWumingzhi, wumingzhiMusic, lastPlay(clock(8, 48, 46, 0))}, // #20134
			playedRow{wumingzhiHash, zeroKey, wumingzhiMusic, lastPlay(clock(14, 25, 14, 0))},     // #20144，即 P
		),
		lyricInfo(append([]lyricRow{
			{keyWumingzhi, pathWumingzhiKRC, "bee95e0c1d2e3f4a5b6c7d8e9f0a1b2c", updated(clock(8, 40, 0, 0))}, // 时刻合成
			{hex32("cc", 2), wumingzhiCache, wumingzhiHash, updated(clock(8, 30, 0, 0))},                      // 时刻合成
		}, extra...)...),
	)
}

// 无名指：P 不带关联，同一文件（Music）的队列行与 LocalFile 行带的 krc 都不用；QQNT 那份是其他文件但不带关联。
// 只得缓存。
func TestCollectQueueWumingzhi(t *testing.T) {
	wantCandidates(t, mustCollect(t, wumingzhiDB(zeroKey), wumingzhiHash), []candidate{
		{wumingzhiCache, SourceCache, updated(clock(8, 30, 0, 0))},
	})
}

// 「1甲」：P 不带关联，其他文件（QQNT 那份）的 LocalFile 行带关联 K：关联一级只有 K，缓存在后；
// 同一文件的 krc 仍不用。
func TestCollectQueueOtherFileAssociation(t *testing.T) {
	k := hex32("dd", 1)
	db := wumingzhiDB(k, lyricRow{k, `D:\Lyrics\无名指-QQNT.krc`, "", updated(clock(7, 0, 0, 0))})
	wantCandidates(t, mustCollect(t, db, wumingzhiHash), []candidate{
		{`D:\Lyrics\无名指-QQNT.krc`, SourceAssociation, updated(clock(7, 0, 0, 0))},
		{wumingzhiCache, SourceCache, updated(clock(8, 30, 0, 0))},
	})
}

// 在线歌曲：各行的 MusicFilePath 为同一个文件名，按同一文件处理。P 不带关联时，「最近播放」同名行的关联不用；
// P 带关联时只用 P 的，较早的队列行另带的关联不用。
func TestCollectQueueOnlineSong(t *testing.T) {
	const name = "暗杠 - 童话镇.mp3"
	other := hex32("dd", 2)
	li := lyricInfo(
		lyricRow{keyTonghua, pathTonghuaLRC, zeroKey, 2},
		lyricRow{other, `D:\other.lrc`, zeroKey, 9},
		lyricRow{hex32("cc", 3), "online.krc", sampleTonghua, 1},
	)
	db := newDB(
		playedTable("RecentlyPlayedItem", playedRow{sampleTonghua, keyTonghua, name, integer(300)}),
		playedTable("PlayQueueAssistItem_0", playedRow{sampleTonghua, zeroKey, name, integer(200)}),
		li,
	)
	wantCandidates(t, mustCollect(t, db, sampleTonghua), []candidate{{"online.krc", SourceCache, 1}})

	db = newDB(
		playedTable("PlayQueueAssistItem_0",
			playedRow{sampleTonghua, other, name, integer(100)},
			playedRow{sampleTonghua, keyTonghua, name, integer(200)},
		),
		li,
	)
	wantCandidates(t, mustCollect(t, db, sampleTonghua), []candidate{
		{pathTonghuaLRC, SourceAssociation, 2},
		{"online.krc", SourceCache, 1},
	})
}

// 同一文件的判定：P 不带关联、路径为 D:\Music\Song.flac 时，路径大小写不同、两端有空白、为空、超长的行，
// 以及没有 MusicFilePath 列的表里的行，都按同一文件算、不收；只收路径不同的那一行。
// P 的路径为空、超长，或 P 所在的队列表没有 MusicFilePath 列时，一行也不收。
func TestCollectQueueSameFile(t *testing.T) {
	h := hex32("aa", 1)
	const song = `D:\Music\Song.flac`
	over := overlong(`D:\`, maxPathBytes+1)
	k := func(i int) string { return hex32("bb", i) }
	var rows []lyricRow
	for i := 1; i <= 6; i++ {
		rows = append(rows, lyricRow{k(i), strings.Repeat("x", i) + ".krc", "", int64(i)})
	}
	others := func() []*fakeTable {
		return []*fakeTable{
			playedTable("LocalFile",
				playedRow{h, k(1), `d:\music\SONG.FLAC`, kgdb.Value{}},
				playedRow{h, k(2), "  " + song + " ", kgdb.Value{}},
				playedRow{h, k(3), "", kgdb.Value{}},
				playedRow{h, k(4), over, kgdb.Value{}},
				playedRow{h, k(5), `D:\Other\Song.flac`, kgdb.Value{}},
			),
			songTable("RecentlyPlayedItem", songRow{h, k(6)}),
			lyricInfo(rows...),
		}
	}
	queue := func(path string) *fakeTable {
		return playedTable("PlayQueueAssistItem_0", playedRow{h, "", path, integer(5)})
	}
	wantCandidates(t, mustCollect(t, newDB(append([]*fakeTable{queue(song)}, others()...)...), h),
		[]candidate{{"xxxxx.krc", SourceAssociation, 5}})

	noPathCol := &fakeTable{name: "PlayQueueAssistItem_0", cols: []string{"LastPlayTime", "LyricKey", "MusicFileHash"},
		rows: [][]kgdb.Value{{integer(5), text(""), text(h)}}}
	for name, q := range map[string]*fakeTable{"P 的路径为空": queue(""), "P 的路径超长": queue(over), "队列表缺列": noPathCol} {
		t.Run(name, func(t *testing.T) {
			wantCandidates(t, mustCollect(t, newDB(append([]*fakeTable{q}, others()...)...), h), nil)
		})
	}
}

// P 的认定：LastPlayTime 并列时取先扫到的那一行；为 0 或负数的行不是 P；PlayQueueBackupItems_N 不算队列，
// 其行 LastPlayTime 再大也不是 P。
func TestCollectQueuePlayingRowChoice(t *testing.T) {
	h, k1, kb := hex32("aa", 1), hex32("bb", 1), hex32("bb", 2)
	const song = `D:\Music\Song.flac`
	li := lyricInfo(lyricRow{k1, `D:\k1.krc`, "", 1}, lyricRow{kb, `D:\backup.krc`, "", 9})
	withKey, without := playedRow{h, k1, song, integer(7)}, playedRow{h, "", song, integer(7)}

	db := newDB(playedTable("PlayQueueAssistItem_0", withKey, without), li)
	wantCandidates(t, mustCollect(t, db, h), []candidate{{`D:\k1.krc`, SourceAssociation, 1}})
	// 并列时先扫到不带关联的那行：它是 P，同一文件的 k1 不用。
	db = newDB(playedTable("PlayQueueAssistItem_0", without, withKey), li)
	wantCandidates(t, mustCollect(t, db, h), nil)

	// LastPlayTime 为 0、负数的不带关联的行不是 P：没有 P，k1 照常收。
	for _, v := range []int64{0, -5} {
		db = newDB(playedTable("PlayQueueAssistItem_0", playedRow{h, "", song, integer(v)}),
			playedTable("LocalFile", playedRow{h, k1, song, kgdb.Value{}}), li)
		wantCandidates(t, mustCollect(t, db, h), []candidate{{`D:\k1.krc`, SourceAssociation, 1}})
	}

	// 备份表的行 LastPlayTime 远大于队列行，也不是 P：P 是队列里带 k1 的行，只用 k1。
	db = newDB(
		playedTable("PlayQueueBackupItems_10062", playedRow{h, kb, song, lastPlay(clock(23, 59, 59, 0))}),
		playedTable("PlayQueueAssistItem_0", playedRow{h, k1, song, lastPlay(clock(0, 0, 1, 0))}),
		li,
	)
	wantCandidates(t, mustCollect(t, db, h), []candidate{{`D:\k1.krc`, SourceAssociation, 1}})
	// 只有备份表：没有 P，两者都收、按 UpdateTime。
	db = newDB(playedTable("PlayQueueBackupItems_10062", playedRow{h, kb, song, lastPlay(clock(23, 59, 59, 0))}),
		playedTable("LocalFile", playedRow{h, k1, song, kgdb.Value{}}), li)
	wantCandidates(t, mustCollect(t, db, h), []candidate{
		{`D:\backup.krc`, SourceAssociation, 9},
		{`D:\k1.krc`, SourceAssociation, 1},
	})
}

// 表名不分大小写，以 PlayQueueAssistItem 开头即算队列（P 带 kq，只用它）；不以它开头的不算（没有 P，全收、按 UpdateTime）。
func TestCollectQueueTableName(t *testing.T) {
	h, kq, kl := hex32("aa", 1), hex32("bb", 1), hex32("bb", 2)
	const song = `D:\Music\Song.flac`
	queued := []candidate{{`D:\queue.krc`, SourceAssociation, 1}}
	notQueued := []candidate{{`D:\local.krc`, SourceAssociation, 9}, {`D:\queue.krc`, SourceAssociation, 1}}
	for name, want := range map[string][]candidate{
		"PlayQueueAssistItem_0":   queued,
		"playqueueassistitem_0":   queued,
		"PLAYQUEUEASSISTITEM_1":   queued,
		"PlayQueueAssistItem":     queued,
		"MyPlayQueueAssistItem_0": notQueued,
		"PlayQueueAssist_0":       notQueued,
	} {
		t.Run(name, func(t *testing.T) {
			db := newDB(
				playedTable("LocalFile", playedRow{h, kl, song, kgdb.Value{}}),
				playedTable(name, playedRow{h, kq, song, integer(5)}),
				lyricInfo(lyricRow{kl, `D:\local.krc`, "", 9}, lyricRow{kq, `D:\queue.krc`, "", 1}),
			)
			wantCandidates(t, mustCollect(t, db, h), want)
		})
	}
}

// 队列表没有 LastPlayTime 列、或该列全为 NULL：没有 P，全部相符行的键都收，按 UpdateTime 降序（与旧规则相同），
// 路径相同也照收。
func TestCollectQueueNoLastPlayTime(t *testing.T) {
	h, q1, q2, n := hex32("aa", 1), hex32("bb", 1), hex32("bb", 2), hex32("bb", 3)
	const song = `D:\Music\Song.flac`
	li := lyricInfo(lyricRow{n, `D:\n.krc`, "", 900}, lyricRow{q1, `D:\q1.krc`, "", 100}, lyricRow{q2, `D:\q2.krc`, "", 200})
	want := []candidate{
		{`D:\n.krc`, SourceAssociation, 900},
		{`D:\q2.krc`, SourceAssociation, 200},
		{`D:\q1.krc`, SourceAssociation, 100},
	}
	for name, queue := range map[string]*fakeTable{
		"缺列":   songTable("PlayQueueAssistItem_0", songRow{h, q1}, songRow{h, q2}),
		"NULL": playedTable("PlayQueueAssistItem_0", playedRow{h, q1, song, kgdb.Value{}}, playedRow{h, q2, song, kgdb.Value{}}),
	} {
		t.Run(name, func(t *testing.T) {
			db := newDB(playedTable("LocalFile", playedRow{h, n, song, kgdb.Value{}}), queue, li)
			wantCandidates(t, mustCollect(t, db, h), want)
		})
	}
}

// 这首歌不在队列表里（队列表只有别的 hash 的行，哪怕键与路径相同、LastPlayTime 很大）：没有 P，与旧规则相同。
func TestCollectQueueNotInQueue(t *testing.T) {
	h, other, k1, k2 := hex32("aa", 1), hex32("aa", 2), hex32("bb", 1), hex32("bb", 2)
	const song = `D:\Music\Song.flac`
	db := newDB(
		playedTable("LocalFile", playedRow{h, k1, song, kgdb.Value{}}),
		playedTable("RecentlyPlayedItem", playedRow{h, k2, song, integer(1)}),
		playedTable("PlayQueueAssistItem_0", playedRow{other, k1, song, integer(999)}, playedRow{other, "", song, integer(1000)}),
		lyricInfo(lyricRow{k1, `D:\k1.krc`, "", 1}, lyricRow{k2, `D:\k2.krc`, "", 9}),
	)
	wantCandidates(t, mustCollect(t, db, h), []candidate{
		{`D:\k2.krc`, SourceAssociation, 9},
		{`D:\k1.krc`, SourceAssociation, 1},
	})
}
