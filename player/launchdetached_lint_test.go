package player

// 本文件只测一件事：AST 门禁「播放器由 PlayerCap 拉起必须经 launch.Detached，绝不直接 Start()」（issue #127）。
//
// # 为什么要门禁
//
// OBS 的 Autostarter 插件在 OBS 退出时按父进程 ID 递归结束 PlayerCap 的全部子进程。直接 exec.Command(...).Start()
// 拉起的播放器是 PlayerCap 的子进程，会被一并结束；直播收尾关 OBS，主播的网易云、Spotify 或酷狗跟着消失。写法上
// 两者只差一行，新加一个「带参数重启」的播放器时最容易照旧写成 Start()。
//
// # 它守什么、不守什么
//
//  (a) 扫 player/ 下的非测试 .go（player/launch 自身除外），exec.Command / exec.CommandContext 的结果被 .Start() 的写法即红：
//      链式调用，以及同一个顶层声明里先赋给变量或字段（:=、=、var）、再对它 .Start()。.Run() / .Output() 不管
//      （taskkill、tasklist、spotify_cli 是等它结束的一次性命令，不留在进程树里）。
//  (b) 四个已知拉起点（cloudmusic 的 EnsureDebugMode、spotify 的 startDetached、kugou 的 launchKuGou、kuwo 的 startExact）必须调用
//      launch.Detached（直接调用，或经同一文件里初始化为 launch.Detached 的包级变量调用，即 spotify 供测试替换的
//      detachFn），且四处各有一条逐字相同的 Warn（退回直接启动时打的那条）。
//
// 盲区：只认 os/exec 的这两个构造函数与 .Start() 这个名字；os.StartProcess、syscall / windows 的 CreateProcess、
// ShellExecute、exec.Cmd 字面量、跨顶层声明传递的 *exec.Cmd 都查不出；不做类型解析。player/ 之外不扫（audiocap 的
// helper 就该随 PlayerCap 一起结束）。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// launchPoints 是四个已知的播放器拉起点：相对 player/ 的文件 → 必须调用 launch.Detached 的函数名。
var launchPoints = map[string]string{
	"cloudmusic/watchdog/process.go":   "EnsureDebugMode",
	"spotify/watchdog/watchdog.go":     "startDetached",
	"kugou/watchdog/watchdog.go":       "launchKuGou",
	"kuwo/watchdog/process_windows.go": "startExact",
}

// TestPlayersNeverStartDirectly 是门禁 (a)。
//
// 变异自证（实测）：cloudmusic 的 EnsureDebugMode 改回 exec.Command(exePath, LaunchFlags...).Start()、spotify 的 startFn
// 改回 exec.Command(exePath, args...).Start()、kugou 的 launchKuGou 改回 cmd := exec.Command(exePath) 之后 cmd.Start()，均红。
func TestPlayersNeverStartDirectly(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	found, err := scanDirectStarts(filepath.Join(root, "player"))
	if err != nil {
		t.Fatalf("扫描源码失败: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("以下位置直接 Start() 了 exec.Command 的结果：\n  %s\n\n"+
			"直接启动的进程是 PlayerCap 的子进程，OBS 的 Autostarter 插件等按进程树结束 PlayerCap 的工具会把它一并结束。\n"+
			"修法：播放器一律经 launch.Detached 拉起（AGENTS.md §3.6）；不该留下的一次性命令用 .Run() / .Output() 等它结束。",
			strings.Join(found, "\n  "))
	}
}

