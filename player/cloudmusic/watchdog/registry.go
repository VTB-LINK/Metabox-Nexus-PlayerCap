package watchdog

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// PatchRegistryAutoStart modifies the HKCU Run registry key for CloudMusic
// to append the debug + keepalive launch flags, so user-autostarted CloudMusic
// also gets debugging port and anti-occlusion flags.
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

	// 追加缺失的启动参数（已存在的不重复添加）
	newVal := val
	for _, f := range LaunchFlags {
		if !strings.Contains(newVal, f) {
			newVal += " " + f
		}
	}

	if newVal != val {
		if err = key.SetStringValue("cloudmusic", newVal); err != nil {
			log.Error("设置注册表 Run 值失败: %v", err)
		} else {
			log.Success("已修补网易云自启动参数: %s", newVal)
		}
	} else {
		log.Info("网易云自启动调试/保活参数已就绪")
	}
}
