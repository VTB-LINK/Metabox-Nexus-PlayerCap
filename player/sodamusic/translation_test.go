package sodamusic

// 本文件测汽水翻译轨（tlyric 式独立 LRC）的解析与按时间戳合并：applySodaTranslations /
// lookupNearest / parseTranslationLRC / lrcTagToMs。
//
// 要害是**按绝对时间戳对齐**（≠ 酷狗按行号），且容差**有界**（±10ms = 译轨的厘秒精度）。
// 取自实测《Falling Stars and Broken Promises》：KRC 行 [16120,..] 的译文是 tlyric 的
// [00:16.12]，两者毫秒一致。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/player/krc"
)

// mm:ss.xx / mm:ss:xx / mm:ss 三种写法都要解对；元数据/畸形标签返回 false。
// 变异自证：把 `(float64(min)*60+sec)*1000` 里的 *60 去掉，00:16.12→16120 会变 16120 之外而红。
func TestLrcTagToMs(t *testing.T) {
	cases := []struct {
		tag  string
		ms   int
		ok   bool
		note string
	}{
		{"00:16.12", 16120, true, "百分秒"},
		{"01:06.24", 66240, true, "分钟进位 = 60000+6240"},
		{"03:36.64", 216640, true, "3 分 36.64 秒"},
		{"01:30", 90000, true, "无小数"},
		{"00:16:12", 16120, true, "冒号分隔的百分秒"},
		{"ti:晴天", 0, false, "元数据标签非时间戳"},
		{"by:", 0, false, "空冒号后"},
		{"garbage", 0, false, "无冒号"},
	}
	for _, c := range cases {
		ms, ok := lrcTagToMs(c.tag)
		if ok != c.ok || (ok && ms != c.ms) {
			t.Fatalf("lrcTagToMs(%q) = (%d,%v), want (%d,%v) —— %s", c.tag, ms, ok, c.ms, c.ok, c.note)
		}
	}
}

// 解析多行 LRC → map[毫秒]译文；文本取最后一个时间戳之后那段；元数据行不入表。
func TestParseTranslationLRC(t *testing.T) {
	lrc := "[ti:x]\n[00:16.12]我们曾在午夜星空许下心愿\n[00:20.08]期盼爱意永远不会消散\n\n[bad line no tag]"
	m := parseTranslationLRC(lrc)
	if len(m) != 2 {
		t.Fatalf("表长 = %d, want 2（[ti:] 与无标签行应被跳过）", len(m))
	}
	if m[16120] != "我们曾在午夜星空许下心愿" {
		t.Fatalf("m[16120] = %q", m[16120])
	}
	if m[20080] != "期盼爱意永远不会消散" {
		t.Fatalf("m[20080] = %q", m[20080])
	}
}

// 压缩型：一行多个时间戳 → 各时间戳都指向同一段译文。
func TestParseTranslationLRCCompressed(t *testing.T) {
	m := parseTranslationLRC("[00:10.00][00:50.00]副歌译文")
	if m[10000] != "副歌译文" || m[50000] != "副歌译文" {
		t.Fatalf("压缩型两个时间戳应都命中同一译文，got %q / %q", m[10000], m[50000])
	}
}

// ★ 核心：按绝对时间戳把译文合并进 SubText。KRC 行 Time（秒）与译文毫秒同源对齐。
// 变异自证：把 applySodaTranslations 里 roundToMs(lines[i].Time) 改成按 Index 取，则第 3 行
// （无对应译文）会错拿到译文而红。
func TestApplySodaTranslationsAlignByTimestamp(t *testing.T) {
	lines := []krc.Line{
		{Index: 0, Time: 16.12, Text: "We made wishes on the midnight sky"},
		{Index: 1, Time: 20.08, Text: "Hoping love would never fade"},
		{Index: 2, Time: 99.00, Text: "line with no translation"}, // 译轨没覆盖 → 应留空
	}
	lrc := "[00:16.12]我们曾在午夜星空许下心愿\n[00:20.08]期盼爱意永远不会消散"
	applySodaTranslations(lines, lrc)
	if lines[0].SubText != "我们曾在午夜星空许下心愿" {
		t.Fatalf("行0 SubText = %q", lines[0].SubText)
	}
	if lines[1].SubText != "期盼爱意永远不会消散" {
		t.Fatalf("行1 SubText = %q", lines[1].SubText)
	}
	if lines[2].SubText != "" {
		t.Fatalf("行2 无对应译文，SubText 应为空，got %q（是否误按行号对齐？）", lines[2].SubText)
	}
}

// 容差是**有界**的 ±10ms（= 译轨自身的厘秒精度），边界内命中、边界外留空。
// 上界钉死这一条：差 80ms 的行绝不强塞——那已不是精度差异，是两条时间轴。
//
// 变异自证：把 matchToleranceMs 改成 100，「差 80ms 不命中」当场红；改成 0，
// 「差 10ms 命中」当场红。两侧都被钉住，不是单边断言。
func TestApplySodaTranslationsToleranceBounds(t *testing.T) {
	cases := []struct {
		name    string
		lineSec float32
		lrc     string
		want    string
	}{
		{"精确相等", 16.12, "[00:16.12]译文", "译文"},
		{"差 10ms（厘秒制译轨 vs 毫秒制 KRC，边界内）", 16.13, "[00:16.12]译文", "译文"},
		{"差 11ms（越界）", 16.131, "[00:16.12]译文", ""},
		{"差 80ms（两条时间轴，绝不强塞）", 16.20, "[00:16.12]译文", ""},
	}
	for _, c := range cases {
		lines := []krc.Line{{Index: 0, Time: c.lineSec, Text: "x"}}
		applySodaTranslations(lines, c.lrc)
		if lines[0].SubText != c.want {
			t.Fatalf("%s：SubText = %q, want %q", c.name, lines[0].SubText, c.want)
		}
	}
}

// 容差内有两个候选时取**最近邻**，不是「先扫到谁算谁」。
// 变异自证：把 lookupNearest 的探测改成从 -matchToleranceMs 单向递增扫，本例会拿到远端那条而红。
func TestLookupNearestPicksClosest(t *testing.T) {
	tmap := map[int]string{16112: "远", 16125: "近"} // 目标 16120：距离 8 vs 5
	got, ok := lookupNearest(tmap, 16120)
	if !ok || got != "近" {
		t.Fatalf("lookupNearest = (%q,%v), want (\"近\",true)——应取最近邻", got, ok)
	}
}

// 空翻译轨 / 空歌词 = no-op，不 panic。
func TestApplySodaTranslationsEmptyNoop(t *testing.T) {
	lines := []krc.Line{{Index: 0, Time: 1.0, Text: "a"}}
	applySodaTranslations(lines, "")
	if lines[0].SubText != "" {
		t.Fatalf("空翻译轨应不动 SubText")
	}
	applySodaTranslations(nil, "[00:01.00]x") // 空歌词不 panic
}
