package wesing

// 本文件钉住封面 goroutine 的句柄归属（issue #117）：内存重试用的句柄由 goroutine 自己另开、自己关，
// 不借会话的句柄；会话结束时 Start 关掉会话句柄，不能影响还在途中的内存扫描。另钉住宣告时没有 mid 时的
// 元数据补读（#145，设计 §4.4「元数据补读」、§6），以及 PCM 模式按路径 mid 的补读（#146，设计 §3.5）。

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
	"Metabox-Nexus-PlayerCap/player/wesing/lyric"
)

const (
	testSessionHandle = syscall.Handle(0x99)   // 会话的句柄：封面 goroutine 不得碰它
	testOwnHandle     = syscall.Handle(0x1234) // openProc 给封面 goroutine 的句柄
	testPID           = uint32(4242)
	testMID           = "003msPI80HftbW"
	testAlbumMID      = "001fN0ks2oUDvX"
	testCover         = "http://imgcache.qq.com/music/photo/mid_album_800/v/X/001fN0ks2oUDvX.jpg"
)

// opLog 记下假操作被调用的先后顺序，供断言「先扫描、后关闭」之类。
type opLog struct {
	mu  sync.Mutex
	ops []string
}

func (l *opLog) add(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, fmt.Sprintf(format, a...))
}

func (l *opLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ops...)
}

// fakeOps 造一组假操作。findAfter 是第几次 findAlbum 才查到专辑 mid（0 = 一直查不到）；
// openErr 非空时 openProc 失败；onDownload 在 download 里被调用（可模拟下载期间会话结束）。
func fakeOps(l *opLog, findAfter int, openErr error, onDownload func()) coverFetchOps {
	finds := 0
	return coverFetchOps{
		openProc: func(pid uint32) (syscall.Handle, error) {
			l.add("open(%d)", pid)
			if openErr != nil {
				return 0, openErr
			}
			return testOwnHandle, nil
		},
		closeProc: func(h syscall.Handle) { l.add("close(%#x)", uintptr(h)) },
		findInfo: func(h syscall.Handle, name string) (lyric.SongInfo, error) {
			l.add("info(%#x,%s)", uintptr(h), name)
			return lyric.SongInfo{}, errors.New("songname field not found")
		},
		// 按 mid 查歌曲信息（PCM 模式补读、兜底模式补歌手，#146）默认一直查不到，见 withByMID；withAlbum 可另行
		// 指定专辑的结果。
		findByMID: func(h syscall.Handle, mid string) (lyric.SongInfo, bool) {
			l.add("bymid(%#x,%s)", uintptr(h), mid)
			return lyric.SongInfo{}, false
		},
		findAlbum: func(h syscall.Handle, mid string) string {
			l.add("album(%#x,%s)", uintptr(h), mid)
			finds++
			if findAfter > 0 && finds >= findAfter {
				return testAlbumMID
			}
			return ""
		},
		download: func(url string) string {
			l.add("download(%s)", url)
			if onDownload != nil {
				onDownload()
			}
			return "B64"
		},
		sleep: func(time.Duration) { l.add("sleep") },
	}
}

func newTestPlayer() *WesingPlayer {
	return &WesingPlayer{BaseEmitter: player.NewBaseEmitter(PlayerName)}
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("封面 goroutine 5 秒内没有结束")
	}
}

// drainEvents 取出事件通道里当前所有事件（非阻塞）。
func drainEvents(p *WesingPlayer) []player.Event {
	var out []player.Event
	for {
		select {
		case e := <-p.EventCh:
			out = append(out, e)
		default:
			return out
		}
	}
}

func indexOf(ops []string, prefix string) int {
	for i, o := range ops {
		if len(o) >= len(prefix) && o[:len(prefix)] == prefix {
			return i
		}
	}
	return -1
}

// 需要内存重试时：goroutine 自己用 pid 另开句柄，所有扫描都用这个句柄、不碰会话句柄，下载完成之后才关，
// 且只关一次；回写带上按专辑 mid 构造的地址与 base64。
//
// 变异自证（均实测，下面几条用例合起来）：删掉 defer closeProc 即红；把关闭挪到重试循环之前即红；
// 有地址也开句柄即红；打开失败后仍扫描即红；不另开句柄（拿 pid 充当句柄）即红。
func TestCoverFetchOwnsItsHandle(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}

	waitDone(t, p.startCoverFetch(fakeOps(l, 2, nil, nil), gen, testPID, "name", testMID, "name", "singer", "title", "", false, false))

	ops := l.snapshot()
	want := []string{
		"open(4242)",
		"sleep", "album(0x1234," + testMID + ")",
		"sleep", "album(0x1234," + testMID + ")",
		"download(" + testCover + ")",
		"close(0x1234)",
	}
	if fmt.Sprint(ops) != fmt.Sprint(want) {
		t.Fatalf("操作顺序不对：\n got %q\nwant %q", ops, want)
	}
	for _, o := range ops {
		if o == fmt.Sprintf("close(%#x)", uintptr(testSessionHandle)) {
			t.Errorf("关闭了会话的句柄：%s", o)
		}
	}

	evts := drainEvents(p)
	if len(evts) != 1 || evts[0].Type != player.EventSongInfoUpdate {
		t.Fatalf("应恰好回写一条 song_info_update，实得 %+v", evts)
	}
	info := evts[0].Data.(*player.SongInfo)
	if info.CoverBase64 != "B64" || info.Cover != testCover ||
		info.Name != "name" || info.Singer != "singer" || info.Title != "title" {
		t.Errorf("回写内容不对：%+v", info)
	}
}

// 封面地址一开始就有：全程不碰进程，不开也不关句柄。
func TestCoverFetchWithURLNeverOpensProcess(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}

	waitDone(t, p.startCoverFetch(fakeOps(l, 1, nil, nil), gen, testPID, "n", testMID, "n", "s", "t", "http://x/y.jpg", false, false))

	ops := l.snapshot()
	if fmt.Sprint(ops) != fmt.Sprint([]string{"download(http://x/y.jpg)"}) {
		t.Errorf("有地址时只应下载，实得 %q", ops)
	}
	if evts := drainEvents(p); len(evts) != 1 {
		t.Errorf("应回写一条，实得 %d", len(evts))
	}
}

