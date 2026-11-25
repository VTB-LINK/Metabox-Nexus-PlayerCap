package assoc

// 本文件只测 IdentifyLocal：经占用查询认定 QQ 正在播放的本地文件（设计 §6.5 第 2、3 步），
// 包括对分的会话数与每次注册的路径集合。

import (
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"testing"
)

const (
	qqPID      = 4321
	neteasePID = 7788
)

// fakeRM 是记录每次参数的假占用查询：open 给出各路径被哪些进程打开；第 failAt 次（从 0 起）调用返回 err。
type fakeRM struct {
	open   map[string][]uint32
	calls  [][]string
	result [][]uint32
	failAt int
	err    error
}

func newFakeRM(open map[string][]uint32) *fakeRM { return &fakeRM{open: open, failAt: -1} }

func (f *fakeRM) holders(paths ...string) ([]uint32, error) {
	f.calls = append(f.calls, slices.Clone(paths))
	if len(f.calls)-1 == f.failAt {
		f.result = append(f.result, nil)
		return nil, f.err
	}
	var pids []uint32
	for _, p := range paths {
		pids = append(pids, f.open[p]...)
	}
	f.result = append(f.result, pids)
	return pids, nil
}

// checkHalving 断言首次注册全部候选，此后每次注册的是此前某次含 QQ 的查询所注册集合的前一半或后一半。
func checkHalving(t *testing.T, label string, cands []string, f *fakeRM) {
	t.Helper()
	if len(f.calls) == 0 || !slices.Equal(f.calls[0], cands) {
		t.Errorf("%s：首次应注册全部 %d 个候选，实际调用 %d 次", label, len(cands), len(f.calls))
		return
	}
	for i := 1; i < len(f.calls); i++ {
		found := false
		for j := 0; j < i && !found; j++ {
			p := f.calls[j]
			if len(p) < 2 || !slices.Contains(f.result[j], qqPID) {
				continue
			}
			found = slices.Equal(f.calls[i], p[:len(p)/2]) || slices.Equal(f.calls[i], p[len(p)/2:])
		}
		if !found {
			t.Errorf("%s：第 %d 次注册的 %q 不是此前某次含 QQ 的查询集合的一半", label, i, f.calls[i])
		}
	}
}

func TestIdentifyLocalSessionBound(t *testing.T) {
	for _, n := range []int{1, 2, 7, 256} {
		cands := make([]string, n)
		for i := range cands {
			cands[i] = fmt.Sprintf(`E:\m\f%03d.flac`, i)
		}
		// 被 QQ 打开的位置：k=1 取最后一个；k=2 取首尾，两条分支各自对分到底。
		for k, held := range [][]string{nil, {cands[n-1]}, {cands[0], cands[n-1]}} {
			if k > n {
				continue
			}
			label := fmt.Sprintf("n=%d k=%d", n, k)
			open := map[string][]uint32{}
			for _, p := range held {
				open[p] = []uint32{qqPID}
			}
			f := newFakeRM(open)
			orig := slices.Clone(cands)
			song := "f000" // 两个被打开时，名字相符的是首个
			got, held, err := IdentifyLocal(cands, song, qqPID, f.holders)

			want := ""
			switch k {
			case 1:
				want = cands[n-1]
			case 2:
				want = cands[0]
			}
			if err != nil || held != k || got != want {
				t.Errorf("%s：得到 (%q, %d, %v)，应为 (%q, %d)", label, got, held, err, want, k)
			}
			if bound := min(1+2*k*bits.Len(uint(n-1)), 2*n-1); len(f.calls) > bound {
				t.Errorf("%s：会话 %d 次，超过上界 %d", label, len(f.calls), bound)
			}
			checkHalving(t, label, cands, f)
			if !slices.Equal(cands, orig) {
				t.Errorf("%s：候选切片被改动：%q", label, cands)
			}
		}
	}

	// n = 256 的确切会话数：首尾两个被打开为 1 + 2 + 2·2·7 = 31；全部被打开为 2n − 1 = 511（对分树的全部节点，最坏情形）。
	cands := make([]string, 256)
	for i := range cands {
		cands[i] = fmt.Sprintf(`E:\m\f%03d.flac`, i)
	}
	for _, c := range []struct {
		label string
		held  []string
		want  int
	}{
		{"n=256 首尾两个被打开", []string{cands[0], cands[255]}, 31},
		{"n=256 全部被打开", cands, 511},
	} {
		open := map[string][]uint32{}
		for _, p := range c.held {
			open[p] = []uint32{qqPID}
		}
		f := newFakeRM(open)
		got, held, err := IdentifyLocal(cands, "f000", qqPID, f.holders)
		if err != nil || held != len(c.held) || got != cands[0] {
			t.Errorf("%s：得到 (%q, %d, %v)，应为 (%q, %d)", c.label, got, held, err, cands[0], len(c.held))
		}
		if len(f.calls) != c.want {
			t.Errorf("%s：会话 %d 次，应恰为 %d", c.label, len(f.calls), c.want)
		}
		checkHalving(t, c.label, cands, f)
	}
}

