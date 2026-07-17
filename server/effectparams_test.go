package server

// 本文件只测 effect-ws 截帧参数的生效时机：必须等 Upgrade 成功，非 WS 请求不得留下副作用。
// 不测参数本身的 clamp、不测帧广播、不测订阅者僵死（ws_zombie_test.go）。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// effectSrv 起一个只挂 handleEffectWS 的 httptest 服务。
func effectSrv(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.handleEffectWS))
	t.Cleanup(srv.Close)
	return srv
}

// TestEffectParamsIgnoredWithoutUpgrade 钉死本条核心：连不上的请求不得改全局画质。
//
// 缺陷背景：setEffectParams 曾在 upgrader.Upgrade 之前调用，而 Upgrade 失败是直接
// return、不回滚。截帧参数是 effectHub 级的全局单例，没有 per-connection 状态可恢复
// —— 于是主播在浏览器地址栏敲 /cloudmusicv3/effect-ws?quality=60 想探活（普通 GET，
// 无升级头，Upgrade 必败），直播画质就被当场改掉，只能重启服务才能恢复。
//
// 这不是安全问题：server.go:104 的 CheckOrigin 恒 true 是 AGENTS.md §14 有意接受的
// 现状，任意网页 new WebSocket 的握手照样成功、参数照样生效。本测试守的是运维 footgun。
//
// 变异自证：把 setEffectParams 移回 Upgrade 之前即红。
func TestEffectParamsIgnoredWithoutUpgrade(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	srv := effectSrv(t, s)

	q0, s0, hc0, fc0, gen0 := s.EffectCaptureParams()

	// 普通 GET —— 无 Connection: upgrade 头，正是地址栏/curl 探活的形状
	resp, err := http.Get(srv.URL + "?quality=60&scale=0.1&header_clickable=0&footer_clickable=1")
	if err != nil {
		t.Fatalf("GET 失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatalf("前置条件：普通 GET 不该完成 WS 握手，实得 %d", resp.StatusCode)
	}

	q1, s1, hc1, fc1, gen1 := s.EffectCaptureParams()
	if q1 != q0 || s1 != s0 || hc1 != hc0 || fc1 != fc0 {
		t.Errorf("探活式 GET 改掉了全局截帧参数：quality %d→%d, scale %.2f→%.2f, header %v→%v, footer %v→%v",
			q0, q1, s0, s1, hc0, hc1, fc0, fc1)
	}
	if gen1 != gen0 {
		t.Errorf("gen 被推进 %d→%d —— 捕获器会据此热更参数/重启会话", gen0, gen1)
	}
}

// TestEffectParamsAppliedOnWSConnect 反方向：别把功能删坏了。
// 没有这条，「把 setEffectParams 整个删掉」这种假修复能通过上面那条。
//
// 注意时序：参数现在写在 Upgrade **之后**，而客户端 Dial 收到 101 即返回 —— 此刻服务端
// 未必已跑完 setEffectParams。故必须等，不能拨完就断言（那样是靠时序巧合，会 flake）。
// 这个窗口对生产无影响：捕获器认的是 HasEffectSubscribers()，而 sub 登记排在
// setEffectParams 之后，见下一条测试。
func TestEffectParamsAppliedOnWSConnect(t *testing.T) {
	s := NewServer([]string{"cloudmusicv3"})
	srv := effectSrv(t, s)

	_, _, _, _, gen0 := s.EffectCaptureParams()

	c, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http")+"?quality=60&header_clickable=0&footer_clickable=1", nil)
	if err != nil {
		t.Fatalf("WS 拨号失败: %v", err)
	}
	defer c.Close()

	waitFor(t, time.Second, func() bool {
		q, _, _, _, _ := s.EffectCaptureParams()
		return q == 60
	}, "握手成功后 quality 应生效为 60（参数没生效 = setEffectParams 被删/没跑）")

	_, _, hc, fc, gen := s.EffectCaptureParams()
	if hc != false || fc != true {
		t.Errorf("握手成功后 header/footer 应为 false/true，实得 %v/%v", hc, fc)
	}
	if gen == gen0 {
		t.Errorf("参数变更必须推进 gen（捕获器据此热更），实得仍为 %d", gen)
	}
}

// TestEffectParamsOrderInHandleEffectWS 钉死 handleEffectWS 里三件事的**源码顺序**：
//
//	Upgrade 成功  →  setEffectParams  →  h.subs[sub] 登记
//
// 两侧各守一个不变量：
//   - 在 Upgrade **之后**：连不上的请求不得留下副作用（见上面第一条测试）。
//   - 在 h.subs 登记**之前**：捕获器唯一的感知入口是 HasEffectSubscribers()，session
//     开头才读 EffectCaptureParams 并锁定 startGen。排在登记之前 → gen++ 严格
//     happens-before 订阅者可见，零窗口；排在之后 → 捕获器可能以旧参数启动 session，
//     而 400ms 热更只重写 quality/fps，header/footer 的注入只在会话启动时做一次 ——
//     本次连接请求的 header/footer 会静默失效。
//
// **为什么用 AST 而不是跑一次连接**：本测试的前身就是运行时版的（拨号 → waitFor 等
// HasEffectSubscribers → 断言参数已就位）。实测它**抓不住**「setEffectParams 挪到
// h.subs 登记之后」这个变异——waitFor 每 10ms 轮询一次，而那两行之间只隔几微秒，
// 轮询粒度根本够不着，于是变异照样绿。那是个装饰品。
//
// 这个不变量本来就是源码顺序的不变量（happens-before 由顺序保证），钉源码顺序才是
// 确定性的。同类先例见 player/callback_lint_test.go。
//
// 变异自证：把 setEffectParams 挪到 h.mu.Unlock() 之后 → 红；挪回 Upgrade 之前 → 红。
func TestEffectParamsOrderInHandleEffectWS(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "effect.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 effect.go 失败: %v", err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "handleEffectWS" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("effect.go 里找不到 handleEffectWS —— 测试的锚点没了，不是代码没问题")
	}

	// 三个锚点的源码位置（0 = 未找到）
	var posUpgrade, posSetParams, posSubs token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "Upgrade":
					if posUpgrade == 0 {
						posUpgrade = v.Pos()
					}
				case "setEffectParams":
					if posSetParams == 0 {
						posSetParams = v.Pos()
					}
				}
			}
		case *ast.AssignStmt:
			// h.subs[sub] = struct{}{}
			for _, lhs := range v.Lhs {
				if ix, ok := lhs.(*ast.IndexExpr); ok {
					if sel, ok := ix.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "subs" && posSubs == 0 {
						posSubs = v.Pos()
					}
				}
			}
		}
		return true
	})

	if posUpgrade == 0 || posSetParams == 0 || posSubs == 0 {
		t.Fatalf("锚点缺失（Upgrade=%v setEffectParams=%v h.subs登记=%v）—— 若确属重构，"+
			"请连同本测试一起更新，别直接删",
			posUpgrade != 0, posSetParams != 0, posSubs != 0)
	}

	line := func(p token.Pos) int { return fset.Position(p).Line }

	if !(posUpgrade < posSetParams) {
		t.Errorf("setEffectParams(:%d) 必须在 Upgrade(:%d) 之后 —— 否则 Upgrade 失败的请求"+
			"（地址栏探活/curl）会改掉全局画质且无从回滚",
			line(posSetParams), line(posUpgrade))
	}
	if !(posSetParams < posSubs) {
		t.Errorf("setEffectParams(:%d) 必须在 h.subs 登记(:%d) 之前 —— 否则捕获器可能以旧参数"+
			"启动会话，本次连接的 header/footer 静默失效",
			line(posSetParams), line(posSubs))
	}
}
