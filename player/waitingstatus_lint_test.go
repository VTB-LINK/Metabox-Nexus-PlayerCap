package player

// 本文件只守一件事：播放器包里的 waiting_process 状态一律经 EmitStatusIfChanged 发出，不得经 Emit
// 直接发（issue #119）。它不测业务逻辑，扫的是各播放器包的源码。
//
// # 它守的是什么
//
// 没运行的播放器，外层循环每轮都要声明一次「未运行」。经 Emit 直接发，订阅者每 2～3 秒收到一条
// 同样的状态，还会每次刷新该通道的最后事件时刻，使无活跃自动隐藏（<player>-idle-hide）在进程未运行
// 期间永远不触发。EmitStatusIfChanged 在内容没变时不发。
//
// # 为什么用 AST 门禁
//
// 各播放器的 Start 要真进程才能跑到这些分支，没有离线夹具。
//
// # 它不守什么
//
// 只查 Status 为 "waiting_process" 的复合字面量必须出现在 EmitStatusIfChanged 的实参里。别的状态
// （waiting_song、standby 等）不在此列；字面量以外的构造方式（先存进别的变量再改 Status）查不出。

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

// isWaitingProcess 报告 n 是不是 Status: "waiting_process" 的键值对。
func isWaitingProcess(n ast.Node) bool {
	kv, ok := n.(*ast.KeyValueExpr)
	if !ok {
		return false
	}
	key, ok := kv.Key.(*ast.Ident)
	if !ok || key.Name != "Status" {
		return false
	}
	lit, ok := kv.Value.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(lit.Value)
	return err == nil && s == "waiting_process"
}

// scanWaitingEmits 返回违规（Status 为 waiting_process 的字面量不在 EmitStatusIfChanged 的实参里）与
// 经 EmitStatusIfChanged 发出的个数。
func scanWaitingEmits(fset *token.FileSet, f *ast.File) (violations []string, viaGate int) {
	allowed := map[ast.Node]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "EmitStatusIfChanged" {
			return true
		}
		for _, arg := range call.Args {
			ast.Inspect(arg, func(m ast.Node) bool {
				if isWaitingProcess(m) {
					allowed[m] = true
					viaGate++
				}
				return true
			})
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		if isWaitingProcess(n) && !allowed[n] {
			violations = append(violations, fset.Position(n.Pos()).String())
		}
		return true
	})
	return violations, viaGate
}

func scanWaitingSrc(t *testing.T, src string) (violations []string, viaGate int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatalf("解析片段失败: %v\n%s", err, src)
	}
	return scanWaitingEmits(fset, f)
}

// TestWaitingProcessGoesThroughStatusGate 各播放器包里 waiting_process 一律经 EmitStatusIfChanged。
//
// 变异自证（均实测）：把任一处 p.EmitStatusIfChanged(…) 改回 p.Emit(player.EventStatusUpdate, …)——
// applemusic 的两处、qqmusic、sodamusic、spotify、wesing 各试一处——即红。
func TestWaitingProcessGoesThroughStatusGate(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	dirs, err := os.ReadDir(filepath.Join(root, "player"))
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	var violations []string
	gatePackages := map[string]int{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, "player", d.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读目录失败: %v", err)
		}
		for _, e := range files {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatalf("解析 %s 失败: %v", name, err)
			}
			v, n := scanWaitingEmits(fset, f)
			violations = append(violations, v...)
			if n > 0 {
				gatePackages[d.Name()] += n
			}
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("下面这些 waiting_process 状态没有经 EmitStatusIfChanged 发出：\n  %s\n\n"+
			"直接 Emit 会让订阅者每轮都收到同样的状态，并使无活跃自动隐藏在进程未运行期间永远不触发。\n"+
			"修法：改成 p.EmitStatusIfChanged(&player.StatusInfo{…})。",
			strings.Join(violations, "\n  "))
	}
	// 零命中自检：认出的播放器包少于 7 个，说明门禁的判据失效了（方法改名了，或发出点换了写法）。
	if len(gatePackages) < 7 {
		t.Errorf("经 EmitStatusIfChanged 发 waiting_process 的播放器包只认出 %d 个（%v），应至少 7 个——门禁的判据失效了",
			len(gatePackages), gatePackages)
	}
}

// TestWaitingStatusLintCatchesKnownHoles 钉死门禁自身认得出目标形状（教训同 cloudmusicanchor_lint_test.go）。
func TestWaitingStatusLintCatchesKnownHoles(t *testing.T) {
	const head = `package p
import "Metabox-Nexus-PlayerCap/player"
`
	wrap := func(body string) string { return head + "func (p *P) Start() {\n" + body + "}\n" }

	shouldCatch := map[string]string{
		"直接 Emit":                     wrap(`	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_process", Reason: player.ReasonProcessNotRunning})` + "\n"),
		"先存进变量再 Emit":                 wrap("	info := &player.StatusInfo{Status: \"waiting_process\"}\n	p.Emit(player.EventStatusUpdate, info)\n"),
		"经 EmitForGen 之类别的出口":         wrap(`	p.EmitForGen(gen, player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_process"})` + "\n"),
		"字面量在 EmitStatusIfChanged 之外": wrap("	p.EmitStatusIfChanged(&player.StatusInfo{Status: \"standby\"})\n	x := player.StatusInfo{Status: \"waiting_process\"}\n	_ = x\n"),
	}
	for name, src := range shouldCatch {
		t.Run(name, func(t *testing.T) {
			if got, _ := scanWaitingSrc(t, src); len(got) == 0 {
				t.Errorf("门禁漏报了这种形状——它正是要堵的：\n%s", src)
			}
		})
	}

	shouldPass := map[string]string{
		"经 EmitStatusIfChanged": wrap(`	p.EmitStatusIfChanged(&player.StatusInfo{Status: "waiting_process", Reason: player.ReasonProcessNotRunning})` + "\n"),
		"别的状态照常 Emit": wrap(`	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby"})` + "\n" +
			`	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "waiting_song"})` + "\n"),
		"Status 不是字面量": wrap("	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: s})\n"),
	}
	for name, src := range shouldPass {
		t.Run(name, func(t *testing.T) {
			if got, _ := scanWaitingSrc(t, src); len(got) > 0 {
				t.Errorf("门禁误报：%v\n%s", got, src)
			}
		})
	}

	// 零命中自检用的计数：经 EmitStatusIfChanged 发出的才计。
	_, n := scanWaitingSrc(t, wrap(`	p.EmitStatusIfChanged(&player.StatusInfo{Status: "waiting_process"})`+"\n"+
		`	p.Emit(player.EventStatusUpdate, &player.StatusInfo{Status: "standby"})`+"\n"))
	if n != 1 {
		t.Errorf("经 EmitStatusIfChanged 发出的计数不对：%d", n)
	}
}
