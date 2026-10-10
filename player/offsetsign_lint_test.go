package player

// 本文件只测一件事：AST 门禁「offset 的符号只写在 player.go 的两个函数里」（issue #54）。
//
// # 为什么要门禁
//
// offset 是延迟量。它经两类算式生效：歌词时间轴上的播出时间（AdjustLyricPlayTime，时间戳 + offset）与
// 歌词行匹配用的时刻（LyricMatchTime，位置 - offset）。后一类原先由六家播放器各写一处「位置 + offset」，
// 其中只有 applemusic 有离线的会话测试：其余五家漏改或写反一处，没有用例会红，表现为该播放器
// lyric_update 的发出时刻与其 play_time 相差两倍 offset。
//
// # 它守什么、不守什么
//
// 两条判据，都按包（目录）算，只看非测试文件：
//   - 调用了 NewBaseEmitter 的包（即播放器包）必须调用 LyricMatchTime；
//   - player/ 的各子目录里不得对名为 offsetSec / offsetMs 的标识符或字段做加、减、取负（含 += 与 -=）。
//     毫秒到秒的换算（float32(p.offsetMs) / 1000）是除法，不在此列。
//
// 只查有没有调用与有没有这几种形状：传给 LyricMatchTime 的位置取的是哪个读数、它的返回值有没有被拿去
// 填对外事件的 position，查不了，靠读代码与真机（applemusic 另有 sessionoffset_test.go）。变量改了名
// （不叫 offsetSec / offsetMs）、先赋给别的变量再做加减，同样查不出。player/ 根目录（本包）不在第二条
// 之内：两个函数就在这里。不做类型解析。
//
// 只扫 player/ 目录：仓库根下可能挂着别的 worktree 的整份拷贝，它们的代码不归本工作区管。

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

// TestPlayersUseLyricMatchTime 全部播放器包强制：行匹配经由 LyricMatchTime，不自行对 offset 做加减。
//
// 变异自证（隔离的 worktree，实测）：把 player/kugou/kugou.go 的行匹配改回 `interpSec + offsetSec`
// → 两条判据都红；改成 `interpSec - offsetSec`（方向对、但绕过公共函数）→ 同样两条都红；保留
// LyricMatchTime 的调用、另在 qqmusic 的 lyric_update 里把位置写成 `progressSec - offsetSec` → 第二条红。
func TestPlayersUseLyricMatchTime(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	missing, arithmetic, players, err := offsetSignProblems(filepath.Join(root, "player"))
	if err != nil {
		t.Fatalf("扫描源码失败: %v", err)
	}
	// 零命中自检：一个播放器包都认不出来时，下面的断言恒真。现有六家。
	if players < 6 {
		t.Fatalf("只认出 %d 个调用 NewBaseEmitter 的包，应不少于 6 个——门禁的判据失效了", players)
	}
	if len(missing) > 0 {
		t.Errorf("以下播放器包没有调用 LyricMatchTime：\n  %s\n\n"+
			"歌词行匹配用的时刻须经 player.LyricMatchTime(位置, offsetSec) 得到，offset 的符号只写在那里。",
			strings.Join(missing, "\n  "))
	}
	if len(arithmetic) > 0 {
		t.Errorf("以下位置自行对 offset 做了加、减或取负：\n  %s\n\n"+
			"offset 的符号只写在 player.go 的 AdjustLyricPlayTime 与 LyricMatchTime 里；"+
			"播出时间用前者、行匹配用后者，对外事件的 position 不含 offset。",
			strings.Join(arithmetic, "\n  "))
	}
}

