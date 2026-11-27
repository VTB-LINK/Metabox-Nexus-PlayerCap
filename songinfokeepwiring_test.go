package main

// 本文件只测一件事：main() 在第一处 .Start() 之前恰好调用一次
// player.SetSongInfoKeepFirst(cfg.SongInfoKeepFirst)（C3 接线门禁，#229）。
//
// 为什么做 AST 门禁：player.SetSongInfoKeepFirst 的零值是开启，main() 若漏接或挪到播放器
// 启动之后，默认开启场景下一切正常、单测照绿，但配置关闭开关（song-info-keep-first: false）
// 在真实进程中会静默失效或与已启动的播放器 goroutine 竞态。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

const playerImportPath = "Metabox-Nexus-PlayerCap/player"

// checkSongInfoKeepFirstWiring 校验 Go 源码 src 中 main() 函数对
// player.SetSongInfoKeepFirst(cfg.SongInfoKeepFirst) 的接线：
//  1. 导入了 Metabox-Nexus-PlayerCap/player；
//  2. main() 中至少存在一处 .Start() 调用（防门禁空转）；
//  3. main() 中恰好存在一次 <player>.SetSongInfoKeepFirst(cfg.SongInfoKeepFirst) 调用，
//     位于 main 函数体顶层语句（非条件分支或闭包内），且位置严格早于第一处 .Start() 调用。
func checkSongInfoKeepFirstWiring(src []byte) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", src, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("解析 main.go 失败: %w", err)
	}

	playerLocal := ""
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err == nil && p == playerImportPath {
			playerLocal = "player"
			if imp.Name != nil {
				playerLocal = imp.Name.Name
			}
		}
	}
	if playerLocal == "" || playerLocal == "_" || playerLocal == "." {
		return fmt.Errorf("main.go 未以具名方式导入 %q", playerImportPath)
	}

	var mainBody *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "main" && fn.Body != nil {
			mainBody = fn.Body
			break
		}
	}
	if mainBody == nil {
		return fmt.Errorf("main.go 里找不到 func main()")
	}

	// 记录 main() 顶层语句中的 SetSongInfoKeepFirst 调用指针，用于确认调用不在 if/for/闭包内。
	topLevelCalls := map[*ast.CallExpr]bool{}
	for _, stmt := range mainBody.List {
		if exprStmt, ok := stmt.(*ast.ExprStmt); ok {
			if call, ok := exprStmt.X.(*ast.CallExpr); ok {
				topLevelCalls[call] = true
			}
		}
	}

	var firstStartPos token.Pos
	var setCalls []*ast.CallExpr
	ast.Inspect(mainBody, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "Start" {
			if !firstStartPos.IsValid() || call.Pos() < firstStartPos {
				firstStartPos = call.Pos()
			}
		}
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == playerLocal && sel.Sel.Name == "SetSongInfoKeepFirst" {
			setCalls = append(setCalls, call)
		}
		return true
	})

	if !firstStartPos.IsValid() {
		return fmt.Errorf("main() 里找不到任何 .Start() 调用：门禁参照点缺失")
	}
	if len(setCalls) != 1 {
		return fmt.Errorf("main() 里应恰好有 1 处 %s.SetSongInfoKeepFirst 调用，实际 %d 处", playerLocal, len(setCalls))
	}

	call := setCalls[0]
	if !topLevelCalls[call] {
		return fmt.Errorf("%s.SetSongInfoKeepFirst 必须位于 main() 顶层语句，不得置于条件分支或闭包内（行 %d）",
			playerLocal, fset.Position(call.Pos()).Line)
	}
	if len(call.Args) != 1 || !isCfgSongInfoKeepFirst(call.Args[0]) {
		return fmt.Errorf("%s.SetSongInfoKeepFirst 实参应为 cfg.SongInfoKeepFirst（行 %d）",
			playerLocal, fset.Position(call.Pos()).Line)
	}
	if call.Pos() >= firstStartPos {
		return fmt.Errorf("%s.SetSongInfoKeepFirst（行 %d）必须位于第一处 .Start() 调用（行 %d）之前",
			playerLocal, fset.Position(call.Pos()).Line, fset.Position(firstStartPos).Line)
	}
	return nil
}

func isCfgSongInfoKeepFirst(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "cfg" && sel.Sel.Name == "SongInfoKeepFirst"
}

