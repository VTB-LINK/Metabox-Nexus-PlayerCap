package wesing

// 本文件钉住 runSession 里本首代次的建立次序（#146，设计 §3.6）：NewSongGen() 排在本首第一条宣告事件之前，
// 封面 goroutine 用同一个代次。它不测业务逻辑，扫的是 wesing.go 的源码。
//
// # 它守的是什么
//
// 此前先发 status_update、song_info_update、all_lyrics，之后才调 NewSongGen()。两者之间上一首迟到的封面回写
// 仍被 EmitForGen 视为有效，服务端收到后整体替换歌曲信息，这首又可能不再回写，于是一直是上一首的。
//
// # 为什么用 AST 门禁
//
// runSession 要真进程才能跑，没有离线夹具；做法同 player/songgenwiring_lint_test.go。
//
// # 它不守什么
//
// 只查形状：runSession 里含 startCoverFetch 调用语句的那个语句块中，要有一条顶层的 `x := p.NewSongGen()`（或
// `x = …`），排在该块第一条宣告语句之前。宣告语句指该块顶层对 Emit / TryEmit 的调用语句、首个实参是
// EventStatusUpdate、EventSongInfoUpdate 或 EventAllLyrics 的，以及对名为 send 的方法的调用语句（allLyricsAnnounce）。
// startCoverFetch 的代次实参（第二个）须是 x 本身；NewSongGen 在 runSession 里恰调一次。嵌套在分支里的宣告、
// 经变量转手的事件类型、接收者是谁都不查。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// stmtCall 返回调用语句 s 的调用表达式，s 不是调用语句时返回 nil。
func stmtCall(s ast.Stmt) *ast.CallExpr {
	es, ok := s.(*ast.ExprStmt)
	if !ok {
		return nil
	}
	c, _ := es.X.(*ast.CallExpr)
	return c
}

// calleeName 是方法调用的方法名（任意接收者），不是方法调用时为空串。
func calleeName(c *ast.CallExpr) string {
	if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
		return sel.Sel.Name
	}
	return ""
}

var announceEvents = map[string]bool{"EventStatusUpdate": true, "EventSongInfoUpdate": true, "EventAllLyrics": true}

// isAnnounce 报告 s 是不是宣告语句。
func isAnnounce(s ast.Stmt) bool {
	c := stmtCall(s)
	if c == nil {
		return false
	}
	switch calleeName(c) {
	case "send":
		return true
	case "Emit", "TryEmit":
		if len(c.Args) > 0 {
			if sel, ok := c.Args[0].(*ast.SelectorExpr); ok {
				return announceEvents[sel.Sel.Name]
			}
		}
	}
	return false
}

// newSongGenVar 报告 s 是不是 `x := p.NewSongGen()` 或 `x = p.NewSongGen()`，返回 x。
func newSongGenVar(s ast.Stmt) (string, bool) {
	as, ok := s.(*ast.AssignStmt)
	if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
		return "", false
	}
	id, ok := as.Lhs[0].(*ast.Ident)
	c, isCall := as.Rhs[0].(*ast.CallExpr)
	if !ok || !isCall || calleeName(c) != "NewSongGen" {
		return "", false
	}
	return id.Name, true
}

