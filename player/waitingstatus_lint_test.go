package player

// 本文件只守一件事：播放器子包里的非歌名状态一律由 player.ProcessStatus / NoSongStatus 合成、经
// EmitStatusIfChanged 发出（issue #119、#231）。它不测业务逻辑，扫的是 player/ 下各子包的源码。
//
// # 它守的是什么
//
// 没在读歌的播放器，外层循环每轮都要声明一次当前的状态。经 Emit 直接发，订阅者每 2～3 秒收到一条
// 同样的状态，还会每次刷新该通道的最后事件时刻，使无活跃自动隐藏（<player>-idle-hide）永远不触发
// （#119）；EmitStatusIfChanged 在内容没变时不发。状态的层次（进程没在运行 / 在运行而读不了 / 连上而
// 没有歌，设计 doc/design/converge-player-behavior-231-design.md §3.2）由 status.go 统一合成，各家自拼
// 的字面量曾把「已退出」与「开着但不支持」都报成 standby（#231）。
//
// # 规则
//
//  1. 子包的非测试文件里不得出现 Status 为 waiting_process、standby、waiting_song、error、loading 的
//     复合字面量（playing、paused 不在此列）。
//  2. 对 ProcessStatus、NoSongStatus 的每一处调用，要么是 EmitStatusIfChanged 的直接实参，要么是同包
//     某个函数的 return 表达式、且该函数在包内的每一处引用都是 EmitStatusIfChanged 直接实参里的调用。
//  3. pendingStatusPackages 里尚未改写的包只查 #119 的原规则：waiting_process 字面量只在
//     EmitStatusIfChanged 的实参里。
//  4. 豁免包若已没有规则 1、2 的违规，即报失败，提醒删掉豁免；认出发出点的包合计少于 8 个即报门禁失效。
//
// # 为什么用 AST 门禁
//
// 各播放器的 Start 要真进程才能跑到这些分支，大多没有离线夹具。
//
// # 它不守什么
//
// 只认复合字面量里的 Status 键：先存进变量的取值（Status: s）、对字段赋值（info.Status = "standby"）查不出。
// 函数按名字认、不做类型解析：辅助函数只在同一目录的非测试文件里找，同名的别的函数或变量的引用一并计入
// （宁可多报）；辅助函数套辅助函数按规则 2 报出。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// pendingStatusPackages 是尚未改用 ProcessStatus / NoSongStatus 的播放器包（目录相对 player/ 的路径）→ 何时移除。
// 任务编号见 doc/design/converge-player-behavior-231-plan.md 第 1 节。
var pendingStatusPackages = map[string]string{
	"qqmusic": "#218 合入后的阶段二移除",
}

// synthesizedStatuses 是子包里只能经 ProcessStatus / NoSongStatus 合成的状态。
var synthesizedStatuses = map[string]bool{
	"waiting_process": true, "standby": true, "waiting_song": true, "error": true, "loading": true,
}

