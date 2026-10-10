package player

// 本文件只测一件事：AST 门禁「cloudmusic 包里带位置的对外事件只许经 runSession 的 emitPos 发出」
// （issue #77）。它不测业务逻辑——扫的是 player/cloudmusic 的源码。
//
// # 它守的是什么
//
// 网易云的位置时钟按 DOM 跳秒对表（doc/design/cloudmusic-dom-clock-sync-design.md）。校准改动了时钟而前端
// 尚不知道的量记在 untold 里，达到门槛就另发一条 playback_resume；任何带位置的对外事件（all_lyrics、
// lyric_update、playback_pause、playback_resume）发出时前端已据它落锚，这个量要清零。清零写在 emitPos
// 里。绕过 emitPos 直接 Emit 一条，量不清零，随后会多发一条由校准触发的 playback_resume，且与那条
// 事件同拍或紧随其后——下游重复重置歌词时钟（AGENTS.md §3.2）。
//
// # 为什么用 AST 门禁
//
// runSession 的轮询循环没有离线夹具（要真 CDP 连接），离线回放用例自己决定何时清零，守不住真实的
// 发出点。AST 扫描不需要编译目标平台，本文件纯 Go。
//
// # 它不守什么
//
// 只查形状：四种事件的常量与字面量只许作 emitPos 调用的第一个实参；Emit / EmitForGen 的事件类型在
// emitPos 之外必须是 EventXxx 常量；emitPos 的函数体里要有对 anchored 的调用。emitPos 之内先发后清
// 还是先清后发、调用点传的位置取的是哪个读数，不查。不做类型解析，事件类型经别的包转手的查不出。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// anchorEvents 是带位置的对外事件：常量名 → 取值。
var anchorEvents = map[string]string{
	"EventAllLyrics":      EventAllLyrics,
	"EventLyricUpdate":    EventLyricUpdate,
	"EventPlaybackPause":  EventPlaybackPause,
	"EventPlaybackResume": EventPlaybackResume,
}

// TestCloudmusicPositionedEventsGoThroughEmitPos cloudmusic 包强制：四种带位置的对外事件只经 emitPos 发出。
//
// 变异自证（均实测）：把 runSession 里任一处 emitPos 调用改回 p.Emit——行号变化的 lyric_update、
// playback_pause、状态变化段的 playback_resume、seek 检测的 playback_resume、由校准触发的
// playback_resume、换歌分支的 all_lyrics 各试一处——即红；删掉 emitPos 里的 pending.anchored() 即红。
// 非换歌拍补发的歌词装在闭包（lateLyrics）里、推迟到对表之后发出，闭包里的发出点同样拦得住：超时兜底的
// lyric_update、Redux 歌词晚到的 all_lyrics 与 lyric_update 各改回 p.Emit 即红。
func TestCloudmusicPositionedEventsGoThroughEmitPos(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	dir := filepath.Join(root, "player", "cloudmusic")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", name, err)
		}
		files = append(files, f)
	}

	violations, sent := scanAnchorEmits(fset, files)
	if len(violations) > 0 {
		t.Errorf("player/cloudmusic 里有带位置的对外事件没有经 emitPos 发出：\n  %s\n\n"+
			"绕过 emitPos，未告知量不清零，随后会多发一条由校准触发的 playback_resume。\n"+
			"修法：改成 emitPos(player.EventXxx, 载荷)。",
			strings.Join(violations, "\n  "))
	}
	// 零命中自检：四种事件各自至少有一处经 emitPos 发出，否则上面的断言可能是空转。
	for name := range anchorEvents {
		if sent[name] == 0 {
			t.Errorf("没有找到经 emitPos 发出的 %s——门禁的判据失效了（emitPos 改名了，或发出点换了写法）", name)
		}
	}
}