// scanSongGenOrder 扫 runSession，返回违规描述；found 报告是否找到了含 startCoverFetch 调用语句的块（找不到说明
// 门禁的判据失效了）。
func scanSongGenOrder(fset *token.FileSet, f *ast.File) (violations []string, found bool) {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || fd.Name.Name != "runSession" || fd.Body == nil {
			continue
		}
		calls := 0
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && calleeName(c) == "NewSongGen" {
				calls++
			}
			return true
		})
		if calls != 1 {
			violations = append(violations, fmt.Sprintf("%s: runSession 里 NewSongGen() 调了 %d 次，应恰为 1 次", fset.Position(fd.Pos()), calls))
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			b, ok := n.(*ast.BlockStmt)
			if !ok {
				return true
			}
			cover := -1
			for i, s := range b.List {
				if c := stmtCall(s); c != nil && calleeName(c) == "startCoverFetch" {
					cover = i
					break
				}
			}
			if cover < 0 {
				return true
			}
			found = true
			pos := fset.Position(b.List[cover].Pos())
			gen, genVar, ann := -1, "", -1
			for i, s := range b.List {
				if v, ok := newSongGenVar(s); ok && gen < 0 {
					gen, genVar = i, v
				}
				if ann < 0 && isAnnounce(s) {
					ann = i
				}
			}
			switch {
			case gen < 0:
				violations = append(violations, fmt.Sprintf("%s: 所在块里没有顶层的 x := p.NewSongGen()", pos))
			case ann < 0:
				violations = append(violations, fmt.Sprintf("%s: 所在块里没有宣告语句——门禁的判据失效了（宣告挪进了分支，或换了写法）", pos))
			case gen > ann:
				violations = append(violations, fmt.Sprintf("%s: NewSongGen() 排在本首第一条宣告（第 %d 行）之后", pos, fset.Position(b.List[ann].Pos()).Line))
			}
			if args := stmtCall(b.List[cover]).Args; genVar == "" || len(args) < 2 || !isIdent(args[1], genVar) {
				violations = append(violations, fmt.Sprintf("%s: startCoverFetch 的代次实参不是 NewSongGen() 赋给的变量", pos))
			}
			return true
		})
	}
	return violations, found
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func scanSongGenOrderSrc(t *testing.T, src string) ([]string, bool) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatalf("解析片段失败: %v\n%s", err, src)
	}
	return scanSongGenOrder(fset, f)
}

// TestRunSessionSongGenBeforeAnnounce wesing.go 的 runSession：NewSongGen() 早于本首第一条宣告事件，封面 goroutine
// 用同一个代次。
//
// 变异自证（实测）：把 coverGen := p.NewSongGen() 挪回 ann.send 之后（#146 之前的次序）即红；去掉这一行、在
// startCoverFetch 的实参里直接调 p.NewSongGen() 即红。
func TestRunSessionSongGenBeforeAnnounce(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "wesing.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 wesing.go 失败: %v", err)
	}
	violations, found := scanSongGenOrder(fset, f)
	if !found {
		t.Fatal("没有在 runSession 里找到含 startCoverFetch 调用语句的块——门禁的判据失效了（改名了，或挪进了分支）")
	}
	for _, v := range violations {
		t.Errorf("%s\n宣告之后才建立代次，其间上一首迟到的封面回写会被当成这首的，服务端整体替换歌曲信息（设计 §3.6）。\n"+
			"修法：在本首第一条宣告事件之前 coverGen := p.NewSongGen()，并把 coverGen 交给 startCoverFetch。", v)
	}
}

