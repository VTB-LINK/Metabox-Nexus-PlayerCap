package main

// 本文件只测一件事：自动更新时旧 exe 的备份名怎么选、留下的备份怎么清（updateBackupPath 与
// removeUpdateBackups，issue #71）。
//
// 背景：更新后旧进程在 restartSelf 里等新进程结束，它的映像即备份文件，一直被占用。新进程若在
// 同一次启动里又要更新，备份名不能再用被占用的那个。
//
// 「被占用」有两种做法。表驱动用例保持文件打开：os.Open 的句柄不带 FILE_SHARE_DELETE（syscall.Open
// 的 sharemode），其间 os.Remove 失败，holdOpen 每次都先断言这一前提。
// TestUpdateBackupWithRunningImages 用真正在运行的映像：子进程是本测试二进制的副本，角色 leaf
// （见 relaunchwait_test.go 的 relaunchHelperMain），读标准输入而阻塞，不运行服务本体。
//
// 没有离线用例的：performUpdateAll 里的调用点（改名、失败回滚）与 cleanupOldExe 对
// removeUpdateBackups 的调用，二者依赖下载与 os.Executable。

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// holdOpen 保持 path 打开到用例结束，并断言其间 os.Remove 失败、文件仍在。
func holdOpen(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := os.Remove(path); err == nil || !exists(path) {
		t.Fatalf("前提不成立：%s 保持打开时仍被删除", path)
	}
}

// copyTestBinary 把本测试二进制复制为 dst。
func copyTestBinary(t *testing.T, dst string) {
	t.Helper()
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, src); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// startBlockedLeaf 以 leaf 角色运行 exe，它读标准输入而阻塞，exe 在此期间是正在运行的映像。
// 返回的函数关闭它的标准输入并等它退出；用例结束时也会调用。
func startBlockedLeaf(t *testing.T, exe string) (release func()) {
	t.Helper()
	cmd := exec.Command(exe, "echo", "0")
	cmd.Env = append(os.Environ(), relaunchHelperEnv+"=leaf")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 %s: %v", exe, err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			stdin.Close()
			cmd.Wait()
		})
	}
	t.Cleanup(release)
	return release
}

// TestUpdateBackupPath 断言备份名的选取：.old 不在或删得掉时用它，删不掉且仍在时改用带本进程号的
// 名字。每例最后照 performUpdateAll 的做法把 exe 改名过去，返回的名字必须让这一步成功；带进程号的
// 残留已存在时靠 os.Rename 的替换语义盖掉。
//
// 变异（均实测为红）：恒返回 .old（改动前的写法，三条被占用的用例改名失败）；恒返回带进程号的名字；
// 不先删 .old、只看它在不在（「.old 存在且可删」红）；按 os.Remove 是否出错判断、不看文件在不在
// （「没有 .old」「只有带进程号的残留」红）。
func TestUpdateBackupPath(t *testing.T) {
	pidSuffix := ".old." + strconv.Itoa(os.Getpid())
	cases := []struct {
		name       string
		setup      func(t *testing.T, exe string)
		wantSuffix string
		oldRemains bool
	}{
		{"没有 .old", func(*testing.T, string) {}, ".old", false},
		{".old 存在且可删", func(t *testing.T, exe string) {
			writeFile(t, exe+".old", "previous")
		}, ".old", false},
		{"只有带进程号的残留", func(t *testing.T, exe string) {
			writeFile(t, exe+pidSuffix, "leftover")
		}, ".old", false},
		{".old 被占用", func(t *testing.T, exe string) {
			writeFile(t, exe+".old", "previous")
			holdOpen(t, exe+".old")
		}, pidSuffix, true},
		{".old 被占用且带进程号的残留已存在", func(t *testing.T, exe string) {
			writeFile(t, exe+".old", "previous")
			holdOpen(t, exe+".old")
			writeFile(t, exe+pidSuffix, "leftover")
		}, pidSuffix, true},
		{".old 是非空目录", func(t *testing.T, exe string) {
			if err := os.Mkdir(exe+".old", 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(exe+".old", "inside"), "x")
		}, pidSuffix, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "App.exe")
			writeFile(t, exe, "current")
			c.setup(t, exe)

			got := updateBackupPath(exe)

			if want := exe + c.wantSuffix; got != want {
				t.Errorf("备份名 %s，期望 %s", filepath.Base(got), filepath.Base(want))
			}
			if exists(exe+".old") != c.oldRemains {
				t.Errorf("调用后 .old 是否仍在：%v，期望 %v", !c.oldRemains, c.oldRemains)
			}
			if err := os.Rename(exe, got); err != nil {
				t.Fatalf("把 exe 改名为备份名失败: %v", err)
			}
			if b, _ := os.ReadFile(got); string(b) != "current" {
				t.Errorf("备份文件的内容是 %q，不是改名过去的 exe", b)
			}
		})
	}
}

