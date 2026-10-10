package config

// 本文件只坐实一件事：标准库里几个文件操作在目标已存在时的现行行为。
//
// 它不测本包的逻辑。doc/design/config-version-and-offset-delay-design.md 里替换 config.yml 的步骤以这些结果
// 为前提：os.Rename 会覆盖已存在的目标，所以备份文件名须自己保证不重名，放置新文件也不能用它；os.Link
// 在目标已存在时失败且不动它，「不存在才放置」首选靠它。硬链接不可用时的退路不在标准库里，见
// replacefile_windows_test.go。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写入 %s 失败: %v", path, err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(b)
}

// os.Rename 覆盖已存在的目标，不报错。
func TestRenameReplacesExistingTarget(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	writeTestFile(t, src, "new")
	writeTestFile(t, dst, "old")
	if err := os.Rename(src, dst); err != nil {
		t.Fatalf("改名到已存在的目标应成功，实际 %v", err)
	}
	if got := readTestFile(t, dst); got != "new" {
		t.Errorf("目标内容 = %q，期望被覆盖为 %q", got, "new")
	}
	if _, err := os.Lstat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("改名之后源文件应不存在，Lstat 返回 %v", err)
	}
}

// os.Link 在目标已存在时失败，目标不变；目标不存在时两个名字指向同一份内容。
func TestLinkFailsOnExistingTarget(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	writeTestFile(t, src, "new")

	if err := os.Link(src, dst); err != nil {
		t.Skipf("临时目录所在的文件系统不支持硬链接: %v", err)
	}
	if got := readTestFile(t, dst); got != "new" {
		t.Errorf("链接出的文件内容 = %q，期望 %q", got, "new")
	}

	other := filepath.Join(dir, "other")
	writeTestFile(t, other, "other")
	err := os.Link(other, dst)
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("链接到已存在的目标应返回「已存在」，实际 %v", err)
	}
	if got := readTestFile(t, dst); got != "new" {
		t.Errorf("链接失败后目标内容 = %q，期望仍为 %q", got, "new")
	}

	// 删掉源名字之后，目标名字下的内容仍在。
	if err := os.Remove(src); err != nil {
		t.Fatalf("删除源名字失败: %v", err)
	}
	if got := readTestFile(t, dst); got != "new" {
		t.Errorf("删除源名字后目标内容 = %q，期望 %q", got, "new")
	}
}

// os.CreateTemp 每次调用都新建一个名字不同的文件，模式里 * 前后的部分原样保留：
// 用它预留的名字只属于这一次调用，别的调用（别的进程同理）拿不到同一个。
func TestCreateTempReservesDistinctNames(t *testing.T) {
	dir := t.TempDir()
	const pattern = "config.backup-v3-20261009-153045-*.yml"

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			t.Fatalf("第 %d 次 CreateTemp 失败: %v", i, err)
		}
		name := filepath.Base(f.Name())
		f.Close()
		if seen[name] {
			t.Fatalf("CreateTemp 返回了重复的名字 %s", name)
		}
		seen[name] = true
		if ok, _ := filepath.Match(pattern, name); !ok {
			t.Errorf("名字 %s 不符合模式 %s", name, pattern)
		}
		if info, err := os.Lstat(filepath.Join(dir, name)); err != nil || info.Size() != 0 {
			t.Errorf("预留出的 %s 应是已存在的空文件，Lstat = %v, %v", name, info, err)
		}
	}
}

// 把文件改名到自己预留的空文件上：成功，预留名下是被移走的内容。源不存在时改名失败，
// 预留的空文件留在原地，须由调用方自己删掉。
func TestRenameOntoReservedName(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "config.yml")
	writeTestFile(t, src, "user settings")

	f, err := os.CreateTemp(dir, "config.backup-v3-*.yml")
	if err != nil {
		t.Fatalf("CreateTemp 失败: %v", err)
	}
	reserved := f.Name()
	f.Close()

	if err := os.Rename(src, reserved); err != nil {
		t.Fatalf("改名到预留的名字应成功，实际 %v", err)
	}
	if got := readTestFile(t, reserved); got != "user settings" {
		t.Errorf("预留名下的内容 = %q，期望是被移走的内容", got)
	}
	if _, err := os.Lstat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("改名之后原名应不存在，Lstat 返回 %v", err)
	}

	f, err = os.CreateTemp(dir, "config.backup-v3-*.yml")
	if err != nil {
		t.Fatalf("CreateTemp 失败: %v", err)
	}
	second := f.Name()
	f.Close()
	if err := os.Rename(src, second); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("源不存在时改名应返回「不存在」，实际 %v", err)
	}
	if info, err := os.Lstat(second); err != nil || info.Size() != 0 {
		t.Errorf("改名失败后预留的空文件应仍在，Lstat = %v, %v", info, err)
	}
}