// TestSongGenOrderLintCatchesKnownHoles 钉死门禁自身认得出目标形状（教训同 songgenwiring_lint_test.go）。
//
// 变异自证（实测）：宣告只认 Emit 不认 TryEmit，「排在 TryEmit(all_lyrics) 之后」漏报而红；不认 send，「排在
// ann.send 之后」漏报而红；不查代次实参，「代次实参是别的变量」漏报而红；不数调用次数（改成多于 2 次才报），
// 「宣告之后又调一次」漏报而红；次序判定差一（gen > ann+1），「排在 X 之后」四例漏报而红；宣告取块里最后一条
// 而不是第一条，同样四例漏报而红。
func TestSongGenOrderLintCatchesKnownHoles(t *testing.T) {
	const head = `package p
import "Metabox-Nexus-PlayerCap/player"
`
	wrap := func(body string) string {
		return head + "func (p *P) runSession() {\n	for {\n" + body + "	}\n}\n"
	}
	const announce = "		p.Emit(player.EventStatusUpdate, s)\n		p.Emit(player.EventSongInfoUpdate, i)\n		ann.send(p.TryEmit, 0, 0)\n"

	// 「排在 X 之后」各例在 NewSongGen 之后另有一条认得出的宣告：认不出 X 的门禁会以为次序是对的、漏报，而不是
	// 因为一条宣告都找不到才报。
	shouldCatch := map[string]string{
		"排在 status_update 之后": wrap("		p.Emit(player.EventStatusUpdate, s)\n		g := p.NewSongGen()\n" +
			"		p.Emit(player.EventSongInfoUpdate, i)\n		p.startCoverFetch(ops, g, pid)\n"),
		"排在 song_info_update 之后": wrap("		p.Emit(player.EventSongInfoUpdate, i)\n		g := p.NewSongGen()\n" +
			"		p.Emit(player.EventStatusUpdate, s)\n		p.startCoverFetch(ops, g, pid)\n"),
		"排在 ann.send 之后": wrap("		ann.send(p.TryEmit, 0, 0)\n		g := p.NewSongGen()\n" +
			"		p.Emit(player.EventStatusUpdate, s)\n		p.startCoverFetch(ops, g, pid)\n"),
		"排在 TryEmit(all_lyrics) 之后": wrap("		p.TryEmit(player.EventAllLyrics, d)\n		g := p.NewSongGen()\n" +
			"		p.Emit(player.EventStatusUpdate, s)\n		p.startCoverFetch(ops, g, pid)\n"),
		"#146 之前的次序": wrap(announce + "		coverGen := p.NewSongGen()\n		p.startCoverFetch(ops, coverGen, pid)\n"),
		"放在条件分支里": wrap("		if x {\n			g = p.NewSongGen()\n		}\n" + announce +
			"		p.startCoverFetch(ops, g, pid)\n"),
		"没有 NewSongGen":      wrap(announce + "		p.startCoverFetch(ops, g, pid)\n"),
		"代次实参直接调 NewSongGen": wrap(announce + "		p.startCoverFetch(ops, p.NewSongGen(), pid)\n"),
		"代次实参是别的变量":          wrap("		g := p.NewSongGen()\n" + announce + "		p.startCoverFetch(ops, h, pid)\n"),
		"宣告之后又调一次": wrap("		g := p.NewSongGen()\n" + announce + "		g = p.NewSongGen()\n" +
			"		p.startCoverFetch(ops, g, pid)\n"),
		"宣告挪进了分支": wrap("		g := p.NewSongGen()\n		if ok {\n" + announce + "		}\n" +
			"		p.startCoverFetch(ops, g, pid)\n"),
	}
	for name, src := range shouldCatch {
		t.Run(name, func(t *testing.T) {
			violations, found := scanSongGenOrderSrc(t, src)
			if !found {
				t.Fatalf("片段里有 startCoverFetch，门禁却说没找到：\n%s", src)
			}
			if len(violations) == 0 {
				t.Errorf("门禁漏报了这种形状——它正是要堵的：\n%s", src)
			}
		})
	}

	shouldPass := map[string]string{
		"标准形态": wrap("		coverGen := p.NewSongGen()\n" + announce + "		p.startCoverFetch(ops, coverGen, pid)\n"),
		"宣告之前的分支里有别的状态事件": wrap("		switch x {\n		case 1:\n			p.Emit(player.EventStatusUpdate, w)\n			continue\n		}\n" +
			"		g := p.NewSongGen()\n" + announce + "		p.startCoverFetch(ops, g, pid)\n"),
		"先声明后赋值": wrap("		var g uint64\n		g = p.NewSongGen()\n" + announce + "		p.startCoverFetch(ops, g, pid)\n"),
		"宣告之后有别的事件": wrap("		g := p.NewSongGen()\n" + announce + "		p.startCoverFetch(ops, g, pid)\n" +
			"		p.Emit(player.EventLyricIdle, struct{}{})\n"),
	}
	for name, src := range shouldPass {
		t.Run(name, func(t *testing.T) {
			violations, found := scanSongGenOrderSrc(t, src)
			if !found {
				t.Fatalf("片段里有 startCoverFetch，门禁却说没找到：\n%s", src)
			}
			if len(violations) > 0 {
				t.Errorf("门禁误报：%v\n%s", violations, src)
			}
		})
	}

	// 零命中自检：没有 startCoverFetch 的 runSession 不算找到，真文件的测试据此拒绝空转。
	if _, found := scanSongGenOrderSrc(t, wrap("		g := p.NewSongGen()\n"+announce)); found {
		t.Error("没有 startCoverFetch 的片段不该算找到")
	}
}

