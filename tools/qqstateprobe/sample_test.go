//go:build windows

package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 本文件的变异自证：2026-11-24 在隔离副本里逐条注入、跑完还原，各条之下为实测变红的用例。

// 变异（实测）：歌名每行都写 → 「歌名不变写 =」「空歌名之后仍为空写 =」「state 读失败不影响歌名比较」红；歌名读失败
// 写成 = → 「歌名读失败写 -」等 7 例红；读失败之后沿用失败之前的歌名作比较 → 「读失败之后读到与失败之前相同的歌名
// 写引号串」「读失败带回的歌名不作比较」红；state 读失败写成 0 → 「state 读失败写 -」「state 读失败时不写带回的取值」
// 「state 读失败不影响歌名比较」「两列都读失败」红；比较歌名时不看上一个样本是否读成（第一个样本与零值空串比较）→
// 「第一个样本空歌名写引号串」等 4 例红。
func TestSampleLine(t *testing.T) {
	type smp struct {
		state   uint32
		stateOK bool
		name    string
		nameOK  bool
		want    string // 不含 t_us 列
	}
	ok := func(state uint32, name, want string) smp { return smp{state, true, name, true, want} }
	nameFail := func(state uint32, want string) smp { return smp{state, true, "", false, want} }
	cases := []struct {
		name string
		seq  []smp // 依次喂给同一个 sampleLiner，t_us 取下标 × 5000
	}{
		{"第一个样本写引号串", []smp{ok(3, "Lettersong", "3\t\"Lettersong\"")}},
		{"歌名不变写 =", []smp{ok(3, "A", "3\t\"A\""), ok(3, "A", "3\t="), ok(2, "A", "2\t=")}},
		{"歌名变了写引号串", []smp{ok(3, "A", "3\t\"A\""), ok(2, "B", "2\t\"B\""), ok(2, "A", "2\t\"A\"")}},
		{"歌名读失败写 -", []smp{ok(3, "A", "3\t\"A\""), nameFail(3, "3\t-")}},
		{"第一个样本歌名读失败写 -", []smp{nameFail(3, "3\t-")}},
		{"连续两次读失败都写 -", []smp{nameFail(3, "3\t-"), nameFail(3, "3\t-")}},
		// 反例：读失败之后不与失败之前的歌名比较。
		{"读失败之后读到与失败之前相同的歌名写引号串", []smp{ok(3, "A", "3\t\"A\""), nameFail(3, "3\t-"), ok(3, "A", "3\t\"A\"")}},
		// 反例：readName 读失败时可能带回不全的歌名，不当作读成。
		{"读失败带回的歌名不作比较", []smp{ok(3, "A", "3\t\"A\""), {3, true, "A", false, "3\t-"}, ok(3, "A", "3\t\"A\"")}},
		// 反例：零值 sampleLiner 里上一个歌名也是空串，第一个样本仍写引号串。
		{"第一个样本空歌名写引号串", []smp{ok(3, "", "3\t\"\"")}},
		{"歌名变为空写引号串", []smp{ok(3, "A", "3\t\"A\""), ok(3, "", "3\t\"\"")}},
		{"空歌名之后仍为空写 =", []smp{ok(3, "", "3\t\"\""), ok(3, "", "3\t=")}},
		{"读失败之后读到空歌名写引号串", []smp{nameFail(3, "3\t-"), ok(3, "", "3\t\"\"")}},
		{"歌名里的制表符与引号转义，不破坏分列", []smp{ok(3, "a\tb\"c", "3\t\"a\\tb\\\"c\"")}},
		// state：取值 0 写 0，读失败写 -，读失败时带回的取值不写出；两列互不影响。
		{"state 为 0 写 0", []smp{ok(0, "A", "0\t\"A\"")}},
		{"state 读失败写 -", []smp{{0, false, "A", true, "-\t\"A\""}}},
		{"state 读失败时不写带回的取值", []smp{{7, false, "A", true, "-\t\"A\""}}},
		{"state 读失败不影响歌名比较", []smp{ok(3, "A", "3\t\"A\""), {0, false, "A", true, "-\t="}}},
		{"两列都读失败", []smp{{0, false, "", false, "-\t-"}}},
		{"大取值按无符号十进制", []smp{ok(0xFFFFFFFF, "A", "4294967295\t\"A\"")}},
	}
	for _, c := range cases {
		var s sampleLiner
		for i, x := range c.seq {
			tus := int64(i) * 5000
			want := strconv.FormatInt(tus, 10) + "\t" + x.want
			if got := s.sampleLine(tus, x.state, x.stateOK, x.name, x.nameOK); got != want {
				t.Errorf("%s，第 %d 个样本：得到 %q，期望 %q", c.name, i, got, want)
			}
		}
	}
}

