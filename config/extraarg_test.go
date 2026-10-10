package config

// 本文件只测 extraArgError：解析之后还有剩余实参时的报错文案（issue #92）。Load 据它打印、输出用法并以 2
// 退出，那一段经真实的 Load 在子进程里测，见根包 cliexit_test.go。

import (
	"testing"

	"Metabox-Nexus-PlayerCap/i18n"
)

// TestExtraArgError 没有剩余实参时不报错；有则点名第一个，按 %q 写出，中英文各一份。
//
// 变异自证（隔离的 worktree，实测）：恒返回 nil、有剩余实参才返回 nil（判断取反）、点名最后一个而不是
// 第一个、不用 %q（空串与带空格的词看不出边界）、中文译表缺这一条 → 红。译表缺这一条另由
// i18n/catalogcoverage_test.go 拦（实测）。
func TestExtraArgError(t *testing.T) {
	for _, rest := range [][]string{nil, {}} {
		if err := extraArgError(rest); err != nil {
			t.Errorf("extraArgError(%#v) = %v，期望 nil", rest, err)
		}
	}

	t.Cleanup(func() { i18n.SetLanguage(i18n.Chinese) })
	cases := []struct {
		rest   []string
		zh, en string
	}{
		{[]string{"offset", "-200"},
			`命令行里多余的词 "offset"：它不是参数，也不是某个参数的值；自它起的内容均未被采用`,
			`unexpected argument "offset": it is neither a flag nor a flag's value; nothing from it onward was applied`},
		{[]string{"yes"},
			`命令行里多余的词 "yes"：它不是参数，也不是某个参数的值；自它起的内容均未被采用`,
			`unexpected argument "yes": it is neither a flag nor a flag's value; nothing from it onward was applied`},
		{[]string{"-offset", "1"}, // -- 之后的
			`命令行里多余的词 "-offset"：它不是参数，也不是某个参数的值；自它起的内容均未被采用`,
			`unexpected argument "-offset": it is neither a flag nor a flag's value; nothing from it onward was applied`},
		{[]string{"", "foo"},
			`命令行里多余的词 ""：它不是参数，也不是某个参数的值；自它起的内容均未被采用`,
			`unexpected argument "": it is neither a flag nor a flag's value; nothing from it onward was applied`},
		{[]string{"a b", "c"},
			`命令行里多余的词 "a b"：它不是参数，也不是某个参数的值；自它起的内容均未被采用`,
			`unexpected argument "a b": it is neither a flag nor a flag's value; nothing from it onward was applied`},
		{[]string{"偏移", "foo"},
			`命令行里多余的词 "偏移"：它不是参数，也不是某个参数的值；自它起的内容均未被采用`,
			`unexpected argument "偏移": it is neither a flag nor a flag's value; nothing from it onward was applied`},
	}
	for _, c := range cases {
		for _, l := range []struct {
			lang i18n.Lang
			want string
		}{{i18n.Chinese, c.zh}, {i18n.English, c.en}} {
			i18n.SetLanguage(l.lang)
			err := extraArgError(c.rest)
			if err == nil || err.Error() != l.want {
				t.Errorf("extraArgError(%q) = %v，期望 %s", c.rest, err, l.want)
			}
		}
	}
}
