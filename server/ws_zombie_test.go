package server

// 本文件只测一个失败模式：WS 写失败后订阅者僵死却仍在册。正反两向都钉住——写失败必须
// 注销（防僵尸），写成功不得注销（防「无脑关连接」的假修复）。

import (
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player"

	"github.com/gorilla/websocket"
)

// dialWS 起一个只挂 handleWS(playerName) 的 httptest 服务并连上去，返回客户端连接。
// 客户端故意不读不写——这正是 OBS overlay 的行为：它只等推送，从不主动发消息。
func dialWS(t *testing.T, s *Server, playerName string) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(s.handleWS(playerName))
	t.Cleanup(srv.Close)

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("WS 拨号失败: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// waitFor 轮询 cond 直到为真或超时。
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s（等待 %v 超时）", msg, d)
}

// TestWSWriteFailureUnregistersSubscriber 钉死：写入 goroutine 遇错必须关连接。
//
// **为什么这条不能只 return**：写失败不蕴含连接已断。gorilla 的 WriteJSON 先
// NextWriter 再 json.Encode，NaN/Inf 在编码阶段就失败，此时一个字节都没上过网线、
// TCP 完全健康。若写 goroutine 只 return，handleWS 的读循环会永远阻塞在 ReadMessage
// （overlay 从不发消息），于是清理路径永不执行：sub 永远留在 s.subscribers 里，
// wsCount() 仍报它在线，而它再也收不到任何事件——一个在册的僵尸。
//
// 本测试用 NaN 制造「连接健康但写失败」，是该场景确定性最高的触发方式（写超时同理，
// 但依赖 5s 墙钟）。变异自证：删掉 handleWS 写 goroutine 里的 conn.Close() 即挂。
func TestWSWriteFailureUnregistersSubscriber(t *testing.T) {
	s := NewServer([]string{"wesing"})
	s.SetActivePlayer("wesing")

	dialWS(t, s, "wesing")
	waitFor(t, 3*time.Second, func() bool { return s.wsCount() == 1 }, "订阅者未登记")

	// json.Marshal 无法编码 NaN → WriteJSON 返回 UnsupportedValueError，而连接完好
	s.NotifySubscribers(player.Event{
		PlayerName: "wesing",
		Type:       player.EventLyricUpdate,
		Data:       math.NaN(),
	}, false)

	waitFor(t, 5*time.Second, func() bool { return s.wsCount() == 0 },
		"写失败后订阅者仍在册——僵尸：连接不关、读循环永久阻塞、清理路径永不执行")
}

// TestWSHealthyEventKeepsSubscriber 钉死反方向：写入成功**不得**关连接。
//
// 没有这条，上面那条测试可以被一个「无条件 conn.Close()」的假修复轻易骗过
// ——那种改法能让 wsCount 归零，却把每个正常客户端都踢下线。
func TestWSHealthyEventKeepsSubscriber(t *testing.T) {
	s := NewServer([]string{"wesing"})
	s.SetActivePlayer("wesing")

	c := dialWS(t, s, "wesing")
	waitFor(t, 3*time.Second, func() bool { return s.wsCount() == 1 }, "订阅者未登记")

	s.NotifySubscribers(player.Event{
		PlayerName: "wesing",
		Type:       player.EventLyricUpdate,
		Data:       &player.LyricUpdate{Text: "正常事件"},
	}, false)

	// 读到它——证明事件确实送达，而不是靠「没断开」蒙混过关
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	found := false
	for range 10 {
		var evt WSEvent
		if err := c.ReadJSON(&evt); err != nil {
			t.Fatalf("读取事件失败（连接被错误地关闭了？）: %v", err)
		}
		if evt.Type == player.EventLyricUpdate {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("未在前 10 条消息中读到 lyric_update")
	}

	// 送达后订阅者必须仍在册
	time.Sleep(200 * time.Millisecond)
	if n := s.wsCount(); n != 1 {
		t.Errorf("正常写入后订阅者被注销了（wsCount=%d，应为 1）——修复过度，正常客户端会被踢下线", n)
	}
}
