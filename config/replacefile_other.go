//go:build !windows

package config

import "Metabox-Nexus-PlayerCap/i18n"

// moveNoReplace 的非 Windows 桩：此平台上硬链接失败时没有退路，一律报错，src 与 dst 都不动。只有非 Windows
// 构建会编到（原为 AGENTS.md §0 第 3 条的 GOOS=linux 自检而设，该条已撤销，#169，桩保留未删）；Linux 上
// 常见的文件系统都支持硬链接，走不到这里。
func moveNoReplace(src, dst string) error {
	return i18n.Errorf("hard links are unavailable and this platform has no fallback for placing the file")
}
