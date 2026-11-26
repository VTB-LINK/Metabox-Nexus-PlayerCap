package player

// 本文件只测一件事：player/kugou 子树（非测试源码，跳过 testdata）对酷狗进程内存保持严格只读。
// 扫的是源码，不测业务逻辑。
//
// # 为什么
//
// 读取运行中的酷狗进程内存直接触碰目标进程地址空间，任何写内存、注入或过宽句柄权限都会危及主播正在进行的直播
// （AGENTS.md §0 第 1 条）。因此整个 player/kugou 子树不得出现写内存/注入 API；读内存与模块枚举限定在
// player/kugou/assoc/mem.go 一处，且该文件对 golang.org/x/sys/windows 的引用严格收口到 10 个只读标识符；
// 全子树所有 OpenProcess 调用限定在明确的文件与最小权限位掩码内，且 inheritHandle 恒为 false。
//
// # 它不查什么
//
// 不做类型解析：经其他包转手的调用、局部标识符遮蔽包名或 cgo 查不出。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const kugouMemFileRel = "player/kugou/assoc/mem.go"

// kugouOpenProcessAccess 是 player/kugou 子树允许调用 OpenProcess 的文件及其精确访问权限掩码。
var kugouOpenProcessAccess = map[string]uint32{
	"player/kugou/assoc/mem.go":         0x0410,     // PROCESS_QUERY_INFORMATION | PROCESS_VM_READ
	"player/kugou/watchdog/confirm.go":  0x1000,     // PROCESS_QUERY_LIMITED_INFORMATION
	"player/kugou/watchdog/watchdog.go": 0x00101000, // SYNCHRONIZE | PROCESS_QUERY_LIMITED_INFORMATION
}

// kugouMemBannedWriteAPIs 是整个 player/kugou 子树一律禁止出现的写内存/注入 API。
var kugouMemBannedWriteAPIs = []string{
	"WriteProcessMemory",
	"VirtualAllocEx",
	"VirtualProtectEx",
	"CreateRemoteThread",
}

// kugouMemOnlyReadAPIs 是仅允许在 player/kugou/assoc/mem.go 中出现的进程只读 API。
var kugouMemOnlyReadAPIs = []string{
	"ReadProcessMemory",
	"EnumProcessModulesEx",
	"GetModuleBaseName",
}

// kugouMemWindowsAllowed 是 player/kugou/assoc/mem.go 对 golang.org/x/sys/windows 允许引用的全部 10 个标识符。
var kugouMemWindowsAllowed = []string{
	"Handle",
	"OpenProcess",
	"ReadProcessMemory",
	"EnumProcessModulesEx",
	"GetModuleBaseName",
	"CloseHandle",
	"PROCESS_QUERY_INFORMATION",
	"PROCESS_VM_READ",
	"LIST_MODULES_64BIT",
	"ERROR_PARTIAL_COPY",
}

// kugouMemBannedDynamic 是 player/kugou/assoc/mem.go 禁止使用的动态加载与系统调用名（另按前缀禁止 Syscall* 与 LoadLibrary*）。
var kugouMemBannedDynamic = []string{
	"NewProc",
	"NewLazyDLL",
	"NewLazySystemDLL",
	"LoadDLL",
	"MustLoadDLL",
	"FindProc",
	"MustFindProc",
	"LoadLibrary",
	"LoadLibraryEx",
	"GetProcAddress",
	"Call",
}

