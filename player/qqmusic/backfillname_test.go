package qqmusic

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

// backfillNameProblems 检查 src（qqmusic.go 的源码）里 runSession 的补取块不改写认领时发出的歌名、歌手与标题（#228）：
// 补取到歌词时接口返回的歌名、歌手常与 QQ 显示的写法不同（副标题、空格、译名），前端按歌名与歌手判同一首歌，改写即被
// 当成又换了一首（2026-11-22 真机录音 7 次）。补取块自它的 if 起、到变化分支的注释止；找不到时自报失效。
func backfillNameProblems(src string) []string {
	const head, tail = "if lyricsPending && !released && meta.Name == lastName", "// --- 快速计时器锚点更新 + seek 检测 ---"
	i, j := strings.Index(src, head), strings.Index(src, tail)
	if i < 0 || j < i {
		return []string{"没找到补取块 —— 门禁失效"}
	}
	block := src[i:j]
	var probs []string
	for _, s := range []string{"lrcName", "lrcSinger", "currentTitle =", "currentName =", "currentSinger =", "currentName, currentSinger ="} {
		if strings.Contains(block, s) {
			probs = append(probs, "补取块里出现 "+s)
		}
	}
	if !strings.Contains(block, "name, singer, title := currentName, currentSinger, currentTitle") {
		probs = append(probs, "补取块发出的歌名、歌手与标题应取认领时的 currentName、currentSinger、currentTitle")
	}
	return probs
}

// claimedNameProblems 把同一条规则扩到补取块之外（#218 变基后）：补取块与播放中换词都经 showLyrics 发歌词，它发出的
// all_lyrics 与无歌词时的标题须取 currentTitle；currentTitle、currentName、currentSinger 只在换歌分支里赋值；
// song_info_update 只在换歌分支与补取块里发（补取块里的写法由 backfillNameProblems 查）。找不到锚点时自报失效。
func claimedNameProblems(src string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "qqmusic.go", src, 0)
	if err != nil {
		return []string{"解析失败: " + err.Error()}
	}
	str := types.ExprString
	var run *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil && fd.Name.Name == "runSession" {
			run = fd
		}
	}
	if run == nil {
		return []string{"没找到 runSession —— 门禁失效"}
	}
	var show *ast.FuncLit
	var sw, fill *ast.IfStmt
	for _, s := range run.Body.List {
		switch s := s.(type) {
		case *ast.AssignStmt:
			if fl, ok := s.Rhs[0].(*ast.FuncLit); ok && len(s.Lhs) == 1 && str(s.Lhs[0]) == "showLyrics" {
				show = fl
			}
		case *ast.ForStmt:
			for _, st := range s.Body.List {
				ifs, ok := st.(*ast.IfStmt)
				switch {
				case !ok:
				case sw == nil && strings.HasPrefix(str(ifs.Cond), "meta.Name != lastName"):
					sw = ifs
				case fill == nil && strings.Contains(str(ifs.Cond), "lyricsPending"):
					fill = ifs
				}
			}
		}
	}
	if show == nil || sw == nil || fill == nil {
		return []string{"没找到 showLyrics、换歌分支或补取块 —— 门禁失效"}
	}
	within := func(n ast.Node, b ast.Node) bool { return n.Pos() >= b.Pos() && n.End() <= b.End() }
	var probs []string
	ast.Inspect(run.Body, func(x ast.Node) bool {
		switch x := x.(type) {
		case *ast.AssignStmt:
			for _, l := range x.Lhs {
				switch str(l) {
				case "currentTitle", "currentName", "currentSinger":
					if !within(x, sw.Body) {
						probs = append(probs, "换歌分支之外给 "+str(l)+" 赋值")
					}
				}
			}
		case *ast.CallExpr:
			for _, a := range x.Args {
				if str(a) == "player.EventSongInfoUpdate" && !within(x, sw.Body) && !within(x, fill.Body) {
					probs = append(probs, "换歌分支与补取块之外发了 song_info_update")
				}
			}
		}
		return true
	})
	emits := 0
	ast.Inspect(show.Body, func(x ast.Node) bool {
		c, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch {
		case str(c.Fun) == "p.emitNoLyrics" && len(c.Args) > 0:
			emits++
			if str(c.Args[0]) != "currentTitle" {
				probs = append(probs, "showLyrics 无歌词一支的标题应取 currentTitle，实得 "+str(c.Args[0]))
			}
		case str(c.Fun) == "p.Emit" && len(c.Args) == 2 && str(c.Args[0]) == "player.EventAllLyrics":
			emits++
			title := ""
			if u, ok := c.Args[1].(*ast.UnaryExpr); ok {
				if cl, ok := u.X.(*ast.CompositeLit); ok {
					for _, el := range cl.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok && str(kv.Key) == "Title" {
							title = str(kv.Value)
						}
					}
				}
			}
			if title != "currentTitle" {
				probs = append(probs, "showLyrics 发 all_lyrics 的标题应取 currentTitle，实得 "+title)
			}
		}
		return true
	})
	if emits == 0 {
		probs = append(probs, "showLyrics 里没找到发歌词的调用 —— 门禁失效")
	}
	return probs
}

