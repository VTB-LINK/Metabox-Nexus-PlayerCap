package i18n

import "fmt"

// Errorf 同 fmt.Errorf，但 format 先过 T 本地化。调用点的 format 必须是字符串常量（英文原文）。
//
// 英文环境下 T 恒等返回原文，这一支把 format 与 a 原样转发给 fmt.Errorf。go vet 的 printf 分析器
// 靠这一句认出本函数是 fmt.Errorf 的包装器，转而对每个调用点的常量 format 做占位符校验（%w/%v
// 与实参是否匹配）。分析器只认「format 参数本身、a... 原样」的转发，且在见到这样的调用之前函数体里
// 不得给 format 赋值；写成 format = T(format) 再转发，它就不再把本函数当包装器，调用点的占位符
// 写错也不报（实测，见 TestErrorfTranslatesFormat 的注释）。改这个函数时保住这一支。
func Errorf(format string, a ...any) error {
	if current == English {
		return fmt.Errorf(format, a...)
	}
	return fmt.Errorf(T(format), a...)
}
