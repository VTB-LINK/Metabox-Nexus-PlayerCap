package player

// 本文件只测一件事：AST 门禁「结束、重启、修补、注入播放器之前必须先过 launch.Gate」（issue #212）。
//
// # 为什么要门禁
//
// 微软商店版播放器（WindowsApps 下的 MSIX 包）结束了拉不回来、补丁写不进、带参数重启起不来。rc.12 的网易云
// 先被 taskkill 结束、再拉起失败，用户的播放器被关掉且不再启动。门禁只有一行，挪到侵入操作之后、挪出重连循环、
// 或在新的侵入路径上漏掉，各播放器用例里的桩未必看得出，本门禁按形状查。
//
// # 它守什么
//
// 登记表 storeGateSites：文件（相对 player/）→ 函数 → 侵入调用的匹配器。对每个登记的函数：
//
//	(a) 至少匹配到一个侵入调用。匹配器失效、函数被改名或拆走时红，而不是静默通过。
//	(b) 每个侵入调用之前都有一个门禁调用：门禁所在的语句列表与侵入调用所在的相同或是其外层，门禁所在的语句先于
//	    含侵入调用的那条语句，且两者之间不隔 for / range 循环或函数字面量的边界（门禁在循环外、侵入调用在循环内
//	    即红：每轮重连都要重新核验）。
//
// 门禁调用：launch.Gate（按 import 认包），或名字以 gate / Gate 开头、经函数体或初始化表达式通到 launch.Gate 的
// 包级函数与变量——同包的，或经 import 引用的本模块另一个包里的（如特效经 watchdog.GateRunning）。辅助函数要以
// gate 开头命名，免得 EnsureDebugMode 这类「内部过了门禁、随后又结束进程」的函数被当成门禁。
//
// # 它不守什么
//
// 不查门禁的返回值有没有被用来中止（调用了门禁、不看结果照样绿）；不查门禁核验的是不是被操作的那个进程；不做类型
// 解析，同名的别的函数会被当成门禁或侵入调用；登记表之外的侵入路径查不出，新增结束、重启、修补、注入播放器的
// 路径时须登记进来（AGENTS.md §3.6）。

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

// gateMatcher 认一个侵入调用：调用表达式的被调方写作 fun（types.ExprString 的结果，如 "killFn"、"m.ops.stop"），
// arg0 非空时第一个实参还须是与之相等的字符串字面量（特效按 CDP 方法名认发送调用）。
type gateMatcher struct {
	fun  string
	arg0 string
}

// storeGateSites 是登记的侵入点：相对 player/ 的文件 → 函数名（方法只写方法名）→ 侵入调用的匹配器。每个函数里
// 匹配到的侵入调用之前都须有门禁。现有代码里没有的写法（网易云直接写 taskkill、直接调 launch.Detached）也登记上，
// 改回那种写法时同样要求门禁在前。
var storeGateSites = map[string]map[string][]gateMatcher{
	"cloudmusic/watchdog/process.go": {
		"EnsureDebugMode": {{fun: "killFn"}, {fun: "detachFn"}, {fun: "exec.Command", arg0: "taskkill"}, {fun: "launch.Detached"}},
	},
	"cloudmusic/effect/effect.go": {
		"injectShim": {{fun: "send", arg0: "Page.addScriptToEvaluateOnNewDocument"}, {fun: "send", arg0: "Runtime.evaluate"}},
		"doPark":     {{fun: "parkFn"}, {fun: "park.Park"}},
	},
	"spotify/watchdog/watchdog.go": {
		"EnsureDebugMode": {{fun: "killFn"}, {fun: "startFn"}},
	},
	"kuwo/watchdog/watchdog.go": {
		"ensure": {{fun: "m.ops.stop"}, {fun: "m.ops.start"}},
	},
	"kugou/watchdog/watchdog.go": {
		"EnsurePatched": {{fun: "launchElevatedHelper"}, {fun: "killAndPatch"}, {fun: "launchKuGou"}},
		"killAndPatch":  {{fun: "kill"}},
	},
	// 酷狗等 CDP 的循环每轮连接之前都过门禁：连接本身不是侵入调用，但随后的取词会话在酷狗里执行脚本，而等待最长 90 秒，
	// 其间可能换成另一份实例。
	"kugou/kugou.go": {
		"waitForCDP": {{fun: "connectCDP"}, {fun: "cdp.Connect"}},
	},
	"sodamusic/watchdog/watchdog.go": {
		"EnsureInspector":   {{fun: "activateInspector"}},
		"activateInspector": {{fun: "windows.OpenProcess"}},
	},
}

