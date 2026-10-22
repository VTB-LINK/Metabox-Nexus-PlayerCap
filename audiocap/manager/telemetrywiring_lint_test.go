package manager

// 本文件只守一件事：音频采集接入遥测（issue #120）的接线还在。它是 AST 门禁，扫的是源码，无构建约束。
//
// # 它守的是什么
//
//	manager_windows.go  reapHelper、teardownHelper 调 settleExit；放弃处（reapHelper、noteSpawnFailure）调
//	                    reportGaveUp，noteCaptureFailure 放弃时上报；看门狗与拆除调 markKilled；demux 调
//	                    recordExit；spawnHelper 起 stderr 读泵。reportLater 的实参只能是直接返回某个事件构造
//	                    函数调用的函数字面量；全文件不得就地写 TelemetryEvent 字面量或 map[string]string /
//	                    map[string]any 字面量；telemetry.ReportOnce 只在 reportToSentry 里调用。
//	main.go             信号处理在 <-sigCh 之后紧接着调 BeginShutdown；helper 分支在 helper.Run 之前紧接着以
//	                    Version、isReleaseVersion(Version) 与 Process: "audio-helper" 调 telemetry.Init。
//	helper_windows.go   Run 不再自己调 telemetry.Init。
//
// # 它不守什么
//
// 只查形状：调用按名字匹配、不做类型解析，不查调用是否落在正确的分支里（那由 manager_telemetry_windows_test.go
// 的行为用例守），不查 settleExit 等函数体内的逻辑。改名或挪走被扫的函数时门禁自报失效，须同步修改本测试。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// wiringCalls 是 manager_windows.go 里各函数体（含其中的函数字面量）必须出现的调用名。
var wiringCalls = map[string][]string{
	"reapHelper":           {"settleExit", "reportGaveUp"},
	"teardownHelper":       {"markKilled", "settleExit"},
	"killHelperForRestart": {"markKilled"},
	"noteSpawnFailure":     {"reportGaveUp"},
	"noteCaptureFailure":   {"reportLater", "CaptureGaveUpEvent"},
	"settleExit":           {"classifyExit", "reportLater", "HelperExitedEvent"},
	"reportGaveUp":         {"reportLater", "HelperGaveUpEvent"},
	"demux":                {"recordExit"},
	"spawnHelper":          {"pumpHelperStderr"},
}

// eventConstructors 是 TelemetryEvent 仅有的三个构造函数。
var eventConstructors = map[string]bool{"HelperExitedEvent": true, "HelperGaveUpEvent": true, "CaptureGaveUpEvent": true}

// callName 返回调用的函数名（标识符或选择器的末段）。
func callName(c *ast.CallExpr) string {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// isPkgCall 报告 c 是不是 pkg.name(...)。
func isPkgCall(c *ast.CallExpr, pkg, name string) bool {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// lintManagerTelemetry 扫 manager_windows.go 的遥测接线，返回违规描述。
func lintManagerTelemetry(fset *token.FileSet, f *ast.File) []string {
	var out []string
	report := func(pos token.Pos, format string, args ...any) {
		out = append(out, fmt.Sprintf("%s:%d: %s", fset.Position(pos).Filename, fset.Position(pos).Line, fmt.Sprintf(format, args...)))
	}

	funcs := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			funcs[fd.Name.Name] = fd
		}
	}
	for name, calls := range wiringCalls {
		fd, ok := funcs[name]
		if !ok {
			report(f.Package, "找不到 %s（改名或挪走后本门禁失效，须同步修改本测试）", name)
			continue
		}
		seen := map[string]bool{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				seen[callName(c)] = true
			}
			return true
		})
		for _, want := range calls {
			if !seen[want] {
				report(fd.Pos(), "%s 里没有对 %s 的调用", name, want)
			}
		}
	}

	var enclosing string
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			enclosing = fd.Name.Name
		} else {
			enclosing = ""
		}
		ast.Inspect(d, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if id, ok := x.Type.(*ast.Ident); ok && id.Name == "TelemetryEvent" {
					report(x.Pos(), "就地写了 TelemetryEvent 字面量：事件只能经构造函数产生")
				}
				if mt, ok := x.Type.(*ast.MapType); ok && isIdent(mt.Key, "string") && (isIdent(mt.Value, "string") || isIdent(mt.Value, "any") || isEmptyInterface(mt.Value)) {
					report(x.Pos(), "就地写了 map[string]%s 字面量：上报的 tags / extra 只能经构造函数产生", exprString(mt.Value))
				}
			case *ast.CallExpr:
				if isPkgCall(x, "telemetry", "ReportOnce") && enclosing != "reportToSentry" {
					report(x.Pos(), "telemetry.ReportOnce 只能在 reportToSentry 里调用（%s 绕过了延后与关停检查）", enclosing)
				}
				if callName(x) == "reportLater" && !reportsViaConstructor(x) {
					report(x.Pos(), "reportLater 的实参须是直接 return 某个事件构造函数调用的函数字面量")
				}
			}
			return true
		})
	}
	return out
}

