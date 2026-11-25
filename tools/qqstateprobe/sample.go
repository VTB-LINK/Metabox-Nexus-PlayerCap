//go:build windows

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

const (
	maxSampleSeconds = 1800
	// 下限 1 ms：样本数上限为 seconds × 1000 / interval（ms），间隔越短上限越大；1800 s、1 ms 时读取总量上限约
	// 3.7 GB，间隔再短这个上限就失去约束作用。
	minSampleInterval = time.Millisecond
	maxSampleInterval = 10 * time.Millisecond // 也是汇总里「间隔超过 10 ms」的界
	sampleSpin        = 2 * time.Millisecond  // Sleep 到目标前这么久，其余自旋
	stopPollInterval  = 100 * time.Millisecond
	sampleReadBound   = 4 + metaBytes // 每个样本的读取上界：字段 4 字节加 readName
)

// sampleOpts 是 sample 的参数。
type sampleOpts struct {
	seconds  int
	field    watch
	interval time.Duration
	out      string
	stop     string
}

// parseSampleArgs 解析并校验 sample 的命令行（不含子命令名）。模块是否已加载、RVA 是否落在映像之内要连上进程才知道，
// 见 fieldAddr。
func parseSampleArgs(args []string) (sampleOpts, error) {
	fs := flag.NewFlagSet("sample", flag.ContinueOnError)
	var o sampleOpts
	var fields watchList
	fs.IntVar(&o.seconds, "seconds", 0, "采样秒数，1～1800")
	fs.Var(&fields, "field", "要采的 DWORD，模块:0xRVA（如 QQMusic.dll:0xD3BBA8），RVA 须 4 字节对齐")
	fs.DurationVar(&o.interval, "interval", 5*time.Millisecond, "采样间隔，1ms～10ms")
	fs.StringVar(&o.out, "out", "", "输出文件（UTF-8 文本）")
	fs.StringVar(&o.stop, "stop", "", "可选：该文件出现即停，每 100 ms 看一次")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	switch {
	case fs.NArg() > 0:
		return o, fmt.Errorf("多余的参数 %q", fs.Args())
	case o.seconds < 1 || o.seconds > maxSampleSeconds:
		return o, fmt.Errorf("-seconds 须在 1～%d 之间，得到 %d", maxSampleSeconds, o.seconds)
	case len(fields) != 1:
		return o, fmt.Errorf("需要且只能有一个 -field 模块:0xRVA，得到 %d 个", len(fields))
	case fields[0].rva%4 != 0:
		return o, fmt.Errorf("-field %s：RVA 须 4 字节对齐", fields[0])
	case o.interval < minSampleInterval || o.interval > maxSampleInterval:
		return o, fmt.Errorf("-interval 须在 %v～%v 之间，得到 %v", minSampleInterval, maxSampleInterval, o.interval)
	case o.out == "":
		return o, fmt.Errorf("需要 -out 文件")
	}
	o.field = fields[0]
	return o, nil
}

// fieldAddr 返回 w 在目标进程里的绝对地址：模块须在 mods 里（模块名不分大小写），整个 DWORD 须落在模块映像之内。
func fieldAddr(mods []module, w watch) (uintptr, error) {
	for _, m := range mods {
		if !strings.EqualFold(m.name, w.module) {
			continue
		}
		if uint64(w.rva)+4 > uint64(m.size) {
			return 0, fmt.Errorf("%s 超出模块映像（大小 0x%X）", w, m.size)
		}
		return uintptr(m.base) + uintptr(w.rva), nil
	}
	return 0, fmt.Errorf("%s：模块未加载（只认 %s）", w, strings.Join(wantModules, "、"))
}

// nextIndex 返回样本 i 之后要采的样本序号。第 k 个样本的目标时刻为开始后 k 个间隔，elapsed 为此刻距开始的时长。
// 落后于第 i+1 个目标不超过一个间隔时照常取 i+1（立即采）；落后一个间隔以上不补采，取 elapsed 之后的第一个目标，
// 缺口照实留在样本时刻里。
func nextIndex(i int, elapsed, interval time.Duration) int {
	if elapsed-time.Duration(i+1)*interval > interval {
		return int(elapsed/interval) + 1
	}
	return i + 1
}

