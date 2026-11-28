package rum

// 本文件钉死三件事：
// 1. 两条 Windows 构建流水线里 ldflags `-X` 注入的符号名与本包真实的 `apiKey`、`host` 变量一致；
// 2. `rum.go` 中 `apiKey` 与 `host` 均为用空串字面量 `""` 初始化的包级 `var`（满足 `-X` 条件且不把真实密钥写进仓库）；
// 3. `release.yml` 的 `Show build info` 步骤对 `rum.apiKey` 执行脱敏，避免 CI 日志泄露密钥。
//
// 变异自证：把 `rum.go` 的 `apiKey` 改为 `const` / 改名 / 非空默认值，或删掉任一 workflow 的 `-X` 或脱敏规则，
// 对应测试用例立即变红。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

var rumLdflagsVars = []string{"apiKey", "host"}

var rumLdflagsWorkflows = []string{
	"../.github/workflows/build-windows.yml",
	"../.github/workflows/release.yml",
}

var rumXFlagRe = regexp.MustCompile(`-X '([^='\s]+)=`)

// TestWorkflowLdflagsSymbolMatchesVars 钉死两条 workflow 的 `-X` 符号名与本包真实变量符号一致。
func TestWorkflowLdflagsSymbolMatchesVars(t *testing.T) {
	pkgPath := reflect.TypeOf(Options{}).PkgPath()

	for _, wf := range rumLdflagsWorkflows {
		t.Run(filepath.Base(wf), func(t *testing.T) {
			data, err := os.ReadFile(wf)
			if err != nil {
				t.Fatalf("读不到 %s: %v", wf, err)
			}
			text := string(data)

			for _, varName := range rumLdflagsVars {
				wantSymbol := pkgPath + "." + varName
				if !strings.Contains(text, "-X '"+wantSymbol+"=") {
					var found []string
					for _, m := range rumXFlagRe.FindAllStringSubmatch(text, -1) {
						found = append(found, m[1])
					}
					t.Errorf("%s 未注入 %s；文件实际的 -X 符号：%v", wf, wantSymbol, found)
				}
			}
		})
	}
}

// TestLdflagsVarsStayInjectable 钉死 rum.go 中的 apiKey 与 host 均为用空串字面量初始化的包级 var。
func TestLdflagsVarsStayInjectable(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "rum.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 rum.go 失败: %v", err)
	}

	found := map[string]bool{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || (gd.Tok != token.VAR && gd.Tok != token.CONST) {
			continue
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name != "apiKey" && n.Name != "host" {
					continue
				}
				found[n.Name] = true
				if gd.Tok == token.CONST {
					t.Fatalf("%s 被声明为 const，`-X` 注入会静默失效", n.Name)
				}
				if i >= len(vs.Values) {
					t.Fatalf("var %s 缺少初始化表达式", n.Name)
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("var %s 必须用 string 常量字面量初始化，实际为 %T", n.Name, vs.Values[i])
				}
				if lit.Value != `""` {
					t.Errorf("var %s 默认值必须是空串 \"\"，实际为 %s", n.Name, lit.Value)
				}
			}
		}
	}

	for _, name := range rumLdflagsVars {
		if !found[name] {
			t.Fatalf("rum.go 中找不到包级 `var %s`", name)
		}
	}
}

// TestReleaseWorkflowRedactsAPIKey 钉死 release.yml 的 Show build info 步骤对 rum.apiKey 进行脱敏。
func TestReleaseWorkflowRedactsAPIKey(t *testing.T) {
	const wf = "../.github/workflows/release.yml"
	data, err := os.ReadFile(wf)
	if err != nil {
		t.Fatalf("读不到 %s: %v", wf, err)
	}
	text := string(data)

	idx := strings.Index(text, "- name: Show build info")
	if idx < 0 {
		t.Fatalf("%s 中找不到 Show build info 步骤", wf)
	}
	step := text[idx:]
	if next := strings.Index(step[1:], "\n      - name:"); next >= 0 {
		step = step[:next+1]
	}

	const wantReplace = `-replace "rum\.apiKey=[^'\s]+", 'rum.apiKey=<redacted>'`
	if !strings.Contains(step, wantReplace) {
		t.Errorf("release.yml 的 Show build info 步骤缺少 rum.apiKey 脱敏规则 %q\n实际步骤：\n%s", wantReplace, step)
	}
}
