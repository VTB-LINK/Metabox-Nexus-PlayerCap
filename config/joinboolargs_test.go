package config

// 本文件只测 joinBoolArgs：解析之前把「开关 + 紧跟的布尔字面量」并成 -x=值，其余实参原样（issue #92）。
//
// 它必须照 flag 包的切分规则走到与 flag.FlagSet.Parse 同一个位置：并多了，取值型参数的取值或 Parse 本该
// 报错的词被改写；并少了，`-audio-normalize false` 的 false 仍是第一个不是参数的词，其后的参数全部不解析。
// 各例断言完整的输出切片。经真实的 Load 的效果见根包 cliswitch_test.go。

import (
	"flag"
	"io"
	"slices"
	"strings"
	"testing"
)

// declinedBool 实现了 IsBoolFlag 而返回假：flag 包把它当取值型参数。
type declinedBool struct{}

func (declinedBool) String() string   { return "" }
func (declinedBool) Set(string) error { return nil }
func (declinedBool) IsBoolFlag() bool { return false }

// joinTestFlags 返回一组测试用的 flag：三个与出货代码同名的开关、两个别的开关、一个声明自己不是开关的，
// 与四个取值型参数。
func joinTestFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	fs.Bool("audio-normalize", false, "")
	fs.Bool("qqmusic-audio-normalize", false, "")
	fs.Bool("wesing-audio-normalize", false, "")
	fs.Bool("verbose", false, "")                              // 现有两类开关之外的名字
	fs.BoolFunc("once", "", func(string) error { return nil }) // 不是 flag.Bool 注册的开关
	fs.Var(declinedBool{}, "maybe", "")
	fs.Int("offset", 0, "")
	fs.Int("poll", 0, "")
	fs.String("addr", "", "")
	fs.String("prior-player", "", "")
	return fs
}