// TestCloudmusicAnchorLintCatchesKnownHoles 钉死门禁自身认得出目标形状（教训同 callback_lint_test.go）。
func TestCloudmusicAnchorLintCatchesKnownHoles(t *testing.T) {
	const head = `package cloudmusic
import "Metabox-Nexus-PlayerCap/player"
`
	const emitPos = `	emitPos := func(evtType string, data interface{}) {
		p.Emit(evtType, data)
		pending.anchored()
	}
`
	wrap := func(body string) string { return head + "func (p *P) run() {\n" + emitPos + body + "}\n" }

	shouldCatch := map[string]string{
		"直接 Emit lyric_update":     wrap(`	p.Emit(player.EventLyricUpdate, x)` + "\n"),
		"直接 Emit playback_pause":   wrap(`	p.Emit(player.EventPlaybackPause, x)` + "\n"),
		"直接 Emit playback_resume":  wrap(`	p.Emit(player.EventPlaybackResume, x)` + "\n"),
		"直接 Emit all_lyrics":       wrap(`	p.Emit(player.EventAllLyrics, x)` + "\n"),
		"经 EmitForGen 发":           wrap(`	p.EmitForGen(gen, player.EventAllLyrics, x)` + "\n"),
		"事件类型写成字符串字面量":             wrap(`	p.Emit("playback_pause", x)` + "\n"),
		"事件类型先存进变量":                wrap("	evt := player.EventLyricUpdate\n	p.Emit(evt, x)\n"),
		"事件类型是来历不明的变量":             wrap("	p.Emit(evt, x)\n"),
		"另起一个不清零的包装函数":             wrap("	send := func(t string, d interface{}) { p.Emit(t, d) }\n	send(player.EventLyricUpdate, x)\n"),
		"直接写事件通道":                  wrap("	p.EventCh <- player.Event{Type: player.EventPlaybackResume}\n"),
		"常量作 emitPos 的第二个实参":       wrap("	emitPos(player.EventStatusUpdate, player.EventLyricUpdate)\n"),
		"emitPos 里没有调 anchored":    head + "func (p *P) run() {\n	emitPos := func(evtType string, data interface{}) { p.Emit(evtType, data) }\n	emitPos(player.EventLyricUpdate, x)\n}\n",
		"没有 emitPos，四种事件直接发":       head + "func (p *P) run() {\n	p.Emit(player.EventAllLyrics, x)\n}\n",
		"别名 import":                "package cloudmusic\nimport base \"Metabox-Nexus-PlayerCap/player\"\nfunc (p *P) run() {\n" + emitPos + "	p.Emit(base.EventLyricUpdate, x)\n}\n",
		"另一个函数里直接发（emitPos 在别处定义）": wrap("") + "func (p *P) other() { p.Emit(player.EventPlaybackPause, x) }\n",
	}
	for name, src := range shouldCatch {
		t.Run(name, func(t *testing.T) {
			if got, _ := scanAnchorSrc(t, src); len(got) == 0 {
				t.Errorf("门禁漏报了这种形状——它正是要堵的：\n%s", src)
			}
		})
	}

	shouldPass := map[string]string{
		"四种事件都经 emitPos": wrap("	emitPos(player.EventAllLyrics, a)\n	emitPos(player.EventLyricUpdate, b)\n" +
			"	emitPos(player.EventPlaybackPause, c)\n	emitPos(player.EventPlaybackResume, d)\n"),
		"不带位置的事件照常直接发": wrap("	p.Emit(player.EventStatusUpdate, s)\n	p.Emit(player.EventClearSongData, nil)\n" +
			"	p.EmitForGen(gen, player.EventSongInfoUpdate, info)\n"),
		"位置采样不经 emitPos": wrap("	p.EmitPositionSample(now, pos, dur)\n"),
	}
	for name, src := range shouldPass {
		t.Run(name, func(t *testing.T) {
			if got, _ := scanAnchorSrc(t, src); len(got) > 0 {
				t.Errorf("门禁误报：%v\n%s", got, src)
			}
		})
	}

	// 零命中自检用的计数：经 emitPos 发出的才计。
	_, sent := scanAnchorSrc(t, wrap("	emitPos(player.EventLyricUpdate, b)\n	p.Emit(player.EventStatusUpdate, s)\n"))
	if sent["EventLyricUpdate"] != 1 || sent["EventAllLyrics"] != 0 {
		t.Errorf("经 emitPos 发出的计数不对：%v", sent)
	}
}