// TestKnownLaunchPointsUseDetached 是门禁 (b)。它同时是 (a) 的零命中自检：扫描若失效、一个拉起点都认不出，这里红。
//
// 变异自证（实测）：cloudmusic 的 EnsureDebugMode 不再调用 launch.Detached、kugou 那条 Warn 改动一个字，均红。
func TestKnownLaunchPointsUseDetached(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	if problems := checkLaunchPoints(filepath.Join(root, "player"), launchPoints); len(problems) > 0 {
		t.Errorf("已知的播放器拉起点不合规：\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestDirectStartLintCatchesKnownShapes 钉住门禁 (a) 认得出的形状与不误报的形状（教训同 callback_lint_test.go：
// 抓不住目标形状的门禁比没有门禁更糟，误报的门禁会逼人绕过它）。
//
// 变异自证（实测，均红）：不认赋给变量或字段的、不认 var 声明、不认 CommandContext、不看 import（别名认不出、没导入
// os/exec 的同名 Command 被误报）、.Run() 也算、也扫测试文件、不排除 launch 目录、按前缀排除 launch。不排除 launch
// 目录时扫真实代码的 TestPlayersNeverStartDirectly 仍绿（launch 包里的 Start 与 exec.Command 不在同一个顶层声明），
// 只由本用例拦住。
func TestDirectStartLintCatchesKnownShapes(t *testing.T) {
	const imp = "package p\nimport \"os/exec\"\n"
	cases := []struct {
		name  string
		path  string // 相对扫描根
		src   string
		found bool
	}{
		{"链式 exec.Command(...).Start()", "p/a.go", imp + `func f() error { return exec.Command("x").Start() }`, true},
		{"链式 exec.CommandContext(...).Start()", "p/a.go", imp + `import "context"
func f(ctx context.Context) error { return exec.CommandContext(ctx, "x").Start() }`, true},
		{":= 之后 Start", "p/a.go", imp + `func f() error { cmd := exec.Command("x"); cmd.Dir = "d"; return cmd.Start() }`, true},
		{"先声明再 = 之后 Start", "p/a.go", imp + `func f() error { var cmd *exec.Cmd; cmd = exec.Command("x"); return cmd.Start() }`, true},
		{"var 之后 Start", "p/a.go", imp + `func f() error { var cmd = exec.Command("x"); return cmd.Start() }`, true},
		{"赋给字段之后 Start", "p/a.go", imp + `type s struct{ cmd *exec.Cmd }
func (v *s) f() error { v.cmd = exec.Command("x"); return v.cmd.Start() }`, true},
		{"包级函数变量里的闭包", "p/a.go", imp + `var startFn = func(p string) error { return exec.Command(p).Start() }`, true},
		{"别名 import", "p/a.go", "package p\nimport ex \"os/exec\"\n" + `func f() error { return ex.Command("x").Start() }`, true},
		{"子目录里的包", "p/q/a.go", imp + `func f() error { return exec.Command("x").Start() }`, true},
		{"名字以 launch 开头而不是 launch 包", "launchx/a.go", imp + `func f() error { return exec.Command("x").Start() }`, true},

		{"Run 不算", "p/a.go", imp + `func f() error { return exec.Command("taskkill", "/F").Run() }`, false},
		{"Output 不算", "p/a.go", imp + `func f() ([]byte, error) { return exec.Command("tasklist").Output() }`, false},
		{"CommandContext 之后 Output 不算", "p/a.go", imp + `import "context"
func f(ctx context.Context) ([]byte, error) { cmd := exec.CommandContext(ctx, "cli"); return cmd.Output() }`, false},
		{"播放器自己的 Start 方法不算", "p/a.go", "package p\n" + `type P struct{}
func (p *P) Start(x int) {}
func f(p *P) { p.Start(1) }`, false},
		{"别的顶层函数里同名变量的 Start 不算", "p/a.go", imp + `type T struct{}
func (T) Start() error { return nil }
func f() error { cmd := exec.Command("x"); return cmd.Run() }
func g() error { var cmd T; return cmd.Start() }`, false},
		{"没 import os/exec 的同名 Command 不算", "p/a.go", "package p\n" + `type E struct{}
func (E) Command(string) *E { return nil }
func (*E) Start() error { return nil }
var exec E
func f() error { return exec.Command("x").Start() }`, false},
		{"测试文件不扫", "p/a_test.go", imp + `func f() error { return exec.Command("x").Start() }`, false},
		{"launch 包自身不扫", "launch/a_windows.go", imp + `func f() error { return exec.Command("x").Start() }`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, filepath.FromSlash(c.path))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(c.src), 0o644); err != nil {
				t.Fatal(err)
			}
			found, err := scanDirectStarts(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(found) > 0; got != c.found {
				t.Errorf("判为直接 Start = %v，期望 %v（%v）", got, c.found, found)
			}
		})
	}
}

// TestLaunchPointCheckCatchesKnownShapes 钉住门禁 (b) 的判定。
//
// 变异自证（实测，均红）：不认经包级变量调用（扫真实代码的 TestKnownLaunchPointsUseDetached 同时红在 spotify）、
// 不查 Warn 一致、包名写死为 launch（别名认不出）、不看 import（同名的别的变量也算）。
func TestLaunchPointCheckCatchesKnownShapes(t *testing.T) {
	const warn = `log.Warn("%s was started directly", name)`
	file := func(body string) string {
		return "package p\nimport \"Metabox-Nexus-PlayerCap/player/launch\"\n" + body
	}
	ok := func(fn string) string {
		return file(`func ` + fn + `() { d, _ := launch.Detached("x", nil, ""); if !d { ` + warn + ` } }`)
	}
	points := map[string]string{"a/a.go": "A", "b/b.go": "B"}
	cases := []struct {
		name  string
		files map[string]string
		bad   bool
	}{
		{"两处都合规", map[string]string{"a/a.go": ok("A"), "b/b.go": ok("B")}, false},
		{"一处不调用 Detached", map[string]string{"a/a.go": ok("A"), "b/b.go": file(`func B() { ` + warn + ` }`)}, true},
		{"调用在别的函数里", map[string]string{"a/a.go": ok("A"), "b/b.go": file(`func B() { ` + warn + ` }
func other() { launch.Detached("x", nil, "") }`)}, true},
		{"函数不存在", map[string]string{"a/a.go": ok("A"), "b/b.go": ok("C")}, true},
		{"文件不存在", map[string]string{"a/a.go": ok("A")}, true},
		{"Warn 文案不一致", map[string]string{"a/a.go": ok("A"), "b/b.go": file(`func B() { d, _ := launch.Detached("x", nil, ""); if !d { log.Warn("%s was started directly.", name) } }`)}, true},
		{"一处没有 Warn", map[string]string{"a/a.go": ok("A"), "b/b.go": file(`func B() { launch.Detached("x", nil, "") }`)}, true},
		{"别名 import", map[string]string{"a/a.go": ok("A"), "b/b.go": "package p\nimport l \"Metabox-Nexus-PlayerCap/player/launch\"\n" +
			`func B() { d, _ := l.Detached("x", nil, ""); if !d { ` + warn + ` } }`}, false},
		{"经初始化为 launch.Detached 的包级变量调用", map[string]string{"a/a.go": ok("A"), "b/b.go": file(`var detachFn = launch.Detached
func B() { d, _ := detachFn("x", nil, ""); if !d { ` + warn + ` } }`)}, false},
		{"经初始化为别的函数的包级变量调用", map[string]string{"a/a.go": ok("A"), "b/b.go": file(`var detachFn = other
func other(string, []string, string) (bool, error) { return true, nil }
func B() { d, _ := detachFn("x", nil, ""); if !d { ` + warn + ` } }`)}, true},
		{"同名而不是 launch 包", map[string]string{"a/a.go": ok("A"), "b/b.go": "package p\n" +
			`type L struct{}
func (L) Detached(string, []string, string) (bool, error) { return true, nil }
var launch L
func B() { d, _ := launch.Detached("x", nil, ""); if !d { ` + warn + ` } }`}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, src := range c.files {
				p := filepath.Join(dir, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			problems := checkLaunchPoints(dir, points)
			if got := len(problems) > 0; got != c.bad {
				t.Errorf("判为不合规 = %v，期望 %v（%v）", got, c.bad, problems)
			}
		})
	}
}

// scanDirectStarts 扫 dir 下的非测试 .go 文件（launch/ 子目录除外），返回直接 Start() 了 exec.Command /
// exec.CommandContext 结果的位置（已排序）。
func scanDirectStarts(dir string) ([]string, error) {
	var found []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel, _ := filepath.Rel(dir, path); filepath.ToSlash(rel) == "launch" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // 解析不了的跳过，不是本门禁的职责
		}
		execName := importName(f, "os/exec")
		if execName == "" {
			return nil
		}
		isCtor := func(e ast.Expr) bool {
			call, ok := e.(*ast.CallExpr)
			if !ok {
				return false
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			x, ok := sel.X.(*ast.Ident)
			return ok && x.Name == execName && (sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext")
		}
		// 以顶层声明为单位：先收集被赋了 exec.Command 结果的变量与字段（按表达式文本），再找对它们或对构造调用本身的 .Start()。
		for _, decl := range f.Decls {
			assigned := map[string]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch s := n.(type) {
				case *ast.AssignStmt:
					for i, rhs := range s.Rhs {
						if i < len(s.Lhs) && isCtor(rhs) {
							assigned[types.ExprString(s.Lhs[i])] = true
						}
					}
				case *ast.ValueSpec:
					for i, v := range s.Values {
						if i < len(s.Names) && isCtor(v) {
							assigned[s.Names[i].Name] = true
						}
					}
				}
				return true
			})
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Start" {
					return true
				}
				if isCtor(sel.X) || assigned[types.ExprString(sel.X)] {
					found = append(found, posOf(fset, dir, call.Pos(), "Start"))
				}
				return true
			})
		}
		return nil
	})
	sort.Strings(found)
	return found, err
}

