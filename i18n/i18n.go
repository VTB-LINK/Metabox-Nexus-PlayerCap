// Package i18n 提供运行期文案的中英切换。
//
// 设计要点（见 AGENTS.md §0.3 与项目 i18n 决策）：
//   - 本包是纯 Go，可 GOOS=linux 构建 —— logger/ 与 config/ 依赖它，而这两个包必须保持
//     Linux 可构建（tools/genconfig 的打包流水线）。Windows-only 的语言探测隔离在
//     detect_windows.go（!windows 侧是 detect_other.go 桩），不污染本文件。
//   - 语言由 main 在启动早期一次性注入（SetLanguage），此后只读；运行期不切换。
//   - 文案以「中文原文」为 key：中文是源语言，英文是译文表。语言为中文时 T 恒等返回原文，
//     故在补齐英文表之前、以及中文系统上，输出与改造前逐字一致（隐私告示与既有 verbatim
//     中文门禁均不受影响）。
package i18n

// Lang 是受支持的界面语言。中文是零值（默认），因此未调用 SetLanguage 的场景
// （如单元测试）一律走中文，既有的 verbatim 中文门禁不受影响。
type Lang int

const (
	Chinese Lang = iota // 简体中文（源语言）
	English             // 英文
)

// current 是进程当前语言。SetLanguage 在启动早期设定一次，之后只读，
// 运行期不切换，故无需加锁。
var current = Chinese

// SetLanguage 设定进程语言。由 main 在任何用户可见输出之前调用一次。
func SetLanguage(l Lang) { current = l }

// Language 返回当前语言。
func Language() Lang { return current }

// T 把一条中文原文映射为当前语言的文案。
// 中文：恒等返回原文。英文：查表命中返回译文，未命中回退原文（不丢信息）。
// 用于 logger 边界，以及需要本地化的格式串与错误文案。
func T(s string) string {
	if current == English {
		if t, ok := english[s]; ok {
			return t
		}
	}
	return s
}

// ListSep 返回当前语言的行内列表分隔符：中文顿号 / 英文逗号加空格。
func ListSep() string {
	if current == English {
		return ", "
	}
	return "，"
}