// scanStandbyInvalidate 扫 runSession 里每个含顶层 Emit(EventClearSongData, …) 调用语句的块：同一块里要有一条
// 顶层的 InvalidateSongGen() 调用语句排在它之前（#146 代码审：只关 K 歌窗口、进程未退时，封面 goroutine 迟到的
// 回写会把已清空的歌曲信息贴回去，同 issue #118）。返回违规描述与找到的块数。
func scanStandbyInvalidate(fset *token.FileSet, f *ast.File) (violations []string, blocks int) {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || fd.Name.Name != "runSession" || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			b, ok := n.(*ast.BlockStmt)
			if !ok {
				return true
			}
			inv := -1
			for i, s := range b.List {
				c := stmtCall(s)
				if c == nil {
					continue
				}
				if calleeName(c) == "InvalidateSongGen" && inv < 0 {
					inv = i
				}
				if calleeName(c) != "Emit" || len(c.Args) == 0 {
					continue
				}
				if sel, ok := c.Args[0].(*ast.SelectorExpr); !ok || sel.Sel.Name != "EventClearSongData" {
					continue
				}
				blocks++
				if inv < 0 { // inv 只在扫到作废语句时赋值，此刻非负即排在前面

					violations = append(violations, fmt.Sprintf("%s: 清空歌曲数据之前没有作废代次（InvalidateSongGen）", fset.Position(s.Pos())))
				}
			}
			return true
		})
	}
	return violations, blocks
}

// TestRunSessionInvalidatesBeforeClear wesing.go 的 runSession：进入待机、清空歌曲数据之前先作废代次。
//
// 变异自证（实测）：删掉待机分支里的 p.InvalidateSongGen() 即红；把它挪到 Emit(EventClearSongData, nil) 之后即红。
func TestRunSessionInvalidatesBeforeClear(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "wesing.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 wesing.go 失败: %v", err)
	}
	violations, blocks := scanStandbyInvalidate(fset, f)
	if blocks == 0 {
		t.Fatal("没有在 runSession 里找到清空歌曲数据的语句——门禁的判据失效了（改名了，或换了写法）")
	}
	for _, v := range violations {
		t.Errorf("%s\n关窗时不作废代次，封面 goroutine 迟到的回写会把已清空的歌曲信息贴回服务端缓存。\n"+
			"修法：在 p.Emit(player.EventClearSongData, nil) 之前 p.InvalidateSongGen()。", v)
	}
}

// TestStandbyInvalidateLintCatchesKnownHoles 钉死门禁自身认得出目标形状。
//
// 变异自证（实测）：作废语句在块里任何位置都算（先扫一遍再判），「作废在清空之后」漏报而红；作废语句在嵌套
// 块里也算（ast.Inspect 整块找），「作废在别的块里」漏报而红。
func TestStandbyInvalidateLintCatchesKnownHoles(t *testing.T) {
	scan := func(body string) ([]string, int) {
		t.Helper()
		src := "package p\nimport \"Metabox-Nexus-PlayerCap/player\"\nfunc (p *P) runSession() {\n	for {\n" + body + "	}\n}\n"
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "snippet.go", src, 0)
		if err != nil {
			t.Fatalf("解析片段失败: %v\n%s", err, src)
		}
		return scanStandbyInvalidate(fset, f)
	}
	const clear = "		p.Emit(player.EventClearSongData, nil)\n"
	for name, body := range map[string]string{
		"没有作废":    "		p.Emit(player.EventStatusUpdate, s)\n" + clear,
		"作废在清空之后": clear + "		p.InvalidateSongGen()\n",
		"作废在别的块里": "		if x {\n			p.InvalidateSongGen()\n		}\n" + clear,
	} {
		if v, n := scan(body); n == 0 || len(v) == 0 {
			t.Errorf("%s：门禁漏报（块 %d 个，违规 %v）", name, n, v)
		}
	}
	if v, n := scan("		p.InvalidateSongGen()\n		p.Emit(player.EventStatusUpdate, s)\n" + clear); n != 1 || len(v) != 0 {
		t.Errorf("标准形态：块 %d 个，违规 %v，want 1 个、无违规", n, v)
	}
	if _, n := scan("		p.Emit(player.EventStatusUpdate, s)\n"); n != 0 {
		t.Errorf("没有清空语句的片段不该算找到，得到 %d 个块", n)
	}
}
