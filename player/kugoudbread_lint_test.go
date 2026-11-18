package player

// 本文件只测一件事：读酷狗数据库与歌词文件的两个包（player/kugou/assoc、player/kugou/kgdb，含子目录，
// 不含测试文件与 testdata）不加锁、不映射文件、不绕开 assoc.openShared 打开文件。扫的是源码，不测业务逻辑。
//
// # 为什么
//
// 读的是酷狗正在写的库与它的文件（doc/design/kugou-local-lyric-design.md §3「Windows 打开方式」、§6.4）。
// 加锁会让酷狗在我们读取期间的写入得到 SQLITE_BUSY，酷狗是否重试未知；映射存在期间酷狗不能截短文件；
// os.Open 一类的共享模式不含 FILE_SHARE_DELETE，句柄存在期间酷狗改名、删除该文件失败。都可能让酷狗报错，
// 违反 AGENTS.md §0 第 1 条。openShared 以读、写、删除三种共享打开（assoc 的 TestOpenSharedAllowsKuGouOps 实测），
// 这两个包里的打开都应经它。
//
// # 它查什么
//
//   - 名字以 LockFile、CreateFileMapping、MapViewOfFile 开头的标识符或字符串字面量（后者即经
//     NewProc("LockFileEx") 取系统调用入口的写法，本仓库调用 Win32 API 的惯用写法正是这种）；
//   - 引用 os 的 Open、OpenFile、ReadFile、Create、WriteFile，syscall 与 x/sys/windows 的 Open（含别名导入；
//     点导入这几个包即算违规）；
//   - openShared 之外出现的、名字以 CreateFile 开头的标识符或字符串字面量（另起一处 CreateFile 可以带别的共享模式）；
//   - 导入路径含 mmap 的包。
//
// # 它不查什么
//
// 不做类型解析：经别的包转手的打开（如调用另一个包里用 os.Open 的函数）、syscall.SyscallN 以数字给出的入口、
// cgo，查不出。openShared 自身传给 CreateFile 的共享模式由 TestOpenSharedAllowsKuGouOps 守，不由本门禁守。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// kugouDBReadDirs 是受本门禁约束的目录（相对仓库根）。
var kugouDBReadDirs = []string{"player/kugou/assoc", "player/kugou/kgdb"}

// kugouOpenFuncs 是各包里打开文件、共享模式不含 FILE_SHARE_DELETE 的函数。
var kugouOpenFuncs = map[string][]string{
	"os":                       {"Open", "OpenFile", "ReadFile", "Create", "WriteFile"},
	"syscall":                  {"Open"},
	"golang.org/x/sys/windows": {"Open"},
}

// TestKuGouDBReadNoLocksNoMmap 扫 kugouDBReadDirs 的非测试源码，不许出现加锁、映射与绕开 openShared 的打开。
//
// 变异自证（实测，在仓库的临时克隆里逐条注入、跑本用例、还原）：在 player/kugou/assoc/snapshot.go
// 加 `var _ = windows.LockFileEx`、`var _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("LockFile")`、
// `var _ = windows.CreateFileMapping`、`var _ = windows.MapViewOfFile`、`var _ = os.Open`、`var _ = os.OpenFile`、
// `var _ = os.ReadFile`；在 readLyricFile 里把 openShared(path) 换成 os.Open(path)；在 player/kugou/kgdb/sqlite.go
// 与子目录的 kgdb/kgdbtest/encrypt.go 各 import "os" 并加 `var _ = os.ReadFile` 或 `var _ = os.Open`；
// 在 snapshot.go 另写一个调用 windows.CreateFile 的函数；把 openShared 改名为 openShared2——各自注入均红。
func TestKuGouDBReadNoLocksNoMmap(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	var all []string
	for _, dir := range kugouDBReadDirs {
		v, err := scanKuGouDBRead(root, filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			t.Fatalf("扫描 %s 失败: %v", dir, err)
		}
		all = append(all, v...)
	}
	if len(all) > 0 {
		t.Errorf("读酷狗数据库与歌词文件的包里发现 %d 处加锁、映射或绕开 openShared 的打开：\n  %s\n\n"+
			"修法：打开酷狗的文件一律经 assoc.openShared（读、写、删除三种共享，不加锁、不映射）；"+
			"取文件大小、判断存在用 os.Stat。设计见 doc/design/kugou-local-lyric-design.md §6.4。",
			len(all), strings.Join(all, "\n  "))
	}
}

