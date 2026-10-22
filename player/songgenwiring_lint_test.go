package player

// 本文件只守一件事：用 BaseEmitter 代次守卫的四家播放器（wesing、cloudmusic、qqmusic、spotify），
// 在 Start 的会话循环里，runSession 返回之后必须先无条件调 InvalidateSongGen()，再发
// clear_song_data（issue #118）。它不测业务逻辑，扫的是四家的源码。
//
// # 它守的是什么
//
// 代次只在换歌（NewSongGen）时前进，会话结束（播放器进程退出）不前进。最后一首歌仍在飞的封面
// goroutine 回来时，捕获的代次仍等于当前代次，EmitForGen 放行，在 clear_song_data 清掉缓存之后把
// 上一首的 song_info_update 贴回去，此后每个新连接经 buildInitEvents 都拿到这份复活的数据
// （修法与来历见 songgen_test.go 里的 TestInvalidateSongGenDropsInFlight）。BaseEmitter 那一半有
// 用例，各播放器是否在会话结束处接上这一调用，此前没有任何门禁。全民K歌就漏过。
//
// # 为什么用 AST 门禁
//
// 四家的 Start 要真进程或真 CDP 才能跑到会话结束，没有离线夹具。AST 扫描不需要编译目标平台。
//
// # 它不守什么
//
// 只查形状：Start 里含 runSession 调用的那个语句块中，其后要有一条不在条件分支里的
// InvalidateSongGen() 调用语句和一条 Emit(…EventClearSongData…) 调用语句，前者排在后者之前；
// 且 runSession 与作废调用之间的语句里（含嵌套，不进函数字面量）不得有 return、break、continue、goto
// 或 Emit(…EventClearSongData…)——它们可能提前清除或让这一轮跳过作废（Codex 审查发现）。这是保守判据，
// 宁可误报。不查是不是同一个接收者，不查 runSession 之前的语句，也不查 panic 之类不经语句的离开方式。
// kugou、sodamusic 靠 context 取消封面回写，applemusic 的封面结果归本次会话，都不在此列。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// songGenPlayers 是用代次守卫的四家，相对 player/ 目录。
var songGenPlayers = []string{
	"wesing/wesing.go",
	"cloudmusic/cloudmusic.go",
	"qqmusic/qqmusic.go",
	"spotify/spotify.go",
}

// callStmt 报告 s 是不是对名为 name 的方法（任意接收者）的调用语句。
func callStmt(s ast.Stmt, name string) (*ast.CallExpr, bool) {
	es, ok := s.(*ast.ExprStmt)
	if !ok {
		return nil, false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return nil, false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return nil, false
	}
	return call, true
}

// emitsClearSongData 报告 s 是不是 Emit(…EventClearSongData…) 调用语句。
func emitsClearSongData(s ast.Stmt) bool {
	call, ok := callStmt(s, "Emit")
	if !ok || len(call.Args) == 0 {
		return false
	}
	sel, ok := call.Args[0].(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "EventClearSongData"
}

// bypassOf 在 s（含嵌套，不进函数字面量，字面量里的 return 不离开外层循环）里找可能提前清除或跳过其后
// 作废调用的语句，返回它的描述；没有则返回空串。
func bypassOf(s ast.Stmt) string {
	why := ""
	ast.Inspect(s, func(n ast.Node) bool {
		if why != "" {
			return false
		}
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			why = "return"
		case *ast.BranchStmt:
			why = x.Tok.String()
		case *ast.ExprStmt:
			if emitsClearSongData(x) {
				why = "clear_song_data"
			}
		}
		return why == ""
	})
	return why
}

