package config

// 本文件只测版本键在文本层的改写（setVersionKeyLine）与改写前后的三道把关（checkRewritable、
// 「恰好对应一行」、sameParse）：已有版本键时只换那一行的值，没有时插入，认不了的写法报错、不产出文本。
// 其余字节一律不变。offset 等整数键行的改写见 migratetext_test.go。

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func joinLines(lines [][]byte) string {
	var b strings.Builder
	for _, l := range lines {
		b.Write(l)
	}
	return b.String()
}

// setVersion 对文本 src 调 setVersionKeyLine（present 取自 src 的解析结果），返回改写后的文本。
func setVersion(t *testing.T, src, stamp string, notes ...string) (string, error) {
	t.Helper()
	_, present := parseYAML(t, src)[versionKey]
	lines, err := setVersionKeyLine(splitLines([]byte(src)), present, stamp, notes)
	return joinLines(lines), err
}

// 已有版本键：只换该行冒号之后、行尾注释之前的部分，写成带双引号的盖章值；缩进、冒号前后的空白、
// 行尾注释、行尾与其余各行逐字节不变。
//
// 变异自证（隔离的 worktree，实测）：写入的值不加引号 → 各例红；不保留行尾注释、不保留行尾（一律写 LF）、
// 不保留缩进 → 对应各例红；冒号后没有空白时不补空格 →「空值」两例红（改写后解析不出版本键）；
// 版本键已存在时按整数键行的规则去找它（值须是十进制整数）→ 除「手改成整数」外各例报错而红。
func TestSetVersionKeyLineReplaces(t *testing.T) {
	const stamp = "v4.0.1"
	cases := []struct{ name, src, want string }{
		{"带 v、双引号", "config-version: \"v4.0.0\"\noffset: -200\n", "config-version: \"v4.0.1\"\noffset: -200\n"},
		{"不带 v", "config-version: \"4.0.0\"\noffset: -200\n", "config-version: \"v4.0.1\"\noffset: -200\n"},
		{"单引号", "config-version: 'v4.0.0'\n", "config-version: \"v4.0.1\"\n"},
		{"不带引号", "config-version: v4.0.0\n", "config-version: \"v4.0.1\"\n"},
		{"行尾注释", "config-version: v4.0.0  # 勿改\naddr: x\n", "config-version: \"v4.0.1\"  # 勿改\naddr: x\n"},
		{"整体缩进", "  config-version: \"v4.0.0\"\n  offset: -200\n", "  config-version: \"v4.0.1\"\n  offset: -200\n"},
		{"冒号前后的空白", "config-version   :\t\"v4.0.0\"   \n", "config-version   :\t\"v4.0.1\"   \n"},
		{"CRLF", "config-version: \"v4.0.0\"\r\noffset: -200\r\n", "config-version: \"v4.0.1\"\r\noffset: -200\r\n"},
		{"行尾混用", "addr: x\r\nconfig-version: \"v4.0.0\"\noffset: -200\r\n", "addr: x\r\nconfig-version: \"v4.0.1\"\noffset: -200\r\n"},
		{"末行无换行", "offset: -200\nconfig-version: \"v4.0.0\"", "offset: -200\nconfig-version: \"v4.0.1\""},
		{"UTF-8 BOM", "\xEF\xBB\xBFconfig-version: \"v4.0.0\"\n", "\xEF\xBB\xBFconfig-version: \"v4.0.1\"\n"},
		{"不在首行", "# 头\n\naddr: x\n# 版本\nconfig-version: \"v4.0.0\"\n", "# 头\n\naddr: x\n# 版本\nconfig-version: \"v4.0.1\"\n"},
		{"手改成小数", "config-version: 4.0\noffset: 0x10\n", "config-version: \"v4.0.1\"\noffset: 0x10\n"},
		{"手改成整数", "config-version: 4\n", "config-version: \"v4.0.1\"\n"},
		{"空值", "config-version:\noffset: -200\n", "config-version: \"v4.0.1\"\noffset: -200\n"},
		{"空值带注释", "config-version: # 空着\n", "config-version: \"v4.0.1\" # 空着\n"},
		{"空串", "config-version: \"\"\n", "config-version: \"v4.0.1\"\n"},
		{"任意文字", "config-version: 不知道\n", "config-version: \"v4.0.1\"\n"},
		{"注释掉的版本键不算一行", "# config-version: \"v1\"\nconfig-version: \"v4.0.0\"\n", "# config-version: \"v1\"\nconfig-version: \"v4.0.1\"\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := setVersion(t, c.src, stamp)
			if err != nil {
				t.Fatalf("改写 %q 失败: %v", c.src, err)
			}
			if got != c.want {
				t.Errorf("改写结果\n得   %q\n期望 %q", got, c.want)
			}
			want := parseYAML(t, c.src)
			want[versionKey] = stamp
			if !sameParse([]byte(got), want) {
				t.Errorf("改写后的解析结果应只有版本键变成 %s：%q", stamp, got)
			}
		})
	}
}

