package kugou

// 本文件只测本地文件在换歌拍不沿用同名上一首的歌词（#205，设计 §6.7「换歌拍」例外、「P 属于这一次播放」）：换歌拍
// 得到「没有」、不待重试、带本地文件标记、这一首有界限且 P 属于这一次播放的结果时，在线取不到或出错都不沿用，按无
// 歌词处理；任一条件不满足照旧沿用。界限取上一首收到过的、属于其那一次播放的 P 的最大值，逐首相接。播放中清词见
// localfileclear_test.go。夹具见 fakelocal_test.go；轮询间隔 1 s，会话从假时钟的起点开始，LastPlayTime 以 lpt 相对
// 它给出。
//
// 变异自证（逐条注入、跑完即还原，实测）记在报告与各用例的注释里。

import (
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/player/kugou/assoc"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// bubbleEpoch 是 synctest 泡内假时钟的起点（testing/synctest 的文档：2000-01-01 00:00:00 UTC），会话从这一刻开始。
var bubbleEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// lpt 是会话开始后 d（可为负）那一刻的 LastPlayTime。
func lpt(d time.Duration) int64 { return fileTime(bubbleEpoch.Add(d)) }

// diskNone 是本地文件规则所指形状的结果：没有、不待重试、带本地文件标记，P 的 LastPlayTime 为 at。
func diskNone(hash string, at int64) assoc.Result {
	return assoc.Result{Hash: hash, Kind: assoc.KindNone, LocalFile: true, PlayedAt: at}
}

// onlineNone 同 diskNone，但不带本地文件标记（P 为在线条目）。
func onlineNone(hash string, at int64) assoc.Result {
	return assoc.Result{Hash: hash, Kind: assoc.KindNone, PlayedAt: at}
}

// sameName 是同名同歌手的几首共用的文件名（开关伴唱、或同名的另一份文件）。
const sameName = "歌手 - 同名歌"

// beats 是一首歌连续 n 拍的读数：换歌那一拍两次（换歌拍与取词后的重读），其后每拍一次；进度从 0 起每拍加 1 s。
func beats(file, hash string, n int) [][]*cdp.PlayInfo {
	at := func(sec float64) *cdp.PlayInfo {
		r := read(hash, "playing", sec, 200)
		r.Filename = file
		return r
	}
	rounds := [][]*cdp.PlayInfo{twice(at(0))}
	for i := 1; i < n; i++ {
		rounds = append(rounds, once(at(float64(i))))
	}
	return rounds
}

// delayedSource 包装 scriptedSource：在首次返回 delay 中登记的 hash 之前先阻塞对应时长，
// 用于模拟 CDP/WebView 卡顿导致的换歌晚识别（#205 5s 界限测试；#250 起在线取词期间已不再阻塞换歌识别）。
type delayedSource struct {
	scriptedSource
	delay map[string]time.Duration
}

func (s *delayedSource) GetPlayInfo() (*cdp.PlayInfo, error) {
	if len(s.reads) == 0 {
		return nil, errors.New("script exhausted")
	}
	r := s.reads[0]
	s.reads = s.reads[1:]
	if r != nil {
		if d := s.delay[r.Hash]; d > 0 {
			delete(s.delay, r.Hash)
			time.Sleep(d)
		}
	}
	return r, nil
}

// runFile 以 fetchTagged 在线取词、轮询间隔 1 s 跑一次带假读取者的会话，并核对会话从 bubbleEpoch 开始；
// delay 非空时在首次切入指定 hash 的 GetPlayInfo() 返回前注入相应延迟，模拟 CDP 卡顿造成的晚识别。
func runFile(t *testing.T, delay map[string]time.Duration, replies map[string][]reply, rounds ...[]*cdp.PlayInfo) localRun {
	t.Helper()
	var run localRun
	if len(delay) == 0 {
		run = runLocalWith(t, 1000, fetchTagged(), replies, rounds...)
	} else {
		var reads []*cdp.PlayInfo
		for _, r := range rounds {
			reads = append(reads, r...)
		}
		origLyrics, origAPI := fetchLyrics, kugouAPIClient
		kugouAPIClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}, Request: r}, nil
		})}
		defer func() { fetchLyrics, kugouAPIClient = origLyrics, origAPI }()

		synctest.Test(t, func(t *testing.T) {
			p := New(localOffsetMs, 1000)
			f := newFakeLocal(replies)
			run.start = f.start
			fetch := fetchTagged()
			fetchLyrics = func(hash string, _ int, _, _ string) ([]klyric.Line, error) {
				run.fetched = append(run.fetched, hash)
				run.fetchAt = append(run.fetchAt, time.Since(f.start))
				return fetch(hash)
			}
			src := &delayedSource{
				scriptedSource: scriptedSource{reads: reads},
				delay:          maps.Clone(delay),
			}
			p.runSession(src, f.starter())
			if f.ctx == nil || f.ctx.Err() == nil || !f.ctxLive {
				t.Errorf("读取者的 ctx 应在会话中未取消、会话返回时已取消（ctx=%v，会话中未取消=%v）", f.ctx, f.ctxLive)
			}
			synctest.Wait()
			run.events = drainAll(p)
			run.set, run.setAt = f.set, f.setAt
		})
	}
	if !run.start.Equal(bubbleEpoch) {
		t.Fatalf("会话开始于 %v，want %v（lpt 以它为零点）", run.start, bubbleEpoch)
	}
	return run
}