// checkLaunchPoints 核对 points（相对 dir 的文件 → 函数名）：每个函数都调用 launch.Detached，且各函数里有一条
// 逐字相同的 Warn。返回不合规之处。
func checkLaunchPoints(dir string, points map[string]string) []string {
	var problems []string
	var common map[string]bool // 各函数 Warn 文案的交集
	files := make([]string, 0, len(points))
	for f := range points {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, rel := range files {
		fn := points[rel]
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, filepath.FromSlash(rel)), nil, 0)
		if err != nil {
			problems = append(problems, rel+": "+err.Error())
			continue
		}
		launchName := importName(f, "Metabox-Nexus-PlayerCap/player/launch")
		isDetached := func(e ast.Expr) bool {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			x, ok := sel.X.(*ast.Ident)
			return ok && launchName != "" && x.Name == launchName && sel.Sel.Name == "Detached"
		}
		// 包级变量初始化为 launch.Detached 的（spotify 的 detachFn，供测试替换），经它调用也算。
		seams := map[string]bool{}
		var body *ast.BlockStmt
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Name.Name == fn && d.Body != nil {
					body = d.Body
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for i, v := range vs.Values {
							if i < len(vs.Names) && isDetached(v) {
								seams[vs.Names[i].Name] = true
							}
						}
					}
				}
			}
		}
		if body == nil {
			problems = append(problems, rel+": 找不到函数 "+fn)
			continue
		}
		calls := false
		warns := map[string]bool{}
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && seams[id.Name] {
				calls = true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if isDetached(sel) {
				calls = true
			}
			if sel.Sel.Name == "Warn" && len(call.Args) > 0 {
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil {
						warns[s] = true
					}
				}
			}
			return true
		})
		if !calls {
			problems = append(problems, rel+": "+fn+" 没有调用 launch.Detached")
		}
		if common == nil {
			common = warns
		} else {
			for w := range common {
				if !warns[w] {
					delete(common, w)
				}
			}
		}
	}
	if len(common) == 0 {
		problems = append(problems, "各拉起点之间没有一条逐字相同的 Warn（退回直接启动时打的那条须三处一致）")
	}
	return problems
}

// importName 返回文件里导入 path 时用的名字（默认取路径末段），没有导入则返回空串。
func importName(f *ast.File, path string) string {
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err == nil && p == path {
			if imp.Name != nil {
				return imp.Name.Name
			}
			return path[strings.LastIndex(path, "/")+1:]
		}
	}
	return ""
}
