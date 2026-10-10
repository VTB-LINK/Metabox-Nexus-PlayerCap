package helper

// 本文件只测 framer：按样本数分帧（帧界切包、静音包计数、残帧、峰值归属、分析窗的保留）与 hopFor。

import (
	"math"
	"reflect"
	"testing"
)

// gotFrame 是 emitFunc 收到的一帧；batch 为拷贝（原切片只在回调内有效）。
type gotFrame struct {
	batch []float32
	peaks [2]float32
	fresh int
}

// collect 返回把每帧追加进 out 的 emitFunc。
func collect(out *[]gotFrame) emitFunc {
	return func(b []float32, p [2]float32, n int) {
		*out = append(*out, gotFrame{append([]float32(nil), b...), p, n})
	}
}

// seq 造 n 个样本的单声道递增序列 start, start+1, …：值即样本序号，用来核对不丢、不重、不乱序。
func seq(start, n int) []float32 {
	s := make([]float32, n)
	for i := range s {
		s[i] = float32(start + i)
	}
	return s
}

// pushSeq 把样本序号 [from, from+n) 作为一个单声道非静音包喂入。push 会就地改写入参，故每次新造。
func pushSeq(f *framer, from, n int, emit emitFunc) {
	f.push(seq(from, n), 1, false, emit)
}

// 每帧恰为 hop 个样本、出帧即清空：25 帧每秒（hop=1920）下 100 个 480 帧的包恰出 25 帧，第 i 帧的
// 样本为 [1920i, 1920(i+1))，新增样本数为 1920。hop 不小于分析窗长时不保留旧样本。
//
// 变异自证（隔离副本，实测）：newFramer 不论 hop 大小都保留 → hop=1920 时每帧只交出最近 1024 个样本，
// 红。
func TestFramerExactHop(t *testing.T) {
	for _, hop := range []int{1920, 1024} { // 1024 恰等于分析窗长，同样不保留
		var got []gotFrame
		f := newFramer(hop, 1024)
		for i := 0; i < 100; i++ {
			pushSeq(f, i*480, 480, collect(&got))
		}
		if want := 48000 / hop; len(got) != want {
			t.Fatalf("hop=%d: 出帧数 = %d, 期望 %d", hop, len(got), want)
		}
		for i, g := range got {
			if g.fresh != hop || !reflect.DeepEqual(g.batch, seq(i*hop, hop)) {
				t.Fatalf("hop=%d 第 %d 帧: fresh=%d len=%d 首样本=%v, 期望 fresh=%d、样本为 [%d, %d)",
					hop, i, g.fresh, len(g.batch), g.batch[0], hop, i*hop, (i+1)*hop)
			}
		}
	}
}

// hop 不小于分析窗长时不保留任何旧样本，帧内混有静音包时也一样（默认帧率属于这种情形，行为须与引入
// 保留之前完全相同）：先出一个整帧，下一帧前半为静音包、后半有声，交给分析的只有后半新增的样本。
// hop 恰等于窗长时同样不保留。
//
// 变异自证（隔离副本，实测）：newFramer 不论 hop 大小都保留 → hop=1024 的第二帧带上旧样本（长度
// 1024），红；判定写成 hop <= analysisLen → 同样红。
func TestFramerNoRetentionAtOrAboveWindow(t *testing.T) {
	for _, hop := range []int{1024, 1920} {
		var got []gotFrame
		f := newFramer(hop, 1024)
		pushSeq(f, 0, hop, collect(&got))
		f.push(seq(9000, hop/2), 1, true, collect(&got))
		pushSeq(f, hop, hop/2, collect(&got))
		if len(got) != 2 {
			t.Fatalf("hop=%d: 出帧数 = %d, 期望 2", hop, len(got))
		}
		if g := got[1]; g.fresh != hop/2 || !reflect.DeepEqual(g.batch, seq(hop, hop/2)) {
			t.Fatalf("hop=%d 第二帧: fresh=%d len=%d 首样本=%v, 期望只有新增的 [%d, %d)", hop, g.fresh, len(g.batch), g.batch[0], hop, hop+hop/2)
		}
	}
}