// fetchTagged 是这几组用例的在线取词：hash 以 X 开头取不到、以 E 开头出错，其余返回 lyricSet(hash)。
func fetchTagged() func(string) ([]klyric.Line, error) {
	return func(hash string) ([]klyric.Line, error) {
		switch {
		case strings.HasPrefix(hash, "X"):
			return nil, nil
		case strings.HasPrefix(hash, "E"):
			return nil, errors.New("injected fetch failure")
		}
		return lyricSet(hash), nil
	}
}

// lyricsAt 是以 pos 发出 tag 那份歌词的事件：all_lyrics 与随后的第 0 行；tag 为空即无歌词（count:0 与 index:-1）。
// 第 0 行在 0 s、offset 为 0.5 s：播放中换词（pos 不小于 1）同一拍发出；换歌拍（pos 为 0）要到下一拍才发，其间没有别的
// 歌词事件时两者仍相邻。
func lyricsAt(pos, tag string) []string {
	if tag == "" {
		return []string{"all_lyrics@" + pos + ":", "lyric_update#-1:"}
	}
	return []string{"all_lyrics@" + pos + ":" + tag + "一/" + tag + "二", "lyric_update#0:" + tag + "一"}
}

// shown 是换歌拍以位置 0 发出 tag 那份歌词的事件，不含下一拍才发的第 0 行：这一首只有换歌拍一拍、或下一拍先有换词时用它。
func shown(tag string) []string {
	if tag == "" {
		return lyricsAt("0.00", "")
	}
	return lyricsAt("0.00", tag)[:1]
}

// 换算：1601-01-01 UTC 为 0，1970-01-01 与 2000-01-01 为 FILETIME 的已知值；不足 100 ns 的舍去；时区不影响。会话用例的
// LastPlayTime 由 lpt 经同一函数造出，换算错了它们看不出，只有本例能发现（变异自证：按微秒换算，只有本例红）。
func TestFileTime(t *testing.T) {
	for _, c := range []struct {
		at   time.Time
		want int64
	}{
		{time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC), 0},
		{time.Unix(0, 0), 116444736000000000},
		{bubbleEpoch, 125911584000000000},
		{bubbleEpoch.Add(199 * time.Nanosecond), 125911584000000001},
		{bubbleEpoch.Add(-time.Second), 125911583990000000},
		{bubbleEpoch.In(time.FixedZone("UTC-7", -7*3600)), 125911584000000000},
	} {
		if got := fileTime(c.at); got != c.want {
			t.Errorf("fileTime(%v) = %d，want %d", c.at, got, c.want)
		}
	}
}

