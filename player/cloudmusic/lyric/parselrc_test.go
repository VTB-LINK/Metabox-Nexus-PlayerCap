package lyric

// 本文件只测 ParseLRC 的 LRC 解析：行首时间戳（含压缩型「一句配多个时间戳」）的剥离、
// 每个时间戳各生成一条、结果按时间排序并重排 Index。不测取词请求或 YRC 合并。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseLRCCompressedTimestamps 钉死压缩型 LRC：一句配多个时间戳时，每个时间戳各生成
// 一条歌词行，正文不得带任何裸时间戳。
//
// 缺陷背景：原正则 `\[(\d+):(\d+)\.(\d+)\](.*)` 的 `(.*)` 从第一个 ] 贪心吃到行尾，对
// `[02:04.45][00:47.19]文字` 只生成一条 {Time:124.45, Text:"[00:47.19]文字"}——正文带着
// 裸时间戳直接上 OBS，且首次出现（47.19）那段整段没歌词。网易云客户端对这种歌是在两个
// 时刻各显示一次的（真机截图确认），压缩型的语义就是「这句在这几个时刻各出现一次」。
//
// 变异自证：把 ParseLRC 改回「只取第一个时间戳 + 正文贪心到行尾」即红。
func TestParseLRCCompressedTimestamps(t *testing.T) {
	// 网易云对压缩型按**降序**排（较晚的时间戳在前），故必须排序后才能升序输出
	got := ParseLRC("[02:04.45][00:47.19]AAA\n[00:17.32]BBB\n")

	if len(got) != 3 {
		t.Fatalf("压缩行应生成 2 条 + 普通行 1 条 = 3 条，实得 %d 条: %+v", len(got), got)
	}
	for i, l := range got {
		if strings.Contains(l.Text, "[") {
			t.Errorf("第 %d 条正文含裸时间戳（会直接显示在 OBS 上）: %q", i, l.Text)
		}
	}
	// 必须按时间升序，且 Index 与时间顺序一致
	wantTimes := []float32{17.32, 47.19, 124.45}
	wantTexts := []string{"BBB", "AAA", "AAA"}
	for i := range wantTimes {
		if got[i].Time != wantTimes[i] {
			t.Errorf("第 %d 条 Time 应为 %.2f，实得 %.2f", i, wantTimes[i], got[i].Time)
		}
		if got[i].Text != wantTexts[i] {
			t.Errorf("第 %d 条 Text 应为 %q，实得 %q", i, wantTexts[i], got[i].Text)
		}
		if got[i].Index != i {
			t.Errorf("Index 应按时间顺序重排为 %d，实得 %d", i, got[i].Index)
		}
	}
}

// TestParseLRCManyTimestamps 压缩型的时间戳**不限于两个**——一句副歌/叠句在整首里出现
// 多少次，行首就有多少个时间戳。这里用 4 个，且故意乱序，确认每个都独立成条并排到正确位置。
func TestParseLRCManyTimestamps(t *testing.T) {
	got := ParseLRC("[03:10.00][00:30.50][02:00.25][01:15.75]HOOK\n[00:05.00]INTRO\n")

	if len(got) != 5 {
		t.Fatalf("4 个时间戳应生成 4 条 + 普通行 1 条 = 5 条，实得 %d 条: %+v", len(got), got)
	}
	wantTimes := []float32{5.0, 30.5, 75.75, 120.25, 190.0}
	for i, want := range wantTimes {
		if got[i].Time != want {
			t.Errorf("第 %d 条 Time 应为 %.2f，实得 %.2f", i, want, got[i].Time)
		}
		if got[i].Index != i {
			t.Errorf("第 %d 条 Index 应为 %d，实得 %d", i, i, got[i].Index)
		}
		if strings.Contains(got[i].Text, "[") {
			t.Errorf("第 %d 条正文含裸时间戳: %q", i, got[i].Text)
		}
	}
	// 那一句应在 4 个时刻各出现一次，正文相同
	for _, i := range []int{1, 2, 3, 4} {
		if got[i].Text != "HOOK" {
			t.Errorf("第 %d 条应为叠句 HOOK，实得 %q", i, got[i].Text)
		}
	}
}

// TestParseLRCKeepsIntentionalBlank 钉死 intentional blank 必须**原样保留**。
//
// 「有时间戳、正文为空」的行不是垃圾，是作词者要求「此刻把歌词清掉」（间奏 / 句间停顿）。
// 真实数据实证（网易云 id=5277704，见 testdata）：其中一条 blank 自己就是压缩型
// （`[02:32.56][01:15.40]`，两个时刻各清一次），且下一句在它之后 **0.18s** 就来——即精确
// 要求「上句显示 1.97s → 清空 0.18s → 下句」。吞掉它，前端在该空的地方会一直糊着上一句。
//
// 本测试的前身把相反的行为（跳过空正文）钉死成了「预期」，是把既有 bug 固化成门禁。
// 保留 blank 后，「这首歌有没有歌词」不能再按行数判 —— 见 cloudmusic 的 hasRealText。
//
// 变异自证：给 ParseLRC 加回 `if text == "" { continue }` 即红。
func TestParseLRCKeepsIntentionalBlank(t *testing.T) {
	// blank 也可能是压缩型（一条 blank 配多个时刻），故每个时间戳同样各生成一条
	got := ParseLRC("[00:05.00]REAL\n[00:30.00][01:00.00]\n[01:15.40]   \n")

	if len(got) != 4 {
		t.Fatalf("1 条实词 + 压缩型 blank 2 条 + 单 blank 1 条 = 4 条，实得 %d 条: %+v", len(got), got)
	}
	want := []struct {
		time float32
		text string
	}{
		{5.0, "REAL"},
		{30.0, ""}, // 压缩型 blank 的第一个时刻
		{60.0, ""}, // 同一条 blank 的第二个时刻
		{75.4, ""}, // 只有空白字符的正文，同样是 blank
	}
	for i, w := range want {
		if got[i].Time != w.time {
			t.Errorf("第 %d 条 Time 应为 %.2f，实得 %.2f", i, w.time, got[i].Time)
		}
		if got[i].Text != w.text {
			t.Errorf("第 %d 条 Text 应为 %q，实得 %q", i, w.text, got[i].Text)
		}
		if got[i].Index != i {
			t.Errorf("第 %d 条 Index 应为 %d，实得 %d", i, i, got[i].Index)
		}
	}
}

