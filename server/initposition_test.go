package server

// 本文件只测一件事：连接补发（buildInitEvents）与 HTTP all_lyrics / lyric_update 快照给出的
// position/progress 是发送时刻的位置（issue #63 第 3 条）。
//
// 缓存只在采样事件（lyric_update、all_lyrics、playback_pause/resume）到达时写入。原样补发会落后
// 「距上次采样的时长」，无歌词歌曲可落后整首；前端拿补发的 position 校准时钟，连上那一刻就跳错位置。
//
// 手法：不真实等待。backdate 把缓存的采样时刻往前挪 d，等价于自采样起已经过了 d。position 的断言
// 一律写成接受式区间 lo <= got && got <= hi（AGENTS.md §3.7：NaN 过不了），上界留 slack 吸收测试
// 自身耗时；钳制与非 playing 的用例取精确值（lo == hi）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

const livePlayer = "wesing"

// slack 是 backdate 到取快照之间测试自身耗时的上限（秒），远小于各用例的经过时长。
const slack = 0.5

// backdate 把缓存的采样时刻往前挪 d，模拟自上次采样起经过了 d。
// 只挪、不覆盖：被测代码若漏写采样时刻，挪的就是更早那次采样，断言照样会红。
func backdate(s *Server, d time.Duration) {
	s.mu.Lock()
	ps := s.playerStates[livePlayer]
	ps.PositionAt = ps.PositionAt.Add(-d)
	s.mu.Unlock()
}

// posEvt 构造一条带位置的采样事件：lyric_update 或 playback_pause / playback_resume。
func posEvt(typ string, pos, dur float32) player.Event {
	prog := player.ClampProgress(pos, dur)
	var data interface{} = &player.PlaybackTimeInfo{Position: pos, Progress: prog}
	if typ == player.EventLyricUpdate {
		data = &player.LyricUpdate{Index: 3, Text: "某行", Position: pos, Progress: prog}
	}
	return player.Event{PlayerName: livePlayer, Type: typ, Data: data}
}

// seedLive 置状态、灌入一首时长 dur 的歌（切歌时 position=0），再灌一条位置为 pos 的 lyric_update。
func seedLive(s *Server, status string, dur, pos float32) {
	s.UpdatePlayerState(statusEvt(livePlayer, status))
	s.UpdatePlayerState(player.Event{PlayerName: livePlayer, Type: player.EventAllLyrics, Data: &player.AllLyricsData{
		Title: "某歌 - 某人", Duration: dur, Count: 1, Lyrics: []player.LyricLine{{Index: 0, Text: "一行"}},
	}})
	s.UpdatePlayerState(posEvt(player.EventLyricUpdate, pos, dur))
}

// checkProgress 断言 progress 与 position 自洽：duration>0 时 ≈ position/duration，否则为 0。
func checkProgress(t *testing.T, step string, pos, prog, dur float32) {
	t.Helper()
	if dur <= 0 {
		if prog != 0 {
			t.Errorf("%s：duration=0 时 progress 应为 0，实得 %v", step, prog)
		}
		return
	}
	if want := pos / dur; !(prog >= want-1e-4 && prog <= want+1e-4) {
		t.Errorf("%s：progress = %v，与 position/duration = %v 不一致", step, prog, want)
	}
}

// expectInit 从同一次 buildInitEvents 取补发的 all_lyrics 与 lyric_update，断言：二者 position/progress
// 相同；position 落在 [lo,hi]；progress 与 position 自洽；整组事件可序列化（无 NaN/Inf）。
func expectInit(t *testing.T, s *Server, step string, lo, hi float32) {
	t.Helper()
	evs := s.buildInitEvents(livePlayer, true)
	if _, err := json.Marshal(evs); err != nil {
		t.Fatalf("%s：补发事件无法序列化（NaN/Inf 进了载荷）: %v", step, err)
	}
	var al *AllLyrics
	var lu *LyricUpdate
	for _, e := range evs {
		switch d := e.Data.(type) {
		case *AllLyrics:
			al = d
		case *LyricUpdate:
			lu = d
		}
	}
	if al == nil || lu == nil {
		t.Fatalf("%s：补发缺 all_lyrics 或 lyric_update", step)
	}
	if al.Position != lu.Position || al.Progress != lu.Progress {
		t.Errorf("%s：all_lyrics (%v, %v) 与 lyric_update (%v, %v) 的 position/progress 不一致——"+
			"前端用 lyric_update 校正时钟，后到的那条会把时钟拉偏", step, al.Position, al.Progress, lu.Position, lu.Progress)
	}
	if !(al.Position >= lo && al.Position <= hi) {
		t.Errorf("%s：position = %v，期望在 [%v, %v]", step, al.Position, lo, hi)
	}
	checkProgress(t, step, al.Position, al.Progress, al.Duration)
}

