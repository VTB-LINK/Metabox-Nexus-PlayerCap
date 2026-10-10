// Command i18nrekey 把后端 i18n 的文案由「中文原文作 key」改写为「英文原文作 key」，并核对改写结果。
//
// 为 issue #61 而写。改写之后仍要用它处理按旧约定写成的代码（别的分支合入后带进来的中文
// 字面量与「中文 → 英文」条目），所以可以重复运行：已是英文的调用点与已是「英文 → 中文」的
// 条目不动。
//
// # 它认什么
//
// 调用点与 i18n/catalogcoverage_test.go 的门禁同一口径，只看调用形状，不做类型解析：
//
//   - 方法名为 Info / Success / Warn / Error / Detail 的调用的第一个实参（logger 的 format）；
//   - i18n.T 与 i18n.Errorf 的第一个实参（i18n 包内为不带包名的 T 与 Errorf）。
//
// 扫 -root 下的 .go 文件，跳过以 . 开头的目录与 build、tools、testdata。_test.go 里只认
// i18n.T 与 i18n.Errorf：测试里的 t.Error 与 logger 的 Error 同名，按方法名认会误改。
//
// 译表取自 i18n 目录下非测试文件里的包级 map[string]string 字面量。条目的方向按键判断：
// 键含汉字的是旧方向（中文 → 英文），其余是新方向（英文 → 中文）。
//
// # 它改什么（-write）
//
//   - 调用点上含汉字、且译表里恰有一句英文与之对应的字面量，换成那句英文。字面量相加的整段
//     换成一个字面量。对应关系从两个方向的条目里找（旧方向的键 → 值，新方向的值 → 键），所以
//     译表对调之后新写的调用复用了表里已有的中文、没有随之新增条目，也改得了。同一句中文对应
//     不止一句英文时不猜：这处调用不改，列出候选由人定，别的调用点照常改写。
//   - 译表里旧方向的条目，键与值对调，条目的位置与注释不动。它的中英对已另有一条时（新方向的
//     那条，不论先后、在不在同一张表；或同一对旧方向条目的前一条）不对调而是去掉：两条表达的
//     是同一对译文，对调会在一张表里写出两个相同的键，gofmt 不管，编译不过。合并冲突两边都留
//     就是这种输入。
//   - 改过的文件按 gofmt 重排。
//
// 以下情形 -write 拒绝改写，须先人工处理：英文相同而中文不同的条目（对调后撞键）；同一个键
// 出现不止一次且值不同；对调、去重之后一张表里仍有重复的键；英文里含汉字的条目。
// 先存进变量再传给 i18n.T 的文案不是直接实参，工具不改，列在核对结果里由人改。
//
// # 它核对什么（不带 -write）
//
// 只读。以下各项任何一项非空即以 1 退出：待改写的调用点、待对调的条目、待去掉的条目、撞键、
// 同一个键的取值冲突、一张表里重复的键、英文里含汉字的条目、对应不止一句英文的中文字面量、
// 出货代码里含汉字却没有英文的字面量、出货代码里不在译表里的字面量（去掉占位符后不含字母与
// 数字的可以不进译表，见 exempt）。
//
// 另列几项供人核对，不影响退出码：按豁免放行的字面量；实参不是字面量的调用点；与译表里的
// 中文逐字相同、却不是直接实参的字面量（先存进变量再传的文案会出现在这里，只上报遥测、
// 与某条日志同文的字面量也会）；没有直接实参引用的条目。
//
// # 与改写前比对（-baseline <目录>）
//
// 把该目录当作改写前的源码树，与 -root 逐文件、按字面量出现的先后配对：
//
//   - 直接实参：中文环境与英文环境下显示的文字都须与改写前相同；
//   - 其余字面量：要么没变，要么是按改写前的译表由中文换成了对应的英文，且改写后的译表能把它
//     译回原来的中文；
//   - 译表：「中文—英文」对须相同，多出键值相同的条目不算差异。
//
// 有差异即以 1 退出。经变量转手的文案由人改，改得对不对靠第二条核对。
//
// 用法：
//
//	go run ./tools/i18nrekey                      # 核对
//	go run ./tools/i18nrekey -write               # 改写
//	go run ./tools/i18nrekey -baseline <旧源码树>   # 与改写前逐字面量比对
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// i18nImportPath 是 i18n 包的 import 路径，用来认出各文件里它的本地名。
const i18nImportPath = "Metabox-Nexus-PlayerCap/i18n"

