package main

// 本文件只测一件事：scanFile 认得出哪些字面量是目标调用的第一个实参，认不出范围外的写法。

import (
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

// scan 解析一段源码并交给 scanFile。
func scan(t *testing.T, name, src string, test bool) ([]literal, []nonLiteral) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("解析合成源码失败: %v", err)
	}
	return scanFile(fset, file, name, []byte(src), test)
}

// directs 返回直接实参，写成「调用=值」。
func directs(lits []literal) []string {
	var out []string
	for _, l := range lits {
		if l.direct() {
			out = append(out, l.callee+"="+l.val)
		}
	}
	return out
}

// TestScanRecognizesTargetCalls 钉死出货代码里的目标形状。
//
// 变异自证（实测）：
//   - scanFile 不再认 logger 的方法名（target 恒不取 loggerMethods）：红，少了 log.* 四条。
//   - 不再认 i18n.T 与 i18n.Errorf：红，少了 loc.* 三条。
//   - 相加的字面量不整段收（foldStringLit 不处理 BinaryExpr）：红，「日志乙」不再是直接实参，字面量处数也多一处。
func TestScanRecognizesTargetCalls(t *testing.T) {
	const src = `package p

import (
	"errors"
	"fmt"
	"net/http"

	loc "Metabox-Nexus-PlayerCap/i18n"
	"Metabox-Nexus-PlayerCap/logger"
)

var log = logger.New("X")

var note = "存进变量的文案"

func f(w http.ResponseWriter, err error, name string) error {
	log.Info("日志甲 %s", name)
	log.Warn("日志乙：" +
		"分两段")
	log.Detail(("日志丙"))
	fmt.Println(loc.T("文案丁"))
	_ = errors.New(loc.T("text E"))
	if err != nil {
		return loc.Errorf("错误己: %w", err)
	}
	log.Error("%s", "当数据注入的中文")

	log.Info(name)
	fmt.Println(loc.T(note))
	fmt.Println("直接打印的中文")
	http.Error(w, "not found", 404)
	return nil
}
`
	lits, nonLits := scan(t, "p.go", src, false)

	want := []string{
		"log.Info=日志甲 %s", "log.Warn=日志乙：分两段", "log.Detail=日志丙",
		"loc.T=文案丁", "loc.T=text E", "loc.Errorf=错误己: %w", "log.Error=%s",
	}
	if got := directs(lits); !slices.Equal(got, want) {
		t.Errorf("直接实参与预期不符：\n  得到 %q\n  预期 %q", got, want)
	}

	// 其余字面量照样逐处收下（供 -baseline 配对），只是不算直接实参。import 路径不收。
	var rest []string
	for _, l := range lits {
		if !l.direct() {
			rest = append(rest, l.val)
		}
	}
	wantRest := []string{"X", "存进变量的文案", "当数据注入的中文", "直接打印的中文", "not found"}
	if !slices.Equal(rest, wantRest) {
		t.Errorf("其余字面量与预期不符：\n  得到 %q\n  预期 %q", rest, wantRest)
	}

	// 实参不是字面量的目标调用。http.Error 与 logger 的 Error 同名，按形状认会算进来，属已知的误收。
	var got []string
	for _, n := range nonLits {
		got = append(got, n.callee+"("+n.src+")")
	}
	wantNonLits := []string{"log.Info(name)", "loc.T(note)", "http.Error(w)"}
	if !slices.Equal(got, wantNonLits) {
		t.Errorf("非字面量调用点与预期不符：\n  得到 %q\n  预期 %q", got, wantNonLits)
	}
}

// TestScanTestFilesOnlyI18nCalls 钉死测试文件里只认 i18n.T 与 i18n.Errorf。
//
// 变异自证（实测）：去掉 scanFile 里对 test 的判断，红——t.Error 与 t.Errorf 的中文被当成 logger 的 format。
func TestScanTestFilesOnlyI18nCalls(t *testing.T) {
	const src = `package p

import (
	"testing"

	"Metabox-Nexus-PlayerCap/i18n"
)

func TestX(t *testing.T) {
	if got := f(); got != i18n.T("文案丁") {
		t.Error("得到的文案不对")
		t.Errorf("得到 %q", got)
	}
	log.Info("测试里的日志")
}
`
	lits, nonLits := scan(t, "p_test.go", src, true)
	if got, want := directs(lits), []string{"i18n.T=文案丁"}; !slices.Equal(got, want) {
		t.Errorf("直接实参与预期不符：\n  得到 %q\n  预期 %q", got, want)
	}
	if len(nonLits) != 0 {
		t.Errorf("测试文件不列非字面量调用点，得到 %v", nonLits)
	}
}

// TestScanBareCallsInsideI18n 钉死 i18n 包内不带包名的 T 与 Errorf 被认出，别的包里同名的函数不认。
//
// 变异自证（实测）：inPkg 恒为假，红——包内的两条认不出；恒为真，红——别的包里的 T 被误收。
func TestScanBareCallsInsideI18n(t *testing.T) {
	const body = `

func g() error {
	_ = T("包内文案")
	return Errorf("包内错误 %d", 1)
}
`
	lits, _ := scan(t, "i18n/x.go", "package i18n"+body, false)
	if got, want := directs(lits), []string{"T=包内文案", "Errorf=包内错误 %d"}; !slices.Equal(got, want) {
		t.Errorf("i18n 包内的直接实参与预期不符：\n  得到 %q\n  预期 %q", got, want)
	}
	lits, _ = scan(t, "q/x.go", "package q"+body, false)
	if got := directs(lits); len(got) != 0 {
		t.Errorf("别的包里不带包名的 T 与 Errorf 不该被认，得到 %q", got)
	}
}
