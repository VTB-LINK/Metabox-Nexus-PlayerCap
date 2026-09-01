package cloudmusic

// 本文件只测 resolveCDPLyrics：CDP 返回的歌词只有解析出**非空**行才算成功。
// 判成功会关掉调用方的 Redux fallback，所以 0 行必须判失败，否则前端整首停在上一首。

import "testing"

// TestResolveCDPLyrics 钉死 CDP 歌词判定：只有解析出非空歌词行才算成功。
//
// 缺陷背景：CDP 的 FetchLyricsViaCDP 对「有 lrc 字段但内容只是元数据标签」的歌会返回
// 非 PureMusic/NoLyric（即代码认为"有歌词"），但 ParseLRC 只保留带 [mm:ss.xx] 时间戳的
// 行，对只含 [ti:]/[ar:] 的 lrc 返回 0 行。原代码在 ParseLRC 之前就置了 cdpLyricsOK=true，
// 于是 activeLyrics 保持空、而 cdpLyricsOK 又挡住了 Redux fallback（cloudmusic.go:379/407
// 都有 !cdpLyricsOK 条件），前端整首歌停在上一首。
//
// 变异自证：删掉 resolveCDPLyrics 里 `if len(parsed)==0 { return nil,false }` 即红。
func TestResolveCDPLyrics(t *testing.T) {
	t.Run("只含元数据标签 → 不成功", func(t *testing.T) {
		parsed, ok := resolveCDPLyrics("[ti:某歌]\n[ar:某歌手]\n[al:某专辑]\n[by:上传者]\n", "", "", "", 0)
		if ok {
			t.Error("只含元数据标签的 lrc 不应标记 CDP 歌词成功（否则挡住 Redux fallback）")
		}
		if len(parsed) != 0 {
			t.Errorf("应解析出 0 行，实得 %d", len(parsed))
		}
	})

	t.Run("无时间戳的纯文本 → 不成功", func(t *testing.T) {
		_, ok := resolveCDPLyrics("just some plain text\nno timestamps at all", "", "", "", 0)
		if ok {
			t.Error("无时间戳的纯文本不应标记成功")
		}
	})

	t.Run("空字符串 → 不成功", func(t *testing.T) {
		if _, ok := resolveCDPLyrics("", "", "", "", 0); ok {
			t.Error("空 lrc 不应标记成功")
		}
	})

	t.Run("只有 intentional blank、无实词 → 不成功", func(t *testing.T) {
		// ParseLRC 会保留 intentional blank（正文为空、表示此刻清空歌词的行），所以
		// len(parsed) > 0 不等于「有歌词」。一首只有 blank 的 lrc 一个字都没有，必须判
		// 失败、交给 Redux fallback，否则关掉 fallback 后 OBS 整首空白。
		// 变异自证：把 hasRealText 换回 len(parsed)==0 即红。
		parsed, ok := resolveCDPLyrics("[00:10.00]\n[00:20.00][00:30.00]\n", "", "", "", 0)
		if ok {
			t.Errorf("只有 blank、无实词的 lrc 不应判为有歌词，实得 %d 条", len(parsed))
		}
	})

	t.Run("实词 + blank 混合 → 成功（blank 不影响判定）", func(t *testing.T) {
		parsed, ok := resolveCDPLyrics("[00:05.00]有词\n[00:10.00]\n", "", "", "", 0)
		if !ok {
			t.Fatal("含实词的 lrc 应判为有歌词，blank 不该拖累判定")
		}
		if len(parsed) != 2 {
			t.Errorf("实词 + blank 应保留 2 条，实得 %d 条", len(parsed))
		}
	})

	t.Run("正常带时间戳 → 成功且返回对应行，音译经 CDP 主路径透传", func(t *testing.T) {
		// romalrc 走 resolveCDPLyrics → MergeRomalrc，钉死 CDP 主路径把音译写进 RomaText
		// 而非 SubText（tlyric 为空，SubText 必须保持空）。
		parsed, ok := resolveCDPLyrics(
			"[ti:x]\n[00:01.00]第一行\n[00:05.50]第二行\n", "",
			"[00:01.00]diyi hang\n[00:05.50]dier hang", "", 0)
		if !ok {
			t.Fatal("带时间戳行的正常 lrc 应标记成功")
		}
		if len(parsed) != 2 {
			t.Fatalf("应解析出 2 行，实得 %d", len(parsed))
		}
		if parsed[0].Text != "第一行" || parsed[1].Text != "第二行" {
			t.Errorf("歌词文本不对: %q / %q", parsed[0].Text, parsed[1].Text)
		}
		if parsed[0].RomaText != "diyi hang" || parsed[1].RomaText != "dier hang" {
			t.Errorf("音译未经 CDP 主路径透传到 RomaText: %q / %q", parsed[0].RomaText, parsed[1].RomaText)
		}
		if parsed[0].SubText != "" || parsed[1].SubText != "" {
			t.Errorf("音译污染了 sub_text: %q / %q", parsed[0].SubText, parsed[1].SubText)
		}
	})
}