// 没有歌曲 MID、窗口歌名也是空的：没法补读，也不需要找封面，不开句柄（设计 §4.4：窗口歌名为空时不补读）。
// #145 之前这一条是「没有 MID 就不开进程」；现在没有 MID 而有窗口歌名时要补读，见 TestCoverFetchBackfill*。
//
// 变异自证（实测）：窗口歌名为空也补读，打开了进程、补读了 5 次而红。
func TestCoverFetchWithoutMIDOrWindowTitleNeverOpensProcess(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}

	waitDone(t, p.startCoverFetch(fakeOps(l, 1, nil, nil), gen, testPID, "", "", "n", "s", "t", "", false, false))

	if ops := l.snapshot(); len(ops) != 0 {
		t.Errorf("既没地址、没 MID，也没窗口歌名，不该有任何操作，实得 %q", ops)
	}
	if evts := drainEvents(p); len(evts) != 0 {
		t.Errorf("不该回写，实得 %+v", evts)
	}
}

// 打不开进程：跳过内存重试，不扫描、不下载、不回写，也不关一个没打开的句柄。没有 MID、要补读时同样。
func TestCoverFetchOpenFailureSkipsScan(t *testing.T) {
	for _, mid := range []string{testMID, ""} {
		p := newTestPlayer()
		gen := p.NewSongGen()
		l := &opLog{}

		waitDone(t, p.startCoverFetch(fakeOps(l, 1, errors.New("denied"), nil), gen, testPID, "n", mid, "n", "s", "t", "", false, false))

		if ops := l.snapshot(); fmt.Sprint(ops) != fmt.Sprint([]string{"open(4242)"}) {
			t.Errorf("mid=%q：打开失败后只应有 open，实得 %q", mid, ops)
		}
		if evts := drainEvents(p); len(evts) != 0 {
			t.Errorf("mid=%q：不该回写，实得 %+v", mid, evts)
		}
	}
}

// 五次都找不到：扫描五次，关闭一次，不下载、不回写。
func TestCoverFetchNeverFoundClosesOnce(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}

	waitDone(t, p.startCoverFetch(fakeOps(l, 0, nil, nil), gen, testPID, "n", testMID, "n", "s", "t", "", false, false))

	ops := l.snapshot()
	if finds, closes := count(ops, "album"), count(ops, "close"); finds != 5 || closes != 1 {
		t.Errorf("应扫描 5 次、关闭 1 次，实得 %d 与 %d：%q", finds, closes, ops)
	}
	if indexOf(ops, "close") != len(ops)-1 {
		t.Errorf("关闭应是最后一步：%q", ops)
	}
	if indexOf(ops, "download") >= 0 {
		t.Errorf("没有地址不该下载：%q", ops)
	}
	if evts := drainEvents(p); len(evts) != 0 {
		t.Errorf("不该回写，实得 %+v", evts)
	}
}

// 下载期间会话结束（代次被作废）：回写被丢弃，但句柄仍要关。
func TestCoverFetchInvalidatedStillClosesHandle(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}

	waitDone(t, p.startCoverFetch(fakeOps(l, 1, nil, p.InvalidateSongGen), gen, testPID, "n", testMID, "n", "s", "t", "", false, false))

	ops := l.snapshot()
	if indexOf(ops, "close(0x1234)") < 0 {
		t.Errorf("代次作废后仍应关闭自己的句柄：%q", ops)
	}
	if evts := drainEvents(p); len(evts) != 0 {
		t.Errorf("代次已作废，回写应被丢弃，实得 %+v", evts)
	}
}

// ===== 元数据补读（#145）=====

const (
	bfWindow  = "晴天" // 宣告时的窗口歌名
	bfSinger  = "周杰伦"
	bfPrevMID = "001prevSongMid"
)

// withInfo 让 findInfo 依次返回 infos，Name 为空的一项表示这一次读不到；用完之后一直读不到。
func withInfo(ops coverFetchOps, l *opLog, infos ...lyric.SongInfo) coverFetchOps {
	n := 0
	ops.findInfo = func(h syscall.Handle, name string) (lyric.SongInfo, error) {
		l.add("info(%#x,%s)", uintptr(h), name)
		var info lyric.SongInfo
		if n < len(infos) {
			info = infos[n]
		}
		n++
		if info.Name == "" {
			return lyric.SongInfo{}, errors.New("songname field not found")
		}
		return info, nil
	}
	return ops
}

// songInfos 取出回写的各条 song_info_update，写成「歌名|歌手|标题|封面|base64」。
func songInfos(t *testing.T, p *WesingPlayer) []string {
	t.Helper()
	var out []string
	for _, e := range drainEvents(p) {
		if e.Type != player.EventSongInfoUpdate {
			t.Fatalf("只应回写 song_info_update，实得 %s", e.Type)
		}
		s := e.Data.(*player.SongInfo)
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s", s.Name, s.Singer, s.Title, s.Cover, s.CoverBase64))
	}
	return out
}