// winAccessConsts 用于静态按位求值 OpenProcess 第 1 实参（来自 golang.org/x/sys/windows）。
var winAccessConsts = map[string]uint32{
	"PROCESS_TERMINATE":                 0x0001,
	"PROCESS_CREATE_THREAD":             0x0002,
	"PROCESS_VM_OPERATION":              0x0008,
	"PROCESS_VM_READ":                   0x0010,
	"PROCESS_VM_WRITE":                  0x0020,
	"PROCESS_DUP_HANDLE":                0x0040,
	"PROCESS_CREATE_PROCESS":            0x0080,
	"PROCESS_SET_QUOTA":                 0x0100,
	"PROCESS_SET_INFORMATION":           0x0200,
	"PROCESS_QUERY_INFORMATION":         0x0400,
	"PROCESS_SUSPEND_RESUME":            0x0800,
	"PROCESS_QUERY_LIMITED_INFORMATION": 0x1000,
	"PROCESS_ALL_ACCESS":                0xFFFF,
	"DELETE":                            0x00010000,
	"READ_CONTROL":                      0x00020000,
	"WRITE_DAC":                         0x00040000,
	"WRITE_OWNER":                       0x00080000,
	"SYNCHRONIZE":                       0x00100000,
	"STANDARD_RIGHTS_REQUIRED":          0x000F0000,
	"STANDARD_RIGHTS_ALL":               0x001F0000,
	"MAXIMUM_ALLOWED":                   0x02000000,
	"GENERIC_ALL":                       0x10000000,
	"GENERIC_EXECUTE":                   0x20000000,
	"GENERIC_WRITE":                     0x40000000,
	"GENERIC_READ":                      0x80000000,
}

// TestKuGouMemReadOnly 扫描真实仓库的 player/kugou 子树，确认进程内存只读门禁全部通过。
func TestKuGouMemReadOnly(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	v, err := scanKuGouMemReadOnly(root)
	if err != nil {
		t.Fatalf("扫描 player/kugou 失败: %v", err)
	}
	if len(v) > 0 {
		t.Errorf("player/kugou 进程内存只读门禁发现 %d 处违规：\n  %s", len(v), strings.Join(v, "\n  "))
	}
}

