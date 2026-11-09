package lyric

// 本文件只测「何时以 CKSongsGraph 的总时长纠正宣告时的值」（设计 §4.3）：adoptTotal 的判据，
// 以及 SongsGraphWatch.Check 把读数、同步窗口、采用判据串起来之后的行为。假内存沿用 roma_test.go 的 fakeMem。

import (
	"fmt"
	"math"
	"testing"
)

// 变异自证：歧义判据改成 >= 1，「相差 1 s」一例红；去掉歧义判据，「相差 1.01 s」「三个里最大最小差 1.2 s」
// 红；floor 改成四舍五入，「183.693 对 183」「182.9999 对 183」「旧值更长」红；去掉来源判断（估值也按整秒
// 比较），「估值恰与 floor 相等」红；取最后一个同步候选而非第一个，「相差 1 s」一例取到 201 而红。
func TestAdoptTotal(t *testing.T) {
	cases := []struct {
		name   string
		synced []float64
		cur    float32
		src    DurationSource
		want   float32 // 0 表示不采用
	}{
		{"无同步候选", nil, 180, DurationFromUI, 0},
		{"界面：183.693 对 183（界面按截断显示）", []float64{183.693}, 183, DurationFromUI, 0},
		{"界面：恰为整秒 183 对 183", []float64{183}, 183, DurationFromUI, 0},
		{"界面：上一首的 180 对这首 181.4", []float64{181.4}, 180, DurationFromUI, 181.4},
		{"界面：182.9999 对 183", []float64{182.9999}, 183, DurationFromUI, 182.9999},
		{"界面：旧值更长 184 对 183.693", []float64{183.693}, 184, DurationFromUI, 183.693},
		{"界面：失败时的 0", []float64{183.693}, 0, DurationFromUI, 183.693},
		{"估值：末行 + 10 s", []float64{183.693}, 193.2, DurationEstimated, 183.693},
		{"估值恰与 floor 相等也纠正", []float64{183.693}, 183, DurationEstimated, 183.693},
		{"两个同步候选相差 1 s：取第一个", []float64{200, 201}, 190, DurationFromUI, 200},
		{"两个同步候选相差 1.01 s：歧义", []float64{200, 201.01}, 190, DurationFromUI, 0},
		{"三个里最大最小差 1.2 s：歧义", []float64{200.5, 200, 201.2}, 190, DurationFromUI, 0},
		{"两个同步候选都与界面相符", []float64{200.3, 200.9}, 200, DurationFromUI, 0},
	}
	for _, c := range cases {
		got, ok := adoptTotal(c.synced, c.cur, c.src)
		if ok != (c.want != 0) || got != c.want {
			t.Errorf("%s：得到 (%v, %v)，want %v", c.name, got, ok, c.want)
		}
	}
}

const testGraphVtable = 0x6A1B2C30

// graphInst 是假内存里的一个 CKSongsGraph 实例。
type graphInst struct {
	f    *fakeMem
	addr uint32
}

func secBytes(s float64) uint32 { return uint32(math.Round(s * pcmBytesPerSec)) }

// set 写这一拍的首 dword、总时长与解码位置。
func (g graphInst) set(vtable uint32, total, pos float64) {
	g.f.put32(g.addr, vtable)
	g.f.put32(g.addr+graphTotalOff, secBytes(total))
	g.f.put32(g.addr+graphPosOff, secBytes(pos))
}

// unmapPos 让解码位置读不出。
func (g graphInst) unmapPos() {
	for i := uint32(0); i < 4; i++ {
		delete(g.f.b, g.addr+graphPosOff+i)
	}
}

// 逐拍喂 SongsGraphWatch：每拍先按 frame 摆好各实例，再以播放时间 t 调 Check。返回纠正发生在第几拍
// （从 1 起，0 表示没有纠正）与纠正后的值；纠正之后的各拍仍调 Check（cur 仍传旧值），断言不再纠正。
func runWatch(t *testing.T, g *SongsGraphWatch, cur float32, ticks int, frame func(tick int, tm float64)) (int, float32) {
	t.Helper()
	at, val := 0, float32(0)
	for i := 1; i <= ticks; i++ {
		tm := float64(i)
		frame(i, tm)
		d, ok := g.Check(float32(tm), cur)
		if !ok {
			continue
		}
		if at != 0 {
			t.Fatalf("第 %d 拍又纠正了一次（%v），第 %d 拍已纠正过", i, d, at)
		}
		at, val = i, d
		g.Adopted(cur, d)
	}
	return at, val
}