// sampleLiner 记着上一个样本的歌名，逐行生成输出。
type sampleLiner struct {
	prev   string
	prevOK bool // 有上一个样本且其歌名读成了
}

// sampleLine 返回一个样本的输出行（不含换行）：t_us、state、name 以制表符分隔。state 为十进制取值，读失败为 -；
// name 读失败为 -，与上一个样本读成的歌名相同为 =，其余（第一个样本、读失败之后的第一个读成、歌名变了）为 Go 引号串。
func (s *sampleLiner) sampleLine(tus int64, state uint32, stateOK bool, name string, nameOK bool) string {
	st := "-"
	if stateOK {
		st = strconv.FormatUint(uint64(state), 10)
	}
	col := "-"
	switch {
	case !nameOK:
	case s.prevOK && name == s.prev:
		col = "="
	default:
		col = strconv.Quote(name)
	}
	s.prev, s.prevOK = name, nameOK
	return strconv.FormatInt(tus, 10) + "\t" + st + "\t" + col
}

// gapStats 累计相邻样本的间隔（微秒）：最大值与严格超过 maxSampleInterval 的次数。
type gapStats struct {
	n         int
	last, max int64
	over      int
}

func (g *gapStats) add(tus int64) {
	if g.n > 0 {
		d := tus - g.last
		g.max = max(g.max, d)
		if d > maxSampleInterval.Microseconds() {
			g.over++
		}
	}
	g.n++
	g.last = tus
}

// sampleClock 是 sampleLoop 的时钟：since 返回距开始的时长，waitUntil 等到距开始为 at 的时刻（已过则立即返回）。
// 实际采样用 qpcClock；用例用可控的假时钟，使跳采与写失败的时序确定。
type sampleClock interface {
	since() time.Duration
	waitUntil(at time.Duration)
}

// qpcClock 是以 QueryPerformanceCounter 计的单调时钟。Go 的 time.Since 在 Windows 上读系统中断时间，步长随系统定时
// 精度而变（2026-11-24 本机实测：中位约 1 ms，最长 10～16 ms），量不出 10 ms 以内的间隔。
type qpcClock struct{ start, freq int64 }

func newQPCClock() (qpcClock, error) {
	var c qpcClock
	if r, _, _ := procQueryPerfFrequency.Call(uintptr(unsafe.Pointer(&c.freq))); r == 0 || c.freq <= 0 {
		return c, fmt.Errorf("QueryPerformanceFrequency 失败")
	}
	c.start = qpcNow()
	return c, nil
}

func qpcNow() int64 {
	var v int64
	procQueryPerfCounter.Call(uintptr(unsafe.Pointer(&v)))
	return v
}

// since 返回距 start 的时长。
func (c qpcClock) since() time.Duration {
	d := qpcNow() - c.start
	return time.Duration(d/c.freq)*time.Second + time.Duration(d%c.freq*int64(time.Second)/c.freq)
}

// waitUntil 等到距 start 为 at 的时刻：Sleep 到 at 之前 sampleSpin，其余自旋（Sleep 会迟到数毫秒）。
func (c qpcClock) waitUntil(at time.Duration) {
	if d := at - c.since() - sampleSpin; d > 0 {
		time.Sleep(d)
	}
	for c.since() < at {
	}
}

