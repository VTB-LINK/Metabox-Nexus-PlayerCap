package main

// 本文件只测一件事：**每日在线心跳绝不做更新动作**。
//
// 它守的是 AGENTS.md §0.1（绝不炸主播的机器）在这条新通路上的具体形态。
//
// 心跳与启动时的版本检查打的是同一个端点、组装的是同一个请求，拿到的响应体里就是
// 完整的 releaseInfo —— 「顺手解析一下、发现有新版就更新」离得只有几行远，而且看起来
// 完全合理。后果是：直播进行到一半，进程把自己的 exe 换掉并 restartSelf()。
// OBS 那头是黑的，主播不知道发生了什么，也没有任何日志会说「我刚刚重启了」。
//
// 靠注释拦不住这个。callback_lint_test.go 的教训写得很直白：写在文档里的规矩，
// 新代码不会去读。所以做成 AST 门禁。
//
// # 门禁的边界（别把它当成比实际更强的保证）
//
// 它只查**直接调用**。心跳里调一个中间函数、由那个函数去执行更新，本门禁看不见。
// 这是刻意的取舍：跨函数追调用图要做别名与包解析，复杂度不成比例。真正的形状
// （在心跳里顺手加两行）它抓得住。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// heartbeatFuncs 是心跳通路上的全部函数。新增一个就登记进来。
var heartbeatFuncs = []string{"startDailyHeartbeat", "sendHeartbeat"}

// forbiddenInHeartbeat 是心跳里绝不允许出现的调用，值为「为什么」。
//
// 报错时会把理由一起打出来 —— 一条只说「不许调这个」的门禁，撞上它的人第一反应是
// 想办法绕过去，而不是理解它在拦什么。
var forbiddenInHeartbeat = map[string]string{
	"performUpdateAll":     "会下载并替换 exe",
	"downloadFile":         "会往磁盘写文件",
	"renameWithRetry":      "会替换正在运行的 exe",
	"restartSelf":          "会重启进程 —— 直播进行到一半重启就是事故",
	"checkAndUpdate":       "整条自动更新流程，只允许在启动时跑一次",
	"decideUpdate":         "做「要不要更新」的决策 —— 心跳不该有决策",
	"pickFastestCDNPrefix": "只在准备下载时才需要测速",
	"Exit":                 "os.Exit 会直接杀死进程",
	"Fatal":                "同上，log.Fatal 内部就是 os.Exit",
	"Fatalf":               "同上",
}

func TestHeartbeatMakesNoUpdateCalls(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读取 main.go 失败: %v", err)
	}

	for _, name := range heartbeatFuncs {
		calls, err := callsInFunc(src, name)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", name, err)
		}
		if calls == nil {
			t.Fatalf("main.go 里找不到函数 %s —— 改名了就同步改 heartbeatFuncs，"+
				"否则这条门禁会安静地什么都不查", name)
		}
		for _, call := range calls {
			if why, bad := forbiddenInHeartbeat[call]; bad {
				t.Errorf("%s 里调用了 %s（%s）—— 心跳只允许发请求、丢响应体。"+
					"自动更新只能发生在启动时，那时 OBS 还没在拉流。", name, call, why)
			}
		}
	}
}

// TestHeartbeatGateCatchesViolations 钉死门禁抓得住它要堵的形状。
//
// 没有这条，谁重构 callsInFunc 都可能把它改成恒绿而无人察觉 —— 一个抓不住目标形状的
// 门禁比没有门禁更糟，它给人已经守住了的错觉（同 player/callback_lint_test.go 的教训）。
func TestHeartbeatGateCatchesViolations(t *testing.T) {
	cases := map[string]string{
		"直接调用更新": `package main
func sendHeartbeat() { performUpdateAll("", "", nil) }`,
		"经包名调用 os.Exit": `package main
import "os"
func sendHeartbeat() { os.Exit(0) }`,
		"藏在闭包里": `package main
func sendHeartbeat() { go func() { restartSelf() }() }`,
		"藏在 if 分支里": `package main
func sendHeartbeat() {
	if true {
		restartSelf()
	}
}`,
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			calls, err := callsInFunc([]byte(src), "sendHeartbeat")
			if err != nil {
				t.Fatal(err)
			}
			hit := false
			for _, call := range calls {
				if _, bad := forbiddenInHeartbeat[call]; bad {
					hit = true
				}
			}
			if !hit {
				t.Errorf("门禁漏报了此形状，实际采到的调用：%v", calls)
			}
		})
	}
}

// TestHeartbeatGateAllowsRequestOnly 确认合规写法不被误报 —— 否则门禁会逼人绕过它。
func TestHeartbeatGateAllowsRequestOnly(t *testing.T) {
	src := `package main
import (
	"io"
	"net/http"
)
func sendHeartbeat() {
	req, err := newVersionCheckRequest("daily")
	if err != nil {
		return
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	markPinged(nowFn())
}`
	calls, err := callsInFunc([]byte(src), "sendHeartbeat")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if why, bad := forbiddenInHeartbeat[call]; bad {
			t.Errorf("合规写法被误报：%s（%s）", call, why)
		}
	}
}

// callsInFunc 返回 src 里名为 funcName 的顶层函数体内**直接调用**的全部函数名。
//
// 函数不存在时返回 nil（区别于「存在但一个调用都没有」的空切片）—— 调用方靠它发现
// 函数改名导致门禁空转。
//
// 取名方式：普通调用取标识符名（restartSelf），选择器调用只取最后一段（os.Exit → Exit）。
// 只取最后一段是为了不被别名 import 骗过，代价是 Exit / Fatal 这类通名可能误伤同名方法
// —— 在心跳这么小的函数里，这个方向的误报比漏报安全得多。
func callsInFunc(src []byte, funcName string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		return nil, err
	}

	var target *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == funcName && fn.Body != nil {
			target = fn
			break
		}
	}
	if target == nil {
		return nil, nil
	}

	seen := map[string]bool{}
	// ast.Inspect 会走进 FuncLit 的函数体，所以闭包里的调用同样会被采到。
	ast.Inspect(target.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			seen[fun.Name] = true
		case *ast.SelectorExpr:
			seen[fun.Sel.Name] = true
		}
		return true
	})

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names) // 报错信息稳定，便于 diff
	return names, nil
}

// TestHeartbeatGateReadsTheRealFile 确认门禁读的是仓库里那个 main.go，不是别的什么。
//
// 这条看着多余，实则是本门禁唯一的「接线」断言：上面所有用例都可以在合成源码上全绿，
// 而真正的 main.go 从没被打开过。
func TestHeartbeatGateReadsTheRealFile(t *testing.T) {
	abs, err := filepath.Abs("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", abs, err)
	}
	if !strings.Contains(string(src), "func sendHeartbeat()") {
		t.Fatalf("%s 里没有 sendHeartbeat —— 门禁读错文件了", abs)
	}
}
