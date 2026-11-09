package player

// 本文件只测一件事：wesing 逐行读主歌词时，行起始时间必须过 IsPlausiblePlayTime 这道守卫，不合理的
// 那一行不收（issue #46）。它不测业务逻辑——扫的是 player/wesing/lyric/reader.go 的源码。
//
// # 为什么还要 AST 门禁
//
// 逐行读取的实现自 issue #85 起接受假内存（loadLyrics），行为由 lyric 包的 lyricvector_test.go 离线测。
// 但 lyric 包 import 了 Windows 专用的 proc，GOOS=linux 编译不过（AGENTS.md §8）；本门禁在 player 包里，
// 只解析源码、不编译目标平台。两者分工：单元测试守行为，本门禁守「守卫还在逐行读取的那个函数里」。
//
// # 它守的是什么
//
// 读行向量越过真实行数时，尾部 entry 是堆残留——把它的字节当 float 解读常得到荒谬值（issue #46 真机
// 实测 timestamp=1.5e18），连同乱码文本被当成一行歌词 append、推上对外 API，1.5e18 会把前端插值直接
// 带飞。原有的「归零/回退」两道时间闸只挡 <=0 与回退，挡不住暴涨。issue #85 起 end 改读 +0x4C，残留项
// 不再被读到，这道守卫留作兜底。
//
// LoadLyrics 只是把进程句柄包成 memory 转给 loadLyrics，逐行读在后者。门禁从 LoadLyrics 出发，沿同一
// 文件里的函数调用（直接或间接）往下找，要求其中某个函数里有这样一条 if：条件调用了 IsPlausiblePlayTime，
// 分支里有 break / continue / return——行时间不合理，这一行就不收。找不到 LoadLyrics 也算没守住。
//
// 只看「调用了 IsPlausiblePlayTime」不够：逐字判定也调用它，但只把 wordsOK 置假、照样收下这一行。
// issue #85 之前的门禁只看调用，删掉行级守卫后靠逐字那处调用照样绿（2026-11-02 实测）。
//
// # 它不守什么
//
// 只认形状：哪个变量被传给 IsPlausiblePlayTime 不查。逐字那处若也写成分支里带 break / continue 的 if，
// 删掉行级守卫它看不出——那时靠 lyricvector_test.go 的行为用例。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadLyricsHasTimeGuard 全仓强制：LoadLyrics 逐行读主歌词时，行时间要过 IsPlausiblePlayTime 守卫。
//
// 变异自证：删掉 reader.go 里 `if !IsPlausiblePlayTime(timeVal) { break }` 即红（逐字那处的调用还在）。
func TestLoadLyricsHasTimeGuard(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	path := filepath.Join(root, "player", "wesing", "lyric", "reader.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 reader.go 失败: %v", err)
	}
	if !lineTimeGuarded(t, string(src)) {
		t.Errorf("player/wesing/lyric/reader.go：从 LoadLyrics 出发找不到行时间的合理性守卫。\n" +
			"越界的堆残留（issue #46 实测 time=1.5e18）会被当成一行歌词推上对外 API。\n" +
			"修法：在逐行读取的函数里，读到行起始 timeVal 后立刻 `if !IsPlausiblePlayTime(timeVal) { break }`——" +
			"IsPlausiblePlayTime 已在 plausibleplaytime_test 覆盖，直接复用。\n" +
			"若只是改了函数名或把逐行读取挪了位置，同步本门禁。")
	}
}

