package i18n

// 本文件只测一件事：「英文原文作 key」这一约定在出货代码与译表两侧都守住——直接交给 logger 或
// i18n 的文案不含汉字、都是中文译表的键，译表的键也不含汉字。
// 它不测本包的业务逻辑——扫的是整个仓库的源码。
//
// # 为什么要门禁
//
// T 对未收录的键回退英文原文，不报错。新增一条日志而没补译表，中文系统上那一行就是英文，
// 没有任何测试会红。反过来，照 issue #61 之前的约定把中文原文写在调用点上，中文系统上看着
// 正常（查不到、原样输出），英文系统上那一行却是中文，同样不报错。
//
// # 它守什么
//
// 扫出货代码（非 _test.go；跳过以 . 开头的目录与 build、tools、testdata），收集**直接作为实参**
// 传给以下调用的字符串字面量（含字面量用 + 相加）：
//
//   - 方法名为 Info / Success / Warn / Error / Detail 的调用的第一个实参（logger 的 format）；
//   - i18n.T 与 i18n.Errorf 的第一个实参（本包内为不带包名的 T 与 Errorf）。
//
// 对每一条断言：不含汉字（TestShippedMessagesHaveNoHan）；是 chinese 的键，或属下述豁免
// （TestCatalogCoversShippedMessages）。另断言 chinese 的键都不含汉字（TestCatalogKeysHaveNoHan）。
// chinese 是本包查表用的那一张，p4 两张表已由各自的 init 并入。
//
// # 豁免
//
// 去掉 fmt 占位符之后不含任何字母与数字的字面量（只剩标点、符号与空白，如 "%s"、"[%d/%d] %s"）
// 没有可译的内容，不要求进译表，判定见 catalogExempt。除此之外没有豁免清单：不含汉字、定为
// 中文环境下原样输出的格式串，在译表里写成键与值相同的一条。
//
// # 它不守什么
//
//   - 当数据注入 %s 的值（AGENTS.md §6.4 ⑤）：是否过了 i18n.T 要看数据流，形状上看不出。
//     已写成 i18n.T("…") 的那些在范围内。
//   - 不该翻译的中文（§6.4 ⑥）：用于匹配与比较的常量、注入页面的脚本、只上报遥测的文案。
//     它们不经上述调用，本就扫不到。
//   - format 不是字面量的调用（先存进变量或常量再传、经辅助函数转手）：扫不到。现有的两处在
//     bannerconfig.go（bannerGlobalLines 与 bannerConfigFileNotes），由根包 bannerconfig_test.go
//     在两种语言下断言输出。
//   - 只按调用形状判断，不做类型解析：别的类型若有同名方法且首个实参是字符串字面量，会被当成
//     logger 调用。现有的同名调用只有 http.Error，首个实参不是字面量；出现误收时在这里区分，
//     不要去补一条无人读取的译文。
//   - 译文本身对不对、是不是中文：不查。占位符由 placeholders_test.go 查。
//   - 译表里无人引用的键：不查。
//
// tools/i18nrekey 的核对与本文件同一口径（认的调用形状、豁免规则各写了一份，改一处须同步另一处）；
// 按旧约定写成的代码合入后，用它的 -write 把调用点与条目改过来。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// i18nImportPath 是本包的 import 路径，用来认出各文件里它的本地名。
const i18nImportPath = "Metabox-Nexus-PlayerCap/i18n"

// loggerMethods 是 logger.Logger 的五个输出方法，第一个参数是 format。
var loggerMethods = map[string]bool{"Info": true, "Success": true, "Warn": true, "Error": true, "Detail": true}

// catalogUse 是一处直接交给 logger 或 i18n 的字符串字面量。
type catalogUse struct {
	pos string // 文件:行
	key string
}

