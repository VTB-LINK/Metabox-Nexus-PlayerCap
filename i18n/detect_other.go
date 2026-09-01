//go:build !windows

package i18n

// DetectSystemLanguage 的非 Windows 桩：仅 CI 的 GOOS=linux 构建（logger/ 与 config/ 的
// 可构建性自检，见 AGENTS.md §0.3）会编到，运行期永不触及。返回 English 与「非中文即英文」
// 的规则一致。
func DetectSystemLanguage() Lang { return English }
