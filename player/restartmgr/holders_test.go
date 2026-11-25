package restartmgr

// 本文件只测 Holders：经 Restart Manager 查询打开给定文件的进程。
// 调真的 API，但只针对 t.TempDir() 里本进程自己建的文件，不碰任何播放器的文件。

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

// writeTemp 在 t.TempDir() 里建一个内容为 "x" 的文件，返回其路径；不保持打开。
func writeTemp(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// 本进程打开的文件与一个无人打开的文件注册进同一个会话：占用者含本进程。
func TestHoldersSelf(t *testing.T) {
	held := writeTemp(t, "held.db")
	idle := writeTemp(t, "idle.db")
	f, err := os.Open(held)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for _, paths := range [][]string{{held}, {idle, held}, {held, idle}} {
		pids, err := Holders(paths...)
		if err != nil {
			t.Fatalf("Holders(%v): %v", paths, err)
		}
		if !slices.Contains(pids, uint32(os.Getpid())) {
			t.Fatalf("Holders(%v) 占用者 = %v，应含本进程 %d", paths, pids, os.Getpid())
		}
	}

	pids, err := HoldersOf(held)
	if err != nil {
		t.Fatalf("HoldersOf: %v", err)
	}
	if !slices.Contains(pids, uint32(os.Getpid())) {
		t.Fatalf("HoldersOf 占用者 = %v，应含本进程 %d", pids, os.Getpid())
	}
}

// 无人打开的文件（一个或多个，含曾被本进程打开、已关闭的）：占用者为空。
func TestHoldersNone(t *testing.T) {
	a := writeTemp(t, "a.db")
	b := writeTemp(t, "b.db")
	f, err := os.Open(b)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	for _, paths := range [][]string{{a}, {a, b}} {
		pids, err := Holders(paths...)
		if err != nil {
			t.Fatalf("Holders(%v): %v", paths, err)
		}
		if len(pids) != 0 {
			t.Fatalf("Holders(%v) 占用者 = %v，应为空", paths, pids)
		}
	}
	pids, err := HoldersOf(a)
	if err != nil || len(pids) != 0 {
		t.Fatalf("HoldersOf = (%v, %v)，应为空、无错误", pids, err)
	}
}

// 不给路径：返回 (nil, nil)，不开会话。
func TestHoldersEmpty(t *testing.T) {
	for name, paths := range map[string][]string{"无参数": nil, "空切片": {}} {
		pids, err := Holders(paths...)
		if pids != nil || err != nil {
			t.Fatalf("%s：Holders = (%v, %v)，应为 (nil, nil)", name, pids, err)
		}
	}
}

// 路径含 NUL 转不成 UTF-16：返回错误，不返回占用者。
func TestHoldersInvalidPath(t *testing.T) {
	ok := writeTemp(t, "ok.db")
	for _, paths := range [][]string{{"bad\x00.db"}, {ok, "bad\x00.db"}} {
		pids, err := Holders(paths...)
		if err == nil || pids != nil {
			t.Fatalf("Holders(%q) = (%v, %v)，应返回错误", paths, pids, err)
		}
	}
	if pids, err := HoldersOf("bad\x00.db"); err == nil || pids != nil {
		t.Fatalf("HoldersOf = (%v, %v)，应返回错误", pids, err)
	}
}
