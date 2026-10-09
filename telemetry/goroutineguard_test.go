package telemetry

// 本文件只测一件事：**出货代码里每个 goroutine 的入口都挂了 `defer telemetry.Guard()`**。
// Guard 的调用形状（必须 defer 直接调用）由 guarddirect_test.go 管，本文件只管「挂没挂」。
//
// # 为什么要钉
//
// 漏挂 Guard 不改变任何行为：panic 照常向上跑、进程照常死。唯一的差别是这次崩溃不进 Sentry，
// 而它只发生在主播的机器上，我们这头永远看不见。编译器、vet、单元测试都不会因此变红。
//
// # 判据
//
// 扫描范围：根包（main.go 所在目录，不递归）、server/、player/ 下全部非测试 .go 文件。
// tools/ 不出货；audiocap/ 是音频子系统，主进程侧按设计本地 recover、不挂 Guard（见下），
// helper 子进程内照常挂，不由本门禁管。其余包不在范围内：
// config / i18n / logger 是 telemetry 的依赖，import 它会成环，规矩对它们不成立。
//
// 对每个 go 语句：
//   - go func(...){...}(...)：函数字面量体的前导 defer 段里有 Guard 或音频豁免。
//   - go f(...) / go x.m(...)：同包内按名字找到的每个同名声明，函数体同上。找不到即违规——
//     外部包函数、函数变量都属此类，没有地方挂 Guard，须改为显式传参的函数字面量。
//   - 其余形状一律违规。
//
// 「前导 defer 段」指函数体首句起连续的 defer 语句。取它而不取严格首句：段内的 defer 都在任何
// 非 defer 语句之前注册，覆盖的 panic 范围相同；而 server/audio.go 的写 goroutine 是
// `defer close(done)` 在前、`defer audioRecover(...)` 在后，严格首句会逼着改音频代码的 defer 顺序。
// 代价：段内排在 Guard 之前的 defer 若在求值实参时 panic，那一次不被捕获。新代码一律把 Guard
// 写成首句，它最外层注册、最后执行，原有 defer 的执行顺序不受影响。
//
// 已知盲区：方法按名字匹配、不做类型检查。同包内多个类型有同名方法时要求全部合规（偏严）；
// 局部函数变量与同包函数同名时会误认成那个函数（偏松，现无此情形）。
//
// # 音频豁免
//
// `defer localRecover(...)`（main.go）与 `defer audioRecover(...)`（server/audio.go）视同合规。
// 音频波形是可降级的附加能力，其 goroutine 本地 recover、绝不挂 Guard：Guard 会 re-panic，
// 为附加能力杀掉主进程就是直播事故。豁免只认这两个名字，不认任意 recover 函数。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// audioRecoverExempt 是音频子系统本地 recover 函数的名字，见文件头「音频豁免」。
var audioRecoverExempt = map[string]bool{"localRecover": true, "audioRecover": true}

// goEntryDecl 是同包内一个可作为 goroutine 入口的函数/方法声明。
type goEntryDecl struct {
	body  *ast.BlockStmt
	local string // 声明所在文件里 telemetry 的本地名，未 import 为 ""
	pos   token.Position
}

