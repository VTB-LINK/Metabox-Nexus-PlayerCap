package lyric

// 本文件测伴奏解码器 CAudioDecodeForPCM 的挑选（设计 §4.1）：+0x04 路径（CString）的边界、
// pickAccompaniment 的四条与唯一性、等待就绪、mid 交叉核对。假内存沿用 roma_test.go 的 fakeMem，
// 按真机布局（pcm.go 头注释）摆出实例。

import (
	"strings"
	"testing"
	"unicode/utf16"
)

const (
	testPCMVtable = 0x6A2B3C40
	testSongMID   = "000AkNWi0qab5P" // 这首歌
	testPrevMID   = "003msPI80HftbW" // 上一首
)

// pcmDir 是真机形状的解码文件目录：…\<mid>\。
func pcmDir(mid string) string {
	return `C:\Users\u\AppData\Roaming\Tencent\WeSing\WeSingCache\WeSingDL\Res\` + mid + `\`
}

// pcmPath 是真机形状的解码文件路径：…\<mid>\<mid><kind>。
func pcmPath(mid, kind string) string { return pcmDir(mid) + mid + kind }

var (
	accPath = pcmPath(testSongMID, "_accompany.pcm")
	konPath = pcmPath(testSongMID, "_kongsinger.pcm")
)

// pcmInst 在 addr 处摆一个 CAudioDecodeForPCM 实例：虚表、路径、总字节与解码字节。
func (f *fakeMem) pcmInst(addr, vtable uint32, path string, total, pos uint32) {
	f.put32(addr, vtable)
	f.put32(addr+pcmPathOff, f.cstring(path))
	f.put32(addr+8, 0)
	f.put32(addr+pcmTotalOff, total)
	f.put32(addr+pcmPosOff, pos)
}

// setPath 把实例的路径换成 path（新分配一个 CString，同真机切歌时路径随歌更换）。
func (f *fakeMem) setPath(addr uint32, path string) { f.put32(addr+pcmPathOff, f.cstring(path)) }

// rawCString 在 ptr 处摆一个头部字符数为 n、内容为 units 的 CString。units 可以比 n 短：后面的字符没有映射，
// 用来造「字符数跨进不可读的页」。
func (f *fakeMem) rawCString(ptr, n uint32, units []uint16) {
	f.put32(ptr-cstringLenOffset, n)
	for i, u := range units {
		f.b[ptr+uint32(i)*2] = byte(u)
		f.b[ptr+uint32(i)*2+1] = byte(u >> 8)
	}
}

// 路径的 CString（设计 §4.1 第 2 条）：数据指针不小于 0x10000，字符数在 1～1024，读不出不收，代理对按
// UTF-16 解码。字符数是从别人进程里读来的，离谱的长度不能照着去读。
//
// 变异自证（实测）：数据指针下限改成 0x10001，「指针恰为 0x10000」红；改成 0xFFFF，「指针 0xFFFF」红；
// 字符数上限改成 1023，「1024 个字符」红（用例起初引用了常量，这条变异没红，改成字面量之后才红）；去掉
// 上限，「1025 个字符」照着读了 2050 字节而红；不拒长度为 0（readPCMPath 只看 ok），「长度为 0」红；逐个
// 码元转 rune 而不整体 utf16.Decode，「代理对」红。
func TestReadPCMPath(t *testing.T) {
	ascii := func(s string) []uint16 { return utf16.Encode([]rune(s)) }
	// 边界写成字面量，不引用 pcmMaxPathChars：引用常量的话改了常量用例跟着变，测不出来。
	t.Run("字符数上限", func(t *testing.T) {
		f := newFakeMem()
		if _, ok := readPCMPath(f, f.cstring(strings.Repeat("a", 1025))); ok {
			t.Error("1025 个字符应不收")
		}
		if f.maxRead > 2048 {
			t.Errorf("不应照着 1025 个字符去读（被要求读 %d 字节）", f.maxRead)
		}
		if got, ok := readPCMPath(f, f.cstring(strings.Repeat("a", 1024))); !ok || len(got) != 1024 {
			t.Errorf("1024 个字符应收下，得到 %d 个字符 %v", len(got), ok)
		}
	})

	surrogate := `C:\Res\𠀀𠀁\x_accompany.pcm` // 两个扩展汉字，各占一对代理码元
	cases := []struct {
		name  string
		build func(f *fakeMem) uint32
		want  string
		ok    bool
	}{
		{"对照组", func(f *fakeMem) uint32 { return f.cstring(accPath) }, accPath, true},
		{"一个字符", func(f *fakeMem) uint32 { return f.cstring("x") }, "x", true},
		{"数据指针为 0", func(f *fakeMem) uint32 { return 0 }, "", false},
		{"指针 0xFFFF（头部与内容都读得出）", func(f *fakeMem) uint32 {
			f.rawCString(0xFFFF, 1, ascii("x"))
			return 0xFFFF
		}, "", false},
		{"指针恰为 0x10000", func(f *fakeMem) uint32 {
			f.rawCString(0x10000, 1, ascii("x"))
			return 0x10000
		}, "x", true},
		{"长度为 0", func(f *fakeMem) uint32 { return f.cstring("") }, "", false},
		{"头部读不出", func(f *fakeMem) uint32 {
			p := f.cstring(accPath)
			for i := uint32(0); i < 4; i++ {
				delete(f.b, p-cstringLenOffset+i)
			}
			return p
		}, "", false},
		{"字符数跨进不可读的页", func(f *fakeMem) uint32 {
			f.rawCString(0x00900000, 100, ascii(strings.Repeat("a", 60)))
			return 0x00900000
		}, "", false},
		{"代理对", func(f *fakeMem) uint32 { return f.cstring(surrogate) }, surrogate, true},
	}
	for _, c := range cases {
		f := newFakeMem()
		if got, ok := readPCMPath(f, c.build(f)); got != c.want || ok != c.ok {
			t.Errorf("%s：得到 (%q, %v)，want (%q, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
	// 代理对计两个码元：字符数按码元算，少算一个就截掉最后一个字符。
	if n := len(utf16.Encode([]rune(surrogate))); n != len([]rune(surrogate))+2 {
		t.Fatalf("用例前提不成立：%d 个码元", n)
	}
}

// 挑选的四条与唯一性（设计 §4.1）。实例地址取真机的量级，命中按地址升序给出（同 AOBScan）。
//
// 变异自证（实测）：
//   - 对齐命中满 8 个即截断（照搬 pickGraphInstances）：「9 个、第 9 个才是伴奏」得 pickNone、「前 8 个里有
//     残留伴奏」选中残留的那个，均红；判不唯一的条件写成 >= 8：「恰为 8 个」「8 个加 3 个未对齐」红；
//   - 去掉对齐判断：「未对齐的伴奏不看」被选中、「8 个加 3 个未对齐」判为不唯一而红；
//   - 去掉虚表判断：「虚表不等」红；后缀改成区分大小写：「后缀大写」红；后缀改成 Contains：「后缀之后还有字」红；
//   - 去掉总字节 > 0：「总字节为 0」「一个合格、另一个伴奏只差总字节」红；上限改成不含 7200 s：「总字节恰为
//     上限」红；去掉上限：「总字节超上限」红；上限常量改成 7199 s：「总字节恰为上限」红；
//   - 去掉 pos <= total：「解码字节超过总字节」「一个合格、另一个伴奏解码字节超过总字节」红；改成 pos < total：
//     「解码字节等于总字节」红；
//   - 合格的不止一个时取第一个：「两个伴奏都合格」红。
func TestPickAccompaniment(t *testing.T) {
	const a0, a1, a2 = 0x0B0E0000, 0x0B0E0100, 0x0B0E0200
	const maxTotal = 7200 * 176400 // 7200 s 的字节数，写成字面量，理由同 TestReadPCMPath
	total, pos := secBytes(183.693), secBytes(12.3)
	shape := func(f *fakeMem) []uint32 { // 真机的形状：伴奏、原唱、空路径各一
		f.pcmInst(a0, testPCMVtable, accPath, total, pos)
		f.pcmInst(a1, testPCMVtable, konPath, total, pos)
		f.pcmInst(a2, testPCMVtable, "", 0, 0)
		return []uint32{a0, a1, a2}
	}
	one := func(path string, total, pos uint32) func(f *fakeMem) []uint32 {
		return func(f *fakeMem) []uint32 {
			f.pcmInst(a0, testPCMVtable, path, total, pos)
			return []uint32{a0}
		}
	}
	// many 摆 n 个对齐的原唱实例，再按 accAt 把其中几个换成伴奏（下标从 0 起），返回全部命中。
	many := func(n int, accAt ...int) func(f *fakeMem) []uint32 {
		return func(f *fakeMem) []uint32 {
			var hits []uint32
			for i := range n {
				a := uint32(0x0C000000 + i*0x600)
				f.pcmInst(a, testPCMVtable, konPath, total, pos)
				hits = append(hits, a)
			}
			for _, i := range accAt {
				f.setPath(hits[i], accPath)
			}
			return hits
		}
	}
	cases := []struct {
		name  string
		build func(f *fakeMem) []uint32
		want  uint32
		res   pickResult
	}{
		{"真机的形状：伴奏、原唱、空路径各一", shape, a0, pickOK},
		{"只有原唱与空路径", func(f *fakeMem) []uint32 { return shape(f)[1:] }, 0, pickNone},
		{"没有命中", func(*fakeMem) []uint32 { return nil }, 0, pickNone},
		{"两个伴奏都合格（停住的残留 A 与在走的 B 同名同路径）", func(f *fakeMem) []uint32 {
			f.pcmInst(a0, testPCMVtable, accPath, total, secBytes(30))
			f.pcmInst(a1, testPCMVtable, accPath, total, pos)
			return []uint32{a0, a1}
		}, 0, pickNotUnique},
		{"对齐命中 9 个、第 9 个才是伴奏", many(9, 8), 0, pickNotUnique},
		{"对齐命中 9 个、前 8 个里有残留伴奏、第 9 个是在走的伴奏", many(9, 2, 8), 0, pickNotUnique},
		{"对齐命中恰为 8 个", many(8, 7), 0x0C000000 + 7*0x600, pickOK},
		{"对齐 8 个加 3 个未对齐", func(f *fakeMem) []uint32 {
			return append([]uint32{0x0BFFFFF1, 0x0BFFFFF2, 0x0BFFFFF3}, many(8, 7)(f)...)
		}, 0x0C000000 + 7*0x600, pickOK},
		{"未对齐的伴奏不看", func(f *fakeMem) []uint32 {
			f.pcmInst(a0+2, testPCMVtable, accPath, total, pos)
			return []uint32{a0 + 2}
		}, 0, pickNone},
		{"虚表不等", func(f *fakeMem) []uint32 {
			f.pcmInst(a0, testPCMVtable+0x10, accPath, total, pos)
			return []uint32{a0}
		}, 0, pickNone},
		{"实例读不出", func(*fakeMem) []uint32 { return []uint32{a0} }, 0, pickNone},
		{"只读得出前 16 字节", func(f *fakeMem) []uint32 {
			f.pcmInst(a0, testPCMVtable, accPath, total, pos)
			for i := uint32(0); i < 4; i++ {
				delete(f.b, a0+pcmPosOff+i)
			}
			return []uint32{a0}
		}, 0, pickNone},
		{"路径读不出（指针小于 0x10000）", func(f *fakeMem) []uint32 {
			f.pcmInst(a0, testPCMVtable, accPath, total, pos)
			f.put32(a0+pcmPathOff, 0x8000)
			return []uint32{a0}
		}, 0, pickNone},
		{"后缀大写", one(pcmPath(testSongMID, "_ACCOMPANY.PCM"), total, pos), a0, pickOK},
		{"后缀之后还有字", one(accPath+".bak", total, pos), 0, pickNone},
		{"只有后缀没有 mid", one("_accompany.pcm", total, pos), a0, pickOK},
		{"总字节为 0", one(accPath, 0, 0), 0, pickNotReady},
		{"总字节恰为上限", one(accPath, maxTotal, pos), a0, pickOK},
		{"总字节超上限", one(accPath, maxTotal+1, pos), 0, pickNotReady},
		{"解码字节超过总字节", one(accPath, total, total+1), 0, pickNone},
		{"解码字节等于总字节", one(accPath, total, total), a0, pickOK},
		{"一个合格、另一个伴奏只差总字节", func(f *fakeMem) []uint32 {
			f.pcmInst(a0, testPCMVtable, accPath, 0, 0)
			f.pcmInst(a1, testPCMVtable, accPath, total, pos)
			return []uint32{a0, a1}
		}, a1, pickOK},
		{"一个合格、另一个伴奏解码字节超过总字节", func(f *fakeMem) []uint32 {
			f.pcmInst(a0, testPCMVtable, accPath, total, pos)
			f.pcmInst(a1, testPCMVtable, accPath, total, total+1)
			return []uint32{a0, a1}
		}, a0, pickOK},
	}
	for _, c := range cases {
		f := newFakeMem()
		got, res := pickAccompaniment(f, testPCMVtable, c.build(f))
		if got != c.want || res != c.res {
			t.Errorf("%s：得到 (%#x, %v)，want (%#x, %v)", c.name, got, res, c.want, c.res)
		}
	}
}

// 等待就绪（设计 §4.1）：只在「有伴奏实例、只差总字节」时隔 200 ms 再挑，至多再挑 5 次；其余结论立即返回。
//
// 变异自证（实测）：等待条件改成「没选中就等」，「没有伴奏实例」「不唯一」「解码字节超过总字节」三例
// 各等了 5 次而红；次数上限改成 4，「第 5 次再挑才写好」「1 s 内一直不就绪」红；改成 6，「1 s 内一直
// 不就绪」等了 6 次而红。
func TestPickReady(t *testing.T) {
	const a0, a1 = 0x0B0E0000, 0x0B0E0100
	total, pos := secBytes(183.693), secBytes(0.4)
	cases := []struct {
		name      string
		build     func(f *fakeMem)
		readyAt   int // 第几次等待之后把 a0 的总字节写好（0 = 不写）
		want      uint32
		res       pickResult
		wantWaits int
	}{
		{"总字节先 0、第 3 次再挑时写好", func(f *fakeMem) { f.pcmInst(a0, testPCMVtable, accPath, 0, 0) }, 3, a0, pickOK, 3},
		{"第 5 次再挑才写好", func(f *fakeMem) { f.pcmInst(a0, testPCMVtable, accPath, 0, 0) }, 5, a0, pickOK, 5},
		{"1 s 内一直不就绪", func(f *fakeMem) { f.pcmInst(a0, testPCMVtable, accPath, 0, 0) }, 0, 0, pickNotReady, 5},
		{"一开始就就绪", func(f *fakeMem) { f.pcmInst(a0, testPCMVtable, accPath, total, pos) }, 0, a0, pickOK, 0},
		{"没有伴奏实例：不等", func(f *fakeMem) { f.pcmInst(a0, testPCMVtable, konPath, 0, 0) }, 0, 0, pickNone, 0},
		{"不唯一：不等", func(f *fakeMem) {
			f.pcmInst(a0, testPCMVtable, accPath, total, pos)
			f.pcmInst(a1, testPCMVtable, accPath, total, pos)
		}, 0, 0, pickNotUnique, 0},
		{"解码字节超过总字节：不等", func(f *fakeMem) { f.pcmInst(a0, testPCMVtable, accPath, total, total+1) }, 0, 0, pickNone, 0},
	}
	for _, c := range cases {
		f := newFakeMem()
		c.build(f)
		waits := 0
		got, res := pickReady(f, testPCMVtable, []uint32{a0, a1}, func() {
			waits++
			if waits == c.readyAt {
				f.put32(a0+pcmTotalOff, total)
				f.put32(a0+pcmPosOff, pos)
			}
		})
		if got != c.want || res != c.res || waits != c.wantWaits {
			t.Errorf("%s：得到 (%#x, %v)、等了 %d 次，want (%#x, %v)、%d 次", c.name, got, res, waits, c.want, c.res, c.wantWaits)
		}
	}
}

// mid 交叉核对（设计 §4.1）：选中之后、宣告之前，路径的文件名须为 <mid>_accompany.pcm；不符时隔 200 ms
// 重读，至多 5 次。mid 为空时跳过。核对通过的那次读数就是宣告用的总时长与初始位置。
//
// 变异自证（实测）：
//   - 不核对 mid：「先旧后新」「切歌那一下」「第 5 次重读才相符」不等就通过，「一直不符」「目录是这首、文件名是
//     上一首」通过，均红；
//   - 拿整条路径比 HasSuffix(<mid>_accompany.pcm)：「文件名多一个前缀」「mid 只是文件名的后半截」红；
//   - 拿路径里有没有 \<mid>\ 来比：「目录是这首、文件名是上一首」「文件名多一个前缀」「路径不带目录」红；
//   - 文件名比较区分大小写：「后缀大写」红；
//   - 次数上限改成 4：「第 5 次重读才相符」「一直不符」红；改成 6：「一直不符」等三例等了 6 次而红；
//   - mid 为空时也重读：「mid 为空、实例已不合格」等了 5 次而红；
//   - 返回第一次的读数而不是核对通过那次的：「先旧后新」「切歌那一下」「第 5 次重读才相符」的总字节与位置
//     不对而红；
//   - 重读时总字节不合格也放行：「切歌那一下」在总字节为 0 时就通过、「mid 为空、实例已不合格」通过而红。
func TestConfirmPCM(t *testing.T) {
	const inst = 0x0B0E0000
	prevTotal, total := secBytes(201.5), secBytes(183.693)
	prevAcc := pcmPath(testPrevMID, "_accompany.pcm")
	type step func(f *fakeMem) // 第 k 次等待之后对内存做的改动
	cases := []struct {
		name      string
		path      string
		total     uint32
		mid       string
		steps     map[int]step
		ok        bool
		wantWaits int
		wantTotal uint32 // ok 时宣告用的总字节
	}{
		{"相符", accPath, total, testSongMID, nil, true, 0, total},
		{"先旧后新：第 2 次重读换成这首歌", prevAcc, prevTotal, testSongMID, map[int]step{
			2: func(f *fakeMem) {
				f.setPath(inst, accPath)
				f.put32(inst+pcmTotalOff, total)
				f.put32(inst+pcmPosOff, 0)
			},
		}, true, 2, total},
		{"切歌那一下：上一首、清零、再是这首", prevAcc, prevTotal, testSongMID, map[int]step{
			1: func(f *fakeMem) { f.setPath(inst, accPath); f.put32(inst+pcmTotalOff, 0); f.put32(inst+pcmPosOff, 0) },
			3: func(f *fakeMem) { f.put32(inst+pcmTotalOff, total) },
		}, true, 3, total},
		{"第 5 次重读才相符", prevAcc, prevTotal, testSongMID, map[int]step{
			5: func(f *fakeMem) {
				f.setPath(inst, accPath)
				f.put32(inst+pcmTotalOff, total)
				f.put32(inst+pcmPosOff, 0)
			},
		}, true, 5, total},
		{"一直不符", prevAcc, prevTotal, testSongMID, nil, false, 5, 0},
		{"目录是这首、文件名是上一首", pcmDir(testSongMID) + testPrevMID + "_accompany.pcm", total, testSongMID, nil, false, 5, 0},
		{"文件名多一个前缀", pcmDir(testSongMID) + "X" + testSongMID + "_accompany.pcm", total, testSongMID, nil, false, 5, 0},
		{"mid 只是文件名的后半截", accPath, total, testSongMID[3:], nil, false, 5, 0},
		{"后缀大写", pcmPath(testSongMID, "_ACCOMPANY.PCM"), total, testSongMID, nil, true, 0, total},
		{"路径不带目录", testSongMID + "_accompany.pcm", total, testSongMID, nil, true, 0, total},
		{"mid 为空：跳过核对", prevAcc, prevTotal, "", nil, true, 0, prevTotal},
		{"mid 为空、实例已不合格：不重读", accPath, 0, "", nil, false, 0, 0},
	}
	for _, c := range cases {
		f := newFakeMem()
		f.pcmInst(inst, testPCMVtable, c.path, c.total, secBytes(0.4))
		waits := 0
		clock, err := confirmPCM(f, testPCMVtable, inst, c.mid, func() {
			waits++
			if s := c.steps[waits]; s != nil {
				s(f)
			}
		})
		if (err == nil) != c.ok || waits != c.wantWaits {
			t.Errorf("%s：err = %v、等了 %d 次，want ok=%v、%d 次", c.name, err, waits, c.ok, c.wantWaits)
			continue
		}
		if !c.ok {
			if clock != nil {
				t.Errorf("%s：不通过时应返回 nil，得到 %+v", c.name, clock)
			}
			continue
		}
		wantPos := float32(float64(secBytes(0.4)) / pcmBytesPerSec)
		if c.steps != nil {
			wantPos = 0
		}
		if clock.total != c.wantTotal || clock.Addr() != inst || clock.Position() != wantPos {
			t.Errorf("%s：总字节 %d、地址 %#x、位置 %v，want %d、%#x、%v", c.name, clock.total, clock.Addr(), clock.Position(), c.wantTotal, inst, wantPos)
		}
	}
}
