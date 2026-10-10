package main

// 本文件只测一件事：改写后每张表里的键唯一，源码仍能通过类型检查。
// 同一中英对新旧两个方向各留了一条（合并冲突两边都留就是这种输入）时，对调会写出两个相同的键；
// gofmt 不查重复键，改写照样成功，而源码已编译不过。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"strings"
	"testing"
)

// typeCheckCatalog 断言一组只含译表的文件能通过类型检查；map 字面量里重复的键在这一步报错。
func typeCheckCatalog(t *testing.T, files map[string]string) {
	t.Helper()
	fset := token.NewFileSet()
	var parsed []*ast.File
	for name, src := range files {
		if !strings.HasPrefix(name, "i18n/") {
			continue
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("%s 解析失败: %v\n%s", name, err, src)
		}
		parsed = append(parsed, f)
	}
	if _, err := (&types.Config{}).Check("i18n", fset, parsed, nil); err != nil {
		t.Errorf("改写后的译表通不过类型检查: %v", err)
	}
}

// TestRewriteKeepsKeysUnique 钉死：同一中英对重复时去掉旧方向的那一条，不对调出重复的键。
// 选去重而不是拒绝改写：两边表达的是同一对译文，去掉哪一条都不丢信息，合并后的收尾不该卡在这里。
//
// 变异自证（隔离副本，实测）：
//   - 旧方向条目一律对调、不去重（修复前的写法）：前四例红，改写被表内重复键的检查拦下
//     （第五例两张表各一条，对调后不在一张表里，仍绿）。
//   - 同时不查表内重复键：前四例红，类型检查报 duplicate key。
//   - 新方向的条目不算已有的一对（只在旧方向条目之间去重）：新旧各一条的三例红。
//   - 去掉条目时不连同它后面的逗号：前五例红，改写后的内容无法格式化。
//   - 去掉条目时不判断它是否独占一行、一律按整行处理：单行书写的两例红。
//   - 整行去掉时不带走行尾注释：「分行书写」例红。
//   - 找逗号时不跳过值后面的块注释（只认空白）：两个「块注释」例红，前一例改写后的内容无法格式化。
func TestRewriteKeepsKeysUnique(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string // 只写译表文件
		want  map[string]string // 改写后全部条目：英文 → 中文
		keep  []string          // 改写后应当还在的片段
		gone  []string          // 改写后应当不在的片段
	}{
		{
			name:  "同一对，新方向在前",
			files: map[string]string{"i18n/messages.go": `"same": "甲", "甲": "same", "乙": "other",`},
			want:  map[string]string{"same": "甲", "other": "乙"},
		},
		{
			name:  "同一对，旧方向在前",
			files: map[string]string{"i18n/messages.go": `"甲": "same", "same": "甲", "乙": "other"`},
			want:  map[string]string{"same": "甲", "other": "乙"},
		},
		{
			name:  "旧方向的同一对在一张表里写了两遍",
			files: map[string]string{"i18n/messages.go": "\n\t\"甲\": \"same\",\n\t\"甲\": \"same\",\n"},
			want:  map[string]string{"same": "甲"},
		},
		{
			name: "分行书写，带注释",
			files: map[string]string{"i18n/messages.go": `
	// 分节注释
	"same": "甲", // 新方向的留下
	"甲":    "same", // 旧方向的整行去掉
	"乙":    "other",
`},
			want: map[string]string{"same": "甲", "other": "乙"},
			keep: []string{"// 分节注释", "// 新方向的留下"},
			gone: []string{"旧方向的整行去掉"},
		},
		{
			name:  "值与逗号之间夹着块注释",
			files: map[string]string{"i18n/messages.go": `"same": "甲", "甲": "same" /* 旧方向的 */, "乙": "other",`},
			want:  map[string]string{"same": "甲", "other": "乙"},
			gone:  []string{"旧方向的"},
		},
		{
			name:  "末条不带逗号，值后跟着块注释",
			files: map[string]string{"i18n/messages.go": `"same": "甲", "甲": "same" /* 旧方向的 */`},
			want:  map[string]string{"same": "甲"},
			gone:  []string{"旧方向的"},
		},
		{
			name: "新方向的那条在另一张表里",
			files: map[string]string{
				"i18n/messages.go": `"甲": "same", "乙": "other",`,
				"i18n/more.go":     `"same": "甲",`,
			},
			want: map[string]string{"same": "甲", "other": "乙"},
		},
		{
			name:  "反例：没有重复，照常对调",
			files: map[string]string{"i18n/messages.go": `"甲": "one", "two": "乙",`},
			want:  map[string]string{"one": "甲", "two": "乙"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{"p/p.go": "package p\n\nfunc f() { log.Info(\"%s\", 1) }\n"}
			for name, body := range c.files {
				v := strings.TrimSuffix(strings.TrimPrefix(name, "i18n/"), ".go")
				files[name] = "package i18n\n\nvar " + v + " = map[string]string{" + body + "}\n"
			}
			tr := mustTree(t, files)
			a := analyze(tr)
			if !a.failed() {
				t.Error("改写之前核对不该通过")
			}
			out, err := rewrite(tr, a)
			if err != nil {
				t.Fatalf("改写失败: %v", err)
			}
			for name, content := range out {
				files[name] = string(content)
			}
			typeCheckCatalog(t, files)

			b := analyze(mustTree(t, files))
			if b.failed() || !maps.Equal(b.native, c.want) || len(b.zh2en) != 0 {
				t.Errorf("改写后的译表与预期不符：新方向 %q，旧方向 %q，核对未通过=%v", b.native, b.zh2en, b.failed())
			}
			all := files["i18n/messages.go"]
			for _, s := range c.keep {
				if !strings.Contains(all, s) {
					t.Errorf("%q 应当还在：\n%s", s, all)
				}
			}
			for _, s := range c.gone {
				if strings.Contains(all, s) {
					t.Errorf("%q 应当已去掉：\n%s", s, all)
				}
			}
		})
	}
}