// TestEveryGoroutineGuarded 扫出货代码，钉死每个 goroutine 入口都挂 Guard。
//
// 变异自证：删掉任一 player 封面 goroutine 的 Guard、删掉 effect.go 里 shimInjectLoop 首句的
// Guard、或新加一个裸 `go func(){}()`，均红。
func TestEveryGoroutineGuarded(t *testing.T) {
	pkgPath := reflect.TypeOf(Options{}).PkgPath()

	// 目录即包：目录 → 该目录下的非测试文件
	pkgs := map[string][]string{}
	entries, err := os.ReadDir("..")
	if err != nil {
		t.Fatalf("读取仓库根目录失败: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && isShippedGoFile(e.Name()) {
			pkgs[".."] = append(pkgs[".."], filepath.Join("..", e.Name()))
		}
	}
	for _, dir := range []string{"server", "player"} {
		err := filepath.WalkDir(filepath.Join("..", dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if isShippedGoFile(d.Name()) {
				pkgs[filepath.Dir(p)] = append(pkgs[filepath.Dir(p)], p)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("遍历 %s 失败: %v", dir, err)
		}
	}

	var violations []string
	var goStmts, exemptHits int
	fset := token.NewFileSet()

	for _, files := range pkgs {
		parsed := make([]*ast.File, 0, len(files))
		funcs := map[string][]goEntryDecl{} // 函数名 → 声明
		methods := map[string][]goEntryDecl{}
		for _, p := range files {
			// 解析失败即红：跳过的话，那个文件里的 go 语句就漏扫了
			f, err := parser.ParseFile(fset, p, nil, 0)
			if err != nil {
				t.Fatalf("解析 %s 失败: %v", p, err)
			}
			parsed = append(parsed, f)
			local := localNameOf(f, pkgPath)
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok {
					continue
				}
				decl := goEntryDecl{fd.Body, local, fset.Position(fd.Pos())}
				if fd.Recv == nil {
					funcs[fd.Name.Name] = append(funcs[fd.Name.Name], decl)
				} else {
					methods[fd.Name.Name] = append(methods[fd.Name.Name], decl)
				}
			}
		}

		for _, f := range parsed {
			local := localNameOf(f, pkgPath)
			imports := importNamesOf(f)
			ast.Inspect(f, func(n ast.Node) bool {
				gs, ok := n.(*ast.GoStmt)
				if !ok {
					return true
				}
				goStmts++
				at := fmt.Sprintf("%s:%d", filepath.ToSlash(fset.Position(gs.Pos()).Filename), fset.Position(gs.Pos()).Line)

				var decls []goEntryDecl
				switch fn := gs.Call.Fun.(type) {
				case *ast.FuncLit:
					decls = []goEntryDecl{{fn.Body, local, fset.Position(fn.Pos())}}
				case *ast.Ident:
					decls = funcs[fn.Name]
				case *ast.SelectorExpr:
					if x, ok := fn.X.(*ast.Ident); ok && imports[x.Name] {
						violations = append(violations, at+"：go 调用外部包函数 "+x.Name+"."+fn.Sel.Name+
							"，无处挂 Guard，须改为显式传参的函数字面量")
						return true
					}
					decls = methods[fn.Sel.Name]
				default:
					violations = append(violations, at+"：go 语句形状无法静态判定入口，须改为函数字面量")
					return true
				}
				if len(decls) == 0 {
					violations = append(violations, at+"：同包内找不到 goroutine 入口的声明（函数变量？），须改为函数字面量")
					return true
				}
				for _, d := range decls {
					switch entryGuard(d.body, d.local) {
					case entryGuarded:
					case entryAudioExempt:
						exemptHits++
					default:
						where := ""
						if _, lit := gs.Call.Fun.(*ast.FuncLit); !lit {
							where = fmt.Sprintf("（入口声明 %s:%d）", filepath.ToSlash(d.pos.Filename), d.pos.Line)
						}
						violations = append(violations, at+"：goroutine 入口的前导 defer 段没有 defer "+
							path.Base(pkgPath)+".Guard()"+where)
					}
				}
				return true
			})
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("下列 goroutine 没有挂 %s.Guard：\n  %s\n\n"+
			"漏挂不改变任何行为（panic 照常杀进程），只是这次崩溃不进 Sentry。"+
			"在入口函数体首句加 `defer telemetry.Guard()`；音频子系统用 localRecover/audioRecover 豁免。",
			pkgPath, strings.Join(violations, "\n  "))
	}

	// 反向自证：一个 go 语句都没扫到，说明扫描范围或匹配逻辑坏了，门禁在空转。
	if goStmts == 0 {
		t.Errorf("出货代码里没有扫到任何 go 语句（%d 个包）—— 扫描范围或匹配逻辑坏了", len(pkgs))
	}
	t.Logf("扫过 %d 个包、%d 个 go 语句，其中 %d 处为音频本地 recover 豁免", len(pkgs), goStmts, exemptHits)
}

// isShippedGoFile 判断文件名是否为参与出货构建的 Go 源文件（排除测试文件）。
func isShippedGoFile(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

// importNamesOf 返回文件 f 里所有 import 的本地名（未起别名时取路径末段）。
func importNamesOf(f *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if imp.Name != nil {
			names[imp.Name.Name] = true
		} else {
			names[path.Base(p)] = true
		}
	}
	return names
}

const (
	entryUnguarded = iota
	entryGuarded
	entryAudioExempt
)

// entryGuard 检查函数体的前导 defer 段（首句起连续的 defer 语句）：
// 有 defer <local>.Guard() 返回 entryGuarded，有音频豁免返回 entryAudioExempt，否则 entryUnguarded。
func entryGuard(body *ast.BlockStmt, local string) int {
	if body == nil {
		return entryUnguarded
	}
	for _, st := range body.List {
		ds, ok := st.(*ast.DeferStmt)
		if !ok {
			break
		}
		if local != "" && isGuardSelector(ds.Call.Fun, local) {
			return entryGuarded
		}
		if id, ok := ds.Call.Fun.(*ast.Ident); ok && audioRecoverExempt[id.Name] {
			return entryAudioExempt
		}
	}
	return entryUnguarded
}
