package qqmusic

// 本文件只守一件事：player/qqmusic 包打开 QQ 音乐进程句柄的只读权限与关闭接线（#233）。
// 扫的是本包顶层全部非测试源码，不测业务逻辑；行为验证在 prochandle_test.go。
//
// # 它守什么
//
//   - 全包非测试源码不得出现 PROCESS_ALL_ACCESS（含包限定与裸标识符），也不得以 NewProc("OpenProcess") 动态取入口；
//   - mem.go 的 qqProcessAccess 常量恰为 windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_READ；
//   - 全包非测试源码至少有一处 OpenProcess 调用（零命中自报失效），且全部经 x/sys/windows 调用、参数恰为 (qqProcessAccess, false, …)；
//   - (*QQMusicMem).Close 存在：先守卫 m == nil || m.hProcess == 0 早退，再清零 m.hProcess = 0，再调 windows.CloseHandle；
//   - (*QQMusicMem).CheckValid 存在：经 m.Close() 关闭句柄，不得直接给 m.hProcess 赋值；
//   - (*QQMusicPlayer).runSession 顶层存在 defer mem.Close()，且位于 ctx/defer cancel() 与轮询循环之前；
//   - (*QQMusicPlayer).Start 在 ConnectQQMusic() 的 if err != nil 之后到 p.runSession(mem, …) 之间没有 return/continue/break/goto。
//
// # 它不查什么
//
// 不做类型解析：经别的包或未在本包直接出现的系统调用、函数内另写 syscall.SyscallN 按数字调 OpenProcess 查不出；
// 也不验证其他工具包（如 tools/qqromaprobe）是否调用 Close。

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

func readQQMusicNonTestSources(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(files))
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = string(raw)
	}
	return out
}

func windowsImportNames(f *ast.File, bad func(ast.Node, string, ...any)) []string {
	var wins []string
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if path != "golang.org/x/sys/windows" {
			continue
		}
		local := "windows"
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == "." || local == "_" {
			bad(imp, "以 %s 导入 windows，其调用查不出", local)
			continue
		}
		wins = append(wins, local)
	}
	return wins
}

func isWinSelector(e ast.Expr, wins []string, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && slices.Contains(wins, x.Name)
}

func recvTypeName(fd *ast.FuncDecl) (recvName, typeName string) {
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return "", ""
	}
	field := fd.Recv.List[0]
	if len(field.Names) > 0 {
		recvName = field.Names[0].Name
	}
	typeName = strings.TrimPrefix(types.ExprString(field.Type), "*")
	return recvName, typeName
}

