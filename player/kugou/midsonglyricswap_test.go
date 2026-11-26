package kugou

// 本文件只测播放中换词（#205，设计 §6.7「播放中」；#250 起稳定播放期事件驱动即时消费读取者结果）：收到当前这首的歌词、
// 且内容（套 offset 之前的指纹）与当前歌词不同，才以此刻的 LivePosition 发一条 all_lyrics、行号置为未定（随后立即按新
// 歌词发当前行）；上一首的、内容相同的、不是歌词的结果都不发事件（关联没了保持当前歌词到下一首，设计 §1）。暂停中换词
// 的位置是暂停时冻结的位置。夹具见 fakelocal_test.go；轮询间隔 1 s，换歌拍在 0 s、等满 300 ms 后在线取词，此后
// 各拍在 1.3 s、2.3 s……
//
// 变异自证（工作树的隔离副本里逐条注入、跑完即还原，实测）：
//   - 换词不重置行号：TimeoutFallsBack、SwitchWhilePaused、SameLyricsNoEvent/换上之后再收到同一份；
//   - 播放中不看 hash：StaleHashDropped；
//   - 换歌拍的指纹在套 offset 之后取：SameLyricsNoEvent/与在线结果相同；
//   - 换词之后不更新指纹：SameLyricsNoEvent/换上之后再收到同一份；
//   - 在线取不到歌词时不重置指纹：LocalAfterNoLyrics；
//   - 换词的位置不看暂停（按在播推算）：SwitchWhilePaused。
//
// 换歌拍同样取一条（去掉「换歌拍不取」）全部用例仍绿：假读取者的结果都不在换歌拍那一刻到达，这条只靠人守。

import (
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/assoc"
)

// 300 ms 内没有结果：在线取词；1.5 s 送来不同的歌词，在 1.3 s（读数 41 s）后的等待期即时收下，位置为 LivePosition
// 41.20 s，发一条 all_lyrics，行号重置后按新歌词立即再发当前行（行号与换词前相同）。
func TestSessionLocalTimeoutFallsBack(t *testing.T) {
	run := runLocal(t, 1000, map[string][]reply{"A": {{1500 * time.Millisecond, found("A", lyricSet("本地"))}}},
		twice(read("A", "playing", 40, 200)),
		once(read("A", "playing", 41, 200)),
		once(read("A", "playing", 42, 200)),
		once(read("A", "playing", 43, 200)),
	)
	if len(run.fetched) != 1 || run.fetchAt[0] != localWait {
		t.Fatalf("在线取词 %q 于 %v，want 一次、于 %v", run.fetched, run.fetchAt, localWait)
	}
	checkLyricEvents(t, lyricEvents(run.events), []string{
		"all_lyrics@40.00:在线一/在线二", "lyric_update#1:在线二",
		"all_lyrics@41.20:本地一/本地二", "lyric_update#1:本地二",
	})
	for _, e := range run.events {
		if e.Type == player.EventPlaybackResume || e.Type == player.EventPlaybackPause {
			t.Fatalf("换词不应发 %s", e.Type)
		}
	}
}

// 播放中收到上一首 hash 的歌词（换到 B 之后、B 的换歌拍等完才到）：不发事件。B 走到 1 s（行匹配用的时刻 0.5 s）时
// 照常发第 0 行，那是行匹配，不是换词。
func TestSessionStaleHashDropped(t *testing.T) {
	run := runLocal(t, 1000, map[string][]reply{"A": {{3 * time.Second, found("A", lyricSet("本地"))}}},
		twice(read("A", "playing", 40, 200)),
		once(read("A", "playing", 41, 200)),
		twice(read("B", "playing", 0, 200)),
		once(read("B", "playing", 1, 200)),
		once(read("B", "playing", 2, 200)),
	)
	checkLyricEvents(t, lyricEvents(run.events), []string{
		"all_lyrics@40.00:在线一/在线二", "lyric_update#1:在线二",
		"all_lyrics@0.00:在线一/在线二", "lyric_update#0:在线一",
	})
}