// statusLiteral 在 n 是 Status: "<字符串>" 的键值对时返回该字符串。
func statusLiteral(n ast.Node) (string, bool) {
	kv, ok := n.(*ast.KeyValueExpr)
	if !ok {
		return "", false
	}
	key, ok := kv.Key.(*ast.Ident)
	if !ok || key.Name != "Status" {
		return "", false
	}
	lit, ok := kv.Value.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// callName 在 n 是调用时返回被调函数或方法的名字（calleeName，positionsample_lint_test.go），否则返回空串。
func callName(n ast.Node) string {
	if call, ok := n.(*ast.CallExpr); ok {
		return calleeName(call.Fun)
	}
	return ""
}

// statusScan 是一个包的扫描结果。
type statusScan struct {
	legacy []string // #119 原规则的违规：waiting_process 字面量不在 EmitStatusIfChanged 的实参里
	strict []string // 规则 1、2 的违规
	gates  int      // 认出的发出点：EmitStatusIfChanged 实参里的 waiting_process 字面量与合规的合成调用
}

// scanStatusPackage 扫一个包（同一目录下的文件，键为文件名）；测试文件不扫。
func scanStatusPackage(fset *token.FileSet, files map[string]*ast.File) statusScan {
	names := make([]string, 0, len(files))
	for name := range files {
		if !strings.HasSuffix(name, "_test.go") {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	parents := map[ast.Node]ast.Node{}
	for _, name := range names {
		var stack []ast.Node
		ast.Inspect(files[name], func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if len(stack) > 0 {
				parents[n] = stack[len(stack)-1]
			}
			stack = append(stack, n)
			return true
		})
	}
	// gateArg 报告 n 是否为某个 EmitStatusIfChanged 调用的直接实参。
	gateArg := func(n ast.Node) bool {
		c, ok := parents[n].(*ast.CallExpr)
		return ok && callName(c) == "EmitStatusIfChanged" && c.Fun != n
	}
	// underGate 报告 n 是否在某个 EmitStatusIfChanged 调用的实参里（任意深度）。
	underGate := func(n ast.Node) bool {
		for ; parents[n] != nil; n = parents[n] {
			if gateArg(n) {
				return true
			}
		}
		return false
	}
	// returnedBy 在 call 是某个顶层函数或方法的 return 表达式时返回该函数名（闭包里的不算）。
	returnedBy := func(call ast.Node) string {
		if _, ok := parents[call].(*ast.ReturnStmt); !ok {
			return ""
		}
		for p := parents[call]; p != nil; p = parents[p] {
			switch fn := p.(type) {
			case *ast.FuncLit:
				return ""
			case *ast.FuncDecl:
				return fn.Name.Name
			}
		}
		return ""
	}

	var sc statusScan
	pos := func(n ast.Node) string { return fset.Position(n.Pos()).String() }
	helpers := map[string]bool{}
	for _, name := range names {
		ast.Inspect(files[name], func(n ast.Node) bool {
			if s, ok := statusLiteral(n); ok && synthesizedStatuses[s] {
				sc.strict = append(sc.strict, pos(n)+"：状态字面量 Status: "+strconv.Quote(s))
				if s == "waiting_process" {
					if underGate(n) {
						sc.gates++
					} else {
						sc.legacy = append(sc.legacy, pos(n)+"：waiting_process 没有经 EmitStatusIfChanged 发出")
					}
				}
			}
			f := callName(n)
			if f != "ProcessStatus" && f != "NoSongStatus" {
				return true
			}
			if gateArg(n) {
				sc.gates++
			} else if h := returnedBy(n); h != "" {
				helpers[h] = true
			} else {
				sc.strict = append(sc.strict, pos(n)+"："+f+" 的结果没有直接交给 EmitStatusIfChanged")
			}
			return true
		})
	}
	// 辅助函数的每一处引用都须是 EmitStatusIfChanged 直接实参里的调用。
	for _, name := range names {
		ast.Inspect(files[name], func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || !helpers[id.Name] {
				return true
			}
			if d, ok := parents[id].(*ast.FuncDecl); ok && d.Name == id {
				return true // 声明本身
			}
			ref := ast.Node(id)
			if sel, ok := parents[id].(*ast.SelectorExpr); ok && sel.Sel == id {
				ref = sel
			}
			if c, ok := parents[ref].(*ast.CallExpr); ok && c.Fun == ref && gateArg(c) {
				sc.gates++
			} else {
				sc.strict = append(sc.strict, pos(id)+"：辅助函数 "+id.Name+" 的结果没有直接交给 EmitStatusIfChanged")
			}
			return true
		})
	}
	return sc
}

// statusProblems 按包是否豁免给出要报的问题。豁免包只查原规则；已没有待改写之处的豁免包报失败（规则 4）。
func statusProblems(pkg string, sc statusScan, exempt bool) []string {
	if !exempt {
		return sc.strict
	}
	if len(sc.strict) == 0 {
		return []string{pkg + "：已没有待改写的状态，豁免已不需要，从 pendingStatusPackages 删掉"}
	}
	return sc.legacy
}

// TestNonSongStatusGoesThroughSynthesis 扫 player/ 下各子包（不含 player 根包与 testdata）。
//
// 变异自证（均实测变红）：把 qqmusic、sodamusic、spotify、wesing 各一处与 applemusic 两处的
// waiting_process 由 EmitStatusIfChanged 改回 Emit，逐处各红（#119 原规则）；从 pendingStatusPackages
// 删掉 kuwo（规则 1）；往 pendingStatusPackages 加一个不存在的包（规则 4）；往 pendingStatusPackages 加回已改写的
// kugou（规则 4）；kugou 连上而没有歌的 NoSongStatus 改回 waiting_song 字面量（规则 1）；往 pendingStatusPackages 加回
// 已改写的 cloudmusic（规则 4）；cloudmusic 连上而没有歌的 NoSongStatus 改回 waiting_song 字面量（规则 1）；cloudmusic
// 的 v2 状态改回 Emit 发出（规则 2）；往 pendingStatusPackages 加回已改写的 spotify 或 sodamusic（规则 4）；spotify 外层循环的
// 状态改回 Emit 发出（规则 2）；往 pendingStatusPackages 加回已改写的 applemusic、wesing 或 kuwo（规则 4）。
func TestNonSongStatusGoesThroughSynthesis(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	base := filepath.Join(root, "player")
	fset := token.NewFileSet()
	pkgs := map[string]map[string]*ast.File{}
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		dir := filepath.Dir(path)
		if dir == base || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, dir)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if pkgs[key] == nil {
			pkgs[key] = map[string]*ast.File{}
		}
		pkgs[key][d.Name()] = f
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 player/ 失败: %v", err)
	}
	for name := range pendingStatusPackages {
		if pkgs[name] == nil {
			pkgs[name] = map[string]*ast.File{} // 豁免了不存在的包：按无违规处理，由规则 4 报出
		}
	}

	names := make([]string, 0, len(pkgs))
	for name := range pkgs {
		names = append(names, name)
	}
	sort.Strings(names)
	var problems, emitting []string
	for _, name := range names {
		_, exempt := pendingStatusPackages[name]
		sc := scanStatusPackage(fset, pkgs[name])
		problems = append(problems, statusProblems(name, sc, exempt)...)
		if sc.gates > 0 {
			emitting = append(emitting, name)
		}
	}
	if len(problems) > 0 {
		t.Errorf("播放器子包的非歌名状态没有按约定合成与发出：\n  %s\n\n"+
			"修法：p.EmitStatusIfChanged(player.ProcessStatus(running, seen, details, blocked))，没有歌时用 "+
			"player.NoSongStatus；经辅助函数返回的，该函数只作 EmitStatusIfChanged 的实参。见 player/status.go。",
			strings.Join(problems, "\n  "))
	}
	// 零命中自检：认出发出点的包少于 8 个，说明门禁的判据失效了（方法改名了，或发出点换了写法）。
	if len(emitting) < 8 {
		t.Errorf("认出非歌名状态发出点的包只有 %d 个（%v），应至少 8 个——门禁的判据失效了", len(emitting), emitting)
	}
}

