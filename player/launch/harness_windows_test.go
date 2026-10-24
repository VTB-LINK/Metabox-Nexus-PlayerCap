package launch

// 本文件是本包各用例共用的夹具：测试二进制重入自身扮演的几种角色（TestMain 按环境变量分流），假播放器的
// 制作，被拉起进程的报告，以及对自己拉起的进程的句柄管理。不含用例。
//
// 假播放器是测试二进制的副本，PE 头的 Subsystem 改成 GUI（与真实播放器一样不带控制台）：控制台程序经 start
// 启动会新开一个可见的控制台窗口，测试时会在桌面上闪窗。改的只是临时目录里的副本，不涉及出货构建（§0 第 5 条）。
// 被拉起的进程都有存活上限（lingerCap），用例中途失败、清理没跑到时也会自己退出。

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	envRole         = "PLAYERCAP_LAUNCH_TEST_ROLE"          // 角色：player / playercap / hang / marker / exit3
	envReport       = "PLAYERCAP_LAUNCH_TEST_REPORT"        // 本进程的报告写到哪里
	envLinger       = "PLAYERCAP_LAUNCH_TEST_LINGER"        // player 写完报告后再活多久（time.ParseDuration）
	envMode         = "PLAYERCAP_LAUNCH_TEST_MODE"          // playercap 拉起假播放器的方式：direct / detached
	envPlayerExe    = "PLAYERCAP_LAUNCH_TEST_PLAYER_EXE"    // playercap 要拉起的假播放器
	envPlayerReport = "PLAYERCAP_LAUNCH_TEST_PLAYER_REPORT" // 假播放器的报告写到哪里
)

// lingerCap 是被拉起进程的存活上限。
const lingerCap = 60 * time.Second

func TestMain(m *testing.M) {
	switch os.Getenv(envRole) {
	case "":
		os.Exit(m.Run())
	case "player":
		rolePlayer()
	case "playercap":
		rolePlayerCap()
	case "hang": // 扮演卡住的 cmd.exe
		writeReport(report{PID: os.Getpid()})
		time.Sleep(20 * time.Second)
	case "marker": // 扮演 cmd.exe，只留下「被调用过」的记号
		writeReport(report{PID: os.Getpid()})
	case "exit3": // 扮演以非 0 退出的 cmd.exe
		os.Exit(3)
	}
	os.Exit(0)
}

// report 是被拉起进程写下的自述。
type report struct {
	PID, PPID  int
	CmdLine    string   // GetCommandLineW 的原文
	Args       []string // os.Args[1:]
	Cwd        string
	Flags      uint32 // STARTUPINFO.dwFlags
	ShowWindow uint16 // STARTUPINFO.wShowWindow
	Detached   bool   // playercap：launch.Detached 的返回
	Err        string // playercap：拉起出错时的错误
}

func writeReport(r report) {
	p := os.Getenv(envReport)
	b, _ := json.Marshal(r)
	// 先写临时文件再改名，读的一方不会读到半截。
	if os.WriteFile(p+".tmp", b, 0o644) == nil {
		os.Rename(p+".tmp", p)
	}
}

func rolePlayer() {
	var si windows.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	windows.GetStartupInfo(&si)
	cwd, _ := os.Getwd()
	writeReport(report{
		PID:        os.Getpid(),
		PPID:       parentPID(uint32(os.Getpid())),
		CmdLine:    windows.UTF16PtrToString(windows.GetCommandLine()),
		Args:       os.Args[1:],
		Cwd:        cwd,
		Flags:      si.Flags,
		ShowWindow: si.ShowWindow,
	})
	if d, err := time.ParseDuration(os.Getenv(envLinger)); err == nil {
		time.Sleep(min(d, lingerCap))
	}
}