// TestCatalogCoversShippedMessages 断言出货代码里交给 logger 与 i18n 的文案都在中文译表里。
//
// 变异自证（隔离副本，实测）：
//   - 从 messages.go 删掉被 config.go 引用的键 "volume normalization switch"：红，报 config/config.go 的那一行。
//   - 在 server/audio.go 里新增一句 serverLog.Warn("mutation: a log line without translation %d", 1)：红，报新增的那一行。
//   - 删掉 messages.go 里键值相同的 "WS Upgrade error: %v"：红，报 server/server.go 的那一行。
//   - 豁免放宽为「不含汉字即可」：仍绿（现有的字面量都在表里），由 TestCatalogExemptionIsNarrow 拦。
//   - 扫描自身：scanCatalogUses 不再认 logger 的方法名，或不再认 i18n.T 与 i18n.Errorf，本用例仍绿
//     （扫到的少了，剩下的都在表里），由 TestCatalogScanCatchesKnownShapes 拦住，两种各自变红。
func TestCatalogCoversShippedMessages(t *testing.T) {
	uses := scanRepoCatalogUses(t, "..")
	if len(uses) == 0 {
		t.Fatal("没有扫到任何交给 logger 或 i18n 的字面量：扫描的判据失效了")
	}
	for _, u := range uses {
		if hasHan(u.key) || catalogExempt(u.key) {
			continue // 含汉字的由 TestShippedMessagesHaveNoHan 报
		}
		if _, ok := chinese[u.key]; !ok {
			t.Errorf("%s：%q 不在中文译表里，中文系统上会原样显示英文。"+
				"把它加进 i18n/messages*.go 的表并配上中文；定为原样输出的写成键与值相同（AGENTS.md §6.4 ①）", u.pos, u.key)
		}
	}
}

// TestShippedMessagesHaveNoHan 断言出货代码里交给 logger 与 i18n 的文案不含汉字：key 是英文原文。
//
// 变异自证（隔离副本，实测）：
//   - 把 server/audio.go 里一条日志的格式串换回它的中文译文：红，报那一行。
//   - 在 config/config.go 里新增 i18n.T("变异：中文原文")：红。
//   - 同时把这句中文加进译表（照旧约定补一条「中文 → 英文」）：仍红，且 TestCatalogKeysHaveNoHan 也红。
func TestShippedMessagesHaveNoHan(t *testing.T) {
	for _, u := range scanRepoCatalogUses(t, "..") {
		if hasHan(u.key) {
			t.Errorf("%s：%q 含汉字。交给 logger 与 i18n 的文案以英文原文作 key，中文写在译表里（AGENTS.md §6.4）；"+
				"译表里已有它的英文时可用 go run ./tools/i18nrekey -write 改写", u.pos, u.key)
		}
	}
}

// TestCatalogKeysHaveNoHan 断言译表的键不含汉字。键含汉字的是 issue #61 之前「中文 → 英文」方向的条目，
// 混进来之后中文环境下会把那句中文译成英文。
//
// 变异自证（隔离副本，实测）：往 messages.go 加一条 "变异：旧方向": "mutation: old direction"，红。
func TestCatalogKeysHaveNoHan(t *testing.T) {
	for en := range chinese {
		if hasHan(en) {
			t.Errorf("译表的键 %q 含汉字：键是英文原文、值是中文。用 go run ./tools/i18nrekey -write 对调", en)
		}
	}
}

// TestCatalogExemptionIsNarrow 钉死豁免只放行没有可译内容的格式串。
//
// 变异自证（实测）：catalogExempt 不先去掉占位符，红；只查字母不查数字，红；恒为真，红；恒为假，红。
func TestCatalogExemptionIsNarrow(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"%s", true},
		{"[%d/%d] %s", true},
		{"  - %-8s | %5.1f%%\n", true},
		{"%[2]s → %[1]v", true},

		{"%ds", false},              // %d 之后的 s 是文案（秒）
		{"[%2d] %6.1fs  %s", false}, // 同上
		{"vtable: 0x%08X", false},
		{"1/%d", false}, // 数字也算内容
		{"%s ok", false},
		{"歌曲: %s", false},
		{"%z", false}, // 不是合法占位符，z 是字母
	}
	for _, c := range cases {
		if got := catalogExempt(c.s); got != c.want {
			t.Errorf("catalogExempt(%q) = %v，预期 %v", c.s, got, c.want)
		}
	}
}

// TestHasHan 钉死「含汉字」的判定：只认汉字，不认全角标点、符号与假名。
// TestShippedMessagesHaveNoHan 与 TestCatalogKeysHaveNoHan 都靠它，它恒为假时那两条恒绿。
//
// 变异自证（实测）：hasHan 恒为假，红（那两条仍绿）；恒为真，红。
func TestHasHan(t *testing.T) {
	cases := map[string]bool{
		"歌曲: %s":  true,
		"CDP 已就绪": true,
		"繁體":      true,

		"Song: %s":         false,
		"a → b — c「」，。：（）": false,
		"♪ %s":             false,
		"かな":               false,
		"":                 false,
	}
	for s, want := range cases {
		if got := hasHan(s); got != want {
			t.Errorf("hasHan(%q) = %v，预期 %v", s, got, want)
		}
	}
}