// 变异（实测）：超过 10 ms 的判定改为含等号 → 「间隔 10000 µs 不计入超过 10 ms」红；第一个样本也算一个间隔（从 0
// 起算）→ 「只有一个样本且时刻不为 0，最大间隔仍为 0」红；最大间隔只取最后一个 → 「最大间隔在中间」「最大间隔在
// 开头」「多次超过 10 ms 各计一次」红。
func TestGapStats(t *testing.T) {
	cases := []struct {
		name      string
		ts        []int64
		max, over int64
	}{
		{"没有样本", nil, 0, 0},
		{"只有一个样本时最大间隔为 0", []int64{0}, 0, 0},
		{"只有一个样本且时刻不为 0，最大间隔仍为 0", []int64{3}, 0, 0},
		{"间隔 10000 µs 不计入超过 10 ms", []int64{0, 10000}, 10000, 0},
		{"间隔 10001 µs 计入", []int64{0, 10001}, 10001, 1},
		{"最大间隔在中间", []int64{0, 5000, 15001, 20000}, 10001, 1},
		{"最大间隔在开头", []int64{100, 5100, 9000}, 5000, 0},
		{"多次超过 10 ms 各计一次", []int64{0, 20000, 30001, 35000}, 20000, 2},
	}
	for _, c := range cases {
		var g gapStats
		for _, ts := range c.ts {
			g.add(ts)
		}
		if g.n != len(c.ts) || g.max != c.max || int64(g.over) != c.over {
			t.Errorf("%s：样本 %d、最大间隔 %d、超过 10 ms %d 次，期望 %d、%d、%d", c.name, g.n, g.max, g.over, len(c.ts), c.max, c.over)
		}
	}
}