func scanStatusSrcs(t *testing.T, srcs map[string]string) statusScan {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for name, src := range srcs {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("解析片段 %s 失败: %v\n%s", name, err, src)
		}
		files[name] = f
	}
	return scanStatusPackage(fset, files)
}

// TestWaitingStatusLintCatchesKnownHoles 钉死门禁自身认得出目标形状（教训同 cloudmusicanchor_lint_test.go）。
//
// 变异自证（均实测变红）：
//   - 规则 2 不查 ProcessStatus / NoSongStatus 的调用位置：「先赋给变量」「直接 Emit」「套在别的调用里」
//     「经 EmitForGen」「闭包里返回」五例红；
//   - 规则 2 不查辅助函数的引用（返回合成状态的函数一律放行）：辅助函数的六个反例红；
//   - 去掉规则 4 的自检（豁免包一律只查原规则）：「豁免包已无违规」「豁免了不存在的包」两例红；
//   - 规则 1 只认 waiting_process：standby、waiting_song、error、loading 与「构造函数返回字面量」五例红；
//   - 测试文件也扫：「测试文件里直接调用辅助函数」一例红；
//   - 闭包里的 return 也算辅助函数：「闭包里返回」一例红。
func TestWaitingStatusLintCatchesKnownHoles(t *testing.T) {
	const head = "package p\nimport \"Metabox-Nexus-PlayerCap/player\"\n"
	wrap := func(body string) map[string]string {
		return map[string]string{"p.go": head + "func (p *P) Start() {\n" + body + "}\n"}
	}
	const helper = "func (p *P) st() *player.StatusInfo { return player.NoSongStatus(player.ReasonNoSong, \"x\") }\n"
	const ps = "player.ProcessStatus(false, false, player.ProcessDetails{}, player.Blocked{})"

	shouldCatch := map[string]map[string]string{
		"直接 Emit 合成的状态":       wrap("	p.Emit(player.EventStatusUpdate, " + ps + ")\n"),
		"先赋给变量再交给门":           wrap("	info := " + ps + "\n	p.EmitStatusIfChanged(info)\n"),
		"套在别的调用里":             wrap("	p.EmitStatusIfChanged(wrap(" + ps + "))\n"),
		"经 EmitForGen 之类别的出口": wrap("	p.EmitForGen(gen, player.EventStatusUpdate, " + ps + ")\n"),
		"闭包里返回": {"p.go": head +
			"func (p *P) Start() { f := func() *player.StatusInfo { return " + ps + " }\n	p.EmitStatusIfChanged(f()) }\n"},
		"辅助函数有一处经 Emit": {"p.go": head + helper +
			"func (p *P) Start() {\n	p.EmitStatusIfChanged(p.st())\n	p.Emit(player.EventStatusUpdate, p.st())\n}\n"},
		"辅助函数在另一个文件里经 Emit": {
			"a.go": head + helper + "func (p *P) Start() { p.EmitStatusIfChanged(p.st()) }\n",
			"b.go": head + "func (p *P) Stop() { p.Emit(player.EventStatusUpdate, p.st()) }\n",
		},
		"辅助函数的结果先赋给变量": {"p.go": head + helper + "func (p *P) Start() { s := p.st(); p.EmitStatusIfChanged(s) }\n"},
		"辅助函数作为函数值传走":  {"p.go": head + helper + "func (p *P) Start() { later(p.st) }\n"},
		"辅助函数套辅助函数": {"p.go": head + helper +
			"func (p *P) outer() *player.StatusInfo { return p.st() }\n" +
			"func (p *P) Start() { p.EmitStatusIfChanged(p.outer()) }\n"},
		"普通函数形式的辅助函数经 Emit": {"p.go": head +
			"func statusFor(err error) *player.StatusInfo { return " + ps + " }\n" +
			"func (p *P) Start() { p.Emit(player.EventStatusUpdate, statusFor(nil)) }\n"},
		"waiting_process 字面量即使经门": wrap("	p.EmitStatusIfChanged(&player.StatusInfo{Status: \"waiting_process\"})\n"),
		"standby 字面量即使经门":         wrap("	p.EmitStatusIfChanged(&player.StatusInfo{Status: \"standby\"})\n"),
		"waiting_song 字面量经 Emit":  wrap("	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: \"waiting_song\"})\n"),
		"error 字面量":               wrap("	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: \"error\"})\n"),
		"loading 字面量":             wrap("	x := player.StatusInfo{Status: \"loading\"}\n	_ = x\n"),
		"构造函数返回字面量（sodamusic 的旧写法）": {"p.go": head +
			"func noSongStatus() *player.StatusInfo { return &player.StatusInfo{Status: \"waiting_song\"} }\n" +
			"func (p *P) Start() { p.EmitStatusIfChanged(noSongStatus()) }\n"},
	}
	for name, srcs := range shouldCatch {
		t.Run("报/"+name, func(t *testing.T) {
			sc := scanStatusSrcs(t, srcs)
			if got := statusProblems("p", sc, false); len(got) == 0 {
				t.Errorf("非豁免包里门禁漏报了这种形状——它正是要堵的：\n%v", srcs)
			}
		})
	}

	shouldPass := map[string]map[string]string{
		"直接作 EmitStatusIfChanged 的实参": wrap("	p.EmitStatusIfChanged(" + ps + ")\n" +
			"	p.EmitStatusIfChanged(player.NoSongStatus(player.ReasonWindowNotOpen, \"x\"))\n"),
		"辅助函数只作 EmitStatusIfChanged 的实参": {
			"a.go": head + helper + "func (p *P) Start() { p.EmitStatusIfChanged(p.st()) }\n",
			"b.go": head + "func (p *P) Stop() { if p.EmitStatusIfChanged(p.st()) { return } }\n",
		},
		"普通函数形式的辅助函数只作门的实参": {"p.go": head +
			"func statusFor(err error, seen bool) *player.StatusInfo {\n" +
			"	if err == nil { return player.NoSongStatus(player.ReasonNoSong, \"x\") }\n" +
			"	return player.ProcessStatus(false, seen, player.ProcessDetails{}, player.Blocked{})\n}\n" +
			"func (p *P) Start() { p.EmitStatusIfChanged(statusFor(nil, true)) }\n"},
		"测试文件里直接调用辅助函数": {
			"a.go":      head + helper + "func (p *P) Start() { p.EmitStatusIfChanged(p.st()) }\n",
			"a_test.go": head + "func TestSt() { _ = (&P{}).st(); _ = player.StatusInfo{Status: \"standby\"} }\n",
		},
		"歌名状态照常 Emit": wrap("	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: \"playing\", Detail: t})\n" +
			"	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: \"paused\", Detail: t})\n"),
		"Status 不是字面量":        wrap("	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: s})\n"),
		"不在 StatusInfo 里的字符串": wrap("	if read() == \"standby\" { return }\n	x := \"waiting_process\"\n	_ = x\n"),
	}
	for name, srcs := range shouldPass {
		t.Run("不报/"+name, func(t *testing.T) {
			sc := scanStatusSrcs(t, srcs)
			if got := statusProblems("p", sc, false); len(got) > 0 {
				t.Errorf("门禁误报：%v\n%v", got, srcs)
			}
		})
	}

	// 豁免包：只查原规则；没有待改写之处时自检报失败。
	exemptCases := []struct {
		name  string
		srcs  map[string]string
		catch bool
	}{
		{"豁免包写 standby 字面量经 Emit 不报", wrap("	p.EmitStatusIfChanged(&player.StatusInfo{Status: \"waiting_process\"})\n" +
			"	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: \"standby\"})\n"), false},
		{"豁免包写 waiting_process 字面量经 Emit 仍报", wrap("	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: \"waiting_process\"})\n"), true},
		{"豁免包写 waiting_process 字面量先存变量仍报", wrap("	info := &player.StatusInfo{Status: \"waiting_process\"}\n	p.EmitStatusIfChanged(info)\n"), true},
		{"豁免包已无违规（全部改用合成）报失败", wrap("	p.EmitStatusIfChanged(" + ps + ")\n"), true},
		{"豁免了不存在的包（没有文件）报失败", map[string]string{}, true},
	}
	for _, c := range exemptCases {
		t.Run("豁免/"+c.name, func(t *testing.T) {
			got := statusProblems("p", scanStatusSrcs(t, c.srcs), true)
			if c.catch && len(got) == 0 {
				t.Errorf("豁免包应报而未报：\n%v", c.srcs)
			}
			if !c.catch && len(got) > 0 {
				t.Errorf("豁免包误报：%v\n%v", got, c.srcs)
			}
		})
	}

	// 零命中自检用的计数：经 EmitStatusIfChanged 发出的才计。
	for _, c := range []struct {
		name string
		srcs map[string]string
		want int
	}{
		{"waiting_process 字面量经门计一次", wrap("	p.EmitStatusIfChanged(&player.StatusInfo{Status: \"waiting_process\"})\n" +
			"	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: \"standby\"})\n"), 1},
		{"合成调用经门计一次", wrap("	p.EmitStatusIfChanged(" + ps + ")\n	p.Emit(player.EventStatusUpdate, " + ps + ")\n"), 1},
		{"辅助函数每处合规调用各计一次", map[string]string{"p.go": head + helper +
			"func (p *P) Start() { p.EmitStatusIfChanged(p.st()); p.EmitStatusIfChanged(p.st()) }\n"}, 2},
		{"没有经门的发出点计零", wrap("	p.Emit(player.EventStatusUpdate, " + ps + ")\n"), 0},
	} {
		if got := scanStatusSrcs(t, c.srcs).gates; got != c.want {
			t.Errorf("%s：发出点计数 %d，期望 %d", c.name, got, c.want)
		}
	}
}
