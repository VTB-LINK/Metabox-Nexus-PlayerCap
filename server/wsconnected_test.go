package server

// 本文件只测 /service-status 的三组连接统计互不串口径（issue #122）：
//   - ws_connected / client_count 只数 /ws 与 /{player}/ws；
//   - audio_ws_connected 数 /audio-ws 与 /{player}/audio-ws；
//   - effect_ws_connected 数 /cloudmusicv3/effect-ws。
//
// 用真的 WebSocket 连接而不是直接往 hub 里塞订阅者：客户端地址要从握手后的 RemoteAddr 登记进去，
// 塞订阅者的写法绕过了这一步，漏登记（地址为空串）也测不出来。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type wsConnState struct {
	Connected bool            `json:"connected"`
	Clients   json.RawMessage `json:"clients"`
}

type wsStatusSnapshot struct {
	ClientCount int         `json:"client_count"`
	WS          wsConnState `json:"ws_connected"`
	Audio       wsConnState `json:"audio_ws_connected"`
	Effect      wsConnState `json:"effect_ws_connected"`
}

func serviceStatusWS(t *testing.T, s *Server) wsStatusSnapshot {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleServiceStatus(rec, httptest.NewRequest("GET", "/service-status", nil))
	var resp struct {
		Data wsStatusSnapshot `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, rec.Body.String())
	}
	return resp.Data
}

// addrSet 把 clients 解成排序后的地址列表；clients 缺失或为 null 时直接判错。
func (w wsConnState) addrSet(t *testing.T) []string {
	t.Helper()
	var addrs []string
	if err := json.Unmarshal(w.Clients, &addrs); err != nil || addrs == nil {
		t.Fatalf("clients 不是数组: %q（%v）", string(w.Clients), err)
	}
	sort.Strings(addrs)
	return addrs
}

func sameAddrs(got []string, want ...string) bool {
	sort.Strings(want)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// wsTestRig 把四类 WS 端点挂到一个 httptest 服务上，dial 返回客户端连接及其本地地址
// （回环上等于服务端看到的 RemoteAddr）。
type wsTestRig struct {
	t   *testing.T
	s   *Server
	url string
}

func newWSTestRig(t *testing.T) *wsTestRig {
	t.Helper()
	s := NewServer([]string{"wesing", "kugou"})
	s.SetServiceInfo(&ServiceInfo{Version: "9.9.9"})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS(""))
	mux.HandleFunc("/audio-ws", s.handleAudioWS(""))
	mux.HandleFunc("/kugou/audio-ws", s.handleAudioWS("kugou"))
	mux.HandleFunc("/cloudmusicv3/effect-ws", s.handleEffectWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &wsTestRig{t: t, s: s, url: "ws" + strings.TrimPrefix(srv.URL, "http")}
}

func (r *wsTestRig) dial(path string) (*websocket.Conn, string) {
	r.t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(r.url+path, nil)
	if err != nil {
		r.t.Fatalf("拨号 %s 失败: %v", path, err)
	}
	r.t.Cleanup(func() { c.Close() })
	return c, c.LocalAddr().String()
}

// waitStatus 等到 cond 对最新快照成立；服务端在握手返回之后才登记订阅者，不能拨号后立刻断言。
func (r *wsTestRig) waitStatus(cond func(wsStatusSnapshot) bool, msg string) wsStatusSnapshot {
	r.t.Helper()
	var last wsStatusSnapshot
	waitFor(r.t, 3*time.Second, func() bool {
		last = serviceStatusWS(r.t, r.s)
		return cond(last)
	}, msg)
	return last
}

// 没有任何连接：三组都是 connected=false、clients 为 [] 而不是 null 或缺失。
//
// 变异自证：AudioClientAddrs / EffectClientAddrs 改成 var addrs []string（空时序列化为 null）即红；
// 删掉响应里的 audio_ws_connected 或 effect_ws_connected 字段即红。
func TestServiceStatusExtraWSEmpty(t *testing.T) {
	st := serviceStatusWS(t, NewServer([]string{"wesing"}))
	for name, w := range map[string]wsConnState{"ws_connected": st.WS, "audio_ws_connected": st.Audio, "effect_ws_connected": st.Effect} {
		if w.Connected {
			t.Errorf("%s.connected = true，无连接时应为 false", name)
		}
		if got := string(w.Clients); got != "[]" {
			t.Errorf("%s.clients = %q，无连接时应为 []", name, got)
		}
	}
	if st.ClientCount != 0 {
		t.Errorf("client_count = %d，应为 0", st.ClientCount)
	}
}

// 逐类加连接，每一步都核对其余两组没被带动：这是「口径不串」的反例集合。
//
// 变异自证（逐个做，各自应红）：
//   - handleAudioWS 里 audioSub 不登记 addr（地址为空串）；
//   - handleEffectWS 里 effectSub 不登记 addr；
//   - audio_ws_connected 改读 s.ClientAddrs()（与 /ws 串口径）；
//   - audio_ws_connected 与 effect_ws_connected 的取值互换；
//   - AudioClientAddrs 只数 player=="" 的订阅者（漏掉 per-player）。
func TestServiceStatusExtraWSCounts(t *testing.T) {
	r := newWSTestRig(t)

	// 只连 /audio-ws：只有 audio 组亮；ws_connected 与 client_count 不受影响。
	_, audioRoot := r.dial("/audio-ws")
	st := r.waitStatus(func(s wsStatusSnapshot) bool { return s.Audio.Connected }, "audio-ws 订阅者未登记")
	if !sameAddrs(st.Audio.addrSet(t), audioRoot) {
		t.Errorf("audio clients = %v，期望 [%s]", st.Audio.addrSet(t), audioRoot)
	}
	if st.WS.Connected || st.ClientCount != 0 || st.Effect.Connected {
		t.Errorf("只连 /audio-ws 却带动了别的组: ws=%v count=%d effect=%v", st.WS.Connected, st.ClientCount, st.Effect.Connected)
	}

	// 再连 /kugou/audio-ws（per-player）：同一组里两条。
	_, audioKugou := r.dial("/kugou/audio-ws")
	st = r.waitStatus(func(s wsStatusSnapshot) bool { return len(s.Audio.addrSet(t)) == 2 }, "per-player audio-ws 订阅者未登记")
	if !sameAddrs(st.Audio.addrSet(t), audioRoot, audioKugou) {
		t.Errorf("audio clients = %v，期望 [%s %s]", st.Audio.addrSet(t), audioRoot, audioKugou)
	}

	// 连 effect-ws：effect 组亮，audio 组不变。
	_, effectAddr := r.dial("/cloudmusicv3/effect-ws")
	st = r.waitStatus(func(s wsStatusSnapshot) bool { return s.Effect.Connected }, "effect-ws 订阅者未登记")
	if !sameAddrs(st.Effect.addrSet(t), effectAddr) {
		t.Errorf("effect clients = %v，期望 [%s]", st.Effect.addrSet(t), effectAddr)
	}
	if !sameAddrs(st.Audio.addrSet(t), audioRoot, audioKugou) {
		t.Errorf("连 effect-ws 后 audio clients 变了: %v", st.Audio.addrSet(t))
	}
	if st.WS.Connected || st.ClientCount != 0 {
		t.Errorf("连 audio/effect 却带动了 ws_connected: %v count=%d", st.WS.Connected, st.ClientCount)
	}

	// 连 /ws：ws_connected 与 client_count 亮，audio / effect 的地址里不得出现 /ws 的地址。
	_, wsAddr := r.dial("/ws")
	st = r.waitStatus(func(s wsStatusSnapshot) bool { return s.WS.Connected }, "/ws 订阅者未登记")
	if st.ClientCount != 1 || !sameAddrs(st.WS.addrSet(t), wsAddr) {
		t.Errorf("ws clients = %v count=%d，期望 [%s] 与 1", st.WS.addrSet(t), st.ClientCount, wsAddr)
	}
	for _, a := range append(st.Audio.addrSet(t), st.Effect.addrSet(t)...) {
		if a == wsAddr {
			t.Errorf("/ws 的地址 %s 混进了 audio 或 effect 组", wsAddr)
		}
	}
}

// 断开后回到空：只关 audio 的两条，audio 组归零（clients 回到 []），effect 与 ws 组不动。
//
// 变异自证：handleAudioWS 清理路径里删掉 removeSub 即红（订阅者在册不销）。
func TestServiceStatusExtraWSDisconnect(t *testing.T) {
	r := newWSTestRig(t)
	c1, _ := r.dial("/audio-ws")
	c2, _ := r.dial("/kugou/audio-ws")
	_, effectAddr := r.dial("/cloudmusicv3/effect-ws")
	r.waitStatus(func(s wsStatusSnapshot) bool {
		return len(s.Audio.addrSet(t)) == 2 && s.Effect.Connected
	}, "订阅者未登记")

	c1.Close()
	c2.Close()
	st := r.waitStatus(func(s wsStatusSnapshot) bool { return !s.Audio.Connected }, "audio-ws 断开后订阅者仍在册")
	if got := string(st.Audio.Clients); got != "[]" {
		t.Errorf("断开后 audio clients = %q，应为 []", got)
	}
	if !st.Effect.Connected || !sameAddrs(st.Effect.addrSet(t), effectAddr) {
		t.Errorf("关 audio 连接带动了 effect 组: %+v", st.Effect)
	}
}
