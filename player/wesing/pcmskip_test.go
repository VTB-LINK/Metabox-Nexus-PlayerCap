package wesing

// 本文件守按首兜底的标记 pcmSkip（#145 的停滞、#196 的速率不符，设计 doc/design/wesing-small-seek-design.md
// §3.6、§4.7）：标记只对这一次播放有效，同一首的重来期间保留，窗口换成别的歌、待机、关窗时清除。runSession 在
// 播放阶段每次看到窗口标题调 seen，待机与 exitWindowClosed 调 reset，exitPCMRate 与 exitPCMStalled 调 mark，选不选
// PCM 只看 on。runSession 要真进程才能跑，接线用 AST 门禁守（做法同 idlenotice_test.go、songgenorder_test.go）。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// S45 标记的序列。名称里写明对应的情形；几种情形在 runSession 里落到同一串调用时并为一例。
//
// 变异自证（实测）：seen 从不清除，「判为不符后换成别的歌」「A、B、A」「标记时标题为空」红；seen 不看空标题，
// 「重来时标题短暂为空」红；reset 不清除，「关窗（待机）后重点同一首」红；mark 不置 on，四个反例都红。
func TestPCMSkip(t *testing.T) {
	cases := []struct {
		name  string
		steps func(s *pcmSkip)
		want  bool
	}{
		// 正例：清除，下一首先试 PCM。
		{"没判过不符、没停滞过", func(s *pcmSkip) { s.seen("晴天") }, false},
		{"判为不符后换成别的歌", func(s *pcmSkip) { s.mark("晴天"); s.seen("七里香") }, false},
		{"A 判为不符、播 B、回到 A", func(s *pcmSkip) { s.mark("晴天"); s.seen("七里香"); s.seen("晴天") }, false},
		{"关窗（待机）后重点同一首（录音 24《Cry On My Shoulder》）", func(s *pcmSkip) { s.mark("晴天"); s.reset(); s.seen("晴天") }, false},
		{"标记时标题为空，之后出现标题", func(s *pcmSkip) { s.mark(""); s.seen(""); s.seen("晴天") }, false},
		// 反例：保留，这一首不再试 PCM。
		// 同一首的重来（exitDeadAddr、exitAltAddr、exitPCMRate、exitPCMStalled 不清 lastTitle）、连续读失败 3 s 的
		// 重来（exitSongChanged 清了 lastTitle，回到播放阶段 seen 同一标题）、很快重点同一首而未经待机（录音 24
		// 《Shivers》）、同名的另一首接着播放：在 runSession 里都是 mark 之后 seen 同一标题。
		{"判为不符后同一首的重来、很快重点同一首、同名的另一首", func(s *pcmSkip) { s.mark("晴天"); s.seen("晴天") }, true},
		// #145 代码审 F6 复审的反例：读失败重来，lastTitle 被清空，窗口标题又短暂为空。
		{"重来时标题短暂为空", func(s *pcmSkip) { s.mark("晴天"); s.seen(""); s.seen("晴天") }, true},
		{"加载阶段不调 seen：重点同一首、加载中再进播放", func(s *pcmSkip) { s.mark("晴天"); s.seen("晴天"); s.seen("晴天") }, true},
		{"换歌之后那一首又判出", func(s *pcmSkip) { s.mark("晴天"); s.seen("七里香"); s.mark("七里香"); s.seen("七里香") }, true},
	}
	for _, c := range cases {
		var s pcmSkip
		c.steps(&s)
		if s.on != c.want {
			t.Errorf("%s：on = %v，want %v", c.name, s.on, c.want)
		}
	}
}

// callsFunc 报告 n 里有没有对普通函数 name 的调用（不是方法调用）。
func callsFunc(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(m ast.Node) bool {
		if c, ok := m.(*ast.CallExpr); ok && isIdent(c.Fun, name) {
			found = true
		}
		return !found
	})
	return found
}

// skipCases 是要调 mark 的两个退出原因。
var skipCases = []string{"exitPCMRate", "exitPCMStalled"}

