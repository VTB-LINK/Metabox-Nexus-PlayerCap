package watchdog

// 本文件只测 openShared：持有它打开的句柄期间，别的进程仍能改名、删除该文件（酷狗更新器替换 DLL 的情形）。
// 只操作 t.TempDir() 里的文件。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenSharedAllowsRenameAndDelete(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "libcef.dll")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openShared(p)
	if err != nil {
		t.Fatalf("openShared: %v", err)
	}
	defer f.Close()

	renamed := filepath.Join(dir, "libcef.dll.old")
	if err := os.Rename(p, renamed); err != nil {
		t.Fatalf("持有句柄时改名失败：%v", err)
	}
	if err := os.Remove(renamed); err != nil {
		t.Fatalf("持有句柄时删除失败：%v", err)
	}
	// 句柄仍可读：删除挂起到句柄关闭，或按 POSIX 语义立即移除目录项，读已打开的句柄都不受影响。
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, 0); err != nil || buf[0] != 'x' {
		t.Fatalf("改名删除后读句柄：%q, %v", buf, err)
	}
}

// 对照组：os.Open 的共享模式不含 FILE_SHARE_DELETE，持有其句柄时改名失败。证明上一个用例能区分两种打开方式。
func TestOSOpenBlocksRename(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "libcef.dll")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(p, filepath.Join(dir, "libcef.dll.old")); err == nil {
		t.Fatal("os.Open 的句柄存续时改名竟然成功，本文件的用例区分不了两种打开方式")
	}
}

func TestOpenSharedMissing(t *testing.T) {
	if f, err := openShared(filepath.Join(t.TempDir(), "missing.dll")); err == nil {
		f.Close()
		t.Fatal("文件不存在应返回错误")
	}
}
