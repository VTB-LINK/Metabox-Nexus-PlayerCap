package qqmusic

// 本文件只测一件事：读 QQ 内存里当前歌曲记录的代码（songrecord.go）对 QQ 的进程只读（设计 §6.4 第 4 步「谁来读」）。
// 扫的是源码，不测业务逻辑。
//
// # 为什么
//
// QQMusicMem 的句柄是 PROCESS_ALL_ACCESS，mem.go 里有写内存与注入的实现（AGENTS.md §7.1「只读」的边界、§3.6）。读记录
// 跑在读取者 goroutine 里、扫的是 QQ 的整片私有可写内存，写错一处即坏主播的 QQ（§0 第 1 条）。所以它只用会话开始时复制的
// 只读句柄，只调用 VirtualQueryEx、ReadProcessMemory、DuplicateHandle、CloseHandle（另有取本进程伪句柄的 CurrentProcess，
// 不是系统调用），复制时访问权限恰为 PROCESS_QUERY_INFORMATION | PROCESS_VM_READ、不继承、不带 DUPLICATE_SAME_ACCESS。
//
// # 它不查什么
//
// 不做类型解析：经别的文件、别的包转手的调用查不出；QQMusicMem 的方法按名字认（取自本包源码里接收者为 QQMusicMem 的方法），
// 别的类型的同名方法也会被报出。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// songRecordWindowsAllowed 是 songrecord.go 里 windows 包可出现的名字：四个进程与句柄调用、CurrentProcess，以及用到的类型与
// 常量（ERROR_INVALID_PARAMETER 用于判地址空间到头）。
var songRecordWindowsAllowed = []string{
	"VirtualQueryEx", "ReadProcessMemory", "DuplicateHandle", "CloseHandle", "CurrentProcess",
	"Handle", "MemoryBasicInformation", "MEM_COMMIT", "PAGE_READWRITE", "PROCESS_QUERY_INFORMATION", "PROCESS_VM_READ",
	"ERROR_INVALID_PARAMETER",
}

// songRecordMemAllowed 是 songrecord.go 可调用的 QQMusicMem 方法：只判版本，不读内存。
var songRecordMemAllowed = []string{"songRecordEnabled", "Version"}

// songRecordBannedSelectors 是动态取系统调用的方法名（syscall 与 x/sys/windows 的 LazyDLL、DLL）。
var songRecordBannedSelectors = []string{"NewProc", "NewLazyDLL", "NewLazySystemDLL", "LoadDLL", "MustLoadDLL", "FindProc",
	"MustFindProc", "Call", "SyscallN", "Syscall", "Syscall6", "Syscall9", "Syscall12", "Syscall15"}

// packageGlobals 扫本包顶层的非测试文件，返回 LazyProc 一类的包级变量（初始化为 xxx.NewProc(…) 的）与接收者为
// QQMusicMem 的方法名。
func packageGlobals(t *testing.T) (procs, memMethods []string) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, s := range d.Specs {
					vs, ok := s.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, v := range vs.Values {
						if c, ok := v.(*ast.CallExpr); ok {
							if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewProc" && i < len(vs.Names) {
								procs = append(procs, vs.Names[i].Name)
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv != nil && len(d.Recv.List) == 1 && strings.TrimPrefix(types.ExprString(d.Recv.List[0].Type), "*") == "QQMusicMem" {
					memMethods = append(memMethods, d.Name.Name)
				}
			}
		}
	}
	if len(procs) == 0 || !slices.Contains(procs, "procWriteProcessMemory") || !slices.Contains(memMethods, "WriteBytes") {
		t.Fatalf("没收集到包级的 LazyProc（%d 个）或 QQMusicMem 的方法（%d 个）—— 门禁失效", len(procs), len(memMethods))
	}
	return procs, memMethods
}

// songRecordReadOnlyProblems 检查 src（songrecord.go 的源码），返回发现的问题。
func songRecordReadOnlyProblems(src string, procs, memMethods []string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "songrecord.go", src, 0)
	if err != nil {
		return []string{"解析失败: " + err.Error()}
	}
	var probs []string
	bad := func(n ast.Node, format string, a ...any) {
		probs = append(probs, fset.Position(n.Pos()).String()+": "+fmt.Sprintf(format, a...))
	}
	var wins []string // windows 包在本文件里的名字（可能有别名、导入不止一次）
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		local := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			local = imp.Name.Name
		}
		switch {
		case path == "syscall":
			bad(imp, "导入了 syscall")
		case path == "golang.org/x/sys/windows" && (local == "." || local == "_"):
			bad(imp, "以 %s 导入 windows，其调用查不出", local)
		case path == "golang.org/x/sys/windows":
			wins = append(wins, local)
		}
	}
	winSel := func(e ast.Expr) (*ast.SelectorExpr, string) {
		if sel, ok := e.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && slices.Contains(wins, x.Name) {
				return sel, x.Name
			}
		}
		return nil, ""
	}
	dups := 0
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			sel, win := winSel(n)
			switch {
			case sel != nil && !slices.Contains(songRecordWindowsAllowed, n.Sel.Name):
				bad(n, "%s.%s 不在只读的四个调用之内", win, n.Sel.Name)
			case slices.Contains(songRecordBannedSelectors, n.Sel.Name):
				bad(n, "动态取系统调用：.%s", n.Sel.Name)
			case slices.Contains(memMethods, n.Sel.Name) && !slices.Contains(songRecordMemAllowed, n.Sel.Name):
				bad(n, "用了 QQMusicMem 的方法 %s（其句柄可写，且由轮询线程清零）", n.Sel.Name)
			}
		case *ast.Ident:
			if slices.Contains(procs, n.Name) {
				bad(n, "用了包级的 LazyProc %s", n.Name)
			}
		case *ast.CallExpr:
			sel, win := winSel(n.Fun)
			if sel == nil || sel.Sel.Name != "DuplicateHandle" {
				break
			}
			dups++
			if len(n.Args) != 7 {
				bad(n, "DuplicateHandle 的实参个数 %d，want 7", len(n.Args))
				break
			}
			want := []string{4: win + ".PROCESS_QUERY_INFORMATION | " + win + ".PROCESS_VM_READ", 5: "false", 6: "0"}
			for i := 4; i < 7; i++ {
				if got := types.ExprString(n.Args[i]); got != want[i] {
					bad(n, "DuplicateHandle 的第 %d 个实参为 %s，want %s", i+1, got, want[i])
				}
			}
		}
		return true
	})
	if dups == 0 {
		probs = append(probs, "没找到 windows.DuplicateHandle 的调用 —— 门禁失效")
	}
	return probs
}

