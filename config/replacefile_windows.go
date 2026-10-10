//go:build windows

package config

import (
	"os"

	"golang.org/x/sys/windows"
)

// moveNoReplace 把 src 改名为 dst。dst 已存在时失败，返回的错误满足 errors.Is(err, fs.ErrExist)，src 与
// dst 都不动；成功时 src 整份移到 dst，没有写到一半的状态。
//
// MoveFileEx 的标志传 0：不带 MOVEFILE_REPLACE_EXISTING（os.Rename 带着它，会覆盖已存在的目标），也不带
// MOVEFILE_COPY_ALLOWED（带上它，改名做不成时会退化成复制加删除）。
//
// 路径原样交给系统调用，不加 \\?\ 前缀。超过 MAX_PATH 的路径同样可用：Go 运行时在 Windows 10 1703
// （10.0.15063）及以上给整个进程置上 IsLongPathAwareProcess（runtime 的 initLongPathSupport，不看注册表的
// LongPathsEnabled），os 包在这些系统上也不再加前缀。更早的系统上超长路径会失败，按放置失败处理。
func moveNoReplace(src, dst string) error {
	from, err := windows.UTF16PtrFromString(src)
	if err == nil {
		var to *uint16
		if to, err = windows.UTF16PtrFromString(dst); err == nil {
			err = windows.MoveFileEx(from, to, 0)
		}
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}