// held 是对分得出的被 QQ 打开的候选数（消歧之前），认定不成（ErrAmbiguous）时同样给出，供本地认定的 Detail（设计 §6.9）。
// 变异自证（实测）：held 取消歧之后的个数，被打开不止一个的九例里七例红（「名字都相符」「都相等（不同目录）」两例消歧之后
// 仍剩 2 个，不红），TestIdentifyLocalSessionBound 与 TestIdentifyLocalHoldersError 同红。
func TestIdentifyLocal(t *testing.T) {
	a, b, c := `E:\m\Seabed.flac`, `E:\m\Fensheng.flac`, `E:\m\Other.flac`
	// 歌名「海底」：eq 与它相等；tagged、live 只包含；track 不含（带标签、文件名为 track01）。
	eq, tagged, live, track := `E:\m\海底.flac`, `E:\m\一支榴莲 - 海底.flac`, `E:\m\海底 (Live).flac`, `E:\m\track01.flac`
	both := func(p, q string) map[string][]uint32 { return map[string][]uint32{p: {qqPID}, q: {qqPID}} }
	cases := []struct {
		name    string
		cands   []string
		song    string
		open    map[string][]uint32
		want    string
		held    int
		wantErr error
	}{
		{"恰好一个被 QQ 打开", []string{a, b, c}, "Seabed", map[string][]uint32{b: {qqPID}}, b, 1, nil},
		{"只有一个被打开时名字不符仍认定", []string{a, b}, "Unrelated", map[string][]uint32{a: {qqPID}}, a, 1, nil},
		{"占用者只有网易云：不算", []string{a, b, c}, "Seabed", map[string][]uint32{a: {neteasePID}}, "", 0, nil},
		{"网易云与 QQ 各开一个：取 QQ 的", []string{a, b, c}, "Seabed", map[string][]uint32{a: {neteasePID}, c: {qqPID}}, c, 1, nil},
		{"同一文件被网易云与 QQ 同时打开", []string{a, b}, "Seabed", map[string][]uint32{b: {neteasePID, qqPID}}, b, 1, nil},
		{"无人打开", []string{a, b, c}, "Seabed", nil, "", 0, nil},
		{"两个被 QQ 打开、一个名字相符（不区分大小写）", []string{a, b, c}, "sEABED", map[string][]uint32{a: {qqPID}, c: {qqPID}}, a, 2, nil},
		{"两个被 QQ 打开、都不相符", []string{a, b, c}, "Unrelated", map[string][]uint32{a: {qqPID}, c: {qqPID}}, "", 2, ErrAmbiguous},
		{"两个被 QQ 打开、歌名为空", []string{a, b}, "", map[string][]uint32{a: {qqPID}, b: {qqPID}}, "", 2, ErrAmbiguous},
		{"两个被 QQ 打开、名字都相符", []string{a, `E:\n\SEABED.mp3`, b}, "Seabed", map[string][]uint32{a: {qqPID}, `E:\n\SEABED.mp3`: {qqPID}}, "", 2, ErrAmbiguous},
		{"三个被 QQ 打开、一个名字相符", []string{a, b, c}, "Fensheng", map[string][]uint32{a: {qqPID}, b: {qqPID}, c: {qqPID}}, b, 3, nil},
		// 消歧只认相等，「包含」不用于消歧（设计 §6.5 第 3 步）。
		{"两个被 QQ 打开、一个相等一个只包含：认定相等的", []string{tagged, eq}, "海底", both(tagged, eq), eq, 2, nil},
		{"两个被 QQ 打开、都只包含：不认定", []string{tagged, live}, "海底", both(tagged, live), "", 2, ErrAmbiguous},
		{"两个被 QQ 打开、都相等（不同目录）：不认定", []string{eq, `F:\备份\海底.MP3`}, "海底", both(eq, `F:\备份\海底.MP3`), "", 2, ErrAmbiguous},
		{"只有一个被打开时不看名字：track01 也认定", []string{track, eq}, "海底", map[string][]uint32{track: {qqPID}}, track, 1, nil},
		// Task 22 设计审的反例：关联表 2 条，两份都被 QQ 打开，正在放的是 track01、另一份文件名包含歌名；按「包含」消歧会
		// 认成另一份、显示它的歌词。准确优先，不认定。
		{"两个被 QQ 打开、track01 与只包含的一份：不认定", []string{track, tagged}, "海底", both(track, tagged), "", 2, ErrAmbiguous},
	}
	for _, tc := range cases {
		f := newFakeRM(tc.open)
		got, held, err := IdentifyLocal(tc.cands, tc.song, qqPID, f.holders)
		if !errors.Is(err, tc.wantErr) || held != tc.held || got != tc.want {
			t.Errorf("%s：得到 (%q, %d, %v)，应为 (%q, %d, %v)", tc.name, got, held, err, tc.want, tc.held, tc.wantErr)
		}
		checkHalving(t, tc.name, tc.cands, f)
	}
}