// loggerMethods 是 logger.Logger 的五个输出方法，第一个参数是 format。
var loggerMethods = map[string]bool{"Info": true, "Success": true, "Warn": true, "Error": true, "Detail": true}

// entry 是译表里的一条。
type entry struct {
	file     string // 相对 root，正斜杠
	line     int
	table    string // 所在的 map 字面量：文件名加变量名
	key, val string
	// 键与值两个表达式在文件里的字节区间
	keyPos, keyEnd, valPos, valEnd int
}

// literal 是源码里的一处字符串字面量；字面量用 + 相加的整段算一处。
type literal struct {
	file   string // 相对 root，正斜杠
	line   int
	test   bool // 在 _test.go 里
	val    string
	callee string // 是目标调用的第一个实参时，为该调用的写法（如 log.Info、i18n.T）；否则为空
	// 表达式在文件里的字节区间
	pos, end int
}

// direct 报告这处字面量是否直接作为目标调用的第一个实参。
func (l literal) direct() bool { return l.callee != "" }

// nonLiteral 是一处第一个实参不是字符串字面量的目标调用。
type nonLiteral struct {
	file   string
	line   int
	callee string
	src    string // 实参的源码
}

// tree 是一棵源码树里与 i18n 有关的全部内容。
type tree struct {
	entries     []entry
	literals    []literal            // 译表文件之外的全部字符串字面量，按文件、按出现先后
	nonLiterals []nonLiteral         // 出货代码里的
	byFile      map[string][]literal // literals 按文件分组
	src         map[string][]byte    // 相对路径 → 文件内容
}

// hasHan 报告 s 是否含汉字。
func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// verbRe 匹配 Go fmt 占位符，与 i18n/placeholders_test.go 的同名正则一致。
var verbRe = regexp.MustCompile(`%[+\-# 0]*(?:\[\d+\])?[0-9]*(?:\.[0-9]+)?[bcdeEfFgGoOpqstTUvwxX%]`)

// exempt 报告 s 是否可以不进译表：去掉占位符之后不含任何字母与数字，只剩标点、符号与空白。
// 与 i18n/catalogcoverage_test.go 的同名函数一致。
func exempt(s string) bool {
	for _, r := range verbRe.ReplaceAllString(s, "") {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// foldStringLit 返回字符串字面量的值，字面量用 + 相加的先拼接；含别的表达式时 ok 为 false。
func foldStringLit(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		a, okA := foldStringLit(v.X)
		b, okB := foldStringLit(v.Y)
		return a + b, okA && okB
	case *ast.ParenExpr:
		return foldStringLit(v.X)
	}
	return "", false
}

// scanFile 按出现先后取出一个文件里的全部字符串字面量，并标出哪些是目标调用的第一个实参；
// 另返回第一个实参不是字面量的目标调用。test 为真时只认 i18n.T 与 i18n.Errorf。
//
// 改写不增减字面量的处数（相加的整段换成一个字面量，仍是一处），-baseline 据此按先后配对。
func scanFile(fset *token.FileSet, file *ast.File, rel string, src []byte, test bool) ([]literal, []nonLiteral) {
	inPkg := file.Name.Name == "i18n"
	local := ""
	for _, imp := range file.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err == nil && p == i18nImportPath {
			local = "i18n"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
	}

	offset := func(p token.Pos) int { return fset.Position(p).Offset }
	var lits []literal
	var nonLits []nonLiteral
	callees := map[ast.Expr]string{} // 目标调用的第一个实参 → 调用的写法
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.CallExpr:
			if len(v.Args) == 0 {
				return true
			}
			target := false
			switch fun := v.Fun.(type) {
			case *ast.SelectorExpr:
				if x, ok := fun.X.(*ast.Ident); ok && local != "" && x.Name == local {
					target = fun.Sel.Name == "T" || fun.Sel.Name == "Errorf"
				} else {
					target = !test && loggerMethods[fun.Sel.Name]
				}
			case *ast.Ident:
				target = inPkg && (fun.Name == "T" || fun.Name == "Errorf")
			}
			if !target {
				return true
			}
			arg := v.Args[0]
			callee := string(src[offset(v.Fun.Pos()):offset(v.Fun.End())])
			if _, ok := foldStringLit(arg); ok {
				callees[arg] = callee
			} else if !test {
				nonLits = append(nonLits, nonLiteral{
					file: rel, line: fset.Position(arg.Pos()).Line, callee: callee,
					src: string(src[offset(arg.Pos()):offset(arg.End())]),
				})
			}
			return true
		case ast.Expr:
			if s, ok := foldStringLit(v); ok {
				lits = append(lits, literal{
					file: rel, line: fset.Position(v.Pos()).Line, test: test, val: s, callee: callees[v],
					pos: offset(v.Pos()), end: offset(v.End()),
				})
				return false
			}
		}
		return true
	})
	return lits, nonLits
}

