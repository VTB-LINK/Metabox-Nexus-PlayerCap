package telemetry

// 本文件只测一件事：**Windows 的数字码翻成人话时，认不出的那些不许丢信息**。
// shim 检测在 compatshim_test.go。
//
// Windows 的 PRODUCT_* 有一百多个，我们只列了主播可能用到的十几个。剩下的一定会遇到 ——
// 关键是遇到时**别把线索扔了**：返回 "Unknown" 等于告诉未来的自己「这台机器是个谜」，
// 而返回 "0x1234" 至少能拿去查文档。

import (
	"strings"
	"testing"
)

// TestEditionNameKnownCodes 已知码翻对。
//
// 0x4 是实测值：本机 GetProductInfo 返回 0x4，系统属性里显示 Windows 10 Enterprise。
func TestEditionNameKnownCodes(t *testing.T) {
	cases := map[uint32]string{
		0x04: "Enterprise",   // 实测：本机
		0x30: "Professional", // 主播最常见
		0x65: "Home",
		0xB2: "Enterprise LTSC",
		0x48: "Education",
	}
	for code, want := range cases {
		if got := editionName(code); got != want {
			t.Errorf("editionName(0x%x) = %q，want %q", code, got, want)
		}
	}
}

// TestEditionNameUnknownKeepsCode 未知码必须把原值带出来。
//
// 变异自证：把 default 分支改成 `return "Unknown"` 即红。
func TestEditionNameUnknownKeepsCode(t *testing.T) {
	got := editionName(0xABCD)
	if !strings.Contains(got, "abcd") && !strings.Contains(got, "ABCD") {
		t.Errorf("editionName(0xABCD) = %q —— 认不出的码必须原样带上，"+
			"否则遇到没见过的 Windows 版本时，唯一能查的线索就没了", got)
	}
}

// TestArchNameKnownCodes 架构码翻对。9 是实测值（本机 x64）。
func TestArchNameKnownCodes(t *testing.T) {
	cases := map[uint16]string{
		9:  "x64", // 实测：本机
		0:  "x86",
		12: "arm64",
		5:  "arm",
	}
	for code, want := range cases {
		if got := archName(code); got != want {
			t.Errorf("archName(%d) = %q，want %q", code, got, want)
		}
	}
}

// TestArchNameUnknownKeepsCode 未知架构同样不许丢码。
//
// 变异自证：把 default 分支改成 `return "unknown"` 即红。
func TestArchNameUnknownKeepsCode(t *testing.T) {
	if got := archName(42); !strings.Contains(got, "42") {
		t.Errorf("archName(42) = %q —— 认不出的架构码必须原样带上", got)
	}
}
