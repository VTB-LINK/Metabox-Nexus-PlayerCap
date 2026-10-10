package config

// 本文件只测一件事：每个配置键在五处都有 —— mergeYAML 的分支、Load 注册的命令行 flag、flag.Visit 的
// 覆盖分支、内置模板 defaultConfigContent、README.md 的命令行参数表（issue #65）。
//
// # 为什么要门禁
//
// 五处都是手写的，互不派生，漏一处不报错：缺 mergeYAML 分支，config.yml 里写了不生效；缺 flag.Visit
// 分支，命令行传了不生效；缺模板行或 README 表行，用户不知道有这个键。
//
// # 键从哪里来
//
// 没有手写的键清单。以 config.go 源码里 mergeYAML 读取的键为准，其余四处与它比对，缺与多都算错。
// 每播放器键在源码里写作 name+"-后缀"，记作 "<player>-后缀"，与 README 表的写法相同。模板里的专属键
// 写的是真实播放器名，按 ../player 下各包传给 RegisterPlayer 的名字展开后再比（本测试二进制不 import
// 播放器包，注册表里没有它们）；README 的 <player> 取值列表也与这份名单比对。
//
// # 它守什么、不守什么
//
// 只查「有没有」，每播放器键另查「是否覆盖全部注册的播放器」。分支里赋没赋值、赋给哪个字段、模板与
// README 写的默认值对不对，都不查（模板全局键的取值见 templatedefaults_test）。
//
// 覆盖全部播放器这一条：模板与 README 按注册名单逐家比对；源码三处要求键的前缀标识符是包住它的
// for _, 标识符 := range registeredPlayers 的循环变量，否则不当作每播放器键，报认不出。不查的话，flag
// 只给一部分播放器注册而其余几处补齐时本测试仍绿，而用户传了其余播放器的这个参数，flag.Parse 报未定义
// 并退出。这一条只按名字与位置判断，不做类型检查：循环体内给循环变量重新赋值或重新声明、函数内遮蔽
// registeredPlayers，查不出。
//
// 只认以下形状：mergeYAML 里对参数 m 的下标读取，flag 包的注册函数，flag.Visit 回调里 switch f.Name
// 的 case 与 f.Name == 键 的比较；键只能是字符串字面量或 循环变量+"-后缀"。键写成别的表达式会报认不出；
// 经辅助函数读取 m、经 FlagSet 注册的扫不到，表现为某一处多出或缺少该键。两种情况都要同步改这里的扫描。
//
// 具名例外只有一个：版本键 config-version（templateOnlyKeys）。它由程序维护，不是配置项——没有命令行
// 参数，不经 mergeYAML（loadFile 在合并之前单独读它），不进 README 的参数表——只出现在模板一处，且必须
// 在模板里。--kugou-patch-helper 与 --audio-capture-helper 由 main() 在 config.Load 之前直接比对
// os.Args，不经 flag 包，不在比对范围内。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// playerKey 是每播放器键里播放器名的占位写法，与 README 参数表一致。
const playerKey = "<player>"

// templateOnlyKeys 是只出现在模板里的键（具名例外，理由见文件头）。
var templateOnlyKeys = []string{versionKey}

var (
	// 模板的键行：顶格的「键:」，或注释掉的示例「# 键:」。
	templateKeyLine = regexp.MustCompile(`(?m)^(?:# )?([a-z][a-z0-9-]*):`)
	// README 参数表的行：首列是反引号包着的 -flag。
	readmeFlagRow = regexp.MustCompile("(?m)^\\| `-([^`]+)` \\|")
	backquoted    = regexp.MustCompile("`([^`]+)`")
)