// TestRewriteRefusesDuplicateKeys 钉死改写不了的重复：键相同而值不同（同键异值），以及新方向的
// 同一条在一张表里写了两遍（没有旧方向的可去）。两者都拒绝改写、核对不通过。
//
// 变异自证（隔离副本，实测）：
//   - rewrite 不看同键异值：「旧方向的同键异值」例红，改写照做（新方向的那例另有表内重复键，照样被拒）。
//   - 不做每张表的重复键检查，或 rewrite 不看它：「新方向的同一条写了两遍」例红。
//   - 重复键不分表、跨表也算：checkfindings_test.go 的「同一个键两张表里值不同」例红。
func TestRewriteRefusesDuplicateKeys(t *testing.T) {
	cases := []struct {
		name, table string
		conflicts   int // 同键异值
		dupKeys     int // 一张表里重复的键
	}{
		{"新方向的同键异值", `"same": "甲", "same": "乙",`, 1, 1},
		{"旧方向的同键异值", `"甲": "one", "甲": "two",`, 1, 0},
		{"新方向的同一条写了两遍", `"same": "甲", "same": "甲",`, 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := mustTree(t, map[string]string{
				"i18n/messages.go": "package i18n\n\nvar chinese = map[string]string{" + c.table + "}\n",
				"p/p.go":           "package p\n\nfunc f() { log.Info(\"%s\", 1) }\n",
			})
			a := analyze(tr)
			if len(a.conflicts) != c.conflicts || len(a.dupKeys) != c.dupKeys {
				t.Errorf("同键异值 %d 处、表内重复键 %d 处，预期 %d、%d", len(a.conflicts), len(a.dupKeys), c.conflicts, c.dupKeys)
			}
			if !a.failed() {
				t.Error("核对不该通过")
			}
			if out, err := rewrite(tr, a); err == nil {
				t.Errorf("应当拒绝改写，得到 %d 个文件", len(out))
			}
		})
	}
}
