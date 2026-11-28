//go:build windows

package watchdog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"Metabox-Nexus-PlayerCap/player/launch"

	"github.com/shirou/gopsutil/v3/process"
	"golang.org/x/sys/windows"
)

// runningFn 与 inspectProcFn 为包级 seam，供离线测试替换进程枚举与详情读取；生产恒指向真实实现。
var (
	runningFn     = launch.Running
	inspectProcFn = inspectProcess
)

func scanProcesses(ctx context.Context) (instance, error) {
	if err := ctx.Err(); err != nil {
		return instance{}, err
	}
	procs, err := runningFn(TargetProcessName)
	if err != nil {
		return instance{}, err
	}
	if err := ctx.Err(); err != nil {
		return instance{}, err
	}
	switch len(procs) {
	case 0:
		return instance{}, ErrNotRunning
	case 1:
	default:
		return instance{}, ErrAmbiguous
	}
	inst, err := inspectProcFn(ctx, procs[0].PID)
	if err != nil {
		if ctx.Err() != nil {
			return instance{}, ctx.Err()
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, os.ErrPermission) {
			return instance{}, fmt.Errorf("%w: %w", launch.ErrExeUnknown, err)
		}
		return instance{}, err
	}
	return inst, nil
}

func inspectProcess(ctx context.Context, pid uint32) (instance, error) {
	if err := ctx.Err(); err != nil {
		return instance{}, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return instance{}, err
	}
	exe, created, err := identity(h)
	windows.CloseHandle(h)
	if err != nil {
		return instance{}, err
	}
	p, err := process.NewProcessWithContext(ctx, int32(pid))
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
	return instance{PID: int(pid), Exe: exe, Created: created, Args: argv[1:]}, nil
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
		emitKuwoRestartIntervention("detached_cmd_fallback", channelSuffixFromExe(exe), 0)
	}
	return nil
}