func count(ops []string, prefix string) int {
	n := 0
	for _, o := range ops {
		if len(o) >= len(prefix) && o[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

// 宣告时取不到歌曲信息（歌名退回窗口歌名、没有歌手与 mid）：第 2 次补读到歌曲信息，先发一次带歌手、不带
// 封面的 song_info_update；有了 mid 之后找封面，第 3 次找到、下载成功，再发一次带封面的。全程用自己开的
// 句柄，最后关闭。
//
// 变异自证（实测）：去掉带歌手的那次回写，第一条没有而红；带歌手的回写之后不更新本地的歌手，带封面的
// 那条歌手为空而红；mid 非空时也补读（补读条件写成恒真），TestCoverFetchOwnsItsHandle 多出 info、不再找
// 封面而红。
func TestCoverFetchBackfillsSingerThenCover(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}
	ops := withInfo(fakeOps(l, 2, nil, nil), l, lyric.SongInfo{}, lyric.SongInfo{Mid: testMID, Name: bfWindow, Singer: bfSinger})

	waitDone(t, p.startCoverFetch(ops, gen, testPID, bfWindow, "", bfWindow, "", bfWindow, "", false, false))

	want := []string{
		"open(4242)",
		"sleep", "info(0x1234," + bfWindow + ")",
		"sleep", "info(0x1234," + bfWindow + ")", "album(0x1234," + testMID + ")",
		"sleep", "album(0x1234," + testMID + ")",
		"download(" + testCover + ")",
		"close(0x1234)",
	}
	if ops := l.snapshot(); fmt.Sprint(ops) != fmt.Sprint(want) {
		t.Fatalf("操作顺序不对：\n got %q\nwant %q", ops, want)
	}
	got := songInfos(t, p)
	wantInfos := []string{
		bfWindow + "|" + bfSinger + "|" + bfWindow + "||",
		bfWindow + "|" + bfSinger + "|" + bfWindow + "|" + testCover + "|B64",
	}
	if fmt.Sprint(got) != fmt.Sprint(wantInfos) {
		t.Errorf("回写不对：\n got %q\nwant %q", got, wantInfos)
	}
}

// 补读到歌曲信息，但 5 次内找不到封面、或封面下载失败：只发带歌手的那一次。
func TestCoverFetchBackfillSingerWithoutCover(t *testing.T) {
	cases := []struct {
		name      string
		findAfter int
		dlFails   bool
	}{
		{"5 次内找不到封面", 0, false},
		{"封面下载失败", 1, true},
	}
	for _, c := range cases {
		p := newTestPlayer()
		gen := p.NewSongGen()
		l := &opLog{}
		ops := withInfo(fakeOps(l, c.findAfter, nil, nil), l, lyric.SongInfo{Mid: testMID, Name: bfWindow, Singer: bfSinger})
		if c.dlFails {
			ops.download = func(url string) string { l.add("download(%s)", url); return "" }
		}

		waitDone(t, p.startCoverFetch(ops, gen, testPID, bfWindow, "", bfWindow, "", bfWindow, "", false, false))

		got := songInfos(t, p)
		if want := []string{bfWindow + "|" + bfSinger + "|" + bfWindow + "||"}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s：回写 %q，want %q", c.name, got, want)
		}
		if ops := l.snapshot(); count(ops, "info") != 1 || indexOf(ops, "close") != len(ops)-1 {
			t.Errorf("%s：补到之后不该再补读，关闭应是最后一步：%q", c.name, ops)
		}
	}
}

// 宣告时已有歌名与歌手、只缺 mid：第 1 次补读到的记录也没有 mid，第 2 次才有，第 3 次找到封面。只发带封面
// 的那一次，歌名、歌手、标题是宣告时的。
//
// 变异自证（实测）：宣告时已有歌手也发带歌手的那次回写，多出两条而红；只认补读到的第一条记录（之后不再
// 补读），补读只有 1 次、拿不到 mid 而红。
func TestCoverFetchBackfillMidOnly(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}
	title := bfWindow + " - " + bfSinger
	ops := withInfo(fakeOps(l, 2, nil, nil), l,
		lyric.SongInfo{Name: bfWindow, Singer: bfSinger},
		lyric.SongInfo{Mid: testMID, Name: bfWindow, Singer: bfSinger})

	waitDone(t, p.startCoverFetch(ops, gen, testPID, bfWindow, "", bfWindow, bfSinger, title, "", false, false))

	got := songInfos(t, p)
	if want := []string{bfWindow + "|" + bfSinger + "|" + title + "|" + testCover + "|B64"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("回写 %q，want %q", got, want)
	}
	if ops := l.snapshot(); count(ops, "info") != 2 || count(ops, "album") != 2 {
		t.Errorf("应补读 2 次、找封面 2 次：%q", ops)
	}
}

// F5 的反例：第 1 次补读只得到上一首 B 的记录（FindSongInfo 匹配不到窗口歌名时退而返回地址最高的记录），
// 第 2 次才得到当前这首 A。拒绝 B（不发它的歌手、不拿它的 mid 找封面），采用 A。
//
// 变异自证（实测）：不核对歌名，第一条回写带上 B 的歌手、拿 B 的 mid 去找封面而红。
func TestCoverFetchBackfillRejectsPreviousSong(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}
	ops := withInfo(fakeOps(l, 1, nil, nil), l,
		lyric.SongInfo{Mid: bfPrevMID, Name: "七里香", Singer: "周杰伦（上一首）"},
		lyric.SongInfo{Mid: testMID, Name: bfWindow, Singer: bfSinger})

	waitDone(t, p.startCoverFetch(ops, gen, testPID, bfWindow, "", bfWindow, "", bfWindow, "", false, false))

	got := songInfos(t, p)
	want := []string{
		bfWindow + "|" + bfSinger + "|" + bfWindow + "||",
		bfWindow + "|" + bfSinger + "|" + bfWindow + "|" + testCover + "|B64",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("回写 %q，want %q", got, want)
	}
	if ops := l.snapshot(); indexOf(ops, "album(0x1234,"+bfPrevMID) >= 0 {
		t.Errorf("不该拿上一首的 mid 找封面：%q", ops)
	}
}

// 宣告的歌名、歌手是上一首 B 的（#146 之前兜底模式宣告 FindSongInfo 退回的记录；现在 songMeta 不再这样宣告，
// 这一条守的是回写条件本身）：补读按窗口歌名匹配上 A 之后，先回写 A 的歌名与歌手、再回写 A 的封面，标题仍是
// 宣告时的。
//
// 变异自证（实测）：补读只在宣告时没有歌手时回写（#146 之前的条件），没有那条带 A 歌手的回写、封面那条带着
// B 的歌名与歌手而红。
func TestCoverFetchBackfillCorrectsAnnouncedPreviousSong(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}
	ops := withInfo(fakeOps(l, 1, nil, nil), l, lyric.SongInfo{Mid: testMID, Name: bfWindow, Singer: bfSinger})
	title := "七里香 - 周杰伦（上一首）"

	waitDone(t, p.startCoverFetch(ops, gen, testPID, bfWindow, "", "七里香", "周杰伦（上一首）", title, "", false, false))

	got := songInfos(t, p)
	want := []string{
		bfWindow + "|" + bfSinger + "|" + title + "||",
		bfWindow + "|" + bfSinger + "|" + title + "|" + testCover + "|B64",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("回写 %q，want %q", got, want)
	}
}

