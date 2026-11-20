package main

// 同一安装目录的自动更新以进程间文件锁串行化（#214，设计 §4.7）。

import (
	"os"
	"path/filepath"
	"sync"

	"Metabox-Nexus-PlayerCap/i18n"

	"golang.org/x/sys/windows"
)

// updateLockName 是更新锁文件的名字，放在安装目录本身。
//
// 放在安装目录而不放系统临时目录：所有进程对同一安装目录用同一个锁位置，不会出现一个进程锁在临时目录、另一个
// 回退到别处而各锁各的。暂存的 .new 本就写在安装目录，能建暂存文件就能建锁文件；锁建不出来时这次更新本来也写不了
// 文件，所以取锁失败即不更新，不会挡掉本可成功的更新。文件系统不分大小写，同一目录的不同写法自然是同一个文件。
const updateLockName = "Metabox-Nexus-PlayerCap.update.lock"

// acquireUpdateLock 取得安装目录 dir 的更新锁，返回释放函数；别的进程持有时等它释放。打开或加锁失败返回错误，
// 调用方不得更新。
//
// 启动时的更新在监听端口之前，没有防多开。两个进程同时更新同一目录时共用固定的 <名字>.new：一个进程已核对过的
// .new 可在它下载其余附件期间被另一个截断重写，随后残缺的文件被换成主程序。串行之后固定名是安全的，崩溃留下的
// 残件下一次覆盖，不会越积越多。
//
// 以 LockFileEx 独占锁文件的第一个字节。锁随句柄、不绑线程，进程退出（含崩溃）时由系统释放。不用命名互斥体：
// 它归获取它的线程所有，goroutine 会换线程，ReleaseMutex 可能以 ERROR_NOT_OWNER 失败；老 conhost 的任务栏
// 后端另在 consoleInit 里 LockOSThread，线程归属更难说清。先不等待试一次，被占用时打一行提示再阻塞等待。
//
// 释放函数解锁、关闭句柄，再尽力删除锁文件，使安装目录不留锁文件；可重复调用。删除是安全的：os.OpenFile 经
// syscall.Open 打开，共享模式只有 FILE_SHARE_READ | FILE_SHARE_WRITE、不带 FILE_SHARE_DELETE（读 Go 1.26.1 源码
// 得出），等锁的进程持有打开的句柄时删除失败（忽略）；无人持有时删掉，后来者新建同名文件重新加锁。
// 一个文件只在没有任何句柄时才删得掉，所以任一时刻所有句柄都指向同一个文件，只有一个持锁者。
func acquireUpdateLock(dir string) (release func(), err error) {
	path := filepath.Join(dir, updateLockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, i18n.Errorf("failed to acquire the update lock: %v", err)
	}
	h := windows.Handle(f.Fd())
	lock := func(flags uint32) error {
		return windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|flags, 0, 1, 0, new(windows.Overlapped))
	}
	err = lock(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err == windows.ERROR_LOCK_VIOLATION {
		mainLog.Info("Another instance is updating this program, waiting for it to finish...")
		err = lock(0)
	}
	if err != nil {
		f.Close()
		return nil, i18n.Errorf("failed to acquire the update lock: %v", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped))
			f.Close()
			os.Remove(path)
		})
	}, nil
}