// TestMainWiresSongInfoKeepFirstBeforeStart 断言仓库内真实 main.go 满足 C3 接线约束。
//
// 变异自证（隔离副本，实测）：main.go 删去 SetSongInfoKeepFirst（无论是否同时删去 player import）→ 红；
// 挪到 srv.Start 之后或所有播放器 .Start() 之后 → 均红。
func TestMainWiresSongInfoKeepFirstBeforeStart(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读取 main.go 失败: %v", err)
	}
	if err := checkSongInfoKeepFirstWiring(src); err != nil {
		t.Error(err)
	}
}

// TestSongInfoKeepFirstWiringGateCatchesViolations 自测 C3 门禁能拦住删去调用、挪到 .Start() 之后、
// 重复调用、实参写死、藏在分支/闭包里等坏形状，并放行合规写法（含别名 import）。
func TestSongInfoKeepFirstWiringGateCatchesViolations(t *testing.T) {
	validCases := map[string]string{
		"标准写法": `package main
import "Metabox-Nexus-PlayerCap/player"
func main() {
	player.SetSongInfoKeepFirst(cfg.SongInfoKeepFirst)
	go func() { srv.Start() }()
	go func() { wp.Start() }()
}`,
		"别名导入": `package main
import p "Metabox-Nexus-PlayerCap/player"
func main() {
	p.SetSongInfoKeepFirst(cfg.SongInfoKeepFirst)
	srv.Start()
}`,
	}
	for name, src := range validCases {
		t.Run("合规/"+name, func(t *testing.T) {
			if err := checkSongInfoKeepFirstWiring([]byte(src)); err != nil {
				t.Errorf("合规写法被误报：%v", err)
			}
		})
	}

	invalidCases := []struct {
		name    string
		src     string
		wantSub string
	}{
		{
			name: "删去调用",
			src: `package main
import "Metabox-Nexus-PlayerCap/player"
func main() {
	go func() { srv.Start() }()
	go func() { wp.Start() }()
}`,
			wantSub: "实际 0 处",
		},
		{
			name: "挪到 Start 之后",
			src: `package main
import "Metabox-Nexus-PlayerCap/player"
func main() {
	go func() { srv.Start() }()
	player.SetSongInfoKeepFirst(cfg.SongInfoKeepFirst)
	go func() { wp.Start() }()
}`,
			wantSub: "之前",
		},
		{
			name: "重复调用两次",
			src: `package main
import "Metabox-Nexus-PlayerCap/player"
func main() {
	player.SetSongInfoKeepFirst(cfg.SongInfoKeepFirst)
	player.SetSongInfoKeepFirst(cfg.SongInfoKeepFirst)
	srv.Start()
}`,
			wantSub: "实际 2 处",
		},
		{
			name: "实参写死为 true",
			src: `package main
import "Metabox-Nexus-PlayerCap/player"
func main() {
	player.SetSongInfoKeepFirst(true)
	srv.Start()
}`,
			wantSub: "cfg.SongInfoKeepFirst",
		},
		{
			name: "藏在条件分支里",
			src: `package main
import "Metabox-Nexus-PlayerCap/player"
func main() {
	if false {
		player.SetSongInfoKeepFirst(cfg.SongInfoKeepFirst)
	}
	srv.Start()
}`,
			wantSub: "顶层语句",
		},
		{
			name: "藏在闭包里",
			src: `package main
import "Metabox-Nexus-PlayerCap/player"
func main() {
	_ = func() { player.SetSongInfoKeepFirst(cfg.SongInfoKeepFirst) }
	srv.Start()
}`,
			wantSub: "顶层语句",
		},
		{
			name: "未导入 player 包",
			src: `package main
func main() {
	player.SetSongInfoKeepFirst(cfg.SongInfoKeepFirst)
	srv.Start()
}`,
			wantSub: "未以具名方式导入",
		},
		{
			name: "缺少 Start 参照点",
			src: `package main
import "Metabox-Nexus-PlayerCap/player"
func main() {
	player.SetSongInfoKeepFirst(cfg.SongInfoKeepFirst)
}`,
			wantSub: "找不到任何 .Start()",
		},
	}
	for _, c := range invalidCases {
		t.Run("违规/"+c.name, func(t *testing.T) {
			err := checkSongInfoKeepFirstWiring([]byte(c.src))
			if err == nil {
				t.Fatal("门禁漏报了此违规形状")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("报错 %q 不含预期关键字 %q", err.Error(), c.wantSub)
			}
		})
	}
}
