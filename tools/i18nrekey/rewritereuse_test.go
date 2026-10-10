package main

// 本文件只测一件事：译表已是新方向之后，按旧约定新写的调用点复用了表里已有的中文（没有随之新增
// 「中文 → 英文」条目），工具照样能把它改成那句英文；一句中文对应不止一句英文时报出而不猜。

import (
	"strings"
	"testing"
)

// reuseTree 是一棵已改造完的树，外加一处按旧约定新写的调用：i18n.T("是")，表里已有 "yes": "是"。
func reuseTree(table, site string) map[string]string {
	return map[string]string{
		"i18n/messages.go": "package i18n\n\nvar chinese = map[string]string{\n" + table + "}\n",
		"p/p.go": `package p

import "Metabox-Nexus-PlayerCap/i18n"

func f() string {
	log.Info("log A %s", 1)
	return ` + site + `
}
`,
	}
}

// TestRewriteReusesExistingTranslation 钉死：中文字面量在表里只有已对调的条目可查时，照常改写，
// 改写后再跑一遍没有待办。
//
// 变异自证（隔离副本，实测）：
//   - 中文 → 英文的索引只收旧方向条目（修复前的写法）：红，i18n.T("是") 留在原处并报「没有英文」。
//   - failed 不算待改写的调用点：红，核对在还有待改写时报通过。
//   - 索引把键值相同的条目也当成中文收进去：仍绿（那类条目不含汉字，查不到）。
func TestRewriteReusesExistingTranslation(t *testing.T) {
	const table = `	"log A %s": "日志甲 %s",
	"yes":      "是",
	"no":       "否",
`
	files := reuseTree(table, `i18n.T("是")`)
	before := mustTree(t, files)
	a := analyze(before)
	if len(a.rewrites) != 1 || len(a.noEnglish) != 0 || len(a.swaps) != 0 {
		t.Fatalf("待改写 %d 处、没有英文 %d 处、待对调 %d 条，预期 1、0、0", len(a.rewrites), len(a.noEnglish), len(a.swaps))
	}
	if !a.failed() {
		t.Error("还有待改写的调用点，核对不该通过")
	}
	out, err := rewrite(before, a)
	if err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	if len(out) != 1 || !strings.Contains(string(out["p/p.go"]), `return i18n.T("yes")`) {
		t.Fatalf("应当只改 p/p.go，把 i18n.T(\"是\") 换成 i18n.T(\"yes\")，得到 %d 个文件：\n%s", len(out), out["p/p.go"])
	}

	files["p/p.go"] = string(out["p/p.go"])
	b := analyze(mustTree(t, files))
	if b.failed() || len(b.rewrites) != 0 {
		t.Errorf("改写后再核对应当通过且没有待改写项，得到待改写 %d 处", len(b.rewrites))
	}
}

// TestRewriteRefusesAmbiguousChinese 钉死：同一句中文在表里对应不止一句英文时，这处调用不改、
// 列出候选、核对不通过；别的调用点照常改写。反例：两句英文对应同一句中文而没有调用点用到这句
// 中文，不算问题。
//
// 变异自证（隔离副本，实测）：
//   - 有歧义时取第一句英文照改：红，歧义不再报出。
//   - failed 不算有歧义的字面量：红。
//   - 候选不去重（同一中英对新旧两个方向各有一条也算两句）：「同一对出现两次」例红。
//   - 索引只收旧方向条目（修复前的写法）：前两例与最后一例红。
func TestRewriteRefusesAmbiguousChinese(t *testing.T) {
	const table = `	"log A %s":      "日志甲 %s",
	"Lyrics loaded": "歌词加载完成",
	"lyrics loaded": "歌词加载完成",
	"yes":           "是",
`
	t.Run("调用点用到有歧义的中文", func(t *testing.T) {
		files := reuseTree(table, `i18n.T("歌词加载完成") + i18n.T("是")`)
		tr := mustTree(t, files)
		a := analyze(tr)
		if len(a.ambiguous) != 1 || len(a.ambiguous[0].english) != 2 {
			t.Fatalf("应当报出一处有两句候选英文的字面量，得到 %+v", a.ambiguous)
		}
		if !a.failed() {
			t.Error("有歧义的字面量未处理，核对不该通过")
		}
		out, err := rewrite(tr, a)
		if err != nil {
			t.Fatalf("改写失败: %v", err)
		}
		got := string(out["p/p.go"])
		if !strings.Contains(got, `i18n.T("歌词加载完成") + i18n.T("yes")`) {
			t.Errorf("有歧义的那处应当原样留下，另一处照常改写，得到：\n%s", got)
		}
	})
	t.Run("旧方向条目与已对调条目给出不同的英文", func(t *testing.T) {
		files := reuseTree(`	"log A %s":      "日志甲 %s",
	"lyrics loaded": "歌词加载完成",
	"歌词加载完成":        "Lyrics loaded",
`, `i18n.T("歌词加载完成")`)
		tr := mustTree(t, files)
		a := analyze(tr)
		if len(a.ambiguous) != 1 || len(a.ambiguous[0].english) != 2 {
			t.Fatalf("应当报出一处有两句候选英文的字面量，得到 %+v", a.ambiguous)
		}
		out, err := rewrite(tr, a)
		if err != nil {
			t.Fatalf("改写失败: %v", err)
		}
		if _, ok := out["p/p.go"]; ok {
			t.Errorf("有歧义的调用点不该被改写，得到：\n%s", out["p/p.go"])
		}
	})
	t.Run("没有调用点用到，不算问题", func(t *testing.T) {
		a := analyze(mustTree(t, reuseTree(table, `i18n.T("yes")`)))
		if len(a.ambiguous) != 0 || a.failed() {
			t.Errorf("两句英文共用一句中文本身不是问题，得到 %+v", a.ambiguous)
		}
	})
	t.Run("同一对出现两次，不算歧义", func(t *testing.T) {
		// 新方向的一条在 chinese 里，另一张表里既有新方向的同一条、又有旧方向的同一对。
		files := reuseTree(`	"log A %s": "日志甲 %s",
	"yes":      "是",
`, `i18n.T("是")`)
		files["i18n/more.go"] = "package i18n\n\nvar more = map[string]string{\"yes\": \"是\"}\n"
		files["i18n/old.go"] = "package i18n\n\nvar old = map[string]string{\"是\": \"yes\"}\n"
		a := analyze(mustTree(t, files))
		if len(a.ambiguous) != 0 || len(a.rewrites) != 1 {
			t.Errorf("同一中英对重复出现不是歧义，应当照常待改写，得到歧义 %d 处、待改写 %d 处", len(a.ambiguous), len(a.rewrites))
		}
	})
	t.Run("只有歧义这一项，核对也不通过", func(t *testing.T) {
		a := analyze(mustTree(t, reuseTree(table, `i18n.T("歌词加载完成")`)))
		if len(a.rewrites)+len(a.swaps)+len(a.noEnglish)+len(a.missing) != 0 || !a.failed() {
			t.Errorf("别的项都应为空而核对不通过，得到待改写 %d、待对调 %d，failed=%v", len(a.rewrites), len(a.swaps), a.failed())
		}
	})
}