// TestUpdateBackupWithRunningImages 用正在运行的映像走一遍「更新后紧接着再更新」：.old 是上一次
// 更新后仍在等待的进程的映像，exe 是当前进程的映像，带进程号的名字上另有一个残留文件。
//
// 依次断言：正在运行的映像删不掉（平台前提）；备份名取带进程号的那个；把运行中的 exe 改名到已存在
// 的残留上成功（os.Rename 在 Windows 上是 MoveFileEx 加 MOVEFILE_REPLACE_EXISTING）；两个进程还在时
// removeUpdateBackups 一个也删不掉、不出错；两个进程结束后它把两个备份都删掉。
//
// 变异（均实测为红）：updateBackupPath 恒返回 .old（改动前的写法）——改名的目标是正在运行的映像，
// 失败；removeUpdateBackups 只删 .old 或不删 .old——进程结束后清不干净。
func TestUpdateBackupWithRunningImages(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "App.exe")
	old := exe + ".old"
	leftover := old + "." + strconv.Itoa(os.Getpid())
	copyTestBinary(t, exe)
	copyTestBinary(t, old)
	writeFile(t, leftover, "leftover")
	releaseOld := startBlockedLeaf(t, old)
	releaseCur := startBlockedLeaf(t, exe)

	if err := os.Remove(old); err == nil || !exists(old) {
		t.Fatal("前提不成立：正在运行的映像被删除")
	}

	got := updateBackupPath(exe)
	if got != leftover {
		t.Errorf("备份名 %s，期望 %s", filepath.Base(got), filepath.Base(leftover))
	}
	if err := os.Rename(exe, got); err != nil {
		t.Fatalf("把运行中的 exe 改名为 %s 失败: %v", filepath.Base(got), err)
	}
	if fi, err := os.Stat(got); err != nil || fi.Size() == int64(len("leftover")) {
		t.Fatalf("改名后 %s 仍是残留文件（%v）", filepath.Base(got), err)
	}

	removeUpdateBackups(exe)
	if !exists(old) || !exists(got) {
		t.Fatalf("进程仍在运行时备份被删除：.old 在 %v，带进程号的在 %v", exists(old), exists(got))
	}

	releaseOld()
	releaseCur()
	// 进程句柄报告退出后，映像文件的占用可能稍晚才解除，重试到删掉为止。
	deadline := time.Now().Add(5 * time.Second)
	for {
		removeUpdateBackups(exe)
		if !exists(old) && !exists(got) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("进程结束后备份仍未清掉：.old 在 %v，带进程号的在 %v", exists(old), exists(got))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRemoveUpdateBackups 断言清理的范围：exe 名加 .old，以及 exe 名加「.old.」加非空的纯 ASCII
// 十进制数字（updateBackupPath 产生的形状）且不是目录的条目；exe 名不分大小写。删不掉的留着、
// 继续删其余的。名字相近的一律不动。
//
// 变异（均实测为红）：只删 .old（改动前的写法）；不删 .old；「.old.」之后不查是不是纯数字；
// 数字判定放宽到全角数字；前缀比较区分大小写；前缀不要求从名字开头匹配；不跳过目录；
// 遇到删不掉的就返回；.old 删不掉就返回。
func TestRemoveUpdateBackups(t *testing.T) {
	doomed := []string{"App.exe.old", "App.exe.old.1", "App.exe.old.4321", "App.exe.old.9", "APP.EXE.OLD.77"}
	kept := []string{
		"App.exe",     // 本体
		"App.exe.new", // 下载中的临时文件
		// .old 之后不是点
		"App.exe.older", "App.exe.old2", "App.exe.oldx.1",
		// 点之后不是非空的纯 ASCII 数字
		"App.exe.old.txt", "App.exe.old.12a", "App.exe.old.1.2", "App.exe.old.-1", "App.exe.old.１２",
		// 开头不是 exe 名
		"xApp.exe.old.1", "Other.exe.old.1", "App.exe.bak.old.1",
	}
	const keptDir = "App.exe.old.999" // 形状相符的空目录

	cases := []struct {
		name string
		held string // 保持打开的文件，空串表示没有
	}{
		{"全部删得掉", ""},
		{"带进程号的一个被占用", "App.exe.old.555"}, // 按文件名排在 App.exe.old.9 之前
		{".old 被占用", "App.exe.old"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range append(append([]string{}, doomed...), kept...) {
				writeFile(t, filepath.Join(dir, name), name)
			}
			if err := os.Mkdir(filepath.Join(dir, keptDir), 0o755); err != nil {
				t.Fatal(err)
			}
			if c.held != "" {
				writeFile(t, filepath.Join(dir, c.held), c.held)
				holdOpen(t, filepath.Join(dir, c.held))
			}

			removeUpdateBackups(filepath.Join(dir, "App.exe"))

			for _, name := range doomed {
				if got, want := exists(filepath.Join(dir, name)), name == c.held; got != want {
					t.Errorf("%s 仍在：%v，期望 %v", name, got, want)
				}
			}
			for _, name := range append([]string{keptDir}, kept...) {
				if !exists(filepath.Join(dir, name)) {
					t.Errorf("%s 被误删", name)
				}
			}
			if c.held != "" && !exists(filepath.Join(dir, c.held)) {
				t.Errorf("被占用的 %s 不见了", c.held)
			}
		})
	}
}