// TestStoreGateBeforeInvasiveCalls 是门禁本身。
//
// 变异自证（均实测变红）：逐个登记函数删掉门禁调用；把门禁挪到侵入调用之后；把特效 injectShim 里的门禁挪到
// shimInjectLoop 的循环之外（只核验一次）；把网易云 watchdog 的 gateAll 改名为不以 gate 开头的名字。酷狗
// EnsurePatched 有两处门禁，只删开头那处本门禁不红（另一处仍在侵入调用之前），它先于「CDP 已可用」的早退由
// kugou/watchdog 的 TestEnsurePatchedGateFirst 守；两处都删即红。
func TestStoreGateBeforeInvasiveCalls(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	if problems := checkStoreGate(root, filepath.Join(root, "player"), storeGateSites); len(problems) > 0 {
		t.Errorf("以下侵入调用之前没有商店版门禁：\n  %s\n\n"+
			"结束、重启、修补、注入播放器之前必须先过 launch.Gate（AGENTS.md §3.6，issue #212）：门禁与侵入调用在同一语句列表"+
			"或其外层、在前，中间不隔循环或函数字面量。", strings.Join(problems, "\n  "))
	}
}

// TestStoreGateLintCatchesKnownShapes 钉住门禁认得出与不误报的形状（教训同 callback_lint_test.go：抓不住目标形状的
// 门禁比没有门禁更糟）。夹具里 kill() 是侵入调用。
//
// 变异自证（均实测变红）：不看循环边界（「循环外门禁一次、循环内反复侵入」等转绿）；不看函数字面量边界（「侵入在
// 闭包里、门禁在外」转绿）；一律取函数体那一层比先后、不要求门禁在同一或外层列表（「门禁在 if 分支里」转绿，循环内、
// 分支里先门禁后侵入被误报）；不要求每个登记函数至少匹配到一个侵入调用；不按名字前缀限定辅助函数（「名字不以 gate
// 开头的辅助函数」转绿）；辅助函数只认同包的（扫真实代码的用例红在特效）；不看 import、按包名字面认 launch
// （「同名而不是 launch 包」转绿）；测试文件里的辅助函数也算。
func TestStoreGateLintCatchesKnownShapes(t *testing.T) {
	const launchImp = `import "Metabox-Nexus-PlayerCap/player/launch"` + "\n"
	pkg := func(body string) map[string]string {
		return map[string]string{"p/a.go": "package p\n" + launchImp + "func kill() {}\n" + body}
	}
	site := map[string]map[string][]gateMatcher{"p/a.go": {"F": {{fun: "kill"}}}}
	cases := []struct {
		name  string
		files map[string]string
		sites map[string]map[string][]gateMatcher
		bad   bool
	}{
		{"先门禁后侵入", pkg(`func F(p string) { if err := launch.Gate(p); err != nil { return }; kill() }`), site, false},
		{"循环内先门禁后侵入", pkg(`func F(ps []string) { for _, p := range ps { if launch.Gate(p) != nil { continue }; kill() } }`), site, false},
		{"门禁在外层列表、侵入在内层 if 里", pkg(`func F(p string, x bool) { if launch.Gate(p) != nil { return }; if x { kill() } }`), site, false},
		{"门禁在外、侵入在 switch 的分支里", pkg(`func F(p string, x int) { if launch.Gate(p) != nil { return }; switch x { case 1: kill() } }`), site, false},
		{"分支里先门禁后侵入", pkg(`func F(p string, x int) { switch x { case 1: if launch.Gate(p) != nil { return }; kill() } }`), site, false},
		{"门禁结果存进变量", pkg(`func F(p string) { err := launch.Gate(p); if err != nil { return }; kill() }`), site, false},
		{"别名 import", map[string]string{"p/a.go": "package p\n" + `import l "Metabox-Nexus-PlayerCap/player/launch"` + "\n" +
			`func kill() {}
func F(p string) { if l.Gate(p) != nil { return }; kill() }`}, site, false},
		{"经同包 gate 开头的辅助函数", pkg(`func gateAll(ps []string) error { for _, p := range ps { if err := launch.Gate(p); err != nil { return err } }; return nil }
func F(ps []string) { if gateAll(ps) != nil { return }; kill() }`), site, false},
		{"辅助函数经另一个辅助函数通到门禁", pkg(`func gateOne(p string) error { return launch.Gate(p) }
func gateAll(ps []string) error { for _, p := range ps { if err := gateOne(p); err != nil { return err } }; return nil }
func F(ps []string) { if gateAll(ps) != nil { return }; kill() }`), site, false},
		{"经初始化为 launch.Gate 的包级变量", pkg(`var gateFn = launch.Gate
func F(p string) { if gateFn(p) != nil { return }; kill() }`), site, false},
		{"经初始化为函数字面量的包级变量", pkg(`var gateFn = func() error { return gateKG(nil) }
func gateKG(ps []string) error { for _, p := range ps { if err := launch.Gate(p); err != nil { return err } }; return nil }
func F() { if gateFn() != nil { return }; kill() }`), site, false},
		{"经另一个包的门禁辅助函数", map[string]string{
			"w/w.go": "package w\n" + launchImp + `func GateRunning() error { return launch.Gate("") }`,
			"p/a.go": "package p\n" + `import "Metabox-Nexus-PlayerCap/w"` + "\n" + `var gateFn = w.GateRunning
func kill() {}
func F() { if gateFn() != nil { return }; kill() }`}, site, false},
		{"按方法名认发送调用：门禁在注入命令之前、在无关命令之后", map[string]string{"p/a.go": "package p\n" + launchImp +
			`func F(send func(string)) { send("Page.enable"); if launch.Gate("") != nil { return }; send("Runtime.evaluate") }`},
			map[string]map[string][]gateMatcher{"p/a.go": {"F": {{fun: "send", arg0: "Runtime.evaluate"}}}}, false},
		{"方法", pkg(`type m struct{}
func (m) F(p string) { if launch.Gate(p) != nil { return }; kill() }`), site, false},

		{"没有门禁", pkg(`func F(p string) { kill() }`), site, true},
		{"门禁在侵入之后", pkg(`func F(p string) { kill(); if launch.Gate(p) != nil { return } }`), site, true},
		{"门禁与侵入在同一条语句", pkg(`func F(p string) { if launch.Gate(p) == nil { kill() } }`), site, true},
		{"循环外门禁一次、循环内反复侵入", pkg(`func F(p string) { if launch.Gate(p) != nil { return }; for { kill() } }`), site, true},
		{"循环外门禁一次、range 内反复侵入", pkg(`func F(p string, n []int) { if launch.Gate(p) != nil { return }; for range n { kill() } }`), site, true},
		{"门禁在 for 的初始化里、侵入在循环体里", pkg(`func F(p string) { for err := launch.Gate(p); err == nil; { kill() } }`), site, true},
		{"门禁在 if 分支里", pkg(`func F(p string, x bool) { if x { if launch.Gate(p) != nil { return } }; kill() }`), site, true},
		{"门禁在闭包里", pkg(`func F(p string) { g := func() error { return launch.Gate(p) }; _ = g; kill() }`), site, true},
		{"侵入在闭包里、门禁在外", pkg(`func F(p string) { if launch.Gate(p) != nil { return }; go func() { kill() }() }`), site, true},
		{"两个侵入只有第一个前面有门禁", pkg(`func F(p string) { if launch.Gate(p) != nil { return }; kill(); for { kill() } }`), site, true},
		{"名字不以 gate 开头的辅助函数", pkg(`func check(p string) error { return launch.Gate(p) }
func F(p string) { if check(p) != nil { return }; kill() }`), site, true},
		{"名字以 gate 开头却通不到门禁", pkg(`func gateAll(p string) error { return nil }
func F(p string) { if gateAll(p) != nil { return }; kill() }`), site, true},
		{"包级变量初始化为别的函数", pkg(`var gateFn = func(string) error { return nil }
func F(p string) { if gateFn(p) != nil { return }; kill() }`), site, true},
		{"另一个包的同名函数通不到门禁", map[string]string{
			"w/w.go": "package w\n" + `func GateRunning() error { return nil }`,
			"p/a.go": "package p\n" + `import "Metabox-Nexus-PlayerCap/w"` + "\n" + `func kill() {}
func F() { if w.GateRunning() != nil { return }; kill() }`}, site, true},
		{"同名而不是 launch 包", map[string]string{"p/a.go": "package p\n" + `type L struct{}
func (L) Gate(string) error { return nil }
var launch L
func kill() {}
func F(p string) { if launch.Gate(p) != nil { return }; kill() }`}, site, true},
		{"辅助函数写在测试文件里", map[string]string{
			"p/a.go": "package p\n" + `func kill() {}
func F(p string) { if gateAll(p) != nil { return }; kill() }`,
			"p/a_test.go": "package p\n" + launchImp + `func gateAll(p string) error { return launch.Gate(p) }`}, site, true},
		{"匹配不到侵入调用", pkg(`func F(p string) { if launch.Gate(p) != nil { return } }`), site, true},
		{"方法名不符的发送调用不算侵入、匹配不到即红", map[string]string{"p/a.go": "package p\n" + launchImp +
			`func F(send func(string)) { send("Page.enable") }`},
			map[string]map[string][]gateMatcher{"p/a.go": {"F": {{fun: "send", arg0: "Runtime.evaluate"}}}}, true},
		{"函数不存在", pkg(`func G(p string) { if launch.Gate(p) != nil { return }; kill() }`), site, true},
		{"文件不存在", map[string]string{"p/b.go": "package p\n"}, site, true},
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
			problems := checkStoreGate(dir, dir, c.sites)
			if got := len(problems) > 0; got != c.bad {
				t.Errorf("判为不合规 = %v，期望 %v（%v）", got, c.bad, problems)
			}
		})
	}
}