// 歌名只差一点也不认：多一个空格、带「（Live）」后缀，都不是窗口上这首。
//
// 变异自证（实测）：不核对歌名，第一条就被采用、回写两次而红。
func TestCoverFetchBackfillNameMustMatchExactly(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}
	ops := withInfo(fakeOps(l, 1, nil, nil), l,
		lyric.SongInfo{Mid: bfPrevMID, Name: bfWindow + " ", Singer: bfSinger},
		lyric.SongInfo{Mid: bfPrevMID, Name: bfWindow + "（Live）", Singer: bfSinger})

	waitDone(t, p.startCoverFetch(ops, gen, testPID, bfWindow, "", bfWindow, "", bfWindow, "", false, false))

	if got := songInfos(t, p); len(got) != 0 {
		t.Errorf("不该回写，实得 %q", got)
	}
	if ops := l.snapshot(); count(ops, "info") != 5 || count(ops, "album") != 0 {
		t.Errorf("应补读 5 次、一次都不找封面：%q", ops)
	}
}

// 5 次都读不到：补读 5 次，不找封面、不下载、不回写，关闭一次且是最后一步。
func TestCoverFetchBackfillNeverFound(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}

	waitDone(t, p.startCoverFetch(withInfo(fakeOps(l, 1, nil, nil), l), gen, testPID, bfWindow, "", bfWindow, "", bfWindow, "", false, false))

	ops := l.snapshot()
	if count(ops, "info") != 5 || count(ops, "album") != 0 || count(ops, "download") != 0 || count(ops, "close") != 1 || indexOf(ops, "close") != len(ops)-1 {
		t.Errorf("应补读 5 次、不找封面不下载、最后关闭一次：%q", ops)
	}
	if got := songInfos(t, p); len(got) != 0 {
		t.Errorf("不该回写，实得 %q", got)
	}
}

// 补读到的记录一直没有 mid：带歌手的回写只发一次，不随每次补读重发；没有 mid 就不找封面。
//
// 变异自证（实测）：不看宣告时有没有歌手就发带歌手的回写，第 2～5 次补读各多发一条而红；回写之后不更新
// 本地的歌手，同样每次都发而红。
func TestCoverFetchBackfillSingerSentOnce(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}
	rec := lyric.SongInfo{Name: bfWindow, Singer: bfSinger}
	ops := withInfo(fakeOps(l, 1, nil, nil), l, rec, rec, rec, rec, rec)

	waitDone(t, p.startCoverFetch(ops, gen, testPID, bfWindow, "", bfWindow, "", bfWindow, "", false, false))

	if got, want := songInfos(t, p), []string{bfWindow + "|" + bfSinger + "|" + bfWindow + "||"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("回写 %q，want %q", got, want)
	}
	if ops := l.snapshot(); count(ops, "info") != 5 || count(ops, "album") != 0 {
		t.Errorf("应补读 5 次、不找封面：%q", ops)
	}
}

// 补读期间切歌（代次变化）：带歌手与带封面的回写都丢弃，句柄照样关。
//
// 变异自证（实测）：带歌手的那次回写不经 EmitForGen（直接 Emit），切歌之后仍发出而红。
func TestCoverFetchBackfillInvalidated(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}
	ops := withInfo(fakeOps(l, 1, nil, nil), l, lyric.SongInfo{Mid: testMID, Name: bfWindow, Singer: bfSinger})
	inner := ops.findInfo
	ops.findInfo = func(h syscall.Handle, name string) (lyric.SongInfo, error) {
		p.InvalidateSongGen() // 补读的这一下切了歌
		return inner(h, name)
	}

	waitDone(t, p.startCoverFetch(ops, gen, testPID, bfWindow, "", bfWindow, "", bfWindow, "", false, false))

	if got := songInfos(t, p); len(got) != 0 {
		t.Errorf("代次已作废，回写应被丢弃，实得 %q", got)
	}
	if ops := l.snapshot(); indexOf(ops, "close(0x1234)") < 0 {
		t.Errorf("代次作废后仍应关闭自己的句柄：%q", ops)
	}
}

