package watchdog

// 本文件只测 findRunningKuGou：从进程快照里定位酷狗主进程，并取其 CEF 子进程的映像目录。

import (
	"errors"
	"testing"
)

func TestFindRunningSingleMain(t *testing.T) {
	m := newFakeMachine()
	got, err := findRunningKuGou(m.procs, m.deps().imagePath)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.PID != stubMainPID || got.ExePath != stubExe {
		t.Fatalf("主进程 = (%d, %q)，应为 (%d, %q)", got.PID, got.ExePath, stubMainPID, stubExe)
	}
	// 4 个子进程在同一版本目录，去重为一个；父进程是主进程而映像名不是 KuGou.exe 的（KGService.exe）不算。
	if len(got.ChildDirs) != 1 || got.ChildDirs[0] != stubVerDir {
		t.Fatalf("ChildDirs = %q，应为 [%q]", got.ChildDirs, stubVerDir)
	}
}

func TestFindRunningNone(t *testing.T) {
	procs := []proc{{PID: 100, PPID: 4, Name: "explorer.exe"}, {PID: 200, PPID: 100, Name: "KuGouHelper.exe"}}
	_, err := findRunningKuGou(procs, newFakeMachine().deps().imagePath)
	wantKind(t, err, ErrConfirmNotReady)

	_, err = findRunningKuGou(nil, newFakeMachine().deps().imagePath)
	wantKind(t, err, ErrConfirmNotReady)
}

// 两个父进程都不是 KuGou.exe 的 KuGou.exe：按未就绪，不取 PID 最小（与 procscan.MainPID 不同，有意）。
// 主进程已退出、子进程成了孤儿时也落在这里（孤儿的父 PID 不在同名集合里）。
func TestFindRunningTwoMains(t *testing.T) {
	m := newFakeMachine()
	m.procs = append(m.procs, proc{PID: 9000, PPID: 100, Name: "KuGou.exe"})
	m.images[9000] = stubExe
	_, err := findRunningKuGou(m.procs, m.deps().imagePath)
	wantKind(t, err, ErrConfirmNotReady)

	orphan := newFakeMachine()
	orphan.procs = append(orphan.procs, proc{PID: 9001, PPID: 12345, Name: "KuGou.exe"})
	_, err = findRunningKuGou(orphan.procs, orphan.deps().imagePath)
	wantKind(t, err, ErrConfirmNotReady)
}

func TestFindRunningCaseInsensitive(t *testing.T) {
	m := newFakeMachine()
	for i := range m.procs {
		switch m.procs[i].PID {
		case stubMainPID:
			m.procs[i].Name = "KUGOU.EXE"
		case 7001, 7002:
			m.procs[i].Name = "kugou.exe"
		}
	}
	// 同一目录大小写不同的两种写法去重为一个，保留先出现的写法。
	m.images[7003] = `C:\PROGRAM FILES\KuGou\KGMusic\20.1.22.27795\KUGOU.EXE`
	got, err := findRunningKuGou(m.procs, m.deps().imagePath)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.PID != stubMainPID {
		t.Fatalf("PID = %d，应为 %d", got.PID, stubMainPID)
	}
	if len(got.ChildDirs) != 1 || got.ChildDirs[0] != stubVerDir {
		t.Fatalf("ChildDirs = %q，应为 [%q]", got.ChildDirs, stubVerDir)
	}
}

func TestFindRunningImagePathFails(t *testing.T) {
	t.Run("主进程", func(t *testing.T) {
		m := newFakeMachine()
		m.imageErr[stubMainPID] = errors.New("access denied")
		_, err := findRunningKuGou(m.procs, m.deps().imagePath)
		wantKind(t, err, ErrConfirmNotReady)
	})
	t.Run("任一子进程", func(t *testing.T) {
		m := newFakeMachine()
		m.imageErr[7003] = errors.New("process exited")
		_, err := findRunningKuGou(m.procs, m.deps().imagePath)
		wantKind(t, err, ErrConfirmNotReady)
	})
}

// 快照之后 PID 被别的程序复用：映像名不再是 KuGou.exe，按未就绪，不拿别人的目录当酷狗的。
func TestFindRunningPIDReused(t *testing.T) {
	t.Run("主进程", func(t *testing.T) {
		m := newFakeMachine()
		m.images[stubMainPID] = `C:\Windows\System32\notepad.exe`
		_, err := findRunningKuGou(m.procs, m.deps().imagePath)
		wantKind(t, err, ErrConfirmNotReady)
	})
	t.Run("子进程", func(t *testing.T) {
		m := newFakeMachine()
		m.images[7002] = `C:\Other\app.exe`
		_, err := findRunningKuGou(m.procs, m.deps().imagePath)
		wantKind(t, err, ErrConfirmNotReady)
	})
}

// 没有子进程不是 findRunningKuGou 的错误（Task 7 只要主进程映像），由 confirmWith 判未就绪。
func TestFindRunningNoChildren(t *testing.T) {
	procs := []proc{{PID: 100, PPID: 4, Name: "explorer.exe"}, {PID: stubMainPID, PPID: 100, Name: "KuGou.exe"}}
	got, err := findRunningKuGou(procs, newFakeMachine().deps().imagePath)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.PID != stubMainPID || len(got.ChildDirs) != 0 {
		t.Fatalf("got = %+v，应为主进程 %d、无子进程目录", got, stubMainPID)
	}
}

// 子进程分在两个目录：两个都列出，由 confirmWith 判不支持。
func TestFindRunningChildDirsDiffer(t *testing.T) {
	m := newFakeMachine()
	m.images[7004] = stubInstall + `\20.1.10.1\KuGou.exe`
	got, err := findRunningKuGou(m.procs, m.deps().imagePath)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got.ChildDirs) != 2 {
		t.Fatalf("ChildDirs = %q，应为两个目录", got.ChildDirs)
	}
}