// A 是会话第一首（在线，与 B 同名），0.1 s 收到属于其这一次播放的 P（0 s），给 B 定下界限 lpt(0)；B 于 2.1 s 换歌，
// 结果于 2.2 s 送到（超时与其他 hash 两例除外）。B 的 hash 以 X 开头在线取不到、以 E 开头出错。
//
// 变异自证：换歌拍例外漏掉出错一支（只在取不到时不沿用），「出错」一例红；去掉本地文件标记一条，两个「不带本地文件
// 标记」例红；去掉待重试一条，「带待重试」红；界限改为不小于即可（P 等于界限也算），「P 等于界限」红。
func TestSessionLocalFileSwitchNoCarry(t *testing.T) {
	retry := diskNone("XB", lpt(2*time.Second))
	retry.Retry = true
	emptyFound := assoc.Result{Hash: "XB", Kind: assoc.KindFound, LocalFile: true, PlayedAt: lpt(2 * time.Second)}
	at := func(r assoc.Result) []reply { return []reply{{100 * time.Millisecond, r}} }
	cases := []struct {
		name string
		file string // B 的文件名
		hash string
		b    []reply
		want []string // B 换歌拍的歌词事件
	}{
		{"取不到", sameName, "XB", at(diskNone("XB", lpt(2*time.Second))), lyricsAt("0.00", "")},
		{"出错", sameName, "EB", at(diskNone("EB", lpt(2*time.Second))), lyricsAt("0.00", "")},
		{"在线取到歌词照常用", sameName, "B", at(diskNone("B", lpt(2*time.Second))), lyricsAt("0.00", "B")},
		{"不同名、取不到", "歌手 - 另一首", "XB", at(diskNone("XB", lpt(2*time.Second))), lyricsAt("0.00", "")},
		// 反例：任一条件不满足，照旧沿用 A 的歌词。
		{"不带本地文件标记", sameName, "XB", at(onlineNone("XB", lpt(2*time.Second))), lyricsAt("0.00", "A")},
		{"不带本地文件标记、出错", sameName, "EB", at(onlineNone("EB", lpt(2*time.Second))), lyricsAt("0.00", "A")},
		{"带待重试", sameName, "XB", at(retry), lyricsAt("0.00", "A")},
		{"P 等于界限", sameName, "XB", at(diskNone("XB", lpt(0))), lyricsAt("0.00", "A")},
		{"P 早于界限", sameName, "XB", at(diskNone("XB", lpt(-time.Second))), lyricsAt("0.00", "A")},
		{"没有 P", sameName, "XB", at(diskNone("XB", 0)), lyricsAt("0.00", "A")},
		{"暂时失败", sameName, "XB", at(assoc.Result{Hash: "XB", Kind: assoc.KindTransient}), lyricsAt("0.00", "A")},
		{"未确认", sameName, "XB", at(assoc.Result{Hash: "XB", Kind: assoc.KindNotConfirmed}), lyricsAt("0.00", "A")},
		{"停用", sameName, "XB", at(assoc.Result{Hash: "XB", Kind: assoc.KindDisabled}), lyricsAt("0.00", "A")},
		{"KindFound 而没有行", sameName, "XB", at(emptyFound), lyricsAt("0.00", "A")},
		{"超时", sameName, "XB", nil, lyricsAt("0.00", "A")},
		{"其他 hash 的结果丢弃、超时", sameName, "XB", at(diskNone("A", lpt(2*time.Second))), lyricsAt("0.00", "A")},
		{"不同名、不带本地文件标记", "歌手 - 另一首", "XB", at(onlineNone("XB", lpt(2*time.Second))), lyricsAt("0.00", "")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := runFile(t, nil, map[string][]reply{
				"A":    {{100 * time.Millisecond, onlineNone("A", lpt(0))}},
				c.hash: c.b,
			}, slices.Concat(beats(sameName, "A", 2), beats(c.file, c.hash, 2))...)
			if !slices.Equal(run.set, []string{"A", c.hash}) || !slices.Equal(run.fetched, []string{"A", c.hash}) {
				t.Fatalf("SetHash %q、在线取词 %q，want 两者都为 [A %s]", run.set, run.fetched, c.hash)
			}
			if run.setAt[1] != 2100*time.Millisecond {
				t.Fatalf("B 于 %v 换歌，want 2.1s", run.setAt[1])
			}
			checkLyricEvents(t, lyricEvents(run.events), slices.Concat(lyricsAt("0.00", "A"), c.want))
		})
	}
}