func scanAnchorSrc(t *testing.T, src string) ([]string, map[string]int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatalf("解析测试源码失败: %v", err)
	}
	return scanAnchorEmits(fset, []*ast.File{f})
}

// scanAnchorEmits 返回违规位置（已排序），以及四种事件各有几处经 emitPos 发出。
func scanAnchorEmits(fset *token.FileSet, files []*ast.File) (violations []string, sent map[string]int) {
	sent = map[string]int{}
	report := func(pos token.Pos, why string) {
		violations = append(violations, fset.Position(pos).String()+" "+why)
	}

	// 第一遍：找 emitPos 的函数字面量，以及各处 emitPos 调用的第一个实参。
	var lits []*ast.FuncLit
	allowed := map[ast.Expr]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == "emitPos" && i < len(n.Rhs) {
						if lit, ok := n.Rhs[i].(*ast.FuncLit); ok {
							lits = append(lits, lit)
						}
					}
				}
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "emitPos" && len(n.Args) > 0 {
					allowed[n.Args[0]] = true
				}
			}
			return true
		})
	}
	for _, lit := range lits {
		anchored := false
		ast.Inspect(lit.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && calleeName(call.Fun) == "anchored" {
				anchored = true
			}
			return true
		})
		if !anchored {
			report(lit.Pos(), "emitPos 的函数体里没有调用 anchored")
		}
	}
	if len(lits) > 1 {
		report(lits[1].Pos(), "emitPos 定义了不止一处")
	}
	inEmitPos := func(pos token.Pos) bool {
		for _, lit := range lits {
			if pos >= lit.Pos() && pos < lit.End() {
				return true
			}
		}
		return false
	}

	// 第二遍：四种事件的常量、字面量只许作 emitPos 的第一个实参；emitPos 之外的 Emit / EmitForGen
	// 事件类型必须是 EventXxx 常量。
	eventConst := func(e ast.Expr) string {
		switch e := e.(type) {
		case *ast.Ident:
			return e.Name
		case *ast.SelectorExpr:
			return e.Sel.Name
		}
		return ""
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if _, ok := anchorEvents[n.Sel.Name]; ok {
					if allowed[n] {
						sent[n.Sel.Name]++
					} else {
						report(n.Pos(), n.Sel.Name+" 没有作为 emitPos 的第一个实参")
					}
					return false // 不再下钻到 Sel，免得同一处报两次
				}
			case *ast.Ident:
				if _, ok := anchorEvents[n.Name]; ok {
					if allowed[n] {
						sent[n.Name]++
					} else {
						report(n.Pos(), n.Name+" 没有作为 emitPos 的第一个实参")
					}
				}
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					if s, err := strconv.Unquote(n.Value); err == nil {
						for name, v := range anchorEvents {
							if s == v {
								report(n.Pos(), "字符串字面量 "+n.Value+"（即 "+name+"）")
							}
						}
					}
				}
			case *ast.CallExpr:
				argIdx := -1
				switch calleeName(n.Fun) {
				case "Emit":
					argIdx = 0
				case "EmitForGen":
					argIdx = 1
				}
				if argIdx >= 0 && argIdx < len(n.Args) && !inEmitPos(n.Pos()) {
					if _, isLit := n.Args[argIdx].(*ast.BasicLit); !isLit && !strings.HasPrefix(eventConst(n.Args[argIdx]), "Event") {
						report(n.Pos(), "emitPos 之外的 "+calleeName(n.Fun)+" 事件类型不是 EventXxx 常量")
					}
				}
			}
			return true
		})
	}
	sort.Strings(violations)
	return violations, sent
}
