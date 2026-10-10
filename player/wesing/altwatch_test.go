package wesing

// 本文件只测 altWatch：当前的播放时间地址停着、而定位时落选的候选在走，改用那个候选。

import "testing"

// anyValid 是不做复核的 valid：哪个候选都算还能改用。
func anyValid(uint32) bool { return true }

// 定位那一刻歌是暂停的，取了排在前面、时间恒为 0 的那个结构；之后开播，在走的是另一个。
// 约每秒检查一次：第一次只记基准，之后当前地址没动、候选涨了约 1 秒，就改用它。
//
// 变异自证：把 check 里的 `mine != prevMine` 去掉（当前地址在走时也照换），「当前地址在走」一例
// 换走而红；把 `d > altAdvanceMin` 去掉，「候选只是抖动」一例换走而红；把 `d < altAdvanceMax` 去掉，
// 「候选乱跳」一例换走而红。
func TestAltWatchSwitchesToAdvancingCandidate(t *testing.T) {
	const live = 0x03E8FB08
	type tick struct {
		mine float32
		alt  float32
		ok   bool // 候选这一次读不读得出
	}
	cases := []struct {
		name  string
		ticks []tick
		want  bool // 最后一次检查是否改用候选
	}{
		{"当前地址停着、候选在走", []tick{{0, 12, true}, {0, 13.02, true}}, true},
		{"第一次检查只记基准", []tick{{0, 12, true}}, false},
		{"当前地址在走", []tick{{30, 12, true}, {31, 13, true}}, false},
		{"都停着（暂停）", []tick{{48, 0, true}, {48, 0, true}, {48, 0, true}}, false},
		{"候选只是抖动", []tick{{0, 12, true}, {0, 12.05, true}}, false},
		{"候选乱跳", []tick{{0, 12, true}, {0, 62, true}}, false},
		{"候选上一次读不出", []tick{{0, 0, false}, {0, 13, true}}, false},
		{"候选这一次读不出", []tick{{0, 12, true}, {0, 0, false}}, false},
		{"读不出之后恢复、再走一秒", []tick{{0, 0, false}, {0, 13, true}, {0, 14.01, true}}, true},
	}
	for _, c := range cases {
		w := altWatch{addrs: []uint32{live}}
		var got bool
		var addr uint32
		for _, tk := range c.ticks {
			read := func(a uint32) (float32, bool) {
				if a != live {
					t.Fatalf("%s：读了不在候选里的地址 %#x", c.name, a)
				}
				return tk.alt, tk.ok
			}
			addr, got = w.check(read, tk.mine, anyValid)
		}
		if got != c.want || (got && addr != live) {
			t.Errorf("%s：改用=%v（地址 %#x），want %v", c.name, got, addr, c.want)
		}
	}
}

// 没有落选的候选（只有一个候选，或已经换过一次）：什么都不做，也不去读。
func TestAltWatchNoCandidates(t *testing.T) {
	var w altWatch
	for i := 0; i < 3; i++ {
		if _, ok := w.check(func(uint32) (float32, bool) {
			t.Fatal("没有候选时不应读内存")
			return 0, false
		}, 0, anyValid); ok {
			t.Fatal("没有候选时不应改用")
		}
	}
}

// 落选的候选不止一个时，复核要逐个做：排前的那个在走却复核不过（那块内存已另作他用，读数仍合理、
// 仍在涨），要接着看后面的，而不是这一秒就此作罢——那样每一秒都卡在它上，后面真在走的永远轮不到。
//
// 变异自证：把 check 里的 `&& valid(a)` 去掉（复核留给调用方在返回之后做），「排前的复核不过」一例
// 返回的是排前的那个而红，「都复核不过」一例照换而红。
func TestAltWatchSkipsCandidateThatFailsRecheck(t *testing.T) {
	const reused, live = 0x0FF31C40, 0x03E8FB08
	ticks := []map[uint32]float32{{reused: 500, live: 12}, {reused: 501, live: 13.02}} // 两个都在走
	cases := []struct {
		name     string
		valid    func(uint32) bool
		wantAddr uint32
		want     bool
	}{
		{"排前的复核不过：改用后面那个", func(a uint32) bool { return a != reused }, live, true},
		{"都复核得过：取排前的", anyValid, reused, true},
		{"都复核不过：不换", func(uint32) bool { return false }, 0, false},
	}
	for _, c := range cases {
		w := altWatch{addrs: []uint32{reused, live}}
		var addr uint32
		var got bool
		for _, tk := range ticks {
			addr, got = w.check(func(a uint32) (float32, bool) { return tk[a], true }, 0, c.valid)
		}
		if got != c.want || (got && addr != c.wantAddr) {
			t.Errorf("%s：改用=%v（地址 %#x），want %v（地址 %#x）", c.name, got, addr, c.want, c.wantAddr)
		}
	}
}
