package main

// 本文件只测一件事：核对（不带 -write）把哪些情形判为未通过、哪些只列出供人看。

import "testing"

// TestExempt 钉死豁免规则：去掉占位符后不含字母与数字的才可以不进译表。
//
// 变异自证（实测）：exempt 不先去掉占位符，红——"%s" 的动词字母被当成文案；
// 只查字母不查数字，红——"1/%d" 被放行；恒为真，红；恒为假，红。
func TestExempt(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"%s", true},
		{"[%d/%d] %s", true},
		{"  - %-8s | %5.1f%%\n", true},
		{"%[2]s → %[1]v", true},
		{"♪ %s", true},
		{"", true},

		{"%ds", false},              // %d 之后的 s 是文案（秒）
		{"[%2d] %6.1fs  %s", false}, // 同上
		{"vtable: 0x%08X", false},
		{"1/%d", false}, // 数字也算内容
		{"%s ok", false},
		{"歌曲: %s", false},
		{"%z", false}, // 不是合法占位符，z 是字母
	}
	for _, c := range cases {
		if got := exempt(c.s); got != c.want {
			t.Errorf("exempt(%q) = %v，预期 %v", c.s, got, c.want)
		}
	}
}

// TestCheckFindings 逐项钉死核对的判定：每例只放一种问题，断言它落在该落的那一项、别的项为空。
//
// 变异自证（实测）：
//   - analyze 不收「含汉字却没有英文」：第 2 例红。
//   - 不收「不在译表里」：第 3 例红；把豁免去掉：第 4 例红。
//   - 不查英文里的汉字：第 5 例红；不查同键异值：第 6 例红。
//   - 测试文件的字面量也按出货代码判：第 7、8 例红。
//   - failed 的八项（待改写、待对调、待去掉、同键异值、表内重复键、一句中文对应多句英文、含汉字却
//     没有英文、不在译表里）漏算任何一项：只有那一项非空的那一例红。
func TestCheckFindings(t *testing.T) {
	const (
		cleanTable = `"log A %s": "日志甲 %s",`
		cleanSite  = `log.Info("log A %s", 1)`
	)
	cases := []struct {
		name     string
		table    string // 第一张表的条目
		second   string // 第二张表的条目
		site     string // 出货代码里的一条语句
		testSite string // 测试文件里的一条语句
		want     string // 预期非空的那一项；空串表示通过
	}{
		{name: "全部到位", table: cleanTable, site: cleanSite},
		{name: "含汉字却没有英文", table: cleanTable, site: `log.Info("没有译文 %d", 1)`, want: "noEnglish"},
		{name: "不在译表里", table: cleanTable, site: `log.Info("no translation %d", 1)`, want: "missing"},
		{name: "只有占位符与标点，豁免", table: cleanTable, site: `log.Info("[%d/%d] %s", 1, 2, "x")`},
		{name: "英文里含汉字", table: `"日志甲": "log 甲",`, site: `log.Info("%s", "x")`, want: "hanEnglish"},
		{name: "同一个键两张表里值不同", table: cleanTable, second: `"log A %s": "日志乙 %s",`, site: cleanSite, want: "conflicts"},
		{name: "测试里含汉字却没有英文，只列出", table: cleanTable, site: cleanSite, testSite: `_ = i18n.T("测试专用")`},
		{name: "测试里不在译表里的英文，不管", table: cleanTable, site: cleanSite, testSite: `_ = i18n.T("test only")`},
		{name: "待改写", table: `"日志甲 %s": "log A %s",`, site: `log.Info("日志甲 %s", 1)`, want: "rewrites"},
		{name: "待改写，复用已对调的条目（没有待对调的）", table: cleanTable, site: `log.Info("日志甲 %s", 1)`, want: "rewrites"},
		{name: "待对调（调用点已是英文）", table: `"日志甲 %s": "log A %s",`, site: cleanSite, want: "swaps"},
		{name: "待去掉（同一对两个方向各一条）", table: `"log A %s": "日志甲 %s", "日志甲 %s": "log A %s",`, site: cleanSite, want: "drops"},
		{name: "一句中文对应两句英文", table: `"log A %s": "日志甲 %s", "log a %s": "日志甲 %s",`, site: `log.Info("日志甲 %s", 1)`, want: "ambiguous"},
		{name: "一张表里重复的键", table: `"log A %s": "日志甲 %s", "log A %s": "日志甲 %s",`, site: cleanSite, want: "dupKeys"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{
				"i18n/messages.go": "package i18n\n\nvar chinese = map[string]string{" + c.table + "}\n",
				"p/p.go":           "package p\n\nfunc f() { " + c.site + " }\n",
			}
			if c.second != "" {
				files["i18n/more.go"] = "package i18n\n\nvar more = map[string]string{" + c.second + "}\n"
			}
			if c.testSite != "" {
				files["p/p_test.go"] = "package p\n\nimport \"Metabox-Nexus-PlayerCap/i18n\"\n\nfunc g() { " + c.testSite + " }\n"
			}
			a := analyze(mustTree(t, files))
			got := map[string]int{
				"rewrites": len(a.rewrites), "swaps": len(a.swaps), "drops": len(a.drops), "collisions": len(a.collisions),
				"conflicts": len(a.conflicts), "dupKeys": len(a.dupKeys), "hanEnglish": len(a.hanEnglish),
				"ambiguous": len(a.ambiguous), "noEnglish": len(a.noEnglish), "missing": len(a.missing),
			}
			for item, n := range got {
				// 条目是旧方向的那几例必然同时待对调。
				if item == "swaps" && (c.name == "待改写" || c.want == "hanEnglish") {
					continue
				}
				if (item == c.want) != (n > 0) {
					t.Errorf("%s = %d，预期非空的只有 %q", item, n, c.want)
				}
			}
			if a.failed() != (c.want != "") {
				t.Errorf("failed() = %v，预期 %v", a.failed(), c.want != "")
			}
		})
	}
}

// TestCheckListsForReview 钉死供人核对的几项列得出来，且不影响是否通过。
//
// 变异自证（实测）：analyze 不收 indirect、exempted、unreferenced 或 noEnglishTests 任一项，对应的断言红。
func TestCheckListsForReview(t *testing.T) {
	a := analyze(mustTree(t, map[string]string{
		"i18n/messages.go": `package i18n

var chinese = map[string]string{
	"log A %s":     "日志甲 %s",
	"via variable": "经变量传入",
	"nobody":       "没人引用",
}
`,
		"p/p.go": `package p

import "Metabox-Nexus-PlayerCap/i18n"

var note = "经变量传入"

func f() {
	log.Info("log A %s", 1)
	log.Info("%s", i18n.T(note))
}
`,
		"p/p_test.go": `package p

import "Metabox-Nexus-PlayerCap/i18n"

func g() { _ = i18n.T("测试专用") }
`,
	}))
	if a.failed() {
		t.Fatal("这棵树应当通过核对")
	}
	if len(a.indirect) != 1 || a.indirect[0].val != "经变量传入" {
		t.Errorf("与译表中文相同的非直接实参应当列出一处，得到 %+v", a.indirect)
	}
	if len(a.exempted) != 1 || a.exempted[0].val != "%s" {
		t.Errorf("按豁免放行的应当列出一处，得到 %+v", a.exempted)
	}
	if len(a.unreferenced) != 2 {
		t.Errorf("没有直接实参引用的条目应当是两条，得到 %+v", a.unreferenced)
	}
	if len(a.noEnglishTests) != 1 {
		t.Errorf("测试里含汉字却没有英文的应当列出一处，得到 %+v", a.noEnglishTests)
	}
}