// 补读匹配上的记录有 mid 而没有歌手时按 mid 补歌手，查到的歌名相同才取（#146，设计 §3.2、§3.5、§4）。宣告时是
// 窗口歌名、没有歌手（songMeta 匹配不到）。
//
// 变异自证（实测）：
//   - 不按 mid 补歌手：「按 mid 查到同名记录」第一条回写没有、封面那条没有歌手而红，前三例少了 bymid 而红；
//   - 按 mid 补歌手不核对歌名：「按 mid 查到的歌名不同」多出一条带别的歌歌手的回写而红；
//   - 记录有歌手也按 mid 查：TestCoverFetchBackfillsSingerThenCover 的操作序列多出 bymid 而红；
//   - 记录没有 mid 也按 mid 查：「记录没有 mid」拿空串查了 5 次而红。
func TestCoverFetchBackfillSingerByMID(t *testing.T) {
	bymid := "bymid(0x1234," + testMID + ")"
	cases := []struct {
		name  string
		rec   lyric.SongInfo
		byMID lyric.SongInfo
		ops   []string // 不含 open 与末尾的 close
		infos []string
	}{
		{"按 mid 查到同名记录：取它的歌手", lyric.SongInfo{Mid: testMID, Name: bfWindow},
			lyric.SongInfo{Mid: testMID, Name: bfWindow, Singer: bfSinger},
			[]string{"sleep", "info(0x1234," + bfWindow + ")", bymid, "album(0x1234," + testMID + ")", "download(" + testCover + ")"},
			[]string{bfWindow + "|" + bfSinger + "|" + bfWindow + "||", bfWindow + "|" + bfSinger + "|" + bfWindow + "|" + testCover + "|B64"}},
		{"按 mid 查到的歌名不同：不补", lyric.SongInfo{Mid: testMID, Name: bfWindow},
			lyric.SongInfo{Mid: testMID, Name: "七里香", Singer: "周杰伦（上一首）"},
			[]string{"sleep", "info(0x1234," + bfWindow + ")", bymid, "album(0x1234," + testMID + ")", "download(" + testCover + ")"},
			[]string{bfWindow + "||" + bfWindow + "|" + testCover + "|B64"}},
		{"按 mid 查不到：不补", lyric.SongInfo{Mid: testMID, Name: bfWindow}, lyric.SongInfo{},
			[]string{"sleep", "info(0x1234," + bfWindow + ")", bymid, "album(0x1234," + testMID + ")", "download(" + testCover + ")"},
			[]string{bfWindow + "||" + bfWindow + "|" + testCover + "|B64"}},
		{"记录没有 mid：不按 mid 查", lyric.SongInfo{Name: bfWindow},
			lyric.SongInfo{Mid: testMID, Name: bfWindow, Singer: bfSinger},
			[]string{"sleep", "info(0x1234," + bfWindow + ")", "sleep", "info(0x1234," + bfWindow + ")", "sleep", "info(0x1234," + bfWindow + ")",
				"sleep", "info(0x1234," + bfWindow + ")", "sleep", "info(0x1234," + bfWindow + ")"},
			nil},
	}
	for _, c := range cases {
		p := newTestPlayer()
		gen := p.NewSongGen()
		l := &opLog{}
		ops := withByMID(withInfo(fakeOps(l, 1, nil, nil), l, c.rec, c.rec, c.rec, c.rec, c.rec), l, c.byMID)

		waitDone(t, p.startCoverFetch(ops, gen, testPID, bfWindow, "", bfWindow, "", bfWindow, "", false, false))

		got := l.snapshot()
		want := append(append([]string{"open(4242)"}, c.ops...), "close(0x1234)")
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s：操作顺序不对：\n got %q\nwant %q", c.name, got, want)
		}
		if gotInfos := songInfos(t, p); fmt.Sprint(gotInfos) != fmt.Sprint(c.infos) {
			t.Errorf("%s：回写不对：\n got %q\nwant %q", c.name, gotInfos, c.infos)
		}
	}
}

// R2 的反例（兜底歌手设计审，设计 §3.5）：宣告时有歌手（来自没有 mid 的记录），补读匹配上的记录有 mid 而歌手为空、
// 按 mid 也补不到：先回写一次清空歌手，封面那次回写同样带空歌手，标题仍是宣告时的。另一例补读匹配上的记录有
// mid、歌手与宣告时不同：照样纠正。
//
// 变异自证（实测）：不拿空歌手覆盖（回写条件加上「补读的歌手非空」）：「歌手为空」没有清空的回写、封面那条带着
// 宣告时的歌手而红；回写条件只看歌名（不看歌手）：两例都没有第一条回写、封面那条带着宣告时的歌手而红。
func TestCoverFetchBackfillOverridesUnverifiedSinger(t *testing.T) {
	title := bfWindow + " - 宣告时的歌手"
	for _, c := range []struct {
		name   string
		singer string // 补读匹配上的记录的歌手
	}{
		{"歌手为空：清空", ""},
		{"歌手不同：纠正", bfSinger},
	} {
		p := newTestPlayer()
		gen := p.NewSongGen()
		l := &opLog{}
		ops := withInfo(fakeOps(l, 1, nil, nil), l, lyric.SongInfo{Mid: testMID, Name: bfWindow, Singer: c.singer})

		waitDone(t, p.startCoverFetch(ops, gen, testPID, bfWindow, "", bfWindow, "宣告时的歌手", title, "", false, false))

		want := []string{
			bfWindow + "|" + c.singer + "|" + title + "||",
			bfWindow + "|" + c.singer + "|" + title + "|" + testCover + "|B64",
		}
		if got := songInfos(t, p); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s：回写 %q，want %q", c.name, got, want)
		}
	}
}

// 兜底模式的补读用「同名优先有 mid」的选取，同宣告（设计 §3.1）；PCM 模式的核对仍用 FindSongInfo（R3），它在
// realPCMQueries 的闭包里，这里查不到，靠 lyric 包的 TestFindSongInfo 守选取本身。
//
// 变异自证（实测）：realCoverFetchOps.findInfo 改回 lyric.FindSongInfo 即红。
func TestRealCoverFetchOpsPreferMID(t *testing.T) {
	if reflect.ValueOf(realCoverFetchOps.findInfo).Pointer() != reflect.ValueOf(lyric.FindSongInfoPreferMID).Pointer() {
		t.Error("兜底模式的补读应按窗口歌名用 lyric.FindSongInfoPreferMID 查记录")
	}
}

// ===== PCM 模式的补读（#146，设计 §3.5、§4）=====
//
// 宣告用的歌曲信息与封面按路径 mid 取（resolvePCM）；未确认的歌曲信息、缺的封面在同一个循环里按路径 mid 补读。
// 常量沿用 pcmsong_test.go：psWindow 是窗口歌名，psMID 是路径 mid，psAlbum 是它的专辑 mid。

var (
	pcCover = lyric.AlbumCoverURL(psAlbum)
	pcCur   = lyric.SongInfo{Mid: psMID, Name: psWindow, Singer: "柳爽"}
	pcPrev  = lyric.SongInfo{Mid: psPrevMID, Name: "这世界那么多人", Singer: "莫文蔚"}
)

// withByMID 让 findByMID 依次返回 infos，Name 为空的一项表示这一次查不到；用完之后一直查不到。
func withByMID(ops coverFetchOps, l *opLog, infos ...lyric.SongInfo) coverFetchOps {
	n := 0
	ops.findByMID = func(h syscall.Handle, mid string) (lyric.SongInfo, bool) {
		l.add("bymid(%#x,%s)", uintptr(h), mid)
		var info lyric.SongInfo
		if n < len(infos) {
			info = infos[n]
		}
		n++
		return info, info.Name != ""
	}
	return ops
}

