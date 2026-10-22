package wesing

// 本文件钉住封面 goroutine 的句柄归属（issue #117）：内存重试用的句柄由 goroutine 自己另开、自己关，
// 不借会话的句柄；会话结束时 Start 关掉会话句柄，不能影响还在途中的内存扫描。

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"
)

const (
	testSessionHandle = syscall.Handle(0x99)   // 会话的句柄：封面 goroutine 不得碰它
	testOwnHandle     = syscall.Handle(0x1234) // openProc 给封面 goroutine 的句柄
	testPID           = uint32(4242)
	testMID           = "003msPI80HftbW"
	testFoundURL      = "http://imgcache.qq.com/music/photo/mid_album_500/a/b/albumid.jpg"
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

// fakeOps 造一组假操作。findAfter 是第几次 findURL 才找到封面地址（0 = 一直找不到）；
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
		findURL: func(h syscall.Handle, mid string) string {
			l.add("find(%#x,%s)", uintptr(h), mid)
			finds++
			if findAfter > 0 && finds >= findAfter {
				return testFoundURL
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
// 且只关一次；回写带上换成 800 档的地址与 base64。
//
// 变异自证（均实测，下面几条用例合起来）：删掉 defer closeProc 即红；把关闭挪到重试循环之前即红；
// 有地址也开句柄即红；打开失败后仍扫描即红；不另开句柄（拿 pid 充当句柄）即红。
func TestCoverFetchOwnsItsHandle(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}

	waitDone(t, p.startCoverFetch(fakeOps(l, 2, nil, nil), gen, testPID, testMID, "name", "singer", "title", ""))

	ops := l.snapshot()
	want := []string{
		"open(4242)",
		"sleep", "find(0x1234," + testMID + ")",
		"sleep", "find(0x1234," + testMID + ")",
		"download(http://imgcache.qq.com/music/photo/mid_album_800/a/b/albumid.jpg)",
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
	if info.CoverBase64 != "B64" || info.Cover != "http://imgcache.qq.com/music/photo/mid_album_800/a/b/albumid.jpg" ||
		info.Name != "name" || info.Singer != "singer" || info.Title != "title" {
		t.Errorf("回写内容不对：%+v", info)
	}
}

// 封面地址一开始就有：全程不碰进程，不开也不关句柄。
func TestCoverFetchWithURLNeverOpensProcess(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}

	waitDone(t, p.startCoverFetch(fakeOps(l, 1, nil, nil), gen, testPID, testMID, "n", "s", "t", "http://x/y.jpg"))

	ops := l.snapshot()
	if fmt.Sprint(ops) != fmt.Sprint([]string{"download(http://x/y.jpg)"}) {
		t.Errorf("有地址时只应下载，实得 %q", ops)
	}
	if evts := drainEvents(p); len(evts) != 1 {
		t.Errorf("应回写一条，实得 %d", len(evts))
	}
}

// 没有歌曲 MID 也不需要重试：同上，不开句柄。
func TestCoverFetchWithoutMIDNeverOpensProcess(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}

	waitDone(t, p.startCoverFetch(fakeOps(l, 1, nil, nil), gen, testPID, "", "n", "s", "t", ""))

	if ops := l.snapshot(); len(ops) != 0 {
		t.Errorf("既没地址也没 MID，不该有任何操作，实得 %q", ops)
	}
	if evts := drainEvents(p); len(evts) != 0 {
		t.Errorf("不该回写，实得 %+v", evts)
	}
}

// 打不开进程：跳过内存重试，不扫描、不下载、不回写，也不关一个没打开的句柄。
func TestCoverFetchOpenFailureSkipsScan(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}

	waitDone(t, p.startCoverFetch(fakeOps(l, 1, errors.New("denied"), nil), gen, testPID, testMID, "n", "s", "t", ""))

	if ops := l.snapshot(); fmt.Sprint(ops) != fmt.Sprint([]string{"open(4242)"}) {
		t.Errorf("打开失败后只应有 open，实得 %q", ops)
	}
	if evts := drainEvents(p); len(evts) != 0 {
		t.Errorf("不该回写，实得 %+v", evts)
	}
}

// 五次都找不到：扫描五次，关闭一次，不下载、不回写。
func TestCoverFetchNeverFoundClosesOnce(t *testing.T) {
	p := newTestPlayer()
	gen := p.NewSongGen()
	l := &opLog{}

	waitDone(t, p.startCoverFetch(fakeOps(l, 0, nil, nil), gen, testPID, testMID, "n", "s", "t", ""))

	ops := l.snapshot()
	finds, closes := 0, 0
	for _, o := range ops {
		switch {
		case len(o) >= 4 && o[:4] == "find":
			finds++
		case len(o) >= 5 && o[:5] == "close":
			closes++
		}
	}
	if finds != 5 || closes != 1 {
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

	waitDone(t, p.startCoverFetch(fakeOps(l, 1, nil, p.InvalidateSongGen), gen, testPID, testMID, "n", "s", "t", ""))

	ops := l.snapshot()
	if indexOf(ops, "close(0x1234)") < 0 {
		t.Errorf("代次作废后仍应关闭自己的句柄：%q", ops)
	}
	if evts := drainEvents(p); len(evts) != 0 {
		t.Errorf("代次已作废，回写应被丢弃，实得 %+v", evts)
	}
}
