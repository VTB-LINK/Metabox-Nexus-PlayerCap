// Package i18n 提供运行期文案的中英切换。
//
// 设计要点（见 AGENTS.md §0.3 与 §6.4）：
//   - 本包是纯 Go，可 GOOS=linux 构建 —— logger/ 与 config/ 依赖它，而这两个包必须保持
//     Linux 可构建（tools/genconfig 的打包流水线）。Windows-only 的语言探测隔离在
//     detect_windows.go（!windows 侧是 detect_other.go 桩），不污染本文件。
//   - 语言由 main 在启动早期一次性注入（SetLanguage），此后只读；运行期不切换。
//   - 文案以「英文原文」为 key：英文是源语言，中文是译文表（chinese，见 messages*.go）。
//     语言为英文时 T 恒等返回原文；语言为中文时查表，未收录的回退英文原文。
//     issue #61 之前方向相反（中文原文为 key、英文为译文表）；改向时逐条对调，两种语言下的
//     输出与改向前逐字相同。
package i18n

// Lang 是受支持的界面语言。中文是零值（默认），因此未调用 SetLanguage 的场景
// （如单元测试、包级变量的初始化）一律走中文，既有的 verbatim 中文门禁不受影响。
type Lang int

const (
	Chinese Lang = iota // 简体中文（查译表）
	English             // 英文（源语言）
)

// current 是进程当前语言。SetLanguage 在启动早期设定一次，之后只读，
// 运行期不切换，故无需加锁。
var current = Chinese

// SetLanguage 设定进程语言。由 main 在任何用户可见输出之前调用一次。
func SetLanguage(l Lang) { current = l }

// Language 返回当前语言。
func Language() Lang { return current }

// T 把一条英文原文映射为当前语言的文案。
// 英文：恒等返回原文。中文：查表命中返回译文，未命中回退原文（不丢信息）。
// 用于 logger 边界，以及需要本地化的格式串与错误文案。
func T(s string) string {
	if current == Chinese {
		if t, ok := chinese[s]; ok {
			return t
		}
	}
	return s
}
