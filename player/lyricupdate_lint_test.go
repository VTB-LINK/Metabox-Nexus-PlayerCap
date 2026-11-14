package player

// 本文件只测一件事：全仓 AST 门禁「LyricUpdate 字面量写了 Index: -1 就必须写 Position 与 Progress」。
// 它不测 player 包的业务逻辑——扫的是整个仓库的源码。
//
// # 为什么这条要用 AST 门禁，而不是单元测试
//
// 发出 index:-1 的包（cloudmusic / kugou / qqmusic / sodamusic / applemusic）都是 Windows-only
// （AGENTS.md §8：GOOS=linux go vet 全部 FAIL）。AST 扫描不需要编译目标平台，一条规则就覆盖
// 全仓的字面量，包括以后新增的调用点。
//
// # 它守的是什么
//
// Progress 的契约是「整首播到哪」。纯音乐时歌照样在走，progress 不该是 0。
// 但 cloudmusic 与 kugou 的 index:-1 字面量此前都漏了这个字段，Go 零值兜出一个 0，
// 而同一时刻它们自己发的 all_lyrics.progress 是实时值——**同一个事件对里自相矛盾**。
// qqmusic 写对了，于是这三家给出两种答案（实测：qqmusic 0.125 / kugou 0）。
//
// 更糟的是这个零值被当成契约写进了对外文档：openapi.yaml 四处声称「index:-1 时
// progress 为 0」。写文档的人照着 cloudmusic 看的，而 cloudmusic 只是漏了字段。
// 一个漏写的字段，先变成 bug，再变成文档，最后变成下游据以实现的假契约。
//
// Position 同理：它是整曲实时位置（= progress × duration），每条 lyric_update 都带、与 index 无关。
// 门禁起初只查 Progress，server 的无活跃隐藏清行（notifyPerPlayerIdleClear）于是只写了 Progress，
// 对外发出 position=0 而 progress 是停下时的值（issue #176）。
//
// # 为什么盯 Index: -1 而不是所有 LyricUpdate
//
// 常规歌词行由 BuildLyricUpdate 统一构造（40e77af 把易错的语义锁在了一处），
// 不会漏字段。index:-1 是各家各自手写字面量的地方——手写就是漏字段的温床。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLyricUpdateIndexMinusOneHasPositionProgress 全仓强制：写 Index: -1 的 LyricUpdate 字面量必须带
// Position 与 Progress。
//
// 变异自证：把 player/kugou/kugou.go 里 index:-1 那处的 `Progress: progress` 删掉即红；
// 把 server/server.go 的 notifyPerPlayerIdleClear 里 `Position: position` 删掉即红（实测）。
func TestLyricUpdateIndexMinusOneHasPositionProgress(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	violations, err := scanLyricUpdateViolations(root)
	if err != nil {
		t.Fatalf("扫描源码失败: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("发现 %d 处 LyricUpdate{Index: -1} 漏写 Position 或 Progress（Go 零值会兜出 0，"+
			"而同一时刻 all_lyrics 的 position/progress 是实时值——同一个事件对自相矛盾，"+
			"且下游据此以为「纯音乐时进度条归零」）：\n  %s\n\n"+
			"修法：position 与 progress 通常在上面几行就算好了（all_lyrics 已经带上它们），"+
			"补 `Position: position, Progress: progress` 即可。参照 player/qqmusic/qqmusic.go 的 emitNoLyrics。",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// TestLyricUpdateLintCatchesKnownHoles 钉死门禁自身能抓住目标形状。
//
// 照抄 callback_lint_test.go 的教训：**一个抓不住目标形状的门禁比没有门禁更糟**——
// 它给人已经守住了的错觉。下面每种形状都是真实出现过或极可能出现的写法。
//
// 变异自证（均实测）：判定改成只查 Progress、只查 Position、两个都缺才报，各有应报的用例红。
func TestLyricUpdateLintCatchesKnownHoles(t *testing.T) {
	shouldCatch := map[string]string{
		// cloudmusic.go:385 与 kugou.go:443 修复前的原样
		"取地址 + 包名限定（三家的真实写法）": `package p
import "Metabox-Nexus-PlayerCap/player"
func f(playTime float32) {
	emit(&player.LyricUpdate{Index: -1, Text: "", Timestamp: 0, PlayTime: playTime})
}`,
		// 同包内可能省掉包名
		"包内无限定名": `package player
func f(playTime float32) {
	emit(&LyricUpdate{Index: -1, Text: "", PlayTime: playTime})
}`,
		// 不取地址
		"值字面量": `package p
import "Metabox-Nexus-PlayerCap/player"
func f() {
	use(player.LyricUpdate{Index: -1, Text: ""})
}`,
		// 字段顺序不同不该影响判定
		"Progress 缺失但字段乱序": `package p
import "Metabox-Nexus-PlayerCap/player"
func f(playTime float32) {
	emit(&player.LyricUpdate{PlayTime: playTime, Text: "", Index: -1})
}`,
		// server/server.go notifyPerPlayerIdleClear 修复前的原样（issue #176）：server 包经类型别名用裸名
		"只带 Progress、缺 Position": `package server
func f(progress float32) {
	use(&LyricUpdate{Index: -1, Progress: progress})
}`,
		"只带 Position、缺 Progress": `package p
import "Metabox-Nexus-PlayerCap/player"
func f(pos float32) {
	emit(&player.LyricUpdate{Index: -1, Text: "", Position: pos})
}`,
		"Position 缺失但字段乱序": `package p
import "Metabox-Nexus-PlayerCap/player"
func f(progress float32) {
	emit(&player.LyricUpdate{Progress: progress, Text: "", Index: -1})
}`,
		// 嵌在别的字面量里（server 的 WSEvent{Data: &LyricUpdate{...}}）
		"嵌在外层字面量里": `package server
func f(name string, progress float32) {
	use(WSEvent{Type: "lyric_update", Player: name, Data: &LyricUpdate{Index: -1, Progress: progress}})
}`,
	}
	for name, src := range shouldCatch {
		t.Run(name, func(t *testing.T) {
			if got := scanSrc(t, src); len(got) == 0 {
				t.Errorf("门禁漏报了这种形状——它正是要堵的：\n%s", src)
			}
		})
	}

	shouldPass := map[string]string{
		"两个都带（修复后的样子）": `package p
import "Metabox-Nexus-PlayerCap/player"
func f(playTime, pos, progress float32) {
	emit(&player.LyricUpdate{Index: -1, Text: "", PlayTime: playTime, Position: pos, Progress: progress})
}`,
		"两个都带、字段乱序": `package server
func f(pos, progress float32) {
	use(&LyricUpdate{Progress: progress, Position: pos, Index: -1})
}`,
		// 常规歌词行走 BuildLyricUpdate，不是本门禁的目标
		"非 -1 的 Index 不管": `package p
import "Metabox-Nexus-PlayerCap/player"
func f(playTime float32) {
	emit(&player.LyricUpdate{Index: 3, Text: "词", PlayTime: playTime})
}`,
		"别的结构体不管": `package p
type Other struct{ Index int }
func f() { use(Other{Index: -1}) }`,
	}
	for name, src := range shouldPass {
		t.Run(name, func(t *testing.T) {
			if got := scanSrc(t, src); len(got) > 0 {
				t.Errorf("门禁误报：%v\n%s", got, src)
			}
		})
	}
}

// TestLyricUpdateLintSkipsAgentWorktrees 钉住全仓扫描跳过 .claude：主目录下的 agent worktree 是停在
// 各自提交上的仓库副本，扫进去会让主目录跑出别的分支的旧代码。别处的子目录照扫。
//
// 变异自证：去掉对 .claude 的跳过即红（实测）。
func TestLyricUpdateLintSkipsAgentWorktrees(t *testing.T) {
	bad := []byte("package p\nfunc f() { use(&LyricUpdate{Index: -1}) }\n")
	root := t.TempDir()
	for _, dir := range []string{"server", filepath.Join(".claude", "worktrees", "old", "server")} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "server.go"), bad, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := scanLyricUpdateViolations(root)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], filepath.Join(root, "server", "server.go")) {
		t.Errorf("应只报 server/server.go 一处、不报 .claude 下的副本，实得 %v", got)
	}
}

