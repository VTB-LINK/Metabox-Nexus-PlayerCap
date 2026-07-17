package player

// 本文件只测一件事：全仓 AST 门禁「LyricUpdate 字面量写了 Index: -1 就必须写 Progress」。
// 它不测 player 包的业务逻辑——扫的是整个仓库的源码。
//
// # 为什么这条要用 AST 门禁，而不是单元测试
//
// 三个发出 index:-1 的包（cloudmusic / kugou / qqmusic）**CI 永远跑不了**（AGENTS.md §8：
// GOOS=linux go vet 全部 FAIL）。而 AST 扫描不需要编译目标平台，所以这是这条契约唯一
// 能自动化的守法。
//
// # 它守的是什么
//
// Progress 的契约是「整首播到哪」。纯音乐时歌照样在走，progress 不该是 0。
// 但 cloudmusic 与 kugou 的 index:-1 字面量此前都漏了这个字段，Go 零值兜出一个 0，
// 而同一时刻它们自己发的 all_lyrics.progress 是实时值——**同一个事件对里自相矛盾**。
// qqmusic 写对了，于是三家给出两种答案（实测：qqmusic 0.125 / kugou 0）。
//
// 更糟的是这个零值被当成契约写进了对外文档：openapi.yaml 四处声称「index:-1 时
// progress 为 0」。写文档的人照着 cloudmusic 看的，而 cloudmusic 只是漏了字段。
// 一个漏写的字段，先变成 bug，再变成文档，最后变成下游据以实现的假契约。
//
// # 为什么盯 Index: -1 而不是所有 LyricUpdate
//
// 常规歌词行由 BuildLyricUpdate 统一构造（40e77af 把易错的语义锁在了一处），
// 不会漏字段。index:-1 是三家各自手写字面量的地方——手写就是漏字段的温床。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLyricUpdateIndexMinusOneHasProgress 全仓强制：写 Index: -1 的 LyricUpdate 字面量必须带 Progress。
//
// 变异自证：把 player/kugou/kugou.go 里 index:-1 那处的 `Progress: progress` 删掉即红。
func TestLyricUpdateIndexMinusOneHasProgress(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	violations, err := scanLyricUpdateViolations(root)
	if err != nil {
		t.Fatalf("扫描源码失败: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("发现 %d 处 LyricUpdate{Index: -1} 漏写 Progress（Go 零值会兜出 progress=0，"+
			"而同一时刻 all_lyrics.progress 是实时值——同一个事件对自相矛盾，"+
			"且下游据此以为「纯音乐时进度条归零」）：\n  %s\n\n"+
			"修法：Progress 通常在上面几行就由 player.ClampProgress 算好了（all_lyrics 已经带上它），"+
			"补 `Progress: progress` 即可。参照 player/qqmusic/qqmusic.go:241。",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// TestLyricUpdateLintCatchesKnownHoles 钉死门禁自身能抓住目标形状。
//
// 照抄 callback_lint_test.go 的教训：**一个抓不住目标形状的门禁比没有门禁更糟**——
// 它给人已经守住了的错觉。下面每种形状都是真实出现过或极可能出现的写法。
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
	}
	for name, src := range shouldCatch {
		t.Run(name, func(t *testing.T) {
			if got := scanSrc(t, src); len(got) == 0 {
				t.Errorf("门禁漏报了这种形状——它正是要堵的：\n%s", src)
			}
		})
	}

	shouldPass := map[string]string{
		"带了 Progress（修复后的样子）": `package p
import "Metabox-Nexus-PlayerCap/player"
func f(playTime, progress float32) {
	emit(&player.LyricUpdate{Index: -1, Text: "", PlayTime: playTime, Progress: progress})
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

// checkFile 返回该文件里所有「LyricUpdate{Index: -1} 但没写 Progress」的位置。
func checkFile(fset *token.FileSet, f *ast.File, path string) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isLyricUpdateLit(lit) {
			return true
		}
		var hasMinusOneIndex, hasProgress bool
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
			case "Progress":
				hasProgress = true
			}
		}
		if hasMinusOneIndex && !hasProgress {
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
			if name := d.Name(); name == ".git" || name == "node_modules" {
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
