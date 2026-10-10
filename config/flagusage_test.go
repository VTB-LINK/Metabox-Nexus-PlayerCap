package config

// 本文件只测 flagUsage：-h 里一个参数的那一项（issue #92）。开关带取值提示 [true|false]，其余各类参数
// 的写法与 flag 包默认格式相同、不列默认值。经真实的 Load 输出的整份用法见根包 cliusage_test.go。

import (
	"flag"
	"testing"
	"time"
)

// TestFlagUsage 逐类参数断言完整的一项。非开关各例的期望值是加提示之前 printFlag 的输出：本用例先于
// 改动写成，当时非开关各例绿、开关各例红（实测）。
//
// 变异自证（隔离的 worktree，实测）：不加提示（改动前的写法）→ 开关 4 例红；给所有参数都加提示 → 非
// 开关各例红；按名字判断（以 audio-normalize 结尾才加）→ verbose、once、x 三例红；isBoolFlag 只看
// Value 实现了 IsBoolFlag、不调用它 →「声明自己不是开关的」红；提示写成 [true/false]、提示与名字之间
// 不留空格 → 开关 4 例红。
func TestFlagUsage(t *testing.T) {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.Bool("audio-normalize", false, "音量归一化开关")
	fs.Bool("verbose", true, "名单之外的开关")
	fs.BoolFunc("once", "BoolFunc 注册的开关", func(string) error { return nil })
	fs.Bool("x", false, "单字母的开关")
	fs.Var(declinedBool{}, "maybe", "声明自己不是开关的")
	fs.Int("offset", -200, "歌词延迟（毫秒）")
	fs.Int("audio-normalize-target", -14, "名字以开关的名字开头的整数参数")
	fs.String("addr", "0.0.0.0:8765", "监听地址")
	fs.String("empty", "", "")
	fs.Float64("ratio", 0, "小数")
	fs.Duration("wait", time.Second, "时长")
	fs.Uint("count", 0, "无符号整数")
	fs.Func("fn", "回调", func(string) error { return nil })
	fs.Int("quoted", 0, "带 `名字` 的说明")
	fs.Int("n", 0, "单字母的整数参数")

	cases := []struct{ name, want string }{
		{"audio-normalize", "  -audio-normalize [true|false]\n    \t音量归一化开关"},
		{"verbose", "  -verbose [true|false]\n    \t名单之外的开关"},
		{"once", "  -once [true|false]\n    \tBoolFunc 注册的开关"},
		{"x", "  -x [true|false]\n    \t单字母的开关"},

		{"maybe", "  -maybe value\n    \t声明自己不是开关的"},
		{"offset", "  -offset int\n    \t歌词延迟（毫秒）"},
		{"audio-normalize-target", "  -audio-normalize-target int\n    \t名字以开关的名字开头的整数参数"},
		{"addr", "  -addr string\n    \t监听地址"},
		{"empty", "  -empty string\n    \t"},
		{"ratio", "  -ratio float\n    \t小数"},
		{"wait", "  -wait duration\n    \t时长"},
		{"count", "  -count uint\n    \t无符号整数"},
		{"fn", "  -fn value\n    \t回调"},
		{"quoted", "  -quoted 名字\n    \t带 名字 的说明"},
		{"n", "  -n int\n    \t单字母的整数参数"},
	}
	for _, c := range cases {
		if got := flagUsage(fs.Lookup(c.name)); got != c.want {
			t.Errorf("flagUsage(-%s) = %q，期望 %q", c.name, got, c.want)
		}
	}
}
