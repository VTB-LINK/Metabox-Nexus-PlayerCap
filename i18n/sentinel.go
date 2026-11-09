package i18n

// Sentinel 返回一个包级 sentinel 错误。msg 是英文原文（中文补进译表），Error() 每次被调用时才经 T
// 本地化：包级 var 在 init 时求值、早于 SetLanguage，写成 errors.New(T(msg)) 会冻结成中文
// （AGENTS.md §6.4 ③）。返回指针，errors.Is 按身份比较，与 errors.New 相同。
func Sentinel(msg string) error { return &sentinel{msg} }

type sentinel struct{ msg string }

func (e *sentinel) Error() string { return T(e.msg) }
