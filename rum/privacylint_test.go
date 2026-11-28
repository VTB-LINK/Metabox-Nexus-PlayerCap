package rum

// 本文件是隐私红线的编译期静态 AST 门禁：
// 扫描全仓非测试 .go 文件中所有 rum.Track / rum.TrackOnce 调用点传入的属性键名
// （含直接实参、同函数内变量声明/下标赋值，以及同包辅助函数返回的 Props/map 字面量），
// 只要键名规范化小写后命中 blockedKeySubstrings 任一子串即直接报红。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const rumImportPath = "Metabox-Nexus-PlayerCap/rum"

type privacyViolation struct {
	pos string
	key string
}

func (v privacyViolation) String() string {
	return fmt.Sprintf("%s: forbidden RUM property key %q", v.pos, v.key)
}

// TestPrivacyLintNoBlockedKeysInShippedCode 扫描全仓出货源码，确保没有任何 rum.Track / rum.TrackOnce
// 调用点传入命中 blockedKeySubstrings 的违禁属性键名。
func TestPrivacyLintNoBlockedKeysInShippedCode(t *testing.T) {
	violations, scannedFiles, err := scanRepoPrivacy("..")
	if err != nil {
		t.Fatalf("扫描仓库隐私键门禁失败: %v", err)
	}
	if scannedFiles == 0 {
		t.Fatal("未扫描到任何出货 .go 文件，门禁路径配置错误")
	}
	if len(violations) > 0 {
		msgs := make([]string, len(violations))
		for i, v := range violations {
			msgs[i] = v.String()
		}
		sort.Strings(msgs)
		t.Errorf("出货代码中发现违禁 RUM 属性键（见 blockedKeySubstrings）：\n  %s", strings.Join(msgs, "\n  "))
	}
	t.Logf("已扫描 %d 个出货 .go 文件，未发现违禁 RUM 属性键", scannedFiles)
}

