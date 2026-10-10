package config

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"unicode/utf8"

	"Metabox-Nexus-PlayerCap/i18n"

	"gopkg.in/yaml.v3"
)

// config.yml 的文本层改写（设计 §3.2）：按字节逐行处理，保留每行自己的行尾，不重新序列化——注释、
// 键序、缩进、空行都不变。它只认少数几种写法，认不了的一律不写文件，由三道分别拦住：
//
//   - 显式检查（checkRewritable）：非 UTF-8 编码、多文档。这两种后面的校验拦不住。
//   - 「恰好对应一行」：要改的键在文本里须恰好对应一行认得的键行。流式写法、带引号的键、键与值分行
//     找不到对应行；嵌套映射或块标量里另有同名的行则对应到不止一行。
//   - 校验（sameParse）：改写后的文本重新解析，须与内存里的期望值完全相同。

// versionKeyComment 是版本键上方的注释，模板与插入版本键时共用。
const versionKeyComment = "配置文件版本，由程序维护，请勿手动修改"

var utf8BOM = []byte("\xEF\xBB\xBF")

// versionLineRE 认版本键行：键 + 冒号 + 同一行内的值 + 可选的行尾注释。值可以带引号或不带，可以为空，
// 也可以是别的类型（手改成 4.0）。分组：1 行首（BOM、缩进）到冒号，2 冒号后的空白，3 值（不以 # 开头：
// 冒号后直接跟注释的是空值），4 行尾注释与行末空白，5 行尾。
//
// 它只看一行：值写在下一行、或值是块标量（| 与 >）时，键行本身照样被认作一行并替换，改写出的文本
// 解析不了或解析结果不符，由 sameParse 拦下。
var versionLineRE = regexp.MustCompile(`^(\x{FEFF}?[ \t]*` + versionKey + `[ \t]*:)([ \t]*)((?:[^ \t#\r\n][^\r\n]*?)?)((?:[ \t]+#[^\r\n]*)?[ \t]*)(\r?\n)?$`)

// checkRewritable 是改写之前的两条显式检查，不过即不得按行改写 data。
//
// 带 BOM 的 UTF-16 能被 yaml 解析，按字节改写却会写坏。yaml.Unmarshal 只解析第一个文档且不报错，后面的
// 文档它与 sameParse 都看不见，逐行改写却会动到它们。
func checkRewritable(data []byte) error {
	if !utf8.Valid(data) {
		return i18n.Errorf("the file is not UTF-8 encoded")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
		return nil // 空文件或只有注释：没有文档
	} else if err != nil {
		return err
	}
	if err := dec.Decode(&doc); !errors.Is(err, io.EOF) {
		return i18n.Errorf("the file contains more than one YAML document")
	}
	return nil
}

// sameParse 报告 data 的解析结果是否与 want 完全相同。
func sameParse(data []byte, want map[string]interface{}) bool {
	var got map[string]interface{}
	if err := yaml.Unmarshal(data, &got); err != nil {
		return false
	}
	return reflect.DeepEqual(got, want)
}

// splitLines 把 data 切成行，每行带着自己的行尾（末行可能没有）。各行依次相接即 data。
func splitLines(data []byte) [][]byte {
	var lines [][]byte
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			lines = append(lines, data)
			break
		}
		lines = append(lines, data[:i+1])
		data = data[i+1:]
	}
	return lines
}

// lineEnding 返回一行的行尾：CRLF、LF 或空串。
func lineEnding(line []byte) string {
	switch {
	case bytes.HasSuffix(line, []byte("\r\n")):
		return "\r\n"
	case bytes.HasSuffix(line, []byte("\n")):
		return "\n"
	}
	return ""
}

// lineBody 返回一行去掉 BOM、缩进与行尾之后的内容，以及它的缩进。
func lineBody(line []byte) (body, indent []byte) {
	line = bytes.TrimPrefix(line, utf8BOM)
	line = bytes.TrimRight(line, "\r\n")
	body = bytes.TrimLeft(line, " \t")
	return body, line[:len(line)-len(body)]
}

