package qqmusic

// 本文件只测一件事：读 QQ 音乐文件的两个包（player/qqmusic、player/qqmusic/assoc，不含测试文件）不经 os.Open、
// os.OpenFile、os.ReadFile 打开文件。扫的是源码，不测业务逻辑。
//
// # 为什么
//
// 读的是 QQ 正在使用的关联表、歌词索引、配置与歌词文件（设计 §6.2）。这三个函数的共享模式不含 FILE_SHARE_DELETE，
// 句柄存在期间 QQ 以改名替换、改名、删除该文件都会失败（readshared_test.go 的对照用例实测），可能让 QQ 报错。
// QQ 的文件一律经 readShared（读、写、删除三种共享，不加锁、不映射）。
//
// # 它不查什么
//
// 不做类型解析：经别的包转手的打开、直接调用 windows.CreateFile 或 syscall 的打开，查不出。readShared 自身的共享模式
// 由 TestOpenSharedAllowsQQOps 守，不由本门禁守。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// qqSharedReadDirs 是受本门禁约束的目录（相对本包目录），只扫各自的顶层。
var qqSharedReadDirs = []string{".", "assoc"}

// qqBannedOpens 是 os 包里共享模式不含删除的打开函数。
var qqBannedOpens = []string{"Open", "OpenFile", "ReadFile"}

// 变异自证（实测，在仓库的临时拷贝里注入、跑本用例）：localfile.go 的 readShared 改用 os.ReadFile 即红。
func TestQQFilesReadOnlyThroughReadShared(t *testing.T) {
	var all []string
	for _, dir := range qqSharedReadDirs {
		v, err := scanQQOpens(dir)
		if err != nil {
			t.Fatalf("扫描 %s 失败: %v", dir, err)
		}
		all = append(all, v...)
	}
	if len(all) > 0 {
		t.Errorf("读 QQ 音乐文件的包里发现 %d 处共享模式不含删除的打开：\n  %s\n\n"+
			"修法：打开 QQ 的文件一律经 readShared；取文件大小、判断存在用 os.Stat。设计见 doc/design/qqmusic-lyric-association-design.md §6.2。",
			len(all), strings.Join(all, "\n  "))
	}
}

// 钉住门禁自身能抓住各种写法，并放过合规的写法与测试文件。
// 变异自证（实测）：不认别名导入，别名一例红；不报点导入，点导入一例红；不跳过 _test.go，测试文件一例红。
func TestQQSharedLintCatchesShapes(t *testing.T) {
	for name, src := range map[string]string{
		"os.Open": `package p
import "os"
func f(p string) (*os.File, error) { return os.Open(p) }`,
		"os.OpenFile": `package p
import "os"
func f(p string) (*os.File, error) { return os.OpenFile(p, os.O_RDONLY, 0) }`,
		"os.ReadFile": `package p
import "os"
func f(p string) ([]byte, error) { return os.ReadFile(p) }`,
		"别名导入": `package p
import xos "os"
func f(p string) ([]byte, error) { return xos.ReadFile(p) }`,
		"点导入": `package p
import . "os"
func f(p string) (*File, error) { return Open(p) }`,
		"取函数值": `package p
import "os"
var read = os.ReadFile`,
	} {
		if v := scanQQSource(t, "x.go", src); len(v) == 0 {
			t.Errorf("%s: 门禁漏报了此形状", name)
		}
	}

	ok := `package p
import (
	"io"
	"os"
)
type db struct{}
func (db) Open() error { return nil }
func f(p string, d db) ([]byte, error) {
	if _, err := os.Stat(p); err != nil {
		return nil, err
	}
	_ = d.Open()
	_ = "os.ReadFile"
	return io.ReadAll(os.NewFile(0, p))
}`
	if v := scanQQSource(t, "x.go", ok); len(v) > 0 {
		t.Errorf("合规写法被误报：%v", v)
	}
	if v := scanQQSource(t, "x_test.go", "package p\nimport \"os\"\nvar read = os.ReadFile\n"); len(v) > 0 {
		t.Errorf("测试文件不在门禁范围内，却被报出：%v", v)
	}
}

// scanQQSource 把 src 写进临时目录下的 name，返回门禁的报告。
func scanQQSource(t *testing.T, name, src string) []string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := scanQQOpens(dir)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// scanQQOpens 扫 dir 顶层的非测试 .go 文件，返回违规处。解析失败即返回错误：门禁看不懂的文件不能当作合规。
func scanQQOpens(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		local := ""
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p != "os" {
				continue
			}
			local = "os"
			if imp.Name != nil {
				local = imp.Name.Name
			}
			if local == "." {
				out = append(out, fset.Position(imp.Pos()).String()+": 点导入 os，其打开文件的函数查不出")
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := s.X.(*ast.Ident); ok && x.Name == local && slices.Contains(qqBannedOpens, s.Sel.Name) {
					out = append(out, fset.Position(s.Pos()).String()+": "+x.Name+"."+s.Sel.Name)
				}
			}
			return true
		})
	}
	return out, nil
}