// TestReaderLintCatchesKnownHoles 钉死门禁自身能抓住目标形状。
//
// 一个抓不住目标形状的门禁比没有门禁更糟——它给「已经守住了」的错觉。
//
// 变异自证：把 lineTimeGuarded 改回只看「调用了 IsPlausiblePlayTime」，「只有逐字判定」「裸调用」两例红；
// 不沿调用往下找、只看 LoadLyrics 本身，「转给内部函数」一例红；不看是否从 LoadLyrics 调到、扫文件里
// 所有函数，「守卫在别的函数里」「没有 LoadLyrics」两例红。
func TestReaderLintCatchesKnownHoles(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		// issue #46 修复前的原样——只有归零/回退两道闸，挡不住暴涨。
		{"无守卫", `package lyric
func LoadLyrics() {
	for {
		timeVal := readFloat()
		if len(lyrics) > 0 && timeVal <= 0 {
			break
		}
		if len(lyrics) > 0 && timeVal < prev-1 {
			break
		}
		lyrics = append(lyrics, timeVal)
	}
}`, false},
		// 守卫在别的函数里，LoadLyrics 调不到它。
		{"守卫在别的函数里", `package lyric
func other(v float32) {
	for {
		if !IsPlausiblePlayTime(v) {
			break
		}
	}
}
func LoadLyrics() { loadLyrics() }
func loadLyrics() {
	for {
		timeVal := readFloat()
		lyrics = append(lyrics, timeVal)
	}
}`, false},
		// issue #85 之前门禁的盲区：行级守卫删了，逐字判定里的调用还在，但它不让这一行不收。
		{"只有逐字判定", `package lyric
func LoadLyrics() { loadLyrics() }
func loadLyrics() {
	for {
		timeVal := readFloat()
		for {
			wStart := readFloat()
			if !ok1 || !IsPlausiblePlayTime(wStart) {
				wordsOK = false
			}
		}
		lyrics = append(lyrics, timeVal)
	}
}`, false},
		{"裸调用", `package lyric
func LoadLyrics() {
	for {
		timeVal := readFloat()
		_ = IsPlausiblePlayTime(timeVal)
		lyrics = append(lyrics, timeVal)
	}
}`, false},
		// 改了名、逐行读取的函数还在：找不到 LoadLyrics 就报，不去猜哪个函数是它。
		{"没有 LoadLyrics", `package lyric
func loadLyrics() {
	for {
		timeVal := readFloat()
		if !IsPlausiblePlayTime(timeVal) {
			break
		}
		lyrics = append(lyrics, timeVal)
	}
}`, false},
		// 现行写法：LoadLyrics 转给内部函数，守卫在内部函数里。
		{"转给内部函数", `package lyric
func LoadLyrics(h Handle, addr uint32) ([]LyricLine, error) {
	return loadLyrics(procMemory{h}, addr)
}
func loadLyrics(m memory, addr uint32) ([]LyricLine, error) {
	for {
		timeVal, ok := m.f32(addr)
		if !ok {
			continue
		}
		if !IsPlausiblePlayTime(timeVal) {
			break
		}
		lyrics = append(lyrics, timeVal)
	}
}`, true},
		// issue #46 修复时的写法：守卫直接在 LoadLyrics 里，带包名调用也认。
		{"守卫在 LoadLyrics 里", `package lyric
func LoadLyrics() {
	for {
		timeVal := readFloat()
		if !lyric.IsPlausiblePlayTime(timeVal) {
			return
		}
		lyrics = append(lyrics, timeVal)
	}
}`, true},
	}
	for _, c := range cases {
		if got := lineTimeGuarded(t, c.src); got != c.want {
			t.Errorf("%s：lineTimeGuarded = %v，want %v", c.name, got, c.want)
		}
	}
}

// lineTimeGuarded 从 LoadLyrics 出发，沿同一文件里的函数调用（直接或间接）往下找，报告其中有没有一个
// 函数含行时间守卫：条件调用了 IsPlausiblePlayTime、分支里有 break / continue / return 的 if。
func lineTimeGuarded(t *testing.T, src string) bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "reader.go", src, 0)
	if err != nil {
		t.Fatalf("解析源码失败: %v", err)
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Body != nil {
			funcs[fn.Name.Name] = fn
		}
	}
	seen := map[string]bool{}
	queue := []string{"LoadLyrics"}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		fn := funcs[name]
		if fn == nil || seen[name] {
			continue
		}
		seen[name] = true
		guarded := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.IfStmt:
				if callsPlausible(n.Cond) && dropsLine(n.Body) {
					guarded = true
				}
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok {
					queue = append(queue, id.Name)
				}
			}
			return true
		})
		if guarded {
			return true
		}
	}
	return false
}

// callsPlausible 报告表达式里有没有调用 IsPlausiblePlayTime（包内裸名与带包名两种写法）。
func callsPlausible(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			found = found || fun.Name == "IsPlausiblePlayTime"
		case *ast.SelectorExpr:
			found = found || fun.Sel.Name == "IsPlausiblePlayTime"
		}
		return true
	})
	return found
}

// dropsLine 报告 if 分支里有没有让这一行不收的语句：break、continue 或 return。
func dropsLine(body *ast.BlockStmt) bool {
	for _, s := range body.List {
		switch s := s.(type) {
		case *ast.ReturnStmt:
			return true
		case *ast.BranchStmt:
			if s.Tok == token.BREAK || s.Tok == token.CONTINUE {
				return true
			}
		}
	}
	return false
}
