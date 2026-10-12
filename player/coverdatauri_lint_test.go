package player

// 本文件只测一件事：AST 门禁「cover_base64 一律经 player.CoverDataURI 生成」（issue #101）。
//
// # 为什么要门禁
//
// 封面统一为 800×800 挂在 CoverDataURI 上（FetchCoverBase64 的末尾与 applemusic 的 setCover 都经它）。日后
// 新增的播放器若自己把图片字节拼成 data URI，发出的封面就绕过了规范化：不报错、不 panic，测试照样绿，只是
// 那一家的 cover_base64 又不是 800×800。
//
// # 它守什么、不守什么
//
// 判据：player/ 下非测试 .go 文件里，含 ";base64," 的字符串字面量（含反引号字面量、fmt 的格式串）只允许出现
// 在 player/cover.go。只看字面量、不看注释（注释里举例写出 data URI 不算违规）。盲区：分段拼出
// 的（";base" + "64,"）、从别处取来的字符串查不到；cover.go 里拼 data URI 的地方有没有经过规范化也不查（由
// coverfetch_test.go 的 TestFetchCoverBase64Normalizes 守）。
//
// 只扫 player/ 目录：tools/cdpexplore 里那一处是解码用的前缀，不归本门禁管；仓库根下可能挂着别的 worktree
// 的整份拷贝，它们的代码也不归本工作区管。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestCoverBase64OnlyViaCoverDataURI 全部播放器强制：拼 data URI 的字面量只在 player/cover.go。
//
// 变异自证：把 player/applemusic/applemusic.go 的 setCover 改回把原字节直接拼成 data URI 的旧写法即红。
func TestCoverBase64OnlyViaCoverDataURI(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根目录失败: %v", err)
	}
	sites, err := base64LiteralSites(filepath.Join(root, "player"))
	if err != nil {
		t.Fatalf("扫描源码失败: %v", err)
	}
	var others []string
	inCover := false
	for _, s := range sites {
		if s.file == "cover.go" {
			inCover = true
		} else {
			others = append(others, s.String())
		}
	}
	// 零命中自检：cover.go 自己那一处都认不出来时，下面的断言恒真。
	if !inCover {
		t.Fatal("没有在 player/cover.go 里认出拼 data URI 的字面量——门禁的判据失效了")
	}
	if len(others) > 0 {
		t.Errorf("以下位置自己拼了 data URI（含 \";base64,\" 的字面量）：\n  %s\n\n"+
			"cover_base64 一律经 player.CoverDataURI 生成，它把封面规范为 800×800（issue #101）。\n"+
			"修法：下载的走 player.FetchCoverBase64，已有图片字节的调用 player.CoverDataURI(播放器名, 字节, MIME)。",
			strings.Join(others, "\n  "))
	}
}

// TestCoverBase64LintCatchesKnownShapes 钉死门禁自身认得出目标形状、不误报注释与测试文件。
//
// 变异自证：只认双引号字面量（不经 strconv.Unquote），「反引号字面量与常量」红；位置只记文件名（任何目录下的
// cover.go 都放行），「子目录里的 cover.go」等四例红；测试文件也扫，「只在测试文件里」红，真仓库那条也红。
func TestCoverBase64LintCatchesKnownShapes(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string // 判为违规的文件（cover.go 不算违规）
	}{
		{"applemusic 的旧写法", map[string]string{"applemusic/applemusic.go": `package applemusic
func f(mime string, b string) string { return "data:" + mime + ";base64," + b }`}, []string{"applemusic/applemusic.go"}},
		{"格式串", map[string]string{"p/p.go": `package p
import "fmt"
func f(m, b string) string { return fmt.Sprintf("data:%s;base64,%s", m, b) }`}, []string{"p/p.go"}},
		{"反引号字面量与常量", map[string]string{"p/p.go": "package p\nconst prefix = `data:image/png;base64,`"}, []string{"p/p.go"}},
		{"cover.go 里的不算", map[string]string{"cover.go": `package player
func f(m, b string) string { return "data:" + m + ";base64," + b }`}, nil},
		{"只在注释里", map[string]string{"p/p.go": `package p
// Cover string // 封面 base64（data:image/jpeg;base64,...）
var Cover string`}, nil},
		{"只在测试文件里", map[string]string{"p/p_test.go": `package p
const want = "data:image/jpeg;base64,AAAA"`}, nil},
		{"子目录里的 cover.go 不算 player/cover.go", map[string]string{"q/cover.go": `package q
const p = ";base64,"`}, []string{"q/cover.go"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, src := range c.files {
				path := filepath.Join(dir, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			sites, err := base64LiteralSites(dir)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, s := range sites {
				if s.file != "cover.go" {
					got = append(got, s.file)
				}
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("判为违规 %v，期望 %v", got, c.want)
			}
		})
	}
}

type base64Site struct {
	file string // 相对扫描目录、斜杠分隔
	line int
}

func (s base64Site) String() string { return "player/" + s.file + ":" + strconv.Itoa(s.line) }

// base64LiteralSites 扫 dir 下的非测试 .go 文件，返回含 ";base64," 的字符串字面量所在位置（按遍历顺序）。
func base64LiteralSites(dir string) ([]base64Site, error) {
	var sites []base64Site
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // 解析不了的跳过，不是本门禁的职责
		}
		rel, _ := filepath.Rel(dir, path)
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(s, ";base64,") {
				sites = append(sites, base64Site{filepath.ToSlash(rel), fset.Position(lit.Pos()).Line})
			}
			return true
		})
		return nil
	})
	return sites, err
}