// 帧界落在包内时在界上切开：30 帧每秒（hop=1600）下同样的 100 个包恰出 30 帧，每帧 1600 个样本，
// 拼起来就是原序列。
//
// 变异自证（隔离副本，实测）：push 不切包（整包并入、计满即出帧）→ 出 25 帧，红；出帧后直接返回、
// 丢掉包的后一段 → 出 25 帧，红。
func TestFramerSplitsPacketAtBoundary(t *testing.T) {
	var got []gotFrame
	f := newFramer(1600, 1024)
	for i := 0; i < 100; i++ {
		pushSeq(f, i*480, 480, collect(&got))
	}
	if len(got) != 30 {
		t.Fatalf("出帧数 = %d, 期望 30", len(got))
	}
	for i, g := range got {
		if g.fresh != 1600 || !reflect.DeepEqual(g.batch, seq(i*1600, 1600)) {
			t.Fatalf("第 %d 帧: fresh=%d len=%d 首样本=%v, 期望样本为 [%d, %d)", i, g.fresh, len(g.batch), g.batch[0], i*1600, (i+1)*1600)
		}
	}
}

// 一个包可出多帧：hop=400 时，已计入 320 个之后的 480 帧的包跨两个帧界，1200 帧的包含三个帧界。
//
// 变异自证（隔离副本，实测）：push 出帧后直接返回（每次喂入至多出一帧，余下丢弃）→ 480 帧的包只出
// 1 帧，红。
func TestFramerMultipleFramesPerPacket(t *testing.T) {
	var got []gotFrame
	f := newFramer(400, 0)
	pushSeq(f, 0, 320, collect(&got))
	if len(got) != 0 {
		t.Fatalf("未满一帧即出帧: %d", len(got))
	}
	pushSeq(f, 320, 480, collect(&got)) // 累计 800：帧界 400、800
	if len(got) != 2 {
		t.Fatalf("480 帧的包应出 2 帧, 得 %d", len(got))
	}
	pushSeq(f, 800, 1200, collect(&got)) // 累计 2000：帧界 1200、1600、2000
	if len(got) != 5 {
		t.Fatalf("1200 帧的包应再出 3 帧, 共得 %d", len(got))
	}
	for i, g := range got {
		if !reflect.DeepEqual(g.batch, seq(i*400, 400)) {
			t.Fatalf("第 %d 帧样本不是 [%d, %d): len=%d 首样本=%v", i, i*400, (i+1)*400, len(g.batch), g.batch[0])
		}
	}
}

// 出帧数只取决于累计的采样帧数，与包怎么切无关。
//
// 变异自证（隔离副本，实测）：同 TestFramerSplitsPacketAtBoundary 的两种变异，均红（不切包时累计
// 13446 个采样帧只出 4 帧）。
func TestFramerIrregularPackets(t *testing.T) {
	var got []gotFrame
	f := newFramer(1920, 1024)
	total := 0
	for _, n := range []int{1, 479, 481, 4800, 3, 1917, 3845, 2, 1918} {
		pushSeq(f, total, n, collect(&got))
		total += n
	}
	if want := total / 1920; len(got) != want {
		t.Fatalf("累计 %d 个采样帧: 出帧数 = %d, 期望 %d", total, len(got), want)
	}
	var all []float32
	for _, g := range got {
		all = append(all, g.batch...)
	}
	if !reflect.DeepEqual(all, seq(0, len(got)*1920)) {
		t.Fatalf("各帧拼接后不是原序列的前 %d 个样本", len(got)*1920)
	}
}

// 静音包推进计数但不进窗口：全是静音包时每 hop 出一帧保活零帧（batch 为空、fresh 为 0）；一帧内
// 半数为静音包时出真帧，窗口只含非静音样本，fresh 为非静音样本数。
//
// 变异自证（隔离副本，实测）：静音包不推进 filled → 全静音时一帧不出，红；flush 的 fresh 恒报 hop →
// 半静音帧的 fresh 为 1920，红。
func TestFramerSilentPackets(t *testing.T) {
	var got []gotFrame
	f := newFramer(1920, 1024)
	for i := 0; i < 10; i++ { // 4800 个静音采样帧：帧界 1920、3840
		f.push(seq(1, 480), 1, true, collect(&got))
	}
	if len(got) != 2 {
		t.Fatalf("全静音: 出帧数 = %d, 期望 2", len(got))
	}
	for i, g := range got {
		if g.fresh != 0 || len(g.batch) != 0 || g.peaks != ([2]float32{}) {
			t.Fatalf("全静音第 %d 帧应为保活零帧: %+v", i, g)
		}
	}

	got = nil
	f = newFramer(1920, 1024)
	pushSeq(f, 0, 480, collect(&got))
	f.push(seq(9000, 480), 1, true, collect(&got))
	pushSeq(f, 480, 480, collect(&got))
	f.push(seq(9000, 480), 1, true, collect(&got))
	if len(got) != 1 {
		t.Fatalf("半静音: 出帧数 = %d, 期望 1", len(got))
	}
	if g := got[0]; g.fresh != 960 || !reflect.DeepEqual(g.batch, seq(0, 960)) {
		t.Fatalf("半静音帧: fresh=%d len=%d, 期望 fresh=960、样本为 [0, 960)", g.fresh, len(g.batch))
	}
}