func TestBackfillKeepsClaimedNames(t *testing.T) {
	if probs := backfillNameProblems(readQQMusicSource(t)); len(probs) > 0 {
		t.Fatalf("补取块改写了认领时的歌名、歌手或标题：\n  %s", strings.Join(probs, "\n  "))
	}
	if probs := claimedNameProblems(readQQMusicSource(t)); len(probs) > 0 {
		t.Fatalf("补取块之外发出的歌名、歌手或标题没有沿用认领时的：\n  %s", strings.Join(probs, "\n  "))
	}
}

// TestBackfillNameCatchesKnownHoles 以改写过的源码确认门禁拦得住修复之前的写法与它的变体。改写找不到原文时自报失效。
func TestBackfillNameCatchesKnownHoles(t *testing.T) {
	src := readQQMusicSource(t)
	const (
		fetch = "lines, _, _, ferr := fetchLRC("
		names = "name, singer, title := currentName, currentSinger, currentTitle"
	)
	cases := []struct {
		name, needle string
		mutate       func(string) string
	}{
		{"歌名取接口返回的", names, func(s string) string {
			s = strings.Replace(s, fetch, "lines, lrcName, _, ferr := fetchLRC(", 1)
			return strings.Replace(s, names, "name, singer, title := lrcName, currentSinger, currentTitle", 1)
		}},
		{"歌名、歌手取本拍内存的", names, func(s string) string {
			return strings.Replace(s, names, `name, singer, title := meta.Name, meta.Singer, meta.Name+" - "+meta.Singer`, 1)
		}},
		{"补取改写 currentTitle", names, func(s string) string {
			return strings.Replace(s, names, names+"\n\t\t\t\t\t\tcurrentTitle = name", 1)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(src, c.needle) {
				t.Fatalf("qqmusic.go 里找不到改写依赖的原文 %q —— 本条自测失效", c.needle)
			}
			mutated := c.mutate(src)
			if mutated == src {
				t.Fatalf("改写没有生效 —— 本条自测失效")
			}
			if len(backfillNameProblems(mutated)) == 0 {
				t.Fatalf("门禁没有拦下这处改写")
			}
		})
	}
}

// TestClaimedNameCatchesKnownHoles 以改写过的源码确认 claimedNameProblems 拦得住补取块之外改写或另发歌名、歌手与标题。
// 改写找不到原文时自报失效。
//
// 变异自证（均实测，在仓库外的副本里）：qqmusic.go 里 showLyrics 的 all_lyrics 标题或无歌词一支的标题改取 currentName、
// 播放中换词时改写 currentTitle、另发 song_info_update，TestBackfillKeepsClaimedNames 均红（backfillNameProblems 只看
// 补取块，四种都拦不住）；claimedNameProblems 不查赋值、不查 song_info_update 的发出处、不查 showLyrics 的标题，本例
// 相应的改写依次红。
func TestClaimedNameCatchesKnownHoles(t *testing.T) {
	src := readQQMusicSource(t)
	const (
		showTitle = "Title: currentTitle, Duration: currentDurationSec, Position: pos"
		showNone  = "p.emitNoLyrics(currentTitle, currentDurationSec, pos, prog)"
		showReset = "\t\tcurrentLyrics = lines\n\t\tlastLineIdx = -1\n"
		tickShow  = "\t\t\t\tshowLyrics(lines, float32(anchorProgressMs)/1000.0, anchorTime)\n"
	)
	cases := []struct {
		name, needle string
		mutate       func(string) string
	}{
		{"showLyrics 的 all_lyrics 标题另取", showTitle, func(s string) string {
			return strings.Replace(s, showTitle, "Title: lyricSt.title, Duration: currentDurationSec, Position: pos", 1)
		}},
		{"showLyrics 无歌词一支的标题另取", showNone, func(s string) string {
			return strings.Replace(s, showNone, "p.emitNoLyrics(lyricSt.title, currentDurationSec, pos, prog)", 1)
		}},
		{"showLyrics 改写 currentTitle", showReset, func(s string) string {
			return strings.Replace(s, showReset, showReset+"\t\tcurrentTitle = lyricSt.title\n", 1)
		}},
		{"播放中换词改写 currentName", tickShow, func(s string) string {
			return strings.Replace(s, tickShow, tickShow+"\t\t\t\tcurrentName, currentSinger = lyricSt.name, lyricSt.singer\n", 1)
		}},
		{"播放中换词另发 song_info_update", tickShow, func(s string) string {
			return strings.Replace(s, tickShow, tickShow+"\t\t\t\tp.Emit(player.EventSongInfoUpdate, &player.SongInfo{Title: currentTitle})\n", 1)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(src, c.needle) {
				t.Fatalf("qqmusic.go 里找不到改写依赖的原文 %q —— 本条自测失效", c.needle)
			}
			mutated := c.mutate(src)
			if mutated == src {
				t.Fatalf("改写没有生效 —— 本条自测失效")
			}
			for _, p := range claimedNameProblems(mutated) {
				if !strings.Contains(p, "门禁失效") && !strings.HasPrefix(p, "解析失败") {
					return
				}
			}
			t.Fatalf("门禁没有拦下这处改写")
		})
	}
}
