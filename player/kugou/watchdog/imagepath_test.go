package watchdog

// 本文件只测 imagePath：按 PID 取进程映像的完整路径。只查询本进程，不碰酷狗的进程。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImagePathSelf(t *testing.T) {
	want, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got, err := imagePath(uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("imagePath: %v", err)
	}
	if !strings.EqualFold(filepath.Clean(got), filepath.Clean(want)) {
		t.Fatalf("imagePath = %q，应为 %q", got, want)
	}
}

// PID 0 是系统空闲进程，OpenProcess 必然失败：返回错误而不是空串加 nil。
func TestImagePathInvalidPID(t *testing.T) {
	if got, err := imagePath(0); err == nil {
		t.Fatalf("imagePath(0) = %q，应返回错误", got)
	}
}