// TestKuGouDBReadLintCatchesShapes 钉住门禁自身能抓住各种写法，并放过合规的写法与测试文件。
//
// 变异自证（实测）：去掉字符串字面量的检查，经 NewProc 的三例红；不认别名导入，别名、点导入、windows.Open
// 三例红；不报点导入，点导入一例红；不区分是否在 openShared 里，合规一例红（TestKuGouDBReadNoLocksNoMmap 同时红）；
// 只查调用、不查引用，「os.Open 取函数值」一例红（「CreateFile 取函数值」由标识符检查兜住）；不跳过 _test.go、
// 不跳过 testdata，各自使「测试文件与 testdata」一例红；放过名为 openShared 的方法、不查 mmap 导入，各自对应一例红。
func TestKuGouDBReadLintCatchesShapes(t *testing.T) {
	bad := map[string]string{
		"LockFileEx": `package p
import "golang.org/x/sys/windows"
func f(h windows.Handle) error { return windows.LockFileEx(h, 0, 0, 1, 0, new(windows.Overlapped)) }`,
		"LockFile 经 NewProc": `package p
import "golang.org/x/sys/windows"
var procLockFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("LockFile")`,
		"CreateFileMapping": `package p
import "golang.org/x/sys/windows"
func f(h windows.Handle) (windows.Handle, error) { return windows.CreateFileMapping(h, nil, windows.PAGE_READONLY, 0, 0, nil) }`,
		"MapViewOfFile 经 NewProc": `package p
import "golang.org/x/sys/windows"
var procMap = windows.NewLazySystemDLL("kernel32.dll").NewProc("MapViewOfFileEx")`,
		"MapViewOfFile": `package p
import "golang.org/x/sys/windows"
func f(m windows.Handle) (uintptr, error) { return windows.MapViewOfFile(m, windows.FILE_MAP_READ, 0, 0, 0) }`,
		"os.Open": `package p
import "os"
func f(p string) (*os.File, error) { return os.Open(p) }`,
		"os.OpenFile": `package p
import "os"
func f(p string) (*os.File, error) { return os.OpenFile(p, os.O_RDONLY, 0) }`,
		"os.ReadFile": `package p
import "os"
func f(p string) ([]byte, error) { return os.ReadFile(p) }`,
		"os.Create": `package p
import "os"
func f(p string) (*os.File, error) { return os.Create(p) }`,
		"os.WriteFile": `package p
import "os"
func f(p string) error { return os.WriteFile(p, nil, 0) }`,
		"os 别名导入": `package p
import xos "os"
func f(p string) ([]byte, error) { return xos.ReadFile(p) }`,
		"os 点导入": `package p
import . "os"
func f(p string) (*File, error) { return Open(p) }`,
		"os.Open 取函数值": `package p
import "os"
var open = os.Open`,
		"syscall.Open": `package p
import "syscall"
func f(p string) (syscall.Handle, error) { return syscall.Open(p, syscall.O_RDONLY, 0) }`,
		"windows.Open": `package p
import w "golang.org/x/sys/windows"
func f(p string) (w.Handle, error) { return w.Open(p, w.O_RDONLY, 0) }`,
		"openShared 之外的 CreateFile": `package p
import "golang.org/x/sys/windows"
func openOther(p *uint16) (windows.Handle, error) {
	return windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, 0, 0)
}`,
		"CreateFile 取函数值": `package p
import "golang.org/x/sys/windows"
var createFile = windows.CreateFile`,
		"CreateFileW 经 NewProc": `package p
import "golang.org/x/sys/windows"
var procCreateFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateFileW")`,
		"方法名为 openShared": `package p
import "golang.org/x/sys/windows"
type s struct{}
func (s) openShared(p *uint16) (windows.Handle, error) {
	return windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, 0, 0)
}`,
		"导入 mmap 包": `package p
import "golang.org/x/exp/mmap"
func f(p string) (*mmap.ReaderAt, error) { return mmap.Open(p) }`,
	}
	for name, src := range bad {
		t.Run(name, func(t *testing.T) {
			if v := scanSource(t, "x.go", src); len(v) == 0 {
				t.Errorf("门禁漏报了此形状——它抓不住自己要堵的东西")
			}
		})
	}

	t.Run("合规", func(t *testing.T) {
		src := `package p
import (
	"io"
	"os"
	"golang.org/x/sys/windows"
)
type db struct{}
func (db) Open() error { return nil }
func openShared(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
func size(p string) (int64, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
func read(f *os.File, d db) ([]byte, error) {
	_ = d.Open()
	open := "open %s: lock-free"
	_ = open
	return io.ReadAll(f)
}`
		if v := scanSource(t, "x.go", src); len(v) > 0 {
			t.Errorf("合规写法被误报：%v", v)
		}
	})

	t.Run("测试文件与 testdata", func(t *testing.T) {
		dir := t.TempDir()
		bad := []byte("package p\nimport \"os\"\nfunc f(p string) ([]byte, error) { return os.ReadFile(p) }\n")
		for _, p := range []string{"x_test.go", filepath.Join("testdata", "x.go")} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, p), bad, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		v, err := scanKuGouDBRead(dir, dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(v) > 0 {
			t.Errorf("测试文件与 testdata 不在门禁范围内，却被报出：%v", v)
		}
	})
}