// 首次或对分途中查询出错：错误原样返回、不当作 ErrAmbiguous，不认定，held 为 0，不再查询。
// 两个被 QQ 打开且名字都不相符，查询全部成功时结果为 ErrAmbiguous、held 为 2。
func TestIdentifyLocalHoldersError(t *testing.T) {
	errRM := errors.New("rm failed")
	cands := []string{`E:\m\a.flac`, `E:\m\b.flac`, `E:\m\c.flac`, `E:\m\d.flac`}
	open := map[string][]uint32{cands[0]: {qqPID}, cands[3]: {qqPID}}

	f := newFakeRM(open)
	if got, held, err := IdentifyLocal(cands, "x", qqPID, f.holders); got != "" || held != 2 || !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("查询全部成功：得到 (%q, %d, %v)，应为 (\"\", 2, ErrAmbiguous)", got, held, err)
	}
	total := len(f.calls)

	for failAt := range total {
		f := newFakeRM(open)
		f.failAt, f.err = failAt, errRM
		got, held, err := IdentifyLocal(cands, "x", qqPID, f.holders)
		if err != errRM || errors.Is(err, ErrAmbiguous) || held != 0 || got != "" {
			t.Errorf("第 %d 次查询出错：得到 (%q, %d, %v)，应为 (\"\", 0, errRM)", failAt, got, held, err)
		}
		if len(f.calls) != failAt+1 {
			t.Errorf("第 %d 次查询出错：之后仍查询，共 %d 次", failAt, len(f.calls))
		}
	}
}

// 全集查询含 QQ，对分时文件已被放开：不认定，不报错。
func TestIdentifyLocalReleasedMidway(t *testing.T) {
	cands := []string{`E:\m\a.flac`, `E:\m\b.flac`}
	f := newFakeRM(map[string][]uint32{cands[1]: {qqPID}})
	holders := func(paths ...string) ([]uint32, error) {
		pids, err := f.holders(paths...)
		delete(f.open, cands[1])
		return pids, err
	}
	got, held, err := IdentifyLocal(cands, "b", qqPID, holders)
	if err != nil || held != 0 || got != "" {
		t.Errorf("得到 (%q, %d, %v)，应为不认定、held 为 0", got, held, err)
	}
}

func TestIdentifyLocalNoCandidates(t *testing.T) {
	for _, cands := range [][]string{nil, {}} {
		f := newFakeRM(nil)
		got, held, err := IdentifyLocal(cands, "a", qqPID, f.holders)
		if err != nil || held != 0 || got != "" || len(f.calls) != 0 {
			t.Errorf("候选 %q：得到 (%q, %d, %v)、查询 %d 次，应为不认定、held 为 0 且不查询", cands, got, held, err, len(f.calls))
		}
	}
}
