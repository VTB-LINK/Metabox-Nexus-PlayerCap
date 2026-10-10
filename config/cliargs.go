package config

import (
	"flag"
	"strconv"
	"strings"

	"Metabox-Nexus-PlayerCap/i18n"
)

// isBoolFlag 报告 f 是否为开关，判据同 flag 包：Value 实现了 IsBoolFlag() 且返回真。
func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// joinBoolArgs 把「开关 + 紧跟的布尔字面量」并成 -x=值，其余实参原样返回；不改动 args。lookup 按名字
// 查已注册的 flag（flag.Lookup），查不到返回 nil。
//
// flag 包的开关不取下一个实参：`-audio-normalize false` 被解析为开，false 成了第一个不是参数的词，解析
// 到此为止，其后的参数全部不生效（issue #92）。解析之前先并成等号写法，两种写法就都按字面生效。
//
// 走法照 flag.FlagSet 的 parseOne，停在它停下的位置，从那里起原样返回：
//   - 以一个或两个横线开头的才是参数；`-x=值` 自带取值。
//   - 取值型参数吃掉下一个实参，那个实参随之跳过：`-offset -200` 的 -200、`-addr true` 的 true 都是取值。
//   - 第一个不是参数的词、没注册的名字（含 -h）：解析在这里停下或报错，不再往后合并。`--` 与 flag 包
//     不认的写法（`---x`、`-=x`）也在其内：去掉横线与等号之后的名字为空或以横线开头，flag 包不允许
//     注册以横线开头的名字，空名字本仓库不注册。
//
// 是不是开关按 isBoolFlag 判断，不写死名字。布尔字面量取 strconv.ParseBool 认的那一组，与等号写法能写的
// 相同；紧跟的不是这一组里的词时不合并，裸写仍表示开。横线个数照旧。
func joinBoolArgs(args []string, lookup func(name string) *flag.Flag) []string {
	out := make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		s := args[i]
		if len(s) < 2 || s[0] != '-' {
			break
		}
		name, _, hasValue := strings.Cut(strings.TrimPrefix(s[1:], "-"), "=")
		f := lookup(name)
		if f == nil {
			break
		}
		out = append(out, s)
		i++
		if hasValue || i == len(args) {
			continue
		}
		if !isBoolFlag(f) {
			out = append(out, args[i])
			i++
		} else if _, err := strconv.ParseBool(args[i]); err == nil {
			out[len(out)-1] = s + "=" + args[i]
			i++
		}
	}
	return append(out, args[i:]...)
}

// extraArgError 为解析之后剩余的实参（flag.Args）生成报错，点名其中第一个；没有剩余实参时返回 nil。
// flag 包停在第一个不是参数的词上（漏了横线的 offset、开关后面的 yes、-- 之后的内容），它与其后的实参
// 都没有被解析。Load 据此报错退出，不静默忽略（issue #92）。
func extraArgError(rest []string) error {
	if len(rest) == 0 {
		return nil
	}
	return i18n.Errorf("unexpected argument %q: it is neither a flag nor a flag's value; nothing from it onward was applied", rest[0])
}