// scanSessionEnd 扫一个文件里 Start 方法的会话循环块，返回违规描述，found 报告是否找到了含
// runSession 调用的块（找不到说明门禁的判据失效了）。
func scanSessionEnd(fset *token.FileSet, f *ast.File) (violations []string, found bool) {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || fd.Name.Name != "Start" || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			b, ok := n.(*ast.BlockStmt)
			if !ok {
				return true
			}
			run := -1
			for i, s := range b.List {
				if _, ok := callStmt(s, "runSession"); ok {
					run = i
					break
				}
			}
			if run < 0 {
				return true
			}
			found = true
			inv, clr := -1, -1
			for i := run + 1; i < len(b.List); i++ {
				if inv < 0 {
					if _, ok := callStmt(b.List[i], "InvalidateSongGen"); ok {
						inv = i
					}
				}
				if clr < 0 && emitsClearSongData(b.List[i]) {
					clr = i
				}
			}
			pos := fset.Position(b.List[run].Pos())
			switch {
			case inv < 0:
				violations = append(violations, fmt.Sprintf("%s:%d: runSession 返回之后没有无条件的 InvalidateSongGen() 调用", pos.Filename, pos.Line))
			case clr < 0:
				violations = append(violations, fmt.Sprintf("%s:%d: runSession 返回之后没有 Emit(…EventClearSongData…)", pos.Filename, pos.Line))
			case inv > clr:
				violations = append(violations, fmt.Sprintf("%s:%d: InvalidateSongGen() 排在 clear_song_data 之后", pos.Filename, pos.Line))
			}
			// 作废调用本身在位时，它之前的语句不得有可能提前清除或跳过它的控制流：分支里先清除再 continue、
			// return、break，都会让这一轮会话结束没经过作废。保守判据，宁可误报，误报时把作废挪到它们之前。
			if inv >= 0 {
				for _, s := range b.List[run+1 : inv] {
					if why := bypassOf(s); why != "" {
						violations = append(violations, fmt.Sprintf("%s:%d: runSession 与 InvalidateSongGen() 之间有 %s，可能跳过作废", pos.Filename, fset.Position(s.Pos()).Line, why))
					}
				}
			}
			return true
		})
	}
	return violations, found
}

func scanSessionEndSrc(t *testing.T, src string) (violations []string, found bool) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatalf("解析片段失败: %v\n%s", err, src)
	}
	return scanSessionEnd(fset, f)
}

// TestSessionEndInvalidatesSongGen 四家的会话结束处：先作废代次，再发 clear_song_data。
//
// 变异自证（均实测）：删掉 wesing 的 p.InvalidateSongGen() 即红；把 qqmusic 的 InvalidateSongGen
// 挪到 Emit(EventClearSongData) 之后即红；删掉 cloudmusic 或 spotify 的该调用即红；在 wesing 的
// CloseProc 与作废之间插入 if … { continue } 即红。
func TestSessionEndInvalidatesSongGen(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	for _, rel := range songGenPlayers {
		t.Run(rel, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, filepath.Join(root, "player", filepath.FromSlash(rel)), nil, 0)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			violations, found := scanSessionEnd(fset, f)
			if !found {
				t.Fatalf("没有在 Start 里找到含 runSession 调用的块——门禁的判据失效了（runSession 改名了，或会话循环换了写法）")
			}
			for _, v := range violations {
				t.Errorf("%s\n会话结束时若不先作废代次，最后一首歌仍在飞的封面回写会在 clear_song_data 之后把已清空的歌曲信息贴回缓存。\n"+
					"修法：在 clear_song_data 之前调 p.InvalidateSongGen()。", v)
			}
		})
	}
}

