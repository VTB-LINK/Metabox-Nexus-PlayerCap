package qqmusic

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player"
)

// TestBackfillNames 验证补取到歌词时歌名与歌手的取舍规则（#229，Q1–Q5）：
//   - 读到的值优先取歌词标签，为空时回退本拍内存值（两项独立回退）；
//   - 再经 player.SupplementField 与认领时的值取舍：开关开启时有值保持、为空（含纯空白）可补非空；
//     开关关闭时一律采用读到的值。
//
// 变异自证（隔离副本实测，全部可编译）：
//   - backfillNames 歌词标签为空时不退回内存值 → Q4 关、Q4 开补空、Q4 关单项回退红；
//   - SupplementField 开启时一律取 A → Q3、Q4 开补空、Q5 补空红；
//   - SupplementField 开启时一律取 R → Q1、Q3、Q4 开、Q5 红；
//   - SupplementField 不去空白（announced != "" || read == ""）→ Q5 三例均红；
//   - SupplementField 忽略开关关闭 → Q2、Q4 关、Q4 关单项回退红。
func TestBackfillNames(t *testing.T) {
	cases := []struct {
		name          string
		keepFirst     bool
		claimedName   string
		claimedSinger string
		memName       string
		memSinger     string
		lrcName       string
		lrcSinger     string
		wantName      string
		wantSinger    string
	}{
		{
			name:          "Q1/开/认领非空保持认领值忽略歌词标签改写",
			keepFirst:     true,
			claimedName:   "晚风",
			claimedSinger: "陈婧霏",
			memName:       "晚风",
			memSinger:     "陈婧霏",
			lrcName:       "晚风 (Live)",
			lrcSinger:     "陈婧霏、X",
			wantName:      "晚风",
			wantSinger:    "陈婧霏",
		},
		{
			name:          "Q2/关/采用歌词标签写法",
			keepFirst:     false,
			claimedName:   "晚风",
			claimedSinger: "陈婧霏",
			memName:       "晚风",
			memSinger:     "陈婧霏",
			lrcName:       "晚风 (Live)",
			lrcSinger:     "陈婧霏、X",
			wantName:      "晚风 (Live)",
			wantSinger:    "陈婧霏、X",
		},
		{
			name:          "Q3/开/认领歌手为空只补歌手歌名仍保持认领值",
			keepFirst:     true,
			claimedName:   "晚风",
			claimedSinger: "",
			memName:       "晚风",
			memSinger:     "",
			lrcName:       "晚风 (Live)",
			lrcSinger:     "X",
			wantName:      "晚风",
			wantSinger:    "X",
		},
		{
			name:          "Q4/开/歌词标签均为空且内存与认领不同时保持认领值",
			keepFirst:     true,
			claimedName:   "晚风",
			claimedSinger: "陈婧霏",
			memName:       "晚风 (内存)",
			memSinger:     "内存歌手",
			lrcName:       "",
			lrcSinger:     "",
			wantName:      "晚风",
			wantSinger:    "陈婧霏",
		},
		{
			name:          "Q4/关/歌词标签均为空退回本拍内存值",
			keepFirst:     false,
			claimedName:   "晚风",
			claimedSinger: "陈婧霏",
			memName:       "晚风 (内存)",
			memSinger:     "内存歌手",
			lrcName:       "",
			lrcSinger:     "",
			wantName:      "晚风 (内存)",
			wantSinger:    "内存歌手",
		},
		{
			name:          "Q4/开/认领歌手为空且歌词标签为空时退回内存补上歌手",
			keepFirst:     true,
			claimedName:   "晚风",
			claimedSinger: "",
			memName:       "晚风 (内存)",
			memSinger:     "内存歌手",
			lrcName:       "",
			lrcSinger:     "",
			wantName:      "晚风",
			wantSinger:    "内存歌手",
		},
		{
			name:          "Q4/关/歌词标签单项为空时各项独立退回内存值",
			keepFirst:     false,
			claimedName:   "晚风",
			claimedSinger: "陈婧霏",
			memName:       "内存歌名",
			memSinger:     "内存歌手",
			lrcName:       "",
			lrcSinger:     "标签歌手",
			wantName:      "内存歌名",
			wantSinger:    "标签歌手",
		},
		{
			name:          "Q5/开/认领歌名为空白串按空补为歌词标签歌名",
			keepFirst:     true,
			claimedName:   " ",
			claimedSinger: "陈婧霏",
			memName:       " ",
			memSinger:     "陈婧霏",
			lrcName:       "晚风",
			lrcSinger:     "陈婧霏、X",
			wantName:      "晚风",
			wantSinger:    "陈婧霏",
		},
		{
			name:          "Q5/开/认领歌手为空白串按空补为歌词标签歌手",
			keepFirst:     true,
			claimedName:   "晚风",
			claimedSinger: "  \t",
			memName:       "晚风",
			memSinger:     "",
			lrcName:       "晚风 (Live)",
			lrcSinger:     "陈婧霏",
			wantName:      "晚风",
			wantSinger:    "陈婧霏",
		},
		{
			name:          "Q5/开/认领为空而读到纯空白时不补正",
			keepFirst:     true,
			claimedName:   "",
			claimedSinger: " ",
			memName:       "",
			memSinger:     "",
			lrcName:       "  ",
			lrcSinger:     "\t ",
			wantName:      "",
			wantSinger:    " ",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { player.SetSongInfoKeepFirst(true) })
			player.SetSongInfoKeepFirst(tc.keepFirst)

			gotName, gotSinger := backfillNames(
				tc.claimedName, tc.claimedSinger,
				tc.memName, tc.memSinger,
				tc.lrcName, tc.lrcSinger,
			)
			if gotName != tc.wantName || gotSinger != tc.wantSinger {
				t.Fatalf("backfillNames(%q, %q, %q, %q, %q, %q) = (%q, %q), 期望 (%q, %q)",
					tc.claimedName, tc.claimedSinger, tc.memName, tc.memSinger, tc.lrcName, tc.lrcSinger,
					gotName, gotSinger, tc.wantName, tc.wantSinger)
			}
		})
	}
}
