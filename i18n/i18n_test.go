package i18n

import "testing"

// 中文（默认/零值）：T 恒等返回原文。既有 verbatim 中文门禁依赖这一点。
func TestChineseIdentity(t *testing.T) {
	defer SetLanguage(Chinese)
	SetLanguage(Chinese)
	const s = "等待 WeSing.exe 启动..."
	if got := T(s); got != s {
		t.Errorf("中文应恒等返回原文，得到 %q", got)
	}
}

// 英文未命中：回退中文原文，不丢信息。
func TestEnglishFallback(t *testing.T) {
	defer SetLanguage(Chinese)
	SetLanguage(English)
	const s = "某条尚未收录的文案 %d"
	if got := T(s); got != s {
		t.Errorf("英文未命中应回退原文，得到 %q", got)
	}
}

// 英文命中：返回译文。用临时 key 验证查表通路，避免依赖具体译文内容。
func TestEnglishLookup(t *testing.T) {
	const k = "__i18n_test_key__"
	english[k] = "translated"
	defer func() {
		delete(english, k)
		SetLanguage(Chinese)
	}()
	SetLanguage(English)
	if got := T(k); got != "translated" {
		t.Errorf("英文命中应返回译文，得到 %q", got)
	}
}

// 探测不 panic，且只返回两个合法值。Windows 上是真实显示语言，
// 非 Windows（CI linux 自检）走桩返回 English。
func TestDetectSystemLanguageSmoke(t *testing.T) {
	if l := DetectSystemLanguage(); l != Chinese && l != English {
		t.Errorf("检测应返回 Chinese 或 English，得到 %v", l)
	}
}
