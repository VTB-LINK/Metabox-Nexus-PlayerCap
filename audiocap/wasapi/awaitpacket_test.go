//go:build windows

// 本文件只测 awaitPacket 的等待判定：何时阻塞等采集事件、何时直接取包，以及吞信号、等待、查队列三者的
// 先后。假引擎按 auto-reset 事件的语义记账（置位不计数，等到即复位），用调用日志而非只用返回值断言。
// Capturer.Read 里的 COM 调用本身只能真机验。
//
// 变异自证（隔离副本，实测）：吞信号挪到首次查询之后 → 「查询与等待之间到包」等用例红；改回「总是先等
// 事件再查」→ 积压与陈旧信号等用例红；等待改成可重复 → 假引擎报第二次等待，红；首次查询出错仍去等待
// → 首次查询出错用例红。

package wasapi

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

// fakeEngine 模拟采集队列与 auto-reset 采集事件。
type fakeEngine struct {
	t        *testing.T
	signaled bool          // 事件当前是否有信号
	queue    []uint32      // 队列里各 packet 的帧数，size 返回队首
	arrive   []uint32      // 阻塞等待期间到达的 packet；为空则等待超时
	lateAt   int           // 第 lateAt 次 size 查询返回之后立刻到一个 480 帧的 packet；0 为不到
	sizeErr  map[int]error // 第 i 次（从 1 起）size 查询返回的错误
	sizes    int
	waits    int
	log      []string
}

// push 模拟引擎写入一个 packet 并置位事件。
func (f *fakeEngine) push(n uint32) {
	f.queue = append(f.queue, n)
	f.signaled = true
}

func (f *fakeEngine) reset() {
	f.log = append(f.log, "reset")
	f.signaled = false
}

func (f *fakeEngine) wait() {
	if f.waits++; f.waits > 1 {
		f.t.Fatalf("单次调用里阻塞等待了第 %d 次，单次 100ms 的阻塞上限被突破；日志 %v", f.waits, f.log)
	}
	switch {
	case f.signaled:
		f.log = append(f.log, "wait:signaled")
	case len(f.arrive) > 0:
		for _, n := range f.arrive {
			f.push(n)
		}
		f.arrive = nil
		f.log = append(f.log, "wait:woken")
	default:
		f.log = append(f.log, "wait:timeout")
		return
	}
	f.signaled = false // 等到即复位
}

func (f *fakeEngine) size() (uint32, error) {
	f.sizes++
	f.log = append(f.log, "size")
	if err := f.sizeErr[f.sizes]; err != nil {
		return 0, err
	}
	var n uint32
	if len(f.queue) > 0 {
		n = f.queue[0]
	}
	if f.sizes == f.lateAt {
		f.push(480)
	}
	return n, nil
}

// call 跑一次 awaitPacket；取到包时按 Read 随后的 GetBuffer/ReleaseBuffer 把它移出队列。
// 返回本次调用的日志，计数清零以便连续调用。
func (f *fakeEngine) call() (uint32, []string, error) {
	f.log, f.sizes, f.waits = nil, 0, 0
	n, err := awaitPacket(f.reset, f.wait, f.size)
	if err == nil && n > 0 {
		f.queue = f.queue[1:]
	}
	return n, f.log, err
}

func expectLog(t *testing.T, step string, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: 调用序列 %v, 期望 %v", step, got, want)
	}
}

// 队列有积压时直接取，不阻塞等待。
func TestAwaitPacketBacklogSkipsWait(t *testing.T) {
	f := &fakeEngine{t: t, queue: []uint32{480, 480, 480}, signaled: true}
	n, log, err := f.call()
	if n != 480 || err != nil {
		t.Fatalf("得 (%d, %v), 期望 (480, nil)", n, err)
	}
	expectLog(t, "有积压", log, "reset", "size")
}

// 积压逐个消化：队列空之前每次都不等；队列空后的那一次才等一次。
func TestAwaitPacketDrainsBacklogThenWaitsOnce(t *testing.T) {
	backlog := []uint32{480, 160, 480, 320}
	f := &fakeEngine{t: t, queue: slices.Clone(backlog), signaled: true}
	for i, want := range backlog {
		n, log, err := f.call()
		if n != want || err != nil {
			t.Fatalf("第 %d 次: 得 (%d, %v), 期望 (%d, nil)", i+1, n, err, want)
		}
		expectLog(t, "消化积压", log, "reset", "size")
	}
	n, log, err := f.call()
	if n != 0 || err != nil {
		t.Fatalf("队列空: 得 (%d, %v), 期望 (0, nil)", n, err)
	}
	expectLog(t, "队列空", log, "reset", "size", "wait:timeout", "size")
}