// 变异自证见 TestSongRecordReadOnlyCatchesMutations；另在仓库的隔离副本里实测，songrecord.go 加一处 WriteProcessMemory、
// 访问参数改为 PROCESS_ALL_ACCESS，本用例都红。
func TestSongRecordReadOnly(t *testing.T) {
	procs, memMethods := packageGlobals(t)
	src, err := os.ReadFile("songrecord.go")
	if err != nil {
		t.Fatal(err)
	}
	if probs := songRecordReadOnlyProblems(string(src), procs, memMethods); len(probs) > 0 {
		t.Errorf("songrecord.go 对 QQ 的进程不是只读：\n  %s\n\n读记录只经复制的只读句柄，只调用 VirtualQueryEx、ReadProcessMemory、"+
			"DuplicateHandle、CloseHandle；设计见 doc/design/qqmusic-lyric-association-design.md §6.4 第 4 步。", strings.Join(probs, "\n  "))
	}
}

// 以改写过的 songrecord.go 自证门禁拦得住：加一处 WriteProcessMemory（直调、经 LazyProc、经别名导入）；访问参数改为
// PROCESS_ALL_ACCESS（x/sys 的与本包的）、多加 PROCESS_VM_WRITE；带 DUPLICATE_SAME_ACCESS；可继承；导入 syscall；
// 用 QQMusicMem 的方法读内存；删掉 DuplicateHandle。每一种都须报出，且改写确实发生了。
func TestSongRecordReadOnlyCatchesMutations(t *testing.T) {
	procs, memMethods := packageGlobals(t)
	raw, err := os.ReadFile("songrecord.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	access := "windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ"
	if !strings.Contains(src, access) {
		access = "windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_READ"
	}
	dupTail := access + ", false, 0)"
	for _, c := range []struct {
		name     string
		old, new string // old 为空即在文末追加 new
		mention  string // 报告里须出现的字样
	}{
		{"直调 WriteProcessMemory", "", "\nfunc mutant(h windows.Handle, b []byte) { var n uintptr; windows.WriteProcessMemory(h, 0, &b[0], 1, &n) }\n", "WriteProcessMemory"},
		{"经 LazyProc 写", "", "\nfunc mutant() { procWriteProcessMemory.Call(0, 0, 0, 0, 0) }\n", "procWriteProcessMemory"},
		{"经别名导入写", `"golang.org/x/sys/windows"`, `"golang.org/x/sys/windows"` + "\n\tw2 \"golang.org/x/sys/windows\"", "w2"},
		{"访问参数为 x/sys 的 PROCESS_ALL_ACCESS", access, "windows.PROCESS_ALL_ACCESS", "PROCESS_ALL_ACCESS"},
		{"访问参数为本包的 PROCESS_ALL_ACCESS", access, "PROCESS_ALL_ACCESS", "PROCESS_ALL_ACCESS"},
		{"多加 PROCESS_VM_WRITE", access, access + "|windows.PROCESS_VM_WRITE", "PROCESS_VM_WRITE"},
		{"DUPLICATE_SAME_ACCESS", dupTail, access + ", false, windows.DUPLICATE_SAME_ACCESS)", "DUPLICATE_SAME_ACCESS"},
		{"可继承", dupTail, access + ", true, 0)", "true"},
		{"导入 syscall", "import (", "import (\n\t\"syscall\"", "syscall"},
		{"用 QQMusicMem 的方法读内存", "", "\nfunc mutant(m *QQMusicMem) uint32 { return m.ReadUint32(0) }\n", "ReadUint32"},
		{"删掉 DuplicateHandle", "windows.DuplicateHandle(", "duplicateHandle(", "DuplicateHandle"},
	} {
		t.Run(c.name, func(t *testing.T) {
			mutated := src + c.new
			if c.old != "" {
				mutated = strings.Replace(src, c.old, c.new, 1)
			}
			if c.name == "经别名导入写" {
				mutated += "\nfunc mutant(h w2.Handle, b []byte) { var n uintptr; w2.WriteProcessMemory(h, 0, &b[0], 1, &n) }\n"
			}
			if mutated == src {
				t.Fatal("改写没有发生 —— 用例失效")
			}
			probs := songRecordReadOnlyProblems(mutated, procs, memMethods)
			if !slices.ContainsFunc(probs, func(p string) bool { return strings.Contains(p, c.mention) }) {
				t.Fatalf("门禁漏报，报告 %q 里没有 %q", probs, c.mention)
			}
		})
	}
}