// TestConfigKeyPlaces 以 mergeYAML 认得的键为准，断言其余四处的键与它是同一组。
//
// 变异自证（隔离副本，实测）。以下各条均红：
//
//   - 全局键 audio-frame-rate，五处各删一处：mergeYAML 的分支、flag.IntVar 注册、flag.Visit 的 case、
//     模板行、README 表行。
//
//   - 每播放器键 -idle-hide，五处各删一处：mergeYAML 循环里的分支、playerCLI 字面量里的 flag.Int 注册、
//     flag.Visit 的 else if 分支、模板的 kugou-idle-hide 行、README 的 -<player>-idle-hide 行。
//
//   - 只加一处：mergeYAML 加只有它认得的键（全局 "ghost-key"、每播放器 name+"-ghost" 各一次）；Load 只
//     注册一个 flag；flag.Visit 只加一个 case；模板只加一行键；README 表只加一行。
//
//   - 播放器名单：README 的 <player> 列表删掉 applemusic；模板的 kugou-poll 拼成 kugo-poll；player/ 下
//     新增一个调用 RegisterPlayer 的包。
//
//   - 覆盖全部播放器：Load 里注册 flag 的循环、flag.Visit 回调里的循环、mergeYAML 里的循环，分别改成
//     遍历 registeredPlayers[:1]；playerCLI 字面量里 -idle-hide 的前缀换成循环外声明的固定变量。
//
//   - 扫描自身：mergeYAML 经变量读键（k := "addr"，再 m[k]）报认不出的键表达式；参数 m 改名，零命中
//     自检报错。
//
//   - 具名例外 config-version：从模板删掉版本键一行、模板里写两行版本键、mergeYAML 加一个读它的分支
//     （m["config-version"]），均红。
//
// 仍绿：保留 flag.Visit 的 audio-frame-rate case、删掉其中的赋值（只查有没有）；注册 flag 的循环体内
// 给循环变量重新赋值（name = registeredPlayers[0]，前缀的核对不做数据流分析）。
func TestConfigKeyPlaces(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("读取 config.go 失败: %v", err)
	}
	readme, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatalf("读取 README.md 失败: %v", err)
	}
	_, section, ok := strings.Cut(string(readme), "\n### 命令行参数\n")
	if !ok {
		t.Fatal("README.md 里找不到「### 命令行参数」一节")
	}
	section, _, _ = strings.Cut(section, "\n#") // 到下一个标题为止

	keys, flags, visited := scanKeys(t, string(src), t.Errorf)
	if len(keys) == 0 {
		t.Fatal("没有从 mergeYAML 扫出任何键：扫描认的是对参数 m 的下标读取，形状变了就同步改这里")
	}
	players := registeredPlayerNames(t, filepath.Join("..", "player"))
	t.Logf("mergeYAML 认得的键：%v；注册的播放器：%v", keys, players)

	const base = "mergeYAML"
	sameSet(t, "Load 注册的命令行 flag", base, keys, flags)
	sameSet(t, "flag.Visit 的覆盖分支", base, keys, visited)
	sameSet(t, "README.md 的命令行参数表", base, keys, submatches(readmeFlagRow, section))

	// 模板里的专属键写的是真实播放器名：把 <player> 键按注册名单展开后再比。
	var expanded []string
	for _, k := range keys {
		suffix, perPlayer := strings.CutPrefix(k, playerKey)
		if !perPlayer {
			expanded = append(expanded, k)
			continue
		}
		for _, p := range players {
			expanded = append(expanded, p+suffix)
		}
	}
	// 具名例外：只出现在模板里的键。模板里必须有，其余四处必须没有（上面三次比对已保证后者）。
	templateKeys := submatches(templateKeyLine, defaultConfigContent)
	for _, k := range templateOnlyKeys {
		n := 0
		templateKeys = slices.DeleteFunc(templateKeys, func(s string) bool {
			if s == k {
				n++
			}
			return s == k
		})
		if n != 1 {
			t.Errorf("模板 defaultConfigContent：%s 应恰好出现一次，实际 %d 次", k, n)
		}
	}
	sameSet(t, "模板 defaultConfigContent", base, expanded, templateKeys)

	// README 里 <player> 的取值列表：「`<player>` ∈ 」之后、第一个全角逗号之前的各个反引号项。
	_, list, _ := strings.Cut(section, "`"+playerKey+"` ∈ ")
	list, _, _ = strings.Cut(list, "，")
	sameSet(t, "README.md 的 <player> 取值列表", "player/ 各包的 RegisterPlayer 调用", players, submatches(backquoted, list))

	if t.Failed() {
		t.Log("新增配置键须同改的位置见 AGENTS.md §3.3 与 §5.2")
	}
}