// TestPrivacyLintCatchesBlockedKeys 反向自证测试：对合成源码注入多种写法的违禁键必被拦下，合规键零误报。
func TestPrivacyLintCatchesBlockedKeys(t *testing.T) {
	shouldCatch := []struct {
		name    string
		src     string
		wantKey string
	}{
		{
			name: "直接实参_song_name",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func f() {
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{"song_name": "secret"})
}`,
			wantKey: "song_name",
		},
		{
			name: "直接实参_exe_path",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func f() {
	rum.Track(rum.CatLifecycle, "app_start", map[string]any{"exe_path": "C:\\a.exe"})
}`,
			wantKey: "exe_path",
		},
		{
			name: "直接实参_unknown_prior_players",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func f() {
	rum.Track(rum.CatLifecycle, "app_start", rum.Props{"unknown_prior_players": []string{"x"}})
}`,
			wantKey: "unknown_prior_players",
		},
		{
			name: "子串命中_my_referer_raw",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func f() {
	rum.TrackOnce("k", rum.CatRouteClient, "client_session", rum.Props{"my_referer_raw": "http://x"})
}`,
			wantKey: "my_referer_raw",
		},
		{
			name: "大写变体_Cover_URL",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func f() {
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{"Cover_URL": "https://img"})
}`,
			wantKey: "Cover_URL",
		},
		{
			name: "同函数短变量声明与嵌套map",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func f() {
	p := rum.Props{
		"ok": true,
		"nested": map[string]any{"artist": "singer"},
	}
	rum.Track(rum.CatPlayer, "player_session", p)
}`,
			wantKey: "artist",
		},
		{
			name: "同函数下标赋值",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func f() {
	p := rum.Props{"player": "wesing"}
	p["machine_guid"] = "guid"
	rum.TrackOnce("k", rum.CatPlayer, "player_blocked", p)
}`,
			wantKey: "machine_guid",
		},
		{
			name: "别名import与同包辅助函数返回",
			src: `package main
import r "Metabox-Nexus-PlayerCap/rum"
func buildProps() r.Props {
	return r.Props{"lyric_text": "hello"}
}
func f() {
	r.Track(r.CatLyricTrack, "track_recognized", buildProps())
}`,
			wantKey: "lyric_text",
		},
		{
			name: "字符串字面量拼接键名",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func f() {
	rum.Track(rum.CatPlayer, "player_session", rum.Props{"libcef" + "_path": "x"})
}`,
			wantKey: "libcef_path",
		},
		{
			name: "嵌套属性值引用局部变量",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func f() {
	nested := map[string]any{"song_name": "secret"}
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{"nested": nested})
}`,
			wantKey: "song_name",
		},
		{
			name: "嵌套切片元素引用局部变量",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func f() {
	item := rum.Props{"artist": "secret"}
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{"items": []any{item}})
}`,
			wantKey: "artist",
		},
		{
			name: "嵌套属性值引用同包辅助函数返回",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func buildNested() map[string]any {
	return map[string]any{"album": "secret"}
}
func f() {
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{"nested": buildNested()})
}`,
			wantKey: "album",
		},
		{
			name: "下标赋值右侧引用局部变量与辅助函数",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func buildMeta() map[string]any {
	return map[string]any{"raw_url": "https://example.com"}
}
func f() {
	p := rum.Props{"ok": true}
	p["meta"] = buildMeta()
	rum.Track(rum.CatRouteClient, "client_session", p)
}`,
			wantKey: "raw_url",
		},
		{
			name: "局部变量互引用不死循环且捕获违禁键",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func f() {
	a := map[string]any{"ok": true}
	b := map[string]any{"a": a, "exe_path": "C:\\a.exe"}
	a["b"] = b
	rum.Track(rum.CatLifecycle, "app_start", a)
}`,
			wantKey: "exe_path",
		},
		{
			name: "透传函数实参中的违禁map字面量",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func identity(v any) any { return v }
func f() {
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{"nested": identity(map[string]any{"song_name": "secret"})})
}`,
			wantKey: "song_name",
		},
		{
			name: "透传函数实参中的违禁局部变量",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func identity(v any) any { return v }
func f() {
	nested := map[string]any{"artist": "secret"}
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{"nested": identity(nested)})
}`,
			wantKey: "artist",
		},
		{
			name: "下标赋值右侧透传函数实参",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func identity(v any) any { return v }
func f() {
	p := rum.Props{"ok": true}
	p["nested"] = identity(map[string]any{"exe_path": "C:\\a.exe"})
	rum.Track(rum.CatLifecycle, "app_start", p)
}`,
			wantKey: "exe_path",
		},
		{
			name: "外部包函数包装实参中的违禁map",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func f() {
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{"nested": ext.Wrap(map[string]any{"cover_url": "https://img"})})
}`,
			wantKey: "cover_url",
		},
		{
			name: "立即调用闭包返回违禁键",
			src: `package main
import "Metabox-Nexus-PlayerCap/rum"
func f() {
	rum.Track(rum.CatLyricTrack, "track_recognized", rum.Props{"nested": func() map[string]any { return map[string]any{"raw_lyric": "abc"} }()})
}`,
			wantKey: "raw_lyric",
		},
	}

	for _, tc := range shouldCatch {
		t.Run("catch/"+tc.name, func(t *testing.T) {
			violations, err := scanSourcesPrivacy(map[string]string{"snippet.go": tc.src})
			if err != nil {
				t.Fatalf("解析合成源码失败: %v", err)
			}
			if len(violations) == 0 {
				t.Fatalf("门禁漏报违禁键 %q：\n%s", tc.wantKey, tc.src)
			}
			found := false
			for _, v := range violations {
				if v.key == tc.wantKey {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("期望捕获键 %q，实际捕获 %v", tc.wantKey, violations)
			}
		})
	}

	shouldPass := `package main
