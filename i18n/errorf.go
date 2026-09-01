package i18n

import "fmt"

// Errorf 同 fmt.Errorf，但先把 format 过 i18n.T 本地化。
//
// 写成「format 先赋值、再原样传入 fmt.Errorf」，是为了让 go vet 的 printf 分析器认出本
// 函数是 fmt.Errorf 的包装器：于是它转而对**调用点的常量 format**做占位符校验（%w/%v 与
// 实参匹配），而不误报「非常量 format string」。故调用点的 format 必须是字符串常量。
func Errorf(format string, a ...any) error {
	format = T(format)
	return fmt.Errorf(format, a...)
}
