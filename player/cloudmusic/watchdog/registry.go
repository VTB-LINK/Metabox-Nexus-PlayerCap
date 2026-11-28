package watchdog

import (
	"strings"

	"Metabox-Nexus-PlayerCap/rum"
	"golang.org/x/sys/windows/registry"
)

// PatchRegistryAutoStart modifies the HKCU Run registry key for CloudMusic
// to append the debug + keepalive launch flags, so user-autostarted CloudMusic
// also gets debugging port and anti-occlusion flags.
func PatchRegistryAutoStart() {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		log.Error("Failed to open registry Run key: %v", err)
		rum.Track(rum.CatPlayer, "player_intervention", rum.Props{
			"player":  "cloudmusicv3",
			"kind":    "autostart_reg_patch",
			"outcome": "reg_err",
		})
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
			log.Error("Failed to set registry Run value: %v", err)
			rum.Track(rum.CatPlayer, "player_intervention", rum.Props{
				"player":  "cloudmusicv3",
				"kind":    "autostart_reg_patch",
				"outcome": "reg_err",
			})
		} else {
			log.Success("Patched NetEase Cloud Music autostart flags: %s", newVal)
			rum.Track(rum.CatPlayer, "player_intervention", rum.Props{
				"player":  "cloudmusicv3",
				"kind":    "autostart_reg_patch",
				"outcome": "ok",
			})
		}
	} else {
		log.Info("NetEase Cloud Music autostart debug/keepalive flags already present")
	}
}
