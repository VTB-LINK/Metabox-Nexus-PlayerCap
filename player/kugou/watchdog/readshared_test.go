package watchdog

// 本文件只测 readShared：以全共享方式读入 KuGou.ini，超过 maxINIBytes 即报错。只读 t.TempDir() 里的文件。

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReadSharedLimit(t *testing.T) {
	dir := t.TempDir()

	exact := bytes.Repeat([]byte{'a'}, maxINIBytes)
	p := filepath.Join(dir, "exact.ini")
	if err := os.WriteFile(p, exact, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readShared(p); err != nil || !bytes.Equal(got, exact) {
		t.Fatalf("恰为上限：len=%d err=%v", len(got), err)
	}

	over := filepath.Join(dir, "over.ini")
	if err := os.WriteFile(over, append(exact, 'b'), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readShared(over); err == nil {
		t.Fatalf("超过上限 1 字节：len=%d，应返回错误", len(got))
	}

	if _, err := readShared(filepath.Join(dir, "missing.ini")); err == nil {
		t.Fatal("文件不存在应返回错误")
	}
}