// 界限（设计 §6.7「P 属于这一次播放」）：A 是会话第一首、没有界限，其 P 只按开始时刻（0 s）减 5 秒判；A 的第一条结果
// 于 0.1 s 到（换歌拍收下），第二条于 0.5 s 到（1.1 s 那拍收下）。B 与 A 同名、在线取不到，于 2.1 s 换歌（A 没有结果
// 的一例为 2.3 s），0.1 s 后收到「没有」、带本地文件标记、P 为 bP 的结果。carry 为照旧沿用 A 的歌词。
//
// 变异自证：界限取最后一条（不取最大值），「先 300 后 100」红；播放中收到的结果不计入（只取换歌拍那一条），「先 100
// 后 300」的「B 等于 300」「B 在两者之间」红；去掉「有界限」要求，「上一首的 P 早于其开始时刻减 5 秒」「没有 P」「没有
// 收到结果」三例红；去掉 5 秒一条，「这一首的 P 早于」与「上一首的 P 早于」两例红；5 秒改为严格晚于（不含恰为减 5 秒），
// 两个「恰为」例红；大于界限改为不小于，「B 等于 300」红。
func TestSessionLocalFileBound(t *testing.T) {
	const sec = time.Second
	results := func(ps ...int64) []reply {
		var rs []reply
		for i, p := range ps {
			rs = append(rs, reply{time.Duration(100+400*i) * time.Millisecond, onlineNone("A", p)})
		}
		return rs
	}
	cases := []struct {
		name  string
		a     []reply
		bP    int64
		carry bool
	}{
		{"先 100 后 300，B 等于 300", results(lpt(-3*sec), lpt(-sec)), lpt(-sec), true},
		{"先 100 后 300，B 大于 300", results(lpt(-3*sec), lpt(-sec)), lpt(-sec) + 1, false},
		{"先 100 后 300，B 在两者之间", results(lpt(-3*sec), lpt(-sec)), lpt(-2 * sec), true},
		{"先 300 后 100，B 在两者之间", results(lpt(-sec), lpt(-3*sec)), lpt(-2 * sec), true},
		{"上一首的 P 恰为其开始时刻减 5 秒", results(lpt(-5 * sec)), lpt(2 * sec), false},
		{"上一首的 P 早于其开始时刻减 5 秒", results(lpt(-5*sec) - 1), lpt(2 * sec), true},
		{"上一首没有 P", results(0), lpt(2 * sec), true},
		{"上一首没有收到结果", nil, lpt(2 * sec), true},
		// 界限为 lpt(-5 s)；B 的开始时刻减 5 秒为 -2.9 s。
		{"这一首的 P 恰为其开始时刻减 5 秒", results(lpt(-5 * sec)), lpt(-2900 * time.Millisecond), false},
		{"这一首的 P 早于其开始时刻减 5 秒", results(lpt(-5 * sec)), lpt(-2900*time.Millisecond) - 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := runFile(t, nil, map[string][]reply{
				"A":  c.a,
				"XB": {{100 * time.Millisecond, diskNone("XB", c.bP)}},
			}, slices.Concat(beats(sameName, "A", 2), beats(sameName, "XB", 2))...)
			wantB := 2100 * time.Millisecond
			if c.a == nil {
				wantB += localWait - 100*time.Millisecond
			}
			if run.setAt[0] != 0 || run.setAt[1] != wantB {
				t.Fatalf("A、B 于 %v 换歌，want 0、%v", run.setAt, wantB)
			}
			want := lyricsAt("0.00", "")
			if c.carry {
				want = lyricsAt("0.00", "A")
			}
			checkLyricEvents(t, lyricEvents(run.events), slices.Concat(lyricsAt("0.00", "A"), want))
		})
	}
}