// TestKuGouMemReadOnlyCatchesMutations 以合成目录验证至少 11 种违规形状全部报出，且合规基线无误报。
func TestKuGouMemReadOnlyCatchesMutations(t *testing.T) {
	baseFiles := map[string]string{
		"player/kugou/assoc/mem.go": `package assoc

import "golang.org/x/sys/windows"

func openTest(pid uint32) (windows.Handle, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return 0, err
	}
	var mod windows.Handle
	var needed uint32
	_ = windows.EnumProcessModulesEx(h, &mod, 8, &needed, windows.LIST_MODULES_64BIT)
	var name [4]uint16
	_ = windows.GetModuleBaseName(h, mod, &name[0], 4)
	var b [4]byte
	var n uintptr
	_ = windows.ReadProcessMemory(h, 0x10000, &b[0], 4, &n)
	_ = windows.ERROR_PARTIAL_COPY
	_ = windows.CloseHandle(h)
	return h, nil
}
`,
		"player/kugou/assoc/reader.go": `package assoc

func helper() {}
`,
		"player/kugou/watchdog/confirm.go": `package watchdog

import "golang.org/x/sys/windows"

func imagePath(pid uint32) (windows.Handle, error) {
	return windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
}
`,
		"player/kugou/watchdog/watchdog.go": `package watchdog

import "golang.org/x/sys/windows"

func openParent(pid uint32) (windows.Handle, error) {
	return windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
}
`,
		// 测试文件与 testdata 不受门禁约束，基线里放入违规形状以确认它们被跳过。
		"player/kugou/assoc/mem_test.go": `package assoc
import "golang.org/x/sys/windows"
func testOnly(h windows.Handle, b []byte) { var n uintptr; _ = windows.WriteProcessMemory(h, 0, &b[0], 1, &n) }
`,
		"player/kugou/testdata/bad.go": `package testdata
import "golang.org/x/sys/windows"
func testOnly(pid uint32) { _, _ = windows.OpenProcess(windows.PROCESS_ALL_ACCESS, true, pid) }
`,
	}

	writeTree := func(t *testing.T, files map[string]string) string {
		t.Helper()
		root := t.TempDir()
		for rel, content := range files {
			full := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}

	t.Run("合规基线无误报", func(t *testing.T) {
		root := writeTree(t, baseFiles)
		v, err := scanKuGouMemReadOnly(root)
		if err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		if len(v) > 0 {
			t.Fatalf("合规基线被误报：\n  %s", strings.Join(v, "\n  "))
		}
	})

	cases := []struct {
		name    string
		mutate  func(files map[string]string)
		mention string
	}{
		{
			name: "1_mem.go改为PROCESS_ALL_ACCESS",
			mutate: func(files map[string]string) {
				files[kugouMemFileRel] = strings.Replace(
					files[kugouMemFileRel],
					"windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ",
					"windows.PROCESS_ALL_ACCESS", 1,
				)
			},
			mention: "PROCESS_ALL_ACCESS",
		},
		{
			name: "2_mem.go多加PROCESS_VM_WRITE",
			mutate: func(files map[string]string) {
				files[kugouMemFileRel] = strings.Replace(
					files[kugouMemFileRel],
					"windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ",
					"windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ|windows.PROCESS_VM_WRITE", 1,
				)
			},
			mention: "PROCESS_VM_WRITE",
		},
		{
			name: "3_watchdog/confirm.go多加PROCESS_VM_READ",
			mutate: func(files map[string]string) {
				files["player/kugou/watchdog/confirm.go"] = strings.Replace(
					files["player/kugou/watchdog/confirm.go"],
					"windows.PROCESS_QUERY_LIMITED_INFORMATION",
					"windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_VM_READ", 1,
				)
			},
			mention: "PROCESS_VM_READ",
		},
		{
			name: "4_inheritHandle改true",
			mutate: func(files map[string]string) {
				files[kugouMemFileRel] = strings.Replace(
					files[kugouMemFileRel],
					", false, pid)",
					", true, pid)", 1,
				)
			},
			mention: "inheritHandle",
		},
		{
			name: "5_调WriteProcessMemory",
			mutate: func(files map[string]string) {
				files["player/kugou/assoc/reader.go"] = `package assoc
import "golang.org/x/sys/windows"
func bad(h windows.Handle, b []byte) { var n uintptr; _ = windows.WriteProcessMemory(h, 0, &b[0], 1, &n) }
`
			},
			mention: "WriteProcessMemory",
		},
		{
			name: "6_调NewLazySystemDLL",
			mutate: func(files map[string]string) {
				files[kugouMemFileRel] += "\nvar dll = windows.NewLazySystemDLL(\"kernel32.dll\")\n"
			},
			mention: "NewLazySystemDLL",
		},
		{
			name: "7_导入syscall",
			mutate: func(files map[string]string) {
				files[kugouMemFileRel] = strings.Replace(
					files[kugouMemFileRel],
					`import "golang.org/x/sys/windows"`,
					"import (\n\t\"syscall\"\n\t\"golang.org/x/sys/windows\"\n)\nvar _ = syscall.Handle(0)", 1,
				)
			},
			mention: "syscall",
		},
		{
			name: "8_导入debug/pe",
			mutate: func(files map[string]string) {
				files[kugouMemFileRel] = strings.Replace(
					files[kugouMemFileRel],
					`import "golang.org/x/sys/windows"`,
					"import (\n\t\"debug/pe\"\n\t\"golang.org/x/sys/windows\"\n)\nvar _ *pe.File", 1,
				)
			},
			mention: "debug/pe",
		},
		{
			name: "9_点导入windows",
			mutate: func(files map[string]string) {
				files[kugouMemFileRel] = strings.Replace(
					files[kugouMemFileRel],
					`import "golang.org/x/sys/windows"`,
					"import (\n\t. \"golang.org/x/sys/windows\"\n\t\"golang.org/x/sys/windows\"\n)", 1,
				)
			},
			mention: "点导入",
		},
		{
			name: "10_删掉mem.go的OpenProcess",
			mutate: func(files map[string]string) {
				files[kugouMemFileRel] = `package assoc

import "golang.org/x/sys/windows"

func noop(h windows.Handle) { _ = windows.CloseHandle(h) }
`
			},
			mention: "OpenProcess",
		},
		{
			name: "11_在assoc/reader.go调OpenProcess",
			mutate: func(files map[string]string) {
				files["player/kugou/assoc/reader.go"] = `package assoc
import "golang.org/x/sys/windows"
func bad(pid uint32) { _, _ = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid) }
`
			},
			mention: "OpenProcess",
		},
		{
			name: "12_OpenProcess作为函数值引用",
			mutate: func(files map[string]string) {
				files[kugouMemFileRel] += "\nvar openFn = windows.OpenProcess\n"
			},
			mention: "OpenProcess",
		},
		{
			name: "13_mem.go外调ReadProcessMemory",
			mutate: func(files map[string]string) {
				files["player/kugou/assoc/reader.go"] = `package assoc
import "golang.org/x/sys/windows"
func bad(h windows.Handle, b []byte) { var n uintptr; _ = windows.ReadProcessMemory(h, 0, &b[0], 1, &n) }
`
			},
			mention: "ReadProcessMemory",
		},
		{
			name: "14_mem.go使用白名单外windows标识符",
			mutate: func(files map[string]string) {
				files[kugouMemFileRel] += "\nfunc bad(u []uint16) string { return windows.UTF16ToString(u) }\n"
			},
			mention: "UTF16ToString",
		},
		{
			name: "15_mem.go缺失时门禁自报失效",
			mutate: func(files map[string]string) {
				delete(files, kugouMemFileRel)
			},
			mention: kugouMemFileRel,
		},
		{
			name: "16_字符串字面量WriteProcessMemory",
			mutate: func(files map[string]string) {
				files["player/kugou/assoc/reader.go"] = "package assoc\nvar s = \"WriteProcessMemory\"\n"
			},
			mention: "WriteProcessMemory",
		},
		{
			name: "17_watchdog/watchdog.go多加PROCESS_VM_READ",
			mutate: func(files map[string]string) {
				files["player/kugou/watchdog/watchdog.go"] = strings.Replace(
					files["player/kugou/watchdog/watchdog.go"],
					"windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION",
					"windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_VM_READ", 1,
				)
			},
			mention: "PROCESS_VM_READ",
		},
		{
			name: "18_mem.go空白导入windows",
			mutate: func(files map[string]string) {
				files[kugouMemFileRel] = strings.Replace(
					files[kugouMemFileRel],
					`import "golang.org/x/sys/windows"`,
					"import (\n\t_ \"golang.org/x/sys/windows\"\n\t\"golang.org/x/sys/windows\"\n)", 1,
				)
			},
			mention: "空白导入",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := make(map[string]string, len(baseFiles))
			for k, v := range baseFiles {
				files[k] = v
			}
			tc.mutate(files)
			root := writeTree(t, files)
			v, err := scanKuGouMemReadOnly(root)
			if err != nil {
				t.Fatalf("扫描失败: %v", err)
			}
			if !slices.ContainsFunc(v, func(msg string) bool { return strings.Contains(msg, tc.mention) }) {
				t.Fatalf("门禁漏报：期望包含 %q，实际违规=%q", tc.mention, v)
			}
		})
	}
}

