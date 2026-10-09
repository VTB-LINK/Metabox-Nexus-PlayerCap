package procscan

import "testing"

func TestMainPID_ChromiumStyle(t *testing.T) {
	// 主进程父为 explorer（非同名），两个子进程父指向主进程。
	procs := []Proc{
		{PID: 100, Name: "explorer.exe", PPID: 4},
		{PID: 200, Name: "cloudmusic.exe", PPID: 100},
		{PID: 300, Name: "cloudmusic.exe", PPID: 200},
		{PID: 400, Name: "cloudmusic.exe", PPID: 200},
	}
	got, ok := MainPID(procs, "cloudmusic.exe")
	if !ok || got != 200 {
		t.Fatalf("MainPID = %d, %v; want 200, true", got, ok)
	}
}

func TestMainPID_Single(t *testing.T) {
	procs := []Proc{
		{PID: 100, Name: "explorer.exe", PPID: 4},
		{PID: 555, Name: "WeSing.exe", PPID: 100},
	}
	got, ok := MainPID(procs, "WeSing.exe")
	if !ok || got != 555 {
		t.Fatalf("MainPID = %d, %v; want 555, true", got, ok)
	}
}

func TestMainPID_MultipleMainsPicksLowestPID(t *testing.T) {
	// 两个独立主进程（父都非同名）→ 取 PID 最小；950 是 700 的子进程应被排除。
	procs := []Proc{
		{PID: 100, Name: "explorer.exe", PPID: 4},
		{PID: 900, Name: "qqmusic.exe", PPID: 100},
		{PID: 700, Name: "qqmusic.exe", PPID: 100},
		{PID: 950, Name: "qqmusic.exe", PPID: 700},
	}
	got, ok := MainPID(procs, "qqmusic.exe")
	if !ok || got != 700 {
		t.Fatalf("MainPID = %d, %v; want 700, true", got, ok)
	}
}

func TestMainPID_NoMatch(t *testing.T) {
	procs := []Proc{
		{PID: 100, Name: "explorer.exe", PPID: 4},
		{PID: 200, Name: "chrome.exe", PPID: 100},
	}
	got, ok := MainPID(procs, "KuGou.exe")
	if ok || got != 0 {
		t.Fatalf("MainPID = %d, %v; want 0, false", got, ok)
	}
}

func TestMainPID_CaseInsensitive(t *testing.T) {
	// 子进程用不同大小写的同名，也须被识别为子进程而排除；返回大小写不敏感命中的主进程。
	procs := []Proc{
		{PID: 100, Name: "explorer.exe", PPID: 4},
		{PID: 210, Name: "QQMusic.exe", PPID: 100},
		{PID: 220, Name: "qqmusic.exe", PPID: 210},
	}
	got, ok := MainPID(procs, "qqmusic.exe")
	if !ok || got != 210 {
		t.Fatalf("MainPID = %d, %v; want 210, true", got, ok)
	}
}
