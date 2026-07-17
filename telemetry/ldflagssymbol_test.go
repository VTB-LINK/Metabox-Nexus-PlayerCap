package telemetry

// 本文件只测一件事：**两条 workflow 里 ldflags 的符号名，和本包里那个变量，是不是同一个东西**。
//
// # 为什么这条必须做成门禁，而不是写句注释
//
// `go build -ldflags -X` 对写错的符号名是**静默 no-op**：零警告、零错误、exit=0，变量保持
// 空串。实测判据是 `go tool nm` 里 `telemetry.dsn.str` 符号直接消失。
//
// 于是整条失败链无声：
//
//	有人重命名 dsn / 挪包 / 改 module path（WesingCap→PlayerCap 这种改名本仓库发生过一次，
//	见 main.go 的 cleanupLegacyExe）
//	  → 四道门禁全绿 —— workflow 不参与编译，而本包的测试直接改包内变量，从不走 ldflags
//	  → 发版
//	  → 所有 CI 构建零 event
//	  → 启动日志打「未注入 DSN，遥测已禁用」
//
// 最后一行是要命的：它与「secret 真的没配」**输出完全相同**。而两条 workflow 的注释里明写着
// 「那就是 secret 没生效的信号」—— 排查的人会直奔 secret 设置，永远查不到符号名上去。
// 而我们这头看到的「Sentry 零 event」，与「主播那边真的没崩过」也完全同形。
//
// 对照 main.Version：它有同样的 -X 耦合，但断了是**响亮**故障 —— Banner 显示 0.0.0、
// isReleaseVersion 立刻改变自动更新行为，当场自曝。
// 「静默 + 可误诊 + 只在别人机器上留痕」这个组合是 dsn 独有的，所以只有它需要门禁。
//
// 变异自证：把 telemetry.go 的 `dsn` 改名成 `sentryDSN`（workflow 不动）——
// gofmt/build/vet/test 原本四道全绿，加了本文件后 TestWorkflowLdflagsSymbolMatchesVar 红。

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

// dsnVarName 是承接 ldflags 注入的包级变量名。改它必须同时改两条 workflow 的 -X。
const dsnVarName = "dsn"

// ldflagsWorkflows 是所有会注入 DSN 的流水线。**新增注入 DSN 的流水线要往这儿加**，
// 否则新流水线不受本门禁保护。
var ldflagsWorkflows = []string{
	"../.github/workflows/build-windows.yml",
	"../.github/workflows/release.yml",
}

// xFlagRe 抓 `-X 'some/pkg.Var=...'` 里的符号名（两条 workflow 都用单引号包 -X 的实参）。
var xFlagRe = regexp.MustCompile(`-X '([^='\s]+)=`)

// TestWorkflowLdflagsSymbolMatchesVar 钉死 workflow 的符号名 == 本包的真实符号名。
//
// 符号名不写死，而是用 reflect 从包自身取 —— 这样包挪目录、module 改名，期望值都自动跟着变，
// 只有 workflow 没跟上时才红。这正是我们要抓的那个漂移。
func TestWorkflowLdflagsSymbolMatchesVar(t *testing.T) {
	wantSymbol := reflect.TypeOf(Options{}).PkgPath() + "." + dsnVarName

	for _, wf := range ldflagsWorkflows {
		t.Run(filepath.Base(wf), func(t *testing.T) {
			data, err := os.ReadFile(wf)
			if err != nil {
				t.Fatalf("读不到 %s: %v —— 流水线被挪走或改名了？"+
					"那 DSN 注入也就跟着没了，本门禁的清单需要同步更新", wf, err)
			}
			text := string(data)

			if !strings.Contains(text, "-X '"+wantSymbol+"=") {
				var found []string
				for _, m := range xFlagRe.FindAllStringSubmatch(text, -1) {
					found = append(found, m[1])
				}
				t.Errorf("%s 里没有注入 %s。\n"+
					"该文件实际的 -X 符号：%v\n"+
					"`-X` 对写错的符号是静默 no-op —— 构建照样成功，但 DSN 永远是空串，"+
					"正式版遥测全灭，且症状与「secret 没配」无法区分。",
					wf, wantSymbol, found)
			}
		})
	}
}

// TestDSNVarStaysLdflagsInjectable 钉死 dsn 变量本身还满足 `-X` 的注入条件。
//
// `-X` 只对**用常量字面量初始化的包级 string 变量**生效。下面每一种改法都会让注入静默失效，
// 而且都是有人真会去做的「改进」：
//
//	const dsn = ""              有人觉得它不变，应该是常量
//	var dsn = defaultDSN        有人想加个默认值
//	var dsn = os.Getenv("...")  有人想加个环境变量兜底
//
// 三种改法编译都过、四道门禁都绿、DSN 全部静默丢失。
//
// 顺带守一条安全线：默认值必须是空串。非空默认值 = 真实 DSN 被写进了仓库和 git 历史。
func TestDSNVarStaysLdflagsInjectable(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "telemetry.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 telemetry.go 失败: %v", err)
	}

	var found bool
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
				if n.Name != dsnVarName {
					continue
				}
				found = true

				if gd.Tok == token.CONST {
					t.Fatalf("%s 被声明成了 const —— `-X` 只能设 var，"+
						"注入会静默失效，正式版遥测全灭", dsnVarName)
				}
				if i >= len(vs.Values) {
					t.Fatalf("var %s 没有初始化表达式", dsnVarName)
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					t.Fatalf("var %s 的初始化不是常量字面量（是 %T）—— "+
						"`-X` 只对「用常量字面量初始化的 string 变量」生效，"+
						"引用其他变量或调用函数都会让注入静默失效",
						dsnVarName, vs.Values[i])
				}
				if lit.Kind != token.STRING {
					t.Fatalf("var %s 的初始化字面量不是 string（是 %v）—— `-X` 只能设 string",
						dsnVarName, lit.Kind)
				}
				if lit.Value != `""` {
					t.Errorf("var %s 的默认值是 %s，必须是空串。\n"+
						"非空默认值意味着真实 DSN 被写进了源码 —— 它会进 git 历史，"+
						"换 DSN 时改不掉。CI 从 secret 注入是唯一的路。", dsnVarName, lit.Value)
				}
			}
		}
	}

	if !found {
		t.Fatalf("telemetry.go 里找不到包级 `var %s` —— 被改名或删了。\n"+
			"两条 workflow 的 -X 仍指着这个名字，它们会静默失效。", dsnVarName)
	}
}