// 队列已空而事件留有陈旧信号（它对应的包已被跳过等待的那几次调用取走）：这次等待不能被它立刻
// 唤醒成假空读，而要等到真正的下一个 packet。
func TestAwaitPacketStaleSignalDoesNotFakeEmptyRead(t *testing.T) {
	f := &fakeEngine{t: t, signaled: true, arrive: []uint32{240}}
	n, log, err := f.call()
	if n != 240 || err != nil {
		t.Fatalf("得 (%d, %v), 期望 (240, nil)：陈旧信号造成了假空读；日志 %v", n, err, log)
	}
	expectLog(t, "陈旧信号", log, "reset", "size", "wait:woken", "size")
}

// 吞信号必须在首次查询之前：查询之后、等待之前到的包，其信号要留给等待，不能被吞掉而白等一个超时。
func TestAwaitPacketPacketBetweenSizeAndWaitNotDelayed(t *testing.T) {
	f := &fakeEngine{t: t, lateAt: 1}
	n, log, err := f.call()
	if n != 480 || err != nil {
		t.Fatalf("得 (%d, %v), 期望 (480, nil)", n, err)
	}
	expectLog(t, "查询后到包", log, "reset", "size", "wait:signaled", "size")
}

// 队列空、等待期间到包：恰好等一次，随后再查一次，返回新包。
func TestAwaitPacketWaitThenPacket(t *testing.T) {
	f := &fakeEngine{t: t, arrive: []uint32{480}}
	n, log, err := f.call()
	if n != 480 || err != nil {
		t.Fatalf("得 (%d, %v), 期望 (480, nil)", n, err)
	}
	expectLog(t, "等到包", log, "reset", "size", "wait:woken", "size")
}

// 队列空、等待超时：恰好等一次，返回 0、无错误。
func TestAwaitPacketWaitTimeout(t *testing.T) {
	f := &fakeEngine{t: t}
	n, log, err := f.call()
	if n != 0 || err != nil {
		t.Fatalf("得 (%d, %v), 期望 (0, nil)", n, err)
	}
	expectLog(t, "超时", log, "reset", "size", "wait:timeout", "size")
}

// 被事件唤醒但队列仍空（等待期间到的是空包）：不再等第二次，返回 0。
func TestAwaitPacketWokenButEmpty(t *testing.T) {
	f := &fakeEngine{t: t, arrive: []uint32{0}}
	n, log, err := f.call()
	if n != 0 || err != nil {
		t.Fatalf("得 (%d, %v), 期望 (0, nil)", n, err)
	}
	expectLog(t, "唤醒仍空", log, "reset", "size", "wait:woken", "size")
}

var errSize = errors.New("GetNextPacketSize 失败")

// 首次查询出错：返回错误，不再等待。
func TestAwaitPacketFirstSizeError(t *testing.T) {
	f := &fakeEngine{t: t, arrive: []uint32{480}, sizeErr: map[int]error{1: errSize}}
	_, log, err := f.call()
	if !errors.Is(err, errSize) {
		t.Fatalf("得错误 %v, 期望 %v", err, errSize)
	}
	expectLog(t, "首次查询出错", log, "reset", "size")
}

// 等待之后的查询出错：返回错误。
func TestAwaitPacketSecondSizeError(t *testing.T) {
	f := &fakeEngine{t: t, arrive: []uint32{480}, sizeErr: map[int]error{2: errSize}}
	_, log, err := f.call()
	if !errors.Is(err, errSize) {
		t.Fatalf("得错误 %v, 期望 %v", err, errSize)
	}
	expectLog(t, "等待后查询出错", log, "reset", "size", "wait:woken", "size")
}

// 无论队列、事件与查询怎样组合，单次调用至多等一次、至多查两次。等第二次时假引擎直接判红。
func TestAwaitPacketWaitsAtMostOnce(t *testing.T) {
	for _, signaled := range []bool{false, true} {
		for _, arrive := range [][]uint32{nil, {480}, {0}} {
			for _, lateAt := range []int{0, 1, 2} {
				for _, errAt := range []int{0, 1, 2} {
					f := &fakeEngine{t: t, signaled: signaled, arrive: slices.Clone(arrive), lateAt: lateAt}
					if errAt > 0 {
						f.sizeErr = map[int]error{errAt: errSize}
					}
					f.call()
					if f.sizes > 2 {
						t.Errorf("signaled=%v arrive=%v lateAt=%d errAt=%d: 查询 %d 次；日志 %v",
							signaled, arrive, lateAt, errAt, f.sizes, f.log)
					}
				}
			}
		}
	}
}
