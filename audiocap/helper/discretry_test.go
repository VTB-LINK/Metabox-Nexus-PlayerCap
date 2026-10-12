package helper

// 本文件只测数据不连续日志投递失败后的补报：discLog.report 经 sendDiag 投递，诊断队列满或无人接收时
// 次数退回待报，由之后到期的 observe 或结束时的 flush 再报（用假时刻与由测试放行的日志写端，不触碰声卡）。

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// recordingSend 返回一个经 sendDiag 投递到 ch 的 send，并把每次调用的次数与结果记进 calls。
func recordingSend(ch chan<- string, calls *[]string) func(uint64) bool {
	return func(n uint64) bool {
		ok := sendDiag(ch, fmt.Sprintf("disc %d", n))
		*calls = append(*calls, fmt.Sprintf("%d:%v", n, ok))
		return ok
	}
}

// TestDiscReportRetriesAfterQueueFull：日志写端阻塞、队列占满时首次出现的 1 次投递失败；写端放行、队列
// 腾空后，这 1 次由到期的下一次 observe 或结束时的 flush 补报，最终写出。
func TestDiscReportRetriesAfterQueueFull(t *testing.T) {
	for _, c := range []struct {
		name  string
		after func(t *testing.T, d *discLog, send func(uint64) bool, calls *[]string)
	}{
		{"到期的下一次 observe 补报", func(t *testing.T, d *discLog, send func(uint64) bool, calls *[]string) {
			d.report(d.observe(1, discStart.Add(time.Second)), send)
			if len(*calls) != 1 {
				t.Fatalf("窗口内不应重试, 投递记录 %q", *calls)
			}
			d.report(d.observe(1, discStart.Add(discLogInterval)), send)
		}},
		{"之后没有新增，结束时 flush 补报", func(t *testing.T, d *discLog, send func(uint64) bool, calls *[]string) {
			d.report(d.flush(), send)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ch := make(chan string, diagChanCap)
			gate := make(chan struct{})
			release := sync.OnceFunc(func() { close(gate) })
			t.Cleanup(release)
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

			// 占满：一条在途（out 阻塞在它上面）加队列 diagChanCap 条。
			sendDiag(ch, "busy")
			select {
			case <-entered:
			case <-time.After(diagTimeout):
				t.Fatalf("diagLoop 在 %v 内未取走第一条", diagTimeout)
			}
			want := []string{"busy"}
			for i := range diagChanCap {
				m := fmt.Sprintf("other%d", i)
				if !sendDiag(ch, m) {
					t.Fatalf("第 %d 条占位投递失败", i)
				}
				want = append(want, m)
			}

			var d discLog
			var calls []string
			send := recordingSend(ch, &calls)
			d.report(d.observe(1, discStart), send) // 首次出现即报，队列满，投递失败
			if !reflect.DeepEqual(calls, []string{"1:false"}) {
				t.Fatalf("投递记录 %q, 期望 [1:false]", calls)
			}

			release()
			deadline := time.Now().Add(diagTimeout)
			for len(ch) > 0 {
				if time.Now().After(deadline) {
					t.Fatalf("放行后队列在 %v 内未腾空", diagTimeout)
				}
				time.Sleep(time.Millisecond)
			}

			c.after(t, &d, send, &calls)
			if !reflect.DeepEqual(calls, []string{"1:false", "1:true"}) {
				t.Fatalf("投递记录 %q, 期望 [1:false 1:true]", calls)
			}
			close(ch)
			select {
			case <-loopDone:
			case <-time.After(diagTimeout):
				t.Fatalf("关闭后 diagLoop 在 %v 内未返回", diagTimeout)
			}
			if want = append(want, "disc 1"); !reflect.DeepEqual(got, want) {
				t.Fatalf("写出 %q, 期望 %q", got, want)
			}
			if n := d.flush(); n != 0 {
				t.Fatalf("补报成功后 flush = %d, 期望 0", n)
			}
		})
	}
}

// TestDiscReportFailureKeepsCount：无人接收时每次投递都失败，次数一直留在待报里、不重复计；窗口内不重试；
// 投递成功的不退回。捕获结束时的 flush 投递失败后 captureLoop 不再重试，这些次数不会写出。
func TestDiscReportFailureKeepsCount(t *testing.T) {
	ch := make(chan string)
	var d discLog
	var calls []string
	send := recordingSend(ch, &calls)

	d.report(d.observe(2, discStart), send)
	d.report(d.observe(3, discStart.Add(time.Second)), send) // 窗口内：不重试，新增并入待报
	d.report(d.flush(), send)
	if want := []string{"2:false", "3:false"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("投递记录 %q, 期望 %q", calls, want)
	}
	if n := d.flush(); n != 3 {
		t.Fatalf("两次失败后 flush = %d, 期望 3", n)
	}

	room := make(chan string, 1)
	var e discLog
	e.report(e.observe(1, discStart), recordingSend(room, &calls))
	if n := e.flush(); n != 0 {
		t.Fatalf("投递成功后 flush = %d, 期望 0", n)
	}
}
