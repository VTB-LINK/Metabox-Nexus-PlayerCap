package player

// 本文件只测 SameLyricText 的模糊匹配边界：它判断两段歌词文本是否为「同一句」，用于把
// 不同来源（CDP / Redux / API）拿到的同一句歌词对上。判太松会把不同句子混为一句、判太严
// 会让本该合并的两句各走各的，所以正反两向都要钉住。

import "testing"

func TestSameLyricTextMatchesSmallMiddleGapsByBoundary(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
		want  bool
	}{
		{
			name:  "exact after punctuation normalization",
			left:  "You don't know Oh oh",
			right: "You dont know, oh-oh",
			want:  true,
		},
		{
			name:  "missing article",
			left:  "I can see the pain living in your eyes",
			right: "I can see pain living in your eyes",
			want:  true,
		},
		{
			name:  "missing middle word",
			left:  "There's nothing left to try now it's gonna hurt us both",
			right: "There's nothing left to try it's gonna hurt us both",
			want:  true,
		},
		{
			name:  "single character spelling variant",
			left:  "I'll never criticise all you've ever meant to my life",
			right: "I'll never criticize all you've ever meant to my life",
			want:  true,
		},
		{
			name:  "early single character deletion",
			left:  "I'd like To make myself believe",
			right: "I'd lke To make myself believe",
			want:  true,
		},
		{
			name:  "different middle keyword",
			left:  "I don't want to let you down",
			right: "I don't want to hold you down",
			want:  false,
		},
		{
			name:  "different verb",
			left:  "You would never ask me why",
			right: "You would never tell me why",
			want:  false,
		},
		{
			name:  "different repeated na counts",
			left:  "Na na na na na na na na na na",
			right: "Na na na na na na na",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SameLyricText(tt.left, tt.right); got != tt.want {
				t.Fatalf("SameLyricText() = %v, want %v", got, tt.want)
			}
		})
	}
}
