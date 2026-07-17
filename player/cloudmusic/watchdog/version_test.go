package watchdog

// 本文件只测 v2/v3 的版本判据。不测 EnsureDebugMode 的进程枚举（需真实网易云进程）、
// 不测注册表。
//
// 测试数据是**实测来的**（2026-07-16，两个真实安装）：
//	v2: 2.10.13.202675   v3: 3.1.35.205293
// 两者的 FileDescription（NetEase Cloud Music）与 CompanyName（NetEase）完全一致，
// 目录结构也几乎一样（都是 CEF：libcef.dll / chrome_elf.dll / v8_context_snapshot.bin
// 全部 HIT；app.asar / node.dll 全部 miss）—— **所以只有版本号能区分它们**。

import (
	"os"
	"testing"
)

// TestIsUnsupportedVersionRealSamples 用两个真实版本号钉死判据。
//
// 变异自证：把 `major < v3MinMajor` 改成 `<=` 即红（v3 被误判成 v2）。
func TestIsUnsupportedVersionRealSamples(t *testing.T) {
	if !IsUnsupportedVersion("2.10.13.202675") {
		t.Error("真实 v2（2.10.13.202675）必须判为不支持 —— 它的 CEF 不开 remote debugging")
	}
	if IsUnsupportedVersion("3.1.35.205293") {
		t.Error("真实 v3（3.1.35.205293）绝不能判为不支持 —— 误判 = 主播的歌词与特效静默消失")
	}
}

// TestIsUnsupportedVersionFailsOpen 钉死**误判方向** —— 这是本条最要紧的不变量。
//
// 两个方向的代价不对称：
//   - v2 当成 v3 → 维持现状（连不上 9222，会重试）。就是今天的样子，不更坏。
//   - v3 当成 v2 → 我们**主动**禁用取词与特效镜像 → 主播的歌词静默消失，而且是我们干的。
//
// 所以凡是「拿不准」，一律按 v3 走。
//
// 变异自证：把任一 `return false` 改成 `return true` 即红。
func TestIsUnsupportedVersionFailsOpen(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"版本号读不到（权限不足/文件被占/无版本资源）", ""},
		{"完全不是版本号", "garbage"},
		{"主版本位不是数字", "x.1.2.3"},
		{"空的主版本位", ".1.2.3"},
		{"负数", "-1.0.0.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if IsUnsupportedVersion(c.in) {
				t.Errorf("%q 属于「拿不准」，必须按 v3 走（返回 false）—— 否则会误杀 v3 用户", c.in)
			}
		})
	}
}

// TestIsUnsupportedVersionBoundary 主版本号的边界。
func TestIsUnsupportedVersionBoundary(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"1.0.0.0", true},    // 更老的版本
		{"2.0.0.0", true},    // v2 起点
		{"2.99.99.99", true}, // v2 末期也还是 v2
		{"3.0.0.0", false},   // v3 起点：支持
		{"3.1.35.205293", false},
		{"4.0.0.0", false},  // 未来版本：按支持走（宁可试连也别静默禁用）
		{"10.0.0.0", false}, // 别把 "10" 的首字符当成 "1"
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := IsUnsupportedVersion(c.in); got != c.want {
				t.Errorf("IsUnsupportedVersion(%q) = %v，want %v", c.in, got, c.want)
			}
		})
	}
}

// TestIsUnsupportedVersionNoDot 没有点号时整串当主版本试。
func TestIsUnsupportedVersionNoDot(t *testing.T) {
	if !IsUnsupportedVersion("2") {
		t.Error("裸 \"2\" 应判为 v2")
	}
	if IsUnsupportedVersion("3") {
		t.Error("裸 \"3\" 应判为支持")
	}
}

// TestReadExeVersionRealFile 拿一个真实 exe 验证读取链路通（含 Signature 校验、
// uintptr→Pointer 的 UAF 防护那段偏移计算）。
//
// 用测试二进制自己：go test 产出的 exe 没有版本资源 → 应返回空串，而空串正好走
// fails-open。用 System32 里的系统 exe 作为「有版本资源」的正样本。
func TestReadExeVersionRealFile(t *testing.T) {
	// 无版本资源 → 空串（且绝不能 panic / 返回垃圾）
	self, err := os.Executable()
	if err != nil {
		t.Skipf("拿不到测试二进制路径: %v", err)
	}
	if v := ReadExeVersion(self); v != "" {
		t.Logf("测试二进制意外带版本资源: %q（不算失败，只是记录）", v)
	}

	// 有版本资源的正样本
	const sysExe = `C:\Windows\System32\notepad.exe`
	if _, err := os.Stat(sysExe); err != nil {
		t.Skipf("找不到 %s，跳过正样本", sysExe)
	}
	v := ReadExeVersion(sysExe)
	if v == "" {
		t.Errorf("%s 应能读出版本号，实得空串 —— 读取链路坏了", sysExe)
	}
	if IsUnsupportedVersion(v) {
		t.Logf("注：notepad 版本 %q 主版本 < 3（正常，它不是网易云）", v)
	}
}

// TestReadExeVersionBadPath 路径不存在/非法时返回空串，不 panic。
// 空串 → fails-open → 按 v3 走，这正是我们要的降级方向。
func TestReadExeVersionBadPath(t *testing.T) {
	for _, p := range []string{
		`C:\definitely\not\here\nope.exe`,
		``,
		"\x00bad",
	} {
		if v := ReadExeVersion(p); v != "" {
			t.Errorf("ReadExeVersion(%q) 应返回空串，实得 %q", p, v)
		}
	}
}
