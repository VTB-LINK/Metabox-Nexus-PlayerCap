package assoc

// 本文件只测结果的「本地文件」标记（设计 §6.5 第 4 步）：有正在播放的队列行 P、且 P 的 MusicFilePath 为绝对路径
// （盘符或 UNC，按 Windows 规则）时为真；只看 P，不看同一 hash 的其他行。经 collect 与 pick 走完整的解析，
// 另测读取者透传、未走到 pick 的结果为假。
//
// 变异自证（逐条注入、跑完即还原，实测）：
//   - 标记改看各歌曲条目表里任一相符行的路径（不只看 P）：三个「没有 P」例与两个「P 为在线形式、同 hash …为绝对路径」
//     例红；
//   - 改以「第二个字符是冒号」判绝对路径：「UNC」「盘符相对」两例红；
//   - pick 只在取到歌词时带标记：TestCollectLocalFile 带标记的各例（其 pick 没有候选、结果为 KindNone）、
//     TestPickCarriesLocalFile 的两个「没有」例、TestReaderSendsLocalFile/没有 红。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player/kugou/kgdb"
)

func TestCollectLocalFile(t *testing.T) {
	h, k := hex32("aa", 1), hex32("bb", 1)
	const (
		disk   = `D:\Music\歌手 - 歌.flac`
		online = "歌手 - 歌.mp3"
	)
	queue := func(rows ...playedRow) *fakeTable { return playedTable("PlayQueueAssistItem_0", rows...) }
	li := lyricInfo(lyricRow{k, `D:\k.krc`, "", 1})
	cases := []struct {
		name     string
		db       *fakeTables
		hash     string
		local    bool
		playedAt int64
	}{
		{"P 为盘符绝对路径", newDB(queue(playedRow{h, "", disk, integer(5)}), li), h, true, 5},
		{"P 为 UNC 路径", newDB(queue(playedRow{h, "", `\\nas\music\歌.flac`, integer(5)}), li), h, true, 5},
		{"P 为正斜杠的盘符路径", newDB(queue(playedRow{h, "", `D:/Music/歌.flac`, integer(5)}), li), h, true, 5},
		{"P 带关联、为绝对路径", newDB(queue(playedRow{h, k, disk, integer(5)}), li), h, true, 5},
		{"P 两端有空白的绝对路径", newDB(queue(playedRow{h, "", "  " + disk + " ", integer(5)}), li), h, true, 5},
		// 反例
		{"P 为在线形式的文件名", newDB(queue(playedRow{h, "", online, integer(5)}), li), h, false, 5},
		{"P 带关联、为在线形式", newDB(queue(playedRow{h, k, online, integer(5)}), li), h, false, 5},
		{"P 为相对路径", newDB(queue(playedRow{h, "", `Music\歌.flac`, integer(5)}), li), h, false, 5},
		{"P 为根相对路径", newDB(queue(playedRow{h, "", `\Music\歌.flac`, integer(5)}), li), h, false, 5},
		{"P 为盘符相对路径", newDB(queue(playedRow{h, "", `D:Music\歌.flac`, integer(5)}), li), h, false, 5},
		{"P 的路径为空", newDB(queue(playedRow{h, "", "", integer(5)}), li), h, false, 5},
		{"P 的路径超长", newDB(queue(playedRow{h, "", overlong(`D:\`, maxPathBytes+1), integer(5)}), li), h, false, 5},
		{"队列表缺 MusicFilePath 列", newDB(&fakeTable{name: "PlayQueueAssistItem_0",
			cols: []string{"LastPlayTime", "LyricKey", "MusicFileHash"},
			rows: [][]kgdb.Value{{integer(5), text(""), text(h)}}}, li), h, false, 5},
		{"没有 P：队列行 LastPlayTime 为 0", newDB(queue(playedRow{h, "", disk, integer(0)}), li), h, false, 0},
		{"没有 P：只有非队列行带绝对路径", newDB(
			playedTable("LocalFile", playedRow{h, "", disk, integer(9)}),
			playedTable("RecentlyPlayedItem", playedRow{h, "", disk, integer(9)}),
			li), h, false, 0},
		{"没有 P：备份表的行带绝对路径、LastPlayTime 更大", newDB(
			playedTable("PlayQueueBackupItems_10062", playedRow{h, "", disk, integer(99)}),
			queue(playedRow{h, "", online, integer(5)}),
			li), h, false, 5},
		{"P 为在线形式、同 hash 的 LocalFile 行为绝对路径", newDB(
			playedTable("LocalFile", playedRow{h, "", disk, kgdb.Value{}}),
			queue(playedRow{h, "", online, integer(5)}),
			li), h, false, 5},
		{"P 为在线形式、同 hash 较早的队列行为绝对路径", newDB(
			queue(playedRow{h, "", disk, integer(3)}, playedRow{h, "", online, integer(5)}), li), h, false, 5},
		{"P 为在线形式、别的 hash 的队列行为绝对路径且更新", newDB(
			queue(playedRow{h, "", online, integer(5)}, playedRow{hex32("aa", 2), "", disk, integer(9)}), li), h, false, 5},
		// 对照：同样的几行，P 换成绝对路径的那一行即带。
		{"P 为绝对路径、同 hash 较早的队列行为在线形式", newDB(
			queue(playedRow{h, "", online, integer(3)}, playedRow{h, "", disk, integer(5)}), li), h, true, 5},
		{"P 为绝对路径、同 hash 的「最近播放」行为在线形式", newDB(
			playedTable("RecentlyPlayedItem", playedRow{h, "", online, integer(9)}),
			queue(playedRow{h, "", disk, integer(5)}),
			li), h, true, 5},
		{"播放 hash 全 0", newDB(queue(playedRow{zeroKey, "", disk, integer(5)}), li), zeroKey, false, 0},
		{"播放 hash 为空", newDB(queue(playedRow{"", "", disk, integer(5)}), li), "", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			col := mustResolve(t, c.db, c.hash)
			if col.localFile != c.local || col.playedAt != c.playedAt {
				t.Fatalf("localFile %v、playedAt %d，want %v、%d", col.localFile, col.playedAt, c.local, c.playedAt)
			}
			// 没有候选、读不到文件时结果为 KindNone，标记同样带上。
			r := pick(col, `C:\KuGou\Lyric`, (&fakeRead{}).fn)
			if r.LocalFile != c.local || r.PlayedAt != c.playedAt {
				t.Fatalf("结果 %+v，want LocalFile %v、PlayedAt %d", r, c.local, c.playedAt)
			}
		})
	}
}

// pick 把标记原样带进结果：取到歌词（关联或缓存）与没有（不论待不待重试）都带。
func TestPickCarriesLocalFile(t *testing.T) {
	for name, c := range map[string]struct {
		col  collected
		kind Kind
	}{
		"关联":      {collected{cands: []candidate{assocCand}, keyed: true, playedAt: 7, localFile: true}, KindFound},
		"缓存":      {collected{cands: []candidate{cacheCand}, playedAt: 7, localFile: true}, KindFound},
		"没有候选":    {collected{playedAt: 7, localFile: true}, KindNone},
		"候选暂缺、没有": {collected{cands: []candidate{{`D:\missing.lrc`, SourceCache, 1}}, playedAt: 7, localFile: true}, KindNone},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeRead{files: map[string]any{assocCand.Path: testLRC, cachePath: testLRC}}
			r := pick(c.col, okConfirmed.LyricDir, f.fn)
			if r.Kind != c.kind || !r.LocalFile || r.PlayedAt != 7 {
				t.Fatalf("结果 %+v，want Kind %d、LocalFile、PlayedAt 7", r, c.kind)
			}
		})
	}
}

// 读取者发出的结果带上标记，KindNone 也带；没有走到 pick 的暂时失败为假。
func TestReaderSendsLocalFile(t *testing.T) {
	for name, c := range map[string]struct {
		col  collected
		kind Kind
	}{
		"缓存": {collected{cands: []candidate{cacheCand}, playedAt: 300, localFile: true}, KindFound},
		"没有": {collected{playedAt: 300, localFile: true}, KindNone},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeDeps{colFn: func(int, string) (collected, error) { return c.col, nil }}
			runReader(t, f, func(t *testing.T, r *Reader) {
				r.SetHash("A")
				if res := latest(t, r); res.Kind != c.kind || !res.LocalFile || res.PlayedAt != 300 {
					t.Fatalf("结果 %+v，want Kind %d、LocalFile、PlayedAt 300", res, c.kind)
				}
			})
		})
	}
	t.Run("暂时失败", func(t *testing.T) {
		f := &fakeDeps{colFn: func(int, string) (collected, error) {
			return collected{playedAt: 300, localFile: true}, errTransientStub
		}}
		runReader(t, f, func(t *testing.T, r *Reader) {
			r.SetHash("A")
			if res := latest(t, r); res.Kind != KindTransient || res.LocalFile {
				t.Fatalf("结果 %+v，want KindTransient、LocalFile 为假", res)
			}
		})
	})
}