// storeGateModule 是本模块的路径：import 路径去掉它即相对模块根的目录。
const storeGateModule = "Metabox-Nexus-PlayerCap/"

// checkStoreGate 按 sites（相对 base 的文件 → 函数 → 侵入调用的匹配器）核对门禁，返回不合规之处（已排序）。本模块的
// import 路径按 root 映射到目录。
func checkStoreGate(root, base string, sites map[string]map[string][]gateMatcher) []string {
	idx := &gateIndex{root: root, cache: map[string]map[string]bool{}}
	var problems []string
	for rel, funcs := range sites {
		path := filepath.Join(base, filepath.FromSlash(rel))
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			problems = append(problems, rel+": "+err.Error())
			continue
		}
		gates := idx.gates(filepath.Dir(path))
		imports := fileImports(f)
		for fn, matchers := range funcs {
			body := funcBody(f, fn)
			if body == nil {
				problems = append(problems, rel+": 找不到函数 "+fn)
				continue
			}
			problems = append(problems, checkGateOrder(fset, base, rel+": "+fn, body, matchers, func(call *ast.CallExpr) bool {
				return idx.isGateRef(call.Fun, imports, gates)
			})...)
		}
	}
	sort.Strings(problems)
	return problems
}

// funcBody 返回文件里名为 name 的函数或方法的函数体，没有则返回 nil。
func funcBody(f *ast.File, name string) *ast.BlockStmt {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name && fd.Body != nil {
			return fd.Body
		}
	}
	return nil
}

