package cdp

// 本文件守 issue #188 在 CDP 这一层的两件事（AGENTS §7.7.4）：
//   - dial 连上即订阅 NodeRuntime.waitingForDisconnect；
//   - evaluate 读到这个事件即关闭连接、返回 ErrHostExiting——哪怕紧随其后就是一条正常的回复。连接关掉，
//     汽水主进程退出前的等待才结束。
// 反例：订阅自己的回复（含旧版 Node 不认这条命令的报错）、名字相近的 NodeRuntime.waitingForDebugger 与其它
// 事件一律跳过，取数照常。
//
// 汽水的 inspector 换成本地的 WebSocket 服务端，按脚本收发帧。不碰汽水、不连 9229。
//
// 变异自证（本 worktree 里逐条注入、跑完即还原，均实测）记在各用例的注释里。

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeInspector 起一个本地 WebSocket 服务端，每条连接交给 serve 处理，返回 ws:// 地址。
func fakeInspector(t *testing.T, serve func(conn *websocket.Conn)) string {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		serve(conn)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

type frame struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// readFrame 读一帧客户端发来的帧；2s 内读不到即返回错误。
func readFrame(conn *websocket.Conn) (frame, error) {
	var f frame
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	err := conn.ReadJSON(&f)
	return f, err
}

// evalReply 是对第 id 条 Runtime.evaluate 的回复，value 为 data 的 JSON 文本（同主进程桥 JSON.stringify 的产物）。
func evalReply(id int, data map[string]interface{}) map[string]interface{} {
	b, _ := json.Marshal(data)
	return map[string]interface{}{
		"id": id,
		"result": map[string]interface{}{
			"result": map[string]interface{}{"type": "string", "value": string(b)},
		},
	}
}

var playingData = map[string]interface{}{"ok": true, "isPlaying": true, "mediaId": "7", "name": "鲜花"}

// 连上后发出的第一帧就是订阅，id 为 1、enabled 为 true。
//
// 变异自证：dial 不发订阅，本例读不到订阅帧变红（本文件另两例因服务端把取数请求当成订阅帧，同样变红）。
func TestDialSubscribesWaitingForDisconnect(t *testing.T) {
	got := make(chan frame, 1)
	errs := make(chan error, 1)
	url := fakeInspector(t, func(conn *websocket.Conn) {
		f, err := readFrame(conn)
		if err != nil {
			errs <- err
			return
		}
		got <- f
	})
	c, err := dial(url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case err := <-errs:
		t.Fatalf("没读到订阅帧：%v", err)
	case f := <-got:
		var p struct {
			Enabled bool `json:"enabled"`
		}
		_ = json.Unmarshal(f.Params, &p)
		if f.ID != 1 || f.Method != "NodeRuntime.notifyWhenWaitingForDisconnect" || !p.Enabled {
			t.Fatalf("第一帧应为订阅（id 1、enabled true），实为 %+v params=%s", f, f.Params)
		}
	}
}

// 读到 waitingForDisconnect：返回 ErrHostExiting、会话标为已断，服务端看到连接被关掉（不是读超时）。
// 事件后面紧跟一条正常回复，也不改变结论：主进程已经在等我们断开了。
//
// 变异自证：删掉 evaluate 里对 waitingForDisconnect 的判断，本例取到数据、连接未关变红；判断后只返回错误
// 不 markClosed，本例因服务端读超时（连接未关）变红。
func TestExtractReleasesOnWaitingForDisconnect(t *testing.T) {
	closed := make(chan error, 1)
	url := fakeInspector(t, func(conn *websocket.Conn) {
		if _, err := readFrame(conn); err != nil { // 订阅
			closed <- err
			return
		}
		_ = conn.WriteJSON(map[string]interface{}{"id": 1, "result": map[string]interface{}{}})
		req, err := readFrame(conn)
		if err != nil {
			closed <- err
			return
		}
		_ = conn.WriteJSON(map[string]interface{}{"method": "NodeRuntime.waitingForDisconnect", "params": map[string]interface{}{}})
		_ = conn.WriteJSON(evalReply(req.ID, playingData))
		_, err = readFrame(conn)
		closed <- err
	})
	c, err := dial(url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	data, err := c.Extract()
	if !errors.Is(err, ErrHostExiting) || data != nil {
		t.Fatalf("应返回 ErrHostExiting，实为 data=%+v err=%v", data, err)
	}
	if !c.IsClosed() {
		t.Fatal("会话应标为已断")
	}
	err = <-closed
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("服务端应看到连接被关闭，实为 %v", err)
	}
}

// 反例：订阅的回复是报错（旧版 Node 不认这条命令）、随后来了名字相近的 waitingForDebugger 与别的事件，
// 都跳过，取数照常、会话不断。
//
// 变异自证：判据改成前缀匹配 `strings.HasPrefix(res.Method, "NodeRuntime.waiting")`，本例因
// waitingForDebugger 被当成退出变红。
func TestExtractIgnoresOtherFrames(t *testing.T) {
	url := fakeInspector(t, func(conn *websocket.Conn) {
		if _, err := readFrame(conn); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]interface{}{"id": 1, "error": map[string]interface{}{
			"code": -32601, "message": "'NodeRuntime.notifyWhenWaitingForDisconnect' wasn't found"}})
		req, err := readFrame(conn)
		if err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]interface{}{"method": "NodeRuntime.waitingForDebugger", "params": map[string]interface{}{}})
		_ = conn.WriteJSON(map[string]interface{}{"method": "Runtime.executionContextDestroyed", "params": map[string]interface{}{"executionContextId": 1}})
		_ = conn.WriteJSON(evalReply(req.ID, playingData))
		_, _ = readFrame(conn) // 等客户端关闭
	})
	c, err := dial(url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	data, err := c.Extract()
	if err != nil || data == nil || data.Name != "鲜花" || !data.IsPlaying {
		t.Fatalf("应照常取到数据，实为 data=%+v err=%v", data, err)
	}
	if c.IsClosed() {
		t.Fatal("会话不应断开")
	}
}
