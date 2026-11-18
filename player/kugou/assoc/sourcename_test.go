package assoc

// 本文件只测来源名：Source.String 返回英文原文，经 i18n.T 后在中英两种语言下各得其文（AGENTS.md §6.4 ⑤⑧）。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/i18n"
)

func TestSourceNames(t *testing.T) {
	defer i18n.SetLanguage(i18n.Language())
	cases := []struct {
		s           Source
		en, zh      string
		description string
	}{
		{SourceAssociation, "KuGou association", "酷狗关联", "关联"},
		{SourceCache, "KuGou cache", "酷狗缓存", "缓存"},
		{0, "", "", "零值"},
		{SourceCache + 1, "", "", "越界"},
	}
	for _, c := range cases {
		i18n.SetLanguage(i18n.English)
		en := i18n.T(c.s.String())
		i18n.SetLanguage(i18n.Chinese)
		if zh := i18n.T(c.s.String()); en != c.en || zh != c.zh {
			t.Errorf("%s：英文 %q、中文 %q，want %q、%q", c.description, en, zh, c.en, c.zh)
		}
	}
}