// 换歌识别晚于 B 开播逾 5 秒：CDP/WebView 卡顿 8 s 导致切入 B（与 A 同名的本地文件）的 GetPlayInfo 延迟返回，
// 其间酷狗已于 1 s 换到 B，会话于 9.1 s 才认出 B，收到的 P 为 1 s。B 的界限为 A 的 0 s，P 大于界限，但早于 B 的开始时刻减
// 5 秒，不属于这一次：照旧沿用，播放中再收到同样的结果也不清；B 也不给下一首 C 定界限，C 收到属于其这一次播放的「没有」仍沿用。
//
// 变异自证：去掉 5 秒一条、换歌拍不记开始时刻，B 不沿用，红；界限不逐首相接、去掉「有界限」要求，C 不沿用，红。
func TestSessionLocalFileLateRecognition(t *testing.T) {
	run := runFile(t, map[string]time.Duration{"XB": 8 * time.Second}, map[string][]reply{
		"A": {{100 * time.Millisecond, onlineNone("A", lpt(0))}},
		"XB": {
			{100 * time.Millisecond, diskNone("XB", lpt(time.Second))},
			{1500 * time.Millisecond, diskNone("XB", lpt(time.Second))},
		},
		"XC": {{100 * time.Millisecond, diskNone("XC", lpt(12*time.Second))}},
	}, slices.Concat(beats(sameName, "A", 1), beats(sameName, "XB", 3), beats(sameName, "XC", 2))...)
	if want := []time.Duration{0, 9100 * time.Millisecond, 12200 * time.Millisecond}; !slices.Equal(run.setAt, want) {
		t.Fatalf("A、B、C 于 %v 换歌，want %v", run.setAt, want)
	}
	checkLyricEvents(t, lyricEvents(run.events), slices.Concat(shown("A"), lyricsAt("0.00", "A"), lyricsAt("0.00", "A")))
}

// 轮询阻塞期间漏认中间几首：0 s 认出 X、P 为 0 s；随后 CDP/WebView 卡顿 11 s 导致切入 A 的 GetPlayInfo 延迟返回，
// 其间酷狗 2 s 播 A、4 s 播本地 H、11 s 重播 A（未落盘）；12.1 s 认出 A，只收到 P 为 2 s 的结果；13.2 s 开伴唱回到 H
// （与 A 同名，hash 即 H 的），收到 P 为 4 s 的「没有」、带本地文件标记。A 的 2 s 大于其界限（0 s），但早于其开始时刻减 5 秒，
// 不属于 A 这一次，伴奏没有界限：照旧沿用 A 的歌词，之后同样的结果也不清。
//
// 变异自证：去掉 5 秒一条，伴奏不沿用，红。
func TestSessionLocalFileMissedSongs(t *testing.T) {
	run := runFile(t, map[string]time.Duration{"A": 11 * time.Second}, map[string][]reply{
		"X0": {{100 * time.Millisecond, onlineNone("X0", lpt(0))}},
		"A":  {{100 * time.Millisecond, onlineNone("A", lpt(2*time.Second))}},
		"XH": {
			{100 * time.Millisecond, diskNone("XH", lpt(4*time.Second))},
			{1500 * time.Millisecond, diskNone("XH", lpt(4*time.Second))},
		},
	}, slices.Concat(beats("歌手 - 别的歌", "X0", 1), beats(sameName, "A", 1), beats(sameName, "XH", 3))...)
	if want := []time.Duration{0, 12100 * time.Millisecond, 13200 * time.Millisecond}; !slices.Equal(run.setAt, want) {
		t.Fatalf("X、A、伴奏于 %v 换歌，want %v", run.setAt, want)
	}
	checkLyricEvents(t, lyricEvents(run.events), slices.Concat(shown(""), shown("A"), lyricsAt("0.00", "A")))
}

// 快速来回：0 s 播本地 H（hash XH），取到其本地歌词、P 为 0 s；1.1 s 重播在线的 A（与 H 不同名），只收到 A 更早一次播放
// 的旧 P（-2 s，落在 A 的开始时刻减 5 秒之内）；2.2 s 开伴唱回到 H（与 A 同名），结果为 H 那一行（P 为 0 s）、没有、
// 不待重试、带本地文件标记，在线也取不到。A 的界限为 0 s，-2 s 不大于它，不属于 A 这一次，伴奏没有界限：照旧沿用 A 的
// 歌词，之后同样的结果也不清。XH 两次换歌只写入一次假读取者的回复，伴奏的结果按第一次写入的时刻排定。
//
// 变异自证：去掉「大于界限」一条，或界限不逐首相接（取上一首各结果 P 的最大值，不论是否属于其那一次播放），伴奏
// 不沿用，红。
func TestSessionLocalFileBackAndForth(t *testing.T) {
	run := runFile(t, nil, map[string][]reply{
		"XH": {
			{100 * time.Millisecond, playedAt(foundAssoc("XH", lyricSet("H")), lpt(0))},
			{2300 * time.Millisecond, diskNone("XH", lpt(0))},
			{3500 * time.Millisecond, diskNone("XH", lpt(0))},
		},
		"A": {{100 * time.Millisecond, onlineNone("A", lpt(-2*time.Second))}},
	}, slices.Concat(beats("歌手 - 本地歌", "XH", 1), beats(sameName, "A", 1), beats(sameName, "XH", 4))...)
	if want := []time.Duration{0, 1100 * time.Millisecond, 2200 * time.Millisecond}; !slices.Equal(run.setAt, want) {
		t.Fatalf("H、A、伴奏于 %v 换歌，want %v", run.setAt, want)
	}
	checkLyricEvents(t, lyricEvents(run.events), slices.Concat(shown("H"), shown("A"), lyricsAt("0.00", "A")))
}