// 变异（实测）：-interval 上界改为严格小于 10 ms → 「间隔 10 ms」红；去掉 1 ms 下限（只要求大于 0）→ 「间隔 500 µs」
// 「间隔 1 ms 减 1 ns」红；-seconds 上界改为严格小于 1800 → 「1800 s 与停止文件」红；去掉 4 字节对齐检查 → 「RVA 不按 4 字节对齐」
// 「RVA 按 2 字节对齐」红；去掉多余参数检查 → 「多余的参数」红。
func TestParseSampleArgs(t *testing.T) {
	args := func(extra ...string) []string {
		return append([]string{"-seconds", "1", "-field", "QQMusic.dll:0xD3BBA8", "-out", "x.tsv"}, extra...)
	}
	good := []struct {
		name string
		args []string
		want sampleOpts
	}{
		{"缺省间隔 5 ms", args(), sampleOpts{1, watch{"QQMusic.dll", 0xD3BBA8}, 5 * time.Millisecond, "x.tsv", ""}},
		{"间隔 1 ms", args("-interval", "1ms"), sampleOpts{1, watch{"QQMusic.dll", 0xD3BBA8}, time.Millisecond, "x.tsv", ""}},
		{"间隔 10 ms", args("-interval", "10ms"), sampleOpts{1, watch{"QQMusic.dll", 0xD3BBA8}, 10 * time.Millisecond, "x.tsv", ""}},
		{"1800 s 与停止文件", []string{"-seconds", "1800", "-field", "qqmusic.dll:4096", "-out", "o", "-stop", "s"},
			sampleOpts{1800, watch{"qqmusic.dll", 4096}, 5 * time.Millisecond, "o", "s"}},
	}
	for _, c := range good {
		got, err := parseSampleArgs(c.args)
		if err != nil || got != c.want {
			t.Errorf("%s：得到 %+v、%v，期望 %+v", c.name, got, err, c.want)
		}
	}
	bad := []struct {
		name string
		args []string
	}{
		{"间隔 0", args("-interval", "0")},
		{"间隔为负", args("-interval", "-5ms")},
		{"间隔 10 ms 加 1 ns", args("-interval", "10000001ns")},
		{"间隔 500 µs", args("-interval", "500us")},
		{"间隔 1 ms 减 1 ns", args("-interval", "999999ns")},
		{"间隔没有单位", args("-interval", "5")},
		{"秒数 0", []string{"-seconds", "0", "-field", "QQMusic.dll:0xD3BBA8", "-out", "x"}},
		{"秒数 1801", []string{"-seconds", "1801", "-field", "QQMusic.dll:0xD3BBA8", "-out", "x"}},
		{"秒数为负", []string{"-seconds", "-1", "-field", "QQMusic.dll:0xD3BBA8", "-out", "x"}},
		{"缺秒数", []string{"-field", "QQMusic.dll:0xD3BBA8", "-out", "x"}},
		{"缺 -field", []string{"-seconds", "1", "-out", "x"}},
		{"模块名缺省", []string{"-seconds", "1", "-field", ":0xD3BBA8", "-out", "x"}},
		{"RVA 不是数", []string{"-seconds", "1", "-field", "QQMusic.dll:zz", "-out", "x"}},
		{"缺 RVA", []string{"-seconds", "1", "-field", "QQMusic.dll", "-out", "x"}},
		{"RVA 不按 4 字节对齐", []string{"-seconds", "1", "-field", "QQMusic.dll:0xD3BBA9", "-out", "x"}},
		{"RVA 按 2 字节对齐", []string{"-seconds", "1", "-field", "QQMusic.dll:0xD3BBAA", "-out", "x"}},
		{"两个 -field", args("-field", "QQMusic.dll:0xD3BBAC")},
		{"缺 -out", []string{"-seconds", "1", "-field", "QQMusic.dll:0xD3BBA8"}},
		{"多余的参数", args("extra")},
		{"未知参数", args("-max", "3")},
	}
	for _, c := range bad {
		if o, err := parseSampleArgs(c.args); err == nil {
			t.Errorf("%s：应报错，得到 %+v", c.name, o)
		}
	}
}

// 变异（实测）：只看 RVA 起点是否在映像内 → 「QQMusic.dll:0xDFFFFE」（DWORD 跨出映像末尾）红；模块名区分大小写 →
// 「qqmusic.DLL:0xD3BBA8」红。
func TestFieldAddr(t *testing.T) {
	mods := []module{
		{"QQMusic.exe", 0x400000, 0x1000},
		{"QQMusic.dll", 0x61000000, 0xE00000},
		{"QQMusicCommon.dll", 0x70000000, 0}, // GetModuleInformation 失败时大小为 0
	}
	good := []struct {
		w    watch
		want uintptr
	}{
		{watch{"QQMusic.dll", 0xD3BBA8}, 0x61D3BBA8},
		{watch{"qqmusic.DLL", 0xD3BBA8}, 0x61D3BBA8}, // 模块名不分大小写
		{watch{"QQMusic.dll", 0}, 0x61000000},
		{watch{"QQMusic.dll", 0xDFFFFC}, 0x61DFFFFC}, // 映像里最后一个 DWORD
		{watch{"QQMusic.exe", 0xFFC}, 0x400FFC},
	}
	for _, c := range good {
		if got, err := fieldAddr(mods, c.w); err != nil || got != c.want {
			t.Errorf("%s：得到 0x%X、%v，期望 0x%X", c.w, got, err, c.want)
		}
	}
	for _, w := range []watch{
		{"QQMusic.dll", 0xE00000},       // 映像末尾之外
		{"QQMusic.dll", 0xDFFFFE},       // DWORD 跨出映像末尾
		{"QQMusic.exe", 0x1000},         // 别的模块的大小为界
		{"QQMusicCommon.dll", 0},        // 大小为 0
		{"QQMusicPlayer.dll", 0xD3BBA8}, // 未加载
		{"QQMusic", 0xD3BBA8},           // 模块名须完整
	} {
		if got, err := fieldAddr(mods, w); err == nil {
			t.Errorf("%s 应报错，得到 0x%X", w, got)
		}
	}
}

