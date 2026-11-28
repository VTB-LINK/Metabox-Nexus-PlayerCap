package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// 随包使用说明的文件名（#215，设计 §4.10）。发行版附件只能用英文名，附件是 README.html；程序目录里另放一份
// 中文名的副本，给看不懂英文的主播。
const (
	readmeName        = "README.html"
	chineseReadmeName = "使用说明.html"
)

// syncChineseReadme 在 dir 里，README.html 存在、而 使用说明.html 不存在或内容不同时，把前者复制为后者。
// 先写临时文件再改名覆盖；失败只记 Detail 日志并返回不含路径的稳定短错误码，不影响启动。
//
// 在程序启动时做，不在下载流程里做：执行更新的是旧版程序，没有这段代码；新版重启后已是最新版，不再下载。
// 内容相同时不写，免得每次启动都改动程序目录。
func syncChineseReadme(dir string) string {
	src, err := os.ReadFile(filepath.Join(dir, readmeName))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			mainLog.Detail("Failed to copy %s to %s: %v", readmeName, chineseReadmeName, err)
			return "read_failed"
		}
		return ""
	}
	dst := filepath.Join(dir, chineseReadmeName)
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, src) {
		return ""
	}
	// 临时文件名固定：上一次写到一半留下的会被截断重写，不会越积越多。
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, src, 0o644); err != nil {
		os.Remove(tmp)
		mainLog.Detail("Failed to copy %s to %s: %v", readmeName, chineseReadmeName, err)
		return "write_failed"
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		mainLog.Detail("Failed to copy %s to %s: %v", readmeName, chineseReadmeName, err)
		return "rename_failed"
	}
	mainLog.Detail("Copied %s to %s", readmeName, chineseReadmeName)
	return ""
}