// TestParseLRCRealSongKeepsBlanks 真实数据锚定：id=5277704 实测有 4 条 intentional blank
// （其中 3 条是压缩型，各占两个时刻），解析后必须一条不少地保留下来。
func TestParseLRCRealSongKeepsBlanks(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "compressed_lrc.txt"))
	if err != nil {
		t.Fatalf("读 testdata 失败: %v", err)
	}
	got := ParseLRC(string(data))
	blanks := 0
	for _, l := range got {
		if l.Text == "" {
			blanks++
		}
	}
	// 原文 4 条 blank 行：1 条单时间戳 + 3 条压缩型（各 2 个时刻）= 1 + 6 = 7 个清空时刻
	if blanks != 7 {
		t.Errorf("真实歌曲的 intentional blank 应保留 7 个清空时刻（1 单 + 3 压缩型×2），实得 %d", blanks)
	}
	if !containsRealText(got) {
		t.Error("这首歌应有实词行")
	}
}

func containsRealText(lines []LyricLine) bool {
	for _, l := range lines {
		if l.Text != "" {
			return true
		}
	}
	return false
}

// TestParseLRCNormalUnchanged 反方向：普通 LRC（无压缩）的行为不得变化。
// 没有这条，「无条件把每行拆成多条」之类的改动能通过上面那条。
func TestParseLRCNormalUnchanged(t *testing.T) {
	got := ParseLRC("[ti:标题]\n[00:01.00]AAA\n[00:05.50]BBB\n[00:10.123]CCC\n")
	if len(got) != 3 {
		t.Fatalf("3 个普通行应生成 3 条（[ti:] 元数据行跳过），实得 %d 条: %+v", len(got), got)
	}
	wantTimes := []float32{1.0, 5.5, 10.123}
	for i, want := range wantTimes {
		if got[i].Time != want {
			t.Errorf("第 %d 条 Time 应为 %.3f，实得 %.3f", i, want, got[i].Time)
		}
		if got[i].Index != i {
			t.Errorf("第 %d 条 Index 应为 %d，实得 %d", i, i, got[i].Index)
		}
	}
	// 毫秒归一化：「1」→100ms、「12」→120ms、「123」→123ms
	if ms := ParseLRC("[00:00.1]X\n"); len(ms) != 1 || ms[0].Time != 0.1 {
		t.Errorf("毫秒「1」应归一为 100ms（0.1s），实得 %+v", ms)
	}
}

// TestParseLRCRealCompressedSong 用真实歌曲（网易云 id=5277704）的 lrc 锚定：解析结果里
// 不得有任何一条正文带裸时间戳，且必须按时间升序。
//
// 该曲实测 24 行里 16 行是压缩型（真实生产数据，非构造样本）——这是本修复可达性的证据。
func TestParseLRCRealCompressedSong(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "compressed_lrc.txt"))
	if err != nil {
		t.Fatalf("读 testdata 失败: %v", err)
	}
	got := ParseLRC(string(data))
	if len(got) == 0 {
		t.Fatal("真实压缩型歌词应解析出行，实得 0 条")
	}
	for i, l := range got {
		if strings.Contains(l.Text, "[") {
			t.Errorf("第 %d 条正文含裸时间戳: %q", i, l.Text)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time < got[i-1].Time {
			t.Errorf("结果必须按时间升序：第 %d 条 %.2f < 第 %d 条 %.2f", i, got[i].Time, i-1, got[i-1].Time)
		}
		if got[i].Index != i {
			t.Errorf("Index 应按时间顺序重排：第 %d 条实得 %d", i, got[i].Index)
		}
	}
	t.Logf("真实压缩型歌曲解析出 %d 条（压缩行各时间戳独立成条）", len(got))
}

// TestParseLRCRealNormalSong 对照组：不含压缩型的真实歌曲（id=156736，实测 0 行压缩型）
// 解析结果同样不得有裸时间戳、且升序——确认修复没把正常歌搞坏。
func TestParseLRCRealNormalSong(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "normal_lrc.txt"))
	if err != nil {
		t.Fatalf("读 testdata 失败: %v", err)
	}
	got := ParseLRC(string(data))
	if len(got) == 0 {
		t.Fatal("真实普通歌词应解析出行，实得 0 条")
	}
	for i, l := range got {
		if strings.Contains(l.Text, "[") {
			t.Errorf("第 %d 条正文含裸时间戳: %q", i, l.Text)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time < got[i-1].Time {
			t.Errorf("结果必须按时间升序：第 %d 条 %.2f < 第 %d 条 %.2f", i, got[i].Time, i-1, got[i-1].Time)
		}
	}
	t.Logf("真实普通歌曲解析出 %d 条", len(got))
}