// callSite 是函数体里的一个调用及其祖先链（自函数体起，不含调用本身）。
type callSite struct {
	call  *ast.CallExpr
	chain []ast.Node
}

// checkGateOrder 核对 body 里每个匹配 matchers 的侵入调用之前都有 isGate 认得的门禁调用（规则见文件头），且至少
// 匹配到一个侵入调用。
func checkGateOrder(fset *token.FileSet, base, where string, body *ast.BlockStmt, matchers []gateMatcher, isGate func(*ast.CallExpr) bool) []string {
	var gates, invasive []callSite
	var stack []ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if call, ok := n.(*ast.CallExpr); ok {
			s := callSite{call, append([]ast.Node(nil), stack...)}
			if isGate(call) {
				gates = append(gates, s)
			}
			if matchesAny(call, matchers) {
				invasive = append(invasive, s)
			}
		}
		stack = append(stack, n)
		return true
	})
	if len(invasive) == 0 {
		return []string{where + " 匹配不到任何侵入调用（匹配器失效，或函数已改写）"}
	}
	var problems []string
	for _, inv := range invasive {
		guarded := false
		for _, g := range gates {
			if gateGuards(g.chain, inv.chain) {
				guarded = true
				break
			}
		}
		if !guarded {
			problems = append(problems, where+" 的 "+posOf(fset, base, inv.call.Pos(), types.ExprString(inv.call.Fun))+" 之前没有门禁")
		}
	}
	return problems
}

// gateGuards 报告祖先链为 g 的门禁调用是否守住祖先链为 inv 的侵入调用：取门禁最近的语句列表 L 与 L 里含门禁的语句，
// L 须在侵入调用的祖先链上，L 里含侵入调用的语句排在后面，且自该语句起到侵入调用之间没有循环或函数字面量。
func gateGuards(g, inv []ast.Node) bool {
	li := -1
	for i := len(g) - 1; i >= 0; i-- {
		if stmtList(g[i]) != nil {
			li = i
			break
		}
	}
	if li < 0 || li+1 >= len(g) {
		return false
	}
	owner, gateStmt := g[li], g[li+1]
	for j, n := range inv {
		if n != owner {
			continue
		}
		if j+1 >= len(inv) || stmtIndex(owner, inv[j+1]) <= stmtIndex(owner, gateStmt) {
			return false
		}
		for _, m := range inv[j+1:] {
			switch m.(type) {
			case *ast.ForStmt, *ast.RangeStmt, *ast.FuncLit:
				return false
			}
		}
		return true
	}
	return false
}

