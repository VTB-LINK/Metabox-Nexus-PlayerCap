package launch

import (
	"fmt"
	"strings"

	"Metabox-Nexus-PlayerCap/i18n"
)

// ErrExeUnknown 表示读不到播放器进程的映像路径，无从确知它不是微软商店版（见 Gate）。
var ErrExeUnknown = i18n.Sentinel("the player's executable path could not be read")

// PackagedError 表示播放器装在 WindowsApps 下，即微软商店版（MSIX 包）。
type PackagedError struct {
	ExePath string
}

func (e *PackagedError) Error() string {
	return fmt.Sprintf(i18n.T("%s is under WindowsApps (Microsoft Store installation)"), e.ExePath)
}

// Gate 报告 exePath 是否允许被结束、重启、修补或注入（issue #212）。各播放器在发现运行中的进程、取得映像路径之后，
// 先于该路径上的一切后续判断与侵入操作调用它；被操作的同名进程不止一个时逐个调用，任一不过即整体拒绝。
//
// 返回 nil：exePath 非空，且没有任何目录段（不含末段文件名）与 "WindowsApps" 忽略大小写相等。
// 返回 ErrExeUnknown：exePath 为空。路径未知即拒绝：确知不是商店版才动手。
// 返回 *PackagedError：在 WindowsApps 下。
//
// 商店版的安装目录只读、启动参数由包清单决定：带参数重启起不来，补丁写不进，结束了也拉不回来（#212 的实例：
// 网易云被 taskkill 结束后，经 cmd.exe 拉起以退出码 1 失败）。判据只看目录段，按 \ 与 / 切分：其他卷的
// D:\WindowsApps、\\?\ 与 \Device\... 前缀的写法同样命中；WindowsAppsBackup 这类非完整段不命中。段尾的点与
// 空格去掉再比较：Win32 路径解析时去掉它们，WindowsApps. 指的就是 WindowsApps。不访问文件系统，8.3 短名写法
// （如 WINDOW~1）认不出。
func Gate(exePath string) error {
	if exePath == "" {
		return ErrExeUnknown
	}
	dir := exePath[:strings.LastIndexAny(exePath, `\/`)+1]
	for _, seg := range strings.FieldsFunc(dir, func(r rune) bool { return r == '\\' || r == '/' }) {
		if strings.EqualFold(strings.TrimRight(seg, ". "), "WindowsApps") {
			return &PackagedError{ExePath: exePath}
		}
	}
	return nil
}
