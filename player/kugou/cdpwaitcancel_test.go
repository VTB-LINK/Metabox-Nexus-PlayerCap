package kugou

// 本文件只测 waitForCDP 交给连接的 ctx：停止与 90 s 的期限都能中止一次挂住的连接（#231 代码审：酷狗的调试端口接受连接而
// 不完成响应时，轮询 goroutine 不得被永久挡住）。连接、门禁与判进程都是桩，在 synctest 的泡里跑，等待是假时钟。

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"Metabox-Nexus-PlayerCap/player/kugou/cdp"
)

// TestWaitForCDPCancelsHungConnect：连接的桩一直挂到 ctx 结束。停止时当即返回；不停止时 90 s 期限到即中止这次连接，
// 下一轮判出超时返回。酷狗始终在运行。
//
// 变异自证（均实测变红）：连接改传 context.Background()（「停止」一例挂住，泡内死锁）；停止时不取消 ctx（「停止」一例
// 到 90 s 才返回）。
func TestWaitForCDPCancelsHungConnect(t *testing.T) {
	og, oc, or := gateRunning, connectCDP, isKuGouRunning
	defer func() { gateRunning, connectCDP, isKuGouRunning = og, oc, or }()

	cases := []struct {
		name     string
		stopAt   time.Duration // 0 即不停止
		returnAt time.Duration
		connects int
	}{
		{"停止", 3 * time.Second, 3 * time.Second, 1},
		{"期限", 0, 92 * time.Second, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := New(0, 0)
				gateRunning = func() error { return nil }
				isKuGouRunning = func() bool { return true }
				connects := 0
				connectCDP = func(ctx context.Context) (*cdp.Client, error) {
					connects++
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if c.stopAt > 0 {
					go func() { time.Sleep(c.stopAt); p.Stop() }()
				}
				t0 := time.Now()
				client, err := p.waitForCDP()
				if el := time.Since(t0); client != nil || err == nil || el != c.returnAt || connects != c.connects {
					t.Errorf("waitForCDP = (%v, %v) 于 %v、连接 %d 次，期望返回错误于 %v、连接 %d 次", client, err, el, connects, c.returnAt, c.connects)
				}
			})
		})
	}
}