// TestConfigKeyScanRequiresPlayerLoop 钉死扫描对每播放器键前缀的核对：只有包住它的
// for _, 变量 := range registeredPlayers 的循环变量才折叠成 <player>，别的写法报认不出、不折叠。
// 三处（mergeYAML、flag 注册、flag.Visit）各喂一遍合成源码，其余两处保持现行写法。
//
// 变异自证（隔离副本，实测）：playerLoopProblem 改为恒返回空串 → 五种反例在三处共 15 例红；去掉对
// 被遍历表达式的判断 → 字面量切片、registeredPlayers[:1]、内层重新绑定三种共 9 例红；同名变量改为
// 以最外层循环为准 → 内层重新绑定 3 例红；去掉「循环体包住标识符」的位置判断 → flag 注册与 flag.Visit
// 两处共 8 例红（合成的 Load 里另有绑定同名变量的循环，mergeYAML 一处没有）。
func TestConfigKeyScanRequiresPlayerLoop(t *testing.T) {
	const current = `for _, name := range registeredPlayers { %s }` // 现行写法；%s 处放读取键的语句
	shapes := []struct {
		name   string
		loop   string
		prefix string // 键表达式里的前缀标识符
		ok     bool
	}{
		{"遍历 registeredPlayers、前缀是循环变量", current, "name", true},
		{"遍历字面量切片", `for _, name := range []string{"wesing"} { %s }`, "name", false},
		{"遍历 registeredPlayers 的一部分", `for _, name := range registeredPlayers[:1] { %s }`, "name", false},
		{"前缀是循环外的普通变量", `name := "wesing"; %s`, "name", false},
		{"遍历 registeredPlayers、前缀不是循环变量", `p := "wesing"; for range registeredPlayers { %s }`, "p", false},
		{"内层循环重新绑定了同名变量", `for _, name := range registeredPlayers { for _, name := range []string{"wesing"} { %s } }`, "name", false},
	}
	places := []struct{ name, read string }{ // name 与扫描报错里的出处一致；read 的 %s 处放前缀
		{"mergeYAML", `_ = m[%s+"-x"]`},
		{"flag 注册", `flag.Int(%s+"-x", 0, "")`},
		{"flag.Visit", `if f.Name == %s+"-x" {}`},
	}
	for i, place := range places {
		for _, shape := range shapes {
			t.Run(place.name+"/"+shape.name, func(t *testing.T) {
				stmts := make([]any, len(places))
				for j, p := range places {
					stmts[j] = fmt.Sprintf(current, fmt.Sprintf(p.read, "name"))
				}
				stmts[i] = fmt.Sprintf(shape.loop, fmt.Sprintf(place.read, shape.prefix))
				src := fmt.Sprintf("package config\n"+
					"func mergeYAML(dst *Config, m map[string]interface{}) { %s }\n"+
					"func Load() {\n%s\nflag.Visit(func(f *flag.Flag) { %s })\n}\n", stmts...)

				var problems []string
				got := make([][]string, len(places))
				got[0], got[1], got[2] = scanKeys(t, src, func(format string, args ...any) {
					problems = append(problems, fmt.Sprintf(format, args...))
				})

				for j, p := range places {
					want := []string{playerKey + "-x"}
					if j == i && !shape.ok {
						want = nil
					}
					if !slices.Equal(got[j], want) {
						t.Errorf("%s 扫出的键 = %v，期望 %v", p.name, got[j], want)
					}
				}
				if shape.ok != (len(problems) == 0) {
					t.Errorf("报出的问题 = %q，期望有问题 = %v", problems, !shape.ok)
				}
				for _, p := range problems {
					if !strings.HasPrefix(p, place.name+" 里有认不出的键表达式") {
						t.Errorf("问题应报在 %s：%s", place.name, p)
					}
				}
			})
		}
	}
}