// 变异（实测）：落后恰好一个间隔也跳过 → 「落后恰好一个间隔：仍立即采」红；跳过时取已过的目标（少加 1）→ 「落后
// 一个间隔以上」「中途落后一个间隔以上」「落后很多」三例红，且「跳过之后的目标在此刻之后」的检查同时报出。
func TestNextIndex(t *testing.T) {
	const iv = 5 * time.Millisecond
	cases := []struct {
		name    string
		i       int
		elapsed time.Duration
		want    int
	}{
		{"早于下一个目标", 0, 4 * time.Millisecond, 1},
		{"正好在下一个目标", 0, 5 * time.Millisecond, 1},
		{"落后下一个目标不到一个间隔：立即采", 0, 9 * time.Millisecond, 1},
		{"落后恰好一个间隔：仍立即采", 0, 10 * time.Millisecond, 1},
		{"落后一个间隔以上：不补采，取此刻之后的第一个目标", 0, 10*time.Millisecond + 1, 3},
		{"中途落后一个间隔以上", 3, 30 * time.Millisecond, 7},
		{"中途落后不到一个间隔", 3, 21 * time.Millisecond, 4},
		{"落后很多", 3, 1003 * time.Millisecond, 201},
	}
	for _, c := range cases {
		got := nextIndex(c.i, c.elapsed, iv)
		if got != c.want {
			t.Errorf("%s：nextIndex(%d, %v) = %d，期望 %d", c.name, c.i, c.elapsed, got, c.want)
		}
		if got > c.i+1 && time.Duration(got)*iv <= c.elapsed {
			t.Errorf("%s：跳过之后的目标 %v 不在此刻 %v 之后", c.name, time.Duration(got)*iv, c.elapsed)
		}
	}
}

var errInjected = errors.New("注入的写失败")

// failAfter 收下前 n 个字节，此后的写入返回 errInjected；跨过第 n 字节的那次写入只收下前面的部分。
type failAfter struct {
	got strings.Builder
	n   int
}

func (f *failAfter) Write(p []byte) (int, error) {
	room := max(f.n-f.got.Len(), 0)
	if len(p) <= room {
		f.got.Write(p)
		return len(p), nil
	}
	f.got.Write(p[:room])
	return room, errInjected
}

// fakeClock 是可控时钟：waitUntil 把时刻推进到目标（已过则不动）；读取耗时、线程被挂起由用例直接加到 now 上。
type fakeClock struct{ now time.Duration }

func (c *fakeClock) since() time.Duration       { return c.now }
func (c *fakeClock) waitUntil(at time.Duration) { c.now = max(c.now, at) }

const loopHead = "# h\nt_us\tstate\tname\n"

// loopSummary 是 sampleLoop 的汇总行（读取字节数取包级计数，用例里不读进程，恒为同一值）。
func loopSummary(samples int, maxGap int64, over int) string {
	return fmt.Sprintf("# samples %d, state_failed 0, name_failed 0, max_gap_us %d, gaps_over_10ms %d, bytes_read %d\n",
		samples, maxGap, over, bytesRead)
}

// loopRun 用假时钟、1 ms 间隔跑 sampleLoop，写入 failAfter{n: n}。read 是只计次数的替身，不碰任何进程，取值恒为
// (3, "A")；第 k 次 read（从 1 数）之后时钟另走 late[k]。stop 为 nil 即不提前结束。
func loopRun(n, maxSamples int, late map[int]time.Duration, stop func(time.Duration) bool) (reads int, out, sum string, err error) {
	clk := &fakeClock{}
	w := &failAfter{n: n}
	read := func() (uint32, bool, string, bool) {
		reads++
		clk.now += late[reads]
		return 3, true, "A", true
	}
	if stop == nil {
		stop = func(time.Duration) bool { return false }
	}
	sum, err = sampleLoop(w, loopHead, clk, maxSamples, time.Millisecond, read, stop)
	return reads, w.got.String(), sum, err
}