// 版本键在解析结果里、文本里却找不到恰好一行认得的版本键行：报错，不产出文本。
//
// 变异自证（隔离的 worktree，实测）：找到多行时取第一行 →「嵌套映射里另有一行」「块标量里另有一行」
// 不报错而红；找不到时改为插入 → 其余各例不报错而红。
func TestSetVersionKeyLineRejectsUnrecognizedForms(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"键带双引号", "\"config-version\": \"v4.0.0\"\n"},
		{"键带单引号", "'config-version': v4.0.0\n"},
		{"流式映射", "{config-version: \"v4.0.0\", offset: -200}\n"},
		{"显式键", "? config-version\n: \"v4.0.0\"\n"},
		{"嵌套映射里另有一行", "config-version: \"v4.0.0\"\nextra:\n  config-version: \"v1\"\n"},
		{"块标量里另有一行", "config-version: \"v4.0.0\"\nnote: |\n  config-version: \"v1\"\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := parseYAML(t, c.src)[versionKey]; !ok {
				t.Fatalf("前置：%q 的解析结果里应有版本键", c.src)
			}
			got, err := setVersion(t, c.src, "v4.0.1")
			if err == nil {
				t.Errorf("应报错，实际改写为 %q", got)
			}
		})
	}
}

// 值写在下一行、值是块标量或带锚点时，版本键行本身形如「键 + 冒号 + 值」而被认作一行、照常替换；
// 由校验（sameParse）拦下：改写出的文本解析不了，或解析结果不是期望值。
func TestVersionKeyRewriteCaughtBySameParse(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"值另起一行", "config-version:\n  \"v4.0.0\"\n"},
		{"字面块标量", "config-version: |\n  v4.0.0\n"},
		{"折叠块标量", "config-version: >\n  v4.0.0\n"},
		{"带锚点", "config-version: &v \"v4.0.0\"\nother: *v\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := setVersion(t, c.src, "v4.0.1")
			if err != nil {
				return // 文本层自己就拒了，同样不会写文件
			}
			want := parseYAML(t, c.src)
			want[versionKey] = "v4.0.1"
			if sameParse([]byte(got), want) {
				t.Errorf("改写结果 %q 不应通过校验", got)
			}
		})
	}
}

// 没有版本键：在第一个键行及紧贴其上的注释之前插入注释、键行与一个空行；缩进取那个键行的缩进，行尾取
// 文件第一行的行尾；首行带 BOM 时 BOM 仍在文件开头；没有键行的文件接在末尾。其余字节不变。
//
// 变异自证（隔离的 worktree，实测）：插入的行尾一律写 LF → 两例 CRLF 红；不跟随缩进 →「整体缩进」红
// （改写后解析失败）；BOM 留在原行（落到文件中间）→ 两例 BOM 红；插在第一个键行之前、不越过它的注释 →
// 「键行上方贴着注释」等四例红；末行无换行时不补行尾 →「只有注释、末行无换行」红。
func TestSetVersionKeyLineInserts(t *testing.T) {
	const stamp, note = "v4.0.0", "版本说明"
	key := "# 版本说明\nconfig-version: \"v4.0.0\"\n"
	cases := []struct{ name, src, want string }{
		{"首行即键行", "offset: -200\npoll: 30\n", key + "\noffset: -200\npoll: 30\n"},
		{"键行上方贴着注释", "# 头\n\n# 偏移\noffset: -200\n", "# 头\n\n" + key + "\n# 偏移\noffset: -200\n"},
		{"文件头注释紧贴键行", "# 头一\n# 头二\noffset: -200\n", key + "\n# 头一\n# 头二\noffset: -200\n"},
		{"键行上方是空行", "# 头\n\noffset: -200\n", "# 头\n\n" + key + "\noffset: -200\n"},
		{"CRLF", "# 头\r\n\r\noffset: -200\r\n", "# 头\r\n\r\n# 版本说明\r\nconfig-version: \"v4.0.0\"\r\n\r\noffset: -200\r\n"},
		{"首行 CRLF、其余 LF", "offset: -200\r\npoll: 30\n", "# 版本说明\r\nconfig-version: \"v4.0.0\"\r\n\r\noffset: -200\r\npoll: 30\n"},
		{"末行无换行", "offset: -200", key + "\noffset: -200"},
		{"整体缩进", "  offset: -200\n  poll: 30\n", "  # 版本说明\n  config-version: \"v4.0.0\"\n\n  offset: -200\n  poll: 30\n"},
		{"UTF-8 BOM、首行即键行", "\xEF\xBB\xBFoffset: -200\n", "\xEF\xBB\xBF" + key + "\noffset: -200\n"},
		{"UTF-8 BOM、首行是注释", "\xEF\xBB\xBF# 头\n\noffset: -200\n", "\xEF\xBB\xBF# 头\n\n" + key + "\noffset: -200\n"},
		{"UTF-8 BOM、注释紧贴键行", "\xEF\xBB\xBF# 头\noffset: -200\n", "\xEF\xBB\xBF" + key + "\n# 头\noffset: -200\n"},
		{"空文件", "", key},
		{"只有注释", "# 头\n# 尾\n", "# 头\n# 尾\n" + key},
		{"只有注释、末行无换行", "# 头", "# 头\n" + key},
		{"只有空行", "\n\n", "\n\n" + key},
		{"注释掉的版本键不算已有", "# config-version: \"v1\"\noffset: -200\n", key + "\n# config-version: \"v1\"\noffset: -200\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := setVersion(t, c.src, stamp, note)
			if err != nil {
				t.Fatalf("插入失败: %v", err)
			}
			if got != c.want {
				t.Errorf("插入结果\n得   %q\n期望 %q", got, c.want)
			}
			want := parseYAML(t, c.src)
			if want == nil {
				want = map[string]interface{}{}
			}
			want[versionKey] = stamp
			if !sameParse([]byte(got), want) {
				t.Errorf("插入后的解析结果应只多出版本键：%q", got)
			}
		})
	}

	t.Run("多行注释与不带注释", func(t *testing.T) {
		got, _ := setVersion(t, "offset: -200\n", stamp, "一", "二")
		if want := "# 一\n# 二\nconfig-version: \"v4.0.0\"\n\noffset: -200\n"; got != want {
			t.Errorf("两行注释\n得   %q\n期望 %q", got, want)
		}
		got, _ = setVersion(t, "offset: -200\n", stamp)
		if want := "config-version: \"v4.0.0\"\n\noffset: -200\n"; got != want {
			t.Errorf("不带注释\n得   %q\n期望 %q", got, want)
		}
	})
}