// 残帧：不满一帧不出帧；flush 立即出帧并把计数归零，有残余为真帧（fresh 为残余样本数），无残余为
// 保活零帧；之后要重新凑满 hop 才出下一帧。
//
// 变异自证（隔离副本，实测）：计数改在 push 凑满时归零、flush 自身不归零 → 残帧之后再喂 1440 个即
// 出帧，红。
func TestFramerFlushPartial(t *testing.T) {
	var got []gotFrame
	f := newFramer(1920, 1024)
	pushSeq(f, 0, 480, collect(&got))
	if len(got) != 0 {
		t.Fatalf("不满一帧即出帧")
	}
	f.flush(collect(&got))
	if len(got) != 1 || got[0].fresh != 480 || !reflect.DeepEqual(got[0].batch, seq(0, 480)) {
		t.Fatalf("残帧: %+v, 期望 fresh=480、样本为 [0, 480)", got)
	}
	f.flush(collect(&got))
	if len(got) != 2 || got[1].fresh != 0 || len(got[1].batch) != 0 {
		t.Fatalf("无残余时 flush 应出保活零帧: %+v", got[1:])
	}
	pushSeq(f, 480, 1440, collect(&got))
	if len(got) != 2 {
		t.Fatalf("flush 后计数未归零: 再喂 1440 个即出帧")
	}
	pushSeq(f, 1920, 480, collect(&got))
	if len(got) != 3 || got[2].fresh != 1920 || !reflect.DeepEqual(got[2].batch, seq(480, 1920)) {
		t.Fatalf("凑满 hop 应出整帧: 共 %d 帧, 末帧 fresh=%d", len(got), got[len(got)-1].fresh)
	}
}

// 峰值只归入样本所在的那一帧：帧界 1600 落在第 4 个包（采样帧 1440~1919）内，界前的右声道峰 0.5
// （采样帧 1540）进前一帧，界后的左声道峰 0.9（采样帧 1640）进后一帧。
//
// 变异自证（隔离副本，实测）：push 先对整包累积峰值再切分 → 前一帧的左声道峰为 0.9，红；flush 不清
// peaks → 后一帧的右声道峰为 0.5，红。
func TestFramerPeaksStayInOwnFrame(t *testing.T) {
	var got []gotFrame
	f := newFramer(1600, 1024)
	for i := 0; i < 8; i++ { // 3840 个立体声采样帧
		pkt := make([]float32, 960)
		for j := range pkt {
			pkt[j] = 0.1
		}
		if i == 3 {
			pkt[2*100+1] = -0.5 // 采样帧 1540，右声道
			pkt[2*200] = 0.9    // 采样帧 1640，左声道
		}
		f.push(pkt, 2, false, collect(&got))
	}
	if len(got) != 2 {
		t.Fatalf("出帧数 = %d, 期望 2", len(got))
	}
	if want := ([2]float32{0.1, 0.5}); got[0].peaks != want {
		t.Fatalf("前一帧 peaks = %v, 期望 %v", got[0].peaks, want)
	}
	if want := ([2]float32{0.9, 0.1}); got[1].peaks != want {
		t.Fatalf("后一帧 peaks = %v, 期望 %v", got[1].peaks, want)
	}
}

