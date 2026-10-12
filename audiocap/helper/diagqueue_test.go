package helper

// 本文件只测采集侧诊断日志的非阻塞投递：sendDiag 满或无人接收即丢、返回 false、永不阻塞，diagLoop 的
// 写端阻塞不传导到投递方、关闭后排空再返回。会挂住的调用一律放进 goroutine 限时等待，挂住即判失败。

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

const diagTimeout = time.Second

// finishes 在 goroutine 里跑 f，至多等 diagTimeout，返回 f 是否已返回。
func finishes(f func()) bool {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f()
	}()
	return waitTimeout(&wg, diagTimeout)
}

// drain 非阻塞取出 ch 中已有的全部消息。
func drain(ch chan string) []string {
	var got []string
	for {
		select {
		case m := <-ch:
			got = append(got, m)
		default:
			return got
		}
	}
}

func TestSendDiagUnbufferedNoReceiverReturns(t *testing.T) {
	ch := make(chan string)
	var ok bool
	if !finishes(func() { ok = sendDiag(ch, "x") }) {
		<-ch // 放行挂住的发送，不留泄漏的 goroutine
		t.Fatalf("无接收者的无缓冲 channel 上 sendDiag 在 %v 内未返回", diagTimeout)
	}
	if ok {
		t.Fatalf("无接收者的无缓冲 channel 上 sendDiag 返回 true, 期望 false")
	}
}

func TestSendDiagFullDropsNewest(t *testing.T) {
	ch := make(chan string, 2)
	ch <- "a"
	ch <- "b"
	var ok bool
	if !finishes(func() { ok = sendDiag(ch, "c") }) {
		drain(ch)
		t.Fatalf("满的 channel 上 sendDiag 在 %v 内未返回", diagTimeout)
	}
	if ok {
		t.Fatalf("满的 channel 上 sendDiag 返回 true, 期望 false")
	}
	if got, want := drain(ch), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("队列 = %q, 期望 %q（丢本条、不丢已有的）", got, want)
	}
}

func TestSendDiagWithRoomDelivers(t *testing.T) {
	ch := make(chan string, 2)
	ch <- "a"
	var ok bool
	if !finishes(func() { ok = sendDiag(ch, "b") }) {
		t.Fatalf("有空位的 channel 上 sendDiag 在 %v 内未返回", diagTimeout)
	}
	if !ok {
		t.Fatalf("有空位的 channel 上 sendDiag 返回 false, 期望 true")
	}
	if got, want := drain(ch), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("队列 = %q, 期望 %q", got, want)
	}
}

// TestDiagLoopBlockedOutDoesNotBlockSender 让 out 阻塞在第一条上，投递方连发远多于容量的条数仍不阻塞；
// 放行后收到的是在途的一条加队列里的 diagChanCap 条，按投递顺序。
func TestDiagLoopBlockedOutDoesNotBlockSender(t *testing.T) {
	ch := make(chan string, diagChanCap)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	var got []string // 只由 diagLoop 所在 goroutine 写，loopDone 之后才读
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		diagLoop(ch, func(s string) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-gate
			got = append(got, s)
		})
	}()

	sendDiag(ch, "m0")
	select {
	case <-entered:
	case <-time.After(diagTimeout):
		close(gate)
		t.Fatalf("diagLoop 在 %v 内未取走第一条", diagTimeout)
	}

	const extra = 10 * diagChanCap
	if !finishes(func() {
		for i := 1; i <= extra; i++ {
			sendDiag(ch, fmt.Sprintf("m%d", i))
		}
	}) {
		close(gate)
		t.Fatalf("out 阻塞时连发 %d 条在 %v 内未返回", extra, diagTimeout)
	}

	close(gate)
	close(ch)
	select {
	case <-loopDone:
	case <-time.After(diagTimeout):
		t.Fatalf("放行并关闭后 diagLoop 在 %v 内未返回", diagTimeout)
	}
	want := []string{"m0"}
	for i := 1; i <= diagChanCap; i++ {
		want = append(want, fmt.Sprintf("m%d", i))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("收到 %q, 期望 %q（在途 1 条 + 队列 %d 条，其余丢弃）", got, want, diagChanCap)
	}
}

func TestDiagLoopDrainsAfterClose(t *testing.T) {
	full := make([]string, diagChanCap)
	for i := range full {
		full[i] = fmt.Sprintf("m%d", i)
	}
	for _, c := range []struct {
		name string
		msgs []string
	}{
		{"关闭时队列为空", nil},
		{"关闭时队列有一条", []string{"a"}},
		{"关闭时队列已满", full},
	} {
		t.Run(c.name, func(t *testing.T) {
			ch := make(chan string, diagChanCap)
			for _, m := range c.msgs {
				ch <- m
			}
			close(ch)
			var got []string
			if !finishes(func() { diagLoop(ch, func(s string) { got = append(got, s) }) }) {
				t.Fatalf("ch 关闭后 diagLoop 在 %v 内未返回", diagTimeout)
			}
			if !reflect.DeepEqual(got, c.msgs) {
				t.Fatalf("收到 %q, 期望 %q", got, c.msgs)
			}
		})
	}
}
