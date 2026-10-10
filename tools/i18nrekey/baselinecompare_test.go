package main

// 本文件只测一件事：-baseline 的比对抓得住改写前后文字不同的各种情形，不误报文字相同的。

import (
	"strings"
	"testing"
)

// baselineBefore 是改写前的一棵小树：旧方向的表，中文字面量既有直接实参也有经变量转手的。
func baselineBefore() map[string]string {
	return map[string]string{
		"i18n/messages.go": `package i18n

var english = map[string]string{
	"日志甲 %s": "log A %s",
	"经变量传入":  "via variable",
}
`,
		"p/p.go": `package p

import "Metabox-Nexus-PlayerCap/i18n"

var note = "经变量传入"

func f() {
	log.Info("日志甲 %s", 1)
	log.Info("plain %d", 2)
	println(i18n.T(note), "与 i18n 无关")
}
`,
	}
}

// baselineAfter 是它正确改写后的样子：直接实参与经变量转手的都换成英文，表对调，
// 另为原本就是英文的那条日志补了键值相同的条目。
func baselineAfter() map[string]string {
	return map[string]string{
		"i18n/messages.go": `package i18n

var chinese = map[string]string{
	"log A %s":     "日志甲 %s",
	"via variable": "经变量传入",
	"plain %d":     "plain %d",
}
`,
		"p/p.go": `package p

import "Metabox-Nexus-PlayerCap/i18n"

var note = "via variable"

func f() {
	log.Info("log A %s", 1)
	log.Info("plain %d", 2)
	println(i18n.T(note), "与 i18n 无关")
}
`,
	}
}

// TestBaselineCompare 从一对正确的改写出发，每例只改坏一处，断言比对报出差异。
//
// 变异自证（实测）：
//   - compare 不比直接实参在中文环境下的文字：「译文换了一句」「原本的英文日志被配上中文」两例红（不再报差异）。
//   - 不比英文环境下的文字：「调用点换成另一句英文」例红。
//   - 其余字面量有改动一律放行：「无关的字面量被改」「经变量转手的换成表外的英文」两例红。
//   - 其余字面量有改动一律报差异：「正确的改写」例红。
//   - 不比译表：「译表少一条」例红。
//   - 字面量处数不同也照常配对：「多出一处字面量」例红。
//   - 不查配对的两处是否同为直接实参：「原来的直接实参不再交给 logger」例红。
//   - render 把旧方向条目也按新方向算：「正确的改写」例红。
func TestBaselineCompare(t *testing.T) {
	cases := []struct {
		name      string
		file      string // 改坏改写后的哪个文件
		old, new  string // 把 old 换成 new
		wantDiffs string // 差异里应当出现的片段；空串表示无差异
	}{
		{name: "正确的改写"},
		{name: "调用点换成另一句英文", file: "p/p.go",
			old: `log.Info("log A %s", 1)`, new: `log.Info("log B %s", 1)`, wantDiffs: "英文环境"},
		{name: "调用点没改，仍是中文", file: "p/p.go",
			old: `log.Info("log A %s", 1)`, new: `log.Info("日志甲 %s", 1)`, wantDiffs: "英文环境"},
		{name: "译文换了一句", file: "i18n/messages.go",
			old: `"log A %s":     "日志甲 %s",`, new: `"log A %s":     "日志子 %s",`, wantDiffs: "中文环境"},
		{name: "原本的英文日志被配上中文", file: "i18n/messages.go",
			old: `"plain %d":     "plain %d",`, new: `"plain %d":     "朴素 %d",`, wantDiffs: "中文环境"},
		{name: "经变量转手的换成表外的英文", file: "p/p.go",
			old: `var note = "via variable"`, new: `var note = "by variable"`, wantDiffs: "不是按译表"},
		// 比对只看改动：没改的非直接实参它看不出该不该改，由核对结果里「与译表里的中文逐字相同」那一项列出。
		{name: "经变量转手的没改，仍是中文", file: "p/p.go",
			old: `var note = "via variable"`, new: `var note = "经变量传入"`},
		{name: "无关的字面量被改", file: "p/p.go",
			old: `"与 i18n 无关"`, new: `"unrelated"`, wantDiffs: "不是按译表"},
		{name: "译表少一条", file: "i18n/messages.go",
			old: `"via variable": "经变量传入",`, new: ``, wantDiffs: "译表少了一对"},
		{name: "多出一处字面量", file: "p/p.go",
			old: `log.Info("plain %d", 2)`, new: `log.Info("plain %d", 2); println("extra")`, wantDiffs: "无法配对"},
		{name: "原来的直接实参不再交给 logger", file: "p/p.go",
			old: `log.Info("log A %s", 1)`, new: `println("log A %s", 1)`, wantDiffs: "是否为直接实参"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			after := baselineAfter()
			if c.file != "" {
				if !strings.Contains(after[c.file], c.old) {
					t.Fatalf("用例写错：%s 里没有 %q", c.file, c.old)
				}
				after[c.file] = strings.Replace(after[c.file], c.old, c.new, 1)
			}
			diffs, summary := compare(mustTree(t, baselineBefore()), mustTree(t, after))
			joined := strings.Join(diffs, "\n")
			switch {
			case c.wantDiffs == "" && len(diffs) != 0:
				t.Errorf("不该有差异（%s）：\n%s", summary, joined)
			case c.wantDiffs != "" && !strings.Contains(joined, c.wantDiffs):
				t.Errorf("差异里应当有 %q，得到：\n%s", c.wantDiffs, joined)
			}
		})
	}
}

// TestBaselineCompareCounts 钉死摘要里的计数：直接实参几处、改写几处、经变量转手的几处。
//
// 变异自证（实测）：直接实参没变也计入改写数，红；经变量转手的不计数，红。
func TestBaselineCompareCounts(t *testing.T) {
	_, summary := compare(mustTree(t, baselineBefore()), mustTree(t, baselineAfter()))
	for _, want := range []string{"直接实参 2 处，其中改写 1 处", "由中文换成英文 1 处", "译表 2 对 → 3 对（多出键值相同的 1 对）"} {
		if !strings.Contains(summary, want) {
			t.Errorf("摘要里应当有 %q，得到：%s", want, summary)
		}
	}
}
