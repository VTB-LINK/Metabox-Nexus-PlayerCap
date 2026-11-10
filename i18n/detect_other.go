//go:build !windows

package i18n

// DetectSystemLanguage 的非 Windows 桩：只有非 Windows 构建会编到，运行期永不触及。原为
// AGENTS.md §0.3 的 GOOS=linux 自检而设，该条已撤销（#169），桩保留未删。返回 English 与
// 「非中文即英文」的规则一致。
func DetectSystemLanguage() Lang { return English }
