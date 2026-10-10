package main

// 本文件只测一件事：仓库里的 doc/openapi.yaml 本身是合法的 YAML。
//
// checkYAMLSyntax 早就有，但只在人手动跑 docsample（要带录音目录）时才会碰到真文件；yaml_test.go
// 测的是这个函数认不认得出坏 YAML，喂的是内联的样本。于是「真文件坏了」在四道门禁里没有任何一道
// 会红——2026-10-09 就这样提交过一次：往两个 schema 的 properties 映射里插进了 Markdown 列表项
// （``- `roma_text_detailed` - …``），整份文件解析失败（yaml: line 2574: found character that cannot
// start any token），Apifox / Swagger 打不开，gofmt / build / vet / test 全绿。

import (
	"os"
	"testing"
)

// 变异自证：在 doc/openapi.yaml 某个 schema 的 properties 映射里插一行 Markdown 列表项（把那次提交的
// 文件原样放回来），解析失败而红。
func TestOpenAPIDocParses(t *testing.T) {
	const path = "../../doc/openapi.yaml"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s：%v", path, err)
	}
	if bad := checkYAMLSyntax("doc/openapi.yaml", string(src)); bad != 0 {
		t.Fatalf("doc/openapi.yaml 解析失败（具体位置见上一行输出）——对外契约挂了，Apifox / Swagger 打不开")
	}
}
