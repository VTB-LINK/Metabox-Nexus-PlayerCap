package lyric

// 本文件只测搜歌候选的匹配判据：时长容差与歌手匹配。它们决定 SearchSongID 从候选里挑
// 哪一首——判松了会挑错歌、把别的歌歌词推上去。

import "testing"

func TestDurationWithinTolerance(t *testing.T) {
	tests := []struct {
		name      string
		candidate int
		target    int
		want      bool
	}{
		{name: "exact", candidate: 240000, target: 240000, want: true},
		{name: "within positive five percent", candidate: 252000, target: 240000, want: true},
		{name: "within negative five percent", candidate: 228000, target: 240000, want: true},
		{name: "outside positive five percent", candidate: 252001, target: 240000, want: false},
		{name: "outside negative five percent", candidate: 227999, target: 240000, want: false},
		{name: "unknown target accepts", candidate: 0, target: 0, want: true},
		{name: "unknown candidate rejects when target known", candidate: 0, target: 240000, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := durationWithinTolerance(tt.candidate, tt.target, 0.05)
			if got != tt.want {
				t.Fatalf("durationWithinTolerance(%d, %d) = %v, want %v", tt.candidate, tt.target, got, tt.want)
			}
		})
	}
}

func TestArtistMatches(t *testing.T) {
	artists := []struct {
		Name string `json:"name"`
	}{{Name: "伍佰"}, {Name: "China Blue"}}

	if !artistMatches(artists, "伍佰 & China Blue") {
		t.Fatalf("artistMatches() = false, want true")
	}
	if artistMatches(artists, "王菲") {
		t.Fatalf("artistMatches() = true, want false")
	}
}
