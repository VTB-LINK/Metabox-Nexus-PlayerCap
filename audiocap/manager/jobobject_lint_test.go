//go:build windows

package manager

// 本文件只测一件事：jobobject_windows.go 交给 Win32 的 Go 变量地址不依赖栈位置（issue #104）。
// 它是 AST 门禁，扫的是源码，不调用任何 Win32。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestJobObjectUintptrEscapes 钉住两条约定：
//
//  1. setJobInformation 的文档注释末行是 //go:uintptrescapes。
//  2. uintptr(unsafe.Pointer(…)) 只就地写在 setJobInformation 的实参里。
//
// 为什么：x/sys/windows 的 SetInformationJobObject 是不带 //go:uintptrescapes 的 Go 函数，调用点 &x 的
// x 留在栈上；转换与系统调用之间若栈增长，已转成整数的地址不随之修正，内核读到旧栈上的内容。
// 先把转换结果存进变量或结构体字段再传，指令同样保护不到（编译器只认调用实参处的转换）。
//
// 已知盲区：只认字面的 uintptr(unsafe.Pointer(…))；只扫 jobobject_windows.go，本包其它文件不在范围内。
func TestJobObjectUintptrEscapes(t *testing.T) {
	v, err := lintJobObject("jobobject_windows.go", nil)
	if err != nil {
		t.Fatalf("解析 jobobject_windows.go 失败: %v", err)
	}
	if len(v) > 0 {
		t.Errorf("发现 %d 处 Go 变量地址依赖栈位置：\n  %s", len(v), strings.Join(v, "\n  "))
	}
}

// compliantSetJobInformation 是带指令的 setJobInformation 声明，供下面的合成用例拼接。
const compliantSetJobInformation = `package p

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// setJobInformation 调 windows.SetInformationJobObject。
//
//go:uintptrescapes
func setJobInformation(job windows.Handle, class uint32, info uintptr, size uint32) error { return nil }
`

// TestJobObjectLintCatchesShapes 钉住门禁必须抓住的形状，防止重构 lintJobObject 时把洞改回去。
func TestJobObjectLintCatchesShapes(t *testing.T) {
	cases := map[string]string{
		"setJobInformation 缺指令": `package p
// setJobInformation 调 windows.SetInformationJobObject。
func setJobInformation(job uintptr, class uint32, info uintptr, size uint32) error { return nil }`,
		"指令不在文档注释末行": `package p
// setJobInformation 调 windows.SetInformationJobObject。
//
//go:uintptrescapes
// 多出的一行。
func setJobInformation(job uintptr, class uint32, info uintptr, size uint32) error { return nil }`,
		"找不到 setJobInformation": `package p
func other() {}`,
		"同名方法不算": `package p
type j struct{}
//go:uintptrescapes
func (j) setJobInformation(job uintptr, class uint32, info uintptr, size uint32) error { return nil }`,
		"短变量声明后再传": compliantSetJobInformation + `
func f(h windows.Handle) { var n uint32; p := uintptr(unsafe.Pointer(&n)); setJobInformation(h, 9, p, 4) }`,
		"赋给已有变量后再传": compliantSetJobInformation + `
func f(h windows.Handle) { var n uint32; var p uintptr; p = uintptr(unsafe.Pointer(&n)); setJobInformation(h, 9, p, 4) }`,
		"存进结构体字段": compliantSetJobInformation + `
type req struct{ info uintptr }
func f() req { var n uint32; return req{info: uintptr(unsafe.Pointer(&n))} }`,
		"交给不带指令的函数": compliantSetJobInformation + `
func f(h windows.Handle) { var n uint32; windows.SetInformationJobObject(h, 9, uintptr(unsafe.Pointer(&n)), 4) }`,
		"经选择器调用同名函数": compliantSetJobInformation + `
type j struct{}
func (j) setJobInformation(a, b, c, d uintptr) {}
func f(x j) { var n uint32; x.setJobInformation(0, 9, uintptr(unsafe.Pointer(&n)), 4) }`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			v, err := lintJobObject("x.go", src)
			if err != nil {
				t.Fatal(err)
			}
			if len(v) == 0 {
				t.Errorf("门禁漏报了此形状")
			}
		})
	}
}

// TestJobObjectLintAllowsInPlace 确认合规写法不被误报。
func TestJobObjectLintAllowsInPlace(t *testing.T) {
	src := compliantSetJobInformation + `
func f(h windows.Handle) error {
	var info struct{ a, b uint64 }
	return setJobInformation(h, 9, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
}`
	v, err := lintJobObject("x.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) > 0 {
		t.Errorf("合规写法被误报：%v", v)
	}
}

// lintJobObject 解析 filename（src 非 nil 时解析 src），返回违规位置。
func lintJobObject(filename string, src any) ([]string, error) {
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

	// (a) setJobInformation 的文档注释末行是 //go:uintptrescapes。
	var decl *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "setJobInformation" {
			decl = fd
		}
	}
	switch {
	case decl == nil:
		report(f.Package, "找不到 setJobInformation（改名或挪走后本门禁失效，需同步修改本测试）")
	case decl.Doc == nil || decl.Doc.List[len(decl.Doc.List)-1].Text != "//go:uintptrescapes":
		report(decl.Pos(), "setJobInformation 的文档注释末行不是 //go:uintptrescapes")
	}

	// (b) uintptr(unsafe.Pointer(…)) 只就地出现在 setJobInformation 的实参里。
	allowed := map[*ast.CallExpr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "setJobInformation" {
			for _, a := range c.Args {
				if ac, ok := a.(*ast.CallExpr); ok && isUintptrOfPointer(ac) {
					allowed[ac] = true
				}
			}
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && isUintptrOfPointer(c) && !allowed[c] {
			report(c.Pos(), "uintptr(unsafe.Pointer(…)) 不是 setJobInformation 的就地实参")
		}
		return true
	})
	return out, nil
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