// procHandleProblems 扫描 srcs（player/qqmusic 顶层非测试源码集合），返回发现的全部问题。
func procHandleProblems(srcs map[string]string) []string {
	names := make([]string, 0, len(srcs))
	for name := range srcs {
		names = append(names, name)
	}
	slices.Sort(names)

	fset := token.NewFileSet()
	parsed := make(map[string]*ast.File, len(srcs))
	winNames := make(map[string][]string, len(srcs))
	var probs []string
	bad := func(n ast.Node, format string, a ...any) {
		if n != nil {
			probs = append(probs, fset.Position(n.Pos()).String()+": "+fmt.Sprintf(format, a...))
			return
		}
		probs = append(probs, fmt.Sprintf(format, a...))
	}

	for _, name := range names {
		f, err := parser.ParseFile(fset, name, srcs[name], 0)
		if err != nil {
			return []string{name + ": 解析失败: " + err.Error()}
		}
		parsed[name] = f
		winNames[name] = windowsImportNames(f, bad)
	}

	// ---- 1. 全包扫描：禁止 PROCESS_ALL_ACCESS、禁止 NewProc("OpenProcess")、约束所有 OpenProcess 调用 ----
	openCalls := 0
	for _, name := range names {
		f := parsed[name]
		wins := winNames[name]
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name == "PROCESS_ALL_ACCESS" {
					bad(x, "不得出现 PROCESS_ALL_ACCESS")
				}
			case *ast.CallExpr:
				switch fun := x.Fun.(type) {
				case *ast.SelectorExpr:
					if fun.Sel.Name == "NewProc" && len(x.Args) == 1 {
						if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if v, err := strconv.Unquote(lit.Value); err == nil && v == "OpenProcess" {
								bad(x, "不得通过 NewProc(\"OpenProcess\") 动态取入口，须经 windows.OpenProcess 调用")
							}
						}
					}
					if fun.Sel.Name == "OpenProcess" {
						openCalls++
						if !isWinSelector(fun, wins, "OpenProcess") {
							bad(x, "OpenProcess 须经 golang.org/x/sys/windows 调用，实得 %s", types.ExprString(fun))
							break
						}
						if len(x.Args) != 3 {
							bad(x, "OpenProcess 实参个数为 %d，want 3", len(x.Args))
							break
						}
						if got := types.ExprString(x.Args[0]); got != "qqProcessAccess" {
							bad(x, "OpenProcess 访问权限实参为 %s，want qqProcessAccess", got)
						}
						if got := types.ExprString(x.Args[1]); got != "false" {
							bad(x, "OpenProcess 的 inherit 实参为 %s，want false", got)
						}
					}
				case *ast.Ident:
					if fun.Name == "OpenProcess" {
						openCalls++
						bad(x, "不得调用裸函数 OpenProcess，须经 windows.OpenProcess(qqProcessAccess, false, …)")
					}
				}
			}
			return true
		})
	}
	if openCalls == 0 {
		bad(nil, "全包非测试文件没找到 OpenProcess 调用 —— 门禁失效")
	}

	// ---- 2. 检查 mem.go：qqProcessAccess 常量、Close()、CheckValid() ----
	memFile := parsed["mem.go"]
	if memFile == nil {
		bad(nil, "没找到 mem.go —— 门禁失效")
	} else {
		memWins := winNames["mem.go"]
		foundAccess := false
		var closeFn, checkFn *ast.FuncDecl
		for _, d := range memFile.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, sp := range d.Specs {
					vs, ok := sp.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, id := range vs.Names {
						if id.Name != "qqProcessAccess" {
							continue
						}
						foundAccess = true
						if i >= len(vs.Values) {
							bad(id, "qqProcessAccess 缺少显式常量表达式")
							continue
						}
						got := types.ExprString(vs.Values[i])
						matched := false
						for _, w := range memWins {
							if got == w+".PROCESS_QUERY_INFORMATION | "+w+".PROCESS_VM_READ" {
								matched = true
								break
							}
						}
						if !matched {
							bad(vs.Values[i], "qqProcessAccess 定义为 %s，want windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_READ", got)
						}
					}
				}
			case *ast.FuncDecl:
				_, typ := recvTypeName(d)
				if typ != "QQMusicMem" {
					continue
				}
				switch d.Name.Name {
				case "Close":
					closeFn = d
				case "CheckValid":
					checkFn = d
				}
			}
		}
		if !foundAccess {
			bad(nil, "mem.go 缺少 qqProcessAccess 常量定义 —— 门禁失效")
		}

		// (*QQMusicMem).Close
		if closeFn == nil || closeFn.Body == nil {
			bad(nil, "mem.go 缺少 (*QQMusicMem).Close 方法")
		} else {
			recv, _ := recvTypeName(closeFn)
			guardPos, clearPos, closePos := token.NoPos, token.NoPos, token.NoPos
			wantGuard := recv + " == nil || " + recv + ".hProcess == 0"
			for _, s := range closeFn.Body.List {
				if ifs, ok := s.(*ast.IfStmt); ok && guardPos == token.NoPos {
					cond := types.ExprString(ifs.Cond)
					if cond == wantGuard && len(ifs.Body.List) == 1 {
						if ret, ok := ifs.Body.List[0].(*ast.ReturnStmt); ok && len(ret.Results) == 0 {
							guardPos = ifs.Pos()
						}
					}
				}
			}
			ast.Inspect(closeFn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.AssignStmt:
					if clearPos == token.NoPos && x.Tok == token.ASSIGN && len(x.Lhs) == 1 && len(x.Rhs) == 1 &&
						types.ExprString(x.Lhs[0]) == recv+".hProcess" && types.ExprString(x.Rhs[0]) == "0" {
						clearPos = x.Pos()
					}
				case *ast.CallExpr:
					if closePos == token.NoPos && isWinSelector(x.Fun, memWins, "CloseHandle") {
						closePos = x.Pos()
					}
				}
				return true
			})
			if guardPos == token.NoPos {
				bad(closeFn, "(*QQMusicMem).Close 缺少 %s 早退守卫（防止 CloseHandle(0) 与 nil 解引用）", wantGuard)
			}
			if clearPos == token.NoPos {
				bad(closeFn, "(*QQMusicMem).Close 缺少 %s.hProcess = 0 清零", recv)
			}
			if closePos == token.NoPos {
				bad(closeFn, "(*QQMusicMem).Close 缺少 windows.CloseHandle 调用")
			}
			if guardPos != token.NoPos && clearPos != token.NoPos && closePos != token.NoPos {
				if !(guardPos < clearPos && clearPos < closePos) {
					bad(closeFn, "(*QQMusicMem).Close 的次序应为早退守卫 → %s.hProcess = 0 → windows.CloseHandle", recv)
				}
			}
		}

		// (*QQMusicMem).CheckValid
		if checkFn == nil || checkFn.Body == nil {
			bad(nil, "mem.go 缺少 (*QQMusicMem).CheckValid 方法")
		} else {
			recv, _ := recvTypeName(checkFn)
			calledClose := false
			ast.Inspect(checkFn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if types.ExprString(x.Fun) == recv+".Close" && len(x.Args) == 0 {
						calledClose = true
					}
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if types.ExprString(lhs) == recv+".hProcess" {
							bad(x, "(*QQMusicMem).CheckValid 不得直接给 %s.hProcess 赋值，须经 %s.Close() 关闭并清零", recv, recv)
						}
					}
				}
				return true
			})
			if !calledClose {
				bad(checkFn, "(*QQMusicMem).CheckValid 必须调用 %s.Close() 关闭已退出进程的句柄", recv)
			}
		}
	}

	// ---- 3. 检查 qqmusic.go：runSession 的 defer mem.Close() 与 Start 的连接-会话路径 ----
	qqFile := parsed["qqmusic.go"]
	if qqFile == nil {
		bad(nil, "没找到 qqmusic.go —— 门禁失效")
	} else {
		var runFn, startFn *ast.FuncDecl
		for _, d := range qqFile.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			_, typ := recvTypeName(fd)
			if typ != "QQMusicPlayer" {
				continue
			}
			switch fd.Name.Name {
			case "runSession":
				runFn = fd
			case "Start":
				startFn = fd
			}
		}

		// (*QQMusicPlayer).runSession
		if runFn == nil || runFn.Body == nil {
			bad(nil, "qqmusic.go 缺少 (*QQMusicPlayer).runSession —— 门禁失效")
		} else {
			closeDeferIdx, ctxIdx, cancelDeferIdx, loopIdx := -1, -1, -1, -1
			for i, s := range runFn.Body.List {
				switch x := s.(type) {
				case *ast.DeferStmt:
					switch types.ExprString(x.Call) {
					case "mem.Close()":
						if closeDeferIdx < 0 {
							closeDeferIdx = i
						}
					case "cancel()":
						if cancelDeferIdx < 0 {
							cancelDeferIdx = i
						}
					}
				case *ast.AssignStmt:
					if len(x.Lhs) == 2 && len(x.Rhs) == 1 &&
						types.ExprString(x.Lhs[0]) == "ctx" && types.ExprString(x.Lhs[1]) == "cancel" &&
						strings.HasPrefix(types.ExprString(x.Rhs[0]), "context.WithCancel(") {
						ctxIdx = i
					}
				case *ast.ForStmt:
					if x.Cond == nil {
						loopIdx = i
					}
				}
			}
			if loopIdx >= 0 {
				ast.Inspect(runFn.Body.List[loopIdx], func(n ast.Node) bool {
					if d, ok := n.(*ast.DeferStmt); ok && types.ExprString(d.Call) == "mem.Close()" {
						bad(d, "runSession 的 defer mem.Close() 不得放在轮询循环内")
					}
					return true
				})
			}
			switch {
			case ctxIdx < 0 || cancelDeferIdx < 0 || loopIdx < 0:
				bad(runFn, "没找到 runSession 的 ctx/defer cancel()/轮询循环 —— 门禁失效")
			case closeDeferIdx < 0:
				bad(runFn, "runSession 顶层缺少 defer mem.Close()")
			case !(closeDeferIdx < ctxIdx && closeDeferIdx < cancelDeferIdx && closeDeferIdx < loopIdx):
				bad(runFn.Body.List[closeDeferIdx], "runSession 的 defer mem.Close() 必须位于 ctx、defer cancel() 与轮询循环之前")
			}
		}

		// (*QQMusicPlayer).Start
		if startFn == nil || startFn.Body == nil {
			bad(nil, "qqmusic.go 缺少 (*QQMusicPlayer).Start —— 门禁失效")
		} else {
			startRecv, _ := recvTypeName(startFn)
			foundBlock := false
			ast.Inspect(startFn.Body, func(n ast.Node) bool {
				b, ok := n.(*ast.BlockStmt)
				if !ok {
					return true
				}
				connectIdx, runIdx := -1, -1
				for i, s := range b.List {
					switch x := s.(type) {
					case *ast.AssignStmt:
						if len(x.Rhs) == 1 && types.ExprString(x.Rhs[0]) == "ConnectQQMusic()" {
							connectIdx = i
						}
					case *ast.ExprStmt:
						if c, ok := x.X.(*ast.CallExpr); ok && types.ExprString(c.Fun) == startRecv+".runSession" {
							runIdx = i
						}
					}
				}
				if connectIdx < 0 {
					return true
				}
				foundBlock = true
				if runIdx < 0 || runIdx <= connectIdx {
					bad(b.List[connectIdx], "Start 里 ConnectQQMusic() 之后没有调用 %s.runSession", startRecv)
					return true
				}
				errIdx := connectIdx + 1
				if ifs, ok := b.List[errIdx].(*ast.IfStmt); !ok || types.ExprString(ifs.Cond) != "err != nil" {
					bad(b.List[connectIdx], "Start 里 ConnectQQMusic() 之后应紧跟 if err != nil 错误处理")
					return true
				}
				for _, s := range b.List[errIdx+1 : runIdx] {
					ast.Inspect(s, func(x ast.Node) bool {
						switch y := x.(type) {
						case *ast.FuncLit:
							return false
						case *ast.ReturnStmt:
							bad(y, "Start 里 ConnectQQMusic() 成功之后到 runSession 之间存在 return，可能泄漏句柄")
						case *ast.BranchStmt:
							bad(y, "Start 里 ConnectQQMusic() 成功之后到 runSession 之间存在 %s，可能泄漏句柄", y.Tok.String())
						}
						return true
					})
				}
				return true
			})
			if !foundBlock {
				bad(startFn, "Start 里没找到 ConnectQQMusic() 调用 —— 门禁失效")
			}
		}
	}

	return probs
}