// 变异自证：Adopted 里不置 done（采用后不停），「正常」一例第 7 拍又纠正而红；readGraph 不核对首 dword，
// 「首 dword 中途不是虚表」一例第 6 拍就纠正而红。
func TestSongsGraphWatchCheck(t *testing.T) {
	const live, stale = 0x0B0EDD50, 0x0A000040
	type want struct {
		at  int
		val float64
	}
	cases := []struct {
		name  string
		insts []uint32
		src   DurationSource
		cur   float32
		frame func(f *fakeMem, tick int, tm float64)
		want  want
	}{
		{"正常：第 6 拍纠正、之后不再纠正", []uint32{live}, DurationFromUI, 180,
			func(f *fakeMem, _ int, tm float64) { graphInst{f, live}.set(testGraphVtable, 181.4, tm+0.05) },
			want{6, 181.4}},
		{"界面值与它相符：不纠正", []uint32{live}, DurationFromUI, 183,
			func(f *fakeMem, _ int, tm float64) { graphInst{f, live}.set(testGraphVtable, 183.693, tm+0.05) },
			want{}},
		{"首 dword 中途不是虚表：窗口清空，从第 5 拍重新攒", []uint32{live}, DurationFromUI, 180,
			func(f *fakeMem, tick int, tm float64) {
				vt := uint32(testGraphVtable)
				if tick == 4 {
					vt = 0x6A1B2D00
				}
				graphInst{f, live}.set(vt, 181.4, tm+0.05)
			},
			want{10, 181.4}},
		{"解码位置中途读不出：窗口清空，从第 5 拍重新攒", []uint32{live}, DurationFromUI, 180,
			func(f *fakeMem, tick int, tm float64) {
				g := graphInst{f, live}
				g.set(testGraphVtable, 181.4, tm+0.05)
				if tick == 4 {
					g.unmapPos()
				}
			},
			want{10, 181.4}},
		{"停住的残留对象排在前面：只认在走的那个", []uint32{stale, live}, DurationEstimated, 160,
			func(f *fakeMem, _ int, tm float64) {
				graphInst{f, stale}.set(testGraphVtable, 300, 3)
				graphInst{f, live}.set(testGraphVtable, 181.4, tm+0.05)
			},
			want{6, 181.4}},
		{"两个都在走、总时长相差 1.5 s：歧义，不纠正", []uint32{stale, live}, DurationEstimated, 160,
			func(f *fakeMem, _ int, tm float64) {
				graphInst{f, stale}.set(testGraphVtable, 182.9, tm+0.05)
				graphInst{f, live}.set(testGraphVtable, 181.4, tm+0.05)
			},
			want{}},
		{"没有实例", nil, DurationEstimated, 160, func(*fakeMem, int, float64) {}, want{}},
	}
	for _, c := range cases {
		f := newFakeMem()
		g := newSongsGraphWatch(f, testGraphVtable, c.insts, 150, c.src)
		at, val := runWatch(t, g, c.cur, 12, func(tick int, tm float64) { c.frame(f, tick, tm) })
		if at != c.want.at || math.Abs(float64(val)-c.want.val) > 1e-3 {
			t.Errorf("%s：第 %d 拍纠正为 %v，want 第 %d 拍纠正为 %v", c.name, at, val, c.want.at, c.want.val)
		}
	}
}

// 纠正后的 all_lyrics 入队失败（事件通道满）时调用方不调 Adopted：之后各拍 Check 仍返回纠正，直到 Adopted。
//
// 变异自证：Check 在返回纠正时就置 done（即改动前的写法），第 7 拍不再返回纠正而红（实测）。
func TestSongsGraphWatchRetriesUntilAdopted(t *testing.T) {
	const live = 0x0B0EDD50
	f := newFakeMem()
	g := newSongsGraphWatch(f, testGraphVtable, []uint32{live}, 150, DurationFromUI)
	var got []int
	for i := 1; i <= 10; i++ {
		tm := float64(i)
		graphInst{f, live}.set(testGraphVtable, 181.4, tm+0.05)
		if _, ok := g.Check(float32(tm), 180); ok {
			got = append(got, i)
			if i == 8 { // 第 6、7 拍入队失败，第 8 拍成功
				g.Adopted(180, 181.4)
			}
		}
	}
	if fmt.Sprint(got) != "[6 7 8]" {
		t.Fatalf("返回纠正的拍 = %v，want [6 7 8]", got)
	}
}

// 本会话 CKSongsGraph 不可用时 wesing.go 传 nil：Check 恒不纠正。
func TestSongsGraphWatchNil(t *testing.T) {
	var g *SongsGraphWatch
	if d, ok := g.Check(10, 180); ok {
		t.Fatalf("nil 也纠正了：%v", d)
	}
}
