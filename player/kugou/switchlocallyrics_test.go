package kugou

// 本文件只测换歌拍取本地歌词（#205，设计 §6.7「换歌拍」）：有读取者时写入新 hash，至多等 localWait 取这一首的
// 结果；取到歌词即用、不在线取词；没有、暂时失败、未确认与超时走在线取词；停用之后本会话不再写入 hash；
// 读取者疑似阻塞时不等；没有歌（hash 为空）不写入；等待中收到停止即结束会话。夹具见 fakelocal_test.go。
//
// 变异自证（工作树的隔离副本里逐条注入、跑完即还原，实测）记在各用例的注释里。

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/assoc"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// 换歌拍等本地结果期间（读取者不回结果）收到 Stop：会话当即返回，不在线取词、不发这一首的 all_lyrics，读取者的 ctx
// 已取消。在线取词单次至多 8 s 且不受 Stop 中断，照常取词的话停止要等它与本拍的事件发完。
//
// 变异自证：收到停止后照常往下走（修复前的写法）即红；该路径去掉 localCancel()，`go vet` 报 lostcancel，本例同红。
func TestSessionStopWhileAwaitingLocal(t *testing.T) {
	origLyrics := fetchLyrics
	var fetched []string
	fetchLyrics = func(hash string, _ int, _, _ string) ([]klyric.Line, error) {
		fetched = append(fetched, hash)
		return lyricSet("在线"), nil
	}
	defer func() { fetchLyrics = origLyrics }()

	synctest.Test(t, func(t *testing.T) {
		p := New(localOffsetMs, 0)
		f := newFakeLocal(nil)
		go func() {
			time.Sleep(100 * time.Millisecond)
			p.Stop()
		}()
		p.runSession(&scriptedSource{reads: twice(read("A", "playing", 40, 200))}, f.starter())
		if len(fetched) != 0 {
			t.Errorf("收到停止后不应在线取词，实际调用 %q", fetched)
		}
		if f.ctx == nil || f.ctx.Err() == nil {
			t.Errorf("会话返回时读取者的 ctx 应已取消（ctx=%v）", f.ctx)
		}
		if !slices.Equal(f.set, []string{"A"}) {
			t.Errorf("SetHash %q，want [A]", f.set)
		}
		synctest.Wait()
		if got := lyricEvents(drainAll(p)); len(got) != 0 {
			t.Errorf("收到停止后不应发歌词事件，实得 %q", got)
		}
	})
}

// 换歌后 100 ms 得到本地歌词：不在线取词，all_lyrics 为本地的行，逐字照样套 offset（0.5 s 的字播出于 1.0 s）。
//
// 变异自证：得到本地歌词仍在线取词，或本地歌词不套 offset，均红。
func TestSessionLocalFoundSkipsOnline(t *testing.T) {
	run := runLocal(t, 0, map[string][]reply{"A": {{100 * time.Millisecond, found("A", lyricSet("本地"))}}},
		twice(read("A", "playing", 40, 200)), once(read("A", "playing", 40, 200)))
	if len(run.fetched) != 0 {
		t.Fatalf("本地取到歌词时不应在线取词，实际调用 %q", run.fetched)
	}
	if !slices.Equal(run.set, []string{"A"}) {
		t.Fatalf("SetHash %q，want [A]", run.set)
	}
	checkLyricEvents(t, lyricEvents(run.events), []string{"all_lyrics@40.00:本地一/本地二", "lyric_update#1:本地二"})
	for _, e := range run.events {
		if d, ok := e.Data.(*player.AllLyricsData); ok {
			if w := d.Lyrics[0].TextDetailed.Words[1]; w.PlayTime != 1.0 {
				t.Fatalf("本地歌词的逐字应套 offset：时间戳 %.2f 的字播出于 %.2f，want 1.00", w.Timestamp, w.PlayTime)
			}
		}
	}
}

// 得到这一首的没有、暂时失败、未确认，或 KindFound 却没有行：不再等，当即在线取词（100 ms，不等满 300 ms）。
// 反例：其他 hash 的结果丢弃，继续等到超时（300 ms）再在线取词。
//
// 变异自证：同 hash 的非歌词结果仍等到超时，前四个子例红；其他 hash 的结果即结束等待，末一个子例红。
func TestSessionLocalNoneTransientNotConfirmed(t *testing.T) {
	cases := []struct {
		name string
		res  assoc.Result
		wait time.Duration
	}{
		{"没有", assoc.Result{Hash: "A", Kind: assoc.KindNone, Retry: true}, 100 * time.Millisecond},
		{"暂时失败", assoc.Result{Hash: "A", Kind: assoc.KindTransient}, 100 * time.Millisecond},
		{"未确认", assoc.Result{Hash: "A", Kind: assoc.KindNotConfirmed}, 100 * time.Millisecond},
		{"KindFound 而没有行", found("A", nil), 100 * time.Millisecond},
		{"其他 hash 的歌词丢弃", found("Z", lyricSet("本地")), localWait},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := runLocal(t, 0, map[string][]reply{"A": {{100 * time.Millisecond, c.res}}},
				twice(read("A", "playing", 40, 200)))
			if !slices.Equal(run.fetched, []string{"A"}) || run.fetchAt[0]-run.setAt[0] != c.wait {
				t.Fatalf("在线取词 %q、距写入 hash %v，want [A]、%v", run.fetched, run.fetchAt, c.wait)
			}
			checkLyricEvents(t, lyricEvents(run.events), []string{"all_lyrics@40.00:在线一/在线二", "lyric_update#1:在线二"})
		})
	}
}

