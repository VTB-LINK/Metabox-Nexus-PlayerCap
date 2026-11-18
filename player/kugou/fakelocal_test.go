package kugou

// 本文件是会话测试用的假读取者（localLyrics 的桩）与带假读取者跑会话的夹具，供换歌拍取本地歌词、播放中换词、
// 读取者启停几组用例共用。不起真读取者，不碰本机酷狗。
//
// 会话、假读取者送结果的 goroutine 与封面 goroutine 都在 synctest 的泡里；播放器与假读取者也在泡里建：会话等
// 本地结果时 select 的 StopCh 与结果通道须属于泡，泡外建的通道不算持久阻塞，假时钟不走。事件同样在泡里取完，
// 泡内建的通道不能在泡外收发。读数不带封面 URL，假时钟走过 coverTotalBudget 时封面 goroutine 会调 API 兜底，
// 故同 prevprogress_test.go 把 kugouAPIClient 换成返回空响应的桩，泡内不联网。

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/kugou/assoc"
	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
	klyric "Metabox-Nexus-PlayerCap/player/kugou/lyric"
)

// reply 是假读取者在某个 hash 被写入之后 after 发出的一条结果。
type reply struct {
	after time.Duration
	res   assoc.Result
}

// fakeLocal 记下 SetHash 的实参与时刻，按 replies 经容量 1、只留最新的通道送结果（同 assoc.Reader）。
// SetHash 只由会话 goroutine 调用；送结果的 goroutine 只碰通道。
type fakeLocal struct {
	start   time.Time
	set     []string
	setAt   []time.Duration // 相对会话开始
	replies map[string][]reply
	results chan assoc.Result
	ctx     context.Context
	ctxLive bool // 每次 SetHash 时 ctx 都未取消
}

func newFakeLocal(replies map[string][]reply) *fakeLocal {
	return &fakeLocal{start: time.Now(), replies: replies, results: make(chan assoc.Result, 1), ctxLive: true}
}

func (f *fakeLocal) SetHash(h string) {
	f.set = append(f.set, h)
	f.setAt = append(f.setAt, time.Since(f.start))
	if f.ctx == nil || f.ctx.Err() != nil {
		f.ctxLive = false
	}
	for _, r := range f.replies[h] {
		go func() {
			time.Sleep(r.after)
			f.send(r.res)
		}()
	}
	delete(f.replies, h)
}

func (f *fakeLocal) Results() <-chan assoc.Result { return f.results }

// send 清空旧结果后发出 res，只留最新一条。
func (f *fakeLocal) send(res assoc.Result) {
	select {
	case <-f.results:
	default:
	}
	select {
	case f.results <- res:
	default:
	}
}

// starter 返回把 f 交给会话、并记下读取者 ctx 的启动器。
func (f *fakeLocal) starter() localLyricsStarter {
	return func(ctx context.Context) localLyrics {
		f.ctx = ctx
		return f
	}
}

// lyricSet 是一份两行的歌词：首行（0 s）带逐字，次行在 30 s。逐字的 PlayTime 由会话就地套 offset，指纹须在套之前取，
// 带逐字才看得出取晚了。每次调用返回新切片。
func lyricSet(tag string) []klyric.Line {
	return []klyric.Line{
		{Index: 0, Time: 0, Text: tag + "一", Detailed: player.LyricTextDetailed{Duration: 1, Words: []player.LyricTextDetailedWord{
			{Text: tag, Timestamp: 0, Duration: 0.5}, {Text: "一", Timestamp: 0.5, Duration: 0.5}}}},
		{Index: 1, Time: 30, Text: tag + "二"},
	}
}

func found(hash string, lines []klyric.Line) assoc.Result {
	return assoc.Result{Hash: hash, Kind: assoc.KindFound, Lines: lines, Source: assoc.SourceCache}
}