// stmtList 返回 n 直接持有的语句列表（代码块、switch / select 的分支），不持有则返回 nil。
func stmtList(n ast.Node) []ast.Stmt {
	switch n := n.(type) {
	case *ast.BlockStmt:
		return n.List
	case *ast.CaseClause:
		return n.Body
	case *ast.CommClause:
		return n.Body
	}
	return nil
}

// stmtIndex 返回 s 在 owner 的语句列表里的下标，不在其中返回 -1。
func stmtIndex(owner, s ast.Node) int {
	for i, x := range stmtList(owner) {
		if x == s {
			return i
		}
	}
	return -1
}

// matchesAny 报告 call 是否匹配 matchers 之一。
func matchesAny(call *ast.CallExpr, matchers []gateMatcher) bool {
	fun := types.ExprString(call.Fun)
	for _, m := range matchers {
		if fun != m.fun {
			continue
		}
		if m.arg0 == "" {
			return true
		}
		if len(call.Args) > 0 {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil && s == m.arg0 {
					return true
				}
			}
		}
	}
	return false
}

// fileImports 返回文件里 import 的名字 → 路径（未起别名的取路径末段）。
func fileImports(f *ast.File) map[string]string {
	m := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		m[name] = p
	}
	return m
}

// gateIndex 按包目录缓存门禁名（见文件头「门禁调用」），本模块的 import 路径按 root 映射到目录。
type gateIndex struct {
	root  string
	cache map[string]map[string]bool
}

// gates 返回 dir 这个包里的门禁名：名字以 gate / Gate 开头、函数体里调用了门禁的包级函数，与初始化表达式是门禁
// （或体内调用了门禁的函数字面量）的包级变量。只看非测试文件；名字相互引用的按不动点求。
func (g *gateIndex) gates(dir string) map[string]bool {
	if m, ok := g.cache[dir]; ok {
		return m
	}
	m := map[string]bool{}
	g.cache[dir] = m // 先占位：import 不成环，占位只防意外的重入
	type candidate struct {
		node    ast.Node // 函数体，或变量的初始化表达式
		imports map[string]string
	}
	cands := map[string]candidate{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			continue
		}
		imports := fileImports(f)
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Body != nil && isGateName(d.Name.Name) {
					cands[d.Name.Name] = candidate{d.Body, imports}
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for i, v := range vs.Values {
							if i < len(vs.Names) && isGateName(vs.Names[i].Name) {
								cands[vs.Names[i].Name] = candidate{v, imports}
							}
						}
					}
				}
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for name, c := range cands {
			if m[name] {
				continue
			}
			var hit bool
			if body, ok := c.node.(*ast.BlockStmt); ok {
				hit = g.callsGate(body, c.imports, m)
			} else {
				hit = g.isGateRef(c.node.(ast.Expr), c.imports, m)
			}
			if hit {
				m[name], changed = true, true
			}
		}
	}
	return m
}

// callsGate 报告 n 里有没有对门禁的调用。
func (g *gateIndex) callsGate(n ast.Node, imports map[string]string, gates map[string]bool) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && g.isGateRef(call.Fun, imports, gates) {
			found = true
		}
		return !found
	})
	return found
}

// isGateRef 报告表达式 e 是否指向门禁：launch.Gate、同包的门禁名、经 import 引用的本模块另一个包里的门禁名，或体内
// 调用了门禁的函数字面量（包级变量的初始化表达式）。
func (g *gateIndex) isGateRef(e ast.Expr, imports map[string]string, gates map[string]bool) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return gates[e.Name]
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		if !ok {
			return false
		}
		path, ok := imports[x.Name]
		if !ok || !strings.HasPrefix(path, storeGateModule) {
			return false
		}
		if path == storeGateModule+"player/launch" && e.Sel.Name == "Gate" {
			return true
		}
		return isGateName(e.Sel.Name) && g.gates(filepath.Join(g.root, filepath.FromSlash(strings.TrimPrefix(path, storeGateModule))))[e.Sel.Name]
	case *ast.FuncLit:
		return g.callsGate(e.Body, imports, gates)
	}
	return false
}

// isGateName 报告 name 是否以 gate / Gate 开头。
func isGateName(name string) bool {
	return strings.HasPrefix(name, "gate") || strings.HasPrefix(name, "Gate")
}
