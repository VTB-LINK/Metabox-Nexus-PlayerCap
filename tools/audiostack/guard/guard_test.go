//go:build windows

package guard

// 本文件钉住 audiostack 的两道安全校验（Codex 审查 R1、R2）。路径规则是 Windows 的（反斜杠、末尾 \.），故仅在
// Windows 上跑。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("建文件 %s: %v", path, err)
	}
}

// 标准文件名无论怎么写都必须被拒绝：原样、大小写、末尾 \.、\子目录\..、正斜杠、符号链接指向它；
// 反例 R1 是原始路径的 Base 为 "."，规范化后才是标准名。
//
// 变异自证（实测）：不解析符号链接（只比 Abs 之后的 Base）→「符号链接」一例红（该例在不能建符号链接的环境里跳过）；
// 对原始输入取 Base 且同时去掉「文件名以点或空格结尾」规则 →「末尾 \.」「\子目录\..」两例红。这两例有两层独立的防护
// （Abs 之后的 Base 比较、点结尾规则），只拆其中一层不会红。大小写一例同样被两处挡住：EvalSymlinks 会把路径还原成磁盘上
// 的真实大小写，故 EqualFold 改成 == 是等价变异，不红。
func TestCheckHelperRejectsStandardName(t *testing.T) {
	dir := t.TempDir()
	std := filepath.Join(dir, StandardExe)
	touch(t, std)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, path string }{
		{"原样", std},
		{"大小写", filepath.Join(dir, strings.ToUpper(StandardExe))},
		{"末尾 \\.（原始 Base 为 .）", std + `\.`},
		{"\\子目录\\..", std + `\sub\..`},
		{"正斜杠写法", strings.ReplaceAll(std, `\`, `/`)},
	} {
		if got, err := CheckHelper(c.path); err == nil {
			t.Errorf("%s: 应被拒绝，却放行为 %q", c.name, got)
		}
	}

	link := filepath.Join(dir, "innocent.exe")
	if err := os.Symlink(std, link); err != nil {
		t.Logf("不能建符号链接（%v），跳过符号链接一例", err)
	} else if got, err := CheckHelper(link); err == nil {
		t.Errorf("指向标准文件的符号链接: 应被拒绝，却放行为 %q", got)
	}
}

// 合法的非标准文件名返回规范化的绝对路径；不存在、目录、空串、以点或空格结尾的名字一律拒绝。
func TestCheckHelperAcceptsAndRejects(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "mbx-audio-test.exe")
	touch(t, ok)
	if got, err := CheckHelper(ok); err != nil || got != ok {
		t.Fatalf("合法路径: got %q err %v, 期望 %q", got, err, ok)
	}
	if got, err := CheckHelper(filepath.Join(dir, `.\mbx-audio-test.exe`)); err != nil || got != ok {
		t.Fatalf("含 .\\ 的合法路径应规范化: got %q err %v", got, err)
	}
	for _, c := range []struct{ name, path string }{
		{"空串", ""},
		{"全空白", "   "},
		{"不存在", filepath.Join(dir, "nope.exe")},
		{"目录", dir},
	} {
		if got, err := CheckHelper(c.path); err == nil {
			t.Errorf("%s: 应被拒绝，却放行为 %q", c.name, got)
		}
	}
}

// 监听地址只允许回环字面 IP；localhost 被改写成 127.0.0.1，不把名字交给 net.Listen 再解析（R2）。
//
// 变异自证（实测）：localhost 原样放行 →「localhost 改写」一例红；用前缀判断代替解析 →「127.0.0.1.evil」
// 「localhost.evil.com」两例红；放行 0.0.0.0 或空 host → 对应例红。
func TestCheckAddr(t *testing.T) {
	for _, c := range []struct {
		addr string
		want string // 空表示应拒绝
	}{
		{"127.0.0.1:8791", "127.0.0.1:8791"},
		{"localhost:8791", "127.0.0.1:8791"},
		{"LOCALHOST:8791", "127.0.0.1:8791"},
		{"[::1]:8791", "[::1]:8791"},
		{"0.0.0.0:8791", ""},
		{":8791", ""},
		{"192.168.1.20:8791", ""},
		{"127.0.0.1.evil.com:8791", ""},
		{"localhost.evil.com:8791", ""},
		{"example.com:8791", ""},
		{"127.0.0.1", ""},
		{"127.0.0.1:0", ""},
		{"127.0.0.1:99999", ""},
		{"127.0.0.1:abc", ""},
		{"", ""},
	} {
		got, err := CheckAddr(c.addr)
		if c.want == "" {
			if err == nil {
				t.Errorf("CheckAddr(%q) 应被拒绝，却放行为 %q", c.addr, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("CheckAddr(%q) = %q, %v; 期望 %q", c.addr, got, err, c.want)
		}
	}
}