// TestInitPositionLive 钉住外推规则：playing 按经过时间外推、钳到 duration（duration=0 不钳上界、
// progress 为 0）；其余状态给最后记录的值。
func TestInitPositionLive(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		dur     float32
		pos     float32       // 最后一次采样的位置
		elapsed time.Duration // 自采样起经过的时长
		lo, hi  float32
	}{
		{"playing 按经过时间外推", "playing", testDuration, 100, 7 * time.Second, 107, 107 + slack},
		{"playing 钳到 duration", "playing", 100, 98, 10 * time.Second, 100, 100},
		{"duration 为 0 不钳上界", "playing", 0, 50, 10 * time.Second, 60, 60 + slack},
		{"paused 给最后记录的值", "paused", testDuration, 30, 10 * time.Second, 30, 30},
		{"standby 给最后记录的值", "standby", testDuration, 30, 10 * time.Second, 30, 30},
		{"waiting_song 给最后记录的值", "waiting_song", testDuration, 30, 10 * time.Second, 30, 30},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewServer([]string{livePlayer})
			seedLive(s, c.status, c.dur, c.pos)
			backdate(s, c.elapsed)
			expectInit(t, s, c.name, c.lo, c.hi)
		})
	}
}

// TestPlaybackEventsSamplePosition 钉住 playback_pause / playback_resume 写入缓存位置、进度与采样时刻。
// 旧采样先挪到 100s 前：事件若漏写位置或采样时刻，结果都会远离期望。
func TestPlaybackEventsSamplePosition(t *testing.T) {
	cases := []struct {
		name   string
		status string
		typ    string
		lo, hi float32
	}{
		{"暂停中收到 playback_pause", "paused", player.EventPlaybackPause, 42.5, 42.5},
		{"播放中收到 playback_resume（seek）后再播 3s", "playing", player.EventPlaybackResume, 45.5, 45.5 + slack},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewServer([]string{livePlayer})
			seedLive(s, c.status, testDuration, 10)
			backdate(s, 100*time.Second)
			s.UpdatePlayerState(posEvt(c.typ, 42.5, testDuration))
			backdate(s, 3*time.Second)
			expectInit(t, s, c.name, c.lo, c.hi)
		})
	}
}

// TestStatusSwitchFreezesAndRestartsClock 按 wesing 的顺序（先状态、后 pause/resume）走一遍暂停与恢复：
// 状态转 paused 时冻结到此刻，pause 到达后以其位置为准，状态转 playing 时从冻结位置重启时钟。
func TestStatusSwitchFreezesAndRestartsClock(t *testing.T) {
	s := NewServer([]string{livePlayer})
	seedLive(s, "playing", testDuration, 10)
	backdate(s, 5*time.Second) // 播了 5s，此刻约 15s

	s.UpdatePlayerState(statusEvt(livePlayer, "paused"))
	backdate(s, 30*time.Second) // 状态已转 paused、pause 事件尚未到达
	expectInit(t, s, "状态转 paused 后", 15, 15+slack)

	s.UpdatePlayerState(posEvt(player.EventPlaybackPause, 15.2, testDuration))
	backdate(s, 30*time.Second)
	expectInit(t, s, "pause 到达后暂停 30s", 15.2, 15.2)

	// 状态转 playing 时重启时钟并清除 pause 标记：resume 未到也从 15.2 起走。
	s.UpdatePlayerState(statusEvt(livePlayer, "playing"))
	backdate(s, 2*time.Second)
	expectInit(t, s, "状态转 playing、resume 尚未到达时再播 2s", 17.2, 17.2+slack)

	s.UpdatePlayerState(posEvt(player.EventPlaybackResume, 17.25, testDuration))
	backdate(s, 2*time.Second)
	expectInit(t, s, "resume 后再播 2s", 19.25, 19.25+slack)
}

// TestPlaybackEventBeforeStatus 按 kugou / sodamusic / qqmusic / applemusic 的暂停顺序（先 pause、后状态）走一遍：
// pause 的位置即暂停点，状态转 paused 之前的补发与离开 playing 时的冻结都不再外推。
func TestPlaybackEventBeforeStatus(t *testing.T) {
	s := NewServer([]string{livePlayer})
	seedLive(s, "playing", testDuration, 10)
	backdate(s, 5*time.Second)

	s.UpdatePlayerState(posEvt(player.EventPlaybackPause, 15, testDuration))
	backdate(s, 2*time.Second) // pause 已到、状态尚为 playing
	expectInit(t, s, "pause 到达后、状态转 paused 之前", 15, 15)

	s.UpdatePlayerState(statusEvt(livePlayer, "paused"))
	backdate(s, 30*time.Second)
	expectInit(t, s, "暂停 30s 后", 15, 15)

	s.UpdatePlayerState(posEvt(player.EventPlaybackResume, 15, testDuration))
	s.UpdatePlayerState(statusEvt(livePlayer, "playing"))
	backdate(s, 2*time.Second)
	expectInit(t, s, "恢复后再播 2s", 17, 17+slack)
}