// sameSet 断言 got 与 want 是同一组名字，逐个报出缺的与多的；base 是 want 的出处。
func sameSet(t *testing.T, place, base string, want, got []string) {
	t.Helper()
	for _, k := range want {
		if !slices.Contains(got, k) {
			t.Errorf("%s：缺 %s（见于 %s）", place, k, base)
		}
	}
	for _, k := range got {
		if !slices.Contains(want, k) {
			t.Errorf("%s：多出 %s（不见于 %s）", place, k, base)
		}
	}
}

// submatches 返回 re 在 s 里每处匹配的第一个捕获组。
func submatches(re *regexp.Regexp, s string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// scanKeys 扫 config.go 的源码 src，按源码顺序返回三处的键：mergeYAML 函数体里对参数 m 做下标读取的键、
// Load 函数体里注册的 flag 名、flag.Visit 回调里按名字分支的 flag 名。认不出的键表达式经 errf 报出：
// 主测试传 t.Errorf，扫描自测传收集器。
func scanKeys(t *testing.T, src string, errf func(format string, args ...any)) (merge, flags, visited []string) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "config.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("解析 config.go 失败: %v", err)
	}

	body := funcBody(t, file, "mergeYAML")
	read := keySink{errf: errf, place: "mergeYAML", loops: rangeLoops(body)}
	ast.Inspect(body, func(n ast.Node) bool {
		if idx, ok := n.(*ast.IndexExpr); ok && isIdentNamed(idx.X, "m") {
			read.add(idx.Index)
		}
		return true
	})

	body = funcBody(t, file, "Load")
	reg := keySink{errf: errf, place: "flag 注册", loops: rangeLoops(body)}
	visit := keySink{errf: errf, place: "flag.Visit", loops: reg.loops}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isIdentNamed(sel.X, "flag") {
			return true
		}
		switch fn := sel.Sel.Name; fn {
		case "Visit":
			visitBranches(&visit, call.Args[0])
		case "Bool", "Int", "Int64", "Uint", "Uint64", "String", "Float64", "Duration", "Func", "BoolFunc":
			reg.add(call.Args[0])
		default:
			if strings.HasSuffix(fn, "Var") { // XxxVar(&v, 名字, ...)
				reg.add(call.Args[1])
			}
		}
		return true
	})
	return read.keys, reg.keys, visit.keys
}

// visitBranches 收集 flag.Visit 回调里按 flag 名分支的键：switch f.Name 的各 case，与 f.Name == 键 的比较。
func visitBranches(keys *keySink, callback ast.Node) {
	isFlagName := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "Name"
	}
	ast.Inspect(callback, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SwitchStmt:
			if !isFlagName(n.Tag) {
				return true
			}
			for _, c := range n.Body.List {
				for _, e := range c.(*ast.CaseClause).List {
					keys.add(e)
				}
			}
		case *ast.BinaryExpr:
			if n.Op == token.EQL && isFlagName(n.X) {
				keys.add(n.Y)
			}
		}
		return true
	})
}

