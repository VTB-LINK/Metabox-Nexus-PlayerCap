package qqmusic

// 本文件只测 shouldDeferForSongMid：换歌时是否**可能**要等 songMid。
// 等多久是 midWaitGate 的事（midwait_test.go），本判定只管「该不该等」。

import "testing"

// TestShouldDeferForSongMid 守住换歌推迟判定的边界。
//
// 缺陷背景：换歌时若在 songMid 就绪前就认领 lastName，则下次轮询不再进换歌分支、
// songMid 就绪也永不重试，上一首的歌词会一直逐行滚在新歌上。修复把「songMid 未就绪
// 就推迟（不认领 lastName）」作为换歌分支的前置守卫。
//
// 本判定的要害是 usesSongMid 前提：只有靠 songMid 取词的版本（v20.05 从 URL 参数、
// v22.41 从堆 JSON）才该等；靠数字 songID 取词的版本（22.16/22.22）songMid 恒空，若漏
// 了这个前提、只看 songMid 是否为空，就会让这些版本永远推迟、整版本加载不了歌词。
//
// 变异自证：把 `usesSongMid &&` 去掉（只看 songMid），或把 `== ""` 改成 `!= ""`，
// 都会让某条断言红。
func TestShouldDeferForSongMid(t *testing.T) {
	// 靠 songMid 取词（v20.05 / v22.41）且 songMid 未就绪 → 推迟，等下次
	if !shouldDeferForSongMid(true, "") {
		t.Error("靠 songMid 取词、songMid 未就绪时应推迟换歌处理")
	}
	// 靠 songMid 取词且 songMid 已就绪 → 不推迟，正常加载
	if shouldDeferForSongMid(true, "000mDR751jtpPf") {
		t.Error("songMid 就绪后不应再推迟")
	}
	// 靠数字 songID 取词的版本（usesSongMid==false）songMid 恒空 → **不推迟**（要害）
	if shouldDeferForSongMid(false, "") {
		t.Error("songID 版本 songMid 恒空，但绝不能推迟——否则永远等不到、整版本加载不了歌词")
	}
	// songID 版本即便偶然有 songMid → 也不推迟
	if shouldDeferForSongMid(false, "abc") {
		t.Error("songID 版本不该推迟")
	}
}