// scanCatalog 取出一个文件里全部包级 map[string]string 字面量的条目。
func scanCatalog(fset *token.FileSet, file *ast.File, rel string) ([]entry, error) {
	var entries []entry
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, v := range vs.Values {
				cl, ok := v.(*ast.CompositeLit)
				if !ok || !isStringMap(cl.Type) {
					continue
				}
				for _, elt := range cl.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					k, okK := foldStringLit(kv.Key)
					val, okV := foldStringLit(kv.Value)
					if !okK || !okV {
						return nil, fmt.Errorf("%s:%d：译表条目的键或值不是字符串字面量", rel, fset.Position(kv.Pos()).Line)
					}
					entries = append(entries, entry{
						file: rel, line: fset.Position(kv.Pos()).Line, table: rel + " 的 " + vs.Names[i].Name, key: k, val: val,
						keyPos: fset.Position(kv.Key.Pos()).Offset, keyEnd: fset.Position(kv.Key.End()).Offset,
						valPos: fset.Position(kv.Value.Pos()).Offset, valEnd: fset.Position(kv.Value.End()).Offset,
					})
				}
			}
		}
	}
	return entries, nil
}

// isStringMap 报告类型表达式是否为 map[string]string。
func isStringMap(e ast.Expr) bool {
	m, ok := e.(*ast.MapType)
	if !ok {
		return false
	}
	k, okK := m.Key.(*ast.Ident)
	v, okV := m.Value.(*ast.Ident)
	return okK && okV && k.Name == "string" && v.Name == "string"
}

// readSources 读入 root 下要扫的 .go 文件，键为相对 root 的正斜杠路径。
func readSources(root string) (map[string][]byte, error) {
	src := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "build" || name == "tools" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src[filepath.ToSlash(rel)] = b
		return nil
	})
	return src, err
}