// setVersionKeyLine 返回把版本键写成 stamp 之后的各行，不改动 lines 本身。
//
// present 为真（解析结果里已有版本键）时，文本里须恰好有一行认得的版本键行：把冒号之后、行尾注释之前的
// 部分整体换成带双引号的 stamp，行首缩进、冒号前后的空白与行尾注释保留。present 为假时插入版本键，
// notes 是写在键上方的各行注释（见 insertVersionKey）。
func setVersionKeyLine(lines [][]byte, present bool, stamp string, notes []string) ([][]byte, error) {
	if !present {
		return insertVersionKey(lines, stamp, notes), nil
	}
	at := -1
	for i, line := range lines {
		if !versionLineRE.Match(line) {
			continue
		}
		if at >= 0 {
			return nil, i18n.Errorf("more than one line looks like the %s key", versionKey)
		}
		at = i
	}
	if at < 0 {
		return nil, i18n.Errorf("no line in a recognized form for the %s key", versionKey)
	}
	g := versionLineRE.FindSubmatch(lines[at])
	space := g[2]
	if len(space) == 0 {
		space = []byte(" ") // 原先冒号后直接是行尾或注释
	}
	out := append([][]byte{}, lines...)
	out[at] = bytes.Join([][]byte{g[1], space, []byte(`"` + stamp + `"`), g[4], g[5]}, nil)
	return out, nil
}

// insertVersionKey 在文件里插入版本键：各行注释、键行、一个空行，放在第一个键行（第一个既非空行也非
// 注释的行）及紧贴其上的注释之前，使原先写给那个键的注释仍贴着它。缩进取那个键行的缩进，行尾取文件
// 第一行的行尾，文件里没有行尾时取 LF。插在文件最前面而首行带 UTF-8 BOM 时，BOM 仍留在文件开头。
// 文件里没有键行（空文件、只有注释）时接在末尾。
func insertVersionKey(lines [][]byte, stamp string, notes []string) [][]byte {
	eol := "\n"
	if len(lines) > 0 && lineEnding(lines[0]) != "" {
		eol = lineEnding(lines[0])
	}
	first := -1
	for i, line := range lines {
		if body, _ := lineBody(line); len(body) > 0 && body[0] != '#' {
			first = i
			break
		}
	}

	var indent []byte
	if first >= 0 {
		_, indent = lineBody(lines[first])
	}
	var block [][]byte
	for _, note := range notes {
		block = append(block, bytes.Join([][]byte{indent, []byte("# " + note + eol)}, nil))
	}
	block = append(block, bytes.Join([][]byte{indent, []byte(versionKey + `: "` + stamp + `"` + eol)}, nil))

	if first < 0 {
		out := append([][]byte{}, lines...)
		if n := len(out); n > 0 && lineEnding(out[n-1]) == "" {
			out[n-1] = append(append([]byte{}, out[n-1]...), eol...)
		}
		return append(out, block...)
	}

	at := first
	for at > 0 {
		if body, _ := lineBody(lines[at-1]); len(body) == 0 || body[0] != '#' {
			break
		}
		at--
	}
	block = append(block, []byte(eol))
	out := append([][]byte{}, lines[:at]...)
	rest := lines[at:]
	if at == 0 && bytes.HasPrefix(rest[0], utf8BOM) {
		block[0] = append(append([]byte{}, utf8BOM...), block[0]...)
		rest = append([][]byte{bytes.TrimPrefix(rest[0], utf8BOM)}, rest[1:]...)
	}
	return append(append(out, block...), rest...)
}

// intLineRE 认整数键行：键 + 冒号 + 可带正负号的十进制整数 + 可选的行尾注释，可以整行被注释掉。
// 十六进制、带下划线分隔等 yaml 同样解析为整数的写法不在其内。分组：1 BOM 与缩进，2 整行被注释掉时的
// # 与其后的空白，3 键，4 冒号及其前后的空白，5 整数，6 行尾注释与行末空白，7 行尾。
var intLineRE = regexp.MustCompile(`^(\x{FEFF}?[ \t]*)(#[ \t]*)?([A-Za-z0-9][A-Za-z0-9_-]*)([ \t]*:[ \t]+)([+-]?[0-9]+)((?:[ \t]+#[^\r\n]*)?[ \t]*)(\r?\n)?$`)