// TestJoinBoolArgs 逐例断言规整后的完整实参表。in 与 want 以空格分隔；含空串或带空格的实参的各例另列在
// 表后。want 为空表示原样返回。
//
// 变异自证（隔离的 worktree，实测）。以下各条均红：
//   - 不做合并（原样返回）。
//   - 合并时不跳过取值型参数的取值；取值型参数的等号写法也吃下一个实参；开关的等号写法后面的字面量也
//     并进来；合并之后不跳过已并进来的字面量。
//   - 裸词之后、没注册的名字之后、-- 之后继续往后合并。
//   - 是不是开关写死成 audio-normalize 与 -audio-normalize 后缀两种名字（名单之外的开关各例红）；只看
//     Value 实现了 IsBoolFlag、不调用它。
//   - 布尔字面量只认 true 与 false；字面量不分大小写。
//   - 合并后一律写成一个横线；查名字时只去掉一个横线；等号之后的部分也算进名字。
//   - 就地改写入参；不查实参长度（空串处越界）；不看首字符是不是横线；末尾的参数之后不查越界。
func TestJoinBoolArgs(t *testing.T) {
	cases := []struct{ name, in, want string }{
		// 空格写法：strconv.ParseBool 认的全部字面量
		{"true", "-audio-normalize true", "-audio-normalize=true"},
		{"false", "-audio-normalize false", "-audio-normalize=false"},
		{"1", "-audio-normalize 1", "-audio-normalize=1"},
		{"0", "-audio-normalize 0", "-audio-normalize=0"},
		{"t", "-audio-normalize t", "-audio-normalize=t"},
		{"f", "-audio-normalize f", "-audio-normalize=f"},
		{"T", "-audio-normalize T", "-audio-normalize=T"},
		{"F", "-audio-normalize F", "-audio-normalize=F"},
		{"TRUE", "-audio-normalize TRUE", "-audio-normalize=TRUE"},
		{"FALSE", "-audio-normalize FALSE", "-audio-normalize=FALSE"},
		{"True", "-audio-normalize True", "-audio-normalize=True"},
		{"False", "-audio-normalize False", "-audio-normalize=False"},

		// 等号写法自带取值，原样
		{"等号写法", "-audio-normalize=false", ""},
		{"等号写法之后的参数照常规整", "-audio-normalize=true -verbose false", "-audio-normalize=true -verbose=false"},
		{"等号写法后面的字面量不并进来", "-audio-normalize=false true", ""},
		{"等号后为空也算自带取值", "-audio-normalize= false", ""},
		{"取值型参数的等号写法不吃下一个实参", "-poll=50 -audio-normalize false", "-poll=50 -audio-normalize=false"},
		{"取值里另有等号", "-addr=a=b -audio-normalize false", "-addr=a=b -audio-normalize=false"},

		// 裸写表示开
		{"裸写在末尾", "-audio-normalize", ""},
		{"裸写在末尾，前有别的参数", "-poll 50 -audio-normalize", ""},
		{"裸写后接别的参数", "-audio-normalize -poll 50", ""},
		{"裸写后接别的开关的等号写法", "-audio-normalize -verbose=false", ""},

		// 紧跟的不是布尔字面量：不合并，且规整到此为止（它是第一个不是参数的词）
		{"yes", "-audio-normalize yes -verbose false", ""},
		{"on", "-audio-normalize on", ""},
		{"大小写混写", "-audio-normalize TRue", ""},
		{"2", "-audio-normalize 2", ""},
		{"字面量后多一个字母", "-audio-normalize falsee", ""},
		{"带引号的字面量", `-audio-normalize "false"`, ""},

		// 横线个数照旧
		{"两个横线", "--audio-normalize false", "--audio-normalize=false"},
		{"两个横线的等号写法", "--audio-normalize=false", ""},
		{"两个横线裸写", "--audio-normalize --poll 50", ""},
		{"两种横线混用", "--poll 50 -audio-normalize false --verbose true", "--poll 50 -audio-normalize=false --verbose=true"},

		// 每播放器的开关与名单之外的开关
		{"qqmusic", "-qqmusic-audio-normalize true", "-qqmusic-audio-normalize=true"},
		{"wesing", "-wesing-audio-normalize false", "-wesing-audio-normalize=false"},
		{"名单之外的开关", "-verbose false", "-verbose=false"},
		{"BoolFunc 注册的开关", "-once false -poll 50", "-once=false -poll 50"},
		{"实现了 IsBoolFlag 而返回假的是取值型", "-maybe true -audio-normalize false", "-maybe true -audio-normalize=false"},
		{"实现了 IsBoolFlag 而返回假的，取值像开关", "-maybe -audio-normalize false", ""},

		// 取值型参数吃掉下一个实参：那个实参不是开关，也不是开关的取值
		{"取值是 true", "-addr true -audio-normalize", ""},
		{"取值是 true，其后的开关照常合并", "-addr true -audio-normalize false", "-addr true -audio-normalize=false"},
		{"取值是 false", "-prior-player false", ""},
		{"负数取值", "-offset -200", ""},
		{"负数取值之后的开关照常合并", "-offset -200 -audio-normalize false -poll 50", "-offset -200 -audio-normalize=false -poll 50"},
		{"取值像开关", "-addr -audio-normalize false", ""},
		{"取值像开关，其后另有开关", "-addr -verbose -audio-normalize false", "-addr -verbose -audio-normalize=false"},
		{"取值是两个横线", "-addr -- -audio-normalize false", "-addr -- -audio-normalize=false"},
		{"取值型参数在末尾缺取值", "-audio-normalize false -offset", "-audio-normalize=false -offset"},

		// -- 终止解析
		{"-- 之后", "-- -audio-normalize false", ""},
		{"-- 之前合并、之后不动", "-audio-normalize false -- -verbose false", "-audio-normalize=false -- -verbose false"},
		{"-- 紧跟在裸写的开关之后", "-audio-normalize -- false", ""},
		{"-- 在末尾", "-verbose false --", "-verbose=false --"},

		// 第一个不是参数的词之后不动
		{"开头就是裸词", "offset -200 -audio-normalize false", ""},
		{"中间的裸词", "-poll 50 foo -audio-normalize false", ""},
		{"裸词之前合并、之后不动", "-verbose true foo -audio-normalize false", "-verbose=true foo -audio-normalize false"},
		{"合并之后多出的字面量", "-audio-normalize true false -verbose false", "-audio-normalize=true false -verbose false"},
		{"单个横线", "- -audio-normalize false", ""},
		{"裸词去掉首字母恰是取值型参数的名字", "xpoll 50 -audio-normalize false", ""},
		{"裸词去掉首字母恰是开关的名字", "xverbose false", ""},

		// 没注册的名字与 flag 包不认的写法：原样留给 Parse 报错，其后不动
		{"没注册的参数", "-nosuch -audio-normalize false", ""},
		{"没注册的参数的等号写法", "-nosuch=1 -audio-normalize false", ""},
		{"名字只差一个字母", "-audio-normalizer false", ""},
		{"-h", "-h -audio-normalize false", ""},
		{"-help", "--help -audio-normalize false", ""},
		{"没注册的参数之前合并、之后不动", "-verbose false -nosuch -audio-normalize false", "-verbose=false -nosuch -audio-normalize false"},
		{"三个横线", "---audio-normalize false", ""},
		{"横线后直接是等号", "-=1 -audio-normalize false", ""},
		{"两个横线后直接是等号", "--=1 -audio-normalize false", ""},

		// 重复与连写
		{"同一个开关两次", "-audio-normalize false -audio-normalize true", "-audio-normalize=false -audio-normalize=true"},
		{"同一个开关两次，一次裸写", "-audio-normalize -audio-normalize false", "-audio-normalize -audio-normalize=false"},
		{"连续两个开关，前一个裸写", "-audio-normalize -qqmusic-audio-normalize false", "-audio-normalize -qqmusic-audio-normalize=false"},
		{"连续三个开关都带取值", "-audio-normalize true -qqmusic-audio-normalize false -verbose 0 -poll 50",
			"-audio-normalize=true -qqmusic-audio-normalize=false -verbose=0 -poll 50"},
		{"开关夹在取值型参数之间", "-offset -300 -audio-normalize false -poll 50", "-offset -300 -audio-normalize=false -poll 50"},
	}
	for _, c := range cases {
		want := c.want
		if want == "" {
			want = c.in
		}
		checkJoin(t, c.name, strings.Fields(c.in), strings.Fields(want))
	}

	// 含空串、带空格的实参与空表：strings.Fields 表达不了。
	checkJoin(t, "空串是取值", []string{"-prior-player", "", "-audio-normalize", "false"}, []string{"-prior-player", "", "-audio-normalize=false"})
	checkJoin(t, "空串不是布尔字面量", []string{"-audio-normalize", "", "-verbose", "false"}, []string{"-audio-normalize", "", "-verbose", "false"})
	checkJoin(t, "空串在开头", []string{"", "-audio-normalize", "false"}, []string{"", "-audio-normalize", "false"})
	checkJoin(t, "字面量带空格", []string{"-audio-normalize", "false ", "-verbose", "false"}, []string{"-audio-normalize", "false ", "-verbose", "false"})
	checkJoin(t, "取值带空格", []string{"-addr", "a b", "-audio-normalize", "false"}, []string{"-addr", "a b", "-audio-normalize=false"})
	checkJoin(t, "空表", []string{}, []string{})
	checkJoin(t, "nil", nil, nil)
}

