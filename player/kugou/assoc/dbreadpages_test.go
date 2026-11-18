package assoc

// 本文件只测读库读了哪些页（设计 §6.4）：只读解析要用的页，不读与关联无关的表，也不整库读入。

import (
	"context"
	"slices"
	"testing"
)

func TestReadCandidatesReadsOnlyNeededPages(t *testing.T) {
	plain := fixturePlain(t, "kugou_like.db")
	share := tablePages(t, plain, "ShareFileItems")
	if len(share) < 2 {
		t.Fatalf("fixture: ShareFileItems spans %d page(s), want an interior page with leaves", len(share))
	}
	hash, want := tonghuaWant(t)
	m := encMemFS(t, plain)

	got, err := readCandidates(context.Background(), testDBPath, hash, m.fs())
	if err != nil {
		t.Fatalf("readCandidates: %v", err)
	}
	wantCandidates(t, got.cands, want)

	for _, n := range m.reads {
		if slices.Contains(share, n) {
			t.Fatalf("read page %d of ShareFileItems %v", n, share)
		}
	}
	// 反向：用到的表的每一页都读到了，上面的断言不是因为什么都没读而成立。
	for _, table := range []string{"LyricInfo", "LocalFile", "RecentlyPlayedItem", "PlayQueueAssistItem_0"} {
		for _, n := range tablePages(t, plain, table) {
			if !slices.Contains(m.reads, n) {
				t.Errorf("page %d of %s not read", n, table)
			}
		}
	}
}
