package i18n

import (
	"regexp"
	"sort"
	"testing"
)

// verbRe 匹配 Go fmt 占位符：可选 flag / 显式序号 [n] / 宽度 / 精度 + 动词字母（或 %%）。
var verbRe = regexp.MustCompile(`%[+\-# 0]*(?:\[\d+\])?[0-9]*(?:\.[0-9]+)?[bcdeEfFgGoOpqstTUvwxX%]`)

// verbs 返回格式串里出现的动词字母多重集（升序），忽略 %% 字面百分号。
func verbs(s string) []string {
	out := []string{}
	for _, m := range verbRe.FindAllString(s, -1) {
		v := m[len(m)-1:]
		if v == "%" { // %% 是字面百分号，不是占位符
			continue
		}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// TestEnglishPlaceholdersMatch 钉死每条英文译文的占位符与中文原文逐一对应。
//
// i18n 最隐蔽的 bug：译文漏掉 / 多出 / 写错一个 %-占位符，fmt 会打出 %!s(MISSING)
// 之类的垃圾，而它只在运行到那条文案时才现形。这里对全表一次性核对，编译后即拦。
// %[n] 显式序号允许语序重排，故只比动词多重集、不比出现顺序。
func TestEnglishPlaceholdersMatch(t *testing.T) {
	for zh, en := range english {
		vz, ve := verbs(zh), verbs(en)
		mismatch := len(vz) != len(ve)
		for i := 0; !mismatch && i < len(vz); i++ {
			if vz[i] != ve[i] {
				mismatch = true
			}
		}
		if mismatch {
			t.Errorf("占位符与原文不一致：\n  中文 %q → %v\n  英文 %q → %v", zh, vz, en, ve)
		}
	}
}
