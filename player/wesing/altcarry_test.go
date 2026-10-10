package wesing

// 本文件只测改用候选之后的候选名单（swapAlt）：被换下的地址进名单，随缓存地址带到之后的歌。

import "testing"

// 名单要跟着缓存地址一起沿用到下一首（runSession 的 cachedAltAddrs）。换地址之后若把被换下的那个丢掉，
// 名单就空了，下一首又回到「沿用的地址一旦是错的就换不回来」；若把刚选中的那个留在名单里，则是自己
// 盯自己。
//
// 变异自证：把 swapAlt 里的 `a = prev` 删掉（名单原样返回），「两个候选」「只有一个候选」两例里刚选中的
// 那个还在名单里、被换下的不在而红。
func TestSwapAltKeepsReplacedAddress(t *testing.T) {
	const stale, live, other = 0x103A12EC, 0x03E8FB08, 0x0FF31C40
	cases := []struct {
		name       string
		alts       []uint32
		next, prev uint32
		want       []uint32
	}{
		{"只有一个候选", []uint32{live}, live, stale, []uint32{stale}},
		{"两个候选", []uint32{other, live}, live, stale, []uint32{other, stale}},
		{"没有候选", nil, live, stale, []uint32{}},
	}
	for _, c := range cases {
		got := swapAlt(c.alts, c.next, c.prev)
		if len(got) != len(c.want) {
			t.Errorf("%s：got %x，want %x", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s：got %x，want %x", c.name, got, c.want)
				break
			}
		}
	}
}
