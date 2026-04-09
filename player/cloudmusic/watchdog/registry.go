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
		log.Error("Error opening Run key: %v", err)
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
			log.Error("Error setting new run value: %v", err)
		} else {
			log.Success("Patched CloudMusic auto-start: %s", newVal)
		}
	} else {
		log.Info("CloudMusic auto-start is already patched.")
	}
}
