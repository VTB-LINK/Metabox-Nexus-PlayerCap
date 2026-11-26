package kugou

// 本文件只测播放中关联不退成缓存（#205，设计 §6.7「播放中」）：当前这首最近一次被采纳的本地歌词来自关联时，播放中
// 再收到带「关联未取用」标记、且 P 的 LastPlayTime（PlayedAt）与采纳时相同的缓存歌词不换（关联在同一次播放中失效，
// 酷狗仍显示原来的歌词，按用户决定「2甲」保持到下一首）；不带标记的缓存（第 1 步本就没有关联键）、PlayedAt 不同的缓存
// （当初的关联属于误选的行或已是新的一次播放）照换；换成另一份关联照换；换歌后重新计。夹具见 fakelocal_test.go；
// 轮询间隔 1 s。
//
// 变异自证（逐条注入、跑完即还原，实测）：
//   - 去掉「关联不退成缓存」或标记恒为假：KeepsAssociation/换歌拍取到关联，播放中关联失效只剩缓存、
//     OnlineThenAssociation/带标记的缓存不换、SameContentAssociation、PlayedAt/相同与内容相同两例 变红；
//   - 不看标记（或标记恒为真）：KeepsAssociation/换歌拍取到关联，落盘后是不带标记的缓存、OnlineThenAssociation/不带
//     标记的缓存照换 变红；
//   - 不比 PlayedAt（会话两侧的 PlayedAt 恒为 0 与此等价）：PlayedAt/不同 变红；
//   - 内容相同时不更新 PlayedAt：PlayedAt/内容相同 变红；内容相同时来源与 PlayedAt 都不记：再加 SameContentAssociation；
//   - 换歌拍不记 PlayedAt：PlayedAt/相同 变红；
//   - 换歌拍不清零来源：ResetsOnSongChange 变红；
//   - 关联换关联也不换（只看当前来源）：KeepsAssociation/换成另一份关联 等变红。

