package main

// 本文件只测一件事：-write 的改写结果——调用点换成英文、旧方向条目对调、其余不动，
// 且对改写后的结果再跑一遍什么都不改。

import (
	"go/format"
	"maps"
	"strings"
	"testing"
)

// mustTree 由一组内存里的源文件建树。
func mustTree(t *testing.T, files map[string]string) *tree {
	t.Helper()
	src := map[string][]byte{}
	for name, content := range files {
		src[name] = []byte(content)
	}
	tr, err := buildTree(src)
	if err != nil {
		t.Fatalf("建树失败: %v", err)
	}
	return tr
}

// mixedTree 是一棵改到一半的源码树：译表里四条旧方向、一条新方向；调用点里有待改写的、
// 已是英文的、不经目标调用的。
func mixedTree() map[string]string {
	return map[string]string{
		"i18n/messages.go": `package i18n

// english 是译表。
var english = map[string]string{
	// 分节注释
	"日志甲 %s":   "log A %s", // 行尾注释
	"日志乙：分两段": "log B: two parts",
	"文案丁":     "text D\n",
	"already English %s": "已是英文 %s",
}

var more = map[string]string{
	"错误己: %w": "error F: %w",
}
`,
		"p/p.go": `package p

import (
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
)

var log = logger.New("X")

func f(name string, err error) error {
	log.Info("日志甲 %s", name) // 注释留在原处
	log.Warn("日志乙：" +
		"分两段")
	log.Detail("already English %s", name)
	println(i18n.T("文案丁"))
	println("日志甲 %s")
	return i18n.Errorf("错误己: %w", err)
}
`,
		"p/p_test.go": `package p

import (
	"testing"

	"Metabox-Nexus-PlayerCap/i18n"
)

func TestF(t *testing.T) {
	if i18n.T("文案丁") == "" {
		t.Error("文案丁")
	}
}
`,
		"q/untouched.go": `package q

func g() { println("与 i18n 无关") }
`,
	}
}

const wantCallSites = `package p

import (
	"Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
)

var log = logger.New("X")

func f(name string, err error) error {
	log.Info("log A %s", name) // 注释留在原处
	log.Warn("log B: two parts")
	log.Detail("already English %s", name)
	println(i18n.T("text D\n"))
	println("日志甲 %s")
	return i18n.Errorf("error F: %w", err)
}
`

const wantTestFile = `package p

import (
	"testing"

	"Metabox-Nexus-PlayerCap/i18n"
)

func TestF(t *testing.T) {
	if i18n.T("text D\n") == "" {
		t.Error("文案丁")
	}
}
`

// TestRewriteRoundTrip 钉死改写的结果，并确认再跑一遍不再改动。
//
// 变异自证（实测）：
//   - plan 不对调条目：红，改写后仍有旧方向条目。
//   - plan 把调用点换成原字面量而不是英文：红，调用点与预期不符。
//   - plan 把对调写成单向覆盖（键换成值，值不动）：红，译表与预期不符。
//   - applyEdits 按偏移从小到大落替换：红，靠后的替换落在错位的区间上，切片越界。
//   - scanFile 在测试文件里也认 logger 的方法名：红，t.Error("文案丁") 被改写。
func TestRewriteRoundTrip(t *testing.T) {
	files := mixedTree()
	before := mustTree(t, files)
	a := analyze(before)
	if len(a.rewrites) != 5 || len(a.swaps) != 4 {
		t.Fatalf("待改写 %d 处、待对调 %d 条，预期 5 处、4 条", len(a.rewrites), len(a.swaps))
	}
	out, err := rewrite(before, a)
	if err != nil {
		t.Fatalf("改写失败: %v", err)
	}

	if got := string(out["p/p.go"]); got != wantCallSites {
		t.Errorf("调用点改写结果与预期不符：\n%s", got)
	}
	if got := string(out["p/p_test.go"]); got != wantTestFile {
		t.Errorf("测试文件改写结果与预期不符：\n%s", got)
	}
	if _, ok := out["q/untouched.go"]; ok || len(out) != 3 {
		t.Errorf("改动的文件应当只有译表、p.go、p_test.go 三个，得到 %d 个", len(out))
	}

	// 译表：条目全部成为新方向，注释留在原处，内容是 gofmt 过的。
	table := out["i18n/messages.go"]
	for _, c := range []string{"// 分节注释", "// 行尾注释", "// english 是译表。"} {
		if !strings.Contains(string(table), c) {
			t.Errorf("译表里的注释 %q 丢了", c)
		}
	}
	if formatted, err := format.Source(table); err != nil || string(formatted) != string(table) {
		t.Errorf("改写后的译表不是 gofmt 过的（err=%v）", err)
	}

	for name, content := range out {
		files[name] = string(content)
	}
	after := mustTree(t, files)
	b := analyze(after)
	wantTable := map[string]string{
		"log A %s":           "日志甲 %s",
		"log B: two parts":   "日志乙：分两段",
		"text D\n":           "文案丁",
		"already English %s": "已是英文 %s",
		"error F: %w":        "错误己: %w",
	}
	if len(b.zh2en) != 0 || !maps.Equal(b.native, wantTable) {
		t.Errorf("改写后的译表与预期不符：旧方向 %v，新方向 %q", b.zh2en, b.native)
	}
	if b.failed() {
		t.Errorf("改写后的核对应当通过：待改写 %d、待对调 %d、缺译 %d", len(b.rewrites), len(b.swaps), len(b.missing))
	}

	// 再跑一遍：不改任何文件。
	again, err := rewrite(after, b)
	if err != nil || len(again) != 0 {
		t.Errorf("对改写后的结果再改写，应当不动任何文件，得到 %d 个（err=%v）", len(again), err)
	}

	// 与改写前比对：无差异。
	if diffs, summary := compare(before, after); len(diffs) != 0 {
		t.Errorf("改写前后比对应当无差异（%s）：\n  %s", summary, strings.Join(diffs, "\n  "))
	}
}

// TestRewriteRefusesCollisions 钉死撞键时拒绝改写。
//
// 变异自证（实测）：rewrite 不检查撞键，前两例红——改写照做，后写的那句中文悄悄丢掉；
// analyze 归并中文时不去重，第三例红——同一对出现两次被报成撞键。
func TestRewriteRefusesCollisions(t *testing.T) {
	const site = `package p

func f() { log.Info("甲"); log.Info("乙") }
`
	cases := []struct {
		name    string
		table   string
		collide bool
	}{
		{"两条旧方向条目英文相同", `"甲": "same", "乙": "same",`, true},
		{"旧方向条目的英文与已有的新方向条目撞", `"same": "甲", "乙": "same",`, true},
		// 不算撞键；改写时去掉旧方向的那一条，结果由 rewritedupkeys_test.go 核对。
		{"同一对新旧方向各出现一次", `"same": "甲", "甲": "same", "乙": "other",`, false},
		{"英文各不相同", `"甲": "one", "乙": "two",`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := mustTree(t, map[string]string{
				"i18n/messages.go": "package i18n\n\nvar english = map[string]string{" + c.table + "}\n",
				"p/p.go":           site,
			})
			a := analyze(tr)
			_, err := rewrite(tr, a)
			if c.collide {
				if len(a.collisions) != 1 || len(a.collisions[0].chinese) != 2 {
					t.Errorf("应当报出一组两句中文的撞键，得到 %+v", a.collisions)
				}
				if err == nil {
					t.Error("撞键时应当拒绝改写")
				}
			} else if len(a.collisions) != 0 || err != nil {
				t.Errorf("不该报撞键：%+v（err=%v）", a.collisions, err)
			}
		})
	}
}
