package effect

// 本文件只测特效路径上的商店版门禁（issue #212）：shim 注入（injectShim）每轮连接之前、泊车（doPark）移窗之前都过
// watchdog.GateRunning，不过即不连接、不注入、不泊车。门禁、调试端口地址与泊车都经包级变量换成桩；9222 由本地
// httptest 的 WebSocket 服务顶替，不连真的网易云、不移真窗口。injectShim 不看特效订阅者，没有订阅者时同样走这里。

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"Metabox-Nexus-PlayerCap/player/launch"

	"github.com/gorilla/websocket"
)

// fakePage 顶替网易云的页面调试目标：记下建立的连接数与收到的 method，读满 3 条即断开。
type fakePage struct {
	mu      sync.Mutex
	conns   int
	methods []string
}

func (f *fakePage) serve(t *testing.T) string {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		f.mu.Lock()
		f.conns++
		f.mu.Unlock()
		for range 3 {
			var m struct {
				Method string `json:"method"`
			}
			if conn.ReadJSON(&m) != nil {
				return
			}
			f.mu.Lock()
			f.methods = append(f.methods, m.Method)
			f.mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func (f *fakePage) seen() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns, slices.Clone(f.methods)
}

// stubGate 换上门禁、调试端口地址与泊车的桩，返回门禁被调用的次数。
func stubGate(t *testing.T, gateErr error, wsURL string, wsErr error, park func() bool) *int {
	t.Helper()
	oldGate, oldURL, oldPark := gateFn, wsURLFn, parkFn
	t.Cleanup(func() { gateFn, wsURLFn, parkFn = oldGate, oldURL, oldPark })
	calls := new(int)
	gateFn = func() error { *calls++; return gateErr }
	wsURLFn = func() (string, error) { return wsURL, wsErr }
	parkFn = park
	return calls
}

// noPark 是泊车的桩：被调用即判失败。
func noPark(t *testing.T) func() bool {
	return func() bool { t.Error("门禁不过，却泊车了"); return false }
}

// TestInjectShimInjectsWhenGatePasses 是下面「不连接、不注入」的对照：门禁通过时确实连上顶替的页面、发出注入命令。
// 没有这一条，桩接线失效时下面恒绿。
func TestInjectShimInjectsWhenGatePasses(t *testing.T) {
	var page fakePage
	calls := stubGate(t, nil, page.serve(t), nil, noPark(t))
	if wait := injectShim(); wait != 300*time.Millisecond {
		t.Errorf("injectShim 返回 %v，期望连接断开后的 300ms", wait)
	}
	conns, methods := page.seen()
	want := []string{"Page.enable", "Page.addScriptToEvaluateOnNewDocument", "Runtime.evaluate"}
	if *calls != 1 || conns != 1 || !slices.Equal(methods, want) {
		t.Errorf("门禁调用 %d 次，连接 %d 次，收到 %v；期望 1、1、%v", *calls, conns, methods, want)
	}
}

// TestInjectShimGateRefuses：门禁不过（商店版、读不到路径、没有在运行的 cloudmusic.exe）时不建立连接、不发任何命令。
//
// 防的缺陷与变异（均实测变红）：去掉门禁；门禁挪到建立连接之后；门禁挪到发送注入命令之后。
func TestInjectShimGateRefuses(t *testing.T) {
	for _, gateErr := range []error{
		&launch.PackagedError{ExePath: `C:\Program Files\WindowsApps\x\cloudmusic.exe`},
		launch.ErrExeUnknown,
		errors.New("failed to list processes"),
	} {
		t.Run(gateErr.Error(), func(t *testing.T) {
			var page fakePage
			calls := stubGate(t, gateErr, page.serve(t), nil, noPark(t))
			if wait := injectShim(); wait != 400*time.Millisecond {
				t.Errorf("injectShim 返回 %v，期望按原有间隔 400ms 重试", wait)
			}
			if conns, methods := page.seen(); *calls != 1 || conns != 0 || len(methods) != 0 {
				t.Errorf("门禁调用 %d 次，连接 %d 次，收到 %v；期望门禁 1 次、不连接", *calls, conns, methods)
			}
		})
	}
}

// TestInjectShimSkipsGateWhenPortDown：9222 不通（网易云未运行）时这一轮在取地址处结束，不枚举进程。
//
// 防的缺陷与变异（实测变红）：门禁挪到取地址之前（网易云未运行期间每 400ms 枚举一次全部进程）。
func TestInjectShimSkipsGateWhenPortDown(t *testing.T) {
	calls := stubGate(t, nil, "", errors.New("connection refused"), noPark(t))
	if wait := injectShim(); wait != 400*time.Millisecond || *calls != 0 {
		t.Errorf("injectShim 返回 %v、门禁调用 %d 次；期望 400ms、0 次", wait, *calls)
	}
}

// TestDoParkGate：取词会话连上且门禁通过才泊车；取词没连上时连门禁也不调（不枚举进程）。泊车的桩返回 false，
// doPark 不会接着连 9222 去隐藏菜单栏。
//
// 防的缺陷与变异（均实测变红）：去掉门禁；门禁挪到泊车之后；去掉取词会话的检查（未连上时调门禁）。
func TestDoParkGate(t *testing.T) {
	cases := []struct {
		name      string
		up        bool
		gateErr   error
		wantGate  int
		wantParks int
	}{
		{"取词已连上、门禁通过", true, nil, 1, 1},
		{"取词已连上、商店版", true, &launch.PackagedError{ExePath: `C:\Program Files\WindowsApps\x\cloudmusic.exe`}, 1, 0},
		{"取词已连上、读不到路径", true, launch.ErrExeUnknown, 1, 0},
		{"取词没连上", false, nil, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parks := 0
			calls := stubGate(t, c.gateErr, "", nil, func() bool { parks++; return false })
			cp := &Capturer{netEaseUp: func() bool { return c.up }}
			cp.doPark()
			if *calls != c.wantGate || parks != c.wantParks {
				t.Errorf("门禁调用 %d 次、泊车 %d 次；期望 %d、%d", *calls, parks, c.wantGate, c.wantParks)
			}
		})
	}
}
