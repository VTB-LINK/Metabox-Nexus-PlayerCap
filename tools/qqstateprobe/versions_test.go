//go:build windows

package main

import (
	"encoding/binary"
	"slices"
	"testing"
)

func TestKnownVersionsTable(t *testing.T) {
	// 与 mem.go knownVersions 的版本集合一致；字符串模型：21.81/22.16/22.22 为 SSO，其余为宽字符。
	sso := []string{"21.81", "22.16", "22.22"}
	want := []string{"20.05", "21.81", "22.16", "22.22", "22.26", "22.31", "22.41", "22.52", "22.60", "22.71", "22.76", "22.81", "22.82"}
	var got []string
	for k, o := range knownVersions {
		got = append(got, k)
		if o.Wide == slices.Contains(sso, k) {
			t.Errorf("%s Wide=%v，字符串模型不对", k, o.Wide)
		}
		if o.Struct1 == 0 {
			t.Errorf("%s Struct1 为 0", k)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("版本集合 %v，期望 %v", got, want)
	}
}

func TestPickOffsets(t *testing.T) {
	cases := []struct {
		in, key string
		exact   bool
	}{
		{"22.82", "22.82", true},
		{"20.05", "20.05", true},
		{"21.81", "21.81", true},
		{"22.83", "22.82", false},   // 比最新还新
		{"20.06", "20.05", false},   // 20.x 的其他小版本落到 20.05
		{"20.50", "20.05", false},   // 距 20.05 为 45，距 21.81 为 1031
		{"21.00", "21.81", false},   // 距 21.81 为 81，距 20.05 为 995
		{"22.19", "22.22", false},   // 与 22.16、22.22 等距，取较大者
		{"1.00", "20.05", false},    // 比最老还老
		{"unknown", "22.82", false}, // 读不到版本：退到最新
		{"", "22.82", false},
	}
	for _, c := range cases {
		if key, exact := pickOffsets(c.in); key != c.key || exact != c.exact {
			t.Errorf("pickOffsets(%q) = (%q, %v)，期望 (%q, %v)", c.in, key, exact, c.key, c.exact)
		}
	}
}

func sso(ptrOrText []byte, length, capacity uint32) []byte {
	b := make([]byte, 24)
	copy(b, ptrOrText)
	binary.LittleEndian.PutUint32(b[0x10:], length)
	binary.LittleEndian.PutUint32(b[0x14:], capacity)
	return b
}

func TestSSOFromBuf(t *testing.T) {
	heapPtr := binary.LittleEndian.AppendUint32(nil, 0x20000)
	readHeap := func(ptr, n uint32) []byte {
		if ptr != 0x20000 {
			t.Fatalf("读了堆地址 0x%X", ptr)
		}
		return []byte("Lettersong\x00")[:n]
	}
	cases := []struct {
		name string
		buf  []byte
		want string
	}{
		{"内联", sso([]byte("无地自容"), 12, 15), "无地自容"},
		{"堆", sso(heapPtr, 10, 31), "Lettersong"},
		{"clear() 后 length 为 0", sso(heapPtr, 0, 31), ""},
		{"堆指针过低", sso([]byte{0, 1, 0, 0}, 10, 31), ""},
		{"length 过大", sso(heapPtr, 1000, 2000), ""},
		{"内联首字节为 0", sso(nil, 0, 15), ""},
		{"不足 24 字节", []byte("abc"), ""},
	}
	for _, c := range cases {
		if got := ssoFromBuf(c.buf, readHeap); got != c.want {
			t.Errorf("%s：得到 %q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestWatchSet(t *testing.T) {
	var w watchList
	for _, s := range []string{"QQMusic.dll:0xD3BBA8", "QQMusicPlayer.dll:4096"} {
		if err := w.Set(s); err != nil {
			t.Fatalf("Set(%q)：%v", s, err)
		}
	}
	want := watchList{{"QQMusic.dll", 0xD3BBA8}, {"QQMusicPlayer.dll", 4096}}
	if !slices.Equal(w, want) {
		t.Fatalf("得到 %v，期望 %v", w, want)
	}
	for _, s := range []string{"QQMusic.dll", ":0x10", "QQMusic.dll:", "QQMusic.dll:zz", "QQMusic.dll:0x100000000", "QQMusic.dll:-4"} {
		if err := (&watchList{}).Set(s); err == nil {
			t.Errorf("Set(%q) 应报错", s)
		}
	}
}

func TestWatchSeries(t *testing.T) {
	h := &Header{Sections: []Section{
		{Module: "QQMusic.dll", Name: ".data", RVA: 0xD3B000, Size: 0x1000},
		{Module: "QQMusic.dll", Name: ".tls", RVA: 0xD40000, Size: 0x10},
	}}
	final := make([]byte, 0x1000)
	binary.LittleEndian.PutUint32(final[0xBAC:], 0x1234)
	changed := &series{init: 1, ticks: []int32{3}, vals: []uint32{0x0200}}
	d := &dataset{
		series: map[uint64]*series{0xBA8: changed},
		final:  [][]byte{final, nil},
	}
	ok := []struct {
		w     watch
		s     *series // nil=期望常量
		lane  int
		value uint32 // 第 5 拍取值
	}{
		{watch{"QQMusic.dll", 0xD3BBA8}, changed, -1, 0x0200},
		{watch{"qqmusic.dll", 0xD3BBA8}, changed, -1, 0x0200}, // 模块名不分大小写
		{watch{"QQMusic.dll", 0xD3BBA9}, changed, 1, 0x02},    // 非对齐按字节
		{watch{"QQMusic.dll", 0xD3BBAC}, nil, -1, 0x1234},     // 没变过：取结束时内容
	}
	for _, c := range ok {
		s, lane, err := watchSeries(h, d, c.w)
		if err != nil {
			t.Fatalf("%s：%v", c.w, err)
		}
		if (c.s != nil && s != c.s) || lane != c.lane || laneVal(s.at(5), lane) != c.value {
			t.Errorf("%s：lane %d 值 %#x，期望 lane %d 值 %#x", c.w, lane, laneVal(s.at(5), lane), c.lane, c.value)
		}
	}
	for _, w := range []watch{
		{"QQMusic.dll", 0xD3C000},       // 节外
		{"QQMusic.dll", 0xD3AFFC},       // 节前
		{"QQMusicPlayer.dll", 0xD3BBA8}, // 别的模块
		{"QQMusic.dll", 0xD40004},       // 节从未读成功
	} {
		if _, _, err := watchSeries(h, d, w); err == nil {
			t.Errorf("%s 应报错", w)
		}
	}
}