import "Metabox-Nexus-PlayerCap/rum"
func buildSafeProps() rum.Props {
	p := rum.Props{
		"unknown_prior_player_count": 1,
		"prior_players":              []string{"wesing"},
		"is_elevated":                true,
		"os_version":                 "10.0.22631",
		"outcome":                    "ok",
	}
	p["gateway_ping_ok"] = false
	return p
}
func f() {
	rum.Track(rum.CatLifecycle, "app_start", buildSafeProps())
	rum.TrackOnce("k", rum.CatUpdateCfg, "config_lifecycle", rum.Props{
		"file_state": "loaded",
		"has_cwd_conflict": false,
	})
}
`
	t.Run("pass/合规键零误报", func(t *testing.T) {
		violations, err := scanSourcesPrivacy(map[string]string{"safe.go": shouldPass})
		if err != nil {
			t.Fatalf("解析合成源码失败: %v", err)
		}
		if len(violations) > 0 {
			t.Errorf("合规代码被误报：%v", violations)
		}
	})

	cycleSafe := `package main
import "Metabox-Nexus-PlayerCap/rum"
func buildMeta() map[string]any {
	return map[string]any{"kind": "ok"}
}
func f() {
	a := map[string]any{"kind": "a", "meta": buildMeta()}
	b := map[string]any{"a": a}
	a["b"] = b
	rum.Track(rum.CatLifecycle, "app_start", a)
}
`
	t.Run("pass/合规互引用不死循环且零误报", func(t *testing.T) {
		violations, err := scanSourcesPrivacy(map[string]string{"cycle_safe.go": cycleSafe})
		if err != nil {
			t.Fatalf("解析合成源码失败: %v", err)
		}
		if len(violations) > 0 {
			t.Errorf("合规互引用代码被误报：%v", violations)
		}
	})
}

func scanRepoPrivacy(root string) ([]privacyViolation, int, error) {
	pkgs := map[string][]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || name == "tools" || name == "testdata" || name == "build") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		dir := filepath.Dir(p)
		pkgs[dir] = append(pkgs[dir], p)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	var all []privacyViolation
	scanned := 0
	for _, files := range pkgs {
		fset := token.NewFileSet()
		parsed := make([]*ast.File, 0, len(files))
		for _, p := range files {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				rel = p
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return nil, scanned, err
			}
			f, err := parser.ParseFile(fset, filepath.ToSlash(rel), src, parser.SkipObjectResolution)
			if err != nil {
				return nil, scanned, err
			}
			parsed = append(parsed, f)
			scanned++
		}
		all = append(all, lintPackageFiles(fset, parsed)...)
	}
	return all, scanned, nil
}

func scanSourcesPrivacy(sources map[string]string) ([]privacyViolation, error) {
	fset := token.NewFileSet()
	parsed := make([]*ast.File, 0, len(sources))
	for name, src := range sources {
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, f)
	}
	return lintPackageFiles(fset, parsed), nil
}

func lintPackageFiles(fset *token.FileSet, files []*ast.File) []privacyViolation {
	pkgFuncs := map[string]*ast.FuncDecl{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Body != nil {
				pkgFuncs[fd.Name.Name] = fd
			}
		}
	}

	var violations []privacyViolation
	for _, f := range files {
		rumLocal := rumLocalImportName(f)
		inRumPkg := f.Name.Name == "rum"
		if rumLocal == "" && !inRumPkg {
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			lintBlock(fset, fd.Body, rumLocal, inRumPkg, pkgFuncs, &violations)
		}
	}
	return violations
}

func rumLocalImportName(f *ast.File) string {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != rumImportPath {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				return ""
			}
			return imp.Name.Name
		}
		return "rum"
	}
	return ""
}

func lintBlock(fset *token.FileSet, scope *ast.BlockStmt, rumLocal string, inRumPkg bool, pkgFuncs map[string]*ast.FuncDecl, out *[]privacyViolation) {
	var stack []*ast.BlockStmt
	stack = append(stack, scope)

	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		if n == nil {
			return
		}
		if fl, ok := n.(*ast.FuncLit); ok && fl.Body != nil {
			stack = append(stack, fl.Body)
			ast.Inspect(fl.Body, func(child ast.Node) bool {
				if child == fl.Body {
					return true
				}
				if innerLit, isLit := child.(*ast.FuncLit); isLit {
					walk(innerLit)
					return false
				}
				if call, isCall := child.(*ast.CallExpr); isCall {
					checkTrackCall(fset, call, stack, rumLocal, inRumPkg, pkgFuncs, out)
				}
				return true
			})
			stack = stack[:len(stack)-1]
			return
		}
		ast.Inspect(n, func(child ast.Node) bool {
			if innerLit, isLit := child.(*ast.FuncLit); isLit {
				walk(innerLit)
				return false
			}
			if call, isCall := child.(*ast.CallExpr); isCall {
				checkTrackCall(fset, call, stack, rumLocal, inRumPkg, pkgFuncs, out)
			}
			return true
		})
	}
	walk(scope)
}

type varKey struct {
	scope *ast.BlockStmt
	name  string
}

func checkTrackCall(fset *token.FileSet, call *ast.CallExpr, scopes []*ast.BlockStmt, rumLocal string, inRumPkg bool, pkgFuncs map[string]*ast.FuncDecl, out *[]privacyViolation) {
	propsIdx := trackPropsArgIndex(call, rumLocal, inRumPkg)
	if propsIdx < 0 || propsIdx >= len(call.Args) {
		return
	}
	visitedFuncs := map[string]bool{}
	visitedVars := map[varKey]bool{}
	inspectPropsExpr(fset, call.Args[propsIdx], scopes, pkgFuncs, visitedFuncs, visitedVars, out)
}

func trackPropsArgIndex(call *ast.CallExpr, rumLocal string, inRumPkg bool) int {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		id, ok := fn.X.(*ast.Ident)
		if !ok || rumLocal == "" || id.Name != rumLocal {
			return -1
		}
		switch fn.Sel.Name {
		case "Track":
			return 2
		case "TrackOnce":
			return 3
		}
	case *ast.Ident:
		if !inRumPkg {
			return -1
		}
		switch fn.Name {
		case "Track":
			return 2
		case "TrackOnce":
			return 3
		}
	}
	return -1
}

func inspectPropsExpr(fset *token.FileSet, expr ast.Expr, scopes []*ast.BlockStmt, pkgFuncs map[string]*ast.FuncDecl, visitedFuncs map[string]bool, visitedVars map[varKey]bool, out *[]privacyViolation) {
	if expr == nil {
		return
	}
	switch e := expr.(type) {
	case *ast.ParenExpr:
		inspectPropsExpr(fset, e.X, scopes, pkgFuncs, visitedFuncs, visitedVars, out)
	case *ast.UnaryExpr:
		inspectPropsExpr(fset, e.X, scopes, pkgFuncs, visitedFuncs, visitedVars, out)
	case *ast.CompositeLit:
		inspectCompositeLitKeys(fset, e, scopes, pkgFuncs, visitedFuncs, visitedVars, out)
	case *ast.Ident:
		inspectVarInScopes(fset, e.Name, scopes, pkgFuncs, visitedFuncs, visitedVars, out)
	case *ast.CallExpr:
		switch fn := e.Fun.(type) {
		case *ast.Ident:
			if fd, exists := pkgFuncs[fn.Name]; exists && !visitedFuncs[fn.Name] {
				visitedFuncs[fn.Name] = true
				inspectBlockReturns(fset, fd.Body, []*ast.BlockStmt{fd.Body}, pkgFuncs, visitedFuncs, visitedVars, out)
			}
		case *ast.FuncLit:
			if fn.Body != nil {
				litScopes := append(append([]*ast.BlockStmt(nil), scopes...), fn.Body)
				inspectBlockReturns(fset, fn.Body, litScopes, pkgFuncs, visitedFuncs, visitedVars, out)
			}
		}
		for _, arg := range e.Args {
			inspectPropsExpr(fset, arg, scopes, pkgFuncs, visitedFuncs, visitedVars, out)
		}
	default:
		ast.Inspect(expr, func(n ast.Node) bool {
			if lit, ok := n.(*ast.CompositeLit); ok {
				inspectCompositeLitKeys(fset, lit, scopes, pkgFuncs, visitedFuncs, visitedVars, out)
				return false
			}
			return true
		})
	}
}

func inspectBlockReturns(fset *token.FileSet, body *ast.BlockStmt, scopes []*ast.BlockStmt, pkgFuncs map[string]*ast.FuncDecl, visitedFuncs map[string]bool, visitedVars map[varKey]bool, out *[]privacyViolation) {
	if body == nil {
		return
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if _, isLit := n.(*ast.FuncLit); isLit {
			return false
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, res := range ret.Results {
			inspectPropsExpr(fset, res, scopes, pkgFuncs, visitedFuncs, visitedVars, out)
		}
		return true
	})
}

func inspectVarInScopes(fset *token.FileSet, varName string, scopes []*ast.BlockStmt, pkgFuncs map[string]*ast.FuncDecl, visitedFuncs map[string]bool, visitedVars map[varKey]bool, out *[]privacyViolation) {
	seenExpr := map[ast.Expr]bool{}
	for i := len(scopes) - 1; i >= 0; i-- {
		vk := varKey{scope: scopes[i], name: varName}
		if visitedVars[vk] {
			continue
		}
		visitedVars[vk] = true
		ast.Inspect(scopes[i], func(n ast.Node) bool {
			switch st := n.(type) {
			case *ast.AssignStmt:
				for idx, lhs := range st.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == varName && idx < len(st.Rhs) {
						rhs := st.Rhs[idx]
						if !seenExpr[rhs] {
							seenExpr[rhs] = true
							inspectPropsExpr(fset, rhs, scopes[:i+1], pkgFuncs, visitedFuncs, visitedVars, out)
						}
					}
					if ie, ok := lhs.(*ast.IndexExpr); ok {
						if id, ok := ie.X.(*ast.Ident); ok && id.Name == varName {
							if key, ok := evalStringLit(ie.Index); ok && isBlockedKey(key) {
								*out = append(*out, privacyViolation{pos: fset.Position(ie.Index.Pos()).String(), key: key})
							}
							if idx < len(st.Rhs) {
								inspectPropsExpr(fset, st.Rhs[idx], scopes[:i+1], pkgFuncs, visitedFuncs, visitedVars, out)
							}
						}
					}
				}
			case *ast.ValueSpec:
				for idx, name := range st.Names {
					if name.Name == varName && idx < len(st.Values) {
						val := st.Values[idx]
						if !seenExpr[val] {
							seenExpr[val] = true
							inspectPropsExpr(fset, val, scopes[:i+1], pkgFuncs, visitedFuncs, visitedVars, out)
						}
					}
				}
			}
			return true
		})
	}
}

func inspectCompositeLitKeys(fset *token.FileSet, lit *ast.CompositeLit, scopes []*ast.BlockStmt, pkgFuncs map[string]*ast.FuncDecl, visitedFuncs map[string]bool, visitedVars map[varKey]bool, out *[]privacyViolation) {
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if key, ok := evalStringLit(kv.Key); ok && isBlockedKey(key) {
				*out = append(*out, privacyViolation{pos: fset.Position(kv.Key.Pos()).String(), key: key})
			}
			inspectPropsExpr(fset, kv.Value, scopes, pkgFuncs, visitedFuncs, visitedVars, out)
			continue
		}
		inspectPropsExpr(fset, elt, scopes, pkgFuncs, visitedFuncs, visitedVars, out)
	}
}

func evalStringLit(e ast.Expr) (string, bool) {
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
		a, okA := evalStringLit(v.X)
		b, okB := evalStringLit(v.Y)
		return a + b, okA && okB
	case *ast.ParenExpr:
		return evalStringLit(v.X)
	}
	return "", false
}