// scanSource 把 src 写进临时目录下的 name，返回门禁的报告。
func scanSource(t *testing.T, name, src string) []string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := scanKuGouDBRead(dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// scanKuGouDBRead 扫 dir 下（含子目录，跳过 testdata）的非测试 .go 文件，返回违规处，位置相对 root。
// 解析失败即返回错误：门禁看不懂的文件不能当作合规。
func scanKuGouDBRead(root, dir string) ([]string, error) {
	var out []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		out = append(out, kugouDBReadViolations(fset, root, f)...)
		return nil
	})
	return out, err
}

func kugouDBReadViolations(fset *token.FileSet, root string, f *ast.File) []string {
	var out []string
	report := func(n ast.Node, what string) { out = append(out, posOf(fset, root, n.Pos(), what)) }

	opens := map[string][]string{} // 本文件里的包名 → 该包打开文件的函数
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if strings.Contains(strings.ToLower(p), "mmap") {
			report(imp, "导入映射文件的包 "+p)
		}
		funcs, ok := kugouOpenFuncs[p]
		if !ok {
			continue
		}
		local := path.Base(p)
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == "." {
			report(imp, "点导入 "+p+"，其打开文件的函数查不出")
		}
		opens[local] = funcs
	}

	for _, decl := range f.Decls {
		fd, _ := decl.(*ast.FuncDecl)
		inOpenShared := fd != nil && fd.Recv == nil && fd.Name.Name == "openShared"
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if what := bannedKuGouName(n.Name, inOpenShared); what != "" {
					report(n, what+" "+n.Name)
				}
			case *ast.BasicLit:
				if s, err := strconv.Unquote(n.Value); n.Kind == token.STRING && err == nil {
					if what := bannedKuGouName(s, inOpenShared); what != "" {
						report(n, what+" "+n.Value)
					}
				}
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && slices.Contains(opens[x.Name], n.Sel.Name) {
					report(n, "共享模式不含删除的打开 "+x.Name+"."+n.Sel.Name)
				}
			}
			return true
		})
	}
	return out
}

// bannedKuGouName 判断标识符或字符串 s 是否为不许出现的系统调用名，是则返回原因。
func bannedKuGouName(s string, inOpenShared bool) string {
	for _, p := range []string{"LockFile", "CreateFileMapping", "MapViewOfFile"} {
		if strings.HasPrefix(s, p) {
			return "加锁或映射文件"
		}
	}
	if strings.HasPrefix(s, "CreateFile") && !inOpenShared {
		return "在 openShared 之外打开文件"
	}
	return ""
}
