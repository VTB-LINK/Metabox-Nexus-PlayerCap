package qqmusic

// 本文件钉死 issue #116：取词成功但零行歌词（纯音乐）时，换歌那次与歌曲 ID 晚到的补取那次发出的事件
// 必须一致——空的 all_lyrics 加 index:-1 的 lyric_update。补取路径原先只发前者，只认 index:-1 的
// 前端在补取路径下收不到「没歌词」。runSession 要真进程，没有离线夹具，故分两层守：发出的内容守在
// emitNoLyrics 上，两处调用点由源码扫描守住。

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// TestEmitNoLyrics 钉死两条事件的内容、顺序与位置口径。
//
// 顺序是承重的：服务端收到 all_lyrics 时会清掉缓存的 lyric_update（server.go 的 UpdatePlayerState），
// index:-1 必须排在它之后才留得住，中途连入的客户端才拿得到。
//
// 变异自证：调换两条的发送顺序、把 Lyrics 换成 nil、删掉 lyric_update 里的 Progress、
// 把 Index 改成 0，各自变红。
func TestEmitNoLyrics(t *testing.T) {
	p := New(0, 500)
	p.emitNoLyrics("某歌 - 某人", 215.5, 12.25, 0.0568)

	var got []player.Event
	for len(p.EventCh) > 0 {
		got = append(got, <-p.EventCh)
	}
	if len(got) != 2 {
		t.Fatalf("应发出 2 条事件，实得 %d 条: %+v", len(got), got)
	}
	if got[0].Type != player.EventAllLyrics || got[1].Type != player.EventLyricUpdate {
		t.Fatalf("顺序应为 all_lyrics → lyric_update，实得 %s → %s", got[0].Type, got[1].Type)
	}

	all, ok := got[0].Data.(*player.AllLyricsData)
	if !ok {
		t.Fatalf("all_lyrics 的 Data 类型不对: %T", got[0].Data)
	}
	if all.Title != "某歌 - 某人" || all.Duration != 215.5 || all.Position != 12.25 || all.Progress != 0.0568 || all.Count != 0 {
		t.Errorf("all_lyrics 内容不对: %+v", all)
	}
	if all.Lyrics == nil || len(all.Lyrics) != 0 {
		t.Errorf("all_lyrics.Lyrics 应为显式空 slice，实得 %#v", all.Lyrics)
	}
	b, err := json.Marshal(all)
	if err != nil {
		t.Fatalf("all_lyrics 序列化失败: %v", err)
	}
	if !strings.Contains(string(b), `"lyrics":[]`) || strings.Contains(string(b), `"lyrics":null`) {
		t.Errorf("all_lyrics 应序列化出 \"lyrics\":[]，实得 %s", b)
	}

	lu, ok := got[1].Data.(*player.LyricUpdate)
	if !ok {
		t.Fatalf("lyric_update 的 Data 类型不对: %T", got[1].Data)
	}
	if lu.Index != -1 || lu.Text != "" || lu.SubText != "" || lu.Timestamp != 0 {
		t.Errorf("lyric_update 应为 index:-1 且文本全空，实得 %+v", lu)
	}
	if lu.Position != all.Position || lu.Progress != all.Progress {
		t.Errorf("lyric_update 的 position/progress 应与 all_lyrics 同值（%v/%v），实得 %v/%v",
			all.Position, all.Progress, lu.Position, lu.Progress)
	}
}

// TestRunSessionEmitsNoLyricsAtBothZeroLineSites 守住 runSession 里两处「取词成功但零行」都经 emitNoLyrics：
// 换歌时的主路径与歌曲 ID 晚到的补取路径。少一处，那一处就又只发 all_lyrics 而漏了 index:-1。
//
// 为什么用源码扫描：runSession 的轮询循环要真进程，补取分支离线喂不进去（回放夹具不取词，见
// sessionreplay_test.go 头注）。门禁只认调用次数，不替代读代码。
//
// 变异自证：把补取分支的 p.emitNoLyrics(...) 改回只发 all_lyrics 的字面量即红（调用数 1）。
func TestRunSessionEmitsNoLyricsAtBothZeroLineSites(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "qqmusic.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 qqmusic.go 失败: %v", err)
	}
	calls := -1
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runSession" {
			continue
		}
		calls = 0
		ast.Inspect(fn, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "emitNoLyrics" {
					calls++
				}
			}
			return true
		})
	}
	if calls < 0 {
		t.Fatal("没找到 runSession —— 本门禁的判据失效了")
	}
	if calls != 2 {
		t.Errorf("runSession 里应有 2 处 emitNoLyrics 调用（换歌主路径、ID 晚到的补取），实得 %d 处", calls)
	}
}
