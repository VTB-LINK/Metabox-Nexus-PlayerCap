//go:build windows

package wasapi

// 本文件只测一件事：wasapi_windows.go 交给 COM / Win32 的 Go 变量地址不依赖栈位置（issue #103）。
// 它是 AST 门禁，扫的是源码，不调用任何 COM。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestComCallUintptrEscapes 钉住两条约定：
//
//  1. comCall 的文档注释末行是 //go:uintptrescapes。
//  2. uintptr(unsafe.Pointer(…)) 只就地写在 comCall、proc.Call（syscall 与 x/sys/windows 的 Call
//     带 //go:uintptrescapes）或 syscall.SyscallN（带 //go:uintptrkeepalive 且 nosplit）的实参里；
//     门禁另放行 *p = … 这种写给 C 的出参。
//
// 为什么：unsafe.Pointer 规则 4 只保证汇编实现的函数的就地转换实参保活且不移动，//go:uintptrescapes
// 把同样的保证给到带该指令的 Go 函数（cmd/compile 文档）。comCall 原是不带指令的 Go 函数，调用点 &x 的
// x 留在栈上；comCall 入口或其中的 make/append 若触发栈增长，栈被整体搬走，已转成整数的地址不随之修正，
// COM 写出参会写到旧栈上。先把转换结果存进变量或结构体字段再传，指令同样保护不到（编译器只认调用实参
// 处的转换），pv.pBlobData 原先就是这种写法。
//
// 已知盲区：只认字面的 uintptr(unsafe.Pointer(…))，unsafe.Pointer 类型的变量直接转 uintptr（如 comCall
// 内的 uintptr(obj)，指向 C 侧的 COM 对象）认不出；callee 按名字匹配，不做类型解析；*p = … 一律放行，
// 所写对象是否在堆上不查（现有一处 completionHandlerQueryInterface，对象由 pin map 钉住）。
func TestComCallUintptrEscapes(t *testing.T) {
	v, err := lintComCall("wasapi_windows.go", nil)
	if err != nil {
		t.Fatalf("解析 wasapi_windows.go 失败: %v", err)
	}
	if len(v) > 0 {
		t.Errorf("发现 %d 处 Go 变量地址依赖栈位置：\n  %s", len(v), strings.Join(v, "\n  "))
	}
}

// compliantComCall 是带指令的 comCall 声明，供下面的合成用例拼接。
const compliantComCall = `package p

import (
	"syscall"
	"unsafe"
)

// comCall 调 vtable 方法。
//
//go:uintptrescapes
func comCall(obj unsafe.Pointer, idx int, args ...uintptr) uintptr { return 0 }
`

// TestComCallLintCatchesShapes 钉住门禁必须抓住的形状，防止重构 lintComCall 时把洞改回去。
func TestComCallLintCatchesShapes(t *testing.T) {
	cases := map[string]string{
		"comCall 缺指令": `package p
import "unsafe"
// comCall 调 vtable 方法。
func comCall(obj unsafe.Pointer, idx int, args ...uintptr) uintptr { return 0 }`,
		"找不到 comCall": `package p
func other() {}`,
		"短变量声明后再传": compliantComCall + `
func f(o unsafe.Pointer) { var n uint32; p := uintptr(unsafe.Pointer(&n)); comCall(o, 0, p) }`,
		"var 声明后再传": compliantComCall + `
func f(o unsafe.Pointer) { var n uint32; var p = uintptr(unsafe.Pointer(&n)); comCall(o, 0, p) }`,
		"赋给已有变量后再传": compliantComCall + `
func f(o unsafe.Pointer) { var n uint32; var p uintptr; p = uintptr(unsafe.Pointer(&n)); comCall(o, 0, p) }`,
		"存进结构体字段": compliantComCall + `
type blob struct{ data uintptr }
func f() blob { var n uint32; return blob{data: uintptr(unsafe.Pointer(&n))} }`,
		"交给不带指令的函数": compliantComCall + `
func helper(a uintptr) {}
func f() { var n uint32; helper(uintptr(unsafe.Pointer(&n))) }`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			v, err := lintComCall("x.go", src)
			if err != nil {
				t.Fatal(err)
			}
			if len(v) == 0 {
				t.Errorf("门禁漏报了此形状")
			}
		})
	}
}

// TestComCallLintAllowsInPlace 确认合规写法不被误报。
func TestComCallLintAllowsInPlace(t *testing.T) {
	src := compliantComCall + `
type proc struct{}
func (proc) Call(a ...uintptr) (uintptr, uintptr, error) { return 0, 0, nil }
func f(o unsafe.Pointer, pr proc, ppv *uintptr) {
	var n uint32
	var x struct{}
	comCall(o, 0, uintptr(unsafe.Pointer(&n)))
	pr.Call(uintptr(unsafe.Pointer(&x)), 0)
	syscall.SyscallN(0, uintptr(unsafe.Pointer(&n)))
	*ppv = uintptr(unsafe.Pointer(&x))
}`
	v, err := lintComCall("x.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) > 0 {
		t.Errorf("合规写法被误报：%v", v)
	}
}

// lintComCall 解析 filename（src 非 nil 时解析 src），返回违规位置。
func lintComCall(filename string, src any) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var out []string
	report := func(pos token.Pos, msg string) {
		p := fset.Position(pos)
		out = append(out, fmt.Sprintf("%s:%d %s", p.Filename, p.Line, msg))
	}

	// (a) comCall 的文档注释里有 //go:uintptrescapes。
	var decl *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "comCall" {
			decl = fd
		}
	}
	switch {
	case decl == nil:
		report(f.Package, "找不到 comCall（改名或挪走后本门禁失效，需同步修改本测试）")
	case !hasDirective(decl.Doc, "//go:uintptrescapes"):
		report(decl.Pos(), "comCall 的文档注释末尾缺 //go:uintptrescapes")
	}

	// (b) uintptr(unsafe.Pointer(…)) 只就地出现在受保护调用的实参里，或作为 *p = … 的右值。
	allowed := map[*ast.CallExpr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			switch calleeName(n.Fun) {
			case "comCall", "Call", "SyscallN":
				for _, a := range n.Args {
					if c, ok := a.(*ast.CallExpr); ok && isUintptrOfPointer(c) {
						allowed[c] = true
					}
				}
			}
		case *ast.AssignStmt:
			if n.Tok != token.ASSIGN || len(n.Lhs) != len(n.Rhs) {
				break
			}
			for i, l := range n.Lhs {
				if _, ok := l.(*ast.StarExpr); !ok {
					continue
				}
				if c, ok := n.Rhs[i].(*ast.CallExpr); ok && isUintptrOfPointer(c) {
					allowed[c] = true
				}
			}
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && isUintptrOfPointer(c) && !allowed[c] {
			report(c.Pos(), "uintptr(unsafe.Pointer(…)) 不是 comCall / proc.Call / syscall.SyscallN 的就地实参")
		}
		return true
	})
	return out, nil
}

func hasDirective(doc *ast.CommentGroup, directive string) bool {
	if doc == nil {
		return false
	}
	for _, c := range doc.List {
		if c.Text == directive {
			return true
		}
	}
	return false
}

func calleeName(fun ast.Expr) string {
	switch fn := fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// isUintptrOfPointer 判断 c 是否为 uintptr(unsafe.Pointer(…))。
func isUintptrOfPointer(c *ast.CallExpr) bool {
	id, ok := c.Fun.(*ast.Ident)
	if !ok || id.Name != "uintptr" || len(c.Args) != 1 {
		return false
	}
	inner, ok := c.Args[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := inner.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Pointer" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "unsafe"
}