// scanPCMSkipWiring 扫 runSession（S46）：
//   - 包住 selectPCM 调用的 if，条件只有 `!x.on`，x 声明为 pcmSkip；
//   - `case exitPCMRate`、`case exitPCMStalled` 的分支各调恰一次 `x.mark(lastTitle)`；
//   - 别处不调 mark，mark 不记在别的变量上、不传别的实参。
//
// found 报告是否找到了门禁的锚点（runSession、包住 selectPCM 的 if、两个 case）。
func scanPCMSkipWiring(fset *token.FileSet, f *ast.File) (violations []string, found bool) {
	var body *ast.BlockStmt
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil && fd.Name.Name == "runSession" && fd.Body != nil {
			body = fd.Body
		}
	}
	if body == nil {
		return nil, false
	}
	bad := func(p token.Pos, msg string) {
		violations = append(violations, fmt.Sprintf("%s: %s", fset.Position(p), msg))
	}

	skip, guards := "", 0
	ast.Inspect(body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || !callsFunc(ifs.Body, "selectPCM") {
			return true
		}
		guards++
		if u, ok := ifs.Cond.(*ast.UnaryExpr); ok && u.Op == token.NOT {
			if sel, ok := u.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "on" {
				if id, ok := sel.X.(*ast.Ident); ok {
					skip = id.Name
					return true
				}
			}
		}
		bad(ifs.Cond.Pos(), "选 PCM 的条件不是单独的 !<标记>.on")
		return true
	})
	if guards == 0 {
		return violations, false
	}

	declared := false
	ast.Inspect(body, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok && isIdent(vs.Type, "pcmSkip") {
			for _, nm := range vs.Names {
				declared = declared || (skip != "" && nm.Name == skip)
			}
		}
		return true
	})
	if skip != "" && !declared {
		bad(body.Pos(), fmt.Sprintf("选 PCM 看的 %s 不是声明为 pcmSkip 的变量", skip))
	}

	marks, seen, ok := map[string]int{}, map[string]bool{}, map[token.Pos]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		cc, isCase := n.(*ast.CaseClause)
		if !isCase {
			return true
		}
		name := ""
		for _, e := range cc.List {
			for _, want := range skipCases {
				if isIdent(e, want) {
					name = want
				}
			}
		}
		if name == "" {
			return true
		}
		seen[name] = true
		for _, s := range cc.Body {
			ast.Inspect(s, func(m ast.Node) bool {
				c, isCall := m.(*ast.CallExpr)
				if !isCall || calleeName(c) != "mark" {
					return true
				}
				sel := c.Fun.(*ast.SelectorExpr)
				if isIdent(sel.X, skip) && len(c.Args) == 1 && isIdent(c.Args[0], "lastTitle") {
					marks[name]++
					ok[c.Pos()] = true
				}
				return true
			})
		}
		return true
	})
	ast.Inspect(body, func(n ast.Node) bool {
		if c, isCall := n.(*ast.CallExpr); isCall && calleeName(c) == "mark" && !ok[c.Pos()] {
			bad(c.Pos(), "mark 只能在 exitPCMRate、exitPCMStalled 的分支里、以 <标记>.mark(lastTitle) 调用")
		}
		return true
	})
	who := skip
	if who == "" {
		who = "<标记>"
	}
	for _, name := range skipCases {
		if !seen[name] {
			return violations, false
		}
		if marks[name] != 1 {
			bad(body.Pos(), fmt.Sprintf("case %s 里调了 %d 次 %s.mark(lastTitle)，want 恰一次", name, marks[name], who))
		}
	}
	return violations, true
}

// S46 runSession 的按首兜底接线：exitPCMRate 与 exitPCMStalled 共用按首标记，选 PCM 只看它（#196）。
//
// 变异自证（实测）：exitPCMRate 恢复会话级开关 pcmOff（#196 之前的形状）、exitPCMRate 不调 mark（同一首会立即
// 重选 PCM）、mark 传空标题（同一首重来时被 seen 清除）、选 PCM 的条件多一个会话级开关，都红。
func TestPCMSkipWiring(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "wesing.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 wesing.go 失败: %v", err)
	}
	violations, found := scanPCMSkipWiring(fset, f)
	if !found {
		t.Fatal("没有在 runSession 里找到包住 selectPCM 的 if 或 exitPCMRate、exitPCMStalled 两个分支——门禁的判据失效了（改名了，或挪走了）")
	}
	for _, v := range violations {
		t.Errorf("%s\n速率不符与停滞都只使这一首改走兜底，下一首仍先试 PCM（#196）。修法：两个退出分支调 skip.mark(lastTitle)，选 PCM 只看 !skip.on。", v)
	}
}