// hop 小于分析窗长（60 帧每秒，hop=800，窗长 1024）：窗口留着此前的非静音样本，连续有声时第 2 帧起
// 交给分析的是最近 1024 个样本，fresh 仍为本帧新增的 800；出过保活零帧后旧样本清空，不进入之后的帧。
//
// 变异自证（隔离副本，实测）：不保留 → 每帧只有 800 个样本，红；保活零帧后不清空 batch → 其后的帧
// 带着静音之前的旧样本（长度 1024），红；fresh 报成交出的样本数 → 第 2 帧为 1024，红；出帧时不截到
// 分析窗长 → 第 2 帧长度 1600，红。
func TestFramerKeepsAnalysisWindow(t *testing.T) {
	var got []gotFrame
	f := newFramer(800, 1024)
	for i := 0; i < 5; i++ { // 2400 个样本：3 帧
		pushSeq(f, i*480, 480, collect(&got))
	}
	if len(got) != 3 {
		t.Fatalf("出帧数 = %d, 期望 3", len(got))
	}
	if g := got[0]; g.fresh != 800 || !reflect.DeepEqual(g.batch, seq(0, 800)) {
		t.Fatalf("首帧没有旧样本可补: fresh=%d len=%d, 期望样本为 [0, 800)", g.fresh, len(g.batch))
	}
	for i := 1; i < 3; i++ {
		if g := got[i]; g.fresh != 800 || !reflect.DeepEqual(g.batch, seq((i+1)*800-1024, 1024)) {
			t.Fatalf("第 %d 帧: fresh=%d len=%d 首样本=%v, 期望 fresh=800、样本为最近 1024 个 [%d, %d)",
				i, g.fresh, len(g.batch), g.batch[0], (i+1)*800-1024, (i+1)*800)
		}
	}

	// 一整帧静音 → 保活零帧；其后的有声帧只含自己的样本。
	f.push(seq(1, 800), 1, true, collect(&got))
	if len(got) != 4 || got[3].fresh != 0 || len(got[3].batch) != 0 {
		t.Fatalf("静音帧应为保活零帧: %+v", got[3:])
	}
	pushSeq(f, 5000, 800, collect(&got))
	if len(got) != 5 || got[4].fresh != 800 || !reflect.DeepEqual(got[4].batch, seq(5000, 800)) {
		t.Fatalf("保活零帧之后的帧不应含旧样本: len=%d 首样本=%v", len(got[4].batch), got[4].batch[0])
	}
}

// 高帧率下帧内混有静音包时，分析窗仍取最近 1024 个非静音样本。hop=800、窗长 1024，依次喂四个 480 帧
// 的包：非静音、非静音、静音、非静音（样本值为非静音样本的序号）。第二帧只新增 320 个样本，而此前已有
// 800 个非静音样本可用，交给分析的应是最近 1024 个 [96, 1120)，不是「上一帧末尾 224 个 + 新增 320 个」。
// 静音包推进帧的计数而不新增样本，保留量若按「每帧恰新增 hop 个」算就不够补满分析窗。
//
// 变异自证（隔离副本，实测）：出帧后只留 analysisLen-hop 个（交出的仍是最近 analysisLen 个）→ 第二帧
// 只有 544 个样本，红；不保留 → 第二帧只有 320 个，红。
func TestFramerHighRateKeepsWindowAcrossSilentPackets(t *testing.T) {
	var got []gotFrame
	f := newFramer(800, 1024)
	pushSeq(f, 0, 480, collect(&got))
	pushSeq(f, 480, 480, collect(&got))
	f.push(seq(9000, 480), 1, true, collect(&got))
	pushSeq(f, 960, 480, collect(&got))
	if len(got) != 2 {
		t.Fatalf("出帧数 = %d, 期望 2", len(got))
	}
	if g := got[0]; g.fresh != 800 || !reflect.DeepEqual(g.batch, seq(0, 800)) {
		t.Fatalf("第一帧: fresh=%d len=%d, 期望 fresh=800、样本为 [0, 800)", g.fresh, len(g.batch))
	}
	if g := got[1]; g.fresh != 320 || !reflect.DeepEqual(g.batch, seq(96, 1024)) {
		t.Fatalf("第二帧: fresh=%d len=%d 首样本=%v, 期望 fresh=320、样本为最近 1024 个 [96, 1120)", g.fresh, len(g.batch), g.batch[0])
	}
}

// 高帧率下静音包与残帧（墙钟兜底的 flush）混在一起：每一帧交给分析的都是最近至多 1024 个非静音样本，
// fresh 只数本帧新增的；出过保活零帧后旧样本清空。hop=800、窗长 1024。
//
// 变异自证（隔离副本，实测）：出帧后只留 analysisLen-hop 个 → 第 2 帧只有 864 个样本，红；出帧时不截
// 到分析窗长（把保留的与新增的全部交出）→ 红；保活零帧后不清空 → 第 5 帧带着 224 个旧样本，红。
func TestFramerHighRateMixedSilenceAndPartial(t *testing.T) {
	var got []gotFrame
	f := newFramer(800, 1024)
	for i := 0; i < 3; i++ { // 非静音 [0, 1440)：凑满一帧，余 640 个计入下一帧
		pushSeq(f, i*480, 480, collect(&got))
	}
	f.push(seq(9000, 160), 1, true, collect(&got)) // 静音包补满第二帧：新增 640 个
	pushSeq(f, 1440, 300, collect(&got))           // 第三帧只来了 300 个
	f.flush(collect(&got))                         // 墙钟兜底：残帧
	f.flush(collect(&got))                         // 再兜底一次，没有新增样本：保活零帧
	pushSeq(f, 1740, 800, collect(&got))           // 保活之后的整帧
	want := []struct {
		fresh, from, n int
	}{
		{800, 0, 800},    // 首帧没有旧样本可补
		{640, 416, 1024}, // [800, 1440) 加上此前的，取最近 1024 个
		{300, 716, 1024}, // 残帧：[1440, 1740) 加上此前的
		{0, 0, 0},        // 保活零帧
		{800, 1740, 800}, // 旧样本已清空
	}
	if len(got) != len(want) {
		t.Fatalf("出帧数 = %d, 期望 %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.fresh != w.fresh || len(g.batch) != w.n || (w.n > 0 && !reflect.DeepEqual(g.batch, seq(w.from, w.n))) {
			first := float32(-1)
			if len(g.batch) > 0 {
				first = g.batch[0]
			}
			t.Fatalf("第 %d 帧: fresh=%d len=%d 首样本=%v, 期望 fresh=%d、样本为 [%d, %d)", i+1, g.fresh, len(g.batch), first, w.fresh, w.from, w.from+w.n)
		}
	}
}

