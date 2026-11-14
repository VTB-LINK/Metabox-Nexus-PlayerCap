package wesing

// 本文件钉住 lyric_idle 的条数（#180）：上一次宣告之后至多一条。前半测 idleNotice 本身，后半是 AST 门禁，钉住
// runSession 只经它发出、宣告时复位。
//
// # 为什么用 AST 门禁
//
// runSession 直接调 proc 包，要真进程才能跑，没有离线夹具；做法同 songgenorder_test.go。序列测试只能证明
// idleNotice 对，证明不了 runSession 用了它。
//
// # 它不守什么
//
// 门禁只查形状：本包非测试文件里 player.EventLyricIdle 与字面量 "lyric_idle" 只出现在 idleNotice 的 emit 方法里；
// runSession 里含 startCoverFetch 调用语句的那个语句块（本首宣告所在的块）有一条顶层的 x.announced() 调用语句。
// 经变量转手的事件类型、emit 的调用点在哪、调了几次都不查。调用处传入的须是返回入队结果的 TryEmit：emit 的
// 参数类型带 bool 返回值，传 p.Emit 编译不过，门禁不另查。runSession 的状态机（lastPhase）不在序列测试之内，
// 「会话开始时窗口未开不发」靠 lastPhase 的零值即 PhaseStandby（proc/memory.go 的 PlayPhase）。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestIdleNoticeSequences 按 runSession 的各条路径驱动 idleNotice，逐步核对发不发。
//
// 步骤：a 是宣告（idle.announced()）；p 是 pollLyrics 返回之后的那一处 idle.emit；s 是进入待机
// （lastPhase != PhaseStandby）时的那一处 idle.emit。两处调的是同一个方法，分开写只为对照 runSession。
// 大写的 P、S 是同一处，但事件通道满、这一次投递被丢弃（TryEmit 返回 false）。
//
// 变异自证（实测）：emit 改成不判已发（总是投递），want 里有 0 的十一例红；emit 改成先置已发、再投递
// （#180 代码审之前的写法，不看投递结果），「轮询结束时那条被丢弃，进入待机时补发」「首次被丢弃、第二次入队、
// 第三次不再发」「两处都被丢弃，宣告复位后再关窗口」「未宣告即关窗口被丢弃，重开后仍未宣告又关窗口」四例红；
// announced 改成空函数，「播放结束、换歌：每首一条」「同一首中途更换取数方式」「关窗口后重开、新歌宣告，
// 再关窗口」「未宣告即关窗口，重开后宣告，再关窗口」四例红。
func TestIdleNoticeSequences(t *testing.T) {
	cases := []struct {
		name  string
		steps string
		want  string // 每个 p / s / P / S 步：1 投递并入队，x 投递但被丢弃，0 没有投递
	}{
		// 修复前每个 s 都发（lastPhase 不是待机），以下前四例各连发两条。
		{"播放中关窗口", "aps", "10"},
		{"换歌后加载中关窗口", "aps", "10"},
		{"exitDeadAddr / exitAltAddr / exitPCMRate / exitPCMStalled 之后、重新宣告之前关窗口", "aps", "10"},
		{"换歌后读歌词失败（不宣告）期间关窗口", "aps", "10"},
		// 反例：该发的仍发。
		{"会话开始时加载中、未宣告即关窗口", "s", "1"},
		{"播放结束、换歌：每首一条", "apapap", "111"},
		{"进程退出", "ap", "1"},
		{"同一首中途更换取数方式：重新宣告之后再结束，各一条", "apap", "11"},
		{"关窗口后重开、新歌宣告，再关窗口", "apsaps", "1010"},
		{"未宣告即关窗口，重开后宣告，再关窗口", "saps", "110"},
		// 宣告之间没有新的宣告，就不再发。
		{"关窗口后重开、未宣告又关窗口", "apss", "100"},
		{"未宣告即关窗口，重开后仍未宣告又关窗口", "ss", "10"},
		// 投递被丢弃的不算已发（#180 代码审）。
		{"轮询结束时那条被丢弃，进入待机时补发", "aPs", "x1"},
		{"首次被丢弃、第二次入队、第三次不再发", "aPss", "x10"},
		{"首次被丢弃，宣告复位后再关窗口", "aPaps", "x10"},
		{"两处都被丢弃，宣告复位后再关窗口", "aPSaps", "xx10"},
		{"未宣告即关窗口被丢弃，重开后仍未宣告又关窗口", "Ss", "x1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var n idleNotice
			var got strings.Builder
			for _, st := range c.steps {
				switch st {
				case 'a':
					n.announced()
				case 'p', 's', 'P', 'S':
					enqueue := st == 'p' || st == 's'
					called := false
					n.emit(func(evtType string, data interface{}) bool {
						if evtType != "lyric_idle" {
							t.Errorf("事件类型 %q，want lyric_idle", evtType)
						}
						if _, ok := data.(struct{}); !ok {
							t.Errorf("载荷 %#v，want struct{}{}（序列化为 {} 而非 null）", data)
						}
						called = true
						return enqueue
					})
					switch {
					case !called:
						got.WriteByte('0')
					case enqueue:
						got.WriteByte('1')
					default:
						got.WriteByte('x')
					}
				}
			}
			if got.String() != c.want {
				t.Errorf("步骤 %s：发出 %s，want %s", c.steps, got.String(), c.want)
			}
		})
	}
}