// S47 门禁自身认得出目标形状。
//
// 变异自证（实测）：门禁只查 exitPCMStalled 一处，「exitPCMRate 只置布尔量」「只有 exitPCMStalled 调 mark」漏报、
// 「缺了 exitPCMRate 分支」算作找到而红（S46 同样红）；门禁不查 mark 的实参，「mark 传空串」「mark 传别的变量」
// 漏报而红。
func TestPCMSkipLintCatchesKnownHoles(t *testing.T) {
	parse := func(src string) (*token.FileSet, *ast.File) {
		t.Helper()
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "snippet.go", "package p\n"+src, 0)
		if err != nil {
			t.Fatalf("解析片段失败: %v\n%s", err, src)
		}
		return fset, f
	}
	// session 拼出一个 runSession：decl 是标记的声明，cond 是选 PCM 的条件，rate、stalled、changed 是三个分支的语句。
	session := func(decl, cond, rate, stalled, changed string) string {
		return "func (p *P) runSession() {\n\t" + decl + "\n\tfor {\n" +
			"\t\tif " + cond + " {\n\t\t\tinst, err := selectPCM(h, pid, &v)\n\t\t\t_, _ = inst, err\n\t\t}\n" +
			"\t\tswitch exitR {\n" +
			"\t\tcase exitSongChanged:\n\t\t\tlastTitle = \"\"\n\t\t\t" + changed + "\n" +
			"\t\tcase exitPCMRate:\n\t\t\t" + rate + "\n" +
			"\t\tcase exitPCMStalled:\n\t\t\t" + stalled + "\n" +
			"\t\t}\n\t}\n}\n"
	}
	const decl, cond, mark = "var skip pcmSkip", "!skip.on", "skip.mark(lastTitle)"

	fset, f := parse(session(decl, cond, mark, mark, ""))
	if v, found := scanPCMSkipWiring(fset, f); !found || len(v) != 0 {
		t.Errorf("标准形态：找到 %v，违规 %v，want 找到、无违规", found, v)
	}
	for name, src := range map[string]string{
		"exitPCMRate 置会话级开关":       session(decl+"\n\tpcmOff := false", "!pcmOff && !skip.on", "pcmOff = true", mark, ""),
		"exitPCMRate 只置布尔量、条件不变":   session(decl, cond, "pcmOff = true", mark, ""),
		"只有 exitPCMStalled 调 mark": session(decl, cond, "continue", mark, ""),
		"mark 传空串":                 session(decl, cond, `skip.mark("")`, mark, ""),
		"mark 传别的变量":               session(decl, cond, "skip.mark(title)", mark, ""),
		"mark 记在别的标记上":             session(decl+"\n\tvar other pcmSkip", cond, "other.mark(lastTitle)", mark, ""),
		"条件多一个会话级开关":               session(decl, "!off && !skip.on", mark, mark, ""),
		"exitSongChanged 里也调 mark": session(decl, cond, mark, mark, mark),
		"exitPCMRate 调两次 mark":     session(decl, cond, mark+"\n\t\t\t"+mark, mark, ""),
		"标记不是 pcmSkip":             session("var skip pcmStall", cond, mark, mark, ""),
	} {
		fset, f := parse(src)
		switch v, found := scanPCMSkipWiring(fset, f); {
		case !found:
			t.Errorf("%s：没找到锚点，片段本身写错了\n%s", name, src)
		case len(v) == 0:
			t.Errorf("%s：门禁漏报\n%s", name, src)
		}
	}
	fset, f = parse(strings.Replace(session(decl, cond, mark, mark, ""), "case exitPCMRate:", "case exitOther:", 1))
	if _, found := scanPCMSkipWiring(fset, f); found {
		t.Error("缺了 exitPCMRate 分支的片段不该算找到")
	}
}
