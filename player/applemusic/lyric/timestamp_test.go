package lyric

// 本文件只测 TTML 时间表达式的解析（parseTS）：时钟写法 `[h:][m:]ss.mmm` 与带度量后缀的偏移写法
// （`1.127s`、`500ms`），以及整份文档用偏移写法时的行与逐字时间。非有限值的拒收见 ttml_nonfinite_test.go。

import (
	"math"
	"os"
	"testing"
)

func approx(a, b float32) bool { return math.Abs(float64(a-b)) < 0.002 }

// 时间戳三种写法都要解对。变异自证：把 parseTS 里 `mult *= 60` 删掉，"4:29.747" 会解成 4*60+29.747 之外而红。
func TestParseTS(t *testing.T) {
	cases := []struct {
		s  string
		v  float32
		ok bool
	}{
		{"29.188", 29.188, true},
		{"1:05.491", 65.491, true},
		{"4:29.747", 269.747, true},
		{"1:02:03.500", 3723.5, true},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, c := range cases {
		v, ok := parseTS(c.s)
		if ok != c.ok || (ok && !approx(v, c.v)) {
			t.Fatalf("parseTS(%q)=(%v,%v) want (%v,%v)", c.s, v, ok, c.v, c.ok)
		}
	}
}

// 偏移写法：数值后紧跟度量 h / m / s / ms，按单位换算成秒。真机见过整份文档都写成 "1.127s" 的
// （2026-10-08），不认后缀时每一行都被跳过。含冒号的时钟写法不接受后缀；数值与后缀之间不得有空白；
// 原有的拒绝规则（NaN、Inf、负数、float32 溢出、空串）对带后缀的写法同样生效。
//
// 变异自证（隔离副本，五种均实测变红）：
//   - 去掉后缀支持（parseTS 不查 metricUnits）：接受组里带后缀的 13 例全红。
//   - metricUnits 里把 "s" 挪到 "ms" 之前："500ms"、"0ms" 被切成数值 "500m" 加 "s"，解析失败而红。
//   - 换算时不乘单位（sec = last）："500ms"、"2m"、"0.5m"、"1h"、"1.5h" 的值不对，"1e36h"、"1e37m" 被接受。
//   - 含冒号时也剥后缀："1:05.5s"、"1:05m"、"1:02:03.5ms"、"0:30h" 被接受。
//   - 剥掉后缀后再 TrimSpace："1 s"、"1.5 ms"、"2\tm" 被接受。
func TestParseTSMetricSuffix(t *testing.T) {
	accept := []struct {
		s string
		v float32
	}{
		{"1.127s", 1.127},
		{"24.32s", 24.32},
		{"218.5s", 218.5},
		{"0s", 0},
		{"500ms", 0.5},
		{"0ms", 0},
		{"2m", 120},
		{"0.5m", 30},
		{"1h", 3600},
		{"1.5h", 5400},
		{" 1.5s ", 1.5},
		{"\t40s\n", 40},
		{".5s", 0.5},
		// 原有写法结果不变
		{"29.188", 29.188},
		{"1:05.491", 65.491},
		{"4:29.747", 269.747},
		{"1:02:03.500", 3723.5},
	}
	for _, c := range accept {
		if v, ok := parseTS(c.s); !ok || !approx(v, c.v) {
			t.Errorf("parseTS(%q)=(%v,%v) want (%v,true)", c.s, v, ok, c.v)
		}
	}
	reject := []string{
		"s", "ms", "h", "m", // 只有后缀
		"1.2ss", "1sm", "1hs", "1mss", // 后缀叠写
		"1:05.5s", "1:05m", "1:02:03.5ms", "0:30h", // 时钟写法不接受后缀
		"1.5x", "1.5S", "1.5MS", "5sec", // 不认识的后缀（度量区分大小写）
		"1 s", "1.5 ms", "2\tm", // 数值与后缀之间有空白
		"s5", "ms5", "s1.5", // 后缀在前
		"-1s", "-0.5ms", "-2m", // 负数
		"NaNs", "nans", "Infs", "infs", "+Infs", "-Infs", "Infinitys", // 非有限
		"1e999s", "1e39s", "1e36h", "1e37m", // float64 / float32 溢出（含换算后才溢出的）
	}
	for _, s := range reject {
		if v, ok := parseTS(s); ok {
			t.Errorf("parseTS(%q) 应拒绝，却得到 (%v,true)", s, v)
		}
	}
}

// 其余时间表达式：度量 min（SMIL 时钟值的写法）与 m 同义；时钟写法至多三段（时:分:秒），
// 四段以上（如带帧号的 hh:mm:ss:ff）拒绝，不再多乘一级 60 算出错值。
//
// 变异自证（隔离副本，实测）：从 metricUnits 去掉 "min"，"2min"、"0.5min" 变红；去掉「超过三段即拒」的
// 判断，"0:0:5:12" 被解成 312、"1:2:3:4"、"0:0:0:0:5" 也被接受而红。
func TestParseTSMinAndSegmentLimit(t *testing.T) {
	accept := []struct {
		s string
		v float32
	}{
		{"2min", 120},
		{"0.5min", 30},
		{"1127ms", 1.127},
		{"1:05", 65},
		{"00:01:05.5", 65.5},
		{"1:02:03.500", 3723.5},
	}
	for _, c := range accept {
		if v, ok := parseTS(c.s); !ok || !approx(v, c.v) {
			t.Errorf("parseTS(%q)=(%v,%v) want (%v,true)", c.s, v, ok, c.v)
		}
	}
	reject := []string{
		"0:0:5:12", "1:2:3:4", "0:0:0:0:5", // 超过三段
		"1,127", "1:05,5", // 逗号小数点
		"min", "2 min", "2mins", "2Min", "1:05min", "-2min", // min 的误写
		":5", "5:", "1::5", // 空段
	}
	for _, s := range reject {
		if v, ok := parseTS(s); ok {
			t.Errorf("parseTS(%q) 应拒绝，却得到 (%v,true)", s, v)
		}
	}
}

// 整份 Line 型文档用偏移写法。夹具 testdata/offset_suffix_line.xml 按真机见到的结构自拟（文字是编的）：
// 根元素没有 xmlns:ttm，<translations/> 为空，<iTunesMetadata … leadingSilence>，<div> 与 <p> 的
// begin / end 全带 s 后缀，<body dur> 仍是时钟写法。
//
// 变异自证（隔离副本，实测）：去掉后缀支持，行数变 0 而红。
func TestParseTTMLOffsetTimeLineFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/offset_suffix_line.xml")
	if err != nil {
		t.Fatalf("读夹具: %v", err)
	}
	doc := string(data)
	if d, ok := DocDuration(doc); !ok || !approx(d, 233.56) {
		t.Fatalf("DocDuration=(%v,%v) want (233.56,true)", d, ok)
	}
	lines := ParseTTML(doc)
	want := []struct {
		time float32
		text string
	}{
		{1.127, "夹具第一行 纸船漂过水洼"},
		{5.9, "夹具第二行 路灯数到第七盏"},
		{10.5, "夹具第三行 口袋里的车票皱了"},
		{24.32, "夹具第四行 雨停在屋檐下"},
		{30.5, "夹具第五行 远处有人收起伞"},
		{218.5, "夹具第六行 末班车亮着灯"},
	}
	if len(lines) != len(want) {
		t.Fatalf("行数=%d want %d", len(lines), len(want))
	}
	for i, w := range want {
		l := lines[i]
		if l.Index != i || !approx(l.Time, w.time) || l.Text != w.text {
			t.Errorf("lines[%d]={Index:%d Time:%v Text:%q} want {%d %v %q}", i, l.Index, l.Time, l.Text, i, w.time, w.text)
		}
		if l.SubText != "" || len(l.Detailed.Words) != 0 {
			t.Errorf("lines[%d]: 无译轨的 Line 型不应有译文或逐字: %+v", i, l)
		}
	}
}