// 伴唱回到本地文件的 hash（同上，H 播完切到在线原唱 O，再开伴唱回到 H）：
//   - 原唱落盘（P 为 1 s，属于 O 这一次，O 给伴奏定下界限 1 s）后开伴唱：落盘前伴奏的 P 是 H 那条本地行（0 s），不大于
//     界限，照旧沿用；落盘后 P 为在线形式（不带标记）的「没有」，不清；其后再来本地行的旧 P，同样不清。
//   - 原唱只收到更早一次播放的旧 P（-60 s）即开伴唱：伴奏没有界限，照旧沿用；之后即使收到 P 属于这一次、带标记的
//     「没有」也不清。
//
// 变异自证：去掉「大于界限」一条、会话不看本地文件标记，「原唱落盘后开伴唱」红；界限不逐首相接、去掉「有界限」要求，
// 「原唱只收到旧 P」红。
func TestSessionLocalFileAccompaniment(t *testing.T) {
	cases := []struct {
		name  string
		oP    int64
		later []reply
	}{
		{"原唱落盘后开伴唱", lpt(time.Second), []reply{
			{3500 * time.Millisecond, onlineNone("XH", lpt(2200*time.Millisecond))},
			{4500 * time.Millisecond, diskNone("XH", lpt(0))},
		}},
		{"原唱只收到旧 P", lpt(-60 * time.Second), []reply{
			{3500 * time.Millisecond, diskNone("XH", lpt(2200*time.Millisecond))},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := runFile(t, nil, map[string][]reply{
				"XH": append([]reply{
					{100 * time.Millisecond, playedAt(foundAssoc("XH", lyricSet("H")), lpt(0))},
					{2300 * time.Millisecond, diskNone("XH", lpt(0))},
				}, c.later...),
				"O": {{100 * time.Millisecond, onlineNone("O", c.oP)}},
			}, slices.Concat(beats("歌手 - 本地歌", "XH", 1), beats(sameName, "O", 1), beats(sameName, "XH", 4))...)
			if want := []time.Duration{0, 1100 * time.Millisecond, 2200 * time.Millisecond}; !slices.Equal(run.setAt, want) {
				t.Fatalf("H、O、伴奏于 %v 换歌，want %v", run.setAt, want)
			}
			checkLyricEvents(t, lyricEvents(run.events), slices.Concat(shown("H"), shown("O"), lyricsAt("0.00", "O")))
		})
	}
}

// 没有读取者（启动器为 nil）：同名的下一首在线取不到或出错，照旧沿用，与接入本地歌词之前相同。轮询间隔为 0，假时钟
// 不走，各首都只有换歌拍的 all_lyrics。
func TestSessionNoReaderStillCarries(t *testing.T) {
	orig := fetchLyrics
	fetchLyrics = func(hash string, _ int, _, _ string) ([]klyric.Line, error) { return fetchTagged()(hash) }
	defer func() { fetchLyrics = orig }()
	synctest.Test(t, func(t *testing.T) {
		p := New(localOffsetMs, 0)
		var reads []*cdp.PlayInfo
		for _, r := range slices.Concat(beats(sameName, "A", 1), beats(sameName, "XB", 1), beats(sameName, "EB", 1)) {
			reads = append(reads, r...)
		}
		p.runSession(&scriptedSource{reads: reads}, nil)
		synctest.Wait()
		checkLyricEvents(t, lyricEvents(drainAll(p)), slices.Concat(shown("A"), shown("A"), shown("A")))
	})
}
