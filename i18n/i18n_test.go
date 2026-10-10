package i18n

import "testing"

// withEntry 临时往译表里放一条，用例结束时撤掉并把语言还原为中文。
// 用临时 key 验证查表通路，不依赖表里某一条具体的译文。
func withEntry(t *testing.T, key, val string) {
	t.Helper()
	chinese[key] = val
	t.Cleanup(func() {
		delete(chinese, key)
		SetLanguage(Chinese)
	})
}

// 未调用 SetLanguage 时是中文。别的包的测试不设语言、直接断言中文输出，靠的就是这一点；
// 测试在哪种语言下跑由它决定，与开发机的系统语言无关（探测只在 main 里调用）。
//
// 变异自证（实测）：current 的初值改为 English，红。
func TestDefaultLanguageIsChinese(t *testing.T) {
	if Language() != Chinese {
		t.Errorf("未设语言时应为 Chinese，得到 %v", Language())
	}
}

// 英文：T 恒等返回原文，表里有没有这一条都一样。
//
// 变异自证（实测）：T 不看语言一律查表，红——英文环境下收录的那条被换成了中文。
func TestEnglishIdentity(t *testing.T) {
	const k = "__i18n_test_key__ %d"
	withEntry(t, k, "译文 %d")
	SetLanguage(English)
	for _, s := range []string{k, "a message that is not in the catalog %d"} {
		if got := T(s); got != s {
			t.Errorf("英文应恒等返回原文 %q，得到 %q", s, got)
		}
	}
}

// 中文命中：返回译文。
//
// 变异自证（实测）：T 的语言判断写反（英文才查表），红；T 恒等返回，红。
func TestChineseLookup(t *testing.T) {
	const k = "__i18n_test_key__ %d"
	withEntry(t, k, "译文 %d")
	SetLanguage(Chinese)
	if got := T(k); got != "译文 %d" {
		t.Errorf("中文命中应返回译文，得到 %q", got)
	}
}

// 中文未命中：回退英文原文，不丢信息。
//
// 变异自证（实测）：未命中时返回空串，红。
func TestChineseFallback(t *testing.T) {
	t.Cleanup(func() { SetLanguage(Chinese) })
	SetLanguage(Chinese)
	const s = "a message that is not in the catalog %d"
	if got := T(s); got != s {
		t.Errorf("中文未命中应回退原文，得到 %q", got)
	}
}

// Errorf 先把 format 过 T 再格式化，%w 照常包住原错误。
//
// 变异自证（隔离副本，实测）：
//   - Errorf 的中文一支不过 T：红，中文环境下仍是英文原文。
//   - go vet 对调用点的校验：把 main.go 里 i18n.Errorf("failed to get program path: %v", err) 的
//     format 改成 "…: %d %s"，go vet 报「format %s reads arg #2, but call has 1 arg」。
//     Errorf 改回 format = T(format) 之后再转发的写法，同一处改动 go vet 不报（issue #61 之前
//     就是这种写法，对改造前的 ce1f934 同样实测为不报）。
func TestErrorfTranslatesFormat(t *testing.T) {
	const k = "__i18n_test_key__ failed: %w"
	withEntry(t, k, "__i18n_test_key__ 失败: %w")
	inner := Errorf("inner")

	SetLanguage(Chinese)
	err := Errorf(k, inner)
	if got, want := err.Error(), "__i18n_test_key__ 失败: inner"; got != want {
		t.Errorf("中文环境下得到 %q，预期 %q", got, want)
	}
	if u, ok := err.(interface{ Unwrap() error }); !ok || u.Unwrap() != inner {
		t.Error("%w 应当包住原错误")
	}

	SetLanguage(English)
	if got, want := Errorf(k, inner).Error(), "__i18n_test_key__ failed: inner"; got != want {
		t.Errorf("英文环境下得到 %q，预期 %q", got, want)
	}
}

// 探测不 panic，且只返回两个合法值。Windows 上是真实显示语言，
// 非 Windows（CI linux 自检）走桩返回 English。
func TestDetectSystemLanguageSmoke(t *testing.T) {
	if l := DetectSystemLanguage(); l != Chinese && l != English {
		t.Errorf("检测应返回 Chinese 或 English，得到 %v", l)
	}
}
