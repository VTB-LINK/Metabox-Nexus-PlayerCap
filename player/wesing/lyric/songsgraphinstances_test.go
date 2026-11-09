package lyric

// 本文件只测 pickGraphInstances：虚表值的命中里只留 4 字节对齐的，至多 maxGraphInstances 个。

import (
	"slices"
	"testing"
)

// 变异自证：去掉对齐判断，未对齐的命中占了名额而红；去掉个数上限，留下 10 个而红。
func TestPickGraphInstances(t *testing.T) {
	hits := []uint32{0x0B0EDD51, 0x0B0EDD50} // 未对齐的排在前面
	for a := uint32(0x0C000000); a < 0x0C000000+9*0x600; a += 0x600 {
		hits = append(hits, a)
	}
	got := pickGraphInstances(hits)
	want := []uint32{0x0B0EDD50}
	for a := uint32(0x0C000000); len(want) < maxGraphInstances; a += 0x600 {
		want = append(want, a)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("得到 %#x，want %#x", got, want)
	}
	if got := pickGraphInstances(nil); len(got) != 0 {
		t.Errorf("没有命中时得到 %#x", got)
	}
}