import (
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/kugou/assoc"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

func foundAssoc(hash string, lines []klyric.Line) assoc.Result {
	return assoc.Result{Hash: hash, Kind: assoc.KindFound, Lines: lines, Source: assoc.SourceAssociation}
}

// foundSkipped 是带「关联未取用」标记的缓存结果：第 1 步得出了关联键，却没取用上（设计 §6.5 第 4 步）。
func foundSkipped(hash string, lines []klyric.Line) assoc.Result {
	r := found(hash, lines)
	r.AssocSkipped = true
	return r
}

// playedAt 给结果带上 P 的 LastPlayTime（assoc.Result.PlayedAt）。其余夹具的 PlayedAt 都为 0，彼此算同一次播放。
func playedAt(r assoc.Result, at int64) assoc.Result {
	r.PlayedAt = at
	return r
}

// 以 P 的 LastPlayTime 区分同一次播放（设计 §6.7「播放中」）。换歌拍 100 ms 取到关联，此后各拍在 1.1 s、2.1 s……
func TestSessionMidSongPlayedAt(t *testing.T) {
	// astra 指出的组合：换歌拍误选了同一文件的旧队列行为 P（200），采纳其关联；落盘后真正的 P（300）不带关联，
	// 「1甲」取到的其他文件暂缺，退到缓存并带标记。PlayedAt 不同，应换成缓存；之后同样的缓存每 5 s 重发，不再发事件。
	t.Run("PlayedAt 不同，带标记的缓存照换", func(t *testing.T) {
		// 每次都是新切片：会话就地给采纳的行套 offset，读取者每次解析也都给出新切片。
		cache := func() assoc.Result { return playedAt(foundSkipped("A", lyricSet("缓存")), 300) }
		run := runLocal(t, 1000, map[string][]reply{"A": {
			{100 * time.Millisecond, playedAt(foundAssoc("A", lyricSet("关联")), 200)},
			{1500 * time.Millisecond, cache()},
			{6500 * time.Millisecond, cache()},
			{11500 * time.Millisecond, cache()},
		}},
			twice(read("A", "playing", 40, 200)),
			once(read("A", "playing", 41, 200)), once(read("A", "playing", 42, 200)), once(read("A", "playing", 43, 200)),
			once(read("A", "playing", 44, 200)), once(read("A", "playing", 45, 200)), once(read("A", "playing", 46, 200)),
			once(read("A", "playing", 47, 200)), once(read("A", "playing", 48, 200)), once(read("A", "playing", 49, 200)),
			once(read("A", "playing", 50, 200)), once(read("A", "playing", 51, 200)), once(read("A", "playing", 52, 200)),
			once(read("A", "playing", 53, 200)),
		)
		checkLyricEvents(t, lyricEvents(run.events), []string{
			"all_lyrics@40.00:关联一/关联二", "lyric_update#1:关联二",
			"all_lyrics@41.40:缓存一/缓存二", "lyric_update#1:缓存二",
		})
	})
	// 同一次播放：换歌拍采纳的关联与之后带标记的缓存 PlayedAt 都是 300，不换。换歌拍不记 PlayedAt 时本例变红。
	t.Run("PlayedAt 相同，带标记的缓存不换", func(t *testing.T) {
		run := runLocal(t, 1000, map[string][]reply{"A": {
			{100 * time.Millisecond, playedAt(foundAssoc("A", lyricSet("关联")), 300)},
			{1500 * time.Millisecond, playedAt(foundSkipped("A", lyricSet("缓存")), 300)},
		}},
			twice(read("A", "playing", 40, 200)),
			once(read("A", "playing", 41, 200)),
			once(read("A", "playing", 42, 200)),
			once(read("A", "playing", 43, 200)),
		)
		checkLyricEvents(t, lyricEvents(run.events), []string{"all_lyrics@40.00:关联一/关联二", "lyric_update#1:关联二"})
	})
	// 换歌拍在 200 采纳关联；落盘后同一份关联在 300 再来（内容相同、不重发，但记下 300）；2.8 s 带标记的缓存在 300，
	// 于 3.1 s 收到、不换。内容相同时不更新 PlayedAt，本例变红。
	t.Run("内容相同的关联也更新 PlayedAt", func(t *testing.T) {
		run := runLocal(t, 1000, map[string][]reply{"A": {
			{100 * time.Millisecond, playedAt(foundAssoc("A", lyricSet("关联")), 200)},
			{1500 * time.Millisecond, playedAt(foundAssoc("A", lyricSet("关联")), 300)},
			{2800 * time.Millisecond, playedAt(foundSkipped("A", lyricSet("缓存")), 300)},
		}},
			twice(read("A", "playing", 40, 200)),
			once(read("A", "playing", 41, 200)),
			once(read("A", "playing", 42, 200)),
			once(read("A", "playing", 43, 200)),
			once(read("A", "playing", 44, 200)),
		)
		checkLyricEvents(t, lyricEvents(run.events), []string{"all_lyrics@40.00:关联一/关联二", "lyric_update#1:关联二"})
	})
}

// 换歌拍 100 ms 取到关联（不在线取词），此后各拍在 1.1 s、2.1 s……；1.5 s 送来的结果在 1.1 s（读数 41 s）后的等待期即时处理（41.40 s）。
func TestSessionMidSongKeepsAssociation(t *testing.T) {
	t.Run("换歌拍取到关联，播放中关联失效只剩缓存", func(t *testing.T) {
		run := runLocal(t, 1000, map[string][]reply{"A": {
			{100 * time.Millisecond, foundAssoc("A", lyricSet("关联"))},
			{1500 * time.Millisecond, foundSkipped("A", lyricSet("缓存"))},
		}},
			twice(read("A", "playing", 40, 200)),
			once(read("A", "playing", 41, 200)),
			once(read("A", "playing", 42, 200)),
			once(read("A", "playing", 43, 200)),
		)
		if len(run.fetched) != 0 {
			t.Fatalf("在线取词 %q，want 无（换歌拍已取到关联）", run.fetched)
		}
		checkLyricEvents(t, lyricEvents(run.events), []string{"all_lyrics@40.00:关联一/关联二", "lyric_update#1:关联二"})
	})
	t.Run("换成另一份关联", func(t *testing.T) {
		run := runLocal(t, 1000, map[string][]reply{"A": {
			{100 * time.Millisecond, foundAssoc("A", lyricSet("关联"))},
			{1500 * time.Millisecond, foundAssoc("A", lyricSet("另一"))},
		}},
			twice(read("A", "playing", 40, 200)),
			once(read("A", "playing", 41, 200)),
			once(read("A", "playing", 42, 200)),
			once(read("A", "playing", 43, 200)),
		)
		checkLyricEvents(t, lyricEvents(run.events), []string{
			"all_lyrics@40.00:关联一/关联二", "lyric_update#1:关联二",
			"all_lyrics@41.40:另一一/另一二", "lyric_update#1:另一二",
		})
	})
	// 审查给的回归：换歌拍时酷狗尚未落盘，把同一文件另一行带关联的队列行当成了正在播放的行，取到关联；落盘后
	// 真正的那一行不带关联，结果是不带标记的缓存，应当换过去（设计 §8）。
	t.Run("换歌拍取到关联，落盘后是不带标记的缓存", func(t *testing.T) {
		run := runLocal(t, 1000, map[string][]reply{"A": {
			{100 * time.Millisecond, foundAssoc("A", lyricSet("关联"))},
			{1500 * time.Millisecond, found("A", lyricSet("缓存"))},
		}},
			twice(read("A", "playing", 40, 200)),
			once(read("A", "playing", 41, 200)),
			once(read("A", "playing", 42, 200)),
			once(read("A", "playing", 43, 200)),
		)
		checkLyricEvents(t, lyricEvents(run.events), []string{
			"all_lyrics@40.00:关联一/关联二", "lyric_update#1:关联二",
			"all_lyrics@41.40:缓存一/缓存二", "lyric_update#1:缓存二",
		})
	})
}

// 换歌拍等满 300 ms 在线取词，此后各拍在 1.3 s、2.3 s、3.3 s……：1.5 s 的关联在 1.3 s（读数 41 s）后即时换上（41.20 s），
// 2.8 s 的缓存在 2.3 s（读数 42 s）后即时收到（42.50 s）：带标记不换，不带标记照换。
func TestSessionMidSongOnlineThenAssociation(t *testing.T) {
	for name, c := range map[string]struct {
		cache assoc.Result
		want  []string
	}{
		"带标记的缓存不换":  {foundSkipped("A", lyricSet("缓存")), nil},
		"不带标记的缓存照换": {found("A", lyricSet("缓存")), []string{"all_lyrics@42.50:缓存一/缓存二", "lyric_update#1:缓存二"}},
	} {
		t.Run(name, func(t *testing.T) {
			run := runLocal(t, 1000, map[string][]reply{"A": {
				{1500 * time.Millisecond, foundAssoc("A", lyricSet("关联"))},
				{2800 * time.Millisecond, c.cache},
			}},
				twice(read("A", "playing", 40, 200)),
				once(read("A", "playing", 41, 200)),
				once(read("A", "playing", 42, 200)),
				once(read("A", "playing", 43, 200)),
				once(read("A", "playing", 44, 200)),
			)
			checkLyricEvents(t, lyricEvents(run.events), append([]string{
				"all_lyrics@40.00:在线一/在线二", "lyric_update#1:在线二",
				"all_lyrics@41.20:关联一/关联二", "lyric_update#1:关联二",
			}, c.want...))
		})
	}
}

// 关联与在线取到的内容相同（不重发），来源仍记为关联：之后带标记的缓存不换。
func TestSessionMidSongSameContentAssociation(t *testing.T) {
	run := runLocal(t, 1000, map[string][]reply{"A": {
		{1500 * time.Millisecond, foundAssoc("A", lyricSet("在线"))},
		{2800 * time.Millisecond, foundSkipped("A", lyricSet("缓存"))},
	}},
		twice(read("A", "playing", 40, 200)),
		once(read("A", "playing", 41, 200)),
		once(read("A", "playing", 42, 200)),
		once(read("A", "playing", 43, 200)),
		once(read("A", "playing", 44, 200)),
	)
	checkLyricEvents(t, lyricEvents(run.events), []string{"all_lyrics@40.00:在线一/在线二", "lyric_update#1:在线二"})
}

// 上一首是关联，换到 B 之后本地未及时给出（在线取词），播放中 B 的缓存照换：来源在换歌拍清零。B 的缓存带标记，
// 只有清零才会换。A 的换歌拍 100 ms 取到关联，1.1 s 一拍；B 于 2.1 s 换歌、2.4 s 在线取词，此后 3.4 s、4.4 s……；
// B 的缓存于 3.6 s 送到并即时生效（1.20 s）。
func TestSessionMidSongResetsOnSongChange(t *testing.T) {
	run := runLocal(t, 1000, map[string][]reply{
		"A": {{100 * time.Millisecond, foundAssoc("A", lyricSet("关联"))}},
		"B": {{1500 * time.Millisecond, foundSkipped("B", lyricSet("缓存"))}},
	},
		twice(read("A", "playing", 40, 200)),
		once(read("A", "playing", 41, 200)),
		twice(read("B", "playing", 0, 200)),
		once(read("B", "playing", 1, 200)),
		once(read("B", "playing", 2, 200)),
		once(read("B", "playing", 3, 200)),
	)
	checkLyricEvents(t, lyricEvents(run.events), []string{
		"all_lyrics@40.00:关联一/关联二", "lyric_update#1:关联二",
		"all_lyrics@0.00:在线一/在线二", "lyric_update#0:在线一",
		"all_lyrics@1.20:缓存一/缓存二", "lyric_update#0:缓存一",
	})
}
