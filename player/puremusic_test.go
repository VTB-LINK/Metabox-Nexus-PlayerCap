package player

// 本文件只测 PureMusicHint：把「平台其实没有歌词，只给了一句提示语」认出来并返回提示原文
// （#231，设计 doc/design/converge-player-behavior-231-design.md §7.4）。
//
// 它是纯函数，各播放器的接线由各自包里的提示测试守。这里钉死判据本身：
//   - 提示句：恰为「纯音乐，请欣赏」，或以「此歌曲为」「该歌曲为」开头且以「欣赏」结尾（精确匹配，不 trim 空白）；
//   - 形状闸门：整首恰一行提示句，或两行且第 0 行去空白后等于非空标题去空白、第 1 行为提示句（返回第 1 行提示原文，绝不是歌名行）。
//
// 变异自证（均实测变红）：
//   - 去掉形状闸门（任意行是提示句即认）→ 「两行而第 0 行与标题不同」「两行而标题为空」「两行而标题仅空白」
//     「两行歌词中第 0 行是共用提示」「两行歌词中第 0 行是 kuwo 提示」「三行含歌名行与提示」「三行以提示结尾」红；
//   - 歌名行比较不去空白（直接 lines[0].Text == title）→ 三条「歌名行…」正例红；
//   - 歌名行加提示时返回 lines[0].Text 而非 lines[1].Text → 三条「歌名行…」正例红；
//   - 提示句判定只看前缀（去掉 strings.HasSuffix(text, "欣赏")）→ 「只有前缀此歌曲为」「只有前缀该歌曲为」
//     「kuwo 提示带尾随空格」红。

import "testing"

// line 造一行歌词，只关心 Text。
func line(idx int, text string) LyricLine {
	return LyricLine{Index: idx, Text: text}
}

