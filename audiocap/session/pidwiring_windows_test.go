//go:build windows

package session

// 本文件钉住 COM 层 sessionAt 经 pidProblem 判 GetProcessId 的结果（Codex 审查 R2）。sessionAt 全是真 COM 调用，
// 离线喂不进去，故用源码扫描：只认调用次数，不替代读代码。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// 变异自证（实测）：sessionAt 里不再调用 pidProblem（如改回只看 failed(hr)）→ 调用数 0，红。
func TestSessionAtJudgesPidViaPidProblem(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "session_windows.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 session_windows.go 失败: %v", err)
	}
	calls := -1
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "sessionAt" {
			continue
		}
		calls = 0
		ast.Inspect(fn, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "pidProblem" {
					calls++
				}
			}
			return true
		})
	}
	if calls < 0 {
		t.Fatal("没找到 sessionAt —— 本门禁的判据失效了")
	}
	if calls != 1 {
		t.Errorf("sessionAt 里应恰有 1 处 pidProblem 调用，实得 %d 处", calls)
	}
}
