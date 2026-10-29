package player

// 本文件只测一件事：AST 门禁「kuwo 包里带位置的对外事件只许经 session.emitPos 发出」（issue #131）。
// 它不测业务逻辑——扫的是 player/kuwo 的源码。
//
// # 它守的是什么
//
// 酷我的拖动偏差修正改变对外位置之后，session 按前端模型判断前端是否与对外位置相差 200 ms 以上，是则按
// 跳变另发一条 playback_resume（doc/design/kuwo-seek-lag-design.md §3.3「通知」）。前端模型只随入队的
// 四种带位置的对外事件（all_lyrics、lyric_update、playback_pause、playback_resume）落锚，更新写在
// emitPos 调用的 anchored 里。绕过 emitPos 直接 Emit / TryEmit 一条，模型不随之落锚，随后会按过时的
// 锚点多发或漏发由拖动偏差修正触发的 playback_resume。
//
// 判据与 cloudmusic 的门禁相同（scanAnchorEmits，见 cloudmusicanchor_lint_test.go），区别有二：kuwo 的
// emitPos 是 session 的方法；session.anchored 按事件类型更新模型（all_lyrics、playback_resume 开始插值，
// playback_pause 停止），四种事件的常量可以作它的 switch 的 case 标签，case 标签不发出任何事件。
// emitPos 只在 TryEmit 入队成功时才调 anchored、anchored 里各事件的语义，门禁不查，由
// player/kuwo/lagnotify_test.go 守。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestKuwoPositionedEventsGoThroughEmitPos kuwo 包强制：四种带位置的对外事件只经 emitPos 发出。
//
// 变异自证（均实测）：把 session.go 里 observe 的 playback_resume 与 playback_pause、emitAll 的 all_lyrics、
// emitLine 的两处 lyric_update 各改回 s.p.Emit 即红；observe 改回先把事件类型存进变量再发即红；删掉
// emitPos 里的 s.anchored(evtType) 即红。
func TestKuwoPositionedEventsGoThroughEmitPos(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	fset, files := parseNonTestFiles(t, filepath.Join(root, "player", "kuwo"))
	violations, sent := scanKuwoAnchorEmits(fset, files)
	if len(violations) > 0 {
		t.Errorf("player/kuwo 里有带位置的对外事件没有经 emitPos 发出：\n  %s\n\n"+
			"绕过 emitPos，前端模型不随之落锚，随后会按过时的锚点多发或漏发由拖动偏差修正触发的 playback_resume。\n"+
			"修法：改成 s.emitPos(player.EventXxx, 载荷)。",
			strings.Join(violations, "\n  "))
	}
	// 零命中自检：四种事件各自至少有一处经 emitPos 发出，否则上面的断言可能是空转。
	for name := range anchorEvents {
		if sent[name] == 0 {
			t.Errorf("没有找到经 emitPos 发出的 %s——门禁的判据失效了（emitPos 改名了，或发出点换了写法）", name)
		}
	}
}

// scanKuwoAnchorEmits 即 scanAnchorEmits，只放过 session.anchored（带接收者、名为 anchored 的方法）
// 里 switch 的 case 标签中的四种事件常量。
func scanKuwoAnchorEmits(fset *token.FileSet, files []*ast.File) ([]string, map[string]int) {
	violations, sent := scanAnchorEmits(fset, files)
	var labels []string
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Name.Name != "anchored" || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if cc, ok := n.(*ast.CaseClause); ok {
					for _, e := range cc.List {
						if sel, ok := e.(*ast.SelectorExpr); ok {
							if _, ok := anchorEvents[sel.Sel.Name]; ok {
								labels = append(labels, fset.Position(sel.Pos()).String()+" ")
							}
						}
					}
				}
				return true
			})
		}
	}
	var kept []string
	for _, v := range violations {
		label := false
		for _, l := range labels {
			label = label || strings.HasPrefix(v, l)
		}
		if !label {
			kept = append(kept, v)
		}
	}
	return kept, sent
}

func scanKuwoAnchorSrc(t *testing.T, src string) ([]string, map[string]int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatalf("解析测试源码失败: %v", err)
	}
	return scanKuwoAnchorEmits(fset, []*ast.File{f})
}

