package server

// 本文件只测一个失败模式：音频 WS 写 goroutine 的 panic 被 audioRecover 恢复后，订阅者仍在册。
// 读循环永久阻塞（overlay 从不发消息），订阅留在 hub，持续维持捕获，直到客户端断开。正反两向都钉住：
// panic 后必须注销，写成功不得注销（防「每帧写完就关连接」的假修复）。

import (
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// panicConn 在 armed 置位后的任何 Write 上 panic。握手响应在置位前写出，不受影响。
type panicConn struct {
	net.Conn
	armed *atomic.Bool
}

func (c panicConn) Write(p []byte) (int, error) {
	if c.armed.Load() {
		panic("测试注入的写 panic")
	}
	return c.Conn.Write(p)
}

// panicListener 把每个接入连接包成 panicConn。Upgrade 经 Hijack 取回的正是它。
type panicListener struct {
	net.Listener
	armed *atomic.Bool
}

func (l panicListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return panicConn{c, l.armed}, nil
}

// TestAudioWSWriterPanicUnregistersSubscriber 钉死：写 goroutine panic 被恢复后必须关连接，
// 使读循环返回、走既有清理路径注销订阅。变异自证：删掉写 goroutine 的 defer conn.Close() 即挂。
func TestAudioWSWriterPanicUnregistersSubscriber(t *testing.T) {
	s := NewServer([]string{"kugou"})
	var armed atomic.Bool
	srv := httptest.NewUnstartedServer(s.handleAudioWS("kugou"))
	srv.Listener = panicListener{srv.Listener, &armed}
	srv.Start()
	t.Cleanup(srv.Close)

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("WS 拨号失败: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	waitFor(t, 3*time.Second, s.audioHub.AnySubscribers, "订阅者未登记")

	armed.Store(true)
	s.audioHub.BroadcastAudioFrame("kugou", []byte("frame"))

	waitFor(t, 5*time.Second, func() bool { return !s.audioHub.AnySubscribers() },
		"写 goroutine panic 被恢复后订阅者仍在册——连接未关、读循环永久阻塞、订阅持续维持捕获")
}

// TestAudioWSHealthyFrameKeepsSubscriber 钉死反方向：写入成功不得关连接、不得注销订阅。
func TestAudioWSHealthyFrameKeepsSubscriber(t *testing.T) {
	s := NewServer([]string{"kugou"})
	srv := httptest.NewServer(s.handleAudioWS("kugou"))
	t.Cleanup(srv.Close)

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("WS 拨号失败: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	waitFor(t, 3*time.Second, s.audioHub.AnySubscribers, "订阅者未登记")

	s.audioHub.BroadcastAudioFrame("kugou", []byte("frame"))

	// 读到它——证明帧确实送达，而不是靠「没断开」蒙混过关
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, got, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("读取帧失败（连接被错误地关闭了？）: %v", err)
	}
	if typ != websocket.BinaryMessage || string(got) != "frame" {
		t.Fatalf("收到 type=%d payload=%q, 期望二进制 %q", typ, got, "frame")
	}

	// 送达后订阅者必须仍在册
	time.Sleep(200 * time.Millisecond)
	if !s.audioHub.AnySubscribers() {
		t.Fatal("正常写入后订阅者被注销了——修复过度，正常客户端会被踢下线")
	}
}