func sample(o sampleOpts) error {
	t, err := openTarget()
	if err != nil {
		return err
	}
	defer procCloseHandle.Call(t.h)
	addr, err := fieldAddr(t.mods, o.field)
	if err != nil {
		return err
	}
	if o.stop != "" {
		if _, err := os.Stat(o.stop); err == nil {
			return fmt.Errorf("停止文件 %s 已存在，删掉再采", o.stop)
		}
	}
	f, err := os.Create(o.out)
	if err != nil {
		return err
	}
	defer f.Close()

	maxSamples := int(time.Duration(o.seconds) * time.Second / o.interval)
	fmt.Printf("每个样本至多读 %d 字节，至多 %d 个样本，读取总量上限 %d 字节；Ctrl+C 可提前结束\n",
		sampleReadBound, maxSamples, uint64(sampleReadBound)*uint64(maxSamples))
	match := "精确匹配"
	if !t.exact {
		match = "表中无此版本，按版本号距离回退，歌名可能读错"
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	head := fmt.Sprintf("# qqstateprobe sample\n# pid %d, QQ 音乐 %s, 偏移表 %s（%s）\n# field %s = 0x%X, interval %v, start %s\n",
		t.pid, t.version, t.offKey, match, o.field, addr, o.interval, time.Now().Format("2006-01-02T15:04:05.000Z07:00"))
	fmt.Print(head)

	read := func() (uint32, bool, string, bool) {
		state, stateOK := t.readU32OK(addr)
		name, nameOK := t.readName()
		return state, stateOK, name, nameOK
	}
	nextStopCheck := stopPollInterval
	stop := func(elapsed time.Duration) bool {
		if ctx.Err() != nil {
			return true
		}
		if o.stop == "" || elapsed < nextStopCheck {
			return false
		}
		nextStopCheck = elapsed + stopPollInterval
		_, err := os.Stat(o.stop)
		return err == nil
	}
	clk, err := newQPCClock()
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	sum, err := sampleLoop(w, head+"t_us\tstate\tname\n", clk, maxSamples, o.interval, read, stop)
	if err == nil {
		err = w.Flush()
	}
	if err != nil {
		return fmt.Errorf("写 %s 失败，输出不完整：%w", o.out, err)
	}
	fmt.Print(sum)
	return f.Close()
}

// sampleLoop 是 sample 的采样主循环：先把 head 写入 w，再按 interval 采至多 maxSamples 个样本（时刻以 clk 计，第 i 个
// 样本的目标为 i 个间隔，落后一个间隔以上即跳采，见 nextIndex），每个样本先取时刻、再调一次 read，逐行写入 w；stop
// 返回真即提前结束。正常结束时把汇总行写入 w 并返回它。写 w 任一次失败即停止采样、不再调 read，返回该错误、汇总为
// 空：之后的样本已无法保存，再读目标进程没有意义（不停下最长会持续 1800 s）。w 带缓冲时，底层写失败在缓冲区刷出时
// 才报出来。
func sampleLoop(w io.Writer, head string, clk sampleClock, maxSamples int, interval time.Duration,
	read func() (state uint32, stateOK bool, name string, nameOK bool), stop func(elapsed time.Duration) bool) (string, error) {
	if _, err := io.WriteString(w, head); err != nil {
		return "", err
	}
	var (
		liner                   sampleLiner
		gaps                    gapStats
		stateFailed, nameFailed int
	)
	for i := 0; i < maxSamples; i = nextIndex(i, clk.since(), interval) {
		clk.waitUntil(time.Duration(i) * interval)
		tus := clk.since().Microseconds()
		state, stateOK, name, nameOK := read()
		if !stateOK {
			stateFailed++
		}
		if !nameOK {
			nameFailed++
		}
		if _, err := io.WriteString(w, liner.sampleLine(tus, state, stateOK, name, nameOK)+"\n"); err != nil {
			return "", err
		}
		gaps.add(tus)
		if stop(clk.since()) {
			break
		}
	}
	sum := fmt.Sprintf("# samples %d, state_failed %d, name_failed %d, max_gap_us %d, gaps_over_10ms %d, bytes_read %d\n",
		gaps.n, stateFailed, nameFailed, gaps.max, gaps.over, bytesRead)
	if _, err := io.WriteString(w, sum); err != nil {
		return "", err
	}
	return sum, nil
}