// 正常结束：输出逐字节核对，read 次数、样本行数与汇总里的样本数三者一致。
//
// 变异（实测）：跳采改为补采（总取 i+1）→ 「落后一个间隔以上」「挂起 20 ms」红；样本时刻改在 read 之后取 → 「落后
// 不到一个间隔」「落后恰好一个间隔」「落后一个间隔以上」「挂起 20 ms」红；不等到目标时刻 → 除「零个样本」「stop 为真
// 即在本样本之后结束」外各例红；忽略 stop → 两个 stop 用例红。nextIndex 的两条变异（落后恰好一个间隔也跳过、跳到
// 已过的目标）在这里分别使「落后恰好一个间隔」与「落后一个间隔以上」「挂起 20 ms」红。
func TestSampleLoopTiming(t *testing.T) {
	const us = time.Microsecond
	cases := []struct {
		name       string
		maxSamples int
		late       map[int]time.Duration
		stop       func(time.Duration) bool
		rows       string
		sum        string
	}{
		{"按时采满", 5, nil, nil,
			"0\t3\t\"A\"\n1000\t3\t=\n2000\t3\t=\n3000\t3\t=\n4000\t3\t=\n", loopSummary(5, 1000, 0)},
		{"落后不到一个间隔：立即采，不跳", 5, map[int]time.Duration{1: 1500 * us}, nil,
			"0\t3\t\"A\"\n1500\t3\t=\n2000\t3\t=\n3000\t3\t=\n4000\t3\t=\n", loopSummary(5, 1500, 0)},
		// 反例：落后恰好一个间隔仍立即采；第 1、2 个样本都在 2 ms（真机上 read 有耗时，二者不会同一微秒）。
		{"落后恰好一个间隔：立即采，不跳", 5, map[int]time.Duration{1: 2000 * us}, nil,
			"0\t3\t\"A\"\n2000\t3\t=\n2000\t3\t=\n3000\t3\t=\n4000\t3\t=\n", loopSummary(5, 2000, 0)},
		// 代码审第 2 轮的反例：第 1 个样本之后挂起到 3.5 ms，第 1～3 个目标跳过，只采 2 个，照常结束。
		{"落后一个间隔以上：跳采，少于设定的样本数", 5, map[int]time.Duration{1: 3500 * us}, nil,
			"0\t3\t\"A\"\n4000\t3\t=\n", loopSummary(2, 4000, 0)},
		{"挂起 20 ms：缺口照实记，计入超过 10 ms", 25, map[int]time.Duration{1: 20 * time.Millisecond}, nil,
			"0\t3\t\"A\"\n21000\t3\t=\n22000\t3\t=\n23000\t3\t=\n24000\t3\t=\n", loopSummary(5, 21000, 1)},
		{"零个样本", 0, nil, nil, "", loopSummary(0, 0, 0)},
		{"stop 为真即在本样本之后结束", 5, nil, func(time.Duration) bool { return true },
			"0\t3\t\"A\"\n", loopSummary(1, 0, 0)},
		{"stop 按时刻：2 ms 起为真", 5, nil, func(el time.Duration) bool { return el >= 2*time.Millisecond },
			"0\t3\t\"A\"\n1000\t3\t=\n2000\t3\t=\n", loopSummary(3, 1000, 0)},
	}
	for _, c := range cases {
		reads, out, sum, err := loopRun(1<<20, c.maxSamples, c.late, c.stop)
		if want := loopHead + c.rows + c.sum; err != nil || out != want || sum != c.sum {
			t.Errorf("%s：err %v，输出 %q，汇总 %q；期望输出 %q", c.name, err, out, sum, want)
		}
		if rows := strings.Count(c.rows, "\n"); reads != rows {
			t.Errorf("%s：read 调了 %d 次，样本行 %d 行", c.name, reads, rows)
		}
	}
}