// TestOffsetSignLintCatchesKnownHoles 钉死门禁自身认得出目标形状（教训同 callback_lint_test.go：
// 抓不住目标形状的门禁比没有门禁更糟）。
func TestOffsetSignLintCatchesKnownHoles(t *testing.T) {
	const header = `package p
import "Metabox-Nexus-PlayerCap/player"
type P struct{ player.BaseEmitter; offsetMs int }
func New() *P { return &P{BaseEmitter: player.NewBaseEmitter("p")} }
`
	const matched = "func (p *P) loop(pos, offsetSec float32) { _ = player.LyricMatchTime(pos, offsetSec) }\n"
	cases := []struct {
		name       string
		files      map[string]string // 路径相对扫描根目录
		missing    bool
		arithmetic bool
	}{
		{"经公共函数匹配", map[string]string{"p/p.go": header + matched}, false, false},
		{"毫秒到秒的换算不算", map[string]string{"p/p.go": header + matched +
			"func (p *P) sec() float32 { return float32(p.offsetMs) / 1000.0 }\n"}, false, false},
		{"没有调用", map[string]string{"p/p.go": header}, true, false},
		{"只在测试文件里调用", map[string]string{"p/p.go": header, "p/p_test.go": "package p\n" + matched}, true, false},
		{"同包另一个文件里调用", map[string]string{"p/p.go": header, "p/session.go": "package p\n" +
			"func (s *session) line() { _ = player.LyricMatchTime(s.pos, s.offsetSec) }\n"}, false, false},
		{"位置加 offset", map[string]string{"p/p.go": header +
			"func (p *P) loop(pos, offsetSec float32) { _ = pos + offsetSec }\n"}, true, true},
		{"位置减 offset", map[string]string{"p/p.go": header + matched +
			"func (p *P) bad(pos, offsetSec float32) { _ = pos - offsetSec }\n"}, false, true},
		{"offset 在左", map[string]string{"p/p.go": header + matched +
			"func (p *P) bad(pos, offsetSec float32) { _ = offsetSec + pos }\n"}, false, true},
		{"字段、带括号", map[string]string{"p/p.go": header + matched +
			"func (s *session) bad(pos float32) { _ = pos + (s.offsetSec) }\n"}, false, true},
		{"取负后传入", map[string]string{"p/p.go": header + matched +
			"func (p *P) bad(pos, offsetSec float32) { _ = player.LyricMatchTime(pos, -offsetSec) }\n"}, false, true},
		{"复合赋值", map[string]string{"p/p.go": header + matched +
			"func (p *P) bad(pos, offsetSec float32) { pos += offsetSec; _ = pos }\n"}, false, true},
		{"毫秒值做减法", map[string]string{"p/p.go": header + matched +
			"func (p *P) bad(posMs int) int { return posMs - p.offsetMs }\n"}, false, true},
		{"换算后再加", map[string]string{"p/p.go": header + matched +
			"func (p *P) bad(pos float32) float32 { return pos + float32(p.offsetMs)/1000 }\n"}, false, true},
		{"子包里的加减也算", map[string]string{"p/p.go": header + matched, "p/lyric/l.go": "package lyric\n" +
			"func shift(t, offsetSec float32) float32 { return t - offsetSec }\n"}, false, true},
		{"名字不同的偏移量不算", map[string]string{"p/p.go": header + matched +
			"func read(base, offset uintptr) uintptr { return base + offset }\n"}, false, false},
		{"根目录不查加减", map[string]string{"player.go": "package player\n" +
			"func LyricMatchTime(position, offsetSec float32) float32 { return position - offsetSec }\n",
			"p/p.go": header + matched}, false, false},
		{"不是播放器包", map[string]string{"util/util.go": "package util\nfunc F() {}\n"}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, src := range c.files {
				path := filepath.Join(dir, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			missing, arithmetic, _, err := offsetSignProblems(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(missing) > 0; got != c.missing {
				t.Errorf("判为没有调用 LyricMatchTime = %v，期望 %v（%v）", got, c.missing, missing)
			}
			if got := len(arithmetic) > 0; got != c.arithmetic {
				t.Errorf("判为自行加减 = %v，期望 %v（%v）", got, c.arithmetic, arithmetic)
			}
		})
	}
}

// offsetSignProblems 扫 dir 下的非测试 .go 文件，按目录归包：missing 是调用了 NewBaseEmitter 却没有调用
// LyricMatchTime 的目录（相对 dir，已排序）；arithmetic 是 dir 的各子目录里对 offsetSec / offsetMs 做
// 加、减、取负的位置（「相对路径:行号」，已排序）；players 是调用了 NewBaseEmitter 的目录总数。
func offsetSignProblems(dir string) (missing, arithmetic []string, players int, err error) {
	type calls struct{ newEmitter, matchTime bool }
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
		rel, _ := filepath.Rel(dir, path)
		inRoot := pkg == filepath.Clean(dir)
		flag := func(n ast.Node) {
			arithmetic = append(arithmetic, filepath.ToSlash(rel)+":"+strconv.Itoa(fset.Position(n.Pos()).Line))
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				switch calleeName(n.Fun) {
				case "NewBaseEmitter":
					c.newEmitter = true
				case "LyricMatchTime":
					c.matchTime = true
				}
			case *ast.BinaryExpr:
				if !inRoot && (n.Op == token.ADD || n.Op == token.SUB) && (mentionsOffset(n.X) || mentionsOffset(n.Y)) {
					flag(n)
				}
			case *ast.UnaryExpr:
				if !inRoot && n.Op == token.SUB && mentionsOffset(n.X) {
					flag(n)
				}
			case *ast.AssignStmt:
				if !inRoot && (n.Tok == token.ADD_ASSIGN || n.Tok == token.SUB_ASSIGN) {
					for _, e := range append(append([]ast.Expr{}, n.Lhs...), n.Rhs...) {
						if mentionsOffset(e) {
							flag(n)
							break
						}
					}
				}
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
		if !c.matchTime {
			rel, _ := filepath.Rel(dir, pkg)
			missing = append(missing, rel)
		}
	}
	sort.Strings(missing)
	sort.Strings(arithmetic)
	return missing, arithmetic, players, err
}

// mentionsOffset 报告加减或取负的操作数 e 是不是用户配置的 offset：名为 offsetSec / offsetMs 的标识符或
// 字段，可带括号、可经类型转换（float32(x)）、可再乘除一个数（float32(p.offsetMs)/1000）。
func mentionsOffset(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name == "offsetSec" || e.Name == "offsetMs"
	case *ast.SelectorExpr:
		return e.Sel.Name == "offsetSec" || e.Sel.Name == "offsetMs"
	case *ast.ParenExpr:
		return mentionsOffset(e.X)
	case *ast.CallExpr: // 类型转换：float32(p.offsetMs)
		return len(e.Args) == 1 && mentionsOffset(e.Args[0])
	case *ast.BinaryExpr:
		if e.Op == token.MUL || e.Op == token.QUO {
			return mentionsOffset(e.X) || mentionsOffset(e.Y)
		}
	}
	return false
}