// isIdleNoticeEmit 报告 fd 是不是 idleNotice（或 *idleNotice）的 emit 方法。
func isIdleNoticeEmit(fd *ast.FuncDecl) bool {
	if fd.Recv == nil || len(fd.Recv.List) != 1 || fd.Name.Name != "emit" {
		return false
	}
	typ := fd.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	return isIdent(typ, "idleNotice")
}

// scanLyricIdleSites 返回 f 里 idleNotice.emit 之外引用 lyric_idle 的位置。
func scanLyricIdleSites(fset *token.FileSet, f *ast.File) (violations []string) {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && isIdleNoticeEmit(fd) {
			continue
		}
		ast.Inspect(d, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if x.Sel.Name == "EventLyricIdle" {
					violations = append(violations, fmt.Sprintf("%s: 引用了 EventLyricIdle", fset.Position(x.Pos())))
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING && strings.Trim(x.Value, "`\"") == "lyric_idle" {
					violations = append(violations, fmt.Sprintf("%s: 写了字面量 \"lyric_idle\"", fset.Position(x.Pos())))
				}
			}
			return true
		})
	}
	return violations
}

// scanIdleReset 扫 runSession：含 startCoverFetch 调用语句的块（本首宣告所在的块）里要有一条顶层的 x.announced()
// 调用语句。found 报告是否找到了那个块。
func scanIdleReset(fset *token.FileSet, f *ast.File) (violations []string, found bool) {
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
			cover, reset := -1, false
			for i, s := range b.List {
				if c := stmtCall(s); c != nil {
					switch calleeName(c) {
					case "startCoverFetch":
						cover = i
					case "announced":
						reset = true
					}
				}
			}
			if cover < 0 {
				return true
			}
			found = true
			if !reset {
				violations = append(violations, fmt.Sprintf("%s: 宣告所在的块里没有顶层的 idle.announced()", fset.Position(b.List[cover].Pos())))
			}
			return true
		})
	}
	return violations, found
}

// TestLyricIdleOnlyViaIdleNotice 本包非测试文件里 lyric_idle 只经 idleNotice.emit 发出，runSession 宣告时复位。
//
// 变异自证（实测）：把待机分支里的 idle.emit(p.TryEmit) 改回 p.Emit(player.EventLyricIdle, struct{}{}) 即红；
// pollLyrics 返回之后那一处同样改回即红；删掉 idle.announced() 即红；三处一并撤回（#180 之前的形状）即红。
func TestLyricIdleOnlyViaIdleNotice(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned, sawRunSession := 0, false
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", name, err)
		}
		scanned++
		for _, v := range scanLyricIdleSites(fset, f) {
			t.Errorf("%s\nlyric_idle 绕过 idleNotice 发出，关窗口时会再次连发两条（#180）。修法：改调 idle.emit(p.TryEmit)。", v)
		}
		if name != "wesing.go" {
			continue
		}
		violations, found := scanIdleReset(fset, f)
		if !found {
			t.Fatal("没有在 runSession 里找到含 startCoverFetch 调用语句的块——门禁的判据失效了（改名了，或挪进了分支）")
		}
		sawRunSession = true
		for _, v := range violations {
			t.Errorf("%s\n宣告时不复位，第一次关窗口之后整个会话都不再发 lyric_idle。修法：本首宣告处调 idle.announced()。", v)
		}
	}
	if scanned == 0 || !sawRunSession {
		t.Fatalf("扫了 %d 个文件、找到 wesing.go：%v——门禁空转", scanned, sawRunSession)
	}
}