// reportsViaConstructor 报告 reportLater 调用的唯一实参是 func() TelemetryEvent { return <构造函数>(…) }。
func reportsViaConstructor(c *ast.CallExpr) bool {
	if len(c.Args) != 1 {
		return false
	}
	fl, ok := c.Args[0].(*ast.FuncLit)
	if !ok || len(fl.Body.List) != 1 {
		return false
	}
	ret, ok := fl.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	call, ok := ret.Results[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && eventConstructors[id.Name]
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func isEmptyInterface(e ast.Expr) bool {
	it, ok := e.(*ast.InterfaceType)
	return ok && (it.Methods == nil || len(it.Methods.List) == 0)
}

func exprString(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "interface{}"
}

// lintMainTelemetry 扫 main.go：信号处理紧接 <-sigCh 调 BeginShutdown；helper 分支紧接 helper.Run 之前调
// telemetry.Init，且带 Version、IsRelease 与 Process: "audio-helper"。
func lintMainTelemetry(fset *token.FileSet, f *ast.File) []string {
	var out []string
	report := func(pos token.Pos, msg string) {
		out = append(out, fmt.Sprintf("%s:%d: %s", fset.Position(pos).Filename, fset.Position(pos).Line, msg))
	}
	var mainFn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "main" {
			mainFn = fd
		}
	}
	if mainFn == nil {
		report(f.Package, "找不到 main 函数")
		return out
	}

	foundSig, foundHelper := false, false
	ast.Inspect(mainFn.Body, func(n ast.Node) bool {
		b, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}
		for i, s := range b.List {
			if isRecvFrom(s, "sigCh") {
				foundSig = true
				if i+1 >= len(b.List) || !isMethodCallStmt(b.List[i+1], "BeginShutdown") {
					report(s.Pos(), "信号处理在 <-sigCh 之后没有紧接着调 BeginShutdown")
				}
			}
			if c := exprCall(s); c != nil && isPkgCall(c, "helper", "Run") {
				foundHelper = true
				if i == 0 {
					report(s.Pos(), "helper 分支在 helper.Run 之前没有调 telemetry.Init")
				} else if msg := checkHelperInit(b.List[i-1]); msg != "" {
					report(b.List[i-1].Pos(), msg)
				}
			}
		}
		return true
	})
	if !foundSig {
		report(mainFn.Pos(), "main 里找不到 <-sigCh（信号处理改写后本门禁失效，须同步修改本测试）")
	}
	if !foundHelper {
		report(mainFn.Pos(), "main 里找不到 helper.Run（helper 分支改写后本门禁失效，须同步修改本测试）")
	}
	return out
}

func exprCall(s ast.Stmt) *ast.CallExpr {
	if es, ok := s.(*ast.ExprStmt); ok {
		if c, ok := es.X.(*ast.CallExpr); ok {
			return c
		}
	}
	return nil
}

func isRecvFrom(s ast.Stmt, ch string) bool {
	es, ok := s.(*ast.ExprStmt)
	if !ok {
		return false
	}
	u, ok := es.X.(*ast.UnaryExpr)
	return ok && u.Op == token.ARROW && isIdent(u.X, ch)
}

func isMethodCallStmt(s ast.Stmt, name string) bool {
	c := exprCall(s)
	if c == nil {
		return false
	}
	_, isSel := c.Fun.(*ast.SelectorExpr)
	return isSel && callName(c) == name
}

