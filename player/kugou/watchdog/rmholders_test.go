package watchdog

// 本文件只测 rmHolders：经 Restart Manager 查询打开某个文件的进程。
// 调真的 API，但只针对 t.TempDir() 里本进程自己建的文件，不碰酷狗的任何文件。

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"unsafe"
)

// RM_PROCESS_INFO 在 x64 上为 668 字节：RM_UNIQUE_PROCESS 12、strAppName 256×2、
// strServiceShortName 64×2、其后 4 个 4 字节字段。布局错了，第二个及以后的条目会错位。
func TestRMProcessInfoSize(t *testing.T) {
	if got := unsafe.Sizeof(rmProcessInfo{}); got != 668 {
		t.Fatalf("sizeof(rmProcessInfo) = %d，应为 668", got)
	}
	if got := unsafe.Offsetof(rmProcessInfo{}.ApplicationType); got != 652 {
		t.Fatalf("ApplicationType 偏移 = %d，应为 652", got)
	}
}

func TestRMHoldersSelf(t *testing.T) {
	p := filepath.Join(t.TempDir(), "held.db")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	pids, err := rmHolders(p)
	if err != nil {
		t.Fatalf("rmHolders: %v", err)
	}
	if !slices.Contains(pids, uint32(os.Getpid())) {
		t.Fatalf("占用者 = %v，应含本进程 %d", pids, os.Getpid())
	}
}

func TestRMHoldersNone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "idle.db")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	pids, err := rmHolders(p)
	if err != nil {
		t.Fatalf("rmHolders: %v", err)
	}
	if len(pids) != 0 {
		t.Fatalf("占用者 = %v，应为空", pids)
	}
}
