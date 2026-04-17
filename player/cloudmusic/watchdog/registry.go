package watchdog

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// PatchRegistryAutoStart modifies the HKCU Run registry key for CloudMusic
// to append the debugging port flag, effectively satisfying "Option B".
func PatchRegistryAutoStart() {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		log.Error("打开注册表 Run 键失败: %v", err)
		return
	}
	defer key.Close()

	val, _, err := key.GetStringValue("cloudmusic")
	if err != nil {
		// cloudmusic not set to autostart by user, or key doesn't exist
		return
	}

	if !strings.Contains(val, DebugFlag) {
		newVal := val + " " + DebugFlag
		err = key.SetStringValue("cloudmusic", newVal)
		if err != nil {
			log.Error("设置注册表 Run 值失败: %v", err)
		} else {
			log.Success("已修补网易云自启动参数: %s", newVal)
		}
	} else {
		log.Info("网易云自启动调试参数已就绪")
	}
}