// TestKuwoAnchorLintCatchesKnownHoles 钉死门禁认得出方法形式的 emitPos 与 TryEmit，且只放过 anchored 的
// case 标签。
//
// 变异自证（均实测）：scanAnchorEmits 不收集名为 emitPos 的 FuncDecl、或只认 emitPos(…) 不认 x.emitPos(…)，
// 「四种事件都经 s.emitPos」误报；第二遍不查 TryEmit，「TryEmit 的事件类型是来历不明的变量」漏报；不查
// anchored、不查定义不止一处，「emitPos 方法体里没有调 anchored」「emitPos 方法定义了两处」各自漏报；
// scanKuwoAnchorEmits 不放过 case 标签，「anchored 按事件类型更新模型」误报；放过 anchored 里的全部常量、
// 或任何函数里的 case 标签，「anchored 里直接 Emit」「别的函数里 case 标签」各自漏报。
func TestKuwoAnchorLintCatchesKnownHoles(t *testing.T) {
	const head = `package kuwo
import "Metabox-Nexus-PlayerCap/player"
`
	const emitPos = `func (s *session) emitPos(evtType string, data interface{}) {
	if s.p.TryEmit(evtType, data) {
		s.anchored(evtType)
	}
}
`
	const anchored = `func (s *session) anchored(evtType string) {
	switch evtType {
	case player.EventAllLyrics, player.EventPlaybackResume:
		s.running = true
	case player.EventPlaybackPause:
		s.running = false
	}
}
`
	wrap := func(body string) string { return head + emitPos + "func (s *session) observe() {\n" + body + "}\n" }

	shouldCatch := map[string]string{
		"直接 Emit all_lyrics":         wrap("	s.p.Emit(player.EventAllLyrics, x)\n"),
		"直接 Emit lyric_update":       wrap("	s.p.Emit(player.EventLyricUpdate, x)\n"),
		"直接 Emit playback_pause":     wrap("	s.p.Emit(player.EventPlaybackPause, x)\n"),
		"直接 Emit playback_resume":    wrap("	s.p.Emit(player.EventPlaybackResume, x)\n"),
		"直接 TryEmit all_lyrics":      wrap("	s.p.TryEmit(player.EventAllLyrics, x)\n"),
		"直接 TryEmit lyric_update":    wrap("	s.p.TryEmit(player.EventLyricUpdate, x)\n"),
		"直接 TryEmit playback_pause":  wrap("	s.p.TryEmit(player.EventPlaybackPause, x)\n"),
		"直接 TryEmit playback_resume": wrap("	s.p.TryEmit(player.EventPlaybackResume, x)\n"),
		"事件类型先存进变量再经 emitPos 发":      wrap("	event := player.EventPlaybackPause\n	if seek {\n		event = player.EventPlaybackResume\n	}\n	s.emitPos(event, x)\n"),
		"事件类型先存进变量再经 Emit 发":         wrap("	event := player.EventLyricUpdate\n	s.p.Emit(event, x)\n"),
		"TryEmit 的事件类型是来历不明的变量":      wrap("	s.p.TryEmit(evt, x)\n"),
		"emitPos 方法体里没有调 anchored": head + "func (s *session) emitPos(evtType string, data interface{}) { s.p.TryEmit(evtType, data) }\n" +
			"func (s *session) observe() {\n	s.emitPos(player.EventLyricUpdate, x)\n}\n",
		"emitPos 方法定义了两处": wrap("	s.emitPos(player.EventLyricUpdate, x)\n") +
			"func (o *other) emitPos(evtType string, data interface{}) {\n	o.p.TryEmit(evtType, data)\n	o.anchored(evtType)\n}\n",
		"anchored 里直接 Emit": wrap("	s.emitPos(player.EventLyricUpdate, x)\n") +
			"func (s *session) anchored(evtType string) {\n	s.p.Emit(player.EventPlaybackPause, x)\n}\n",
		"别的函数里 case 标签": wrap("	switch evt {\n	case player.EventPlaybackPause:\n		s.emitPos(evt, x)\n	}\n"),
	}
	for name, src := range shouldCatch {
		t.Run(name, func(t *testing.T) {
			if got, _ := scanKuwoAnchorSrc(t, src); len(got) == 0 {
				t.Errorf("门禁漏报了这种形状——它正是要堵的：\n%s", src)
			}
		})
	}

	shouldPass := map[string]string{
		"四种事件都经 s.emitPos": wrap("	s.emitPos(player.EventAllLyrics, a)\n	s.emitPos(player.EventLyricUpdate, b)\n" +
			"	s.emitPos(player.EventPlaybackPause, c)\n	s.emitPos(player.EventPlaybackResume, d)\n"),
		"不带位置的事件照常直接发": wrap("	s.p.Emit(player.EventStatusUpdate, st)\n	s.p.Emit(player.EventClearSongData, nil)\n" +
			"	s.p.TryEmit(player.EventSongInfoUpdate, info)\n"),
		"位置采样不经 emitPos":     wrap("	s.p.EmitPositionSample(now, position, s.duration)\n"),
		"anchored 按事件类型更新模型": wrap("	s.emitPos(player.EventLyricUpdate, b)\n") + anchored,
	}
	for name, src := range shouldPass {
		t.Run(name, func(t *testing.T) {
			if got, _ := scanKuwoAnchorSrc(t, src); len(got) > 0 {
				t.Errorf("门禁误报：%v\n%s", got, src)
			}
		})
	}

	// 零命中自检用的计数：经方法形式的 emitPos 发出的才计，anchored 的 case 标签不计。
	_, sent := scanKuwoAnchorSrc(t, wrap("	s.emitPos(player.EventPlaybackResume, d)\n	s.p.Emit(player.EventStatusUpdate, st)\n")+anchored)
	if sent["EventPlaybackResume"] != 1 || sent["EventAllLyrics"] != 0 {
		t.Errorf("经 emitPos 发出的计数不对：%v", sent)
	}
}
