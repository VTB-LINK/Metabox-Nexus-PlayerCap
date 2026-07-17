package player

// 本文件只测一件事：全仓 AST 门禁「NewCallback 只允许出现在包级 var 的初始化表达式」。
// 它不测 player 包的任何业务逻辑——扫的是整个仓库的源码。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoNewCallbackInsideFunc 全仓强制：syscall/windows 的 NewCallback 系列
// 只允许出现在**包级 var 的初始化表达式那一层**，绝不允许在任何函数体内调用。
//
// 为什么用测试而不是靠注释：这条规矩此前已写在开发文档里，player/wesing/proc/memory.go
// 也是照做的样板，但后加的 park 包依然在 findMainAny 与 DebugList 里逐次新建回调
// ——写在文档里的规矩，新代码不会去读。规则背景见 AGENTS.md §3。
//
// 违反它的真实后果（park 那次）：NewCallback 有全局上限（2000）且**永不回收**。
// findMainAny 经 mainWindow 被 IsMainMinimized/IsMainForeground 调用，而后两者在
// effect 的 windowStateLoop 里每 80ms 各跑一次（无门控，进程在就一直跑）。网易云
// 进程已存在但主窗尚未建好时，缓存命不中 → 每 tick 泄漏 2 个回调，跨重启累积。
//
// **本测试自身的历史教训**：初版只 type-assert *ast.FuncDecl，于是漏报三种形状
// （均已实测确认漏报，见 TestLintCatchesKnownHoles 覆盖）：
//  1. 包级 var = FuncLit，NewCallback 在 FuncLit 体内 —— **正是要堵的泄漏形状**
//  2. import 起了别名，如 import w "golang.org/x/sys/windows"
//  3. NewCallbackCDecl（同样有配额，不在原匹配名单里）
//
// 一个抓不住目标形状的门禁比没有门禁更糟——它给人已经守住了的错觉。
func TestNoNewCallbackInsideFunc(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	violations, err := scanNewCallbackViolations(root)
	if err != nil {
		t.Fatalf("扫描源码失败: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("发现 %d 处在函数体内调用 NewCallback（回调有全局上限 2000 且永不回收，"+
			"在轮询路径上会耗尽配额并 panic 杀死进程）：\n  %s\n\n"+
			"修法：照 player/wesing/proc/memory.go:113-152 的样板——把回调提为包级 var，"+
			"用包级变量传入参/收结果，并用 sync.Mutex 串行化。",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// TestLintCatchesKnownHoles 钉死上面那三种曾经漏报的形状——门禁必须能抓住它们。
// 没有这条，谁重构 scanNewCallbackViolations 都可能把漏洞改回去而无人察觉。
func TestLintCatchesKnownHoles(t *testing.T) {
	cases := map[string]string{
		"包级 var = FuncLit 内 NewCallback": `package p
import "golang.org/x/sys/windows"
var leaky = func() uintptr {
	return windows.NewCallback(func(a, b uintptr) uintptr { return 1 })
}`,
		"别名 import": `package p
import w "golang.org/x/sys/windows"
func f() uintptr { return w.NewCallback(func(a, b uintptr) uintptr { return 1 }) }`,
		"NewCallbackCDecl": `package p
import "golang.org/x/sys/windows"
func f() uintptr { return windows.NewCallbackCDecl(func(a, b uintptr) uintptr { return 1 }) }`,
		"普通函数体内（基础形状）": `package p
import "syscall"
func f() uintptr { return syscall.NewCallback(func(a, b uintptr) uintptr { return 1 }) }`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			v, err := scanNewCallbackViolations(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(v) == 0 {
				t.Errorf("门禁漏报了此形状——它抓不住自己要堵的东西")
			}
		})
	}
}

// TestLintAllowsPackageLevelVar 确认合规写法不被误报（否则门禁会逼人绕过它）。
func TestLintAllowsPackageLevelVar(t *testing.T) {
	src := `package p
import "syscall"
var ok = syscall.NewCallback(func(a, b uintptr) uintptr { return 1 })
var okBlock = struct{}{}
var (
	ok2 = syscall.NewCallback(func(a, b uintptr) uintptr { return 1 })
)`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := scanNewCallbackViolations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) > 0 {
		t.Errorf("合规的包级 var 被误报：%v", v)
	}
}

// scanNewCallbackViolations 扫描 root 下所有 .go，返回违规位置。
//
// 判定方式：全文件 ast.Inspect，维护「当前是否位于某个函数体内」的深度计数。
// 包级 var 的初始化表达式深度为 0 → 合规；一旦进入任何 FuncDecl.Body 或
// FuncLit.Body，深度 > 0 → 该层内的 NewCallback 一律违规。
//
// 不按 import 路径判包名（会被别名骗过），改为按 selector 的方法名匹配
// NewCallback / NewCallbackCDecl —— 这两个名字在 Go 生态里几乎专属于
// syscall 与 x/sys/windows，误报风险远低于漏报代价。
func scanNewCallbackViolations(root string) ([]string, error) {
	var violations []string
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // 解析不了的文件不是本测试的职责
		}

		funcDepth := 0
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				return false
			}
			switch node := n.(type) {
			case *ast.FuncDecl:
				// 进入函数体：用一个独立的子遍历，保证深度正确回退
				if node.Body != nil {
					funcDepth++
					ast.Inspect(node.Body, inspectorFor(fset, root, path, &funcDepth, &violations))
					funcDepth--
				}
				return false
			case *ast.FuncLit:
				funcDepth++
				ast.Inspect(node.Body, inspectorFor(fset, root, path, &funcDepth, &violations))
				funcDepth--
				return false
			case *ast.CallExpr:
				if funcDepth > 0 && isNewCallbackCall(node) {
					violations = append(violations, posOf(fset, root, node.Pos(), "包级表达式"))
				}
			}
			return true
		})
		return nil
	})
	return violations, err
}

func inspectorFor(fset *token.FileSet, root, path string, depth *int, out *[]string) func(ast.Node) bool {
	return func(n ast.Node) bool {
		if n == nil {
			return false
		}
		switch node := n.(type) {
		case *ast.FuncLit:
			*depth++
			ast.Inspect(node.Body, inspectorFor(fset, root, path, depth, out))
			*depth--
			return false
		case *ast.CallExpr:
			if isNewCallbackCall(node) {
				*out = append(*out, posOf(fset, root, node.Pos(), "函数体内"))
			}
		}
		return true
	}
}

func isNewCallbackCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	// 按方法名匹配，不按包名——别名 import 会骗过包名判定
	return sel.Sel.Name == "NewCallback" || sel.Sel.Name == "NewCallbackCDecl"
}

func posOf(fset *token.FileSet, root string, pos token.Pos, where string) string {
	p := fset.Position(pos)
	rel, err := filepath.Rel(root, p.Filename)
	if err != nil {
		rel = p.Filename
	}
	return fmt.Sprintf("%s:%d （%s）", filepath.ToSlash(rel), p.Line, where)
}

// repoRoot 从当前包目录向上找到含 go.mod 的目录。
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