// checkHelperInit 检查 helper.Run 之前的那条语句，合规返回空串。
func checkHelperInit(s ast.Stmt) string {
	c := exprCall(s)
	if c == nil || !isPkgCall(c, "telemetry", "Init") || len(c.Args) != 1 {
		return "helper 分支在 helper.Run 之前紧接着的不是 telemetry.Init(…)"
	}
	lit, ok := c.Args[0].(*ast.CompositeLit)
	if !ok {
		return "helper 分支的 telemetry.Init 实参不是 telemetry.Options 字面量"
	}
	fields := map[string]ast.Expr{}
	for _, e := range lit.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok {
				fields[k.Name] = kv.Value
			}
		}
	}
	if v, ok := fields["Process"].(*ast.BasicLit); !ok || v.Value != strconv.Quote("audio-helper") {
		return `helper 分支的 telemetry.Init 没有 Process: "audio-helper"`
	}
	if !isIdent(fields["Version"], "Version") {
		return "helper 分支的 telemetry.Init 没有 Version: Version"
	}
	if ic, ok := fields["IsRelease"].(*ast.CallExpr); !ok || !isIdent(ic.Fun, "isReleaseVersion") || len(ic.Args) != 1 || !isIdent(ic.Args[0], "Version") {
		return "helper 分支的 telemetry.Init 没有 IsRelease: isReleaseVersion(Version)"
	}
	return ""
}

// lintHelperTelemetry 扫 helper_windows.go：Run 里不得调 telemetry.Init（由 main.go 的 helper 分支负责）。
func lintHelperTelemetry(fset *token.FileSet, f *ast.File) []string {
	var out []string
	var run *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "Run" {
			run = fd
		}
	}
	if run == nil {
		return []string{"helper_windows.go 里找不到 Run（改名或挪走后本门禁失效，须同步修改本测试）"}
	}
	ast.Inspect(run.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && isPkgCall(c, "telemetry", "Init") {
			out = append(out, fmt.Sprintf("%s:%d: helper.Run 里调了 telemetry.Init：会用不带版本与 process 标签的选项覆盖 main.go 的初始化", fset.Position(c.Pos()).Filename, fset.Position(c.Pos()).Line))
		}
		return true
	})
	return out
}

func parseFileOrSrc(t *testing.T, filename string, src any) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", filename, err)
	}
	return fset, f
}

// TestTelemetryWiring 扫真实文件。
//
// 变异自证（实测）：删掉 reapHelper 或 teardownHelper 里的 settleExit、noteSpawnFailure 里的 reportGaveUp、
// main.go 信号处理里的 BeginShutdown、helper 分支里的 telemetry.Init，或往 helper.Run 里加回空选项的 Init，均红。
func TestTelemetryWiring(t *testing.T) {
	for _, c := range []struct {
		file string
		lint func(*token.FileSet, *ast.File) []string
	}{
		{"manager_windows.go", lintManagerTelemetry},
		{"../../main.go", lintMainTelemetry},
		{"../helper/helper_windows.go", lintHelperTelemetry},
	} {
		t.Run(c.file, func(t *testing.T) {
			fset, f := parseFileOrSrc(t, c.file, nil)
			for _, v := range c.lint(fset, f) {
				t.Error(v)
			}
		})
	}
}

// managerWiringBase 是合规的 manager 片段，下面的反例在它之上逐处改坏。
const managerWiringBase = `package manager
import "Metabox-Nexus-PlayerCap/telemetry"
func reportToSentry(ev TelemetryEvent) { telemetry.ReportOnce(ev.Key, ev.Message, ev.Tags, ev.Extra) }
func New() *Manager { return &Manager{dispatched: map[string]uint32{}, at: map[string]time.Time{}} }
func (m *Manager) spawnHelper() error { go m.pumpHelperStderr(r, tail); go m.demux(hp, stdout); return nil }
func (m *Manager) demux(hp *helperProc, r io.Reader) { hp.cmd.Wait(); hp.recordExit() }
func (m *Manager) reapHelper() { m.quickFails++; m.settleExit(hp); if m.quickFails >= maxQuickFails { m.reportGaveUp(causeQuickExit, nil) } }
func (m *Manager) teardownHelper() { hp.markKilled(); <-hp.exited; m.settleExit(hp) }
func (m *Manager) killHelperForRestart() { m.helper.markKilled() }
func (m *Manager) noteSpawnFailure(err error) { if x { m.reportGaveUp(causeSpawnFailure, err) } }
func (m *Manager) noteCaptureFailure(p string) { if g { m.reportLater(func() TelemetryEvent { return CaptureGaveUpEvent(p, 1, tail.Snapshot()) }) } }
func (m *Manager) settleExit(hp *helperProc) { if !classifyExit(e, k) { return }; m.reportLater(func() TelemetryEvent { return HelperExitedEvent(1, 0, 0, "") }) }
func (m *Manager) reportGaveUp(c string, err error) { m.reportLater(func() TelemetryEvent { return HelperGaveUpEvent(c, 0, 0, false, err, "") }) }
func (m *Manager) reportLater(build func() TelemetryEvent) {}
`