// TestCatalogScanCatchesKnownShapes 钉死扫描认得出目标形状、不误收范围外的写法。
//
// 变异自证（实测）：scanCatalogUses 不再认 logger 的方法名、不再认 i18n.T 与 i18n.Errorf、
// 只收含汉字的字面量（issue #61 之前的判据）、不认本包内不带包名的 T，各自变红。
func TestCatalogScanCatchesKnownShapes(t *testing.T) {
	const src = `package p

import (
	"errors"
	"fmt"

	loc "Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
)

var log = logger.New("X")

func f(err error, name string) error {
	log.Info("log A %s", name)
	log.Warn("log B: " + "two parts")
	log.Detail(("log C"))
	fmt.Println(loc.T("text D"))
	_ = errors.New(loc.T("text E"))
	if err != nil {
		return loc.Errorf("error F: %w", err)
	}
	log.Success("照旧约定写的中文 %s", name)
	log.Error("%s", "当数据注入的值") // 只收第一个实参

	log.Info(name)                  // 不是字面量：不收
	fmt.Println("printed directly") // 不经 logger 与 i18n：不收
	return nil
}
`
	want := []string{"log A %s", "log B: two parts", "log C", "text D", "text E", "error F: %w", "照旧约定写的中文 %s", "%s"}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("解析合成源码失败: %v", err)
	}
	var got []string
	for _, u := range scanCatalogUses(fset, file) {
		got = append(got, u.key)
	}
	if !slices.Equal(got, want) {
		t.Errorf("扫出的键与预期不符：\n  得到 %q\n  预期 %q", got, want)
	}

	// 本包内的调用不带包名。
	const inPkg = `package i18n

func g() string { return T("in-package text") }
`
	file, err = parser.ParseFile(fset, "i.go", inPkg, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("解析合成源码失败: %v", err)
	}
	if uses := scanCatalogUses(fset, file); len(uses) != 1 || uses[0].key != "in-package text" {
		t.Errorf("本包内不带包名的 T 调用没有被认出：%v", uses)
	}
}

// scanRepoCatalogUses 扫 root 下的出货代码，返回全部直接交给 logger 或 i18n 的字符串字面量。
func scanRepoCatalogUses(t *testing.T, root string) []catalogUse {
	t.Helper()
	fset := token.NewFileSet()
	var uses []catalogUse
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "build" || name == "tools" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		uses = append(uses, scanCatalogUses(fset, file)...)
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 %s 失败: %v", root, err)
	}
	return uses
}

// scanCatalogUses 取出一个文件里直接交给 logger 或 i18n 的字符串字面量，按出现顺序返回。
func scanCatalogUses(fset *token.FileSet, file *ast.File) []catalogUse {
	// 本文件里 i18n 包的本地名；没有 import 时为空串。本包自身的调用不带包名。
	inPkg := file.Name.Name == "i18n"
	local := ""
	for _, imp := range file.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err == nil && p == i18nImportPath {
			local = "i18n"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
	}

	var uses []catalogUse
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		target := false
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if x, ok := fun.X.(*ast.Ident); ok && local != "" && x.Name == local {
				target = fun.Sel.Name == "T" || fun.Sel.Name == "Errorf"
			} else {
				target = loggerMethods[fun.Sel.Name]
			}
		case *ast.Ident:
			target = inPkg && (fun.Name == "T" || fun.Name == "Errorf")
		}
		if !target {
			return true
		}
		if s, ok := foldStringLit(call.Args[0]); ok {
			uses = append(uses, catalogUse{pos: fset.Position(call.Args[0].Pos()).String(), key: s})
		}
		return true
	})
	return uses
}

// foldStringLit 返回字符串字面量的值，字面量用 + 相加的先拼接；含别的表达式时 ok 为 false。
func foldStringLit(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		a, okA := foldStringLit(v.X)
		b, okB := foldStringLit(v.Y)
		return a + b, okA && okB
	case *ast.ParenExpr:
		return foldStringLit(v.X)
	}
	return "", false
}

// hasHan 报告 s 是否含汉字。
func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// catalogExempt 报告 s 是否可以不进译表：去掉 fmt 占位符（verbRe，见 placeholders_test.go）之后
// 不含任何字母与数字。
func catalogExempt(s string) bool {
	for _, r := range verbRe.ReplaceAllString(s, "") {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