func TestPureMusicHint(t *testing.T) {
	const (
		sharedHint   = "纯音乐，请欣赏"
		kuwoDJHint   = "此歌曲为DJ舞曲，请您欣赏"
		kuwoPureHint = "此歌曲为没有填词的纯音乐，请您欣赏"
		kuwoGaiHint  = "该歌曲为纯音乐请欣赏"
	)

	cases := []struct {
		name     string
		lines    []LyricLine
		title    string
		wantHint string
		wantOK   bool
	}{
		// 正例：单行提示
		{"共用说法一行", []LyricLine{line(0, sharedHint)}, "", sharedHint, true},
		{"共用说法一行带标题", []LyricLine{line(0, sharedHint)}, "某歌 - 某歌手", sharedHint, true},
		{"kuwo DJ 舞曲提示一行", []LyricLine{line(0, kuwoDJHint)}, "", kuwoDJHint, true},
		{"kuwo 无填词纯音乐提示一行", []LyricLine{line(0, kuwoPureHint)}, "", kuwoPureHint, true},
		{"kuwo 该歌曲为提示一行", []LyricLine{line(0, kuwoGaiHint)}, "", kuwoGaiHint, true},

		// 正例：歌名行加提示（标题含空格、第 0 行不含空格；必须返回第 1 行提示原文而非第 0 行歌名）
		{
			name:     "歌名行加 kuwo 提示",
			lines:    []LyricLine{line(0, "Go Time-Mark Petrie"), line(1, kuwoGaiHint)},
			title:    "Go Time - Mark Petrie",
			wantHint: kuwoGaiHint,
			wantOK:   true,
		},
		{
			name:     "歌名行加共用提示",
			lines:    []LyricLine{line(0, "Go Time-Mark Petrie"), line(1, sharedHint)},
			title:    "Go Time - Mark Petrie",
			wantHint: sharedHint,
			wantOK:   true,
		},
		{
			name:     "歌名行含制表符与全角外空白差异仍匹配",
			lines:    []LyricLine{line(0, "Mark\tPetrie-GoTime"), line(1, kuwoPureHint)},
			title:    "Mark Petrie - Go Time",
			wantHint: kuwoPureHint,
			wantOK:   true,
		},

		// 反例：形状闸门
		{
			name:   "两行而第 0 行与标题不同",
			lines:  []LyricLine{line(0, "Other Song-Mark Petrie"), line(1, kuwoGaiHint)},
			title:  "Go Time - Mark Petrie",
			wantOK: false,
		},
		{
			name:   "两行而标题为空",
			lines:  []LyricLine{line(0, "Go Time-Mark Petrie"), line(1, kuwoGaiHint)},
			title:  "",
			wantOK: false,
		},
		{
			name:   "两行而标题仅空白",
			lines:  []LyricLine{line(0, "   "), line(1, kuwoGaiHint)},
			title:  " ",
			wantOK: false,
		},
		{
			name:   "两行歌词中第 0 行是共用提示",
			lines:  []LyricLine{line(0, sharedHint), line(1, "这是真的歌词")},
			title:  "Go Time - Mark Petrie",
			wantOK: false,
		},
		{
			name:   "两行歌词中第 0 行是 kuwo 提示",
			lines:  []LyricLine{line(0, kuwoDJHint), line(1, "第二行")},
			title:  "Go Time - Mark Petrie",
			wantOK: false,
		},
		{
			name:   "两行歌名行加普通歌词",
			lines:  []LyricLine{line(0, "Go Time-Mark Petrie"), line(1, "第一句歌词")},
			title:  "Go Time - Mark Petrie",
			wantOK: false,
		},
		{
			name:   "三行含歌名行与提示",
			lines:  []LyricLine{line(0, "Go Time-Mark Petrie"), line(1, kuwoGaiHint), line(2, "多余一行")},
			title:  "Go Time - Mark Petrie",
			wantOK: false,
		},
		{
			name:   "三行以提示结尾",
			lines:  []LyricLine{line(0, "第一行"), line(1, "第二行"), line(2, sharedHint)},
			title:  "第一行",
			wantOK: false,
		},

		// 反例：提示句文本本身不匹配
		{"只有前缀此歌曲为", []LyricLine{line(0, "此歌曲为DJ舞曲")}, "", "", false},
		{"只有前缀该歌曲为", []LyricLine{line(0, "该歌曲为纯音乐")}, "", "", false},
		{"只有后缀请您欣赏", []LyricLine{line(0, "请您欣赏")}, "", "", false},
		{"只有后缀欣赏", []LyricLine{line(0, "欣赏")}, "", "", false},
		{"纯音乐三字", []LyricLine{line(0, "纯音乐")}, "", "", false},
		{"共用提示前后带空格", []LyricLine{line(0, " 纯音乐，请欣赏 ")}, "", "", false},
		{"共用提示带尾随空格", []LyricLine{line(0, "纯音乐，请欣赏 ")}, "", "", false},
		{"kuwo 提示带前导空格", []LyricLine{line(0, " 此歌曲为DJ舞曲，请您欣赏")}, "", "", false},
		{"kuwo 提示带尾随空格", []LyricLine{line(0, "此歌曲为DJ舞曲，请您欣赏 ")}, "", "", false},
		{"英文说法不认", []LyricLine{line(0, "Instrumental")}, "", "", false},
		{"普通单行歌词", []LyricLine{line(0, "这首歌只有一行歌词")}, "", "", false},

		// 反例：空输入
		{"nil 切片", nil, "Go Time - Mark Petrie", "", false},
		{"空切片", []LyricLine{}, "Go Time - Mark Petrie", "", false},
		{"一行空文本", []LyricLine{line(0, "")}, "Go Time - Mark Petrie", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotHint, gotOK := PureMusicHint(c.lines, c.title)
			if gotOK != c.wantOK || gotHint != c.wantHint {
				t.Errorf("PureMusicHint(%+v, %q) = (%q, %v)，期望 (%q, %v)",
					c.lines, c.title, gotHint, gotOK, c.wantHint, c.wantOK)
			}
		})
	}
}