// withAlbum 让 findAlbum 依次返回 albums，空串表示这一次查不到；用完之后一直查不到。
func withAlbum(ops coverFetchOps, l *opLog, albums ...string) coverFetchOps {
	n := 0
	ops.findAlbum = func(h syscall.Handle, mid string) string {
		l.add("album(%#x,%s)", uintptr(h), mid)
		a := ""
		if n < len(albums) {
			a = albums[n]
		}
		n++
		return a
	}
	return ops
}

// 补读到的歌曲信息回写没入队（事件通道满被丢弃）：下一拍按已查到的重发，不再扫内存（#146 代码审）。专辑一直查
// 不到，没有封面那次回写可以顺带纠正，所以这里不重发就整首没有歌手。
//
// 变异自证（实测）：不看 EmitForGen 的返回值、查到即记为已确认：通道恢复之后没有重发、整首没有歌手而红；重发前
// 再查一次（每拍都调 findByMID）：查了两次而红。
func TestCoverFetchPCMRetriesDroppedInfo(t *testing.T) {
	p := newTestPlayer()
	p.EventCh = make(chan player.Event) // 无缓冲、无人接收：第一拍的回写被丢弃
	var l opLog
	ops := withAlbum(withByMID(pcmOps(&l), &l, pcCur), &l)
	sleeps := 0
	inner := ops.sleep
	ops.sleep = func(d time.Duration) {
		inner(d)
		if sleeps++; sleeps == 2 {
			p.EventCh = make(chan player.Event, 16) // 第二拍之前通道恢复（同一 goroutine 里换，不竞争）
		}
	}
	waitDone(t, p.startCoverFetch(ops, p.NewSongGen(), 4242, psWindow, psMID, psWindow, "", psWindow, "", true, true))
	if got, want := fmt.Sprint(songInfos(t, p)), fmt.Sprint([]string{psWindow + "|柳爽|" + psWindow + "||"}); got != want {
		t.Errorf("回写 %s，want %s", got, want)
	}
	if n := count(l.snapshot(), "bymid"); n != 1 {
		t.Errorf("按 mid 查了 %d 次，want 1 次（查到之后只重发，不重扫）：%q", n, l.snapshot())
	}
}

// pcmOps 是 PCM 模式用例的假操作：按窗口歌名查记录会给出上一首的，不该被调用。
func pcmOps(l *opLog) coverFetchOps {
	return withInfo(fakeOps(l, 0, nil, nil), l, pcPrev, pcPrev, pcPrev, pcPrev, pcPrev)
}

// noFallbackQueries 断言 PCM 模式没有按窗口歌名查记录（设计 §3.5）。
func noFallbackQueries(t *testing.T, ops []string) {
	t.Helper()
	if count(ops, "info") != 0 {
		t.Errorf("PCM 模式不该按窗口歌名查记录：%q", ops)
	}
}

// 补读的各种情形（设计 §3.5、§4）。cover 是宣告时的封面地址，unconfirmed 是宣告时歌曲信息未确认。
//
// 变异自证（实测）：
//   - PCM 模式照走兜底的补读（分支条件写成恒假）：歌曲信息未确认的五例没有按路径 mid 补读而红；已确认、只缺
//     封面的两例两种模式都按 mid 查专辑，仍绿；
//   - 是否补读不看未确认（只看缺不缺封面）：「歌曲信息第 2 次查到」「歌曲信息一直查不到」不补读而红
//     （TestCoverFetchPCMBackfillAfterF1 也红）；
//   - 查到歌曲信息之后不记为已确认：查到之后每次都再查，四例的操作不对而红；
//   - 歌曲信息的回写不带已有的封面地址：「歌曲信息第 2 次查到」「封面先于歌曲信息查到」的第一条没有地址而红
//     （TestCoverFetchPCMBackfillAfterF1 也红）；
//   - 封面查到之后仍每次查专辑：「封面先于歌曲信息查到」多查了专辑、封面宣告时已有的两例也查了专辑而红；
//   - 封面有了就结束循环（不等歌曲信息）：封面宣告时已有或先查到的三例没有补到歌曲信息而红
//     （TestCoverFetchPCMBackfillAfterF1 也红）；
//   - 都齐了也不结束循环：四例多等了几轮而红；
//   - 歌曲信息的回写不经 EmitForGen（直接 Emit）：TestCoverFetchPCMInvalidated 切歌之后仍发出而红。
//
// 以上变异下 #145 的补读用例（TestCoverFetchBackfill* 等）都仍绿：兜底模式的行为没有被 PCM 分支带动。
func TestCoverFetchPCMBackfill(t *testing.T) {
	type tc struct {
		name        string
		cover       string
		unconfirmed bool
		singer      string // 宣告时的歌手
		byMID       []lyric.SongInfo
		albums      []string
		ops         []string // 不含 open 与末尾的 close
		infos       []string
	}
	s, a := "bymid(0x1234,"+psMID+")", "album(0x1234,"+psMID+")"
	cases := []tc{
		{"歌曲信息第 2 次查到，封面宣告时已有", pcCover, true, "",
			[]lyric.SongInfo{{}, pcCur}, nil,
			[]string{"sleep", s, "sleep", s, "download(" + pcCover + ")"},
			[]string{psWindow + "|柳爽|" + psWindow + "|" + pcCover + "|", psWindow + "|柳爽|" + psWindow + "|" + pcCover + "|B64"}},
		{"歌曲信息已确认，宣告时缺封面，第 2 次查到专辑", "", false, "柳爽",
			nil, []string{"", psAlbum},
			[]string{"sleep", a, "sleep", a, "download(" + pcCover + ")"},
			[]string{psWindow + "|柳爽|" + psWindow + "|" + pcCover + "|B64"}},
		{"封面先于歌曲信息查到", "", true, "",
			[]lyric.SongInfo{{}, {}, pcCur}, []string{psAlbum},
			[]string{"sleep", s, a, "sleep", s, "sleep", s, "download(" + pcCover + ")"},
			[]string{psWindow + "|柳爽|" + psWindow + "|" + pcCover + "|", psWindow + "|柳爽|" + psWindow + "|" + pcCover + "|B64"}},
		{"第 1 次就都查到", "", true, "",
			[]lyric.SongInfo{pcCur}, []string{psAlbum},
			[]string{"sleep", s, a, "download(" + pcCover + ")"},
			[]string{psWindow + "|柳爽|" + psWindow + "||", psWindow + "|柳爽|" + psWindow + "|" + pcCover + "|B64"}},
		{"5 次都查不到专辑：不发封面", "", false, "柳爽",
			nil, nil,
			[]string{"sleep", a, "sleep", a, "sleep", a, "sleep", a, "sleep", a},
			nil},
		{"歌曲信息一直查不到：5 次之后下载宣告时的封面", pcCover, true, "",
			nil, nil,
			[]string{"sleep", s, "sleep", s, "sleep", s, "sleep", s, "sleep", s, "download(" + pcCover + ")"},
			[]string{psWindow + "||" + psWindow + "|" + pcCover + "|B64"}},
		{"歌曲信息查到、专辑一直查不到：只发歌曲信息", "", true, "",
			[]lyric.SongInfo{pcCur}, nil,
			[]string{"sleep", s, a, "sleep", a, "sleep", a, "sleep", a, "sleep", a},
			[]string{psWindow + "|柳爽|" + psWindow + "||"}},
	}
	for _, c := range cases {
		p := newTestPlayer()
		gen := p.NewSongGen()
		l := &opLog{}
		ops := withAlbum(withByMID(pcmOps(l), l, c.byMID...), l, c.albums...)
		title := psWindow
		if c.singer != "" {
			title += " - " + c.singer
		}

		waitDone(t, p.startCoverFetch(ops, gen, testPID, psWindow, psMID, psWindow, c.singer, title, c.cover, true, c.unconfirmed))

		got := l.snapshot()
		want := append(append([]string{"open(4242)"}, c.ops...), "close(0x1234)")
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s：操作顺序不对：\n got %q\nwant %q", c.name, got, want)
		}
		noFallbackQueries(t, got)
		// 已确认的那一例宣告时带着歌手，回写的标题是宣告时的「歌名 - 歌手」。
		wantInfos := c.infos
		if c.singer != "" {
			wantInfos = nil
			for _, w := range c.infos {
				wantInfos = append(wantInfos, strings.Replace(w, "|"+psWindow+"|", "|"+title+"|", 1))
			}
		}
		if gotInfos := songInfos(t, p); fmt.Sprint(gotInfos) != fmt.Sprint(wantInfos) {
			t.Errorf("%s：回写不对：\n got %q\nwant %q", c.name, gotInfos, wantInfos)
		}
	}
}