// buildTree 从一组源文件里取出译表、目标调用与其余字面量。
// i18n/ 下含包级 map[string]string 字面量的非测试文件是译表文件，其内容只当条目读。
func buildTree(src map[string][]byte) (*tree, error) {
	t := &tree{src: src, byFile: map[string][]literal{}}
	names := make([]string, 0, len(src))
	for rel := range src {
		names = append(names, rel)
	}
	sort.Strings(names)

	fset := token.NewFileSet()
	for _, rel := range names {
		file, err := parser.ParseFile(fset, rel, src[rel], parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		test := strings.HasSuffix(rel, "_test.go")
		if !test && strings.HasPrefix(rel, "i18n/") {
			entries, err := scanCatalog(fset, file, rel)
			if err != nil {
				return nil, err
			}
			if len(entries) > 0 {
				t.entries = append(t.entries, entries...)
				continue
			}
		}
		lits, nonLits := scanFile(fset, file, rel, src[rel], test)
		t.literals = append(t.literals, lits...)
		t.nonLiterals = append(t.nonLiterals, nonLits...)
		t.byFile[rel] = lits
	}
	if len(t.entries) == 0 {
		return nil, fmt.Errorf("i18n 目录下没有读到译表条目")
	}
	return t, nil
}

// loadTree 读入并解析 root 下的源码树。
func loadTree(root string) (*tree, error) {
	src, err := readSources(root)
	if err != nil {
		return nil, err
	}
	t, err := buildTree(src)
	if err != nil {
		return nil, fmt.Errorf("%s：%w", root, err)
	}
	return t, nil
}

// collision 是对调后会撞键的一组条目：同一句英文对应不止一句中文。
type collision struct {
	english string
	chinese []string // 各不相同，按条目出现先后
	where   []string // 与 chinese 一一对应的「文件:行」
}

// candidates 是译表里一句中文对应的各句英文，两个方向的条目都算。
type candidates struct {
	english []string // 各不相同，按条目出现先后
	where   []string // 与 english 一一对应的「文件:行」
}

// ambiguity 是一处改写不了的中文字面量：译表里这句中文对应不止一句英文。
type ambiguity struct {
	lit literal
	candidates
}

// analysis 是对一棵源码树的核对结果。
type analysis struct {
	zh2en     map[string]string      // 旧方向条目：中文 → 英文
	native    map[string]string      // 新方向条目：英文 → 中文
	en2zh     map[string]string      // native 连同旧方向条目对调后的结果
	byChinese map[string]*candidates // 中文 → 英文，两个方向的条目都收：改写调用点查它

	// 以下各项非空即未通过
	rewrites   []literal   // 待改写：含汉字、译表里恰有一句英文的直接实参
	swaps      []entry     // 待对调：旧方向的条目
	collisions []collision // 撞键
	drops      []entry     // 待去掉：旧方向的条目，它的中英对已另有一条
	conflicts  []string    // 同一个键出现不止一次且值不同
	dupKeys    []string    // 对调之后同一张表里重复的键
	hanEnglish []entry     // 旧方向条目的英文里含汉字，对调后键会含汉字
	ambiguous  []ambiguity // 含汉字、译表里对应不止一句英文的直接实参
	noEnglish  []literal   // 出货代码里含汉字、译表里没有英文的直接实参
	missing    []literal   // 出货代码里不含汉字、不在译表里、又不属豁免的直接实参

	// 以下各项供人核对
	exempted       []literal // 出货代码里不在译表里、按豁免放行的直接实参
	noEnglishTests []literal // 测试里含汉字、译表里没有英文的直接实参
	unreferenced   []entry   // 没有出货代码的直接实参引用的条目
	indirect       []literal // 出货代码里与译表的中文逐字相同、却不是直接实参的字面量
}

// failed 报告核对是否未通过。撞键与英文里含汉字的条目都以旧方向的条目为前提，有它们就有待对调的
// 条目，不必另算。待改写的调用点要单算：它复用表里已对调的中文时，没有待对调的条目。
func (a *analysis) failed() bool {
	return len(a.rewrites)+len(a.swaps)+len(a.drops)+len(a.conflicts)+len(a.dupKeys)+len(a.ambiguous)+len(a.noEnglish)+len(a.missing) > 0
}

// english 返回一句中文在译表里唯一对应的英文；没有或不止一句时 ok 为 false。
func (a *analysis) english(zh string) (string, bool) {
	if c := a.byChinese[zh]; c != nil && len(c.english) == 1 {
		return c.english[0], true
	}
	return "", false
}

// analyze 核对一棵源码树。
func analyze(t *tree) *analysis {
	a := &analysis{zh2en: map[string]string{}, native: map[string]string{}, en2zh: map[string]string{}, byChinese: map[string]*candidates{}}

	// 键值对逐条归到「英文 → 各句中文」之下，撞键由此得出。
	byEnglish := map[string]*collision{}
	var order []string
	seen := map[string]entry{}
	for _, e := range t.entries {
		old := hasHan(e.key)
		if prev, ok := seen[e.key]; ok {
			if prev.val != e.val {
				a.conflicts = append(a.conflicts, fmt.Sprintf("%q：%s:%d 为 %q，%s:%d 为 %q",
					e.key, prev.file, prev.line, prev.val, e.file, e.line, e.val))
			}
			continue
		}
		seen[e.key] = e
		zh, en := e.val, e.key
		if old {
			zh, en = e.key, e.val
			a.zh2en[zh] = en
			if hasHan(en) {
				a.hanEnglish = append(a.hanEnglish, e)
			}
		} else {
			a.native[en] = zh
		}
		where := fmt.Sprintf("%s:%d", e.file, e.line)
		if hasHan(zh) {
			cand := a.byChinese[zh]
			if cand == nil {
				cand = &candidates{}
				a.byChinese[zh] = cand
			}
			if !slices.Contains(cand.english, en) {
				cand.english = append(cand.english, en)
				cand.where = append(cand.where, where)
			}
		}
		c := byEnglish[en]
		if c == nil {
			c = &collision{english: en}
			byEnglish[en] = c
			order = append(order, en)
			a.en2zh[en] = zh
		}
		if !slices.Contains(c.chinese, zh) {
			c.chinese = append(c.chinese, zh)
			c.where = append(c.where, where)
		}
	}
	for _, en := range order {
		if c := byEnglish[en]; len(c.chinese) > 1 {
			a.collisions = append(a.collisions, *c)
		}
	}

	// 旧方向的条目：中英对已另有一条的去掉，其余对调。新方向的那条不论排在前面还是后面、
	// 在不在同一张表里都算；同一对旧方向条目写了不止一遍的，留第一条。
	type pair struct{ en, zh string }
	claimed := map[pair]bool{}
	for _, e := range t.entries {
		if !hasHan(e.key) {
			claimed[pair{e.key, e.val}] = true
		}
	}
	dropped := map[int]bool{} // t.entries 的下标
	for i, e := range t.entries {
		if !hasHan(e.key) {
			continue
		}
		if p := (pair{e.val, e.key}); claimed[p] {
			a.drops = append(a.drops, e)
			dropped[i] = true
		} else {
			claimed[p] = true
			a.swaps = append(a.swaps, e)
		}
	}

	// 对调、去重之后，每张表里的键须唯一：重复的键 gofmt 不管，编译不过。
	final := map[string]map[string]string{} // 表 → 键 → 「文件:行」
	for i, e := range t.entries {
		if dropped[i] {
			continue
		}
		key := e.key
		if hasHan(e.key) {
			key = e.val
		}
		if final[e.table] == nil {
			final[e.table] = map[string]string{}
		}
		where := fmt.Sprintf("%s:%d", e.file, e.line)
		if prev, ok := final[e.table][key]; ok {
			a.dupKeys = append(a.dupKeys, fmt.Sprintf("%s：%q 在 %s 与 %s 各有一条", e.table, key, prev, where))
			continue
		}
		final[e.table][key] = where
	}

	referenced := map[string]bool{}
	for _, l := range t.literals {
		switch {
		case !l.direct():
			if !l.test && a.byChinese[l.val] != nil {
				a.indirect = append(a.indirect, l)
			}
		case hasHan(l.val):
			cand := a.byChinese[l.val]
			switch {
			case cand != nil && len(cand.english) == 1:
				a.rewrites = append(a.rewrites, l)
				referenced[cand.english[0]] = referenced[cand.english[0]] || !l.test
			case cand != nil:
				a.ambiguous = append(a.ambiguous, ambiguity{lit: l, candidates: *cand})
			case l.test:
				a.noEnglishTests = append(a.noEnglishTests, l)
			default:
				a.noEnglish = append(a.noEnglish, l)
			}
		case l.test:
		default:
			if _, ok := a.en2zh[l.val]; ok {
				referenced[l.val] = true
			} else if exempt(l.val) {
				a.exempted = append(a.exempted, l)
			} else {
				a.missing = append(a.missing, l)
			}
		}
	}
	for _, e := range t.entries {
		en := e.key
		if hasHan(e.key) {
			en = e.val
		}
		if !referenced[en] {
			a.unreferenced = append(a.unreferenced, e)
		}
	}
	return a
}

// report 把核对结果写到标准输出。
func (a *analysis) report(t *tree) {
	direct, shipped := 0, 0
	for _, l := range t.literals {
		if l.direct() {
			direct++
			if !l.test {
				shipped++
			}
		}
	}
	fmt.Printf("译表条目 %d 条；直接传入字面量的调用点 %d 处（出货代码 %d 处，测试 %d 处）\n",
		len(t.entries), direct, shipped, direct-shipped)

	section := func(title string, n int, each func(i int) string) {
		fmt.Printf("\n%s：%d\n", title, n)
		for i := 0; i < n; i++ {
			fmt.Println("  " + each(i))
		}
	}
	lit := func(ls []literal) func(int) string {
		return func(i int) string {
			l := ls[i]
			if l.direct() {
				return fmt.Sprintf("%s:%d  %s(%q)", l.file, l.line, l.callee, l.val)
			}
			return fmt.Sprintf("%s:%d  %q", l.file, l.line, l.val)
		}
	}
	ent := func(es []entry) func(int) string {
		return func(i int) string {
			return fmt.Sprintf("%s:%d  %q: %q", es[i].file, es[i].line, es[i].key, es[i].val)
		}
	}

	rewritesShipped := 0
	for _, l := range a.rewrites {
		if !l.test {
			rewritesShipped++
		}
	}
	fmt.Printf("\n待改写的调用点：%d（出货代码 %d，测试 %d）\n", len(a.rewrites), rewritesShipped, len(a.rewrites)-rewritesShipped)
	fmt.Printf("待对调的条目：%d\n", len(a.swaps))
	section("待去掉的条目（旧方向，它的中英对已另有一条）", len(a.drops), ent(a.drops))
	section("撞键（同一句英文对应不止一句中文）", len(a.collisions), func(i int) string {
		c := a.collisions[i]
		var b strings.Builder
		fmt.Fprintf(&b, "%q", c.english)
		for j := range c.chinese {
			fmt.Fprintf(&b, "\n      %s  %q", c.where[j], c.chinese[j])
		}
		return b.String()
	})
	section("同一个键出现不止一次且值不同", len(a.conflicts), func(i int) string { return a.conflicts[i] })
	section("对调之后同一张表里重复的键", len(a.dupKeys), func(i int) string { return a.dupKeys[i] })
	section("英文里含汉字的条目", len(a.hanEnglish), ent(a.hanEnglish))
	section("含汉字、译表里对应不止一句英文的字面量（须人工定用哪一句）", len(a.ambiguous), func(i int) string {
		m := a.ambiguous[i]
		var b strings.Builder
		fmt.Fprintf(&b, "%s:%d  %s(%q)", m.lit.file, m.lit.line, m.lit.callee, m.lit.val)
		for j := range m.english {
			fmt.Fprintf(&b, "\n      %s  %q", m.where[j], m.english[j])
		}
		return b.String()
	})
	section("含汉字却没有英文的字面量（出货代码）", len(a.noEnglish), lit(a.noEnglish))
	section("不在译表里的字面量（出货代码）", len(a.missing), lit(a.missing))

	fmt.Println("\n—— 以下供人核对，不影响退出码 ——")
	section("按豁免放行的字面量（出货代码）", len(a.exempted), lit(a.exempted))
	section("实参不是字面量的调用点（出货代码）", len(t.nonLiterals), func(i int) string {
		n := t.nonLiterals[i]
		return fmt.Sprintf("%s:%d  %s(%s)", n.file, n.line, n.callee, n.src)
	})
	section("与译表里的中文逐字相同、却不是直接实参的字面量（出货代码）", len(a.indirect), lit(a.indirect))
	section("没有出货代码的直接实参引用的条目", len(a.unreferenced), ent(a.unreferenced))
	section("含汉字却没有英文的字面量（测试）", len(a.noEnglishTests), lit(a.noEnglishTests))
}

// edit 是对一个文件的一处替换。
type edit struct {
	pos, end int
	text     string
}

// applyEdits 把互不重叠的替换落到 src 上，并按 gofmt 重排。
func applyEdits(src []byte, edits []edit) ([]byte, error) {
	sort.Slice(edits, func(i, j int) bool { return edits[i].pos > edits[j].pos })
	out := append([]byte(nil), src...)
	for _, e := range edits {
		out = append(out[:e.pos], append([]byte(e.text), out[e.end:]...)...)
	}
	return format.Source(out)
}

// removal 返回去掉一条条目要删的字节区间：条目连同它后面的逗号；条目独占若干行时把这几行整个
// 删掉（含行尾注释），与别的条目同在一行时只删到逗号后的空白为止。
//
// 值与逗号之间夹着的块注释（"甲": "same" /* … */,）随条目一起删：只认空白的话找不到逗号，
// 留下一个孤立的逗号，改写后的内容无法格式化。行注释与跨行的块注释不会出现在这个位置——
// 它们相当于换行，值后面会被补上分号，那样的源码本来就解析不了。
func removal(src []byte, e entry) (pos, end int) {
	blank := func(b byte) bool { return b == ' ' || b == '\t' }
	pos, end = e.keyPos, e.valEnd
	for {
		for end < len(src) && blank(src[end]) {
			end++
		}
		if !bytes.HasPrefix(src[end:], []byte("/*")) {
			break
		}
		n := bytes.Index(src[end+2:], []byte("*/"))
		if n < 0 {
			break
		}
		end += 2 + n + 2
	}
	if end < len(src) && src[end] == ',' {
		end++
	}
	for end < len(src) && blank(src[end]) {
		end++
	}

	lineStart := pos
	for lineStart > 0 && blank(src[lineStart-1]) {
		lineStart--
	}
	lineEnd := end
	if lineEnd+1 < len(src) && src[lineEnd] == '/' && src[lineEnd+1] == '/' {
		for lineEnd < len(src) && src[lineEnd] != '\n' {
			lineEnd++
		}
	}
	if (lineStart == 0 || src[lineStart-1] == '\n') && lineEnd < len(src) && src[lineEnd] == '\n' {
		return lineStart, lineEnd + 1
	}
	return pos, end
}

// plan 算出改写要做的全部替换，按文件分组：调用点的中文字面量换成英文，旧方向条目的键与值对调，
// 中英对已另有一条的旧方向条目去掉。
func plan(t *tree, a *analysis) map[string][]edit {
	edits := map[string][]edit{}
	for _, l := range a.rewrites {
		en, _ := a.english(l.val) // 进了 rewrites 的都恰有一句英文
		edits[l.file] = append(edits[l.file], edit{l.pos, l.end, strconv.Quote(en)})
	}
	for _, e := range a.swaps {
		src := t.src[e.file]
		edits[e.file] = append(edits[e.file],
			edit{e.keyPos, e.keyEnd, string(src[e.valPos:e.valEnd])},
			edit{e.valPos, e.valEnd, string(src[e.keyPos:e.keyEnd])})
	}
	for _, e := range a.drops {
		pos, end := removal(t.src[e.file], e)
		edits[e.file] = append(edits[e.file], edit{pos, end, ""})
	}
	return edits
}

// rewrite 算出改写后的文件内容，只含有改动的文件。
func rewrite(t *tree, a *analysis) (map[string][]byte, error) {
	if len(a.collisions)+len(a.conflicts)+len(a.dupKeys)+len(a.hanEnglish) > 0 {
		return nil, fmt.Errorf("译表有撞键、同键异值、表内重复的键或含汉字的英文，须先人工处理")
	}
	out := map[string][]byte{}
	for file, edits := range plan(t, a) {
		b, err := applyEdits(t.src[file], edits)
		if err != nil {
			return nil, fmt.Errorf("%s：改写后的内容无法格式化: %w", file, err)
		}
		out[file] = b
	}
	return out, nil
}

// render 返回一处直接实参在本源码树的译表下，中文环境与英文环境各显示什么。
// 旧方向的条目按「中文环境原样、英文环境查表」算，新方向的条目反之。
func (a *analysis) render(s string) (zh, en string) {
	if e, ok := a.zh2en[s]; ok {
		return s, e
	}
	if z, ok := a.native[s]; ok {
		return z, s
	}
	return s, s
}

// pairs 返回译表的全部「中文—英文」对。
func (a *analysis) pairs() map[[2]string]bool {
	p := map[[2]string]bool{}
	for zh, en := range a.zh2en {
		p[[2]string{zh, en}] = true
	}
	for en, zh := range a.native {
		p[[2]string{zh, en}] = true
	}
	return p
}

// compare 把 before 当作改写前的源码树，与 after 逐文件、按字面量的先后配对，返回差异。
//
// 直接实参：两种语言下显示的文字须与改写前相同。其余字面量：要么没变，要么是按改写前的译表
// 由中文换成了对应的英文，且改写后的译表能把它译回原来的中文。译表：「中文—英文」对须相同，
// 多出键值相同的条目不算差异。
func compare(before, after *tree) (diffs []string, summary string) {
	ba, aa := analyze(before), analyze(after)

	bp, ap := ba.pairs(), aa.pairs()
	identities := 0
	for p := range bp {
		if !ap[p] {
			diffs = append(diffs, fmt.Sprintf("译表少了一对：%q — %q", p[0], p[1]))
		}
	}
	for p := range ap {
		switch {
		case bp[p]:
		case p[0] == p[1]:
			identities++
		default:
			diffs = append(diffs, fmt.Sprintf("译表多了一对：%q — %q", p[0], p[1]))
		}
	}

	files := map[string]bool{}
	for f := range before.byFile {
		files[f] = true
	}
	for f := range after.byFile {
		files[f] = true
	}
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)

	direct, rewritten, indirect := 0, 0, 0
	for _, f := range names {
		bl, okB := before.byFile[f]
		al, okA := after.byFile[f]
		if !okB || !okA {
			diffs = append(diffs, fmt.Sprintf("%s：只在一侧存在，未比对", f))
			continue
		}
		if len(bl) != len(al) {
			diffs = append(diffs, fmt.Sprintf("%s：字面量处数不同（%d → %d），无法配对", f, len(bl), len(al)))
			continue
		}
		for i := range bl {
			o, n := bl[i], al[i]
			switch {
			case o.direct() != n.direct():
				diffs = append(diffs, fmt.Sprintf("%s:%d：第 %d 处字面量是否为直接实参，两侧不同", f, n.line, i+1))
			case o.direct():
				direct++
				ozh, oen := ba.render(o.val)
				nzh, nen := aa.render(n.val)
				if ozh != nzh {
					diffs = append(diffs, fmt.Sprintf("%s:%d  %s：中文环境 %q → %q", f, n.line, n.callee, ozh, nzh))
				}
				if oen != nen {
					diffs = append(diffs, fmt.Sprintf("%s:%d  %s：英文环境 %q → %q", f, n.line, n.callee, oen, nen))
				}
				if ozh == nzh && oen == nen && o.val != n.val {
					rewritten++
				}
			case o.val != n.val:
				if en, ok := ba.english(o.val); ok && en == n.val && aa.native[n.val] == o.val {
					indirect++
				} else {
					diffs = append(diffs, fmt.Sprintf("%s:%d：字面量 %q → %q，不是按译表由中文换成英文", f, n.line, o.val, n.val))
				}
			}
		}
	}
	sort.Strings(diffs)
	summary = fmt.Sprintf("比对文件 %d 个；直接实参 %d 处，其中改写 %d 处；其余字面量按译表由中文换成英文 %d 处；"+
		"译表 %d 对 → %d 对（多出键值相同的 %d 对）",
		len(names), direct, rewritten, indirect, len(bp), len(ap), identities)
	return diffs, summary
}