// registeredPlayerNames 扫 dir 下各目录的非测试源码，返回传给 RegisterPlayer 的播放器名（已排序）。
// 实参须是字符串字面量或同目录里声明的字符串常量，其他形状报错。
func registeredPlayerNames(t *testing.T, dir string) []string {
	type pkg struct {
		consts map[string]string
		args   []ast.Expr
	}
	pkgs := map[string]*pkg{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		p := pkgs[filepath.Dir(path)]
		if p == nil {
			p = &pkg{consts: map[string]string{}}
			pkgs[filepath.Dir(path)] = p
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ValueSpec:
				for i, name := range n.Names {
					if i < len(n.Values) {
						if s, ok := stringLit(n.Values[i]); ok {
							p.consts[name.Name] = s
						}
					}
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RegisterPlayer" {
					p.args = append(p.args, n.Args[0])
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 %s 失败: %v", dir, err)
	}

	var names []string
	for _, p := range pkgs {
		for _, arg := range p.args {
			name, ok := stringLit(arg)
			if id, isIdent := arg.(*ast.Ident); isIdent {
				name, ok = p.consts[id.Name]
			}
			if !ok {
				t.Errorf("RegisterPlayer 的实参 %s 认不出播放器名", types.ExprString(arg))
				continue
			}
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// funcBody 返回 file 里名为 name 的顶层函数的函数体；找不到即失败，免得函数改名后门禁空转。
func funcBody(t *testing.T, file *ast.File, name string) *ast.BlockStmt {
	t.Helper()
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name && fn.Body != nil {
			return fn.Body
		}
	}
	t.Fatalf("config.go 里找不到函数 %s", name)
	return nil
}

// keySink 收一处的键。
type keySink struct {
	errf  func(format string, args ...any)
	place string           // 报错里的出处
	loops []*ast.RangeStmt // 所在函数体里的全部 range 循环
	keys  []string
}

// add 记下键表达式 e 的键名；认不出形状即报错，不静默跳过。
// 字符串字面量即键名。标识符 + "-后缀" 是每播放器键，记作 "<player>-后缀"，前提是标识符通过
// playerLoopProblem 的核对。
func (s *keySink) add(e ast.Expr) {
	key, ok := stringLit(e)
	why := `键须是字符串字面量，或 range registeredPlayers 的循环变量 + "-后缀"`
	if b, isBinary := e.(*ast.BinaryExpr); isBinary && b.Op == token.ADD {
		id, isIdent := b.X.(*ast.Ident)
		suffix, isLit := stringLit(b.Y)
		if isIdent && isLit {
			key, why = playerKey+suffix, playerLoopProblem(s.loops, id)
			ok = why == ""
		}
	}
	if !ok {
		s.errf("%s 里有认不出的键表达式 %s：%s", s.place, types.ExprString(e), why)
		return
	}
	s.keys = append(s.keys, key)
}

// playerLoopProblem 核对每播放器键的前缀标识符 id：它须是包住它的 for _, id := range registeredPlayers
// 的循环变量，这样键才覆盖全部注册的播放器。符合返回空串，否则返回原因。同名变量被内层循环重新绑定时
// 以最内层为准。只按名字与位置判断，不做类型检查。
func playerLoopProblem(loops []*ast.RangeStmt, id *ast.Ident) string {
	var loop *ast.RangeStmt
	for _, r := range loops {
		binds := isIdentNamed(r.Key, id.Name) || isIdentNamed(r.Value, id.Name)
		if binds && r.Body.Pos() <= id.Pos() && id.End() <= r.Body.End() {
			loop = r // loops 先外后内，留下的是最内层
		}
	}
	if loop == nil {
		return id.Name + " 不是外层 range 循环的循环变量"
	}
	if !isIdentNamed(loop.X, "registeredPlayers") {
		return id.Name + " 所在的循环遍历的是 " + types.ExprString(loop.X) +
			"，不是 registeredPlayers 本身，未必覆盖全部注册的播放器"
	}
	return ""
}

// rangeLoops 返回 body 里的全部 range 循环，按源码顺序（嵌套时先外后内）。
func rangeLoops(body ast.Node) []*ast.RangeStmt {
	var loops []*ast.RangeStmt
	ast.Inspect(body, func(n ast.Node) bool {
		if r, ok := n.(*ast.RangeStmt); ok {
			loops = append(loops, r)
		}
		return true
	})
	return loops
}

// isIdentNamed 报告 e 是否为名叫 name 的标识符。
func isIdentNamed(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// stringLit 返回字符串字面量的值。
func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