// 收到停用之后，换下一首不再 SetHash，直接在线取词。停用是整个会话的，不看结果的 hash。
// 反例：播放中收到的是没有，下一首照常 SetHash。
//
// 变异自证：收到停用不作处理，前三个子例红。
func TestSessionLocalDisabledStopsAsking(t *testing.T) {
	rounds := [][]*cdp.PlayInfo{
		twice(read("A", "playing", 40, 200)),
		once(read("A", "playing", 41, 200)),
		once(read("A", "playing", 42, 200)),
		twice(read("B", "playing", 0, 200)),
	}
	cases := []struct {
		name    string
		replies []reply
		wantSet []string
	}{
		{"换歌拍收到停用", []reply{{100 * time.Millisecond, assoc.Result{Hash: "A", Kind: assoc.KindDisabled}}}, []string{"A"}},
		{"播放中收到停用", []reply{{1500 * time.Millisecond, assoc.Result{Hash: "A", Kind: assoc.KindDisabled}}}, []string{"A"}},
		{"停用不看 hash", []reply{{100 * time.Millisecond, assoc.Result{Kind: assoc.KindDisabled}}}, []string{"A"}},
		{"播放中收到没有", []reply{{1500 * time.Millisecond, assoc.Result{Hash: "A", Kind: assoc.KindNone}}}, []string{"A", "B"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := runLocal(t, 1000, map[string][]reply{"A": c.replies}, rounds...)
			if !slices.Equal(run.set, c.wantSet) || !slices.Equal(run.fetched, []string{"A", "B"}) {
				t.Fatalf("SetHash %q、在线取词 %q，want %q、[A B]", run.set, run.fetched, c.wantSet)
			}
		})
	}
}

// 读数 hash 为空（nil 与空 hash 两种）即没有歌，不 SetHash；随后加载的歌照常写入。
func TestSessionEmptyHashNotSent(t *testing.T) {
	run := runLocal(t, 0, nil,
		[]*cdp.PlayInfo{nil, {Filename: "歌手 - 歌", PlayStatus: "playing", Duration: "200000"}},
		twice(read("A", "playing", 0, 200)),
		[]*cdp.PlayInfo{{}},
	)
	if !slices.Equal(run.set, []string{"A"}) {
		t.Fatalf("SetHash %q，want [A]", run.set)
	}
}

// 上次 SetHash 之后 5 s 没有任何结果（读取者疑似阻塞）：下一首换歌拍不等，写入 hash 的同一时刻即在线取词；hash
// 仍照常写入。反例：未满 5 s，或收到过结果，照常等满 300 ms。连续换歌时从最早一次未获结果的写入算起：A 于 0 s、
// B 于 3.3 s 写入，都没有结果，C 于 6.6 s 换歌时离 A 已过 5 s，不等（从 B 算起只过了 3.3 s）。
//
// 变异自证：不判阻塞，「连续换歌」「5 s 无结果」两子例红；改为从最近一次写入算起，「连续换歌」子例红。
func TestSessionStuckReaderSkipsWait(t *testing.T) {
	t.Run("连续换歌从最早一次算起", func(t *testing.T) {
		run := runLocal(t, 1000, nil,
			twice(read("A", "playing", 0, 200)),
			once(read("A", "playing", 1, 200)), once(read("A", "playing", 2, 200)),
			twice(read("B", "playing", 0, 200)),
			once(read("B", "playing", 1, 200)), once(read("B", "playing", 2, 200)),
			twice(read("C", "playing", 0, 200)),
		)
		if !slices.Equal(run.set, []string{"A", "B", "C"}) || len(run.fetchAt) != 3 {
			t.Fatalf("SetHash %q、在线取词 %q，want [A B C]、三次", run.set, run.fetched)
		}
		if b, c := run.fetchAt[1]-run.setAt[1], run.fetchAt[2]-run.setAt[2]; b != localWait || c != 0 {
			t.Fatalf("B、C 的换歌拍各等了 %v、%v（写入于 %v），want %v、0", b, c, run.setAt, localWait)
		}
	})

	cases := []struct {
		name     string
		replies  map[string][]reply
		aTicks   int // 换歌拍之后 A 还读几轮（轮询间隔 1 s）
		wantWait time.Duration
	}{
		{"5 s 无结果", nil, 5, 0},
		{"未满 5 s", nil, 2, localWait},
		{"收到过结果", map[string][]reply{"A": {{100 * time.Millisecond, assoc.Result{Hash: "A", Kind: assoc.KindNone}}}}, 5, localWait},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rounds := [][]*cdp.PlayInfo{twice(read("A", "playing", 0, 200))}
			for i := range c.aTicks {
				rounds = append(rounds, once(read("A", "playing", float64(i+1), 200)))
			}
			rounds = append(rounds, twice(read("B", "playing", 0, 200)))
			run := runLocal(t, 1000, c.replies, rounds...)
			if !slices.Equal(run.set, []string{"A", "B"}) || len(run.fetchAt) != 2 {
				t.Fatalf("SetHash %q、在线取词 %q，want [A B]、两次", run.set, run.fetched)
			}
			if got := run.fetchAt[1] - run.setAt[1]; got != c.wantWait {
				t.Fatalf("B 的换歌拍从写入 hash 到在线取词等了 %v，want %v（A 写入于 %v，B 写入于 %v）", got, c.wantWait, run.setAt[0], run.setAt[1])
			}
		})
	}
}