const mainWiringBase = `package main
func main() {
	defer telemetry.Guard()
	if len(os.Args) >= 2 && os.Args[1] == "--audio-capture-helper" {
		telemetry.Init(telemetry.Options{Version: Version, IsRelease: isReleaseVersion(Version), Process: "audio-helper"})
		helper.Run(os.Stdin, os.Stdout)
		return
	}
	go func() {
		defer telemetry.Guard()
		<-sigCh
		audioMgr.BeginShutdown()
		mainLog.Info("x")
		os.Exit(0)
	}()
}
`

const helperWiringBase = `package helper
func Run(stdin io.Reader, stdout io.Writer) {
	log.SetOutput(os.Stderr)
	defer telemetry.Guard()
}
`

// mutate 在 base 上把 old 换成 new，old 必须恰好出现一次。
func mutate(t *testing.T, base, old, new string) string {
	t.Helper()
	if n := strings.Count(base, old); n != 1 {
		t.Fatalf("片段里 %q 出现 %d 次，期望 1", old, n)
	}
	return strings.Replace(base, old, new, 1)
}

// TestTelemetryWiringLintCatchesKnownHoles 钉死门禁自身认得出要堵的形状、不误报合规写法。
func TestTelemetryWiringLintCatchesKnownHoles(t *testing.T) {
	type lintCase struct {
		lint func(*token.FileSet, *ast.File) []string
		src  string
	}
	mgr := func(old, new string) lintCase {
		return lintCase{lintManagerTelemetry, mutate(t, managerWiringBase, old, new)}
	}
	mainc := func(old, new string) lintCase {
		return lintCase{lintMainTelemetry, mutate(t, mainWiringBase, old, new)}
	}

	shouldCatch := map[string]lintCase{
		"reapHelper 不结算":                 mgr("m.quickFails++; m.settleExit(hp);", "m.quickFails++;"),
		"reapHelper 放弃时不上报":              mgr("{ m.reportGaveUp(causeQuickExit, nil) }", "{ }"),
		"teardownHelper 不结算":             mgr("<-hp.exited; m.settleExit(hp)", "<-hp.exited"),
		"teardownHelper 不记下手":            mgr("hp.markKilled(); <-hp.exited", "<-hp.exited"),
		"看门狗不记下手":                        mgr("{ m.helper.markKilled() }", "{ }"),
		"noteSpawnFailure 不上报":           mgr("{ m.reportGaveUp(causeSpawnFailure, err) }", "{ }"),
		"noteCaptureFailure 不上报":         mgr("{ m.reportLater(func() TelemetryEvent { return CaptureGaveUpEvent(p, 1, tail.Snapshot()) }) }", "{ }"),
		"settleExit 不分类":                 mgr("if !classifyExit(e, k) { return }; ", ""),
		"demux 不记退出":                     mgr("hp.cmd.Wait(); hp.recordExit()", "hp.cmd.Wait()"),
		"spawnHelper 不起读泵":               mgr("go m.pumpHelperStderr(r, tail); ", ""),
		"被扫的函数改名":                        mgr("func (m *Manager) settleExit(", "func (m *Manager) settle("),
		"就地写 TelemetryEvent 字面量":         mgr("return CaptureGaveUpEvent(p, 1, tail.Snapshot())", `return TelemetryEvent{Key: "audiocap.capture_gave_up"}`),
		"就地写 map[string]string 字面量":      mgr("func (m *Manager) reportLater(build func() TelemetryEvent) {}", `func (m *Manager) reportLater(build func() TelemetryEvent) { tags := map[string]string{"player": "x"}; _ = tags }`),
		"就地写 map[string]any 字面量":         mgr("func (m *Manager) reportLater(build func() TelemetryEvent) {}", `func (m *Manager) reportLater(build func() TelemetryEvent) { _ = map[string]any{"failures": 1} }`),
		"就地写 map[string]interface{} 字面量": mgr("func (m *Manager) reportLater(build func() TelemetryEvent) {}", `func (m *Manager) reportLater(build func() TelemetryEvent) { _ = map[string]interface{}{"failures": 1} }`),
		"ReportOnce 绕过 reportToSentry":   mgr("{ m.helper.markKilled() }", `{ m.helper.markKilled(); telemetry.ReportOnce("k", "m", nil, nil) }`),
		"reportLater 传函数变量":              mgr("m.reportLater(func() TelemetryEvent { return HelperExitedEvent(1, 0, 0, \"\") })", "m.reportLater(build); HelperExitedEvent(1, 0, 0, \"\")"),
		"reportLater 的字面量不直接返回构造函数":      mgr("func() TelemetryEvent { return HelperGaveUpEvent(c, 0, 0, false, err, \"\") }", "func() TelemetryEvent { ev := HelperGaveUpEvent(c, 0, 0, false, err, \"\"); return ev }"),
		"reportLater 的字面量返回别的函数":         mgr("return HelperExitedEvent(1, 0, 0, \"\")", "return otherEvent(HelperExitedEvent(1, 0, 0, \"\"))"),

		"信号处理不调 BeginShutdown": mainc("		audioMgr.BeginShutdown()\n", ""),
		"BeginShutdown 不在最开头":  mainc("		audioMgr.BeginShutdown()\n		mainLog.Info(\"x\")\n", "		mainLog.Info(\"x\")\n		audioMgr.BeginShutdown()\n"),
		"BeginShutdown 在条件分支里": mainc("		audioMgr.BeginShutdown()\n", "		if ok {\n			audioMgr.BeginShutdown()\n		}\n"),
		"找不到 <-sigCh":          mainc("		<-sigCh\n", "		<-other\n"),
		"helper 分支不 Init":      mainc("		telemetry.Init(telemetry.Options{Version: Version, IsRelease: isReleaseVersion(Version), Process: \"audio-helper\"})\n", ""),
		"Init 在 helper.Run 之后": mainc("		telemetry.Init(telemetry.Options{Version: Version, IsRelease: isReleaseVersion(Version), Process: \"audio-helper\"})\n		helper.Run(os.Stdin, os.Stdout)\n", "		helper.Run(os.Stdin, os.Stdout)\n		telemetry.Init(telemetry.Options{Version: Version, IsRelease: isReleaseVersion(Version), Process: \"audio-helper\"})\n"),
		"Init 缺 Process":       mainc(", Process: \"audio-helper\"", ""),
		"Init 的 Process 取值不对":  mainc("Process: \"audio-helper\"", "Process: \"helper\""),
		"Init 缺 Version":       mainc("Version: Version, ", ""),
		"Init 的 Version 写死":    mainc("Version: Version, ", "Version: \"4.0.0\", "),
		"Init 缺 IsRelease":     mainc("IsRelease: isReleaseVersion(Version), ", ""),
		"Init 的 IsRelease 写死":  mainc("IsRelease: isReleaseVersion(Version)", "IsRelease: true"),
		"Init 用空选项":            mainc("telemetry.Options{Version: Version, IsRelease: isReleaseVersion(Version), Process: \"audio-helper\"}", "telemetry.Options{}"),
		"找不到 helper.Run":       mainc("helper.Run(os.Stdin, os.Stdout)", "helper.Serve(os.Stdin, os.Stdout)"),

		"helper.Run 里加回 Init": {lintHelperTelemetry, mutate(t, helperWiringBase, "	defer telemetry.Guard()\n", "	telemetry.Init(telemetry.Options{})\n	defer telemetry.Guard()\n")},
		"helper 找不到 Run":      {lintHelperTelemetry, mutate(t, helperWiringBase, "func Run(", "func Serve(")},
	}
	for name, c := range shouldCatch {
		t.Run("catch/"+name, func(t *testing.T) {
			fset, f := parseFileOrSrc(t, "snippet.go", c.src)
			if v := c.lint(fset, f); len(v) == 0 {
				t.Errorf("门禁漏报了这种形状：\n%s", c.src)
			}
		})
	}

	shouldPass := map[string]lintCase{
		"manager 合规":         {lintManagerTelemetry, managerWiringBase},
		"main 合规":            {lintMainTelemetry, mainWiringBase},
		"main 的 Init 键序不同":   mainc("telemetry.Options{Version: Version, IsRelease: isReleaseVersion(Version), Process: \"audio-helper\"}", "telemetry.Options{Process: \"audio-helper\", IsRelease: isReleaseVersion(Version), Version: Version}"),
		"helper 合规":          {lintHelperTelemetry, helperWiringBase},
		"manager 别的 map 字面量": mgr("func (m *Manager) reportLater(build func() TelemetryEvent) {}", "func (m *Manager) reportLater(build func() TelemetryEvent) { _ = map[string]uint32{}; _ = map[string]captureFailure{} }"),
		"settleExit 里多了别的语句": mgr("if !classifyExit(e, k) { return }; ", "m.lastExit = e; if !classifyExit(e, k) { return }; "),
	}
	for name, c := range shouldPass {
		t.Run("pass/"+name, func(t *testing.T) {
			fset, f := parseFileOrSrc(t, "snippet.go", c.src)
			if v := c.lint(fset, f); len(v) > 0 {
				t.Errorf("门禁误报：%v\n%s", v, c.src)
			}
		})
	}
}