// 已没有可补读的：歌曲信息已确认且封面地址已有，只下载、不开进程；路径 mid 为空时没有可补读的，不开进程（设计 §3.5）。
//
// 变异自证（实测）：是否补读不看封面（有路径 mid 就补读），「都已齐全」开了进程而红；不看路径 mid 是否为空，
// 「路径 mid 为空」两例开了进程、拿空串去查而红。
func TestCoverFetchPCMNothingToBackfill(t *testing.T) {
	cases := []struct {
		name, mid, cover string
		unconfirmed      bool
		ops              []string
		infos            int
	}{
		{"都已齐全", psMID, pcCover, false, []string{"download(" + pcCover + ")"}, 1},
		{"路径 mid 为空、没有封面", "", "", true, nil, 0},
		{"路径 mid 为空、封面已有", "", pcCover, true, []string{"download(" + pcCover + ")"}, 1},
	}
	for _, c := range cases {
		p := newTestPlayer()
		gen := p.NewSongGen()
		l := &opLog{}
		waitDone(t, p.startCoverFetch(pcmOps(l), gen, testPID, psWindow, c.mid, psWindow, "", psWindow, c.cover, true, c.unconfirmed))
		if got := l.snapshot(); fmt.Sprint(got) != fmt.Sprint(c.ops) {
			t.Errorf("%s：操作 %q，want %q", c.name, got, c.ops)
		}
		if got := songInfos(t, p); len(got) != c.infos {
			t.Errorf("%s：回写 %q，want %d 条", c.name, got, c.infos)
		}
	}
}

// 补读期间切歌（代次变化）：歌曲信息与封面的回写都丢弃，句柄照样关（设计 §4：切歌后迟到的结果被丢弃）。
func TestCoverFetchPCMInvalidated(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}
	ops := withAlbum(withByMID(pcmOps(l), l, pcCur), l, psAlbum)
	inner := ops.findByMID
	ops.findByMID = func(h syscall.Handle, mid string) (lyric.SongInfo, bool) {
		p.InvalidateSongGen() // 补读的这一下切了歌
		return inner(h, mid)
	}

	waitDone(t, p.startCoverFetch(ops, gen, testPID, psWindow, psMID, psWindow, "", psWindow, "", true, true))

	if got := songInfos(t, p); len(got) != 0 {
		t.Errorf("代次已作废，回写应被丢弃，实得 %q", got)
	}
	if ops := l.snapshot(); indexOf(ops, "close(0x1234)") != len(ops)-1 {
		t.Errorf("代次作废后仍应关闭自己的句柄，且是最后一步：%q", ops)
	}
}

// F1 的反例走完全程（设计 §4）：按路径 mid 查不到、按窗口歌名退回上一首 A 的完整记录。宣告不含 A 的歌名、
// 歌手、封面，歌名为窗口歌名、歌手为空、标为未确认；补读按路径 mid 查到之后补发这首的歌手。
func TestCoverFetchPCMBackfillAfterF1(t *testing.T) {
	q := fakePCM{rec: pcPrev, path: psMID, albums: map[string]string{psMID: psAlbum, psPrevMID: psPrevAlbum}}
	_, s, err := resolvePCM(psWindow, q.queries())
	if err != nil || s.confirmed || s.singer != "" || s.name != psWindow || s.cover != pcCover {
		t.Fatalf("宣告不对：%+v，err = %v", s, err)
	}

	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}
	ops := withByMID(pcmOps(l), l, lyric.SongInfo{}, pcCur)
	waitDone(t, p.startCoverFetch(ops, gen, testPID, psWindow, s.mid, s.name, s.singer, s.title, s.cover, true, !s.confirmed))

	got := songInfos(t, p)
	want := []string{psWindow + "|柳爽|" + psWindow + "|" + pcCover + "|", psWindow + "|柳爽|" + psWindow + "|" + pcCover + "|B64"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("回写 %q，want %q", got, want)
	}
	noFallbackQueries(t, l.snapshot())
	for _, g := range got {
		if strings.Contains(g, "莫文蔚") || strings.Contains(g, "这世界那么多人") {
			t.Errorf("回写带上了上一首的内容：%q", g)
		}
	}
}

