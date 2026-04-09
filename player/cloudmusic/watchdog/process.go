package watchdog

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/logger"

	"github.com/shirou/gopsutil/v3/process"
)

var log = logger.New("Watchdog")

const TargetProcessName = "cloudmusic.exe"
const DebugFlag = "--remote-debugging-port=9222"

// EnsureDebugMode checks if cloudmusic is running. If so, checks if it has the debug flag.
// If not, it kills it and restarts it with the debug flag.
// Returns true if the process was restarted, so we can wait a bit before connecting.
func EnsureDebugMode() (bool, error) {
	procs, err := process.Processes()
	if err != nil {
		return false, fmt.Errorf("failed to list processes: %v", err)
	}

	var hasNetease bool
	var hasDebugFlag bool
	var exePath string
	var pidsToKill []int

	for _, p := range procs {
		name, err := p.Name()
		if err != nil {
			continue
		}
		if strings.EqualFold(name, TargetProcessName) {
			hasNetease = true
			pidsToKill = append(pidsToKill, int(p.Pid))

			// Try to get exe path if we haven't already
			if exePath == "" {
				ep, err := p.Exe()
				if err == nil && ep != "" {
					exePath = ep
				}
			}

			// Check command line
			cmdline, err := p.Cmdline()
			if err == nil {
				if strings.Contains(cmdline, DebugFlag) {
					hasDebugFlag = true
				}
			}
		}
	}

	if !hasNetease {
		// Not running at all, nothing to do
		return false, nil
	}

	if hasDebugFlag {
		// Already running with debugging enabled
		return false, nil
	}

	log.Warn("Found %s without debug flag. Initiating restart...", TargetProcessName)

	if exePath == "" {
		// Fallback to standard path
		exePath = `C:\Program Files\NetEase\CloudMusic\cloudmusic.exe`
	}

	// Kill all instances
	log.Info("Killing %d process(es)...", len(pidsToKill))
	exec.Command("taskkill", "/F", "/IM", TargetProcessName).Run()
	time.Sleep(2 * time.Second)

	// Restart with debug flag
	log.Info("Restarting %s with %s", exePath, DebugFlag)
	cmd := exec.Command(exePath, DebugFlag)
	err = cmd.Start()
	if err != nil {
		return false, fmt.Errorf("failed to restart process: %v", err)
	}

	// Wait a few seconds for CEF to initialize
	time.Sleep(3 * time.Second)
	return true, nil
}
