package assoc

// 本文件只测 ParseRecordTail：自前缀命中处截取当前歌曲记录的网址与参数 0（设计 §6.4 第 4 步「网址截取」）。

import (
	"strings"
	"testing"
)

func TestParseRecordTail(t *testing.T) {
	const s4 = `http://stream4.qqmusic.qq.com/`
	// 主机号 92 个字符时网址恰为 128 字节，93 个时为 129 字节。
	url128 := `http://stream` + strings.Repeat("x", 92) + `.qqmusic.qq.com/123.wma`
	url129 := `http://stream` + strings.Repeat("x", 93) + `.qqmusic.qq.com/123.wma`
	p31, p32 := strings.Repeat("a", 31), strings.Repeat("a", 32)

	cases := []struct {
		name    string
		in      string
		url, p0 string // url 为空即应为假
	}{
		{"stream4 的 mid 键", s4 + `000JptHs40Om4N.wma0=000JptHs40Om4N&1=abc&2=def`, s4 + `000JptHs40Om4N.wma`, "000JptHs40Om4N"},
		{"streamundefined 的 id 键", `http://streamundefined.qqmusic.qq.com/268359544.wma0=000JptHs40Om4N&2=x`, `http://streamundefined.qqmusic.qq.com/268359544.wma`, "000JptHs40Om4N"},
		{"https", `https://stream4.qqmusic.qq.com/000AbCd.wma0=000AbCd&`, `https://stream4.qqmusic.qq.com/000AbCd.wma`, "000AbCd"},
		{"网址恰为 128 字节", url128 + `0=000AbCd&`, url128, "000AbCd"},
		{"参数 0 为 31 字节", s4 + `123.wma0=` + p31 + `&`, s4 + `123.wma`, p31},
		{"参数 0 之后的内容不看", s4 + `000AbCd.wma0=000AbCd&` + strings.Repeat("\xff", 500), s4 + `000AbCd.wma`, "000AbCd"},

		// 网址：128 字节内第一个 .wma 为止，须归为 mid 键或 id 键。
		{"128 字节内没有 .wma（网址 129 字节）", url129 + `0=000AbCd&`, "", ""},
		{"没有 .wma", s4 + `000AbCd.mp30=000AbCd&`, "", ""},
		{".WMA", s4 + `000AbCd.WMA0=000AbCd&`, "", ""},
		{"网址带查询串", s4 + `000AbCd?x=1.wma0=000AbCd&`, "", ""},
		{"网址带端口", `http://stream4.qqmusic.qq.com:80/000AbCd.wma0=000AbCd&`, "", ""},
		{"网址带用户信息", `http://u@stream4.qqmusic.qq.com/000AbCd.wma0=000AbCd&`, "", ""},
		{"主机不是 stream*", `http://example.com/000AbCd.wma0=000AbCd&`, "", ""},
		{"主机以 qqmusic.qq.com 开头而另有后缀", `http://stream4.qqmusic.qq.com.example.com/000AbCd.wma0=000AbCd&`, "", ""},
		{"主机的 * 含点", `http://stream4.a.qqmusic.qq.com/000AbCd.wma0=000AbCd&`, "", ""},
		{"只截到第一个 .wma", s4 + `abc.wmaxyz.wma0=abc&`, "", ""},
		{"id 超出 uint32", s4 + `4294967296.wma0=000AbCd&`, "", ""},

		// 紧接着须为 0=，其后到 & 为止（32 字节内）为非空的 ASCII 字母数字。
		{"没有 0=", s4 + `000AbCd.wma1=000AbCd&`, "", ""},
		{"只有 =", s4 + `000AbCd.wma=000AbCd&`, "", ""},
		{".wma 之后直接是 mid", s4 + `000AbCd.wma000AbCd&`, "", ""},
		{".wma 之后直接是字母数字（id 键）", s4 + `123.wmaabc&`, "", ""},
		{"参数 0 为空", s4 + `123.wma0=&1=x`, "", ""},
		{"参数 0 含连字符", s4 + `123.wma0=000Ab-Cd&`, "", ""},
		{"参数 0 含空格", s4 + `123.wma0=000Ab Cd&`, "", ""},
		{"参数 0 含非 ASCII", s4 + `123.wma0=000Ab中&`, "", ""},
		{"32 字节内没有 &", s4 + `123.wma0=` + p32 + `&`, "", ""},
		{"& 在第 200 字节（超出窗口）", s4 + `123.wma0=` + strings.Repeat("a", 161) + `&`, "", ""},

		// mid 键网址须与参数 0 相同。
		{"mid 键网址与参数 0 不同", s4 + `000AbCd.wma0=000Other&`, "", ""},
		{"mid 键网址与参数 0 只差大小写", s4 + `000AbCd.wma0=000abcd&`, "", ""},

		// =&0= 写法：.wma 之后为 =&0=，参数 0 自 =&0= 之后起算，& 须在其后 32 字节内。
		{"=&0= 写法的 mid 键", s4 + `000TestMid0001.wma=&0=000TestMid0001&1=7421224119753021581&2=x`, s4 + `000TestMid0001.wma`, "000TestMid0001"},
		{"=&0= 写法的 id 键（streamundefined）", `http://streamundefined.qqmusic.qq.com/268359544.wma=&0=000TestMid0001&2=x`, `http://streamundefined.qqmusic.qq.com/268359544.wma`, "000TestMid0001"},
		{"=&0= 写法的 https", `https://stream4.qqmusic.qq.com/000AbCd.wma=&0=000AbCd&`, `https://stream4.qqmusic.qq.com/000AbCd.wma`, "000AbCd"},
		{"=&0= 写法的网址恰为 128 字节", url128 + `=&0=000AbCd&`, url128, "000AbCd"},
		{"=&0= 写法的 id 键", s4 + `123.wma=&0=000AbCd&`, s4 + `123.wma`, "000AbCd"},
		{"=&0= 写法参数 0 为 31 字节（& 为其后第 32 字节）", s4 + `123.wma=&0=` + p31 + `&`, s4 + `123.wma`, p31},
		{"=&0= 写法之后的内容不看", s4 + `000AbCd.wma=&0=000AbCd&` + strings.Repeat("\xff", 500), s4 + `000AbCd.wma`, "000AbCd"},
		{"0= 写法后接 &2=（没有 &1=）", s4 + `000AbCd.wma0=000AbCd&2=x`, s4 + `000AbCd.wma`, "000AbCd"},
		{"=0= 不是 0= 写法", s4 + `000AbCd.wma=0=000AbCd&`, "", ""},
		{"&0= 不是 =&0= 写法", s4 + `000AbCd.wma&0=000AbCd&`, "", ""},
		{"==&0=", s4 + `000AbCd.wma==&0=000AbCd&`, "", ""},
		{"=&&0=", s4 + `000AbCd.wma=&&0=000AbCd&`, "", ""},
		{"=&1=", s4 + `000AbCd.wma=&1=000AbCd&`, "", ""},
		{"多一个空格的 =&0=", s4 + `000AbCd.wma =&0=000AbCd&`, "", ""},
		{"=&0= 写法之前只截到 =&", s4 + `000AbCd.wma=&`, "", ""},
		{"=&0= 写法之前只截到 =&0=", s4 + `000AbCd.wma=&0=`, "", ""},
		{"=&0= 写法参数 0 为空", s4 + `123.wma=&0=&1=x`, "", ""},
		{"=&0= 写法参数 0 含连字符", s4 + `123.wma=&0=000Test-Mid01&`, "", ""},
		{"=&0= 写法 32 字节内没有 &", s4 + `123.wma=&0=` + p32 + `&`, "", ""},
		{"=&0= 写法 mid 键网址与参数 0 不同", s4 + `000AbCd.wma=&0=000Other&`, "", ""},
		{"=&0= 写法 mid 键网址与参数 0 只差大小写", s4 + `000AbCd.wma=&0=000abcd&`, "", ""},

		// 尾部被截断。
		{"尾部止于 .wma", s4 + `000AbCd.wma`, "", ""},
		{"尾部止于 0", s4 + `000AbCd.wma0`, "", ""},
		{"尾部止于参数中途", s4 + `000AbCd.wma0=000Ab`, "", ""},
		{"尾部止于参数之后、& 之前", s4 + `000AbCd.wma0=000AbCd`, "", ""},
		{"尾部止于网址中途", s4 + `000Ab`, "", ""},

		// 须以 http 开头（大小写也算不合）。
		{"以 ttp 开头", `ttp://stream4.qqmusic.qq.com/000AbCd.wma0=000AbCd&`, "", ""},
		{"以 HTTP 开头", `HTTP://stream4.qqmusic.qq.com/000AbCd.wma0=000AbCd&`, "", ""},
		{"前面多一个字节", `xhttp://stream4.qqmusic.qq.com/000AbCd.wma0=000AbCd&`, "", ""},
		{"空", "", "", ""},
	}
	for _, c := range cases {
		got, ok := ParseRecordTail([]byte(c.in))
		want := RecordTail{URL: c.url, Param0: c.p0}
		if ok != (c.url != "") || got != want {
			t.Errorf("%s：ParseRecordTail = (%+v, %v)，应为 (%+v, %v)", c.name, got, ok, want, c.url != "")
		}
	}
	if s := s4 + `123.wma0=` + strings.Repeat("a", 161) + `&`; strings.IndexByte(s, '&') != 200 {
		t.Fatalf("用例自检：& 在第 %d 字节，应为 200", strings.IndexByte(s, '&'))
	}
	if len(url128) != 128 || len(url129) != 129 {
		t.Fatalf("用例自检：网址长 %d、%d，应为 128、129", len(url128), len(url129))
	}
}