// TestSessionEndLintCatchesKnownHoles 钉死门禁自身认得出目标形状（教训同 cloudmusicanchor_lint_test.go）。
func TestSessionEndLintCatchesKnownHoles(t *testing.T) {
	const head = `package p
import "Metabox-Nexus-PlayerCap/player"
`
	wrap := func(body string) string {
		return head + "func (p *P) Start() {\n	for {\n" + body + "	}\n}\n"
	}

	shouldCatch := map[string]string{
		"没有 InvalidateSongGen": wrap("		p.runSession(c)\n" +
			"		p.Emit(player.EventStatusUpdate, s)\n		p.Emit(player.EventClearSongData, nil)\n"),
		"排在 clear_song_data 之后": wrap("		p.runSession(c)\n" +
			"		p.Emit(player.EventClearSongData, nil)\n		p.InvalidateSongGen()\n"),
		"放在 runSession 之前": wrap("		p.InvalidateSongGen()\n		p.runSession(c)\n" +
			"		p.Emit(player.EventClearSongData, nil)\n"),
		"放在条件分支里": wrap("		p.runSession(c)\n		if x {\n			p.InvalidateSongGen()\n		}\n" +
			"		p.Emit(player.EventClearSongData, nil)\n"),
		"放在 defer 里": wrap("		p.runSession(c)\n		defer p.InvalidateSongGen()\n" +
			"		p.Emit(player.EventClearSongData, nil)\n"),
		"有作废没有 clear_song_data": wrap("		p.runSession(c)\n		p.InvalidateSongGen()\n" +
			"		p.Emit(player.EventStatusUpdate, s)\n"),
		"方法名不对": wrap("		p.runSession(c)\n		p.InvalidateSongGeneration()\n" +
			"		p.Emit(player.EventClearSongData, nil)\n"),
		"clear_song_data 写成字符串字面量": wrap("		p.runSession(c)\n		p.InvalidateSongGen()\n" +
			"		p.Emit(\"clear_song_data\", nil)\n"),
		"分支里先清除再 continue，跳过作废": wrap("		p.runSession(c)\n		if failed {\n			p.Emit(player.EventClearSongData, nil)\n			continue\n		}\n" +
			"		p.InvalidateSongGen()\n		p.Emit(player.EventClearSongData, nil)\n"),
		"作废之前有 return": wrap("		p.runSession(c)\n		if stopped {\n			return\n		}\n		p.InvalidateSongGen()\n" +
			"		p.Emit(player.EventClearSongData, nil)\n"),
		"作废之前有 continue": wrap("		p.runSession(c)\n		if failed {\n			continue\n		}\n		p.InvalidateSongGen()\n" +
			"		p.Emit(player.EventClearSongData, nil)\n"),
		"作废之前有 break": wrap("		p.runSession(c)\n		if failed {\n			break\n		}\n		p.InvalidateSongGen()\n" +
			"		p.Emit(player.EventClearSongData, nil)\n"),
		"作废之前嵌套深处有清除": wrap("		p.runSession(c)\n		if a {\n			if b {\n				p.Emit(player.EventClearSongData, nil)\n			}\n		}\n" +
			"		p.InvalidateSongGen()\n		p.Emit(player.EventClearSongData, nil)\n"),
	}
	for name, src := range shouldCatch {
		t.Run(name, func(t *testing.T) {
			violations, found := scanSessionEndSrc(t, src)
			if !found {
				t.Fatalf("片段里有 runSession，门禁却说没找到：\n%s", src)
			}
			if len(violations) == 0 {
				t.Errorf("门禁漏报了这种形状——它正是要堵的：\n%s", src)
			}
		})
	}

	shouldPass := map[string]string{
		"标准形态": wrap("		p.runSession(c)\n		p.InvalidateSongGen()\n" +
			"		p.Emit(player.EventStatusUpdate, s)\n		p.Emit(player.EventClearSongData, nil)\n"),
		"作废之前夹了别的语句": wrap("		p.runSession(c)\n		proc.CloseProc(h)\n		log.Info(\"x\")\n" +
			"		p.InvalidateSongGen()\n		p.Emit(player.EventClearSongData, nil)\n"),
		"状态更新夹在作废与清除之间": wrap("		p.runSession(c)\n		p.InvalidateSongGen()\n" +
			"		p.Emit(player.EventStatusUpdate, s)\n		p.Emit(player.EventClearSongData, nil)\n"),
		"作废之前只有日志与关闭": wrap("		p.runSession(c)\n		if err := c.Close(); err != nil {\n			log.Warn(\"x\")\n		}\n		p.InvalidateSongGen()\n" +
			"		p.Emit(player.EventClearSongData, nil)\n"),
		"作废之前的函数字面量里有 return": wrap("		p.runSession(c)\n		f := func() {\n			return\n		}\n		f()\n		p.InvalidateSongGen()\n" +
			"		p.Emit(player.EventClearSongData, nil)\n"),
	}
	for name, src := range shouldPass {
		t.Run(name, func(t *testing.T) {
			violations, found := scanSessionEndSrc(t, src)
			if !found {
				t.Fatalf("片段里有 runSession，门禁却说没找到：\n%s", src)
			}
			if len(violations) > 0 {
				t.Errorf("门禁误报：%v\n%s", violations, src)
			}
		})
	}

	// 零命中自检：没有 runSession 的 Start 不算找到，真文件的测试据此拒绝空转。
	if _, found := scanSessionEndSrc(t, wrap("		p.Emit(player.EventClearSongData, nil)\n")); found {
		t.Error("没有 runSession 的片段不该算找到会话循环块")
	}
}