// rolePlayerCap 扮演 PlayerCap：按 envMode 拉起假播放器，报告结果后一直运行，等着被结束。
func rolePlayerCap() {
	exe, self := os.Getenv(envPlayerExe), os.Getenv(envReport)
	r := report{PID: os.Getpid()}
	// 以下三项经环境变量交给假播放器（cmd.exe 与 start 都原样传下去）。
	os.Setenv(envRole, "player")
	os.Setenv(envLinger, lingerCap.String())
	os.Setenv(envReport, os.Getenv(envPlayerReport))
	var err error
	if os.Getenv(envMode) == "detached" {
		r.Detached, err = Detached(exe, []string{"--remote-debugging-port=9222"}, "")
	} else {
		err = exec.Command(exe, "--remote-debugging-port=9222").Start() // 对照组：修复前的写法
	}
	if err != nil {
		r.Err = err.Error()
	}
	os.Setenv(envReport, self)
	writeReport(r)
	time.Sleep(lingerCap)
}

func parentPID(pid uint32) int {
	for _, e := range snapshot() {
		if e.ProcessID == pid {
			return int(e.ParentProcessID)
		}
	}
	return 0
}

func snapshot() []windows.ProcessEntry32 {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(h)
	var out []windows.ProcessEntry32
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err := windows.Process32First(h, &e); err == nil; err = windows.Process32Next(h, &e) {
		out = append(out, e)
	}
	return out
}

var guiImage = sync.OnceValues(func() ([]byte, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(self)
	if err != nil {
		return nil, err
	}
	pe := binary.LittleEndian.Uint32(b[0x3c:])
	if string(b[pe:pe+4]) != "PE\x00\x00" {
		return nil, os.ErrInvalid
	}
	// 签名 4 字节 + COFF 头 20 字节之后是可选头，Subsystem 在可选头的第 68 字节；2 = IMAGE_SUBSYSTEM_WINDOWS_GUI。
	binary.LittleEndian.PutUint16(b[pe+4+20+68:], 2)
	return b, nil
})

// fakePlayer 在 dst 放一份 GUI 子系统的测试二进制，返回 dst。
func fakePlayer(t *testing.T, dst string) string {
	t.Helper()
	b, err := guiImage()
	if err != nil {
		t.Fatalf("制作假播放器: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

func waitReport(t *testing.T, path string, limit time.Duration) report {
	t.Helper()
	end := time.Now().Add(limit)
	for time.Now().Before(end) {
		if b, err := os.ReadFile(path); err == nil {
			var r report
			if err := json.Unmarshal(b, &r); err != nil {
				t.Fatalf("解析报告 %s: %v", path, err)
			}
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%v 内没有等到报告 %s", limit, path)
	return report{}
}

// longPath 把路径里的 8.3 短名（临时目录常见）展开成长名，便于与进程映像路径比较。
func longPath(p string) string {
	in, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetLongPathName(in, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || int(n) > len(buf) {
		return p
	}
	return windows.UTF16ToString(buf[:n])
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(longPath(a)), filepath.Clean(longPath(b)))
}

// own 打开 pid 并核对其映像就是 image（本用例放下的那份），核对不过即判失败、不动它；用例结束时结束它。
// 之后一律经返回的句柄操作，不再按 PID 找进程，PID 被复用也不会误伤别的进程。
func own(t *testing.T, pid int, image string) windows.Handle {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatalf("打开进程 %d: %v", pid, err)
	}
	if got := imagePath(h); !samePath(got, image) {
		windows.CloseHandle(h)
		t.Fatalf("进程 %d 的映像是 %q，不是本用例放下的 %q", pid, got, image)
	}
	t.Cleanup(func() {
		windows.TerminateProcess(h, 1)
		windows.WaitForSingleObject(h, 5000)
		windows.CloseHandle(h)
	})
	return h
}

// awaitExit 等 pid 退出：打不开即已退出并被回收；打得开、尚未退出且映像是 image 时等它 limit，超时则结束它并判失败。
func awaitExit(t *testing.T, pid int, image string, limit time.Duration) {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	if exited(h, 0) || !samePath(imagePath(h), image) {
		return // 已退出，或 PID 已被别的进程复用
	}
	if !exited(h, limit) {
		windows.TerminateProcess(h, 1)
		t.Errorf("进程 %d（%s）在 %v 内没有退出，已结束它", pid, image, limit)
	}
}

func imagePath(h windows.Handle) string {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// exited 报告句柄所指的进程是否在 limit 内退出。
func exited(h windows.Handle, limit time.Duration) bool {
	ev, _ := windows.WaitForSingleObject(h, uint32(limit/time.Millisecond))
	return ev == windows.WAIT_OBJECT_0
}