// 首行是文档起始标记 `---` 时，版本键被插在它之前，文件成了两个文档：由校验拦下（解析结果只剩版本键）。
func TestVersionKeyInsertBeforeDocumentMarkerCaughtBySameParse(t *testing.T) {
	const src = "---\noffset: -200\n"
	got, err := setVersion(t, src, "v4.0.0", "版本说明")
	if err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	if sameParse([]byte(got), map[string]interface{}{"offset": -200, versionKey: "v4.0.0"}) {
		t.Errorf("插在 --- 之前的结果 %q 不应通过校验", got)
	}
}

// 改写前的两条显式检查：不是 UTF-8、不止一个文档，都不得改写。空文件与只有注释的文件没有文档，可以改写。
//
// 变异自证（隔离的 worktree，实测）：去掉 UTF-8 检查 → 两例 UTF-16 红（「非法字节」一例不红：yaml 解码
// 自己报错，同样判为不可改写）；去掉文档数量的检查 → 三例多文档红。
func TestCheckRewritable(t *testing.T) {
	utf16le := func(s string) string {
		var b []byte
		for _, u := range utf16.Encode(append([]rune{0xFEFF}, []rune(s)...)) {
			b = append(b, byte(u), byte(u>>8))
		}
		return string(b)
	}
	utf16be := func(s string) string {
		var b []byte
		for _, u := range utf16.Encode(append([]rune{0xFEFF}, []rune(s)...)) {
			b = append(b, byte(u>>8), byte(u))
		}
		return string(b)
	}
	for _, c := range []struct {
		name string
		src  string
		ok   bool
	}{
		{"普通文件", "offset: 200\npoll: 30\n", true},
		{"UTF-8 BOM", "\xEF\xBB\xBFoffset: 200\n", true},
		{"带中文注释", "# 歌词延迟\noffset: 200\n", true},
		{"空文件", "", true},
		{"只有注释", "# 头\n", true},
		{"首行是 ---", "---\noffset: 200\n", true},
		{"末尾是 ...", "offset: 200\n...\n", true},
		{"UTF-16LE 带 BOM", utf16le("offset: 200\r\n"), false},
		{"UTF-16BE 带 BOM", utf16be("offset: 200\r\n"), false},
		{"非法字节", "offset: 200 # \xff\xfe\n", false},
		{"两个文档", "offset: 200\n---\nconfig-version: \"v4.0.0\"\noffset: -500\n", false},
		{"两个文档，第二个为空映射", "offset: 200\n---\n{}\n", false},
		{"三个文档", "a: 1\n---\nb: 2\n---\nc: 3\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := checkRewritable([]byte(c.src)); (err == nil) != c.ok {
				t.Errorf("checkRewritable = %v，期望可改写 = %v", err, c.ok)
			}
		})
	}
}