// ===== 兜底模式的宣告（#146，设计 §3.4）=====

// songMeta 的各种情形（设计 §3.1、§3.2、§3.4、§4）：只用窗口歌名匹配上的记录，匹配不到（含查找出错）时宣告窗口
// 歌名、歌手与 mid 留空、不查专辑；记录有 mid 而没有歌手时按 mid 补歌手，查到的歌名相同才取；有 mid 就按它查专辑。
// 查找返回别的歌的记录在真实的 FindSongInfoPreferMID 下不会发生（#146 起不退回），songMeta 仍再核对一次歌名。
//
// 变异自证（实测）：
//   - 匹配不到时照旧用查找返回的记录（不核对歌名，#146 之前）：「查找返回了别的歌的记录」宣告了 B 的歌名、歌手、
//     拿 B 的 mid 查了专辑而红，「窗口歌名为空、查找返回了记录」同样红；
//   - 查找出错时用别的来源的歌名（如 #146 之前的进程窗口标题）：「R1」「窗口歌名为空」宣告的不是窗口歌名而红；
//   - 按 mid 补歌手不核对歌名：「按 mid 查到的歌名不同」取了别的歌的歌手而红；
//   - 不按 mid 补歌手：「按 mid 查到同名记录」没有歌手而红，「没有歌手」三例少了 bymid 而红；
//   - 有歌手也按 mid 查：「匹配上、专辑查到」「匹配上、专辑查不到」多出一次 bymid 而红；
//   - 宣告前不查专辑（封面留给补读）：「匹配上、专辑查到」与「按 mid 查到同名记录」「按 mid 查到的歌名不同」没有
//     封面而红，有 mid 的五例少了 album 而红；
//   - 查专辑与补歌手不看 mid 是否为空：「记录没有 mid」两例拿空串查了而红。
func TestSongMetaFallback(t *testing.T) {
	cur := lyric.SongInfo{Mid: testMID, Name: bfWindow, Singer: bfSinger}
	noSinger := lyric.SongInfo{Mid: testMID, Name: bfWindow}
	prev := lyric.SongInfo{Mid: bfPrevMID, Name: "七里香", Singer: "周杰伦（上一首）"}
	notFound := errors.New("Found 3 songname matches but none named")
	withSinger := bfWindow + " - " + bfSinger + "|" + bfWindow + "|" + bfSinger
	bare := bfWindow + "|" + bfWindow + "|"
	bymid, album := "bymid("+testMID+")", "album("+testMID+")"
	cases := []struct {
		name, window string
		rec          lyric.SongInfo
		err          error
		byMID        lyric.SongInfo // 按 mid 查到的记录，Name 为空即查不到
		album        string
		want         string // 标题|歌名|歌手|封面|mid
		calls        string
	}{
		{"匹配上、专辑查到", bfWindow, cur, nil, cur, testAlbumMID,
			withSinger + "|" + testCover + "|" + testMID, "[" + album + "]"},
		{"匹配上、专辑查不到", bfWindow, cur, nil, cur, "",
			withSinger + "||" + testMID, "[" + album + "]"},
		{"匹配上、没有歌手，按 mid 查到同名记录：取它的歌手", bfWindow, noSinger, nil, cur, testAlbumMID,
			withSinger + "|" + testCover + "|" + testMID, "[" + bymid + " " + album + "]"},
		{"匹配上、没有歌手，按 mid 查到的歌名不同：不补", bfWindow, noSinger, nil,
			lyric.SongInfo{Mid: testMID, Name: "七里香", Singer: "周杰伦（上一首）"}, testAlbumMID,
			bare + "|" + testCover + "|" + testMID, "[" + bymid + " " + album + "]"},
		{"匹配上、没有歌手，按 mid 查不到：不补", bfWindow, noSinger, nil, lyric.SongInfo{}, "",
			bare + "||" + testMID, "[" + bymid + " " + album + "]"},
		{"记录没有 mid、有歌手：不查专辑", bfWindow, lyric.SongInfo{Name: bfWindow, Singer: bfSinger}, nil, cur, testAlbumMID,
			withSinger + "||", "[]"},
		{"记录没有 mid、也没有歌手：不按 mid 查", bfWindow, lyric.SongInfo{Name: bfWindow}, nil, cur, testAlbumMID,
			bare + "||", "[]"},
		{"R1：唯一的记录是上一首，查找返回未找到", bfWindow, lyric.SongInfo{}, notFound, cur, testAlbumMID,
			bare + "||", "[]"},
		{"查找返回了别的歌的记录：不认", bfWindow, prev, nil, prev, testAlbumMID,
			bare + "||", "[]"},
		{"窗口歌名为空", "", lyric.SongInfo{}, notFound, cur, testAlbumMID,
			"||||", "[]"},
		{"窗口歌名为空、查找返回了记录：不认", "", cur, nil, cur, testAlbumMID,
			"||||", "[]"},
	}
	for _, c := range cases {
		var calls []string
		title, name, singer, cover, mid := songMeta(c.window,
			func(string) (lyric.SongInfo, error) { return c.rec, c.err },
			func(m string) (lyric.SongInfo, bool) {
				calls = append(calls, "bymid("+m+")")
				return c.byMID, c.byMID.Name != ""
			},
			func(m string) string { calls = append(calls, "album("+m+")"); return c.album })
		if got := strings.Join([]string{title, name, singer, cover, mid}, "|"); got != c.want {
			t.Errorf("%s：宣告 %q，want %q", c.name, got, c.want)
		}
		if got := fmt.Sprint(calls); got != c.calls {
			t.Errorf("%s：查询 %s，want %s", c.name, got, c.calls)
		}
	}
}
