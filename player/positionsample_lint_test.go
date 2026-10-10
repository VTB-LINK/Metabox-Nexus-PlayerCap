package player

// 本文件只测一件事：AST 门禁「每个播放器包都调用 EmitPositionSample」（issue #70）。
//
// # 为什么要门禁
//
// 位置采样靠各播放器在自己的轮询循环里发。漏接一家不报错、不 panic，测试也照样绿：那一家在没有
// 歌词行可推的时段（无歌词的歌、长间奏）不再有任何采样，服务端按墙钟外推的位置随时长累计偏差，
// 只在连接补发与 HTTP 快照里露出来。新接入的播放器最容易漏（AGENTS.md §5.1）。
//
// # 它守什么、不守什么
//
// 判据是「调用了 NewBaseEmitter 的包」——即播放器包——里有没有对 EmitPositionSample 的调用。
// 只查有没有调用；调用条件（播放中、本曲 all_lyrics 已发出）与传入位置的口径（不含 offset）查不了，
// 各播放器里只有 applemusic 有离线的会话测试，其余靠读代码与真机。
//
// 只扫 player/ 目录：仓库根下可能挂着别的 worktree 的整份拷贝，它们的代码不归
// 本工作区管。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestPlayersEmitPositionSample 全部播放器包强制：调用 NewBaseEmitter 的包必须调用 EmitPositionSample。
//
// 变异自证：删掉 player/kugou/kugou.go 里对 EmitPositionSample 的调用即红。
func TestPlayersEmitPositionSample(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	missing, players, err := playerPkgsMissingSample(filepath.Join(root, "player"))
	if err != nil {
		t.Fatalf("扫描源码失败: %v", err)
	}
	// 零命中自检：一个播放器包都认不出来时，下面的断言恒真。现有六家。
	if players < 6 {
		t.Fatalf("只认出 %d 个调用 NewBaseEmitter 的包，应不少于 6 个——门禁的判据失效了", players)
	}
	if len(missing) > 0 {
		t.Errorf("以下播放器包没有调用 EmitPositionSample：\n  %s\n\n"+
			"没有歌词行在推的时段，服务端拿不到这一家的位置采样，连接补发与 HTTP 快照的位置会随时长累计偏差。\n"+
			"修法：在轮询循环里、歌词行匹配处调用它，条件与位置的口径见 BaseEmitter.EmitPositionSample 的注释。",
			strings.Join(missing, "\n  "))
	}
}

// TestPositionSampleLintCatchesKnownHoles 钉死门禁自身认得出目标形状（教训同 callback_lint_test.go：
// 抓不住目标形状的门禁比没有门禁更糟）。
func TestPositionSampleLintCatchesKnownHoles(t *testing.T) {
	const newEmitter = `package p
import "Metabox-Nexus-PlayerCap/player"
type P struct{ player.BaseEmitter }
func New() *P { return &P{BaseEmitter: player.NewBaseEmitter("p")} }
`
	cases := []struct {
		name    string
		files   map[string]string
		missing bool
	}{
		{"没有调用", map[string]string{"p.go": newEmitter}, true},
		{"只在测试文件里调用", map[string]string{"p.go": newEmitter, "p_test.go": `package p
func f(p *P) { p.EmitPositionSample(now, 1, 2) }`}, true},
		{"别名 import", map[string]string{"p.go": `package p
import base "Metabox-Nexus-PlayerCap/player"
var e = base.NewBaseEmitter("p")`}, true},
		{"同一文件里调用", map[string]string{"p.go": newEmitter + `func (p *P) loop() { p.EmitPositionSample(now, 1, 2) }`}, false},
		{"同包另一个文件里、经字段调用", map[string]string{"p.go": newEmitter, "session.go": `package p
func (s *session) tick() { s.p.EmitPositionSample(s.now(), 1, 2) }`}, false},
		{"不是播放器包", map[string]string{"util.go": `package util
func F() {}`}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, src := range c.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			missing, _, err := playerPkgsMissingSample(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(missing) > 0; got != c.missing {
				t.Errorf("判为漏接 = %v，期望 %v（%v）", got, c.missing, missing)
			}
		})
	}
}

// playerPkgsMissingSample 扫 dir 下的非测试 .go 文件，按目录归包：返回调用了 NewBaseEmitter 却没有
// 调用 EmitPositionSample 的目录（相对 dir，已排序），以及调用了 NewBaseEmitter 的目录总数。
func playerPkgsMissingSample(dir string) (missing []string, players int, err error) {
	type calls struct{ newEmitter, sample bool }
	pkgs := map[string]*calls{}
	fset := token.NewFileSet()

	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // 解析不了的跳过，不是本门禁的职责
		}
		pkg := filepath.Dir(path)
		c := pkgs[pkg]
		if c == nil {
			c = &calls{}
			pkgs[pkg] = c
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch calleeName(call.Fun) {
			case "NewBaseEmitter":
				c.newEmitter = true
			case "EmitPositionSample":
				c.sample = true
			}
			return true
		})
		return nil
	})
	for pkg, c := range pkgs {
		if !c.newEmitter {
			continue
		}
		players++
		if !c.sample {
			rel, _ := filepath.Rel(dir, pkg)
			missing = append(missing, rel)
		}
	}
	sort.Strings(missing)
	return missing, players, err
}

// calleeName 返回调用表达式里被调函数或方法的名字：f(...) 的 f，x.y.f(...) 的 f。
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}