// Word 型文档用偏移写法：行、逐字的起点与时长都按后缀换算（s 与 ms 混用），<body dur> 带后缀也认。
//
// 变异自证（隔离副本，实测）：去掉后缀支持，DocDuration 失败、行数变 0 而红；把 "s" 挪到 "ms" 之前，
// 第二个词的 end="2500ms" 解不出、时长变 0 而红；换算时不乘单位，该词时长变成 2498.25 而红。
func TestParseTTMLOffsetTimeWords(t *testing.T) {
	doc := `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" itunes:timing="Word">` +
		`<body dur="12.5s"><div begin="1.0s" end="4.0s">` +
		`<p begin="1.0s" end="4.0s" itunes:key="L1"><span begin="1.0s" end="1.5s">Paper</span> <span begin="1.75s" end="2500ms">boat</span> <span begin="3s" end="4.0s">drifts</span></p>` +
		`</div></body></tt>`
	if d, ok := DocDuration(doc); !ok || !approx(d, 12.5) {
		t.Errorf("DocDuration=(%v,%v) want (12.5,true)", d, ok)
	}
	lines := ParseTTML(doc)
	if len(lines) != 1 {
		t.Fatalf("行数=%d want 1", len(lines))
	}
	l := lines[0]
	if !approx(l.Time, 1.0) || l.Text != "Paper boat drifts" {
		t.Fatalf("行={Time:%v Text:%q} want {1.0 \"Paper boat drifts\"}", l.Time, l.Text)
	}
	if !approx(l.Detailed.Timestamp, 1.0) || !approx(l.Detailed.Duration, 3.0) {
		t.Fatalf("Detailed={Timestamp:%v Duration:%v} want {1.0 3.0}", l.Detailed.Timestamp, l.Detailed.Duration)
	}
	want := []struct {
		text    string
		ts, dur float32
	}{
		{"Paper ", 1.0, 0.5},
		{"boat ", 1.75, 0.75},
		{"drifts", 3.0, 1.0},
	}
	w := l.Detailed.Words
	if len(w) != len(want) {
		t.Fatalf("逐字数=%d want %d", len(w), len(want))
	}
	for i, c := range want {
		if w[i].Text != c.text || !approx(w[i].Timestamp, c.ts) || !approx(w[i].Duration, c.dur) {
			t.Errorf("word%d=%+v want {%q %v %v}", i, w[i], c.text, c.ts, c.dur)
		}
	}
}
