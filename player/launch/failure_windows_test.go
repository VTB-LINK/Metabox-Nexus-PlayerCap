package launch

// 本文件只测 Detached 的失败路径：程序不存在、不是普通文件时不调用 cmd.exe 而直接报错；cmd.exe 卡住时到时结束它
// 并报错；cmd.exe 以非 0 退出时报错。cmd.exe 由测试二进制顶替（替换包级变量 shell），按角色留下记号、长睡或以 3
// 退出，不调用真的 cmd.exe，不会弹出任何框。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeShell 用测试二进制顶替 cmd.exe，扮演 role；返回它的报告写到哪里。
func fakeShell(t *testing.T, role string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	old := shell
	shell = self
	t.Cleanup(func() { shell = old })
	rep := filepath.Join(t.TempDir(), "shell.json")
	t.Setenv(envRole, role)
	t.Setenv(envReport, rep)
	return rep
}

// TestDetachedShellMarkerWorks 是下面两条「没有调用 cmd.exe」断言的前提：程序存在时顶替的 cmd.exe 确实被调用、
// 留下了记号。没有这一条，记号机制失效时下面两条恒绿。
func TestDetachedShellMarkerWorks(t *testing.T) {
	exe := fakePlayer(t, filepath.Join(t.TempDir(), "player.exe"))
	rep := fakeShell(t, "marker")
	if detached, err := Detached(exe, nil, ""); err != nil || !detached {
		t.Fatalf("Detached = (%v, %v)，期望 (true, nil)", detached, err)
	}
	if _, err := os.Stat(rep); err != nil {
		t.Fatalf("顶替的 cmd.exe 没有留下记号: %v", err)
	}
}

// TestDetachedMissingExe：程序不存在时返回 fs.ErrNotExist，不调用 cmd.exe。
//
// 防的缺陷与变异（实测变红）：去掉 os.Stat 的检查。真的 cmd.exe 拿到不存在的程序时，start 弹出错误框并阻塞
// （cmd 带 HideWindow，框不显示），开发机实测 6 秒后仍未退出；Detached 要等满 waitLimit 才报超时。
func TestDetachedMissingExe(t *testing.T) {
	rep := fakeShell(t, "marker")
	detached, err := Detached(filepath.Join(t.TempDir(), "nope", "player.exe"), nil, "")
	if detached || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Detached = (%v, %v)，期望 (false, fs.ErrNotExist)", detached, err)
	}
	if _, err := os.Stat(rep); err == nil {
		t.Error("程序不存在，却调用了 cmd.exe")
	}
}

// TestDetachedDirectoryExe：exePath 是目录（哪怕名字以 .exe 结尾）时报错，不调用 cmd.exe。
//
// 防的缺陷与变异（实测变红）：去掉「是普通文件」的检查。真的 start 拿到目录时按关联打开（用资源管理器打开
// 该目录；推断，未实测，以免在桌面上弹窗）。
func TestDetachedDirectoryExe(t *testing.T) {
	rep := fakeShell(t, "marker")
	dir := filepath.Join(t.TempDir(), "player.exe")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if detached, err := Detached(dir, nil, ""); detached || err == nil {
		t.Errorf("Detached = (%v, %v)，期望报错", detached, err)
	}
	if _, err := os.Stat(rep); err == nil {
		t.Error("exePath 是目录，却调用了 cmd.exe")
	}
}

// TestDetachedShellTimeout：cmd.exe 超过 waitLimit 不退出时结束它并报错，Detached 不随它一直等。
//
// 防的缺陷与变异（实测变红）：cmd.exe 改用不带 ctx 的 exec.Command（Detached 一直等到顶替者 20 秒后自行退出，
// 用例在 10 秒时判失败）；超时后不报错。
func TestDetachedShellTimeout(t *testing.T) {
	exe := fakePlayer(t, filepath.Join(t.TempDir(), "player.exe"))
	rep := fakeShell(t, "hang")
	old := waitLimit
	waitLimit = 2 * time.Second // 留够顶替者启动、写下报告的时间（全量测试并行时进程启动会变慢）
	t.Cleanup(func() { waitLimit = old })

	type result struct {
		detached bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		d, err := Detached(exe, nil, "")
		done <- result{d, err}
	}()
	select {
	case r := <-done:
		if r.detached || r.err == nil {
			t.Errorf("Detached = (%v, %v)，期望超时报错", r.detached, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cmd.exe 卡住时 Detached 10 秒内没有返回")
	}
	// 顶替者自己要睡 20 秒，3 秒内退出即说明是被结束的。
	self, _ := os.Executable()
	awaitExit(t, waitReport(t, rep, 5*time.Second).PID, self, 3*time.Second)
}

// TestDetachedShellExitCode：cmd.exe 以非 0 退出（例如 start 找不到程序）时报错。
//
// 防的缺陷与变异（实测变红）：忽略 cmd.Run 的错误、一律返回 (true, nil)。
func TestDetachedShellExitCode(t *testing.T) {
	exe := fakePlayer(t, filepath.Join(t.TempDir(), "player.exe"))
	fakeShell(t, "exit3")
	if detached, err := Detached(exe, nil, ""); detached || err == nil {
		t.Errorf("Detached = (%v, %v)，期望报错", detached, err)
	}
}