// checkJoin 断言 joinBoolArgs(in) 与 want 逐项相同（nil 与空表不作区分），且没有改动 in。
func checkJoin(t *testing.T, name string, in, want []string) {
	t.Helper()
	before := slices.Clone(in)
	got := joinBoolArgs(in, joinTestFlags().Lookup)
	if !slices.Equal(got, want) {
		t.Errorf("%s：joinBoolArgs(%q) = %q，期望 %q", name, in, got, want)
	}
	if !slices.Equal(in, before) {
		t.Errorf("%s：入参被改成了 %q，原为 %q", name, in, before)
	}
}

// TestJoinBoolArgsStopsWhereParseStops 以 flag 包自己的解析为准，核对规整没有越过 Parse 停下的位置：
// 规整后的实参表交给 Parse，剩余实参（Args）必须是原实参表的一段后缀，逐项相同。并错位置（把取值型参数的
// 取值、-- 之后或裸词之后的内容并掉）时，剩余实参里会出现原表没有的 -x=值，或少一项。
//
// 变异自证（隔离的 worktree，实测）：裸词之后、没注册的名字之后、-- 之后继续往后合并，就地改写入参 → 红。
func TestJoinBoolArgsStopsWhereParseStops(t *testing.T) {
	for _, in := range [][]string{
		{"-audio-normalize", "false", "-poll", "50"},
		{"-poll", "50", "foo", "-audio-normalize", "false"},
		{"-audio-normalize", "yes", "-audio-normalize", "false"},
		{"-audio-normalize", "true", "false", "-verbose", "false"},
		{"-poll", "50", "--", "-audio-normalize", "false"},
		{"--", "-audio-normalize", "false"},
		{"offset", "-200", "-audio-normalize", "false"},
		{"-addr", "true", "-audio-normalize", "false", "bar", "-verbose", "true"},
		{"-offset", "-200", "-audio-normalize", "false", "-", "-verbose", "true"},
	} {
		fs := joinTestFlags()
		fs.SetOutput(io.Discard)
		if err := fs.Parse(joinBoolArgs(in, fs.Lookup)); err != nil {
			t.Errorf("%q 规整后解析出错: %v", in, err)
			continue
		}
		rest := fs.Args()
		if len(rest) > len(in) || !slices.Equal(rest, in[len(in)-len(rest):]) {
			t.Errorf("%q 规整后的剩余实参 %q 不是原实参表的后缀", in, rest)
		}
	}
}
