package player

// 本文件只测一件事：wesing 的 LoadLyrics 必须对读到的歌词 time 做合理性守卫（issue #46）。
// 它不测业务逻辑——扫的是 player/wesing/lyric/reader.go 的源码。
//
// # 为什么用 AST 门禁而不是单元测试
//
// LoadLyrics 逐字节读 WeSing 进程内存（syscall.Handle），无法在无进程的环境里单测；
// 而 wesing 包 CI 永远跑不了（AGENTS.md §8：GOOS=linux 编译 FAIL）。AST 扫描不编译目标
// 平台，所以这是这条守卫唯一能自动化的守法。运行时行为另由真机变异自证（删守卫→乱码复现）。
//
// # 它守的是什么
//
// LoadLyrics 按 (endPtr-beginPtr)/4 估算行数逐个读 entry。当 end 指针指向的位置超出真实
// 行数时，尾部 entry 是堆残留——把它的字节当 float 解读常得到荒谬值（issue #46 真机实测
// timestamp=1.5e18），连同乱码文本被当成一行歌词 append、推上对外 API，1.5e18 会把前端
// 插值直接带飞。原有的「归零/回退」两道时间闸只挡 <=0 与回退，挡不住暴涨。
//
// 守卫复用 IsPlausiblePlayTime（接受式 [0,100000)、拒 NaN/Inf，已由 plausibleplaytime_test
// 严密覆盖）。本门禁只保证「这道守卫还在 LoadLyrics 里」——把它删掉就红。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadLyricsHasTimeGuard 全仓强制：wesing 的 LoadLyrics 必须调用 IsPlausiblePlayTime。
//
// 变异自证：删掉 reader.go 里 `if !IsPlausiblePlayTime(timeVal) { break }` 即红。
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
	if !loadLyricsGuarded(t, string(src)) {
		t.Errorf("player/wesing/lyric/reader.go 的 LoadLyrics 缺少 time 合理性守卫。\n" +
			"越界的堆残留（issue #46 实测 time=1.5e18）会被当成一行歌词推上对外 API。\n" +
			"修法：读到 timeVal 后立刻 `if !IsPlausiblePlayTime(timeVal) { break }`——" +
			"IsPlausiblePlayTime 已在 plausibleplaytime_test 覆盖，直接复用。")
	}
}

// TestReaderLintCatchesKnownHoles 钉死门禁自身能抓住目标形状。
//
// 一个抓不住目标形状的门禁比没有门禁更糟——它给「已经守住了」的错觉。
func TestReaderLintCatchesKnownHoles(t *testing.T) {
	// 无守卫：issue #46 修复前的原样——只有归零/回退两道闸，挡不住暴涨。
	const noGuard = `package lyric
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
}`
	if loadLyricsGuarded(t, noGuard) {
		t.Error("门禁漏报了「LoadLyrics 无 IsPlausiblePlayTime 守卫」——它正是要堵的")
	}

	// 守卫在别的函数里、LoadLyrics 自己没有：不该算通过。
	const guardElsewhere = `package lyric
func other() { _ = IsPlausiblePlayTime(1) }
func LoadLyrics() {
	for {
		timeVal := readFloat()
		lyrics = append(lyrics, timeVal)
	}
}`
	if loadLyricsGuarded(t, guardElsewhere) {
		t.Error("门禁被骗：守卫在别的函数里，LoadLyrics 自己没有守卫")
	}

	// 有守卫（修复后的样子）：应通过。
	const guarded = `package lyric
func LoadLyrics() {
	for {
		timeVal := readFloat()
		if !IsPlausiblePlayTime(timeVal) {
			break
		}
		lyrics = append(lyrics, timeVal)
	}
}`
	if !loadLyricsGuarded(t, guarded) {
		t.Error("门禁误报：LoadLyrics 明明有 IsPlausiblePlayTime 守卫")
	}
}

// loadLyricsGuarded 判断源码里的 LoadLyrics 函数体内是否调用了 IsPlausiblePlayTime。
func loadLyricsGuarded(t *testing.T, src string) bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "reader.go", src, 0)
	if err != nil {
		t.Fatalf("解析源码失败: %v", err)
	}
	guarded := false
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "LoadLyrics" || fn.Body == nil {
			return true
		}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			// 含 player. 限定与包内裸名两种写法
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				if fun.Name == "IsPlausiblePlayTime" {
					guarded = true
				}
			case *ast.SelectorExpr:
				if fun.Sel.Name == "IsPlausiblePlayTime" {
					guarded = true
				}
			}
			return true
		})
		return false // 找到 LoadLyrics 就不再往下钻
	})
	return guarded
}
