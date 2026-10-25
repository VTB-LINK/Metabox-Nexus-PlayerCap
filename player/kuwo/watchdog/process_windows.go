//go:build windows

package watchdog

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/player/launch"

	"github.com/shirou/gopsutil/v3/process"
	"golang.org/x/sys/windows"
)

func scanProcesses(ctx context.Context) (instance, error) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return instance{}, err
	}
	var found instance
	for _, p := range procs {
		if err := ctx.Err(); err != nil {
			return instance{}, err
		}
		name, err := p.NameWithContext(ctx)
		if err != nil || !strings.EqualFold(name, TargetProcessName) {
			continue
		}
		if found.PID != 0 {
			return instance{}, ErrAmbiguous
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(p.Pid))
		if err != nil {
			return instance{}, err
		}
		exe, created, err := identity(h)
		windows.CloseHandle(h)
		if err != nil {
			return instance{}, err
		}
		cmd, err := p.CmdlineWithContext(ctx)
		if err != nil {
			return instance{}, err
		}
		argv, err := windows.DecomposeCommandLine(cmd)
		if err != nil || len(argv) == 0 || !strings.EqualFold(filepath.Clean(argv[0]), filepath.Clean(exe)) {
			return instance{}, ErrAmbiguous
		}
		found = instance{PID: int(p.Pid), Exe: exe, Created: created, Args: argv[1:]}
	}
	if found.PID == 0 {
		return instance{}, ErrNotRunning
	}
	return found, nil
}

func identity(h windows.Handle) (string, uint64, error) {
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", 0, err
	}
	var created, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil {
		return "", 0, err
	}
	return windows.UTF16ToString(buf[:size]), uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), nil
}

func stopExact(ctx context.Context, i instance) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(i.PID))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	exe, created, err := identity(h)
	if err != nil {
		return err
	}
	if created != i.Created || !strings.EqualFold(filepath.Clean(exe), filepath.Clean(i.Exe)) {
		return ErrAmbiguous
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err = windows.TerminateProcess(h, 0); err != nil {
		return err
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, err := windows.WaitForSingleObject(h, 100)
		if err != nil {
			return err
		}
		if state == windows.WAIT_OBJECT_0 {
			return nil
		}
	}
	return ErrAmbiguous
}

var detachFn = launch.Detached

func startExact(exe string, args []string) error {
	// Keep the restarted player out of PlayerCap's process tree (#127).
	detached, err := detachFn(exe, args, filepath.Dir(exe))
	if err != nil {
		return err
	}
	if !detached {
		log.Warn("%s was started directly (its path, arguments or working directory cannot be passed through cmd.exe unchanged); it stays in PlayerCap's process tree, so tools that end PlayerCap together with its child processes (such as OBS autostart plugins) will close it as well", TargetProcessName)
	}
	return nil
}
