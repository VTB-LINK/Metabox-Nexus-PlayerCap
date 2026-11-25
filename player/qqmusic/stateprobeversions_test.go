package qqmusic

// 本文件只测 tools/qqstateprobe 的偏移表与 knownVersions 同步（#226，doc/design/qqmusic-start-stall-design.md §6.8
// 第 1 条）：探测工具不引用生产包，为读歌名与进度照抄了 knownVersions 的一部分偏移；增改版本时漏同步即红。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// probeVersionsProblems 比对 src（tools/qqstateprobe/versions.go 的源码）里的 knownVersions 与本包的 knownVersions：
// 版本集合相同，每个版本的 Struct1、Struct2Ptr、FastTimerPtr、FastTimerOff、ProgressDllOff、NameOff、ProgressOff 与
// Wide（UseWideStrings）逐项相等。工具表里出现不认得的字段、或字段值不是字面量时报出（门禁要跟着改）。
func probeVersionsProblems(src string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "versions.go", src, 0)
	if err != nil {
		return []string{"解析失败: " + err.Error()}
	}
	var table *ast.CompositeLit
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.VAR {
			continue
		}
		for _, sp := range g.Specs {
			vs := sp.(*ast.ValueSpec)
			if len(vs.Names) == 1 && vs.Names[0].Name == "knownVersions" && len(vs.Values) == 1 {
				table, _ = vs.Values[0].(*ast.CompositeLit)
			}
		}
	}
	if table == nil {
		return []string{"没找到 var knownVersions = map[string]verOffsets{…} —— 门禁失效"}
	}
	var probs, got []string
	for _, e := range table.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			return []string{"knownVersions 的条目不是 键: 值 —— 门禁失效"}
		}
		ver, err := strconv.Unquote(types.ExprString(kv.Key))
		lit, ok := kv.Value.(*ast.CompositeLit)
		if err != nil || !ok {
			return []string{"knownVersions 的条目 " + types.ExprString(kv.Key) + " 不是 \"版本\": {…} —— 门禁失效"}
		}
		got = append(got, ver)
		fields := map[string]uint64{}
		wide := false
		for _, fe := range lit.Elts {
			fkv, ok := fe.(*ast.KeyValueExpr)
			if !ok {
				return []string{ver + " 的字段不是 名: 值 —— 门禁失效"}
			}
			name, val := types.ExprString(fkv.Key), types.ExprString(fkv.Value)
			switch name {
			case "Wide":
				wide = val == "true"
			case "Struct1", "Struct2Ptr", "FastTimerPtr", "FastTimerOff", "ProgressDllOff", "NameOff", "ProgressOff":
				n, err := strconv.ParseUint(val, 0, 64)
				if err != nil {
					return []string{fmt.Sprintf("%s 的 %s 不是整数字面量（%s）—— 门禁失效", ver, name, val)}
				}
				fields[name] = n
			default:
				return []string{fmt.Sprintf("%s 有门禁不认得的字段 %s —— 门禁要跟着改", ver, name)}
			}
		}
		o, ok := knownVersions[ver]
		if !ok {
			probs = append(probs, "工具表有 "+ver+"，knownVersions 没有")
			continue
		}
		want := map[string]uint64{"Struct1": uint64(o.Struct1), "Struct2Ptr": uint64(o.Struct2Ptr),
			"FastTimerPtr": uint64(o.FastTimerPtr), "FastTimerOff": uint64(o.FastTimerOff),
			"ProgressDllOff": uint64(o.ProgressDllOff), "NameOff": uint64(o.NameOff), "ProgressOff": uint64(o.ProgressOff)}
		for name, w := range want {
			if fields[name] != w {
				probs = append(probs, fmt.Sprintf("%s 的 %s：工具表 %#x，knownVersions %#x", ver, name, fields[name], w))
			}
		}
		if wide != o.UseWideStrings {
			probs = append(probs, fmt.Sprintf("%s 的 Wide：工具表 %v，knownVersions 的 UseWideStrings %v", ver, wide, o.UseWideStrings))
		}
	}
	for ver := range knownVersions {
		if !slices.Contains(got, ver) {
			probs = append(probs, "knownVersions 有 "+ver+"，工具表没有")
		}
	}
	slices.Sort(probs)
	return probs
}

func readProbeVersions(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tools", "qqstateprobe", "versions.go"))
	if err != nil {
		t.Fatalf("读 tools/qqstateprobe/versions.go 失败: %v", err)
	}
	return string(b)
}

// TestStateProbeVersionsInSync：tools/qqstateprobe/versions.go 的偏移表与 knownVersions 一致。
func TestStateProbeVersionsInSync(t *testing.T) {
	if probs := probeVersionsProblems(readProbeVersions(t)); len(probs) > 0 {
		t.Fatalf("tools/qqstateprobe/versions.go 与 knownVersions 不同步：\n  %s", strings.Join(probs, "\n  "))
	}
}

// TestStateProbeVersionsCatchesDrift 以改写过的工具表确认门禁拦得住：改一个偏移、漏一个版本、多一个版本、改字符串
// 模型、加一个不认得的字段，各自报出问题。改写找不到原文时自报失效。
//
// 变异自证（均实测，改的是 probeVersionsProblems）：不比对 ProgressDllOff、不比对 Wide、不查工具表缺版本、不查工具表
// 多出的版本、不拒不认得的字段，各自让对应的一例红。
func TestStateProbeVersionsCatchesDrift(t *testing.T) {
	src := readProbeVersions(t)
	cases := []struct{ name, old, new string }{
		{"22.82 的 Struct1 写错", `"22.82": {Struct1: 0xCCB468,`, `"22.82": {Struct1: 0xCCB488,`},
		{"漏掉 22.82", `	"22.82": {Struct1: 0xCCB468, ProgressOff: 0x0C, Wide: true},` + "\n", ""},
		{"多出 22.83", `	"22.82": {`, `	"22.83": {Struct1: 0xCCB468, ProgressOff: 0x0C, Wide: true},` + "\n" + `	"22.82": {`},
		{"20.05 的进度偏移写错", "ProgressDllOff: 0xB61E78", "ProgressDllOff: 0xAF3600"},
		{"22.16 改成宽字符", `"22.16": {Struct1: 0xC87C80,`, `"22.16": {Wide: true, Struct1: 0xC87C80,`},
		{"不认得的字段", `"22.82": {Struct1: 0xCCB468,`, `"22.82": {SongIDOff: 0x73F28, Struct1: 0xCCB468,`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(src, c.old) {
				t.Fatalf("versions.go 里找不到改写依赖的原文 %q —— 本条自测失效", c.old)
			}
			if probs := probeVersionsProblems(strings.Replace(src, c.old, c.new, 1)); len(probs) == 0 {
				t.Fatal("门禁没有拦下这处改写")
			}
		})
	}
}