// scanKuGouMemReadOnly 递归扫描 root/player/kugou 下非测试源码（跳过 testdata），返回所有违规描述。
func scanKuGouMemReadOnly(root string) ([]string, error) {
	dir := filepath.Join(root, "player", "kugou")
	fset := token.NewFileSet()
	var violations []string
	seenMemFile := false
	memOpenProcessCalls := 0

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
		rel, err := filepath.Rel(root, p)
		if err != nil {
			rel = p
		}
		rel = filepath.ToSlash(rel)
		if rel == kugouMemFileRel {
			seenMemFile = true
		}
		fv, memCalls := checkKuGouMemFile(fset, root, rel, f)
		violations = append(violations, fv...)
		memOpenProcessCalls += memCalls
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !seenMemFile {
		violations = append(violations, "未找到 "+kugouMemFileRel+" —— 门禁失效")
	} else if memOpenProcessCalls == 0 {
		violations = append(violations, kugouMemFileRel+" 未找到 OpenProcess 调用 —— 门禁失效")
	}
	return violations, nil
}

func checkKuGouMemFile(fset *token.FileSet, root, rel string, f *ast.File) (violations []string, memOpenCalls int) {
	report := func(n ast.Node, format string, a ...any) {
		violations = append(violations, posOf(fset, root, n.Pos(), fmt.Sprintf(format, a...)))
	}
	isMemFile := rel == kugouMemFileRel
	wantAccess, allowOpen := kugouOpenProcessAccess[rel]

	var winPkgs []string
	for _, imp := range f.Imports {
		impPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		local := path.Base(impPath)
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if isMemFile {
			switch impPath {
			case "syscall", "debug/pe":
				report(imp, "禁止导入 %s", impPath)
			case "golang.org/x/sys/windows":
				if local == "." {
					report(imp, "禁止点导入 golang.org/x/sys/windows")
				} else if local == "_" {
					report(imp, "禁止空白导入 golang.org/x/sys/windows")
				} else {
					winPkgs = append(winPkgs, local)
				}
			}
		} else if impPath == "golang.org/x/sys/windows" && local != "." && local != "_" {
			winPkgs = append(winPkgs, local)
		}
	}

	isWinSel := func(sel *ast.SelectorExpr) (string, bool) {
		if x, ok := sel.X.(*ast.Ident); ok && slices.Contains(winPkgs, x.Name) {
			return x.Name, true
		}
		return "", false
	}

	checkedOpenIdents := map[*ast.Ident]bool{}

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			switch fn := node.Fun.(type) {
			case *ast.SelectorExpr:
				if fn.Sel.Name != "OpenProcess" {
					break
				}
				checkedOpenIdents[fn.Sel] = true
				if !allowOpen {
					report(node, "不允许在该文件调用 OpenProcess")
					break
				}
				if _, ok := isWinSel(fn); !ok {
					report(node, "OpenProcess 必须经 golang.org/x/sys/windows 调用")
					break
				}
				if isMemFile {
					memOpenCalls++
				}
				checkOpenProcessArgs(node, winPkgs, wantAccess, report)
			case *ast.Ident:
				if fn.Name != "OpenProcess" {
					break
				}
				checkedOpenIdents[fn] = true
				report(node, "OpenProcess 必须经 golang.org/x/sys/windows 显式限定调用")
			}

		case *ast.SelectorExpr:
			if isMemFile {
				if pkg, ok := isWinSel(node); ok && !slices.Contains(kugouMemWindowsAllowed, node.Sel.Name) {
					report(node, "%s.%s 不在 mem.go 允许的 10 个 windows 标识符白名单内", pkg, node.Sel.Name)
				}
				if isBannedDynamicSelector(node.Sel.Name) {
					report(node, "mem.go 禁止动态加载或直接系统调用 .%s", node.Sel.Name)
				}
			}

		case *ast.Ident:
			for _, api := range kugouMemBannedWriteAPIs {
				if strings.Contains(node.Name, api) {
					report(node, "禁止写内存或注入 API 标识符 %s", node.Name)
				}
			}
			if !isMemFile {
				for _, api := range kugouMemOnlyReadAPIs {
					if strings.Contains(node.Name, api) {
						report(node, "禁止在 %s 之外引用 %s", kugouMemFileRel, node.Name)
					}
				}
			} else if isBannedDynamicIdent(node.Name) {
				report(node, "mem.go 禁止动态加载或系统调用标识符 %s", node.Name)
			}
			if node.Name == "OpenProcess" && !checkedOpenIdents[node] {
				if !allowOpen {
					report(node, "不允许在该文件引用 OpenProcess")
				} else {
					report(node, "OpenProcess 不得作为非调用的函数值或其他标识符引用")
				}
			}

		case *ast.BasicLit:
			if node.Kind != token.STRING {
				break
			}
			s, err := strconv.Unquote(node.Value)
			if err != nil {
				break
			}
			for _, api := range kugouMemBannedWriteAPIs {
				if strings.Contains(s, api) {
					report(node, "禁止写内存或注入 API 字符串字面量 %s", node.Value)
				}
			}
			if !isMemFile {
				for _, api := range kugouMemOnlyReadAPIs {
					if strings.Contains(s, api) {
						report(node, "禁止在 %s 之外出现只读 API 字符串字面量 %s", kugouMemFileRel, node.Value)
					}
				}
			} else if isBannedDynamicIdent(s) {
				report(node, "mem.go 禁止动态加载字符串字面量 %s", node.Value)
			}
			if s == "OpenProcess" {
				report(node, "禁止通过字符串字面量引用 OpenProcess")
			}
		}
		return true
	})

	return violations, memOpenCalls
}

