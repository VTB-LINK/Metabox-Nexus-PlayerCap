package main

// 本文件只测 acquireUpdateLock：同一安装目录的自动更新以进程间文件锁串行化（#214，设计 §4.7）。
//
// 各用例以自己的临时目录充当安装目录。LockFileEx 的锁属于句柄：同一进程里两次获取用的是两个句柄，与两个进程之间
// 同样互斥。
//
// 「仍在等」以 300 ms 内没有取到判定，「应当取到」以 5 s 为上限：取锁本身是本地调用，毫秒级完成，两边都留了
// 两个数量级以上的余量，CI（Windows，2 核）上不会因调度抖动误判。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	lockStillWaiting = 300 * time.Millisecond
	lockAcquireLimit = 5 * time.Second
)

type lockResult struct {
	release func()
	err     error
}

// acquireAsync 在另一个 goroutine 里获取 dir 的更新锁，返回时发出结果。
func acquireAsync(dir string) <-chan lockResult {
	ch := make(chan lockResult, 1)
	go func() {
		release, err := acquireUpdateLock(dir)
		ch <- lockResult{release, err}
	}()
	return ch
}

// mustAcquire 同步获取 dir 的更新锁，失败即中止用例。
func mustAcquire(t *testing.T, dir string) func() {
	t.Helper()
	release, err := acquireUpdateLock(dir)
	if err != nil || release == nil {
		t.Fatalf("获取更新锁失败：release 为 nil：%v，错误 %v", release == nil, err)
	}
	return release
}

// assertWaiting 断言 ch 在 lockStillWaiting 内没有返回，是则返回 true。返回了即报错、释放取到的锁、返回 false；
// 不中止用例，调用方照常释放自己持有的锁，免得后续用例在这把锁上等到超时。
func assertWaiting(t *testing.T, ch <-chan lockResult) bool {
	t.Helper()
	select {
	case r := <-ch:
		if r.release != nil {
			r.release()
		}
		t.Errorf("锁已被持有，却没有等待就返回了（错误 %v）", r.err)
		return false
	case <-time.After(lockStillWaiting):
		return true
	}
}

// assertAcquired 断言 ch 在 lockAcquireLimit 内取到锁，返回释放函数。
func assertAcquired(t *testing.T, ch <-chan lockResult) func() {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil || r.release == nil {
			t.Fatalf("应取到锁：release 为 nil：%v，错误 %v", r.release == nil, r.err)
		}
		return r.release
	case <-time.After(lockAcquireLimit):
		t.Fatalf("%v 内没有取到锁", lockAcquireLimit)
		return nil
	}
}

// assertFailed 断言 ch 在 lockAcquireLimit 内以「无法取得更新锁」失败。
func assertFailed(t *testing.T, ch <-chan lockResult) {
	t.Helper()
	select {
	case r := <-ch:
		if r.release != nil {
			r.release()
		}
		if !errMatchesKey(r.err, "failed to acquire the update lock: %v") {
			t.Fatalf("应返回「无法取得更新锁」，实际 %v", r.err)
		}
	case <-time.After(lockAcquireLimit):
		t.Fatalf("%v 内没有返回：取锁失败时不应等待", lockAcquireLimit)
	}
}

func lockExists(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, updateLockName))
	return err == nil
}

// waitThenAcquire 断言 waiter 在 holder 释放之前一直等待、释放之后取到锁，返回 waiter 的释放函数；waiter 没有等待
// 时返回 nil。
func waitThenAcquire(t *testing.T, holder func(), waiter <-chan lockResult) func() {
	t.Helper()
	waited := assertWaiting(t, waiter)
	holder()
	if !waited {
		return nil
	}
	return assertAcquired(t, waiter)
}

// 有等待者时：持锁方释放后锁文件删不掉（等待者正打开着它）、仍在；等待者取得锁，它释放后锁文件被删除。
//
// 变异自证（实测）：去掉锁（直接返回空操作与 nil）→ 第二次获取不等待，红；改为共享锁（去掉
// LOCKFILE_EXCLUSIVE_LOCK）→ 同上，红；被占用时不阻塞等待（只试一次，失败即返回错误）→ 第二次获取立即返回错误，红；
// 释放时不删锁文件 → 等待者释放后锁文件仍在，红；以 O_EXCL 新建锁文件 → 第二次获取立即返回错误，红。
func TestUpdateLockSameDirWaits(t *testing.T) {
	dir := t.TempDir()
	second := waitThenAcquire(t, mustAcquire(t, dir), acquireAsync(dir))
	if second == nil {
		return
	}
	if !lockExists(dir) {
		t.Error("等待者持锁期间锁文件不在")
	}
	second()
	if lockExists(dir) {
		t.Error("最后一个持锁者释放后锁文件仍在")
	}
}

