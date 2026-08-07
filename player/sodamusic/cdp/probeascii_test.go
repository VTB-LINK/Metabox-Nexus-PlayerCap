package cdp

// 本文件只测一件事：内层探针 JS 必须是纯 ASCII。
//
// 它守的不是风格，是一条会**整包静默失效**的不变量。bridgeExpr 用 Go 的 base64 编码
// innerProbeJS，再由页面里的 `atob` 解回；而 `atob` 只认 Latin-1，字符串里出现任何中文，
// 解出来就是乱码 → JS 解析失败 → inspector 直接掐断 WS。
//
// 症状与原因离得很远，这正是它值得做成门禁的理由：日志里只会看到每 2 秒一条
// 「CDP 连接成功」不停重连，Extract 报 `wsarecv: 远程主机强迫关闭了一个现有的连接`，
// 所有端点的 data 全空——没有任何一处提示「你往 JS 字符串里写了中文」。
// 真实发生过一次：给探针加注释时把中文写进了字符串里，四道门禁全绿，真机全挂。
//
// 注释要写就写在 Go 这边（innerProbeJS 上方的文档注释），别写进字符串。

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestInnerProbeJSIsASCII(t *testing.T) {
	// 变异自证：把 innerProbeJS 里任意一处 ASCII 换成中文（例如 'no-port' → '无端口'），
	// 本测试立即变红；这正是它要拦的那一次改动。
	for i, r := range innerProbeJS {
		if r > utf8.RuneSelf {
			line := 1 + strings.Count(innerProbeJS[:i], "\n")
			t.Fatalf("innerProbeJS 第 %d 行出现非 ASCII 字符 %q（U+%04X）——atob 只认 Latin-1，"+
				"这会让探针在真机上解析失败并被 inspector 掐断连接。注释请写到 Go 侧。",
				line, r, r)
		}
	}
}