// TestLyricIdleLintCatchesKnownHoles 钉死门禁自身认得出目标形状。
//
// 变异自证（实测）：scanLyricIdleSites 不看字面量，「字面量」一例漏报而红；放行任何名为 emit 的方法（不看
// 接收者），「别的类型的 emit 方法」一例漏报而红；scanIdleReset 在整个函数体里找 announced，「复位在分支里」
// 一例漏报而红。
func TestLyricIdleLintCatchesKnownHoles(t *testing.T) {
	parse := func(src string) (*token.FileSet, *ast.File) {
		t.Helper()
		src = "package p\nimport \"Metabox-Nexus-PlayerCap/player\"\n" + src
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "snippet.go", src, 0)
		if err != nil {
			t.Fatalf("解析片段失败: %v\n%s", err, src)
		}
		return fset, f
	}
	const emitMethod = "func (n *idleNotice) emit(tryEmit func(string, interface{}) bool) {\n" +
		"	if !n.sent {\n		n.sent = tryEmit(player.EventLyricIdle, struct{}{})\n	}\n}\n"

	for name, src := range map[string]string{
		"待机分支直接 Emit": "func (p *P) runSession() {\n	p.Emit(player.EventClearSongData, nil)\n	p.Emit(player.EventLyricIdle, struct{}{})\n}\n",
		"TryEmit":     "func (p *P) runSession() {\n	p.TryEmit(player.EventLyricIdle, struct{}{})\n}\n",
		"待机分支直接 TryEmit、自行记账": "func (p *P) runSession() {\n	if !sent {\n		sent = p.TryEmit(player.EventLyricIdle, struct{}{})\n	}\n}\n",
		"字面量": "func (p *P) runSession() {\n	p.TryEmit(\"lyric_idle\", struct{}{})\n}\n",
		"别的类型的 emit 方法": "func (o *other) emit(tryEmit func(string, interface{}) bool) {\n" +
			"	o.sent = tryEmit(player.EventLyricIdle, struct{}{})\n}\n",
		"包级变量":           "var ev = player.EventLyricIdle\n",
		"emit 方法之外的同名函数": "func emit() {\n	_ = player.EventLyricIdle\n}\n",
	} {
		fset, f := parse(emitMethod + src)
		if v := scanLyricIdleSites(fset, f); len(v) == 0 {
			t.Errorf("%s：门禁漏报\n%s", name, src)
		}
	}
	if fset, f := parse(emitMethod + "func (n idleNotice) other() {}\n"); len(scanLyricIdleSites(fset, f)) != 0 {
		t.Error("idleNotice.emit 里的引用不该报")
	}

	wrap := func(body string) string {
		return "func (p *P) runSession() {\n	for {\n" + body + "	}\n}\n"
	}
	const cover = "		p.startCoverFetch(ops, g, pid)\n"
	for name, body := range map[string]string{
		"没有复位":   "		g := p.NewSongGen()\n		p.Emit(player.EventStatusUpdate, s)\n" + cover,
		"复位在分支里": "		if x {\n			idle.announced()\n		}\n		g := p.NewSongGen()\n" + cover,
	} {
		fset, f := parse(wrap(body))
		if v, found := scanIdleReset(fset, f); !found || len(v) == 0 {
			t.Errorf("%s：门禁漏报（找到 %v，违规 %v）", name, found, v)
		}
	}
	fset, f := parse(wrap("		g := p.NewSongGen()\n		idle.announced()\n		p.Emit(player.EventStatusUpdate, s)\n" + cover))
	if v, found := scanIdleReset(fset, f); !found || len(v) != 0 {
		t.Errorf("标准形态：找到 %v，违规 %v，want 找到、无违规", found, v)
	}
	fset, f = parse(wrap("		idle.announced()\n"))
	if _, found := scanIdleReset(fset, f); found {
		t.Error("没有 startCoverFetch 的片段不该算找到")
	}
}
