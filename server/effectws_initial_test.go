package server

// 本文件只测 effect-ws 连接建立时「初始状态」的发送：不得在消费者（写 goroutine）启动前
// 向已公开的订阅队列做阻塞发送（issue #123）；初始状态本身仍须先于帧送达。

import (
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestEffectWSInitialSendDoesNotWedgeHandler 钉死：订阅者登记之后、队列被广播填满的情况下，
// 连接关闭仍能走到注销路径。
//
// 缺陷背景：handleEffectWS 先把订阅者登记进 h.subs，再同步 `sub.ch <- 初始状态`，而写 goroutine
// 在这之后才启动。若登记与发送之间广播把容量为 8 的队列填满，这次发送永久阻塞——handler 进不了
// 读循环，客户端关连接也不能解除，订阅者永远留在 h.subs（HasEffectSubscribers 恒真，捕获器
// 持续供帧；effect_ws_connected 持续显示已断开的地址）。
//
// 复现手法：测试线程先持有 s.mu 写锁，让 handler 卡在 effectStatusJSON 的 RLock 上（此时订阅者
// 已登记），期间广播 16 帧把队列填满，再释放锁并关闭客户端。
//
// 变异自证：把初始状态的发送挪回写 goroutine 启动之前即红。select 里的 done 分支防的是写
// goroutine 已因写失败退出后再次阻塞，需要写失败与队列填满的精确时序，不做确定性复现。
func TestEffectWSInitialSendDoesNotWedgeHandler(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	srv := effectSrv(t, s)

	s.mu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			s.mu.Unlock()
		}
	}()

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("WS 拨号失败: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	waitFor(t, 3*time.Second, s.HasEffectSubscribers, "订阅者未登记")

	for i := 0; i < 16; i++ {
		s.BroadcastEffectFrame([]byte{0xFF, 0xD8, byte(i)})
	}
	s.mu.Unlock()
	unlocked = true
	c.Close()

	waitFor(t, 3*time.Second, func() bool { return !s.HasEffectSubscribers() },
		"关闭客户端后订阅者仍在册：初始状态的发送卡死了 handler")
}

// TestEffectWSInitialStatusArrivesFirst 钉死反方向：把初始状态后移不能让它丢失或排到帧之后。
// 无并发广播时，客户端收到的第一条消息必须是文本的 status。
func TestEffectWSInitialStatusArrivesFirst(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	srv := effectSrv(t, s)

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("WS 拨号失败: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	mt, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("读不到初始状态: %v", err)
	}
	if mt != websocket.TextMessage || !strings.Contains(string(data), `"type":"status"`) {
		t.Errorf("第一条消息 = (%d) %q，期望文本 status", mt, data)
	}
}