// 写失败：返回注入的错误、汇总为空，写失败的那个样本已读、此后不再读。
//
// 变异（实测）：忽略文件头的写错误 → 「文件头写不全」红；忽略样本行的写错误 → 「文件头恰好写满、第一个样本行写失败」
// 「第三个样本行写到一半失败」「第二个样本行恰好写满、第三行写失败」红；忽略汇总行的写错误 → 「采满之后汇总行写不全」红。
func TestSampleLoopWriteFailure(t *testing.T) {
	const r0, r1 = "0\t3\t\"A\"\n", "1000\t3\t=\n"
	full := loopHead + r0 + r1 + "2000\t3\t=\n3000\t3\t=\n4000\t3\t=\n" + loopSummary(5, 1000, 0)
	cases := []struct {
		name  string
		n     int
		reads int
		out   string // 写下的内容（含跨过第 n 字节那次写入的前半）
	}{
		{"文件头写不全：一次也不读", len(loopHead) - 1, 0, loopHead[:len(loopHead)-1]},
		// 反例：文件头恰好写满不算失败，失败的是第一个样本行。
		{"文件头恰好写满、第一个样本行写失败", len(loopHead), 1, loopHead},
		{"第三个样本行写到一半失败", len(loopHead+r0+r1) + 3, 3, loopHead + r0 + r1 + "200"},
		// 反例：第二个样本行恰好写满不算失败，失败的是第三行。
		{"第二个样本行恰好写满、第三行写失败", len(loopHead + r0 + r1), 3, loopHead + r0 + r1},
		{"采满之后汇总行写不全", len(full) - 1, 5, full[:len(full)-1]},
	}
	for _, c := range cases {
		reads, out, sum, err := loopRun(c.n, 5, nil, nil)
		if !errors.Is(err, errInjected) || sum != "" || out != c.out || reads != c.reads {
			t.Errorf("%s：err %v、汇总 %q、read %d 次、输出 %q；期望注入的写失败、汇总为空、read %d 次、输出 %q",
				c.name, err, sum, reads, out, c.reads, c.out)
		}
	}
	// 反例：汇总行恰好写满即成功。
	if reads, out, sum, err := loopRun(len(full), 5, nil, nil); err != nil || out != full || sum != loopSummary(5, 1000, 0) || reads != 5 {
		t.Errorf("汇总行恰好写满：err %v、read %d 次、汇总 %q", err, reads, sum)
	}
}

// 真的 QPC 时钟：只核对 read 次数、样本行数与汇总里的样本数三者一致，样本时刻不减；线程可能被挂起而跳采，不要求
// 采满。
func TestSampleLoopQPCClock(t *testing.T) {
	clk, err := newQPCClock()
	if err != nil {
		t.Fatal(err)
	}
	var w strings.Builder
	reads := 0
	read := func() (uint32, bool, string, bool) {
		reads++
		return 3, true, "A", true
	}
	sum, err := sampleLoop(&w, loopHead, clk, 20, time.Millisecond, read, func(time.Duration) bool { return false })
	out := w.String()
	if err != nil || !strings.HasPrefix(out, loopHead) || !strings.HasSuffix(out, sum) {
		t.Fatalf("err %v，输出 %q", err, out)
	}
	rows := strings.Split(strings.TrimSuffix(strings.TrimPrefix(out, loopHead), sum), "\n")
	rows = rows[:len(rows)-1] // 末行之后的空串
	if reads < 1 || reads > 20 || len(rows) != reads || !strings.HasPrefix(sum, fmt.Sprintf("# samples %d,", reads)) {
		t.Errorf("read %d 次、样本行 %d 行、汇总 %q，三者应一致且 read 在 1～20 次", reads, len(rows), sum)
	}
	var last int64 = -1
	for _, r := range rows {
		tus, err := strconv.ParseInt(strings.SplitN(r, "\t", 2)[0], 10, 64)
		if err != nil || tus < last {
			t.Errorf("样本行 %q：时刻不是数或比上一行小", r)
		}
		last = tus
	}
}