// 经 push 并入的包照样清洗非有限采样（NaN、±Inf 按 0 计入），帧内的样本与峰值都是有限值。清洗本身
// 的边界由 TestWindow 守，这里守的是 push 确实经过 window.add。
//
// 变异自证（隔离副本，实测）：push 绕过 add、自行内联降混与取峰 → 帧内出现 NaN，红。
func TestFramerCleansNonFiniteSamples(t *testing.T) {
	var got []gotFrame
	f := newFramer(3, 0)
	nan, inf := float32(math.NaN()), float32(math.Inf(1))
	f.push([]float32{nan, 0.2, -0.8, inf, -inf, 0.6}, 2, false, collect(&got))
	if len(got) != 1 {
		t.Fatalf("出帧数 = %d, 期望 1", len(got))
	}
	assertFinite(t, "batch", got[0].batch)
	assertFinite(t, "peaks", got[0].peaks[:])
	assertClose(t, "batch", got[0].batch, []float32{0.1, -0.4, 0.3})
	if want := ([2]float32{0.8, 0.6}); got[0].peaks != want {
		t.Fatalf("peaks = %v, 期望 %v", got[0].peaks, want)
	}
}

// 空包与不足一个采样帧的包既不出帧也不死循环；声道数不为正按单声道计。
//
// 变异自证（隔离副本，实测）：去掉 push 里对 channels 的下限 → channels 为 0 时除零 panic，红；
// 去掉 newFramer 里对 hop 的下限 → hop 为 0，红。
func TestFramerDegenerateInput(t *testing.T) {
	var got []gotFrame
	f := newFramer(4, 0)
	f.push(nil, 2, false, collect(&got))
	f.push([]float32{0.5}, 2, false, collect(&got)) // 半个立体声采样帧
	if len(got) != 0 || f.filled != 0 {
		t.Fatalf("空包或半帧不应计入: 出帧 %d, filled=%d", len(got), f.filled)
	}
	f.push([]float32{1, 2, 3, 4}, 0, false, collect(&got)) // 声道数 0 按单声道
	if len(got) != 1 || !reflect.DeepEqual(got[0].batch, []float32{1, 2, 3, 4}) {
		t.Fatalf("声道数为 0 应按单声道: %+v", got)
	}
	if g := newFramer(0, 1024); g.hop != 1 {
		t.Fatalf("hop 小于 1 应按 1 计, 得 %d", g.hop)
	}
}

// 帧率到每帧采样帧数的换算：整除时精确，不能整除时向下取整，帧率不为正或高于采样率时至少为 1。
//
// 变异自证（隔离副本，实测）：改为四舍五入 → 48000/27 = 1777.78 得 1778，红；去掉对帧率的下限 →
// 帧率 0 时除零 panic，红；去掉对结果的下限 → 帧率高于采样率时得 0，红。
func TestHopFor(t *testing.T) {
	for _, c := range []struct{ rate, fps, want int }{
		{48000, 25, 1920}, {48000, 30, 1600}, {48000, 60, 800}, {48000, 15, 3200},
		{48000, 26, 1846}, {48000, 27, 1777},
		{48000, 0, 48000}, {48000, -3, 48000}, {48000, 96000, 1},
	} {
		if got := hopFor(c.rate, c.fps); got != c.want {
			t.Errorf("hopFor(%d, %d) = %d, 期望 %d", c.rate, c.fps, got, c.want)
		}
	}
}