// 无人等待时释放即删除锁文件，安装目录里不留东西。
//
// 变异自证（实测）：释放时不删锁文件 → 红。
func TestUpdateLockRemovedOnRelease(t *testing.T) {
	dir := t.TempDir()
	release := mustAcquire(t, dir)
	if !lockExists(dir) {
		t.Fatal("持锁期间锁文件不在")
	}
	release()
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("释放后安装目录应为空，实际 %v", names)
	}
}

// 上一次崩溃留下的锁文件（系统已随进程退出释放了锁）不妨碍取锁，释放后照常删除。
//
// 变异自证（实测）：以 O_EXCL 新建锁文件（已存在即失败）→ 返回错误，红。
func TestUpdateLockLeftoverFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, updateLockName), "")
	mustAcquire(t, dir)()
	if lockExists(dir) {
		t.Fatal("释放后锁文件仍在")
	}
}

// 同一目录的不同写法是同一把锁：锁文件在目录里，文件系统不分大小写。
func TestUpdateLockSameDirSpellings(t *testing.T) {
	dir := t.TempDir()
	for name, other := range map[string]string{
		"大写":      strings.ToUpper(dir),
		"末尾带分隔符":  dir + string(filepath.Separator),
		"含 . 的路径": filepath.Join(dir, ".") + string(filepath.Separator) + ".",
	} {
		t.Run(name, func(t *testing.T) {
			if second := waitThenAcquire(t, mustAcquire(t, dir), acquireAsync(other)); second != nil {
				second()
			}
		})
	}
}

// 变异自证（实测）：锁文件改放在各目录共用的位置（以上一级目录代替安装目录）→ 第二个目录等到超时，红。
func TestUpdateLockDifferentDirsIndependent(t *testing.T) {
	first := mustAcquire(t, t.TempDir())
	defer first()
	assertAcquired(t, acquireAsync(t.TempDir()))()
}

// lockHeld 报告 dir 的锁文件此刻是否被别的句柄锁着：另开一个句柄，不等待地试锁一次。
func lockHeld(t *testing.T, dir string) bool {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, updateLockName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := windows.Handle(f.Fd())
	err = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if err == nil {
		windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped))
		return false
	}
	if err != windows.ERROR_LOCK_VIOLATION {
		t.Fatal(err)
	}
	return true
}

// 释放函数可重复调用：再调一次不出错，也不会解掉此后别人取得的锁。
//
// 变异自证（实测）：释放不用 sync.Once（每次都按旧句柄值解锁、关闭）→ 此后取得锁的句柄复用了旧句柄值时，再调一次
// 旧释放函数就解掉了它的锁，红。是否复用由系统分配句柄决定、不是每次都发生，故做 10 轮，每轮重新取一个 first，
// 各轮互不牵连（同一个 first 连用多轮时，一旦它的句柄值被别处占去，此后各轮都不复用：20 次里 3 次漏过）；
// 每轮不等待地探一次锁。变异下连跑 20 次红 20 次。
func TestUpdateLockReleaseTwice(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 10; i++ {
		first := mustAcquire(t, dir)
		first()
		first()
		if lockHeld(t, dir) {
			t.Fatalf("第 %d 轮：释放之后锁仍被持有", i)
		}
		// 同步获取、中间不起 goroutine：新句柄更可能复用 first 关掉的句柄值。
		second := mustAcquire(t, dir)
		first()
		held := lockHeld(t, dir)
		second()
		if !held {
			t.Fatalf("第 %d 轮：已释放过的旧释放函数再调一次，解掉了此后取得的锁", i)
		}
	}
}

// 安装目录不存在：立即返回错误，不等待，也不建出目录。暂存的 .new 本就写在安装目录，锁建不出来时这次更新也写不了文件。
//
// 变异自证（实测）：取锁失败改回放行（返回空操作与 nil 错误）→ 红。
func TestUpdateLockDirMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing", "deeper")
	assertFailed(t, acquireAsync(dir))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("不应建出目录 %s：%v", dir, err)
	}
}

// 锁文件的位置被一个同名目录占着：立即返回错误，不动那个目录。
//
// 变异自证（实测）：取锁失败改回放行（返回空操作与 nil 错误）→ 红。
func TestUpdateLockPathIsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, updateLockName), 0o755); err != nil {
		t.Fatal(err)
	}
	assertFailed(t, acquireAsync(dir))
	if fi, err := os.Stat(filepath.Join(dir, updateLockName)); err != nil || !fi.IsDir() {
		t.Fatalf("%s 应仍是目录：%v", updateLockName, err)
	}
}