func checkOpenProcessArgs(call *ast.CallExpr, winPkgs []string, wantAccess uint32, report func(ast.Node, string, ...any)) {
	if len(call.Args) != 3 {
		report(call, "OpenProcess 实参个数为 %d，要求 3", len(call.Args))
		return
	}
	got, ok := evalAccessMask(call.Args[0], winPkgs)
	if !ok {
		report(call, "OpenProcess 第 1 实参 %s 无法静态按位求值（要求 0x%X）", types.ExprString(call.Args[0]), wantAccess)
	} else if got != wantAccess {
		report(call, "OpenProcess 第 1 实参 %s 求值为 0x%X，要求 0x%X", types.ExprString(call.Args[0]), got, wantAccess)
	}
	inherit, ok := call.Args[1].(*ast.Ident)
	if !ok || inherit.Name != "false" {
		report(call, "OpenProcess 第 2 实参 inheritHandle 为 %s，必须为布尔字面量 false", types.ExprString(call.Args[1]))
	}
}

// evalAccessMask 静态求值 OpenProcess 第 1 实参的按位或表达式。
func evalAccessMask(e ast.Expr, winPkgs []string) (uint32, bool) {
	switch expr := e.(type) {
	case *ast.ParenExpr:
		return evalAccessMask(expr.X, winPkgs)
	case *ast.BinaryExpr:
		if expr.Op != token.OR {
			return 0, false
		}
		l, lok := evalAccessMask(expr.X, winPkgs)
		r, rok := evalAccessMask(expr.Y, winPkgs)
		if !lok || !rok {
			return 0, false
		}
		return l | r, true
	case *ast.SelectorExpr:
		x, ok := expr.X.(*ast.Ident)
		if !ok || !slices.Contains(winPkgs, x.Name) {
			return 0, false
		}
		v, ok := winAccessConsts[expr.Sel.Name]
		return v, ok
	case *ast.BasicLit:
		if expr.Kind != token.INT {
			return 0, false
		}
		v, err := strconv.ParseUint(expr.Value, 0, 32)
		if err != nil {
			return 0, false
		}
		return uint32(v), true
	default:
		return 0, false
	}
}

func isBannedDynamicSelector(name string) bool {
	if strings.HasPrefix(name, "Syscall") || strings.HasPrefix(name, "LoadLibrary") {
		return true
	}
	return slices.Contains(kugouMemBannedDynamic, name)
}

func isBannedDynamicIdent(name string) bool {
	if name == "Call" {
		return false
	}
	return isBannedDynamicSelector(name)
}