// localRun 是一次带假读取者的会话的记录。时刻都相对会话开始（假时钟）；start 是会话开始的时刻。
type localRun struct {
	start   time.Time
	events  []player.Event
	set     []string
	setAt   []time.Duration
	fetched []string // fetchLyrics 的 hash 实参
	fetchAt []time.Duration
}

// localOffsetMs 是这几组用例的 offset：非 0，逐字的 PlayTime 因之改写（见 lyricSet）。
const localOffsetMs = 500

// runLocal 以 rounds 跑一次带假读取者的会话（每个元素是一轮的读数）：在线取词对 hash 以 X 开头的歌返回空（取不到
// 歌词），对其余的返回 lyricSet("在线")；假读取者按 replies 送结果。顺带断言读取者的 ctx 在会话中未取消、会话返回时
// 已取消。
func runLocal(t *testing.T, pollMs int, replies map[string][]reply, rounds ...[]*cdp.PlayInfo) localRun {
	t.Helper()
	return runLocalWith(t, pollMs, nil, replies, rounds...)
}

// runLocalWith 同 runLocal，fetch 非 nil 时在线取词改由它给出（在泡内调用，其中的 time.Sleep 走假时钟）。
func runLocalWith(t *testing.T, pollMs int, fetch func(hash string) ([]klyric.Line, error), replies map[string][]reply, rounds ...[]*cdp.PlayInfo) localRun {
	t.Helper()
	var reads []*cdp.PlayInfo
	for _, r := range rounds {
		reads = append(reads, r...)
	}
	var run localRun
	origLyrics, origAPI := fetchLyrics, kugouAPIClient
	kugouAPIClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}, Request: r}, nil
	})}
	defer func() { fetchLyrics, kugouAPIClient = origLyrics, origAPI }()

	synctest.Test(t, func(t *testing.T) {
		p := New(localOffsetMs, pollMs)
		f := newFakeLocal(replies)
		run.start = f.start
		fetchLyrics = func(hash string, _ int, _, _ string) ([]klyric.Line, error) {
			run.fetched = append(run.fetched, hash)
			run.fetchAt = append(run.fetchAt, time.Since(f.start))
			if fetch != nil {
				return fetch(hash)
			}
			if strings.HasPrefix(hash, "X") {
				return nil, nil
			}
			return lyricSet("在线"), nil
		}
		p.runSession(&scriptedSource{reads: reads}, f.starter())
		if f.ctx == nil || f.ctx.Err() == nil || !f.ctxLive {
			t.Errorf("读取者的 ctx 应在会话中未取消、会话返回时已取消（ctx=%v，会话中未取消=%v）", f.ctx, f.ctxLive)
		}
		synctest.Wait() // 已取消的封面 goroutine 退出之后再取事件
		run.events = drainAll(p)
		run.set, run.setAt = f.set, f.setAt
	})
	return run
}

// drainAll 取走通道里已有的全部事件。
func drainAll(p *KuGouPlayer) []player.Event {
	var got []player.Event
	for {
		select {
		case evt := <-p.EventCh:
			got = append(got, evt)
		default:
			return got
		}
	}
}

// lyricEvents 按先后返回 all_lyrics 与 lyric_update，记为「all_lyrics@位置:各行原文」与「lyric_update#行号:原文」。
func lyricEvents(evts []player.Event) []string {
	var got []string
	for _, e := range evts {
		switch d := e.Data.(type) {
		case *player.AllLyricsData:
			var texts []string
			for _, l := range d.Lyrics {
				texts = append(texts, l.Text)
			}
			got = append(got, e.Type+"@"+strconv.FormatFloat(float64(d.Position), 'f', 2, 32)+":"+strings.Join(texts, "/"))
		case *player.LyricUpdate:
			got = append(got, e.Type+"#"+strconv.Itoa(d.Index)+":"+d.Text)
		}
	}
	return got
}

func checkLyricEvents(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("歌词事件\n got  %q\n want %q", got, want)
	}
}
