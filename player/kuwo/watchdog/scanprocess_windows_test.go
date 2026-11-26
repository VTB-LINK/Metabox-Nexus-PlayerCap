//go:build windows

package watchdog

// 本文件经 runningFn / inspectProcFn 桩离线测 scanProcesses 与 EnsureDebugMode 的进程枚举和权限错误映射
// （#231 代码审 F2）：
//   - 无进程返回 ErrNotRunning；
//   - 多进程返回 ErrAmbiguous；
//   - 唯一进程读取遇到 windows.ERROR_ACCESS_DENIED 或 os.ErrPermission 时包装为 launch.ErrExeUnknown，
//     使上层按 standby / no_admin 报告权限不足；
//   - ctx.Canceled 优先原样返回，不被吞成 launch.ErrExeUnknown；
//   - 非权限错误原样透传，不误映射为 launch.ErrExeUnknown。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"Metabox-Nexus-PlayerCap/player/launch"

	"golang.org/x/sys/windows"
)

// TestScanProcessesPermissionMapping
//
// 变异自证（实测变红）：scanProcesses 直接返回原始 windows.ERROR_ACCESS_DENIED 而不包装 launch.ErrExeUnknown
// → 「权限拒绝（ERROR_ACCESS_DENIED）」「包装后的权限拒绝」「权限拒绝（os.ErrPermission）」三例均红。
func TestScanProcessesPermissionMapping(t *testing.T) {
	origRun, origInspect := runningFn, inspectProcFn
	defer func() { runningFn, inspectProcFn = origRun, origInspect }()

	t.Run("无进程返回ErrNotRunning", func(t *testing.T) {
		runningFn = func(name string) ([]launch.Proc, error) {
			if name != TargetProcessName {
				t.Errorf("按 %q 枚举，期望 %q", name, TargetProcessName)
			}
			return nil, nil
		}
		inspectProcFn = func(context.Context, uint32) (instance, error) {
			t.Fatal("无进程时不应调 inspectProcFn")
			return instance{}, nil
		}
		if err := EnsureDebugMode(context.Background()); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("got %v, want ErrNotRunning", err)
		}
	})

	t.Run("多进程返回ErrAmbiguous", func(t *testing.T) {
		runningFn = func(string) ([]launch.Proc, error) {
			return []launch.Proc{{PID: 100}, {PID: 200}}, nil
		}
		inspectProcFn = func(context.Context, uint32) (instance, error) {
			t.Fatal("多进程时不应调 inspectProcFn")
			return instance{}, nil
		}
		if err := EnsureDebugMode(context.Background()); !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("got %v, want ErrAmbiguous", err)
		}
	})

	t.Run("权限拒绝（ERROR_ACCESS_DENIED）经EnsureDebugMode映射为ErrExeUnknown", func(t *testing.T) {
		runningFn = func(string) ([]launch.Proc, error) {
			return []launch.Proc{{PID: 4242}}, nil
		}
		inspectProcFn = func(_ context.Context, pid uint32) (instance, error) {
			if pid != 4242 {
				t.Errorf("pid=%d want 4242", pid)
			}
			return instance{}, windows.ERROR_ACCESS_DENIED
		}
		err := EnsureDebugMode(context.Background())
		if !errors.Is(err, launch.ErrExeUnknown) || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Fatalf("got %v, want wrapped launch.ErrExeUnknown + ERROR_ACCESS_DENIED", err)
		}
	})

	t.Run("包装后的权限拒绝映射为ErrExeUnknown", func(t *testing.T) {
		runningFn = func(string) ([]launch.Proc, error) {
			return []launch.Proc{{PID: 4242}}, nil
		}
		inspectProcFn = func(context.Context, uint32) (instance, error) {
			return instance{}, fmt.Errorf("cmdline: %w", windows.ERROR_ACCESS_DENIED)
		}
		err := EnsureDebugMode(context.Background())
		if !errors.Is(err, launch.ErrExeUnknown) {
			t.Fatalf("got %v, want launch.ErrExeUnknown", err)
		}
	})

	t.Run("权限拒绝（os.ErrPermission）映射为ErrExeUnknown", func(t *testing.T) {
		runningFn = func(string) ([]launch.Proc, error) {
			return []launch.Proc{{PID: 4242}}, nil
		}
		inspectProcFn = func(context.Context, uint32) (instance, error) {
			return instance{}, os.ErrPermission
		}
		err := EnsureDebugMode(context.Background())
		if !errors.Is(err, launch.ErrExeUnknown) {
			t.Fatalf("got %v, want launch.ErrExeUnknown", err)
		}
	})

	t.Run("ctx取消原样返回不被吞成ErrExeUnknown", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		runningFn = func(string) ([]launch.Proc, error) {
			return []launch.Proc{{PID: 4242}}, nil
		}
		inspectProcFn = func(context.Context, uint32) (instance, error) {
			cancel()
			return instance{}, windows.ERROR_ACCESS_DENIED
		}
		err := EnsureDebugMode(ctx)
		if !errors.Is(err, context.Canceled) || errors.Is(err, launch.ErrExeUnknown) {
			t.Fatalf("got %v, want context.Canceled (not ErrExeUnknown)", err)
		}
	})

	t.Run("非权限错误原样透传不误报ErrExeUnknown", func(t *testing.T) {
		want := errors.New("other inspect failure")
		runningFn = func(string) ([]launch.Proc, error) {
			return []launch.Proc{{PID: 4242}}, nil
		}
		inspectProcFn = func(context.Context, uint32) (instance, error) {
			return instance{}, want
		}
		err := EnsureDebugMode(context.Background())
		if !errors.Is(err, want) || errors.Is(err, launch.ErrExeUnknown) {
			t.Fatalf("got %v, want %v without ErrExeUnknown", err, want)
		}
	})

	t.Run("唯一进程读取成功返回实例", func(t *testing.T) {
		want := instance{PID: 4242, Exe: `C:\Kuwo\9.6.0.0_W1\bin\kwmusic.exe`, Created: 123, Args: []string{DebugFlag}}
		runningFn = func(string) ([]launch.Proc, error) {
			return []launch.Proc{{PID: 4242, ExePath: want.Exe}}, nil
		}
		inspectProcFn = func(context.Context, uint32) (instance, error) {
			return want, nil
		}
		got, err := scanProcesses(context.Background())
		if err != nil || got.PID != want.PID || got.Exe != want.Exe || got.Created != want.Created {
			t.Fatalf("got (%+v, %v), want %+v", got, err, want)
		}
	})
}