// scanSrc 解析一段源码并跑检查，供门禁自测用。
func scanSrc(t *testing.T, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatalf("解析测试源码失败: %v", err)
	}
	return checkFile(fset, f, "x.go")
}

// isLyricUpdateLit 判断一个复合字面量是不是 LyricUpdate（含 player. 限定与包内裸名两种）。
func isLyricUpdateLit(lit *ast.CompositeLit) bool {
	switch t := lit.Type.(type) {
	case *ast.Ident:
		return t.Name == "LyricUpdate"
	case *ast.SelectorExpr:
		return t.Sel.Name == "LyricUpdate"
	}
	return false
}

// isNegativeOne 判断表达式是不是字面量 -1。
//
// 只认字面量：index 取自变量时（如常规歌词行的 l.Index）本门禁不该插手，
// 那条路径由 BuildLyricUpdate 统一构造。
func isNegativeOne(e ast.Expr) bool {
	u, ok := e.(*ast.UnaryExpr)
	if !ok || u.Op != token.SUB {
		return false
	}
	b, ok := u.X.(*ast.BasicLit)
	return ok && b.Kind == token.INT && b.Value == "1"
}

// checkFile 返回该文件里所有「LyricUpdate{Index: -1} 但没写 Position 或 Progress」的位置。
func checkFile(fset *token.FileSet, f *ast.File, path string) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isLyricUpdateLit(lit) {
			return true
		}
		var hasMinusOneIndex, hasPosition, hasProgress bool
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "Index":
				if isNegativeOne(kv.Value) {
					hasMinusOneIndex = true
				}
			case "Position":
				hasPosition = true
			case "Progress":
				hasProgress = true
			}
		}
		if hasMinusOneIndex && !(hasPosition && hasProgress) {
			out = append(out, fset.Position(lit.Pos()).String())
		}
		return true
	})
	return out
}

// scanLyricUpdateViolations 扫全仓。
func scanLyricUpdateViolations(root string) ([]string, error) {
	var violations []string
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// .claude 下是 agent 的 worktree（各自一份仓库副本，可能停在旧提交上），不属于本仓库的源码。
			if name := d.Name(); name == ".git" || name == ".claude" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // 解析不了的跳过，不是本门禁的职责
		}
		rel, _ := filepath.Rel(root, path)
		for _, v := range checkFile(fset, f, rel) {
			violations = append(violations, v)
		}
		return nil
	})
	return violations, err
}