// TestNonPauseSampleClearsPauseMark 钉住 pause 之后的非 pause 采样会清除 pause 标记：状态仍为
// playing 时，其后的补发从新采样照常外推。clear_song_data 处的清除无法从输出观察（其后任何采样
// 都会重写标记），不在覆盖内。
func TestNonPauseSampleClearsPauseMark(t *testing.T) {
	cases := []struct {
		name string
		evt  player.Event
	}{
		{"lyric_update", posEvt(player.EventLyricUpdate, 20, testDuration)},
		{"playback_resume", posEvt(player.EventPlaybackResume, 20, testDuration)},
		{"all_lyrics", player.Event{PlayerName: livePlayer, Type: player.EventAllLyrics, Data: &player.AllLyricsData{
			Title: "某歌 - 某人", Duration: testDuration, Position: 20, Progress: player.ClampProgress(20, testDuration),
			Count: 1, Lyrics: []player.LyricLine{{Index: 0, Text: "一行"}},
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewServer([]string{livePlayer})
			seedLive(s, "playing", testDuration, 10)
			s.UpdatePlayerState(posEvt(player.EventPlaybackPause, 15, testDuration))
			s.UpdatePlayerState(c.evt)
			backdate(s, 3*time.Second)
			al := initAllLyrics(t, s, livePlayer)
			if !(al.Position >= 23 && al.Position <= 23+slack) {
				t.Errorf("position = %v，期望在 [23, %v]（pause 标记未被清除时停在 20）", al.Position, 23+slack)
			}
			checkProgress(t, c.name, al.Position, al.Progress, al.Duration)
		})
	}
}

// httpData 调一个 HTTP 快照端点，返回其 data 原文。
func httpData(t *testing.T, h http.HandlerFunc) json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/", nil))
	var resp struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, rec.Body.String())
	}
	return resp.Data
}

// TestHTTPSnapshotsLive 钉住 HTTP all_lyrics 与 lyric_update 快照同样外推到响应时刻。
func TestHTTPSnapshotsLive(t *testing.T) {
	cases := []struct {
		name string
		h    func(*Server, string) http.HandlerFunc
	}{
		{"all_lyrics", (*Server).handleAllLyrics},
		{"lyric_update", (*Server).handleLyricUpdate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewServer([]string{livePlayer})
			seedLive(s, "playing", testDuration, 100)
			backdate(s, 7*time.Second)

			var got struct {
				Position float32 `json:"position"`
				Progress float32 `json:"progress"`
			}
			if err := json.Unmarshal(httpData(t, c.h(s, livePlayer)), &got); err != nil {
				t.Fatal(err)
			}
			if !(got.Position >= 107 && got.Position <= 107+slack) {
				t.Errorf("position = %v，期望在 [107, %v]", got.Position, 107+slack)
			}
			checkProgress(t, c.name, got.Position, got.Progress, testDuration)
		})
	}
}

// TestInitLyricUpdateKeepsFieldsAndCache 钉住补发与 HTTP 快照的 lyric_update 除 position/progress 外与
// 缓存逐字段相同，且缓存的 ps.LyricUpdate 本身不被修改（补发只持读锁；写回外推值还会让下次补发重复计时）。
func TestInitLyricUpdateKeepsFieldsAndCache(t *testing.T) {
	s := NewServer([]string{livePlayer})
	s.UpdatePlayerState(statusEvt(livePlayer, "playing"))
	s.UpdatePlayerState(player.Event{PlayerName: livePlayer, Type: player.EventLyricUpdate, Data: &player.LyricUpdate{
		Index: 37, Text: "夜に駆ける", SubText: "向夜晚奔去", RomaText: "yoru ni kakeru",
		Timestamp: 157.22, PlayTime: 156.46, Position: 156.4659, Progress: 0.6297,
		TextDetailed: player.LyricTextDetailed{Timestamp: 156.96, PlayTime: 156.46, Duration: 7.02, Words: []player.LyricTextDetailedWord{
			{Timestamp: 156.96, PlayTime: 156.46, Duration: 0.18, Text: "夜"},
		}},
	}})
	backdate(s, 5*time.Second)

	s.mu.RLock()
	before := *s.playerStates[livePlayer].LyricUpdate
	s.mu.RUnlock()

	sources := []struct {
		name string
		get  func() LyricUpdate
	}{
		{"连接补发", func() LyricUpdate { return *initLyricUpdate(t, s, livePlayer) }},
		{"HTTP 快照", func() LyricUpdate {
			var lu LyricUpdate
			if err := json.Unmarshal(httpData(t, s.handleLyricUpdate(livePlayer)), &lu); err != nil {
				t.Fatal(err)
			}
			return lu
		}},
	}
	for _, src := range sources {
		t.Run(src.name, func(t *testing.T) {
			got := src.get()
			if !(got.Position >= before.Position+5) {
				t.Errorf("position = %v，未外推（缓存 %v，已过 5s）", got.Position, before.Position)
			}
			want := before
			want.Position, want.Progress = got.Position, got.Progress
			if !reflect.DeepEqual(got, want) {
				t.Errorf("除 position/progress 外与缓存不同：\n got  %+v\n want %+v", got, want)
			}
			s.mu.RLock()
			after := *s.playerStates[livePlayer].LyricUpdate
			s.mu.RUnlock()
			if !reflect.DeepEqual(after, before) {
				t.Errorf("缓存的 LyricUpdate 被原地修改：\n before %+v\n after  %+v", before, after)
			}
		})
	}
}
