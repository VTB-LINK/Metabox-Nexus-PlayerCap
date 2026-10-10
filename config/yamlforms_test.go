package config

// 本文件只坐实一件事：yaml.v3 对 config.yml 里几种写法的现行解析结果。
//
// 它不测本包的逻辑。doc/design/config-version-and-offset-delay-design.md 的迁移规则（哪些写法的 offset
// 会被取反、哪些文件不改写）以这些结果为前提：mergeYAML 只采纳解析为 int 的 offset，其余类型被拒；
// 重复的键使整份文件解析失败。升级 yaml 依赖后若本测试变红，先回到该设计文档核对迁移规则。

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"
	"unicode/utf16"

	"gopkg.in/yaml.v3"
)

// 多文档：yaml.Unmarshal 只解析第一个文档、不报错，其后的文档它看不见；要知道文件里有几个文档，
// 得用 yaml.Decoder 再 Decode 一次，看是不是 io.EOF。逐行改写会动到后面的文档，所以改写前须显式检查。
func TestYAMLMultiDocumentOnlyFirstIsParsed(t *testing.T) {
	const src = "offset: 200\n---\nconfig-version: \"v4.0.0\"\noffset: -500\n"

	var m map[string]interface{}
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("解析多文档失败: %v", err)
	}
	if len(m) != 1 || m["offset"] != 200 {
		t.Errorf("Unmarshal 应只得到第一个文档 {offset: 200}，实际 %v", m)
	}

	dec := yaml.NewDecoder(bytes.NewReader([]byte(src)))
	var first, second map[string]interface{}
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("Decode 第一个文档失败: %v", err)
	}
	if err := dec.Decode(&second); err != nil {
		t.Fatalf("多文档的第二次 Decode 应成功，实际 %v", err)
	}
	if second["offset"] != -500 {
		t.Errorf("第二个文档 = %v，期望含 offset: -500", second)
	}

	dec = yaml.NewDecoder(bytes.NewReader([]byte("offset: 200\n")))
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("Decode 单文档失败: %v", err)
	}
	if err := dec.Decode(&second); !errors.Is(err, io.EOF) {
		t.Errorf("单文档的第二次 Decode 应返回 io.EOF，实际 %v", err)
	}
}

// 整数的两端：最小值解析为 int，取反会溢出回它自己；超过 int64 上限的十进制数不再是 int。
func TestYAMLIntegerExtremes(t *testing.T) {
	var m map[string]interface{}
	if err := yaml.Unmarshal([]byte("offset: -9223372036854775808\n"), &m); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	v, ok := m["offset"].(int)
	if !ok || v != math.MinInt64 {
		t.Fatalf("offset = %T(%v)，期望 int 的最小值", m["offset"], m["offset"])
	}
	if -v != v {
		t.Errorf("int 最小值取反应溢出回自身，实际 %d", -v)
	}

	m = nil
	if err := yaml.Unmarshal([]byte("offset: 9223372036854775808\n"), &m); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if _, isInt := m["offset"].(int); isInt {
		t.Errorf("超过 int64 上限的值不应解析为 int，实际 %T(%v)", m["offset"], m["offset"])
	}
	t.Logf("9223372036854775808 解析为 %T", m["offset"])
}

func TestYAMLFormsOfConfigValues(t *testing.T) {
	cases := []struct {
		name string
		src  string
		key  string
		want any
	}{
		{"十进制整数", "offset: 200", "offset", 200},
		{"负数", "offset: -200", "offset", -200},
		{"带正号", "offset: +200", "offset", 200},
		{"零", "offset: 0", "offset", 0},
		{"负零", "offset: -0", "offset", 0},
		{"行尾注释", "offset: 200 # 注释", "offset", 200},
		{"十六进制", "offset: 0x10", "offset", 16},
		{"下划线分隔", "offset: 1_000", "offset", 1000},
		{"双引号", `offset: "200"`, "offset", "200"},
		{"单引号", "offset: '200'", "offset", "200"},
		{"小数", "offset: 200.0", "offset", 200.0},
		{"空值", "offset:", "offset", nil},
		{"UTF-8 BOM", "\xef\xbb\xbfoffset: 200", "offset", 200},
		{"CRLF", "offset: 200\r\npoll: 30\r\n", "offset", 200},
		{"整体缩进", "  offset: 200\n  poll: 30\n", "offset", 200},
		{"版本号不加引号", "config-version: 4.0.0", "config-version", "4.0.0"},
		{"预发布版本号不加引号", "config-version: 4.0.0-beta.3", "config-version", "4.0.0-beta.3"},
		{"两段版本号不加引号", "config-version: 4.0", "config-version", 4.0},
		{"两段版本号加引号", `config-version: "4.0"`, "config-version", "4.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var m map[string]interface{}
			if err := yaml.Unmarshal([]byte(c.src), &m); err != nil {
				t.Fatalf("解析 %q 失败: %v", c.src, err)
			}
			got, ok := m[c.key]
			if !ok {
				t.Fatalf("解析 %q 后没有键 %s: %v", c.src, c.key, m)
			}
			if got != c.want {
				t.Errorf("%q 的 %s = %T(%v)，期望 %T(%v)", c.src, c.key, got, got, c.want, c.want)
			}
		})
	}
}

// 被注释掉的键不进解析结果。
func TestYAMLCommentedKeyIsAbsent(t *testing.T) {
	var m map[string]interface{}
	if err := yaml.Unmarshal([]byte("# wesing-offset: 0\noffset: 200\n"), &m); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if _, ok := m["wesing-offset"]; ok || len(m) != 1 {
		t.Errorf("注释掉的键不应出现在解析结果里: %v", m)
	}
}

// 重复的键使整份文件解析失败：loadFile 记 FileParseFailed，config.yml 一层不生效。
func TestYAMLDuplicateKeyFailsParse(t *testing.T) {
	var m map[string]interface{}
	if err := yaml.Unmarshal([]byte("offset: 200\npoll: 30\noffset: 300\n"), &m); err == nil {
		t.Errorf("重复的键应使解析失败，实际得到 %v", m)
	}
}

// 带 BOM 的 UTF-16 文件（记事本的「Unicode」编码）能被解析：按字节逐行改写的逻辑不能假定文件是 UTF-8。
func TestYAMLUTF16WithBOMParses(t *testing.T) {
	units := utf16.Encode(append([]rune{0xFEFF}, []rune("offset: 200\r\n")...))
	data := make([]byte, 0, len(units)*2)
	for _, u := range units {
		data = append(data, byte(u), byte(u>>8)) // UTF-16LE
	}
	var m map[string]interface{}
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("解析 UTF-16LE 失败: %v", err)
	}
	if m["offset"] != 200 {
		t.Errorf("offset = %T(%v)，期望 200", m["offset"], m["offset"])
	}
}