// TestQQMusicProcHandleReadOnlyAndClosed 钉住 player/qqmusic 包进程句柄的只读权限与关闭接线。
// 门禁自身对 16 种反例变异的自证见下一条 TestProcHandleLintCatchesMutations。
//
// 真实源码变异自证（均实测）：qqProcessAccess 改回 windows.PROCESS_ALL_ACCESS、CheckValid 改回仅 m.hProcess = 0
// 不调 m.Close()、Close 去掉 windows.CloseHandle、Close 去掉 m.hProcess = 0、runSession 去掉 defer mem.Close()，
// 本例均红；前四项同时使 prochandle_test.go 中对应行为用例变红。
func TestQQMusicProcHandleReadOnlyAndClosed(t *testing.T) {
	probs := procHandleProblems(readQQMusicNonTestSources(t))
	if len(probs) > 0 {
		t.Fatalf("player/qqmusic 进程句柄只读与关闭门禁不通过：\n  %s", strings.Join(probs, "\n  "))
	}
}

// TestProcHandleLintCatchesMutations 在内存中对真实源码逐条注入 16 种反例变异，
// 验证每条改写确实生效且门禁报出对应问题。
func TestProcHandleLintCatchesMutations(t *testing.T) {
	orig := readQQMusicNonTestSources(t)
	if probs := procHandleProblems(orig); len(probs) > 0 {
		t.Fatalf("基线源码未通过门禁，无法做变异自证：\n  %s", strings.Join(probs, "\n  "))
	}

	const (
		accessDef   = "const qqProcessAccess = windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_READ"
		openCall    = "windows.OpenProcess(qqProcessAccess, false, pid)"
		closeGuard  = "\tif m == nil || m.hProcess == 0 {\n\t\treturn\n\t}\n"
		closeClear  = "\tm.hProcess = 0\n"
		closeHandle = "\twindows.CloseHandle(windows.Handle(h))\n"
		checkClose  = "\t\tm.Close()\n"
		deferClose  = "\tdefer mem.Close()\n"
		deferCancel = "\tdefer cancel()\n"
		pollSleep   = "\t\tif p.sleepOrStop(lyricSt.pollSleep(pollInterval, time.Now())) {\n\t\t\treturn\n\t\t}\n"
		startLog    = "\t\tlog.Info(\"Connected to QQMusic.exe (PID: %d, version: %s)\", mem.pid, mem.Version())\n"
	)

	replace := func(old, new string) func(string) string {
		return func(s string) string { return strings.Replace(s, old, new, 1) }
	}
	moveAfter := func(block, anchor string) func(string) string {
		return func(s string) string {
			if !strings.Contains(s, block) || !strings.Contains(s, anchor) {
				return s
			}
			removed := strings.Replace(s, block, "", 1)
			return strings.Replace(removed, anchor, anchor+block, 1)
		}
	}

	cases := []struct {
		name    string
		file    string
		mutate  func(string) string
		mention string
	}{
		{
			name:    "qqProcessAccess 改为 windows.PROCESS_ALL_ACCESS",
			file:    "mem.go",
			mutate:  replace(accessDef, "const qqProcessAccess = windows.PROCESS_ALL_ACCESS"),
			mention: "PROCESS_ALL_ACCESS",
		},
		{
			name:    "qqProcessAccess 多加 PROCESS_VM_WRITE",
			file:    "mem.go",
			mutate:  replace(accessDef, accessDef+" | windows.PROCESS_VM_WRITE"),
			mention: "PROCESS_VM_WRITE",
		},
		{
			name:    "qqProcessAccess 多加 PROCESS_VM_OPERATION",
			file:    "mem.go",
			mutate:  replace(accessDef, accessDef+" | windows.PROCESS_VM_OPERATION"),
			mention: "PROCESS_VM_OPERATION",
		},
		{
			name:    "OpenProcess 直接传 0x1F0FFF",
			file:    "mem.go",
			mutate:  replace(openCall, "windows.OpenProcess(0x1F0FFF, false, pid)"),
			mention: "0x1F0FFF",
		},
		{
			name:    "OpenProcess 的 inherit 改为 true",
			file:    "mem.go",
			mutate:  replace(openCall, "windows.OpenProcess(qqProcessAccess, true, pid)"),
			mention: "true",
		},
		{
			name:    "重新引入 const PROCESS_ALL_ACCESS = 0x1F0FFF",
			file:    "mem.go",
			mutate:  replace(accessDef, accessDef+"\nconst PROCESS_ALL_ACCESS = 0x1F0FFF"),
			mention: "PROCESS_ALL_ACCESS",
		},
		{
			name:    "删去或改名 OpenProcess（零命中自检触发）",
			file:    "mem.go",
			mutate:  replace(openCall, "openProcHelper(qqProcessAccess, false, pid)"),
			mention: "门禁失效",
		},
		{
			name:    "Close 去掉 m.hProcess == 0 守卫",
			file:    "mem.go",
			mutate:  replace(closeGuard, ""),
			mention: "早退守卫",
		},
		{
			name:    "Close 守卫倒序先解引用 m.hProcess 后判 nil",
			file:    "mem.go",
			mutate:  replace(closeGuard, "\tif m.hProcess == 0 || m == nil {\n\t\treturn\n\t}\n"),
			mention: "早退守卫",
		},
		{
			name:    "Close 去掉 m.hProcess = 0 清零",
			file:    "mem.go",
			mutate:  replace(closeClear, ""),
			mention: "hProcess = 0",
		},
		{
			name:    "Close 去掉 windows.CloseHandle 调用",
			file:    "mem.go",
			mutate:  replace(closeHandle, "\t_ = h\n"),
			mention: "CloseHandle",
		},
		{
			name:    "CheckValid 退回 m.hProcess = 0 而不调 m.Close()",
			file:    "mem.go",
			mutate:  replace(checkClose, "\t\tm.hProcess = 0\n"),
			mention: "CheckValid",
		},
		{
			name:    "runSession 去掉 defer mem.Close()",
			file:    "qqmusic.go",
			mutate:  replace(deferClose, ""),
			mention: "defer mem.Close()",
		},
		{
			name:    "runSession 将 defer mem.Close() 挪到 defer cancel() 之后",
			file:    "qqmusic.go",
			mutate:  moveAfter(deferClose, deferCancel),
			mention: "之前",
		},
		{
			name:    "runSession 将 defer mem.Close() 挪进轮询循环内",
			file:    "qqmusic.go",
			mutate:  moveAfter(deferClose, pollSleep),
			mention: "轮询循环",
		},
		{
			name:    "Start 在 ConnectQQMusic 与 runSession 之间插入提前 continue",
			file:    "qqmusic.go",
			mutate:  replace(startLog, startLog+"\t\tif mem.Version() == \"\" {\n\t\t\tcontinue\n\t\t}\n"),
			mention: "continue",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := make(map[string]string, len(orig))
			for k, v := range orig {
				files[k] = v
			}
			src, ok := files[c.file]
			if !ok {
				t.Fatalf("找不到源文件 %s", c.file)
			}
			mutated := c.mutate(src)
			if mutated == src {
				t.Fatalf("对 %s 的改写没有生效 —— 本条自测失效", c.file)
			}
			files[c.file] = mutated
			probs := procHandleProblems(files)
			if len(probs) == 0 {
				t.Fatalf("门禁漏报（期望包含 %q）", c.mention)
			}
			for _, p := range probs {
				if strings.Contains(p, "解析失败") {
					t.Fatalf("改写后代码无法解析，自测失效: %q", probs)
				}
			}
			if !slices.ContainsFunc(probs, func(p string) bool { return strings.Contains(p, c.mention) }) {
				t.Fatalf("门禁报告 %q 里不包含 %q", probs, c.mention)
			}
		})
	}
}
