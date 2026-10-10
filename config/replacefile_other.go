//go:build !windows

package config

import "Metabox-Nexus-PlayerCap/i18n"

// moveNoReplace 的非 Windows 桩：此平台上硬链接失败时没有退路，一律报错，src 与 dst 都不动。只有 CI 的
// GOOS=linux 构建（AGENTS.md §0 第 3 条）会编到；Linux 上常见的文件系统都支持硬链接，走不到这里。
func moveNoReplace(src, dst string) error {
	return i18n.Errorf("hard links are unavailable and this platform has no fallback for placing the file")
}