func main() {
	root := flag.String("root", ".", "源码树根目录")
	write := flag.Bool("write", false, "改写调用点、对调译表条目")
	baseline := flag.String("baseline", "", "改写前的源码树；给出时与它逐调用点比对")
	flag.Parse()

	fail := func(err error) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	t, err := loadTree(*root)
	if err != nil {
		fail(err)
	}

	if *baseline != "" {
		before, err := loadTree(*baseline)
		if err != nil {
			fail(err)
		}
		diffs, summary := compare(before, t)
		fmt.Println(summary)
		fmt.Printf("\n差异：%d\n", len(diffs))
		for _, d := range diffs {
			fmt.Println("  " + d)
		}
		if len(diffs) > 0 {
			os.Exit(1)
		}
		return
	}

	a := analyze(t)
	if *write {
		out, err := rewrite(t, a)
		if err != nil {
			a.report(t)
			fail(err)
		}
		for file, b := range out {
			if err := os.WriteFile(filepath.Join(*root, filepath.FromSlash(file)), b, 0644); err != nil {
				fail(err)
			}
		}
		fmt.Printf("已改写 %d 个文件。以下为改写后的核对结果。\n\n", len(out))
		if t, err = loadTree(*root); err != nil {
			fail(err)
		}
		a = analyze(t)
	}
	a.report(t)
	if a.failed() {
		os.Exit(1)
	}
}