// rewriteMigrated 返回迁移之后的各行，不改动 lines 本身。三件事：
//
//   - 把值层改过的键（changed）写回各自的行，只换数字，缩进、间距、行尾注释保留。每个键在文本里须恰好
//     对应一行认得的整数键行，且行里的数字就是它原先的值，否则报错。
//   - 整行恰为「# + 整数键行」的行（被注释掉的键）同样换算：用户日后取消注释时，它会按新语义生效。
//     这一条没有解析结果可对照，所以只认整行匹配，且数字须是规范的十进制写法（去掉前导的 + 之后与
//     strconv.Itoa 的结果相同）：带前导零的数字 yaml 按八进制解析（01000 即 512），照十进制取反会改掉
//     它日后的值。写法不规范的、数值超出 int 范围的不动。
//   - 注释行里说明旧语义的短语（migrationStep.phrases）整体替换，其余注释不动。
func rewriteMigrated(lines [][]byte, changed map[string]intChange, steps []migrationStep, players []string) ([][]byte, error) {
	convert := func(key string, v int) int { // 按各步依次换算被注释掉的键
		for _, s := range steps {
			for _, k := range s.intKeys(players) {
				if k == key {
					v = s.convert(v)
				}
			}
		}
		return v
	}

	out := append([][]byte{}, lines...)
	seen := map[string]int{}
	for i, line := range lines {
		g := intLineRE.FindSubmatch(line)
		if g == nil {
			continue
		}
		key := string(g[3])
		n, err := strconv.Atoi(string(g[5]))
		value := n
		if len(g[2]) > 0 { // 被注释掉的键
			if err != nil || strconv.Itoa(n) != string(bytes.TrimPrefix(g[5], []byte("+"))) {
				continue
			}
			value = convert(key, n)
		} else {
			c, ok := changed[key]
			if !ok {
				continue
			}
			seen[key]++
			if err != nil || n != c.old {
				return nil, i18n.Errorf("the line for %s does not carry its value as a plain decimal integer", key)
			}
			value = c.new
		}
		if value != n {
			out[i] = bytes.Join([][]byte{g[1], g[2], g[3], g[4], []byte(strconv.Itoa(value)), g[6], g[7]}, nil)
		}
	}
	for key := range changed {
		if seen[key] != 1 {
			return nil, i18n.Errorf("no single line in a recognized form for the %s key", key)
		}
	}

	for i, line := range out {
		if body, _ := lineBody(line); len(body) == 0 || body[0] != '#' {
			continue
		}
		for _, s := range steps {
			for _, p := range s.phrases {
				line = bytes.ReplaceAll(line, []byte(p[0]), []byte(p[1]))
			}
		}
		out[i] = line
	}
	return out, nil
}

// rewriteText 返回 config.yml 改写后的全文：先做 steps 的迁移（rewriteMigrated），再把版本键写成 stamp
// （setVersionKeyLine；hadKey 报告解析结果里原先有没有版本键）。want 是改写后应有的解析结果，
// 改写出的文本解析出来与它不符即报错。任何一步报错都不产出文本，调用方不得写文件。
func rewriteText(data []byte, hadKey bool, want map[string]interface{}, changed map[string]intChange, steps []migrationStep, players []string, stamp string) ([]byte, error) {
	if err := checkRewritable(data); err != nil {
		return nil, err
	}
	lines, err := rewriteMigrated(splitLines(data), changed, steps, players)
	if err != nil {
		return nil, err
	}
	notes := []string{versionKeyComment}
	for _, s := range steps {
		if s.note != "" {
			notes = append(notes, s.note)
		}
	}
	if lines, err = setVersionKeyLine(lines, hadKey, stamp, notes); err != nil {
		return nil, err
	}
	out := bytes.Join(lines, nil)
	if !sameParse(out, want) {
		return nil, i18n.Errorf("the rewritten text does not parse back to the expected values")
	}
	return out, nil
}