// 播放中收到与在线结果相同的行（逐字带 offset，指纹在套 offset 之前取才相同）：不发事件。换上本地歌词之后再收到
// 同一份（读取者待重试时每 5 s 重发）：同样不发。
func TestSessionSameLyricsNoEvent(t *testing.T) {
	t.Run("与在线结果相同", func(t *testing.T) {
		run := runLocal(t, 1000, map[string][]reply{"A": {{1500 * time.Millisecond, found("A", lyricSet("在线"))}}},
			twice(read("A", "playing", 40, 200)),
			once(read("A", "playing", 41, 200)),
			once(read("A", "playing", 42, 200)),
		)
		checkLyricEvents(t, lyricEvents(run.events), []string{"all_lyrics@40.00:在线一/在线二", "lyric_update#1:在线二"})
	})
	t.Run("换上之后再收到同一份", func(t *testing.T) {
		run := runLocal(t, 1000, map[string][]reply{"A": {
			{1500 * time.Millisecond, found("A", lyricSet("本地"))},
			{2500 * time.Millisecond, found("A", lyricSet("本地"))},
		}},
			twice(read("A", "playing", 40, 200)),
			once(read("A", "playing", 41, 200)),
			once(read("A", "playing", 42, 200)),
			once(read("A", "playing", 43, 200)),
		)
		checkLyricEvents(t, lyricEvents(run.events), []string{
			"all_lyrics@40.00:在线一/在线二", "lyric_update#1:在线二",
			"all_lyrics@41.20:本地一/本地二", "lyric_update#1:本地二",
		})
	})
}

// 这首在线取不到歌词（上一首有）：酷狗稍后给出的歌词即使与上一首的相同，也照样换上——没有歌词时当前歌词的指纹
// 不能还是上一首的。
func TestSessionLocalAfterNoLyrics(t *testing.T) {
	run := runLocal(t, 1000, map[string][]reply{"X": {{1500 * time.Millisecond, found("X", lyricSet("在线"))}}},
		twice(read("A", "playing", 40, 200)),
		twice(read("X", "playing", 40, 200)),
		once(read("X", "playing", 41, 200)),
		once(read("X", "playing", 42, 200)),
	)
	checkLyricEvents(t, lyricEvents(run.events), []string{
		"all_lyrics@40.00:在线一/在线二", "lyric_update#1:在线二",
		"all_lyrics@40.00:", "lyric_update#-1:",
		"all_lyrics@41.20:在线一/在线二", "lyric_update#1:在线二",
	})
}

// 反例：播放中收到的不是歌词（没有、暂时失败、未确认）：不发事件，当前歌词保持到下一首（设计 §1：关联没了不重新
// 在线取词），不再在线取词。
func TestSessionMidSongNonLyricResultsNoEvent(t *testing.T) {
	for _, k := range []assoc.Kind{assoc.KindNone, assoc.KindTransient, assoc.KindNotConfirmed} {
		t.Run(kindName(k), func(t *testing.T) {
			run := runLocal(t, 1000, map[string][]reply{"A": {{1500 * time.Millisecond, assoc.Result{Hash: "A", Kind: k}}}},
				twice(read("A", "playing", 40, 200)),
				once(read("A", "playing", 41, 200)),
				once(read("A", "playing", 42, 200)),
			)
			if len(run.fetched) != 1 {
				t.Fatalf("在线取词 %q，want 只在换歌拍一次", run.fetched)
			}
			checkLyricEvents(t, lyricEvents(run.events), []string{"all_lyrics@40.00:在线一/在线二", "lyric_update#1:在线二"})
		})
	}
}

func kindName(k assoc.Kind) string {
	return map[assoc.Kind]string{assoc.KindNone: "没有", assoc.KindTransient: "暂时失败", assoc.KindNotConfirmed: "未确认"}[k]
}

// 暂停中收到不同的歌词：all_lyrics 的位置是暂停时冻结的 40 s；假时钟再走 2 s、又收到一份不同的歌词，位置仍是 40 s。
// 不发 playback_pause / playback_resume。
func TestSessionSwitchWhilePaused(t *testing.T) {
	run := runLocal(t, 1000, map[string][]reply{"A": {
		{1500 * time.Millisecond, found("A", lyricSet("关联"))},
		{3500 * time.Millisecond, found("A", lyricSet("缓存"))},
	}},
		twice(read("A", "paused", 40, 200)),
		once(read("A", "paused", 40, 200)),
		once(read("A", "paused", 40, 200)),
		once(read("A", "paused", 40, 200)),
		once(read("A", "paused", 40, 200)),
	)
	checkLyricEvents(t, lyricEvents(run.events), []string{
		"all_lyrics@40.00:在线一/在线二", "lyric_update#1:在线二",
		"all_lyrics@40.00:关联一/关联二", "lyric_update#1:关联二",
		"all_lyrics@40.00:缓存一/缓存二", "lyric_update#1:缓存二",
	})
	for _, e := range run.events {
		if e.Type == player.EventPlaybackResume || e.Type == player.EventPlaybackPause {
			t.Fatalf("暂停中换词不应发 %s", e.Type)
		}
	}
}
