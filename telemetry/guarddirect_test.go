package telemetry

// 本文件只测一件事：**全仓的 telemetry.Guard 都是 `defer telemetry.Guard()` 直接调用**。
// Guard 的语义在 panicrepanic_test.go，栈裁剪在 stackframetrim_test.go。
//
// # 为什么这条只能做成 AST 门禁
//
// Go 的 recover() **只在被 defer 直接调用的函数里有效**。所以：
//
//	defer telemetry.Guard()              ✓ recover() 拿到 panic
//	defer func(){ telemetry.Guard() }()  ✗ recover() 返回 nil
//
// 第二种写法多包了一层闭包，Guard 内部的 recover() 就够不到 panic 了：panic 照常向上跑、
// 进程照常死（所以主播那边看不出任何区别），**而上报静默失效**。四道门禁全绿，单元测试也
// 全绿——因为被测的 Guard 本身没坏，坏的是调用形状。
//
// 这正是 AGENTS.md §3.0 说的「新增规矩优先做成门禁」：写成注释的话，它迟早被违反，
// 而违反的后果只在主播的机器上、以「什么都没上报」的形式存在，我们这头永远看不见。
//
// 参照 player/callback_lint_test.go —— 那条 NewCallback 门禁是同一种东西：语言层面的隐性
// 约束，编译器和 vet 都不管，只能自己钉。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestGuardOnlyUsedAsDirectDefer 扫全仓，钉死 Guard 的调用形状。
//
// 变异自证：把 main.go 任意一处改成 `defer func() { telemetry.Guard() }()` 即红。
func TestGuardOnlyUsedAsDirectDefer(t *testing.T) {
	pkgPath := reflect.TypeOf(Options{}).PkgPath() // 包挪目录/module 改名都自动跟上

	var violations []string
	var scanned, guarded int

	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint // 读不到的目录跳过，不让门禁因权限问题假红
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "build-assets", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // 解析不了的文件交给 go build 去报，这里不重复报
		}

		// 本文件里 telemetry 包叫什么（可能是别名 import）
		local := localNameOf(f, pkgPath)
		if local == "" {
			return nil // 没 import telemetry
		}
		scanned++

		// 合法位置：defer <local>.Guard() 里那个 Fun 的位置
		ok := map[token.Pos]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			ds, isDefer := n.(*ast.DeferStmt)
			if !isDefer {
				return true
			}
			if isGuardSelector(ds.Call.Fun, local) {
				ok[ds.Call.Fun.Pos()] = true
			}
			return true
		})

		// 所有引用：不在合法集合里的一律违规（含闭包包裹、赋值给变量、当参数传等）
		ast.Inspect(f, func(n ast.Node) bool {
			sel, isSel := n.(*ast.SelectorExpr)
			if !isSel || !isGuardSelector(sel, local) {
				return true
			}
			if ok[sel.Pos()] {
				guarded++
				return true
			}
			p := fset.Position(sel.Pos())
			violations = append(violations, fmt.Sprintf("%s:%d", filepath.ToSlash(path), p.Line))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("遍历仓库失败: %v", err)
	}

	if len(violations) > 0 {
		t.Errorf("下列位置的 %s.Guard 不是 `defer` 直接调用：\n  %s\n\n"+
			"Go 的 recover() 只在被 defer 直接调用的函数里有效。包一层闭包"+
			"（defer func(){ Guard() }()）会让 recover() 返回 nil —— panic 照常向上跑、"+
			"进程照常死，看不出任何区别，但**上报静默失效**。",
			pkgPath, strings.Join(violations, "\n  "))
	}

	// 反向自证：如果一个 Guard 调用点都没扫到，说明这个门禁在空转 —— 那比没有门禁更糟，
	// 因为它会让人以为「已经守住了」。main.go 现在就有 7 处（main 自己 + 6 个 goroutine）。
	if guarded == 0 {
		t.Errorf("全仓没有扫到任何 %s.Guard 调用点（检查了 %d 个 import 了本包的文件）—— "+
			"要么接线全没了，要么这个门禁的匹配逻辑坏了。两种都得查。", pkgPath, scanned)
	}
	t.Logf("扫过 %d 个 import 了 telemetry 的文件，%d 处合法的 defer Guard()", scanned, guarded)
}

// localNameOf 返回 pkgPath 在文件 f 里的本地名（别名 import 时是别名），没 import 则返回 ""。
func localNameOf(f *ast.File, pkgPath string) string {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != pkgPath {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				return "" // 空导入/点导入不会有 pkg.Guard 形式
			}
			return imp.Name.Name
		}
		return filepath.Base(pkgPath)
	}
	return ""
}

// isGuardSelector 判断表达式是不是 `<local>.Guard`。
func isGuardSelector(e ast.Expr, local string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Guard" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == local
}
